#!/usr/bin/env python3
"""judge.py — score raw benchmark results and emit the summary table.

Two layers, cheapest first (the cost/noise discipline the methodology doc
mandates):
  1. deterministic check (exact / contains) — zero noise, zero cost
  2. optional LLM judge (--judge-model) for "judge"-check tasks (T3), with an
     optional self-consistency pass (--judge-runs N) that re-asks the judge
     and reports agreement.

Outputs: results-scored.jsonl (adds "success" to each record) and
summary.json: per (subset, tier) → {n, success_rate, success_std,
compression_ratio, calls_with_ratio}.
"""

from __future__ import annotations

import argparse
import json
import statistics
import urllib.request
from pathlib import Path

JUDGE_PROMPT = """You are grading an AI assistant on a code-comprehension question.

Question: {question}
Reference answer: {ground_truth}
Assistant answer: {answer}

Score 1-5 for correctness:
  5 = correct and complete
  4 = correct but imprecise
  3 = partially correct
  2 = mostly wrong
  1 = wrong or refused

Respond with ONLY one integer 1-5."""


def llm_judge(url: str, api_key: str, model: str, question: str, gt: str, answer: str,
              timeout: float) -> int | None:
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": JUDGE_PROMPT.format(
            question=question, ground_truth=gt, answer=answer)}],
        "temperature": 0,
    }).encode()
    req = urllib.request.Request(url.rstrip("/") + "/v1/chat/completions", data=body,
                                 headers={"Content-Type": "application/json",
                                          **({"Authorization": f"Bearer {api_key}"} if api_key else {})})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            payload = json.loads(resp.read().decode())
        text = payload["choices"][0]["message"]["content"]
        for token in text.split():
            if token.strip(" .") in {"1", "2", "3", "4", "5"}:
                return int(token.strip(" ."))
        return None
    except Exception:
        return None  # judge failures are recorded as unscored, never silently dropped


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--results", default=str(Path(__file__).parent / "results.jsonl"))
    ap.add_argument("--out-scored", default=str(Path(__file__).parent / "results-scored.jsonl"))
    ap.add_argument("--out-summary", default=str(Path(__file__).parent / "summary.json"))
    ap.add_argument("--judge-model", default="", help="enable LLM judging for 'judge' tasks")
    ap.add_argument("--api-base", default="http://127.0.0.1:8787")
    ap.add_argument("--api-key", default="")
    ap.add_argument("--judge-runs", type=int, default=2, help="judge self-consistency runs")
    ap.add_argument("--timeout", type=float, default=120.0)
    args = ap.parse_args()

    records = [json.loads(l) for l in Path(args.results).read_text(encoding="utf-8").splitlines() if l.strip()]
    scored_path, summary_path = Path(args.out_scored), Path(args.out_summary)
    judge_calls = 0

    # Judge score cache (sidecar): keyed by task/tier/run/answer so re-scoring
    # (e.g. re-printing the table) never re-calls the LLM for judged answers.
    cache_path = Path(args.out_scored).with_suffix(".judgecache.json")
    cache: dict[str, list[int]] = {}
    if cache_path.exists():
        cache = json.loads(cache_path.read_text(encoding="utf-8"))

    with scored_path.open("w", encoding="utf-8") as f:
        for rec in records:
            if rec["check"] == "judge":
                if args.judge_model and rec["answer"]:
                    ckey = f"{rec['task_id']}|{rec['tier']}|{rec['run']}|{hash(rec['answer'])}"
                    if ckey in cache:
                        scores = cache[ckey]
                    else:
                        scores = []
                        for _ in range(max(1, args.judge_runs)):
                            s = llm_judge(args.api_base, args.api_key, args.judge_model,
                                          rec["question"], rec["ground_truth"], rec["answer"], args.timeout)
                            judge_calls += 1
                            if s is not None:
                                scores.append(s)
                        if scores:
                            cache[ckey] = scores
                    if scores:
                        rec["judge_scores"] = scores
                        rec["success"] = statistics.mean(scores) >= 4.0
                    else:
                        rec["success"] = None
                else:
                    rec["success"] = None
            elif rec["success"] is None:
                # dry-run=False replay: redo the deterministic check here
                norm = lambda s: str(s).strip().lower().strip('."')
                if rec["check"] == "exact":
                    rec["success"] = norm(rec["answer"]) == norm(rec["ground_truth"])
                elif rec["check"] == "contains":
                    rec["success"] = norm(rec["ground_truth"]) in norm(rec["answer"])
            f.write(json.dumps(rec, ensure_ascii=False) + "\n")
    cache_path.write_text(json.dumps(cache), encoding="utf-8")

    # aggregate: per (subset, tier)
    groups: dict[tuple[str, str], list] = {}
    for rec in records:
        groups.setdefault((rec["subset"], rec["tier"]), []).append(rec)
    summary = {}
    for (subset, tier), rs in sorted(groups.items()):
        scored = [r for r in rs if r.get("success") is not None]
        ratios = [r["tokencut"]["ratio"] for r in rs if r["tokencut"]["ratio"] > 0]
        summary[f"{subset}|{tier}"] = {
            "n": len(rs),
            "n_scored": len(scored),
            "success_rate": round(sum(1 for r in scored if r["success"]) / len(scored), 4) if scored else None,
            "success_std": round(statistics.pstdev([1.0 if r["success"] else 0.0 for r in scored]), 4) if len(scored) > 1 else None,
            "compression_ratio_mean": round(statistics.mean(ratios), 4) if ratios else 0.0,
            "latency_ms_mean": round(statistics.mean([r["latency_ms"] for r in rs]), 1),
        }
    summary_path.write_text(json.dumps(summary, indent=2), encoding="utf-8")

    # readable table
    print(f"{'subset':<16} {'tier':<11} {'success':>8} {'std':>6} {'ratio':>7} {'n':>5}")
    for key, s in sorted(summary.items()):
        subset, tier = key.split("|")
        sr = f"{s['success_rate']*100:5.1f}%" if s["success_rate"] is not None else "  n/a"
        std = f"{s['success_std']:.3f}" if s["success_std"] is not None else "-"
        print(f"{subset:<16} {tier:<11} {sr:>8} {std:>6} {s['compression_ratio_mean']*100:6.1f}% {s['n']:>5}")
    if judge_calls:
        print(f"(llm judge calls: {judge_calls})")
    print(f"\nscored -> {scored_path}\nsummary -> {summary_path}")


if __name__ == "__main__":
    main()

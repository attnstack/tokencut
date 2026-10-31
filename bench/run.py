#!/usr/bin/env python3
"""run.py — tokencut benchmark runner.

For each task × tier × run it sends the task's messages to an OpenAI-compatible
endpoint (optionally through the tokencut proxy), captures the answer and the
x-tokencut-* observability headers, and appends one JSONL record per call.

Modes:
  real      — hits a real model through --proxy (or directly at --direct).
              Costs real money; see benchmark-methodology.md for the budget.
  dry-run   — pairs with `tokencut --mock-upstream` (a reference solver, NOT
              an LLM). Validates the whole pipeline offline and produces a
              reference-solver Pareto curve; it does NOT measure model quality.

Examples:
  # offline pipeline check (no API key)
  tokencut --mock-upstream &      # listens on 127.0.0.1:8787
  python run.py --proxy http://127.0.0.1:8787 --dry-run

  # real run (needs OPENAI_API_KEY-authorized endpoint behind the proxy)
  python run.py --proxy http://127.0.0.1:8787 --api-key sk-... \
      --model gpt-4o-mini --tiers none,lossless,balanced,aggressive,b0.5,b0.7,b0.9
"""

from __future__ import annotations

import argparse
import json
import time
import urllib.error
import urllib.request
from pathlib import Path

# tiers → the "tokencut" override object injected into the request body.
# "none" sends no override and targets --direct (bypassing the proxy) when
# available, so the baseline is a true un-proxied call.
TIER_OVERRIDES: dict[str, dict] = {
    "none": {},
    "lossless": {"tier": "lossless"},
    "balanced": {"tier": "balanced"},
    "aggressive": {"tier": "aggressive"},
}


def tier_override(tier: str) -> dict:
    if tier in TIER_OVERRIDES:
        return dict(TIER_OVERRIDES[tier])
    if tier.startswith("b"):
        # b0.5 → target 0.5 (i.e. compress 50%); also accept b50.
        spec = tier[1:]
        target = float(spec) if "." in spec else float(spec) / 100.0
        if not 0 < target < 1:
            raise ValueError(f"target out of range in tier {tier!r}")
        return {"target": target}
    raise ValueError(f"unknown tier {tier!r} (use none/lossless/balanced/aggressive/b<rate>)")


def chat_once(url: str, api_key: str, model: str, messages: list[dict], override: dict,
              timeout: float) -> tuple[dict | None, dict[str, str], float]:
    """One chat-completions call. Returns (response_json|None, tokencut_headers, latency_ms)."""
    body: dict = {"model": model, "messages": messages, "temperature": 0}
    if override:
        body["tokencut"] = override
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        url.rstrip("/") + "/v1/chat/completions", data=data,
        headers={"Content-Type": "application/json", **({"Authorization": f"Bearer {api_key}"} if api_key else {})},
        method="POST",
    )
    start = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            payload = json.loads(resp.read().decode())
            latency_ms = (time.perf_counter() - start) * 1000
            tk_headers = {k.lower(): v for k, v in resp.headers.items() if k.lower().startswith("x-tokencut")}
            return payload, tk_headers, latency_ms
    except urllib.error.HTTPError as e:
        # upstream rejected the request: record it as a failed call, keep going
        latency_ms = (time.perf_counter() - start) * 1000
        return {"error": {"status": e.code, "body": e.read().decode()[:400]}}, {}, latency_ms


def normalize(s: str) -> str:
    return s.strip().lower().strip('."')


def check_answer(task: dict, answer: str) -> bool:
    """Deterministic scoring for exact/contains subsets (T3 is judge-scored)."""
    if task["check"] == "exact":
        return normalize(answer) == normalize(task["ground_truth"])
    if task["check"] == "contains":
        return normalize(task["ground_truth"]) in normalize(answer)
    return False  # "judge" tasks are scored by judge.py in real mode


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--proxy", default="http://127.0.0.1:8787", help="tokencut proxy base URL")
    ap.add_argument("--direct", default="", help="direct (no proxy) base URL for the 'none' tier; defaults to the proxy itself")
    ap.add_argument("--model", default="mock-model", help="model name passed through")
    ap.add_argument("--api-key", default="", help="bearer token forwarded to the endpoint")
    ap.add_argument("--tasks", default=str(Path(__file__).parent / "tasks.jsonl"))
    ap.add_argument("--tiers", default="none,lossless,balanced,aggressive,b0.5,b0.7,b0.9")
    ap.add_argument("--runs", type=int, default=3, help="runs per (task, tier); temperature=0 so runs check stability")
    ap.add_argument("--subsets", default="t1,t2,t3,t4", help="comma list: t1,t2,t3,t4")
    ap.add_argument("--dry-run", action="store_true", help="score deterministically inline (mock upstream mode)")
    ap.add_argument("--limit", type=int, default=0, help="cap tasks per subset (0 = all)")
    ap.add_argument("--timeout", type=float, default=120.0)
    ap.add_argument("--out", default=str(Path(__file__).parent / "results.jsonl"))
    args = ap.parse_args()

    subsets = {s.strip() for s in args.subsets.split(",")}
    tiers = [t.strip() for t in args.tiers.split(",")]
    tasks = [json.loads(l) for l in Path(args.tasks).read_text(encoding="utf-8").splitlines() if l.strip()]
    if args.limit:
        by_subset: dict[str, list] = {}
        for t in tasks:
            by_subset.setdefault(t["subset"][:2], []).append(t)
        tasks = [t for s in sorted(subsets) for t in by_subset.get(s, [])[: args.limit]]

    out = Path(args.out)
    n_calls = n_errors = 0
    with out.open("w", encoding="utf-8") as f:
        for task in tasks:
            if task["subset"][:2] not in subsets:
                continue
            for tier in tiers:
                direct = (tier == "none" and args.direct) or not args.proxy
                url = args.direct if tier == "none" and args.direct else args.proxy
                if not url:
                    url = args.proxy
                override = tier_override(tier)
                if tier == "none" and not args.direct:
                    override = {"enabled": False}  # still through the proxy, compression off
                for run in range(args.runs):
                    # Real agent shape: payload messages first, the question as
                    # the final user message (which the proxy exempts).
                    messages = list(task["context"]) + [{"role": "user", "content": task["question"]}]
                    payload, tk, latency_ms = chat_once(
                        url, args.api_key, args.model, messages, override, args.timeout
                    )
                    n_calls += 1
                    answer = ""
                    if payload and "choices" in payload:
                        answer = payload["choices"][0]["message"]["content"]
                    elif payload and "error" in payload:
                        n_errors += 1
                    rec = {
                        "task_id": task["id"],
                        "subset": task["subset"],
                        "check": task["check"],
                        "tier": tier,
                        "run": run,
                        "question": task["question"],
                        "ground_truth": task["ground_truth"],
                        "answer": answer,
                        "success": check_answer(task, answer) if args.dry_run and task["check"] != "judge" else None,
                        "latency_ms": round(latency_ms, 1),
                        "error": (payload or {}).get("error"),
                        "tokencut": {
                            "original_tokens": int(tk.get("x-tokencut-original-tokens", 0) or 0),
                            "compressed_tokens": int(tk.get("x-tokencut-compressed-tokens", 0) or 0),
                            "ratio": float(tk.get("x-tokencut-ratio", 0) or 0),
                            "strategies": tk.get("x-tokencut-strategies", ""),
                        },
                    }
                    f.write(json.dumps(rec, ensure_ascii=False) + "\n")
        f.flush()
    print(f"wrote {n_calls} calls ({n_errors} errors) -> {out}")


if __name__ == "__main__":
    main()

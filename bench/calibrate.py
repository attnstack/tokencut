#!/usr/bin/env python3
"""calibrate.py — fit tokencut's token-estimation constants against a real
tokenizer (tiktoken), on the actual benchmark payload shapes plus CJK-mixed
text.

Fits the model  tokens ≈ ascii_runes / C_a + wide_runes / C_w
by least squares over the corpus, then reports per-subset relative error for
the fitted constants vs the shipped defaults (4.0 / 1.4).

Usage:  python calibrate.py [--encodings cl100k_base,o200k_base]
Output: recommended constants per encoding + error table; copy the winner
        into config.yaml's tokenest section (and internal defaults).

Requires: tiktoken with the encoding vocabulary already cached locally
(`python -c "import tiktoken; tiktoken.get_encoding('cl100k_base')"` once
with network access).
"""

from __future__ import annotations

import argparse
import json
import math
import statistics
from pathlib import Path

DEFAULT_CA, DEFAULT_CW = 4.0, 1.4


def ascii_wide_runes(s: str) -> tuple[int, int]:
    """Same counting rule as internal/tokenest: runes <128 are ASCII."""
    ascii_n = wide_n = 0
    for r in s:
        if ord(r) < 128:
            ascii_n += 1
        else:
            wide_n += 1
    return ascii_n, wide_n


def fit(ascii_counts: list[int], wide_counts: list[int], tokens: list[int]) -> tuple[float, float]:
    """Least squares for tokens ≈ a·u + w·v; returns (C_a, C_w) = (1/u, 1/v)."""
    n = len(ascii_counts)
    saa = sum(a * a for a in ascii_counts)
    sww = sum(w * w for w in wide_counts)
    saw = sum(a * w for a, w in zip(ascii_counts, wide_counts))
    sat = sum(a * t for a, t in zip(ascii_counts, tokens))
    swt = sum(w * t for w, t in zip(wide_counts, tokens))
    det = saa * sww - saw * saw
    if det == 0:
        raise SystemExit("degenerate corpus: need both ASCII-only and wide-heavy samples")
    u = (sww * sat - saw * swt) / det
    v = (saa * swt - saw * sat) / det
    if u <= 0 or v <= 0:
        raise SystemExit(f"non-positive fit (u={u}, v={v}) — corpus too small or skewed")
    return 1.0 / u, 1.0 / v


def rel_err(samples: list[tuple[int, int, int]], ca: float, cw: float) -> float:
    errs = []
    for a, w, t in samples:
        est = a / ca + w / cw
        errs.append(abs(est - t) / max(t, 1))
    return statistics.mean(errs)


def build_corpus() -> dict[str, str]:
    """Real benchmark payloads + CJK-mixed prose, keyed by subset name."""
    corpus: dict[str, str] = {}
    tasks_path = Path(__file__).parent / "tasks.jsonl"
    if tasks_path.exists():
        tasks = [json.loads(l) for l in tasks_path.read_text(encoding="utf-8").splitlines() if l.strip()]
        for t in tasks:
            # payload = the big context message; skip system prompts
            payload = t["context"][1]["content"]
            corpus.setdefault(t["subset"], "")
            corpus[t["subset"]] += payload + "\n"
            corpus.setdefault("questions", "")
            corpus["questions"] += t["question"] + "\n"
    else:
        raise SystemExit("run gen_tasks.py first — calibration must use the real payload shapes")
    # CJK-mixed sample: the widest-content surface tokencut will see in zh usage
    corpus["cjk_mixed"] = (
        "tokencut 是一个 LLM 上下文压缩代理，把工具输出在进入模型前压掉 60% 到 95%。"
        "JSON 压完仍是合法 JSON：对象折叠为 {\"...\": \"12 fields omitted\"}，"
        "数组保留首尾并插入 {\"_omitted\": 42}。设计目标是确定性：同样的输入永远得到同样的输出，"
        "缓存键 = 内容指纹 + 档位。压缩率口径：token（tiktoken cl100k 校准，误差目标 <5%）。\n"
    ) * 20
    # cap corpus slices per subset to keep the run quick but representative
    return {k: (v[:200_000] if k != "cjk_mixed" else v) for k, v in corpus.items()}


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--encodings", default="cl100k_base,o200k_base")
    args = ap.parse_args()

    corpus = build_corpus()
    print(f"corpus subsets: {', '.join(f'{k}({len(v)//1024}KB)' for k, v in corpus.items())}\n")

    # Content-kind mapping (matches internal/classify.Kind):
    #   json  -> t1/t4 payloads      log -> t2 payloads
    #   code  -> t3 payloads         text -> questions
    # Wide (CJK) constant: fit on cjk_mixed given the fitted ASCII constant.
    kind_of = {
        "t1_json_qa": "json", "t4_record_qa": "json",
        "t2_log_triage": "log", "t3_code_qa": "code", "questions": "text",
    }

    for enc_name in args.encodings.split(","):
        try:
            import tiktoken
            enc = tiktoken.get_encoding(enc_name)
        except Exception as e:
            print(f"[{enc_name}] unavailable ({e}) — skipped (vocabulary must be cached locally)")
            continue

        measured: dict[str, tuple[int, int, int]] = {}
        for name, text in corpus.items():
            a, w = ascii_wide_runes(text)
            t = len(enc.encode(text, disallowed_special=()))
            measured[name] = (a, w, t)

        # Per-kind ASCII constant: those payloads are pure ASCII, so C_a = a/t directly.
        kind_ca: dict[str, float] = {}
        for name, kind in kind_of.items():
            a, w, t = measured[name]
            if w > 0.5 * a:  # skip degenerate mixes
                continue
            kind_ca[kind] = a / t
        ca_text = kind_ca.get("text", 4.0)

        # Wide constant: residual tokens of cjk_mixed after paying its ASCII
        # part at the text constant.
        a, w, t = measured["cjk_mixed"]
        residual = t - a / ca_text
        cw = w / residual if residual > 0 else float("nan")

        print(f"[{enc_name}]  per-kind constants (fitted):")
        for kind, ca in sorted(kind_ca.items()):
            print(f"    {kind:<5} ascii_chars_per_token: {ca:.2f}")
        print(f"    wide  wide_chars_per_token:  {cw:.2f}   (ascii part priced at text C_a={ca_text:.2f})")

        # Error table: per-kind constants vs the old global defaults.
        print(f"\n{'subset':<16} {'tokens':>8} {'err(defaults)':>14} {'err(per-kind)':>13}")
        for name, (a, w, t) in measured.items():
            kind = kind_of.get(name, "wide")
            if kind == "wide":
                est_def = a / DEFAULT_CA + w / DEFAULT_CW
                est_new = a / ca_text + w / cw
            else:
                ca = kind_ca[kind]
                est_def = a / DEFAULT_CA + w / DEFAULT_CW
                est_new = a / ca + w / cw
            print(f"{name:<16} {t:>8} {abs(est_def-t)/t*100:>13.1f}% {abs(est_new-t)/t*100:>12.1f}%")
        print()
        yaml = ["tokenest:", "  # fitted against %s on the benchmark corpus (bench/calibrate.py)" % enc_name]
        for kind, ca in sorted(kind_ca.items()):
            yaml.append(f"  {kind}_chars_per_token: {ca:.2f}")
        yaml.append(f"  wide_chars_per_token: {cw:.2f}")
        print("    # recommended config:\n    " + "\n    ".join(yaml) + "\n")


if __name__ == "__main__":
    main()

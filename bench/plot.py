#!/usr/bin/env python3
"""plot.py — Pareto curve: compression ratio (x) vs task success rate (y),
overall and per subset, from summary.json. Saves pareto.png (matplotlib
required; skip gracefully if absent) and prints the markdown table for README.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path

SUBSET_LABELS = {
    "t1_json_qa": "T1 JSON QA",
    "t2_log_triage": "T2 log triage",
    "t3_code_qa": "T3 code QA",
    "t4_record_qa": "T4 record QA",
}
TIER_ORDER = ["none", "lossless", "b0.5", "balanced", "b0.7", "b0.9", "aggressive"]


def load(summary_path: Path) -> dict:
    raw = json.loads(summary_path.read_text(encoding="utf-8"))
    series: dict[str, list[tuple[float, float | None]]] = {}
    for key, s in raw.items():
        subset, tier = key.split("|")
        if s["success_rate"] is None:
            continue
        series.setdefault(subset, []).append((s["compression_ratio_mean"], s["success_rate"]))
    for subset in series:
        series[subset].sort(key=lambda p: p[0])
    return series


def markdown_table(series: dict) -> str:
    lines = ["| subset | compression → success points (ratio, success) |", "|---|---|"]
    for subset, pts in sorted(series.items()):
        cells = ", ".join(f"({r*100:.0f}%, {s*100:.0f}%)" for r, s in pts)
        lines.append(f"| {SUBSET_LABELS.get(subset, subset)} | {cells} |")
    return "\n".join(lines)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--summary", default=str(Path(__file__).parent / "summary.json"))
    ap.add_argument("--out", default=str(Path(__file__).parent / "pareto.png"))
    args = ap.parse_args()

    series = load(Path(args.summary))
    if not series:
        raise SystemExit("no scored data in summary.json — run judge.py first")

    print(markdown_table(series))

    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except ImportError:
        print("\n(matplotlib not installed — table above is the deliverable; `pip install matplotlib` for pareto.png)")
        return

    fig, ax = plt.subplots(figsize=(7, 4.5))
    for subset, pts in sorted(series.items()):
        xs = [p[0] * 100 for p in pts]
        ys = [p[1] * 100 for p in pts]
        ax.plot(xs, ys, marker="o", label=SUBSET_LABELS.get(subset, subset))
    ax.set_xlabel("mean compression ratio (%)")
    ax.set_ylabel("task success rate (%)")
    ax.set_title("tokencut: compression vs task success (Pareto)")
    ax.grid(True, alpha=0.3)
    ax.legend()
    fig.tight_layout()
    fig.savefig(args.out, dpi=150)
    print(f"\nsaved -> {args.out}")


if __name__ == "__main__":
    main()

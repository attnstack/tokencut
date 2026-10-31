#!/usr/bin/env python3
"""gen_tasks.py — deterministic benchmark task generator.

Generates bench/tasks.jsonl with 250 tasks:
  T1 (100) JSON QA over GitHub-API-shaped repo payloads
  T2 (50)  log incident triage over synthetic service logs with an injected fault
  T3 (50)  code comprehension over small real-style snippets (judge-scored)
  T4 (50)  record-scoped QA over structured "RAG" passage sets

Honesty note (also in benchmark-methodology.md): fixtures are SYNTHETIC but
shape-realistic, generated with a fixed seed so every run is byte-identical.
They exercise the compression pipeline's failure modes (does the folded
field survive?), not the diversity of production traffic. Real-model runs on
live data are the follow-up; see `--fetch-live` placeholder in the docs.

Task record fields:
  id, subset, check ("exact"|"contains"|"judge"),
  ground_truth, question, context (list of message dicts)
"""

from __future__ import annotations

import argparse
import json
import random
from pathlib import Path

SEED = 20261031

T1_KEY_POOL = [
    ("stargazers_count", "number"),
    ("open_issues_count", "number"),
    ("forks_count", "number"),
    ("default_branch", "string"),
    ("created_at", "string"),
    ("spdx_id", "nested"),  # asked via the license object
]

T1_MSG_POOL = [
    "Fix nil pointer deref in retry path",
    "Add pprof endpoint",
    "Bump dependency versions",
    "Refactor connection pool shutdown",
    "Handle context cancellation in worker",
    "Fix off-by-one in pagination",
    "Add request ID to error logs",
    "Cache compiled templates",
]

T3_SNIPPETS = [
    (
        "retry_with_backoff",
        '''def retry_with_backoff(fn, attempts=5, base=0.2, factor=2.0):
    delay = base
    for i in range(attempts):
        try:
            return fn()
        except TransientError:
            if i == attempts - 1:
                raise
            time.sleep(delay)
            delay *= factor
    raise RuntimeError("unreachable")
''',
        "What does retry_with_backoff do after the final failed attempt?",
        "It re-raises the last TransientError to the caller.",
    ),
    (
        "parse_header",
        '''def parse_header(line):
    """Parse 'Key: value' into a tuple, or None if malformed."""
    if ":" not in line:
        return None
    key, _, value = line.partition(":")
    key = key.strip()
    if not key:
        return None
    return (key, value.strip())
''',
        "What does parse_header return when the line has no colon?",
        "None.",
    ),
    (
        "sliding_window",
        '''def sliding_window_max(nums, k):
    from collections import deque
    dq = deque()
    out = []
    for i, n in enumerate(nums):
        while dq and nums[dq[-1]] <= n:
            dq.pop()
        dq.append(i)
        if dq[0] <= i - k:
            dq.popleft()
        if i >= k - 1:
            out.append(nums[dq[0]])
    return out
''',
        "What data structure does sliding_window_max use and what does it return?",
        "A monotonic deque; it returns the maximum of each window of size k.",
    ),
    (
        "merge_intervals",
        '''def merge_intervals(intervals):
    intervals = sorted(intervals)
    merged = []
    for start, end in intervals:
        if merged and start <= merged[-1][1]:
            merged[-1][1] = max(merged[-1][1], end)
        else:
            merged.append([start, end])
    return merged
''',
        "When does merge_intervals merge two intervals?",
        "When the next start is less than or equal to the previous merged end.",
    ),
]

T4_FIELDS = ["launch_year", "price_usd", "team_size", "region"]

def gen_repo(i: int, rng: random.Random) -> dict:
    topics = [f"topic-{rng.randint(0, 30)}" for _ in range(24)]
    commits = [
        {
            "sha": f"{rng.getrandbits(64):016x}",
            "author": f"dev-{rng.randint(1, 12)}",
            "date": f"2026-{rng.randint(1, 12):02d}-{rng.randint(1, 28):02d}",
            "message": rng.choice(T1_MSG_POOL),
            "additions": rng.randint(1, 900),
        }
        for _ in range(80)
    ]
    contributors = [
        {"login": f"dev-{n}", "contributions": rng.randint(1, 500)} for n in range(40)
    ]
    return {
        "id": 10_000_000 + i,
        "name": f"service-{i:03d}",
        "full_name": f"acme/service-{i:03d}",
        "private": False,
        "owner": {"login": "acme", "id": 900 + i, "type": "Organization"},
        "description": "Internal service used by the platform team for load testing.",
        "stargazers_count": rng.randint(0, 4000),
        "forks_count": rng.randint(0, 400),
        "open_issues_count": rng.randint(0, 200),
        "default_branch": rng.choice(["main", "develop", "release"]),
        "created_at": f"20{rng.randint(18, 25)}-{rng.randint(1, 12):02d}-01",
        "updated_at": "2026-10-01",
        "language": rng.choice(["Go", "Python", "Rust", "Java"]),
        "license": {"key": "mit", "spdx_id": rng.choice(["MIT", "Apache-2.0", "BSD-3-Clause"])},
        "topics": topics,
        "commits": commits,
        "contributors": contributors,
    }


def gen_t1(rng: random.Random, count: int) -> list[dict]:
    tasks = []
    for i in range(count):
        repo = gen_repo(i, rng)
        key, kind = rng.choice(T1_KEY_POOL)
        if kind == "nested":
            gt = repo["license"]["spdx_id"]
            question = (
                f'Based on the API response below, what is the value of "{key}" '
                f"of the license object? Answer with only the value."
            )
        else:
            gt = str(repo[key])
            question = (
                f'Based on the API response below, what is the value of "{key}" '
                f"of the repository? Answer with only the value."
            )
        ctx = json.dumps(repo, indent=1)
        tasks.append(
            {
                "id": f"t1-{i:03d}",
                "subset": "t1_json_qa",
                "check": "exact",
                "ground_truth": gt,
                "question": question,
                # Payload is its own message, question is the LAST user message
                # (real agent turn shape; the payload gets compressed, the
                # question stays exempt). Pure JSON — real tool outputs are
                # unprefixed, and a prose prefix would misroute the message to
                # the text pipeline instead of the JSON pruner.
                "context": [
                    {"role": "system", "content": "Answer using only the provided context. Be terse."},
                    {"role": "user", "content": ctx},
                ],
            }
        )
    return tasks


LOG_NORMAL = [
    "INFO serving request req_id={n} path=/api/v1/items",
    "INFO db query took {n}ms rows={m}",
    "INFO cache hit rate {m}% key=user:{n}",
    "INFO worker-{m} processed job {n}",
    "INFO healthcheck ok",
    "INFO gc pause {n}ms heap={m}MB",
]

def gen_log_text(rng: random.Random, fault_host: str, fault_port: int, n_lines: int) -> str:
    lines = []
    for i in range(n_lines):
        ts = f"2026-10-31 09:{i // 60:02d}:{i % 60:02d}.{i % 1000:03d}"
        tpl = rng.choice(LOG_NORMAL)
        line = f"{ts} {tpl.format(n=rng.randint(1, 9999), m=rng.randint(1, 99))}"
        lines.append(line)
        # Inject the recurring fault around the 75% mark; decoys appear 1-2
        # times only so "most frequent failure" is unambiguous.
        if abs(i - n_lines * 3 // 4) < n_lines // 16:
            lines.append(
                f"{ts} ERROR Failed to connect to {fault_host}:{fault_port} (timeout after 2000ms)"
            )
        if i % (n_lines // 40) == 0:
            decoy = f"10.9.{rng.randint(0, 9)}.{rng.randint(0, 9)}:{rng.randint(6000, 6999)}"
            lines.append(f"{ts} WARN slow upstream {decoy} responded in 900ms")
    return "\n".join(lines)


def gen_t2(rng: random.Random, count: int) -> list[dict]:
    tasks = []
    for i in range(count):
        host = f"10.0.{rng.randint(1, 30)}.{rng.randint(1, 250)}"
        port = rng.choice([5432, 6379, 9092, 8123])
        text = gen_log_text(rng, host, port, rng.randint(3500, 6000))
        question = (
            "These are service logs around an incident. "
            "What host:port shows repeated connection failures? Answer with only host:port."
        )
        tasks.append(
            {
                "id": f"t2-{i:03d}",
                "subset": "t2_log_triage",
                "check": "exact",
                "ground_truth": f"{host}:{port}",
                "question": question,
                "context": [
                    {"role": "system", "content": "You are an on-call engineer. Answer from the logs only."},
                    {"role": "user", "content": text},
                ],
            }
        )
    return tasks


def gen_t3(rng: random.Random, count: int) -> list[dict]:
    tasks = []
    for i in range(count):
        name, code, question, gt = T3_SNIPPETS[i % len(T3_SNIPPETS)]
        variation = f"# variant {i // len(T3_SNIPPETS)}, helper for module-{i % 7}\n"
        tasks.append(
            {
                "id": f"t3-{i:03d}",
                "subset": "t3_code_qa",
                "check": "judge",  # scored by the LLM judge in real mode
                "ground_truth": gt,
                "question": question + " Answer in one sentence.",
                "context": [
                    {"role": "system", "content": "You are a senior engineer. Answer about the code only."},
                    {"role": "user", "content": "```python\n" + variation + code + "```"},
                ],
            }
        )
    return tasks


def gen_t4(rng: random.Random, count: int) -> list[dict]:
    tasks = []
    for i in range(count):
        n_products = 25
        target = rng.randint(0, n_products - 1)
        records = []
        for j in range(n_products):
            rec = {
                "product": f"product-{j:03d}" if j != target else f"Zephyr-{i:03d}",
                "category": rng.choice(["db", "queue", "cache", "monitor"]),
                "launch_year": 2015 + rng.randint(0, 10),
                "price_usd": rng.randint(20, 900),
                "team_size": rng.randint(2, 40),
                "region": rng.choice(["us", "eu", "apac"]),
                "notes": "internal record; " + "x" * rng.randint(20, 60),
            }
            records.append(rec)
        key = rng.choice([k for k in T4_FIELDS])
        gt = str(records[target][key])
        question = (
            f'According to the records, what is the value of "{key}" '
            f'for the product "Zephyr-{i:03d}"? Answer with only the value.'
        )
        ctx = json.dumps(records, indent=1)
        tasks.append(
            {
                "id": f"t4-{i:03d}",
                "subset": "t4_record_qa",
                "check": "exact",
                "ground_truth": gt,
                "question": question,
                "context": [
                    {"role": "system", "content": "Answer using only the provided records. Be terse."},
                    {"role": "user", "content": ctx},
                ],
            }
        )
    return tasks


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", default=str(Path(__file__).parent / "tasks.jsonl"))
    args = ap.parse_args()

    rng = random.Random(SEED)
    tasks = gen_t1(rng, 100) + gen_t2(rng, 50) + gen_t3(rng, 50) + gen_t4(rng, 50)
    out = Path(args.out)
    with out.open("w", encoding="utf-8") as f:
        for t in tasks:
            f.write(json.dumps(t, ensure_ascii=False) + "\n")
    print(f"wrote {len(tasks)} tasks -> {out}")


if __name__ == "__main__":
    main()

# Benchmark methodology

Every number quoted in the README traces back to this file. If a number
isn't covered here, don't quote it.

## 1. Task set

250 tasks, generated deterministically by `gen_tasks.py` (fixed seed, one
command, byte-identical output):

| subset | n | content | scoring |
|---|---|---|---|
| T1 JSON QA | 100 | GitHub-API-shaped repo payloads (80 commits, 40 contributors of filler), question targets one field | deterministic exact match |
| T2 log triage | 50 | synthetic service logs (3500–6000 lines), recurring fault injected at ~75% + sparse decoys | exact match (host:port) |
| T3 code QA | 50 | 4 real-style Python snippets × variants, asks about behavior | LLM judge (rubric 1–5, ≥4 = pass) |
| T4 record QA | 50 | 25 structured records, look up one field by product name | exact match |

Honest caveats:

1. The fixtures are synthetic but shape-realistic. They are designed to
   probe the compression pipeline's failure modes (does the folded field
   survive?) and do not represent the diversity of production traffic.
   Replaying real traces is future work.
2. The offline mode (`--dry-run` with `--mock-upstream`) runs against a
   built-in reference solver (a lookup/regex extractor), not a language
   model. Those numbers validate the pipeline and must never be quoted as
   model quality.

## 2. Compression-ratio metric

- Primary metric: tokens. Counts come from tokencut's estimator
  (chars ÷ per-kind constants, see `internal/tokenest`), exposed via the
  `x-tokencut-original-tokens` / `x-tokencut-compressed-tokens` headers.
  It is not tokenizer-exact by design: the pruner needs an O(n),
  dependency-free unit for scoring.
- Calibration (done 2026-10-31): `calibrate.py` fits per-kind constants
  against tiktoken on the benchmark corpus. This replaced the original
  global constant (~4 chars/token), which measured 25–41% off on JSON/log
  payloads:

  | kind | cl100k chars/token | o200k deviation |
  |---|---|---|
  | json | 3.12 | ±0% |
  | log | 2.34 | ±0% |
  | code | 3.80 | ~1% |
  | text | 4.46 | ~0.5% |
  | wide (CJK) | 0.74 | 1.02 |

  Reproduce with `python bench/calibrate.py` (needs a locally cached
  tiktoken vocabulary).

- Known residual, stated plainly: within the JSON kind there is ~±17%
  variance (hex shas tokenize differently from repeated keys). This affects
  the absolute precision of savings figures, not pruning correctness (the
  pruner's internal accounting is exact additive rune counts), and only
  makes budget targets slightly over- or under-shoot.

## 3. Tiers and baselines

| tier | meaning |
|---|---|
| none | goes through the proxy with compression disabled |
| lossless | canonicalization + dedup only, zero information change |
| b0.5 / b0.7 / b0.9 | exact target compression 50% / 70% / 90% |
| balanced / aggressive | default tiers (60% / 90%) |

Planned baselines not yet run: llmlingua at equal token budget (its output
on JSON isn't valid JSON, which is the point of the comparison), and a
rule-based pruner ablation (scoring removed) to check that the scoring
actually beats fixed rules.

## 4. Run parameters and the actual run

Plan (hosted API, not yet executed): single strong model, N=3, budget < $5.

Actual run (2026-10-31, local model):

- Model: `qwen2.5:7b` on Ollama 0.40.7, derived as `tokencut-eval` with
  `num_ctx 16384` only. (Ollama's default 4k context silently truncates
  the uncompressed payloads and would have broken the baseline.)
- 250 tasks × 7 tiers × N=1 (deviation from the planned N=3: temp=0 and
  80% of tasks use deterministic scoring; the T3 judge score's single-run
  variance is therefore unquantified) = 1750 calls, 0 errors.
- Aggregate: 46,938,042 → 8,715,988 tokens (81.4% saved); end-to-end
  latency 2.24 s (none) → 0.49–0.85 s (compressed tiers), means.
- Reproduce: create the derived model, start the proxy with
  `TOKENCUT_UPSTREAM=http://localhost:11434`, then
  `python bench/run.py --model tokencut-eval --runs 1` and
  `python bench/judge.py --judge-model tokencut-eval`.

Key finding (T2 feasibility flip): T2 payloads are 106,893–151,059 tokens,
7–10× over the 16k window. Uncompressed, the model sees only the first
~15% (the fault is at the 75% mark, outside the window) and answers all 50
tasks with the same hallucinated address that appears nowhere in the logs.
Compressed to ~1,050 tokens, it answers 50/50 correctly. On the Pareto
chart these two points should be read as "task infeasible", not "0%".

## 5. Judging

- T1/T2/T4 (80% of samples): deterministic matching, zero judge noise.
- T3: judged by qwen2.5:7b itself (rubric 1–5, two runs). Self-consistency
  350/350 = 100%. Limitation, stated plainly: self-judging risks optimism
  (same model answers and grades), so treat 74% as the model's relative
  level on this task family, not a cross-model number. Judging with an
  independent strong model is a follow-up.
- Judge scores are cached in a sidecar file so re-printing tables never
  re-calls the model.

## 6. Significance

- Each cell reports the success rate with a Bernoulli std across tasks.
  With N=1 this excludes run-to-run variance (near zero for deterministic
  scoring at temp=0; unquantified for T3).
- Differences within 2×std must be described as "within noise", never as
  "no loss" or "a loss". Claims below 5 pp are not made (sample size gives
  ~0.8 power for ≥5 pp differences).
- This run: T1/T2/T4 differences are far beyond 5 pp; T3 shows no
  difference across tiers, and none is claimed.

## 7. Pre-publication checklist

- [x] every number traces to a section here plus the command that produced it
- [x] test set / sample size / baseline / std all stated
- [x] offline reference-solver numbers are never quoted as model quality
- [x] judge self-consistency measured, self-judging limitation declared
- [x] N=1 deviation declared (§4)
- [ ] rerun on a hosted strong model with N=3 (cross-model matrix)
- [ ] llmlingua comparison at equal budget

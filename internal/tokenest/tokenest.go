// Package tokenest estimates LLM token counts from character statistics.
// Why an estimator instead of a real tokenizer: the pruner calls est() on
// every node during scoring, so it must be O(n) with tiny constants and zero
// dependencies.
//
// Constants are PER CONTENT KIND and were fitted against tiktoken
// (cl100k_base; o200k_base ASCII constants agree within 1%) on the benchmark
// corpus — run bench/calibrate.py to reproduce or recalibrate. The original
// single global constant (ASCII≈4) measured 25–41% error on JSON/log
// payloads; per-kind constants removed it for homogeneous classes. Known
// residual: the JSON class itself has ~17% internal variance (hex shas vs
// repeated keys tokenize differently) — this shifts reported savings by that
// margin and is documented in bench/benchmark-methodology.md; it only makes
// budget decisions slightly over/under-shoot, never affects output validity.
package tokenest

import "math"

type kindConstants struct {
	ascii float64 // ASCII runes per token
	wide  float64 // wide (non-ASCII) runes per token
}

var kindTable = map[string]kindConstants{
	// cl100k_base fits on bench/tasks.jsonl payloads (2026-10-31):
	"json":  {3.12, 0.74},
	"log":   {2.34, 0.74},
	"code":  {3.80, 0.74},
	"text":  {4.46, 0.74},
	"table": {3.5, 0.74}, // interpolated; no dedicated corpus subset yet
	"html":  {3.5, 0.74}, // interpolated; v1 routes html to conservative paths
}

// ConfigureKind overrides one kind's constants (chars per token).
// Values <= 0 are ignored so partial overrides are safe. Exposed for
// config.yaml recalibration — see bench/calibrate.py.
func ConfigureKind(kind string, ascii, wide float64) {
	c, ok := kindTable[kind]
	if !ok {
		c = kindTable["text"]
	}
	if ascii > 0 {
		c.ascii = ascii
	}
	if wide > 0 {
		c.wide = wide
	}
	kindTable[kind] = c
}

func constsFor(kind string) kindConstants {
	if c, ok := kindTable[kind]; ok {
		return c
	}
	return kindTable["text"]
}

// EstimateFor returns the estimated token count of s, priced at the
// constants for the given content kind (see classify.Kind values:
// json/log/code/html/table/text). Unknown kinds fall back to text.
// Empty input is 0 tokens.
func EstimateFor(kind, s string) int {
	if s == "" {
		return 0
	}
	t := estimateFloatFor(kind, s)
	if t < 1 {
		return 1
	}
	return int(math.Ceil(t))
}

// Estimate is EstimateFor("text", s) — kept for callers without a kind.
func Estimate(s string) int { return EstimateFor("text", s) }

// estimateFloatFor is the unrounded core. Escaped JSON counts as its
// serialized form, which matches how a tokenizer sees the wire format.
func estimateFloatFor(kind, s string) float64 {
	if s == "" {
		return 0
	}
	c := constsFor(kind)
	ascii, wide := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			wide++
		}
	}
	return float64(ascii)/c.ascii + float64(wide)/c.wide
}

// RunesForTokens converts a token budget into the rune budget the pruner
// uses internally (its accounting is exact additive rune counts; see
// compress/jsonpruner.go). Only used for JSON payloads, so priced at the
// json constants. Conservative: assumes the ASCII ratio.
func RunesForTokens(tokens int) int {
	if tokens <= 0 {
		return 0
	}
	return int(float64(tokens) * constsFor("json").ascii)
}

package budget

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"tokencut/internal/compress"
)

func benchJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// BenchmarkFullPipeline measures budget.Apply end to end on a realistic agent
// turn (system + big tool output + question) — this is the number that
// matters for "proxy overhead vs LLM latency".
func BenchmarkFullPipeline(b *testing.B) {
	request := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":%s},{"role":"user","content":"Summarize the item with id 42."}]}`,
		benchJSONString(benchPayloadB(500)))
	b.ReportAllocs()
	b.SetBytes(int64(len(request)))
	for i := 0; i < b.N; i++ {
		out, rep, err := newBenchController().Apply([]byte(request))
		if err != nil || !rep.Changed {
			b.Fatalf("pipeline: %v changed=%v", err, rep.Changed)
		}
		_ = out
	}
}

func newBenchController() *Controller {
	cfg := DefaultConfig()
	cfg.LatencyBudget = time.Second
	cfg.Cache = compress.NewLRU(1024)
	return NewController(cfg)
}

func benchPayloadB(items int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < items; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id": %d, "name": "item-%d", "status": "active", "payload": "%s", "meta": {"created": "2026-10-%02d", "score": %d}}`,
			i, i, strings.Repeat("x", 60), i%28+1, i*7%100)
	}
	b.WriteByte(']')
	return b.String()
}

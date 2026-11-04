package budget

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"tokencut/internal/compress"
)

// Pipeline-level goldens: freeze byte-exact Apply() output for requests
// exercising prune / dedup / diff / multimodal-skip. Performance refactors
// in the compress package must keep these identical.

type pipelineGoldenCase struct {
	Name   string `json:"name"`
	Output string `json:"output"`
}

func goldenJSONStringP(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func goldenBigArray(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id": %d, "name": "item-%d", "status": "active", "payload": "%s"}`, i, i, strings.Repeat("x", 60))
	}
	b.WriteByte(']')
	return b.String()
}

func goldenController() *Controller {
	cfg := DefaultConfig()
	cfg.LatencyBudget = 5 * time.Second // goldens must not hit the deadline
	cfg.Cache = nil                     // cache-free: outputs independent of call order
	return NewController(cfg)
}

func TestGoldenPipeline(t *testing.T) {
	big := goldenBigArray(120)
	v1 := strings.Repeat("stable line of the file content\n", 150)
	v2 := v1 + "one appended line at the end\n"
	type spec struct {
		name string
		body string
	}
	reqs := []spec{
		{"prune-request", fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"system","content":"You are terse."},{"role":"user","content":%s},{"role":"user","content":%s}]}`,
			goldenJSONStringP(big), goldenJSONStringP(`what is the value of "status" for "item-7"? Answer with only the value.`))},
		{"dedup-request", fmt.Sprintf(`{"messages":[{"role":"user","content":%s},{"role":"user","content":%s},{"role":"user","content":%s}]}`,
			goldenJSONStringP(big), goldenJSONStringP(big), goldenJSONStringP("summarize"))},
		{"diff-request", fmt.Sprintf(`{"messages":[{"role":"user","content":%s},{"role":"user","content":%s}]}`,
			goldenJSONStringP(v1), goldenJSONStringP(v2))},
		{"multimodal-request", fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"text","text":%s},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
			goldenJSONStringP(big))},
	}
	cases := make([]pipelineGoldenCase, 0, len(reqs))
	ctrl := goldenController()
	for _, r := range reqs {
		out, _, err := ctrl.Apply([]byte(r.body))
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		cases = append(cases, pipelineGoldenCase{Name: r.name, Output: string(out)})
	}
	path := "testdata/golden/pipeline_golden.json"
	if os.Getenv("GOLDEN_UPDATE") != "" {
		if err := os.MkdirAll("testdata/golden", 0o755); err != nil {
			t.Fatal(err)
		}
		b, _ := json.MarshalIndent(cases, "", " ")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden file missing (run with GOLDEN_UPDATE=1 once): %v", err)
	}
	var wantCases []pipelineGoldenCase
	if err := json.Unmarshal(want, &wantCases); err != nil {
		t.Fatalf("golden corrupt: %v", err)
	}
	for i, c := range wantCases {
		if c.Output != cases[i].Output {
			t.Errorf("golden mismatch in %s: pipeline output changed", c.Name)
		}
	}
	var _ = compress.DefaultDrainConfig // referenced so the import stays honest if cases change
}

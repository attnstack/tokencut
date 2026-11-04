package compress

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tokencut/internal/tokenest"
)

// Golden tests freeze the byte-exact output of the compressors. Performance
// refactors must keep these files identical: if this test fails after a
// "pure optimization", the optimization changed semantics. Regenerate with
// GOLDEN_UPDATE=1 (and inspect the diff!).

type goldenCase struct {
	Name   string `json:"name"`
	Output string `json:"output"`
}

func goldenPath(name string) string {
	return filepath.Join("testdata", "golden", name)
}

func compareGolden(t *testing.T, file string, cases []goldenCase) {
	t.Helper()
	want, err := os.ReadFile(goldenPath(file))
	if err != nil {
		t.Fatalf("golden file missing (run with GOLDEN_UPDATE=1 once): %v", err)
	}
	var wantCases []goldenCase
	if err := json.Unmarshal(want, &wantCases); err != nil {
		t.Fatalf("golden file corrupt: %v", err)
	}
	if len(wantCases) != len(cases) {
		t.Fatalf("golden case count changed: want %d got %d", len(wantCases), len(cases))
	}
	for i, c := range cases {
		if c.Output != wantCases[i].Output {
			t.Errorf("golden mismatch in %s[%d] (%s): output changed.\n--- want (%d bytes) ---\n%s\n--- got (%d bytes) ---\n%s",
				file, i, c.Name, len(wantCases[i].Output), truncGolden(wantCases[i].Output, 500), len(c.Output), truncGolden(c.Output, 500))
		}
	}
}

func writeGolden(t *testing.T, file string, cases []goldenCase) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(goldenPath(file)), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(cases, "", " ")
	if err := os.WriteFile(goldenPath(file), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func truncGolden(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func goldenJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func goldenArray(n int) string {
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

func benchScalars(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"v": %d, "tag": "t%d"}`, i, i%7)
	}
	b.WriteByte(']')
	return b.String()
}

func nestedMixed() string {
	var b strings.Builder
	b.WriteString(`{"report": {"generated": "2026-11-01", "items": [`)
	for i := 0; i < 60; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		status, errMsg := "ok", ""
		if i%9 == 0 {
			status, errMsg = "failed", "ECONNRESET"
		}
		fmt.Fprintf(&b, `{"id": %d, "status": "%s", "error": "%s", "detail": "%s", "meta": {"retries": %d, "host": "host-%d"}}`,
			i, status, errMsg, strings.Repeat("d", 40), i%4, i%8)
	}
	b.WriteString(`], "totals": {"ok": 53, "failed": 7}, "cursor": "abc123"}}`)
	return b.String()
}

func unicodeDoc() string {
	var b strings.Builder
	b.WriteString(`{"标题": "测试文档", "条目": [`)
	for i := 0; i < 40; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"名称": "条目-%d", "说明": "这是一段比较长的中文说明内容用来测试宽字符估算路径", "值": %d}`, i, i*3)
	}
	b.WriteString(`]}`)
	return b.String()
}

func hvDoc() string {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < 30; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"field%d": "%s"`, i, strings.Repeat("x", 50))
	}
	b.WriteString(`, "id": 42, "status": "active", "url": "https://example.com/webhook"}`)
	return b.String()
}

func deepNest(depth int) string {
	return strings.Repeat(`{"a":`, depth) + `{"leaf": "` + strings.Repeat("v", 80) + `"}` + strings.Repeat("}", depth)
}

func longStrings() string {
	return `{"blob": "` + strings.Repeat("z", 6000) + `", "notes": "` + strings.Repeat("n", 3000) + `", "id": 7}`
}

func numbersDoc() string {
	return `{"a": 12345678901234567890, "b": 0.1, "c": 1e100, "neg": -42, "arr": [1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25]}`
}

func goldenPruneCases(t *testing.T) []goldenCase {
	t.Helper()
	type spec struct {
		name     string
		input    string
		frac     float64
		fixed    int
		keywords []Keyword
	}
	specs := []spec{
		{"array-200-half", goldenArray(200), 0.5, 0, nil},
		{"array-200-tenth", goldenArray(200), 0.1, 0, nil},
		{"array-200-tiny", goldenArray(200), 0, 80, nil},
		{"array-500-aggressive", goldenArray(500), 0.1, 0, nil},
		{"array-scalars-300", benchScalars(300), 0.3, 0, nil},
		{"nested-mixed", nestedMixed(), 0.4, 0, nil},
		{"keyword-needle", goldenArray(80), 0.25, 0, SimpleKeywords("needle")},
		{"keyword-status", nestedMixed(), 0.5, 0, SimpleKeywords("status", "error")},
		{"unicode", unicodeDoc(), 0.4, 0, nil},
		{"hv-keys", hvDoc(), 0.3, 0, nil},
		{"deep-200", deepNest(200), 0.3, 0, nil},
		{"long-strings", longStrings(), 0.25, 0, nil},
		{"numbers", numbersDoc(), 0.3, 0, nil},
		{"within-budget", goldenArray(50), 0, 1 << 20, nil},
	}
	cases := make([]goldenCase, 0, len(specs))
	for _, s := range specs {
		budget := s.fixed
		if s.frac > 0 {
			budget = int(float64(tokenest.EstimateFor("json", s.input)) * s.frac)
		}
		out, _, err := PruneJSON(s.input, budget, s.keywords, DefaultJSONPruneConfig())
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		cases = append(cases, goldenCase{Name: s.name, Output: out})
	}
	return cases
}

func TestGoldenPrune(t *testing.T) {
	cases := goldenPruneCases(t)
	if os.Getenv("GOLDEN_UPDATE") != "" {
		writeGolden(t, "prune_golden.json", cases)
		return
	}
	compareGolden(t, "prune_golden.json", cases)
}

func goldenDrainCases() []goldenCase {
	var b1, b2, b3 strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b1, "2026-10-31 10:%02d:%02d INFO GET /api/users/%d 200 %dms\n", (i/60)%60, i%60, i, i%900)
	}
	samples := []string{
		"2026-10-31 ERROR Failed to connect to 10.0.3.18:5432 (timeout after 2000ms)",
		"2026-10-31 WARN slow upstream 10.9.1.2:6379 responded in 900ms",
		"INFO healthcheck ok",
	}
	for i := 0; i < 4000; i++ {
		for j, s := range samples {
			if (i+j)%4 == 0 {
				b2.WriteString(s)
				b2.WriteString("\n")
			}
		}
	}
	for i := 0; i < 800; i++ {
		fmt.Fprintf(&b3, "2026-11-01 09:%02d:%02d.%03d worker-%d processed job %d in %dms\n", (i/60)%60, i%60, i%1000, i%16, i, i%500)
	}
	cases := make([]goldenCase, 0, 4)
	for i, in := range []string{b1.String(), b2.String(), b3.String(), "too\nfew\nlines\nhere"} {
		res := DrainCompress(in, DefaultDrainConfig())
		out := res.Output
		if !res.OK {
			out = "NOT_OK:" + res.Reason
		}
		cases = append(cases, goldenCase{Name: fmt.Sprintf("drain-%d", i), Output: out})
	}
	return cases
}

func TestGoldenDrain(t *testing.T) {
	cases := goldenDrainCases()
	if os.Getenv("GOLDEN_UPDATE") != "" {
		writeGolden(t, "drain_golden.json", cases)
		return
	}
	compareGolden(t, "drain_golden.json", cases)
}

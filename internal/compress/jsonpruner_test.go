package compress

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"tokencut/internal/tokenest"
)

func bigArrayJSON(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id": %d, "name": "item-%d", "payload": "%s"}`, i, i, strings.Repeat("x", 40))
	}
	b.WriteByte(']')
	return b.String()
}

func TestPruneArrayWithinBudgetKeepsOriginal(t *testing.T) {
	in := bigArrayJSON(5)
	out, rep, err := PruneJSON(in, 10000, nil, DefaultJSONPruneConfig())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !rep.Kept || out != in {
		t.Errorf("expected untouched output, kept=%v", rep.Kept)
	}
}

func TestPruneArrayFold(t *testing.T) {
	in := bigArrayJSON(100)
	budget := tokenest.EstimateFor("json", in) / 4
	out, rep, err := PruneJSON(in, budget, nil, DefaultJSONPruneConfig())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("output not valid JSON: %.120s", out)
	}
	if rep.FinalTokens > budget {
		t.Errorf("budget not met: %d > %d", rep.FinalTokens, budget)
	}
	if !strings.Contains(out, `"_omitted"`) {
		t.Errorf("expected omission marker in: %.200s", out)
	}
	if rep.Folds == 0 {
		t.Errorf("expected folds > 0")
	}
	// head/tail semantics: first and last ids survive.
	if !strings.Contains(out, `"item-0"`) || !strings.Contains(out, `"item-99"`) {
		t.Errorf("head/tail items missing: %.200s", out)
	}
}

func TestPruneObjectFoldKeepsHighValueKeys(t *testing.T) {
	in := `{"data": {"junk1": "` + strings.Repeat("a", 60) + `", "junk2": "` + strings.Repeat("b", 60) + `"}, "id": 7}`
	out, _, err := PruneJSON(in, 20, nil, DefaultJSONPruneConfig())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, `"id":7`) {
		t.Errorf("high-value key id lost: %s", out)
	}
	if !strings.Contains(out, "fields omitted") {
		t.Errorf("expected object fold marker: %s", out)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}

func TestPruneKeepsQueryRelevantItems(t *testing.T) {
	var items []string
	for i := 0; i < 60; i++ {
		items = append(items, fmt.Sprintf(`{"name": "alpha-%d", "payload": "%s"}`, i, strings.Repeat("p", 50)))
	}
	items = append(items, `{"name": "needle-42", "payload": "unique needle content here"}`)
	in := "[" + strings.Join(items, ",") + "]"
	out, rep, err := PruneJSON(in, tokenest.EstimateFor("json", in)/3, SimpleKeywords("needle"), DefaultJSONPruneConfig())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "needle-42") {
		t.Errorf("query-relevant item dropped:\n%.300s", out)
	}
	if rep.FinalTokens >= rep.OriginalTokens {
		t.Errorf("no compression: %d -> %d", rep.OriginalTokens, rep.FinalTokens)
	}
}

func TestPruneInvalidJSONFails(t *testing.T) {
	for _, in := range []string{"not json", `{"a":1} trailing`, "{", ""} {
		if _, _, err := PruneJSON(in, 10, nil, DefaultJSONPruneConfig()); err == nil {
			t.Errorf("expected ErrNotJSON for %q", in)
		}
	}
}

func TestPruneLongStringCap(t *testing.T) {
	in := `{"blob": "` + strings.Repeat("z", 5000) + `"}`
	out, rep, err := PruneJSON(in, 500, nil, DefaultJSONPruneConfig())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.StringsCapped == 0 {
		t.Errorf("expected string capping")
	}
	if !strings.Contains(out, "chars truncated") {
		t.Errorf("expected truncation marker: %.120s", out)
	}
	if !json.Valid([]byte(out)) {
		t.Errorf("capped output invalid: %.120s", out)
	}
}

func TestPruneDeepNestingNoCrash(t *testing.T) {
	depth := 1000
	in := strings.Repeat(`{"a":`, depth) + "1" + strings.Repeat("}", depth)
	out, _, err := PruneJSON(in, 200, nil, DefaultJSONPruneConfig())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !json.Valid([]byte(out)) {
		t.Errorf("deep output invalid")
	}
}

func TestPruneUnicodeAndNumbers(t *testing.T) {
	in := `{"中文": "你好世界，这是一段比较长的中文内容用来测试估算器", "pi": 3.14159265358979, "big": 1e100, "neg": -42}`
	out, _, err := PruneJSON(in, tokenest.EstimateFor("json", in)+10, nil, DefaultJSONPruneConfig())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != in {
		t.Errorf("within-budget input must be untouched\nin:  %s\nout: %s", in, out)
	}
	out, _, err = PruneJSON(in, 15, nil, DefaultJSONPruneConfig())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "中文") && !strings.Contains(out, "omitted") {
		t.Errorf("unicode content mangled: %s", out)
	}
	if !json.Valid([]byte(out)) {
		t.Errorf("unicode output invalid: %s", out)
	}
}

func TestPrunePreservesNumbersExactly(t *testing.T) {
	// json.Number path: big integers must survive round-trip unchanged.
	in := `{"a": 12345678901234567890, "b": 0.1, "c": [1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20]}`
	out, _, err := PruneJSON(in, 30, nil, DefaultJSONPruneConfig())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "12345678901234567890") {
		t.Errorf("big integer precision lost: %s", out)
	}
}

func TestPruneDeterministic(t *testing.T) {
	in := bigArrayJSON(50)
	a, _, _ := PruneJSON(in, 80, nil, DefaultJSONPruneConfig())
	b, _, _ := PruneJSON(in, 80, nil, DefaultJSONPruneConfig())
	if a != b {
		t.Errorf("same input must produce byte-identical output")
	}
}

// FuzzPruneJSON: the invariant that matters — for ANY valid JSON input and
// any budget, the output must be valid JSON and never larger than the input.
func FuzzPruneJSON(f *testing.F) {
	seeds := []string{
		`{}`, `[]`, `null`, `{"a":1}`, `[1,2,3]`,
		bigArrayJSON(20),
		`{"中文":"你好","nested":{"deep":{"deeper":[1,2,3,{"x":"y"}]}}}`,
		`{"dup":1,"dup":2,"esc":"a\"b\\c\n\t","empty":""}`,
		`{"big": 123456789012345678901234567890, "f": 1.5e-300}`,
		`[[[[[[[[[[1]]]]]]]]]]`,
		`{"a": true, "b": false, "c": null, "d": {"id": 9, "list": [1,2,3,4,5,6,7,8,9,10]}}`,
	}
	for _, s := range seeds {
		f.Add(s, 50)
	}
	f.Fuzz(func(t *testing.T, data string, budget int) {
		if budget < 0 || budget > 1<<20 {
			budget = 50
		}
		if !json.Valid([]byte(data)) {
			t.Skip()
		}
		out, _, err := PruneJSON(data, budget, SimpleKeywords("x"), DefaultJSONPruneConfig())
		if err != nil {
			if out != data {
				t.Fatalf("error path must return original input, got modified output")
			}
			return
		}
		if !json.Valid([]byte(out)) {
			t.Fatalf("output not valid JSON\nin:  %.200s\nout: %.200s", data, out)
		}
		if tokenest.Estimate(out) > tokenest.Estimate(data) {
			t.Fatalf("output larger than input\nin:  %.120s\nout: %.120s", data, out)
		}
	})
}

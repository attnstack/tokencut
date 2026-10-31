package yamlite

import (
	"reflect"
	"testing"
)

func TestParseNestedAndFlow(t *testing.T) {
	src := `
# top comment
listen: "127.0.0.1:8787"     # inline comment
upstream: https://api.openai.com
tier: balanced
targets: {balanced: 0.60, aggressive: 0.90}
exempt:
  system: true
  last_user: true
pricing:
  "gpt-4o": {input: 2.50, output: 10.00}
high_value_keys: [id, name, type, "with space"]
rate: 0
`
	m, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m["listen"] != "127.0.0.1:8787" {
		t.Errorf("listen = %v", m["listen"])
	}
	if m["upstream"] != "https://api.openai.com" {
		t.Errorf("unquoted URL with colon failed: %v", m["upstream"])
	}
	targets, ok := m["targets"].(map[string]any)
	if !ok || targets["balanced"] != 0.60 || targets["aggressive"] != 0.90 {
		t.Errorf("targets = %v", m["targets"])
	}
	exempt, ok := m["exempt"].(map[string]any)
	if !ok || exempt["system"] != true || exempt["last_user"] != true {
		t.Errorf("nested block map failed: %v", m["exempt"])
	}
	pricing, ok := m["pricing"].(map[string]any)
	if !ok {
		t.Fatalf("pricing = %v", m["pricing"])
	}
	gpt, ok := pricing["gpt-4o"].(map[string]any)
	if !ok || gpt["input"] != 2.5 || gpt["output"] != 10.0 {
		t.Errorf("nested flow map failed: %v", pricing)
	}
	keys, ok := m["high_value_keys"].([]any)
	if !ok || !reflect.DeepEqual(keys, []any{"id", "name", "type", "with space"}) {
		t.Errorf("flow list failed: %v", m["high_value_keys"])
	}
	if m["rate"] != int64(0) {
		t.Errorf("int scalar = %v (%T)", m["rate"], m["rate"])
	}
}

func TestParseScalars(t *testing.T) {
	m, err := Parse("a: true\nb: false\nc: 42\nd: -3.5\ne: null\nf: hello world\ng: 'it''s'\nh: \"line\\nbreak\"")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m["a"] != true || m["b"] != false {
		t.Errorf("bools: %v", m)
	}
	if m["c"] != int64(42) || m["d"] != -3.5 {
		t.Errorf("numbers: %v", m)
	}
	if m["e"] != nil {
		t.Errorf("null: %v", m["e"])
	}
	if m["f"] != "hello world" {
		t.Errorf("bare string: %v", m["f"])
	}
	if m["g"] != "it's" {
		t.Errorf("single-quote escape: %v", m["g"])
	}
	if m["h"] != "line\nbreak" {
		t.Errorf("double-quote escape: %v", m["h"])
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"tab indent":     "a:\n\tb: 1",
		"bad indent":     "a: 1\n  b: 2",
		"unterminated":   "a: \"oops",
		"missing key":    "just text",
		"trailing flow":  "a: {b: 1} extra",
		"unterminated f": "a: [1, 2",
	}
	for name, src := range cases {
		if _, err := Parse(src); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

func TestParseEmptyAndComments(t *testing.T) {
	m, err := Parse("\n# only comments\n\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("expected empty map, got %v", m)
	}
}

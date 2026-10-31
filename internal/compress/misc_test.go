package compress

import (
	"strings"
	"testing"

	"tokencut/internal/classify"
)

func TestLRUEviction(t *testing.T) {
	c := NewLRU(3)
	for _, k := range []string{"a", "b", "c"} {
		c.Put(k, k)
	}
	// "a" is oldest; touching it makes "b" the victim.
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a missing")
	}
	c.Put("d", "d")
	if _, ok := c.Get("b"); ok {
		t.Errorf("b should have been evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("%s should be present", k)
		}
	}
	if c.Hits != 4 || c.Evictions != 1 {
		t.Errorf("hits=%d evictions=%d", c.Hits, c.Evictions)
	}
}

func TestFingerprintStableAcrossKeyOrderAndWhitespace(t *testing.T) {
	a := `{"a": 1, "b": 2}`
	b := "{\n  \"b\": 2,\n  \"a\": 1\n}\n"
	if Fingerprint(a, classify.JSON) != Fingerprint(b, classify.JSON) {
		t.Errorf("json fingerprints must ignore key order and whitespace")
	}
	t1 := "hello   world\n\tfoo"
	t2 := "hello world foo"
	if Fingerprint(t1, classify.Text) != Fingerprint(t2, classify.Text) {
		t.Errorf("text fingerprints must collapse whitespace")
	}
	if Fingerprint(a, classify.JSON) == Fingerprint(a, classify.Text) {
		t.Errorf("kind participates in normalization")
	}
}

func TestCanonicalJSON(t *testing.T) {
	out, err := CanonicalJSON(`{ "b" : 2 , "a" : 1 }`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `{"a":1,"b":2}` {
		t.Errorf("canonical = %s", out)
	}
	// Big integers must not go through float64.
	out, err = CanonicalJSON(`{"n": 12345678901234567890}`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `{"n":12345678901234567890}` {
		t.Errorf("precision lost: %s", out)
	}
	if _, err := CanonicalJSON(`{"a":1} junk`); err == nil {
		t.Errorf("trailing garbage must fail")
	}
}

func TestSlimCode(t *testing.T) {
	src := "# comment line\nimport os\n\n\n\n# another comment\ndef f():\n    return os.getcwd()  # keep inline\n"
	out, ok := SlimCode(src)
	if !ok {
		t.Fatalf("expected slimming")
	}
	if len(out) >= len(src) {
		t.Errorf("not smaller")
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "# comment line" || line == "# another comment" {
			t.Errorf("full-line comment survived: %s", out)
		}
	}
	if !strings.Contains(out, "return os.getcwd()  # keep inline") {
		t.Errorf("inline comment or code damaged:\n%s", out)
	}
	if !strings.Contains(out, "import os") {
		t.Errorf("code lost:\n%s", out)
	}
}

func TestSlimCodeShebangKept(t *testing.T) {
	src := "#!/usr/bin/env python3\nprint(1)\n" + strings.Repeat("# c\n", 30)
	out, ok := SlimCode(src)
	if !ok {
		t.Fatalf("expected slimming")
	}
	if !strings.Contains(out, "#!/usr/bin/env python3") {
		t.Errorf("shebang dropped:\n%s", out)
	}
}

func TestSlimCodeNoWin(t *testing.T) {
	src := "def f():\n    return 1\n"
	if _, ok := SlimCode(src); ok {
		t.Errorf("no comments to strip — must report no win")
	}
}

func TestKeywordsOf(t *testing.T) {
	kw := KeywordsOf(`Find the "stargazers_count" for repo acme/widgets in the response below.`, 8)
	if len(kw) == 0 || kw[0].Text != "stargazers_count" || kw[0].Weight != 3 {
		t.Errorf("quoted keyword must come first with weight 3: %v", kw)
	}
	found := false
	for _, k := range kw {
		if k.Text == "acme/widgets" || k.Text == "acme" || k.Text == "widgets" {
			found = true
		}
	}
	if !found {
		t.Errorf("identifier keywords missing: %v", kw)
	}
}

package classify

import (
	"strings"
	"testing"
)

func TestSniffJSON(t *testing.T) {
	r := Sniff(`{"a": 1, "b": [1,2,3]}`)
	if r.Kind != JSON || r.Confidence < 0.9 {
		t.Errorf("json sniff = %+v", r)
	}
	r = Sniff(`[{"id":1},{"id":2}]`)
	if r.Kind != JSON {
		t.Errorf("json array sniff = %+v", r)
	}
	// A bare JSON string literal is valid JSON but not worth structural work.
	if r := Sniff(`"just a string"`); r.Kind == JSON {
		t.Errorf("scalar root misrouted to json: %+v", r)
	}
}

func TestSniffLog(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString("2026-10-31 10:00:01 INFO GET /api/users/42 200 12ms\n")
	}
	r := Sniff(b.String())
	if r.Kind != Log {
		t.Errorf("log sniff = %+v", r)
	}
}

func TestSniffCode(t *testing.T) {
	fenced := "Here is the code:\n```python\ndef f(x):\n    return x + 1\n```\n"
	if r := Sniff(fenced); r.Kind != Code {
		t.Errorf("fenced code sniff = %+v", r)
	}
	source := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(1)\n}\n"
	if r := Sniff(source); r.Kind != Code {
		t.Errorf("source sniff = %+v", r)
	}
}

func TestSniffHTML(t *testing.T) {
	if r := Sniff("<!DOCTYPE html><html><body><p>hi</p></body></html>"); r.Kind != HTML {
		t.Errorf("html sniff = %+v", r)
	}
}

func TestSniffTable(t *testing.T) {
	table := "name|age|city\nalice|30|berlin\nbob|25|oslo\ncarol|41|lima\n"
	if r := Sniff(table); r.Kind != Table {
		t.Errorf("table sniff = %+v", r)
	}
}

func TestSniffTextFallback(t *testing.T) {
	r := Sniff("This is a plain sentence about something. It has no special structure at all.")
	if r.Kind != Text {
		t.Errorf("text sniff = %+v", r)
	}
	if r := Sniff(""); r.Kind != Text {
		t.Errorf("empty sniff = %+v", r)
	}
}

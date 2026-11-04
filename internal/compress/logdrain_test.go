package compress

import (
	"fmt"
	"strings"
	"testing"
)

func TestDrainMergesRepetitiveLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "2026-10-31 10:00:%02d INFO GET /api/users/%d 200 12ms\n", i%60, i)
	}
	res := DrainCompress(b.String(), DefaultDrainConfig())
	if !res.OK {
		t.Fatalf("expected OK, reason=%s", res.Reason)
	}
	if res.Templates != 1 {
		t.Errorf("expected 1 template, got %d: %.200s", res.Templates, res.Output)
	}
	if !strings.Contains(res.Output, "INFO GET") {
		t.Errorf("literal tokens lost: %s", res.Output)
	}
	if !strings.Contains(res.Output, "x200") {
		t.Errorf("count missing: %s", res.Output)
	}
}

func TestDrainMultipleTemplates(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "INFO cache hit key=%d\n", i)
		fmt.Fprintf(&b, "ERROR db timeout query=select %d\n", i)
	}
	res := DrainCompress(b.String(), DefaultDrainConfig())
	if !res.OK {
		t.Fatalf("expected OK, reason=%s", res.Reason)
	}
	if res.Templates != 2 {
		t.Errorf("expected 2 templates, got %d:\n%s", res.Templates, res.Output)
	}
}

func TestDrainTooFewLines(t *testing.T) {
	res := DrainCompress("line one 1\nline two 2\nline three 3", DefaultDrainConfig())
	if res.OK {
		t.Errorf("expected !OK for tiny input")
	}
}

func TestDrainNoRepetitionFails(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "completely unique sentence number %d about topic %d\n", i, i*7)
	}
	res := DrainCompress(b.String(), DefaultDrainConfig())
	// 40 unique single-line templates → poor repetition → caller keeps original.
	if res.OK && res.Templates > res.Lines/2 {
		t.Errorf("should have failed as poor repetition: %d templates / %d lines", res.Templates, res.Lines)
	}
	if res.OK && strings.Count(res.Output, "\n") >= res.Lines {
		t.Errorf("output must be smaller than input lines")
	}
}

func TestDrainSampleParamsPresent(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "worker handled job %d\n", i)
	}
	res := DrainCompress(b.String(), DefaultDrainConfig())
	if !res.OK {
		t.Fatalf("expected OK: %s", res.Reason)
	}
	// Some variable samples must survive so users can eyeball value ranges.
	if !strings.Contains(res.Output, "vars:") {
		t.Errorf("no variable samples in output: %s", res.Output)
	}
}

func TestMaskToken(t *testing.T) {
	cases := map[string]string{
		"200":                                  "<*>",
		"12ms":                                 "<*>",
		"192.168.0.1":                          "<*>",
		"550e8400-e29b-41d4-a716-446655440000": "<*>",
		"status=200":                           "status=<*>",
		"INFO":                                 "INFO",
		"GET":                                  "GET",
		"/health":                              "/health",
		"connected":                            "connected",
		"HTTP/1.1":                             "<*>", // digit rule: acceptable tradeoff, documented
	}
	for in, want := range cases {
		got, _ := maskToken(in)
		if got != want {
			t.Errorf("maskToken(%q) = %q, want %q", in, got, want)
		}
	}
}

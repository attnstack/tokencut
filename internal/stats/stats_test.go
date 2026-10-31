package stats

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordAndSnapshot(t *testing.T) {
	r, err := NewRecorder("test", map[string]ModelPricing{
		"gpt-4o": {Input: 2.50, Output: 10.00},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	r.Record("gpt-4o", 1000, 400, map[string]int{"json-prune": 1}, 1, 2*time.Millisecond)
	r.Record("gpt-4o", 1000, 1000, map[string]int{"none": 1}, 0, time.Millisecond)
	s := r.Snapshot()
	if s["requests_total"].(uint64) != 2 {
		t.Errorf("requests = %v", s["requests_total"])
	}
	if s["compressed_total"].(uint64) != 1 {
		t.Errorf("compressed = %v", s["compressed_total"])
	}
	// saved = (1000-400)/1e6 * 2.50 = 0.0015
	if s["saved_usd"].(float64) < 0.0014 || s["saved_usd"].(float64) > 0.0016 {
		t.Errorf("saved_usd = %v", s["saved_usd"])
	}
	if s["cache_hits_total"].(uint64) != 1 {
		t.Errorf("cache hits = %v", s["cache_hits_total"])
	}
	if len(s["recent"].([]RequestRecord)) != 2 {
		t.Errorf("recent = %d", len(s["recent"].([]RequestRecord)))
	}
}

func TestJSONLSink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats", "events.jsonl")
	r, err := NewRecorder("test", nil, path)
	if err != nil {
		t.Fatal(err)
	}
	r.Record("m", 100, 50, map[string]int{"json-prune": 1}, 0, time.Millisecond)
	r.Record("m", 200, 80, map[string]int{"log-drain": 1}, 2, time.Millisecond)
	r.Close() // flush + stop
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		var rec RequestRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("line %d: %v", n, err)
		}
		n++
	}
	if n != 2 {
		t.Errorf("expected 2 jsonl lines, got %d", n)
	}
}

func TestPromText(t *testing.T) {
	r, _ := NewRecorder("test", nil, "")
	r.Record("m", 10, 5, map[string]int{"json-prune": 1}, 0, time.Millisecond)
	txt := r.PromText()
	for _, want := range []string{
		"tokencut_requests_total 1",
		`tokencut_strategy_total{strategy="json-prune"} 1`,
		"tokencut_saved_usd_total",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("prom text missing %q:\n%s", want, txt)
		}
	}
}

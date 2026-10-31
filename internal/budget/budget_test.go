package budget

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func bigJSON(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"idx": %d, "note": "%s"}`, i, strings.Repeat("v", 60))
	}
	b.WriteByte(']')
	return b.String()
}

func testController(t *testing.T, mutate func(*Config)) *Controller {
	t.Helper()
	cfg := DefaultConfig()
	cfg.LatencyBudget = 5 * time.Second // tests must not hit the deadline
	cfg.Targets = map[string]float64{"balanced": 0.5, "aggressive": 0.9, "lossless": 0}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewController(cfg)
}

func mustApply(t *testing.T, c *Controller, body string) ([]byte, *Report) {
	t.Helper()
	out, rep, err := c.Apply([]byte(body))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return out, rep
}

func TestExemptions(t *testing.T) {
	c := testController(t, nil)
	sys := `{"desc": "` + strings.Repeat("s", 2000) + `", "list": [1,2,3,4,5,6,7,8,9,10,11,12,13,14,15]}`
	// [system, big user, final user]: index 1 must compress; system and the
	// LAST user must stay exempt.
	body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"system","content":%s},{"role":"user","content":%s},{"role":"user","content":"now summarize"}]}`,
		mustJSONString(t, sys), mustJSONString(t, bigJSON(50)))
	out, rep := mustApply(t, c, body)
	var env struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("out: %v", err)
	}
	if !strings.Contains(env.Messages[0].Content, `"desc"`) || !strings.Contains(env.Messages[0].Content, `"list"`) {
		t.Errorf("system message must only be canonicalized, keys must survive: %.160s", env.Messages[0].Content)
	}
	if rep.Messages[0].Strategy != "exempt" {
		t.Errorf("system not marked exempt: %+v", rep.Messages[0])
	}
	if rep.Messages[1].Strategy == "exempt" || rep.Messages[1].Strategy == "none" {
		t.Errorf("big non-last user message should compress: %+v", rep.Messages[1])
	}
	if rep.Messages[2].Strategy != "exempt" {
		t.Errorf("last user must be exempt: %+v", rep.Messages[2])
	}
}

func TestLastUserExempt(t *testing.T) {
	c := testController(t, nil)
	body := fmt.Sprintf(`{"messages":[{"role":"user","content":%s},{"role":"assistant","content":"ack"},{"role":"user","content":%s}]}`,
		mustJSONString(t, bigJSON(50)), mustJSONString(t, bigJSON(50)))
	_, rep := mustApply(t, c, body)
	if rep.Messages[2].Strategy != "exempt" {
		t.Errorf("last user must be exempt: %+v", rep.Messages[2])
	}
	if rep.Messages[0].Strategy == "exempt" {
		t.Errorf("earlier user must not be exempt: %+v", rep.Messages[0])
	}
}

func TestDedupWithinRequest(t *testing.T) {
	c := testController(t, nil)
	same := mustJSONString(t, bigJSON(80))
	// 3 identical big user messages: index 0 seeds the fingerprint, index 1
	// dedups, index 2 is the exempt last user.
	body := fmt.Sprintf(`{"messages":[{"role":"user","content":%s},{"role":"user","content":%s},{"role":"user","content":%s},{"role":"assistant","content":"ok"}]}`,
		same, same, same)
	out, rep := mustApply(t, c, body)
	if rep.Strategies["dedup"] != 1 {
		t.Fatalf("dedup not applied: %+v", rep.Strategies)
	}
	if !strings.Contains(string(out), "[duplicate of message #0") {
		t.Errorf("dedup marker missing")
	}
}

func TestDiffWithinRequest(t *testing.T) {
	c := testController(t, func(cfg *Config) { cfg.ExemptLastUser = false })
	v1 := strings.Repeat("stable line of the file content\n", 120)
	v2 := v1 + "one appended line at the end\n"
	body := fmt.Sprintf(`{"messages":[{"role":"user","content":%s},{"role":"user","content":%s}]}`,
		mustJSONString(t, v1), mustJSONString(t, v2))
	out, rep := mustApply(t, c, body)
	if rep.Strategies["diff"] != 1 {
		t.Fatalf("diff not applied: %+v", rep.Strategies)
	}
	if !strings.Contains(string(out), "[tokencut-diff]") {
		t.Errorf("diff header missing")
	}
}

func TestMultimodalPartsSkipped(t *testing.T) {
	c := testController(t, func(cfg *Config) { cfg.ExemptLastUser = false })
	body := fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"text","text":%s},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
		mustJSONString(t, bigJSON(80)))
	_, rep := mustApply(t, c, body)
	if rep.Strategies["skipped-multimodal"] != 1 {
		t.Fatalf("multimodal skip not recorded: %+v", rep.Strategies)
	}
}

func TestMalformedBodyTransparent(t *testing.T) {
	c := testController(t, nil)
	for _, body := range []string{`hello`, `[1,2,3]`, `{"no_messages": true}`, `{"messages": null}`} {
		out, rep, err := c.Apply([]byte(body))
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if string(out) != body {
			t.Errorf("%q: forwarded body must be byte-identical, got %q", body, out)
		}
		if rep.Changed {
			t.Errorf("%q: must not be marked changed", body)
		}
	}
}

func TestPerRequestOverrideDisable(t *testing.T) {
	c := testController(t, nil)
	body := fmt.Sprintf(`{"messages":[{"role":"user","content":%s}],"tokencut":{"enabled":false}}`,
		mustJSONString(t, bigJSON(80)))
	out, rep := mustApply(t, c, body)
	if rep.Enabled {
		t.Errorf("override must disable")
	}
	// The override field is stripped; message bytes are preserved raw
	// (content is an escaped JSON string inside the message).
	if strings.Contains(string(out), `"tokencut"`) {
		t.Errorf("override field must be stripped even when disabled")
	}
	if !strings.Contains(string(out), `{\"idx\": 0`) {
		t.Errorf("message content must survive byte-for-byte: %.200s", out)
	}
}

func TestPerRequestOverrideTarget(t *testing.T) {
	c := testController(t, func(cfg *Config) { cfg.ExemptLastUser = false })
	body := fmt.Sprintf(`{"messages":[{"role":"user","content":%s}],"tokencut":{"target":0.9}}`,
		mustJSONString(t, bigJSON(100)))
	_, rep := mustApply(t, c, body)
	if rep.Ratio < 0.7 {
		t.Errorf("target 0.9 not approached: ratio=%.2f", rep.Ratio)
	}
}

func TestZeroSavingsKeepsOriginalBytes(t *testing.T) {
	c := testController(t, nil)
	body := `{"messages":[{"role":"user","content":"tiny already"}]}`
	out, rep := mustApply(t, c, body)
	if rep.Changed || string(out) != body {
		t.Errorf("tiny message must be forwarded unchanged")
	}
}

func TestLatencyBudgetStopsCompression(t *testing.T) {
	c := testController(t, func(cfg *Config) {
		cfg.LatencyBudget = time.Microsecond // expires mid-compression
		cfg.ExemptLastUser = false
	})
	// Distinct contents so dedup can't short-circuit the loop; each message
	// needs real compression work, so the deadline fires between messages.
	many := make([]string, 60)
	for i := range many {
		many[i] = fmt.Sprintf(`{"role":"user","content":%s}`, mustJSONString(t, bigJSON(60+i)))
	}
	body := `{"messages":[` + strings.Join(many, ",") + `]}`
	_, rep, err := c.Apply([]byte(body))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// The request must still succeed end to end (fail-open per message), and
	// most messages must be skipped once the deadline fires.
	skipped := 0
	for _, m := range rep.Messages {
		if strings.HasPrefix(m.Strategy, "skipped") {
			skipped++
		}
	}
	if skipped < 30 {
		t.Errorf("expected most messages skipped after deadline, got %d", skipped)
	}
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

package mockupstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postChat(t *testing.T, h http.Handler, body string) (*http.Response, map[string]any) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp, out
}

func TestSolveJSONKeyLookup(t *testing.T) {
	ctx := `[{"name":"alpha","stargazers_count":42},{"name":"beta","stargazers_count":7}]`
	body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":%s},{"role":"user","content":%s}]}`,
		mustJSON(t, "Context:\n"+ctx),
		mustJSON(t, `Based on the API response below, what is the value of "stargazers_count"? Answer with only the value.`))
	_, out := postChat(t, Handler(), body)
	content := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if content != "42" {
		t.Errorf("solver returned %q, want 42", content)
	}
}

func TestSolveRecordScopedLookup(t *testing.T) {
	ctx := `[{"product":"Zephyr","launch_year":2019,"price_usd":120},{"product":"Nimbus","launch_year":2021,"price_usd":80}]`
	q := `According to the records, what is the value of "launch_year" for the product "Zephyr"? Answer with only the value.`
	body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":%s},{"role":"user","content":%s}]}`,
		mustJSON(t, "Context:\n"+ctx), mustJSON(t, q))
	_, out := postChat(t, Handler(), body)
	content := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if content != "2019" {
		t.Errorf("solver returned %q, want 2019", content)
	}
}

func TestSolveLogFault(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("2026-10-31 INFO serving request\n")
		if i < 30 {
			fmt.Fprintf(&b, "2026-10-31 ERROR Failed to connect to 10.0.3.18:5432 (timeout)\n")
		}
	}
	body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":%s},{"role":"user","content":%s}]}`,
		mustJSON(t, b.String()),
		mustJSON(t, `These are service logs around an incident. What host:port shows repeated connection failures? Answer with only host:port.`))
	_, out := postChat(t, Handler(), body)
	content := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if content != "10.0.3.18:5432" {
		t.Errorf("solver returned %q", content)
	}
}

func TestSolveNotFound(t *testing.T) {
	body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":%s},{"role":"user","content":%s}]}`,
		mustJSON(t, "nothing relevant"), mustJSON(t, `What is the "missing_key"?`))
	_, out := postChat(t, Handler(), body)
	content := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if content != "not found in provided context" {
		t.Errorf("expected explicit not-found, got %q", content)
	}
}

func TestStreamShape(t *testing.T) {
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hello world this is a test"}]}`
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	chunks, done := 0, false
	for sc.Scan() {
		line := sc.Text()
		if line == "data: [DONE]" {
			done = true
		} else if strings.HasPrefix(line, "data: ") {
			chunks++
		}
	}
	if !done || chunks < 2 {
		t.Errorf("stream shape wrong: chunks=%d done=%v", chunks, done)
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

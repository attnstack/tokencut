package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tokencut/internal/budget"
	"tokencut/internal/compress"
	"tokencut/internal/config"
	"tokencut/internal/stats"
)

func statsNewRecorderForTest() (*stats.Recorder, error) {
	return stats.NewRecorder("test", map[string]stats.ModelPricing{}, "")
}

func bigJSON(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"idx": %d, "blob": "%s"}`, i, strings.Repeat("d", 80))
	}
	b.WriteByte(']')
	return b.String()
}

type captured struct {
	body    []byte
	header  http.Header
	purpose string
}

func newTestServer(t *testing.T, upstreamHandler http.HandlerFunc) (*httptest.Server, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.Upstream = upstream.URL
	cfg.Tier = "balanced"
	cfg.Targets = map[string]float64{"balanced": 0.5, "aggressive": 0.9, "lossless": 0}
	hv := make(map[string]struct{})
	for _, k := range []string{"id", "name", "type", "status", "error", "code", "message", "url", "data", "key", "path", "file"} {
		hv[k] = struct{}{}
	}
	bcfg := budget.Config{
		Tier: cfg.Tier, Targets: cfg.Targets,
		ExemptSystem: true, ExemptLastUser: false, // tests exercise compression on single-message requests
		DiffMode:      true,
		LatencyBudget: 5 * time.Second,
		Cache:         compress.NewLRU(128),
		Prune:         compress.JSONPruneConfig{LongStringCap: 2000, KeywordSample: 2, HighValueKeys: hv},
		Drain:         compress.DefaultDrainConfig(),
	}
	rec, err := statsNewRecorderForTest()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	srv, err := New(cfg, budget.NewController(bcfg), rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, upstream
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

func TestChatCompletionsCompressedAndHeaders(t *testing.T) {
	capCh := make(chan captured, 1)
	ts, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capCh <- captured{body: b, header: r.Header.Clone()}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	})
	orig := bigJSON(200)
	body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%s}],"tokencut":{"target":0.9}}`,
		mustJSONString(t, orig))
	resp := postJSON(t, ts.URL+"/v1/chat/completions", body)
	defer resp.Body.Close()

	cap := <-capCh
	if len(cap.body) >= len(body) {
		t.Fatalf("upstream body not compressed: %d vs %d", len(cap.body), len(body))
	}
	var env map[string]any
	if err := json.Unmarshal(cap.body, &env); err != nil {
		t.Fatalf("forwarded body invalid JSON: %v\n%.200s", err, cap.body)
	}
	if !strings.Contains(string(cap.body), "_omitted") {
		t.Errorf("expected pruned marker")
	}
	if got := resp.Header.Get("x-tokencut-ratio"); got == "" {
		t.Errorf("observability headers missing: %v", resp.Header)
	}
	if resp.Header.Get("x-tokencut-original-tokens") == "" {
		t.Errorf("original tokens header missing")
	}
}

func TestNonChatPathPassthrough(t *testing.T) {
	capCh := make(chan captured, 1)
	ts, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capCh <- captured{body: b, header: r.Header.Clone()}
		fmt.Fprint(w, `{"ok":true}`)
	})
	body := fmt.Sprintf(`{"input":%s}`, mustJSONString(t, bigJSON(100)))
	resp := postJSON(t, ts.URL+"/v1/embeddings", body)
	defer resp.Body.Close()
	cap := <-capCh
	if string(cap.body) != body {
		t.Errorf("non-chat path must be byte-transparent")
	}
	if resp.Header.Get("x-tokencut-ratio") != "" {
		t.Errorf("no tokencut headers on passthrough path")
	}
}

func TestMalformedBodyForwardedByteIdentical(t *testing.T) {
	capCh := make(chan captured, 1)
	ts, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capCh <- captured{body: b}
		fmt.Fprint(w, `{"ok":true}`)
	})
	body := "this is not json at all {"
	resp := postJSON(t, ts.URL+"/v1/chat/completions", body)
	defer resp.Body.Close()
	cap := <-capCh
	if string(cap.body) != body {
		t.Errorf("malformed body must be forwarded byte-identical, got %q", cap.body)
	}
}

func TestSSEStreamsThroughUnbuffered(t *testing.T) {
	upstreamDelay := 300 * time.Millisecond
	ts, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// consume the (compressed) body so the connection is sane
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"chunk\":1}\n\n")
		flusher.Flush()
		time.Sleep(upstreamDelay)
		fmt.Fprint(w, "data: {\"chunk\":2}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	body := fmt.Sprintf(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":%s}]}`,
		mustJSONString(t, bigJSON(50)))
	start := time.Now()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	firstRead := time.Since(start)
	if err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if !strings.Contains(line, "chunk") {
		t.Errorf("first SSE line unexpected: %q", line)
	}
	// The first chunk must arrive before the upstream sleep elapses; that
	// proves the proxy is not buffering the stream.
	if firstRead >= upstreamDelay {
		t.Errorf("first chunk arrived after %v (>= upstream delay %v): proxy is buffering", firstRead, upstreamDelay)
	}
	rest, _ := io.ReadAll(reader)
	if !strings.Contains(string(rest), "[DONE]") {
		t.Errorf("stream truncated: %q", rest)
	}
}

func TestPanicInPipelineFailsOpen(t *testing.T) {
	// Simulate a panic inside Apply by using a controller with a poisoned
	// cache value of the wrong type.
	upstreamBody := make(chan []byte, 1)
	cfg := config.Default()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamBody <- b
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	cfg.Upstream = upstream.URL
	rec, _ := statsNewRecorderForTest()
	lru := compress.NewLRU(16)
	lru.Put("poison", "not-a-CachedPrune") // Get returns wrong type; must not crash
	bcfg := budget.Config{
		Tier: "balanced", Targets: map[string]float64{"balanced": 0.5},
		LatencyBudget: 5 * time.Second, Cache: lru,
		Prune: compress.DefaultJSONPruneConfig(), Drain: compress.DefaultDrainConfig(),
	}
	srv, err := New(cfg, budget.NewController(bcfg), rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%s}]}`, mustJSONString(t, bigJSON(30)))
	resp := postJSON(t, ts.URL+"/v1/chat/completions", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fail-open broken: status %d", resp.StatusCode)
	}
	if b := <-upstreamBody; !json.Valid(b) {
		t.Errorf("upstream got invalid body")
	}
}

func TestLargeBodyCompresses(t *testing.T) {
	capCh := make(chan captured, 1)
	ts, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capCh <- captured{body: b}
		fmt.Fprint(w, `{"ok":true}`)
	})
	orig := bigJSON(500)
	body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%s}]}`, mustJSONString(t, orig))
	resp := postJSON(t, ts.URL+"/v1/chat/completions", body)
	defer resp.Body.Close()
	cap := <-capCh
	if len(cap.body) >= len(body) {
		t.Errorf("large body should compress: %d vs %d", len(cap.body), len(body))
	}
	if !strings.Contains(string(cap.body), "_omitted") {
		t.Errorf("expected pruning markers")
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

// BenchmarkProxyRoundTrip measures the proxy's added latency over a direct
// call to an identical upstream (loopback; includes HTTP overhead on both
// sides — the DELTA is the meaningful number).
func BenchmarkProxyRoundTrip(b *testing.B) {
	payload := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%s}]}`,
		mustJSONStringB(bigJSON(200)))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Upstream = upstream.URL
	rec, _ := statsNewRecorderForTest()
	bcfg := budget.Config{
		Tier: "balanced", Targets: map[string]float64{"balanced": 0.5},
		LatencyBudget: 5 * time.Second, Cache: compress.NewLRU(1024),
		Prune: compress.DefaultJSONPruneConfig(), Drain: compress.DefaultDrainConfig(),
	}
	srv, err := New(cfg, budget.NewController(bcfg), rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		b.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	b.Run("via_proxy", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(payload))
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
	b.Run("direct_upstream", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			resp, err := http.Post(upstream.URL+"/v1/chat/completions", "application/json", strings.NewReader(payload))
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

func mustJSONStringB(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

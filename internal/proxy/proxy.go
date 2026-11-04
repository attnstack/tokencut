// proxy.go — the transparent OpenAI-compatible reverse proxy.
//
// Hot-path contract:
//   - only POST /v1/chat/completions bodies are inspected and possibly
//     rewritten; every other path/method is byte-transparent passthrough
//   - compression runs BEFORE forwarding, so its stats are known before the
//     first response byte and can ride on the response headers
//   - SSE passthrough: FlushInterval = -1 makes ReverseProxy flush every
//     upstream write immediately — the single most important line for
//     "first token latency unchanged"
//   - fail-open: parse errors, panics, oversized bodies, zero savings —
//     every one of them forwards the original request. Compression must
//     never be the reason a request fails.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime/debug"
	"strconv"
	"time"

	"tokencut/internal/budget"
	"tokencut/internal/config"
	"tokencut/internal/dashboard"
	"tokencut/internal/ratelimit"
	"tokencut/internal/stats"
)

type ctxKeyType struct{}

var ctxKey ctxKeyType

type Server struct {
	cfg     *config.Config
	ctrl    *budget.Controller
	rec     *stats.Recorder
	rp      *httputil.ReverseProxy
	limiter *ratelimit.Bucket
	logger  *slog.Logger
}

func New(cfg *config.Config, ctrl *budget.Controller, rec *stats.Recorder, logger *slog.Logger) (*Server, error) {
	target, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("proxy: parse upstream %q: %w", cfg.Upstream, err)
	}
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		// LLMs think for a long time before the first byte; do not kill
		// long generations. There is intentionally NO overall request timeout:
		// a transparent proxy must not cancel a 5-minute completion.
		ResponseHeaderTimeout: 300 * time.Second,
	}
	var limiter *ratelimit.Bucket
	if cfg.RateRPS > 0 {
		limiter = ratelimit.New(cfg.RateRPS)
	}
	s := &Server{cfg: cfg, ctrl: ctrl, rec: rec, limiter: limiter, logger: logger}
	s.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Do not advertise the proxy to upstream API endpoints; strip
			// inbound forwarding headers so upstreams can't be confused by them.
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-Proto")
		},
		// FlushInterval = -1 flushes every upstream write to the client
		// immediately. The default buffering would add a full response round
		// of latency before the first SSE token reaches the caller.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if v, ok := resp.Request.Context().Value(ctxKey).(*budget.Report); ok {
				setObservabilityHeaders(resp.Header, v, cfg.Pricing)
			}
			return nil
		},
		Transport: tr,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("upstream error", "path", r.URL.Path, "err", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"tokencut: upstream unreachable","type":"proxy_error"}}`))
		},
	}
	return s, nil
}

func setObservabilityHeaders(h http.Header, rep *budget.Report, pricing map[string]config.PricingEntry) {
	h.Set("x-tokencut-original-tokens", strconv.Itoa(rep.OriginalTokens))
	h.Set("x-tokencut-compressed-tokens", strconv.Itoa(rep.FinalTokens))
	h.Set("x-tokencut-ratio", fmt.Sprintf("%.2f", rep.Ratio))
	if p, ok := pricing[rep.Model]; ok {
		usd := float64(rep.OriginalTokens-rep.FinalTokens) / 1e6 * p.Input
		if usd >= 0 {
			h.Set("x-tokencut-saved-usd", fmt.Sprintf("%.6f", usd))
		}
	}
	if len(rep.Strategies) > 0 {
		s := ""
		for k, v := range rep.Strategies {
			if s != "" {
				s += ","
			}
			s += fmt.Sprintf("%s:%d", k, v)
		}
		h.Set("x-tokencut-strategies", s)
	}
}

// Handler returns the full application mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dashboard", dashboard.Handler(s.rec))
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(s.rec.Snapshot())
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"listen":%q,"upstream":%q,"tier":%q,"diff_mode":%t,"latency_budget_ms":%d,"max_body_bytes":%d,"rate_rps":%f}`,
			s.cfg.Listen, s.cfg.Upstream, s.cfg.Tier, s.cfg.DiffMode, s.cfg.LatencyBudgetMS, s.cfg.MaxBodyBytes, s.cfg.RateRPS)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(s.rec.PromText()))
	})
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.rp.ServeHTTP(w, r)
	})
	return s.withRecovery(mux)
}

// chat is the only rewritten path. Everything else hits ReverseProxy raw.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if s.limiter != nil && !s.limiter.Allow() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"tokencut: rate limit exceeded","type":"rate_limit_error"}}`))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(s.cfg.MaxBodyBytes)+1))
	if err != nil {
		s.logger.Error("read request body", "err", err)
		http.Error(w, `{"error":{"message":"tokencut: failed to read request body","type":"proxy_error"}}`, http.StatusBadRequest)
		return
	}
	oversized := len(body) > s.cfg.MaxBodyBytes

	start := time.Now()
	var newBody []byte
	var rep *budget.Report
	if oversized {
		newBody, rep = body, &budget.Report{Enabled: false, Note: "body over max_body_bytes; forwarded unchanged", Strategies: map[string]int{}}
	} else {
		newBody, rep = s.applySafe(body)
	}
	s.rec.Record(rep.Model, rep.OriginalTokens, rep.FinalTokens, rep.Strategies, rep.CacheHits, time.Since(start))
	s.logger.Debug("chat completion",
		"model", rep.Model,
		"orig", rep.OriginalTokens,
		"final", rep.FinalTokens,
		"ratio", rep.Ratio,
		"strategies", rep.Strategies,
		"compress_ms", float64(time.Since(start).Microseconds())/1000,
		"changed", rep.Changed,
		"note", rep.Note,
	)

	ctx := context.WithValue(r.Context(), ctxKey, rep)
	r2 := r.Clone(ctx)
	r2.Body = io.NopCloser(bytes.NewReader(newBody))
	r2.ContentLength = int64(len(newBody))
	r2.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	s.rp.ServeHTTP(w, r2)
}

// applySafe is the fail-open boundary: a panic or error anywhere in the
// compression pipeline forwards the ORIGINAL request bytes.
func (s *Server) applySafe(body []byte) (out []byte, rep *budget.Report) {
	rep = &budget.Report{Enabled: true, Strategies: map[string]int{}}
	out = body
	defer func() {
		if rec := recover(); rec != nil {
			s.logger.Error("panic in compression pipeline; forwarding original request",
				"panic", rec, "stack", string(debug.Stack()))
			out, rep = body, &budget.Report{Enabled: false, Note: "panic in pipeline; forwarded unchanged", Strategies: map[string]int{}}
		}
	}()
	var err error
	out, rep, err = s.ctrl.Apply(body)
	if err != nil {
		s.logger.Error("compression error; forwarding original request", "err", err)
		out = body
	}
	return out, rep
}

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic in http handler", "panic", rec, "path", r.URL.Path, "stack", string(debug.Stack()))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"tokencut: internal error","type":"proxy_error"}}`))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

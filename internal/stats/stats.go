// stats.go — in-memory observation state + optional JSONL persistence.
// Why JSONL instead of SQLite (a deviation from the original plan, argued in
// the README): the hot-path store is the LRU cache (in-memory); observation
// data is append-only and read rarely, so an async JSONL appender with
// drop-on-overflow keeps the proxy dependency-free and its tail latency
// untouched. SQLite becomes worth it only when you need SQL queries over
// historical data — a v2 need, not a v1 one.
package stats

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ModelPricing is USD per 1M tokens.
type ModelPricing struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

type ModelStats struct {
	Requests   uint64  `json:"requests"`
	TokensOrig uint64  `json:"tokens_original"`
	TokensFinal uint64 `json:"tokens_final"`
	SavedUSD   float64 `json:"saved_usd"`
}

type RequestRecord struct {
	Time           time.Time      `json:"time"`
	Model          string         `json:"model"`
	OriginalTokens int            `json:"original_tokens"`
	FinalTokens    int            `json:"final_tokens"`
	Ratio          float64        `json:"ratio"`
	Strategies     map[string]int `json:"strategies"`
	CompressMS     float64        `json:"compress_ms"`
}

type Recorder struct {
	mu        sync.Mutex
	startedAt time.Time
	Version   string

	Requests      uint64
	Compressed    uint64
	CacheHits     uint64
	TokensOrig    uint64
	TokensFinal   uint64
	SavedUSD      float64
	strategies    map[string]uint64
	models        map[string]*ModelStats
	recent        []RequestRecord
	recentMax     int
	DroppedEvents uint64

	pricing map[string]ModelPricing
	sink    *jsonlSink
}

func NewRecorder(version string, pricing map[string]ModelPricing, jsonlPath string) (*Recorder, error) {
	r := &Recorder{
		startedAt:  time.Now(),
		Version:    version,
		strategies: map[string]uint64{},
		models:     map[string]*ModelStats{},
		recentMax:  200,
		pricing:    pricing,
	}
	if jsonlPath != "" {
		s, err := newJSONLSink(jsonlPath)
		if err != nil {
			return nil, err
		}
		r.sink = s
	}
	return r, nil
}

// Record accumulates one request. Cheap and lock-guarded; safe for the proxy
// hot path.
func (r *Recorder) Record(model string, origTok, finalTok int, strategies map[string]int, cacheHits int, compressTime time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Requests++
	if finalTok < origTok {
		r.Compressed++
	}
	r.TokensOrig += uint64(origTok)
	r.TokensFinal += uint64(finalTok)
	r.CacheHits += uint64(cacheHits)
	for k, v := range strategies {
		r.strategies[k] += uint64(v)
	}
	ms := r.models[model]
	if ms == nil {
		ms = &ModelStats{}
		r.models[model] = ms
	}
	ms.Requests++
	ms.TokensOrig += uint64(origTok)
	ms.TokensFinal += uint64(finalTok)
	if p, ok := r.pricing[model]; ok {
		usd := float64(origTok-finalTok) / 1e6 * p.Input
		if usd > 0 {
			r.SavedUSD += usd
			ms.SavedUSD += usd
		}
	}
	rec := RequestRecord{
		Time:           time.Now(),
		Model:          model,
		OriginalTokens: origTok,
		FinalTokens:    finalTok,
		Strategies:     strategies,
		CompressMS:     float64(compressTime.Microseconds()) / 1000,
	}
	if origTok > 0 {
		rec.Ratio = 1 - float64(finalTok)/float64(origTok)
	}
	r.recent = append(r.recent, rec)
	if len(r.recent) > r.recentMax {
		r.recent = r.recent[len(r.recent)-r.recentMax:]
	}
	if r.sink != nil {
		r.sink.write(rec)
	}
}

// Snapshot returns a JSON-friendly aggregate for /api/stats.
func (r *Recorder) Snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	ratio := 0.0
	if r.TokensOrig > 0 {
		ratio = 1 - float64(r.TokensFinal)/float64(r.TokensOrig)
	}
	strategies := map[string]uint64{}
	for k, v := range r.strategies {
		strategies[k] = v
	}
	models := map[string]*ModelStats{}
	for k, v := range r.models {
		models[k] = v
	}
	recent := make([]RequestRecord, len(r.recent))
	copy(recent, r.recent)
	return map[string]any{
		"version":            r.Version,
		"uptime_seconds":     time.Since(r.startedAt).Seconds(),
		"requests_total":     r.Requests,
		"compressed_total":   r.Compressed,
		"cache_hits_total":   r.CacheHits,
		"tokens_original":    r.TokensOrig,
		"tokens_final":       r.TokensFinal,
		"compression_ratio":  ratio,
		"saved_usd":          r.SavedUSD,
		"strategies":         strategies,
		"models":             models,
		"recent":             recent,
		"dropped_events":     r.DroppedEvents,
		"estimation_note":    "token counts are tokencut estimates (chars/token calibrated), not tokenizer-exact",
	}
}

// PromText renders the Prometheus text exposition format (subset, std-lib
// only — enough for a scrape config; a full client library is a v2 want).
func (r *Recorder) PromText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var sb strings.Builder
	fmt.Fprintf(&sb, "# HELP tokencut_requests_total Requests seen by the proxy.\n")
	fmt.Fprintf(&sb, "# TYPE tokencut_requests_total counter\n")
	fmt.Fprintf(&sb, "tokencut_requests_total %d\n", r.Requests)
	fmt.Fprintf(&sb, "# TYPE tokencut_requests_compressed_total counter\n")
	fmt.Fprintf(&sb, "tokencut_requests_compressed_total %d\n", r.Compressed)
	fmt.Fprintf(&sb, "# TYPE tokencut_input_tokens_original_total counter\n")
	fmt.Fprintf(&sb, "tokencut_input_tokens_original_total %d\n", r.TokensOrig)
	fmt.Fprintf(&sb, "# TYPE tokencut_input_tokens_final_total counter\n")
	fmt.Fprintf(&sb, "tokencut_input_tokens_final_total %d\n", r.TokensFinal)
	fmt.Fprintf(&sb, "# TYPE tokencut_cache_hits_total counter\n")
	fmt.Fprintf(&sb, "tokencut_cache_hits_total %d\n", r.CacheHits)
	fmt.Fprintf(&sb, "# TYPE tokencut_saved_usd_total counter\n")
	fmt.Fprintf(&sb, "tokencut_saved_usd_total %f\n", r.SavedUSD)
	if len(r.strategies) > 0 {
		fmt.Fprintf(&sb, "# TYPE tokencut_strategy_total counter\n")
		for _, k := range sortedStrategyKeys(r.strategies) {
			fmt.Fprintf(&sb, "tokencut_strategy_total{strategy=%q} %d\n", k, r.strategies[k])
		}
	}
	return sb.String()
}

// Close flushes and stops the JSONL sink.
func (r *Recorder) Close() {
	if r.sink != nil {
		r.sink.close()
	}
}

// jsonlSink writes records to a JSONL file on a background goroutine with
// drop-on-overflow: observation must never block or crash the proxy path.
type jsonlSink struct {
	ch      chan RequestRecord
	file    *os.File
	w       *bufio.Writer
	done    chan struct{}
	dropped *uint64
}

func newJSONLSink(path string) (*jsonlSink, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("stats: create dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("stats: open jsonl: %w", err)
	}
	s := &jsonlSink{
		ch:      make(chan RequestRecord, 4096),
		file:    f,
		w:       bufio.NewWriterSize(f, 64*1024),
		done:    make(chan struct{}),
		dropped: new(uint64),
	}
	go s.loop()
	return s, nil
}

func (s *jsonlSink) write(rec RequestRecord) {
	select {
	case s.ch <- rec:
	default:
		atomic.AddUint64(s.dropped, 1)
	}
}

func (s *jsonlSink) loop() {
	defer close(s.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case rec, ok := <-s.ch:
			if !ok {
				s.flush()
				return
			}
			b, err := json.Marshal(rec)
			if err == nil {
				_, _ = s.w.Write(b)
				_, _ = s.w.WriteString("\n")
			}
		case <-ticker.C:
			s.flush()
		}
	}
}

func (s *jsonlSink) flush() {
	if err := s.w.Flush(); err != nil {
		// Persistence is best-effort by design; the in-memory aggregates stay
		// authoritative. Surface the failure via stderr once per flush.
		fmt.Fprintf(os.Stderr, "stats: jsonl flush: %v\n", err)
	}
}

func (s *jsonlSink) close() {
	close(s.ch)
	<-s.done
	_ = s.file.Close()
}

// sortedStrategyKeys is a tiny helper for stable output ordering.
func sortedStrategyKeys(m map[string]uint64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

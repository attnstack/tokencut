// budget.go — the per-request orchestration layer: per-request overrides,
// exemptions, intra-request dedup, intra-request diff, tiered per-message
// compression and the wall-clock latency budget.
//
// First principle (the one that matters): COMPRESSION MUST NEVER BREAK A
// REQUEST. Every failure path — malformed body, compressor panic, deadline
// exceeded, cache poisoned — degrades to "forward the original bytes".
package budget

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"tokencut/internal/classify"
	"tokencut/internal/compress"
	"tokencut/internal/tokenest"
)

type Config struct {
	Tier           string // lossless | balanced | aggressive
	Targets        map[string]float64
	ExemptSystem   bool
	ExemptLastUser bool
	DiffMode       bool
	LatencyBudget  time.Duration
	Cache          *compress.LRU
	Prune          compress.JSONPruneConfig
	Drain          compress.DrainConfig
}

func DefaultConfig() Config {
	return Config{
		Tier:           "balanced",
		Targets:        map[string]float64{"balanced": 0.60, "aggressive": 0.90, "lossless": 0},
		ExemptSystem:   true,
		ExemptLastUser: true,
		DiffMode:       true,
		LatencyBudget:  30 * time.Millisecond,
		Prune:          compress.DefaultJSONPruneConfig(),
		Drain:          compress.DefaultDrainConfig(),
	}
}

type MessageReport struct {
	Index          int    `json:"index"`
	Role           string `json:"role"`
	OriginalTokens int    `json:"original_tokens"`
	FinalTokens    int    `json:"final_tokens"`
	Strategy       string `json:"strategy"`
	Exempt         bool   `json:"exempt"`
}

type Report struct {
	Enabled        bool            `json:"enabled"`
	Model          string          `json:"model"`
	OverrideTier   string          `json:"override_tier,omitempty"`
	Messages       []MessageReport `json:"messages"`
	OriginalTokens int             `json:"original_tokens"`
	FinalTokens    int             `json:"final_tokens"`
	Ratio          float64         `json:"ratio"` // 1 - final/orig
	Strategies     map[string]int  `json:"strategies"`
	CacheHits      int             `json:"cache_hits"`
	Changed        bool            `json:"changed"`
	Note           string          `json:"note,omitempty"`
}

type Controller struct {
	cfg Config
}

func NewController(cfg Config) *Controller {
	return &Controller{cfg: cfg}
}

type wirePart struct {
	raw    map[string]json.RawMessage
	isText bool
	text   string
}

type wireMessage struct {
	raw     map[string]json.RawMessage
	role    string
	kind    int    // 0 = no/other content, 1 = string, 2 = parts array
	cKind   string // classify.Kind of the payload text (kind-aware token pricing)
	text    string
	parts   []wirePart
	partsC  []string
	origTok int
	strat   string
	exempt  bool
	changed bool
}

type override struct {
	enabled *bool
	tier    string
	target  float64
}

// Apply compresses the request body. It never returns a non-nil error for
// user-input reasons; any parse failure returns (original body, report,
// nil) and the proxy forwards transparently.
func (c *Controller) Apply(body []byte) ([]byte, *Report, error) {
	rep := &Report{Enabled: true, Strategies: map[string]int{}}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		rep.Note = "body is not a JSON object; forwarded unchanged"
		return body, rep, nil
	}
	msgsRaw, ok := envelope["messages"]
	if !ok {
		rep.Note = "no messages field; forwarded unchanged"
		return body, rep, nil
	}
	var rawArr []map[string]json.RawMessage
	if err := json.Unmarshal(msgsRaw, &rawArr); err != nil {
		rep.Note = "messages is not an array of objects; forwarded unchanged"
		return body, rep, nil
	}

	// Per-request override (used by the benchmark harness; also lets a client
	// pick a tier per call). Stripped before forwarding — never leaks upstream.
	var ov override
	if ob, exists := envelope["tokencut"]; exists {
		delete(envelope, "tokencut")
		var o struct {
			Enabled *bool   `json:"enabled"`
			Tier    string  `json:"tier"`
			Target  float64 `json:"target"`
		}
		if json.Unmarshal(ob, &o) == nil {
			ov.enabled = o.Enabled
			ov.tier = o.Tier
			ov.target = o.Target
			if o.Tier != "" {
				rep.OverrideTier = o.Tier
			}
		}
	}
	if ov.enabled != nil && !*ov.enabled {
		rep.Enabled = false
		rep.Note = "compression disabled by per-request override"
		// The override field must never reach upstream (APIs reject unknown
		// params), even when compression is off. Re-marshal is semantically
		// identical: messages stay as raw bytes, only top-level keys re-sort.
		out, err := json.Marshal(envelope)
		if err != nil {
			return body, rep, nil // fail open to original bytes
		}
		return out, rep, nil
	}

	if mb, ok := envelope["model"]; ok {
		var m string
		if json.Unmarshal(mb, &m) == nil {
			rep.Model = m
		}
	}

	msgs := parseMessages(rawArr)
	if len(msgs) == 0 {
		rep.Note = "empty messages; forwarded unchanged"
		return body, rep, nil
	}

	ratio := c.effectiveRatio(ov)
	keywords := compress.KeywordsOf(lastUserText(msgs), 12)

	for i, m := range msgs {
		m.origTok = m.estimate()
		rep.OriginalTokens += m.origTok
		if c.cfg.ExemptSystem && m.role == "system" {
			m.exempt = true
		}
		if c.cfg.ExemptLastUser && m.role == "user" && i == lastUserRoleIndex(msgs, "user") {
			m.exempt = true
		}
	}

	c.dedupPass(msgs, rep)
	if c.cfg.DiffMode && ratio > 0 {
		c.diffPass(msgs, rep)
	}
	c.compressPass(msgs, rep, ratio, keywords)

	changedAny := false
	for i, m := range msgs {
		if m.changed {
			changedAny = true
		}
		final := m.currentTokens()
		rep.FinalTokens += final
		strat := m.strat
		if strat == "" {
			if m.exempt {
				strat = "exempt"
			} else {
				strat = "none"
			}
		}
		rep.Strategies[strat]++
		rep.Messages = append(rep.Messages, MessageReport{
			Index: i, Role: m.role, OriginalTokens: m.origTok, FinalTokens: final,
			Strategy: strat, Exempt: m.exempt,
		})
	}
	rep.Ratio = 0
	if rep.OriginalTokens > 0 {
		rep.Ratio = 1 - float64(rep.FinalTokens)/float64(rep.OriginalTokens)
		if rep.Ratio < 0 {
			rep.Ratio = 0
		}
	}
	if !changedAny {
		// Nothing gained: return the ORIGINAL bytes so the proxy is
		// byte-transparent when it has nothing to offer.
		rep.Note = "no compression opportunity found"
		return body, rep, nil
	}
	rep.Changed = true

	for _, m := range msgs {
		if !m.changed {
			continue
		}
		switch m.kind {
		case 1:
			if b, err := json.Marshal(m.text); err == nil {
				m.raw["content"] = b
			} else {
				return body, rep, fmt.Errorf("budget: re-marshal string content: %w", err)
			}
		case 2:
			for j := range m.parts {
				if m.parts[j].isText && m.parts[j].text != m.partsC[j] {
					b, err := json.Marshal(m.partsC[j])
					if err != nil {
						return body, rep, fmt.Errorf("budget: re-marshal part text: %w", err)
					}
					m.parts[j].raw["text"] = b
				}
			}
			arr := make([]map[string]json.RawMessage, len(m.parts))
			for j := range m.parts {
				arr[j] = m.parts[j].raw
			}
			if b, err := json.Marshal(arr); err == nil {
				m.raw["content"] = b
			} else {
				return body, rep, fmt.Errorf("budget: re-marshal parts: %w", err)
			}
		}
	}
	newArr := make([]json.RawMessage, len(msgs))
	for i, m := range msgs {
		b, err := json.Marshal(m.raw)
		if err != nil {
			return body, rep, fmt.Errorf("budget: re-marshal message: %w", err)
		}
		newArr[i] = b
	}
	if b, err := json.Marshal(newArr); err != nil {
		return body, rep, fmt.Errorf("budget: re-marshal messages: %w", err)
	} else {
		envelope["messages"] = b
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return body, rep, fmt.Errorf("budget: re-marshal envelope: %w", err)
	}
	return out, rep, nil
}

func (c *Controller) effectiveRatio(ov override) float64 {
	if ov.target > 0 && ov.target < 1 {
		return ov.target
	}
	tier := c.cfg.Tier
	if _, ok := c.cfg.Targets[ov.tier]; ok {
		tier = ov.tier
	} else if ov.tier == "lossless" {
		tier = "lossless"
	}
	return c.cfg.Targets[tier]
}

func parseMessages(rawArr []map[string]json.RawMessage) []*wireMessage {
	out := make([]*wireMessage, 0, len(rawArr))
	for _, m := range rawArr {
		wm := &wireMessage{raw: m}
		if b, ok := m["role"]; ok {
			_ = json.Unmarshal(b, &wm.role)
		}
		if cb, ok := m["content"]; ok && len(cb) > 0 && string(cb) != "null" {
			var s string
			if err := json.Unmarshal(cb, &s); err == nil {
				wm.kind = 1
				wm.text = s
			} else {
				var parts []map[string]json.RawMessage
				if err2 := json.Unmarshal(cb, &parts); err2 == nil {
					wm.kind = 2
					for _, p := range parts {
						wp := wirePart{raw: p}
						if t, ok := p["type"]; ok && string(t) == `"text"` {
							wp.isText = true
							var tx string
							if json.Unmarshal(p["text"], &tx) == nil {
								wp.text = tx
							}
						}
						wm.parts = append(wm.parts, wp)
					}
				}
			}
		}
		if wm.kind == 2 {
			wm.partsC = make([]string, len(wm.parts))
			for j := range wm.parts {
				wm.partsC[j] = wm.parts[j].text
			}
		}
		// Classify once per message; the kind drives kind-aware token pricing
		// AND the dedup/diff/compressor routing below.
		switch {
		case wm.kind == 1:
			wm.cKind = string(classify.Sniff(wm.text).Kind)
		case wm.kind == 2:
			for j := range wm.parts {
				if wm.parts[j].text != "" {
					wm.cKind = string(classify.Sniff(wm.parts[j].text).Kind)
					break
				}
			}
		default:
			wm.cKind = string(classify.Text)
		}
		out = append(out, wm)
	}
	return out
}

func (m *wireMessage) estimate() int {
	switch m.kind {
	case 1:
		return tokenest.EstimateFor(m.cKind, m.text)
	case 2:
		t := 0
		for j := range m.parts {
			t += tokenest.EstimateFor(m.cKind, m.parts[j].text)
		}
		return t
	}
	return 0
}

func (m *wireMessage) currentTokens() int {
	switch m.kind {
	case 1:
		return tokenest.EstimateFor(m.cKind, m.text)
	case 2:
		t := 0
		for j := range m.parts {
			t += tokenest.EstimateFor(m.cKind, m.partsC[j])
		}
		return t
	}
	return 0
}

func lastUserText(msgs []*wireMessage) string {
	idx := lastUserRoleIndex(msgs, "user")
	if idx < 0 {
		return ""
	}
	return msgs[idx].text
}

func lastUserRoleIndex(msgs []*wireMessage, role string) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].role == role {
			return i
		}
	}
	return -1
}

// dedupPass replaces exact duplicate contents (same fingerprint) inside one
// request with a pointer to the first occurrence. The OpenAI wire format has
// no conversation id — multi-turn history is resent in full every turn — so
// this is where the "agent resent the same tool output 5 times" waste dies.
func (c *Controller) dedupPass(msgs []*wireMessage, rep *Report) {
	seen := map[uint64]int{}
	for i, m := range msgs {
		if m.kind != 1 || m.exempt || m.origTok <= 200 {
			continue
		}
		kind := classify.Kind(m.cKind)
		fp := compress.Fingerprint(m.text, kind)
		if j, dup := seen[fp]; dup {
			m.text = fmt.Sprintf("[duplicate of message #%d — identical content omitted]", j)
			m.strat = "dedup"
			m.changed = true
			continue
		}
		seen[fp] = i
	}
}

// diffPass rewrites "same content re-sent after a small edit" (file re-reads
// after an agent edit) as a change set against the earlier in-request
// version. Guarded: below similarity threshold or diff > 60% of full text →
// keep the full text.
func (c *Controller) diffPass(msgs []*wireMessage, rep *Report) {
	type baseEntry struct {
		index int
		text  string
	}
	bases := map[uint64]baseEntry{}
	for i, m := range msgs {
		if m.kind != 1 || m.exempt || m.origTok < 800 || m.changed {
			continue
		}
		res := classify.Result{Kind: classify.Kind(m.cKind), Confidence: 1}
		// JSON is excluded only because the pruner handles it better. No
		// confidence gate here: TryDiff has its own guards (similarity
		// threshold, size caps, output-ratio cap), which are the real safety.
		if res.Kind == classify.JSON {
			continue
		}
		lines := firstLines(m.text, 24)
		fp := compress.Fingerprint(lines, classify.Text)
		if b, ok := bases[fp]; ok {
			if d, ok := compress.TryDiff(b.text, m.text); ok {
				m.text = d
				m.strat = "diff"
				m.changed = true
				continue
			}
		}
		if _, exists := bases[fp]; !exists {
			bases[fp] = baseEntry{index: i, text: m.text}
		}
	}
}

func firstLines(s string, n int) string {
	count := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			count++
			if count >= n {
				return s[:i]
			}
		}
	}
	return s
}

// compressPass runs the tiered pipeline per message until the tier target is
// reached or no compressor helps. Lossless (canonicalization) always runs
// first; type-specific lossy compressors run only when a target is set.
func (c *Controller) compressPass(msgs []*wireMessage, rep *Report, ratio float64, keywords []compress.Keyword) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.LatencyBudget)
	defer cancel()
	for _, m := range msgs {
		if m.exempt || m.strat == "dedup" {
			continue
		}
		if ctx.Err() != nil {
			m.strat = "skipped-latency-budget"
			continue
		}
		if m.kind == 2 {
			hasNonText := false
			for j := range m.parts {
				if !m.parts[j].isText {
					hasNonText = true
					break
				}
			}
			if hasNonText {
				// Multimodal parts: images etc. are not ours to touch.
				m.strat = "skipped-multimodal"
				continue
			}
			for j := range m.parts {
				c.compressText(ctx, m, j, rep, ratio, keywords)
			}
			continue
		}
		if m.kind != 1 {
			continue
		}
		c.compressText(ctx, m, -1, rep, ratio, keywords)
	}
}

// compressText compresses one text unit (a string content, or one text part;
// partIdx = -1 for string content).
func (c *Controller) compressText(ctx context.Context, m *wireMessage, partIdx int, rep *Report, ratio float64, keywords []compress.Keyword) {
	get := func() string {
		if partIdx < 0 {
			return m.text
		}
		return m.partsC[partIdx]
	}
	set := func(s string) {
		if partIdx < 0 {
			m.text = s
			m.changed = true
		} else {
			m.partsC[partIdx] = s
			m.changed = true
		}
	}
	orig := get()
	if orig == "" {
		return
	}
	best := orig
	bestTok := tokenest.EstimateFor(m.cKind, best)
	strat := ""
	var decodedJSON any // canonical-stage decode result, reused by the pruner

	// Stage 1 — lossless canonicalization (JSON only): sorted keys, compact
	// separators. Zero information change. Kind is precomputed per message.
	// The decoded value is kept so Stage 3 skips a full re-decode.
	res := classify.Result{Kind: classify.Kind(m.cKind), Confidence: 1}
	if res.Kind == classify.JSON {
		if canon, val, err := compress.CanonicalJSONValue(best); err == nil {
			decodedJSON = val
			if tok := tokenest.EstimateFor(m.cKind, canon); tok < bestTok {
				best, bestTok, strat = canon, tok, "canonical"
			}
		}
	}

	// Stage 2 — cache lookup for the (pure) JSON pruner. Cache key binds the
	// canonical fingerprint to the target so multi-turn repeats are O(1).
	targetTok := bestTok
	if ratio > 0 {
		targetTok = int(float64(bestTok) * (1 - ratio))
	}
	cacheKey := ""
	if c.cfg.Cache != nil && res.Kind == classify.JSON {
		fp := compress.Fingerprint(best, classify.JSON)
		cacheKey = strconv.FormatUint(fp, 16) + "|" + c.cfg.Tier + "|" + strconv.FormatFloat(ratio, 'f', 3, 64)
		if v, ok := c.cfg.Cache.Get(cacheKey); ok {
			if ce, ok := v.(compress.CachedPrune); ok && ce.FinalTokens < bestTok {
				best, bestTok, strat = ce.Output, ce.FinalTokens, "json-prune"
				rep.CacheHits++
			}
		}
	}

	// Stage 3 — type-specific lossy compression while over budget.
	if bestTok > targetTok && ratio > 0 {
		switch res.Kind {
		case classify.JSON:
			var out string
			var pr *compress.PruneReport
			var err error
			if decodedJSON != nil {
				out, pr, err = compress.PruneJSONValue(decodedJSON, best, bestTok, targetTok, keywords, c.cfg.Prune)
			} else {
				out, pr, err = compress.PruneJSON(best, targetTok, keywords, c.cfg.Prune)
			}
			if err == nil && pr.FinalTokens < bestTok {
				best, bestTok, strat = out, pr.FinalTokens, "json-prune"
				if c.cfg.Cache != nil && cacheKey != "" {
					c.cfg.Cache.Put(cacheKey, compress.CachedPrune{Output: out, FinalTokens: pr.FinalTokens})
				}
			}
		case classify.Log:
			if dr := compress.DrainCompress(best, c.cfg.Drain); dr.OK {
				if tok := tokenest.EstimateFor(m.cKind, dr.Output); tok < bestTok {
					best, bestTok, strat = dr.Output, tok, "log-drain"
				}
			}
		case classify.Code:
			if s, ok := compress.SlimCode(best); ok {
				if tok := tokenest.EstimateFor(m.cKind, s); tok < bestTok {
					best, bestTok, strat = s, tok, "code-slim"
				}
			}
		default:
			// html/table/text: no v1 compressor; strategy stays lossless-only.
		}
	}

	if strat != "" && bestTok < tokenest.EstimateFor(m.cKind, orig) {
		set(best)
		m.strat = strat
	}
}

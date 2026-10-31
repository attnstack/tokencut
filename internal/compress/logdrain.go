// logdrain.go — Drain-style log template mining (He et al., OSDI'17),
// simplified for the request path: variable-looking tokens are masked BEFORE
// tree routing, templates merge at leaves by token similarity.
//
// Honest scope note: the output is structure-lossless but parameter-LOSSY — per-line parameter association
// is dropped, and what survives is template structure + counts + top distinct
// variable samples. "1万行 → 几十行模板" 的压缩率恰恰来自丢弃参数关联；保留
// 全量参数表等于没压缩。If mining yields no meaningful win (few repeated
// lines), OK=false and the caller keeps the original text.
package compress

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type DrainConfig struct {
	Depth        int     // internal-tree routing depth
	SimThreshold float64 // merge threshold at leaves
	MaxChildren  int     // fan-out cap per node before falling back to <*>
	MaxSamples   int     // distinct variable samples kept per template
	MaxTemplates int     // output cap; overflow summarized
	MinLines     int     // below this, mining cannot pay for itself
}

func DefaultDrainConfig() DrainConfig {
	return DrainConfig{Depth: 4, SimThreshold: 0.4, MaxChildren: 100, MaxSamples: 5, MaxTemplates: 50, MinLines: 20}
}

type DrainResult struct {
	Output    string
	Lines     int
	Templates int
	OK        bool
	Reason    string
}

type dtNode struct {
	kids map[string]*dtNode
	tpls []*dtTemplate
}

type dtTemplate struct {
	pattern []string
	count   int
	samples map[string]int // variable tuple → occurrences
	capped  bool
}

var (
	reNum   = regexp.MustCompile(`^-?\d+([.,]\d+)?$`)
	reHex   = regexp.MustCompile(`^[0-9a-fA-F]{8,}$`)
	reUUID  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reIP    = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
	reTrail = regexp.MustCompile(`[",;:)\]!]+$`)
)

// maskToken marks variable-looking tokens. UUIDs/IPs/hex become <*> whole
// (an IP:PORT must stay ONE variable — splitting "host:<*>" would strand the
// port outside every sample); "status=200" keeps its key ("status=<*>", the
// logfmt case, '=' only); any other digit-bearing token becomes <*>.
func maskToken(tok string) (string, bool) {
	t := reTrail.ReplaceAllString(tok, "")
	hasDigit := strings.ContainsAny(t, "0123456789")
	if reUUID.MatchString(t) || reIP.MatchString(t) || (reHex.MatchString(t) && hasDigit) {
		return "<*>", true
	}
	if i := strings.IndexByte(t, '='); i >= 0 && i+1 < len(t) {
		if reNum.MatchString(t[i+1:]) {
			return t[:i+1] + "<*>", true
		}
	}
	if hasDigit {
		return "<*>", true
	}
	return tok, false
}

type maskedLine struct {
	toks  []string // original tokens (for variable samples)
	mask  []string // masked tokens (for routing/template/similarity)
}

func DrainCompress(text string, cfg DrainConfig) DrainResult {
	if cfg.Depth <= 0 {
		cfg = DefaultDrainConfig()
	}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	rawLines := strings.Split(normalized, "\n")
	lines := make([]maskedLine, 0, len(rawLines))
	for _, rl := range rawLines {
		f := strings.Fields(rl)
		if len(f) == 0 {
			continue
		}
		ml := maskedLine{toks: f, mask: make([]string, len(f))}
		for i, tok := range f {
			m, _ := maskToken(tok)
			ml.mask[i] = m
		}
		lines = append(lines, ml)
	}
	if len(lines) < cfg.MinLines {
		return DrainResult{Reason: "too few lines to mine", Lines: len(lines)}
	}

	// Length bucketing: templates never cross different token counts.
	buckets := map[int][]maskedLine{}
	for _, ml := range lines {
		buckets[len(ml.mask)] = append(buckets[len(ml.mask)], ml)
	}
	root := &dtNode{kids: map[string]*dtNode{}}
	for _, bucket := range buckets {
		for _, ml := range bucket {
			insertLine(root, ml, cfg)
		}
	}

	tpls := collectTemplates(root)
	if len(tpls) == 0 {
		return DrainResult{Lines: len(lines), Reason: "no templates"}
	}
	if len(tpls) > len(lines)/2 {
		// Mining found little repetition: the "compressed" form would be as
		// long as the input. Fail safe → caller keeps the original.
		return DrainResult{Lines: len(lines), Templates: len(tpls), Reason: "poor repetition"}
	}
	sort.Slice(tpls, func(i, j int) bool { return tpls[i].count > tpls[j].count })

	var sb strings.Builder
	shown := 0
	if len(tpls) > cfg.MaxTemplates {
		shown = cfg.MaxTemplates
	} else {
		shown = len(tpls)
	}
	fmt.Fprintf(&sb, "[logdrain] %d lines -> %d templates; per-line variable values dropped, samples below\n", len(lines), len(tpls))
	for i := 0; i < shown; i++ {
		t := tpls[i]
		fmt.Fprintf(&sb, "x%d %s\n", t.count, strings.Join(t.pattern, " "))
		if len(t.samples) > 0 {
			vals := make([]string, 0, len(t.samples))
			for k := range t.samples {
				vals = append(vals, k)
			}
			sort.Slice(vals, func(a, b int) bool { return t.samples[vals[a]] > t.samples[vals[b]] })
			if len(vals) > cfg.MaxSamples {
				vals = vals[:cfg.MaxSamples]
			}
			// \x1f is the internal tuple separator; render samples with plain
			// spaces so the output stays model-friendly.
			for i := range vals {
				vals[i] = strings.ReplaceAll(vals[i], "\x1f", " ")
			}
			distinct := len(t.samples)
			if t.capped {
				fmt.Fprintf(&sb, "  vars: %s (+many distinct)\n", strings.Join(vals, " | "))
			} else {
				fmt.Fprintf(&sb, "  vars: %s (+%d distinct)\n", strings.Join(vals, " | "), distinct-len(vals))
			}
		}
	}
	if len(tpls) > shown {
		rest := 0
		for _, t := range tpls[shown:] {
			rest += t.count
		}
		fmt.Fprintf(&sb, "(+%d more templates, %d lines)\n", len(tpls)-shown, rest)
	}
	return DrainResult{Output: sb.String(), Lines: len(lines), Templates: len(tpls), OK: true}
}

func insertLine(root *dtNode, ml maskedLine, cfg DrainConfig) {
	cur := root
	for d := 0; d < cfg.Depth; d++ {
		tok := ml.mask[d] // same bucket ⇒ same token count ⇒ index safe
		if child, ok := cur.kids[tok]; ok {
			cur = child
			continue
		}
		if child, ok := cur.kids["<*>"]; ok && len(cur.kids) >= cfg.MaxChildren {
			cur = child
			continue
		}
		key := tok
		if len(cur.kids) >= cfg.MaxChildren {
			key = "<*>" // fan-out cap: too many distinct prefixes → generalize
		}
		nxt := &dtNode{kids: map[string]*dtNode{}}
		cur.kids[key] = nxt
		cur = nxt
	}
	best, bestSim := -1, 0.0
	for i, t := range cur.tpls {
		if s := tokenSim(t.pattern, ml.mask); s > bestSim {
			bestSim, best = s, i
		}
	}
	if best >= 0 && bestSim >= cfg.SimThreshold {
		t := cur.tpls[best]
		for i := range t.pattern {
			if t.pattern[i] != ml.mask[i] {
				t.pattern[i] = "<*>" // union: disagreements generalize
			}
		}
		t.count++
		addVars(t, ml)
		return
	}
	pat := make([]string, len(ml.mask))
	copy(pat, ml.mask)
	t := &dtTemplate{pattern: pat, count: 1, samples: map[string]int{}}
	addVars(t, ml)
	cur.tpls = append(cur.tpls, t)
}

func addVars(t *dtTemplate, ml maskedLine) {
	var tuple []string
	for i, p := range t.pattern {
		// Sample every variable position, including prefix-preserved masks
		// like "status=<*>": the concrete value is what models (and the
		// reference solver) need to recover.
		if strings.HasSuffix(p, "<*>") && i < len(ml.toks) {
			tuple = append(tuple, ml.toks[i])
		}
	}
	if len(tuple) == 0 {
		return
	}
	if len(t.samples) >= 10000 {
		t.capped = true
		return
	}
	t.samples[strings.Join(tuple, "\x1f")]++
}

func tokenSim(a, b []string) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	m := 0
	for i := range a {
		if a[i] == b[i] {
			m++
		}
	}
	return float64(m) / float64(len(a))
}

func collectTemplates(root *dtNode) []*dtTemplate {
	var out []*dtTemplate
	var walk func(n *dtNode)
	walk = func(n *dtNode) {
		out = append(out, n.tpls...)
		for _, c := range n.kids {
			walk(c)
		}
	}
	walk(root)
	return out
}

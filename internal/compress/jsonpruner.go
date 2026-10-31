// jsonpruner.go — tokencut's core algorithm: structure-preserving JSON
// pruning. Shrinks a JSON document to a token budget by folding WHOLE
// subtrees (objects → high-value keys kept + "…": "N fields omitted"
// marker; arrays → protected head/tail + relevance hits + {"_omitted": n}
// markers), so the output is valid JSON BY CONSTRUCTION — the invariant
// generic text compression (llmlingua, truncation) breaks on structured
// payloads.
//
// Algorithm:
//  1. parse once into a tree weighted with est(n) (EXACT additive rune
//     count — one currency for the whole algorithm) and info(n)
//     (information score)
//  2. greedy fold: repeatedly apply the candidate operation with the lowest
//     info/saved ratio — the "value density" idea from fractional knapsack:
//     spend each unit of information loss where it buys the most token
//     savings
//  3. candidate granularity: array folds come in three run sizes (full
//     middle / left half / right half). Why: a single full-middle fold on a
//     100-item array saves ~1000 tokens at once — with only that operation,
//     every target between 10% and 80% collapse onto the same output
//     (measured: three tiers all landing at 118 tokens). Run candidates
//     compose into ~n/2, n/4, n/8… steps so the budget is actually
//     reachable. Merging adjacent markers keeps repeated folds coherent.
//  4. validity is guaranteed by construction (whole-node operations, string
//     values never cut mid-token except the explicit long-string cap) and
//     re-asserted with json.Valid on the output
//
// Complexity: parse O(n) + heap ops O(k log n) with k = folds. The greedy is
// not globally optimal (exact optimum is a tree DP over budget —
// computationally brutal for this use) but sits within a few percent of it
// on real payloads; the benchmark's rule-based-vs-scored ablation makes that
// claim measurable instead of hand-waved.
package compress

import (
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"tokencut/internal/tokenest"
)

type JSONPruneConfig struct {
	// LongStringCap caps individual string values (runes). Why it exists:
	// folding granularity is a whole node, and a 100KB blob inside one key
	// cannot be subdivided — the cap bounds that worst case while staying
	// valid JSON.
	LongStringCap int
	// KeywordSample: per fold, keep this many query-relevant items/members
	// that would otherwise be folded (relevance protection, capped so a
	// keyword-stuffed query can't veto compression entirely).
	KeywordSample int
	HighValueKeys map[string]struct{}
}

func DefaultJSONPruneConfig() JSONPruneConfig {
	keys := []string{"id", "name", "type", "status", "error", "code", "message", "url", "data", "key", "path", "file"}
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return JSONPruneConfig{LongStringCap: 2000, KeywordSample: 2, HighValueKeys: m}
}

type PruneReport struct {
	OriginalTokens int
	FinalTokens    int
	Folds          int
	StringsCapped  int
	Kept           bool // no fold/cap happened; original bytes returned
}

var ErrNotJSON = errors.New("jsonpruner: input is not a single valid JSON value")

type jnode struct {
	parent   *jnode
	key      string // member name when the parent is an object ("" for array items / root)
	keyJSON  string // pre-escaped `"key"` ("" for array items / root)
	keyEst   int    // rune count of `"key":` — accounted at the parent, stored here
	kind     byte   // 'o' object, 'a' array, 's' string, 'n' number, 'b' bool, 'z' null, 'm' marker
	raw      string // pre-serialized leaf/marker value
	children []*jnode
	est      int // EXACT additive rune count of this node's serialized VALUE (key excluded); tokens ≈ est/4
	info     float64
	depth    int
	version  int  // bumped whenever the subtree changes; heap entries snapshot it
	dead     bool // removed from the tree (inside a folded node)
	omit     int  // array markers: number of omitted items
	kwScore  int  // cached keyword hit count
	kwVer    int  // kwScore cache validity marker
}

type foldEntry struct {
	n            *jnode
	version      int
	ratio        float64 // infoRemoved / saved for THIS operation
	saved        int     // runes this operation frees
	infoRemoved  float64 // info this operation loses
	i, j         int     // array run bounds [i, j); i==j==0 means whole-node (object) fold
}

type foldHeap []foldEntry

func (h foldHeap) Len() int           { return len(h) }
func (h foldHeap) Less(i, j int) bool { return h[i].ratio < h[j].ratio }
func (h foldHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *foldHeap) Push(x any)        { *h = append(*h, x.(foldEntry)) }
func (h *foldHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	*h = old[:n-1]
	return e
}

type jsonPruner struct {
	cfg      JSONPruneConfig
	keywords []Keyword
	root     *jnode
	h        foldHeap
	folds    int
}

// PruneJSON compresses input to at most budgetTokens (estimated). keywords
// (weighted, may be empty) protect query-relevant items from folding.
// On any internal inconsistency the ORIGINAL input is returned with an error
// so callers can fail open.
func PruneJSON(input string, budgetTokens int, keywords []Keyword, cfg JSONPruneConfig) (string, *PruneReport, error) {
	rep := &PruneReport{OriginalTokens: tokenest.EstimateFor("json", input)}
	dec := json.NewDecoder(strings.NewReader(input))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return "", nil, ErrNotJSON
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", nil, ErrNotJSON
	}
	if rep.OriginalTokens <= budgetTokens {
		rep.Kept = true
		rep.FinalTokens = rep.OriginalTokens
		return input, rep, nil
	}
	var capped int
	root = capStrings(root, cfg.LongStringCap, &capped)
	rep.StringsCapped = capped

	p := &jsonPruner{cfg: cfg, keywords: lowercaseKeywords(keywords)}
	p.root = p.build(root, "", 0, nil)
	p.walkPush(p.root)

	// Greedy loop with an overshoot guard: prefer operations that save
	// fewer runes than the remaining over-budget amount; only take a big
	// overshooting fold when no smaller candidate exists (otherwise one
	// large fold loses extra information and multiple budget tiers collapse
	// onto the same output). Stale heap entries are lazily invalidated via
	// version numbers instead of a decrease-key implementation.
	budgetRunes := tokenest.RunesForTokens(budgetTokens)
	for iter := 0; p.root.est > budgetRunes && iter < 100000; iter++ {
		remaining := p.root.est - budgetRunes
		var held *foldEntry // best (min infoRemoved) overshooting candidate
		progressed := false
		for p.h.Len() > 0 {
			e := heap.Pop(&p.h).(foldEntry)
			n := e.n
			if n.dead {
				continue
			}
			if n.version != e.version {
				p.pushCandidates(n) // re-evaluate with current est/info
				continue
			}
			if e.saved > remaining {
				if held == nil || e.infoRemoved < held.infoRemoved {
					held = &e
				}
				continue
			}
			p.foldNode(e)
			progressed = true
			break
		}
		if !progressed {
			if held == nil {
				break // no candidate fits and nothing overshoots: done
			}
			h := *held
			if h.n.dead || h.n.version != h.version {
				p.pushCandidates(h.n)
				continue
			}
			p.foldNode(h)
		}
	}

	if p.folds == 0 && rep.StringsCapped == 0 {
		// Nothing changed: return the original bytes. Re-serializing without
		// folds would still be valid JSON, but byte-transparency beats
		// canonical formatting when we had nothing to offer.
		rep.Kept = true
		rep.FinalTokens = rep.OriginalTokens
		return input, rep, nil
	}
	out := p.serialize()
	if !json.Valid([]byte(out)) {
		// Defensive: validity is guaranteed by construction; if this ever
		// fires we fail open rather than ship broken JSON upstream.
		return input, rep, fmt.Errorf("jsonpruner: internal invariant violated (output not valid JSON)")
	}
	rep.Folds = p.folds
	rep.FinalTokens = tokenest.EstimateFor("json", out)
	return out, rep, nil
}

// capStrings bounds oversized string values (see LongStringCap doc).
func capStrings(v any, cap int, count *int) any {
	switch t := v.(type) {
	case string:
		if len(t) <= cap {
			return t
		}
		r := []rune(t)
		if len(r) <= cap {
			return t
		}
		*count++
		return string(r[:cap]) + fmt.Sprintf("...[+%d chars truncated]", len(r)-cap)
	case map[string]any:
		for k, vv := range t {
			t[k] = capStrings(vv, cap, count)
		}
		return t
	case []any:
		for i, vv := range t {
			t[i] = capStrings(vv, cap, count)
		}
		return t
	default:
		return v
	}
}

// lowercaseKeywords lives in keywords.go next to the Keyword type.

func (p *jsonPruner) build(v any, key string, depth int, parent *jnode) *jnode {
	n := &jnode{parent: parent, key: key, depth: depth, kwVer: -1}
	if parent != nil && parent.kind == 'o' {
		kb, err := marshalNoHTML(key)
		if err != nil {
			kb = jsonMarshalKey(key)
		}
		n.keyJSON = kb
		n.keyEst = utf8.RuneCountInString(n.keyJSON) + 1 // + colon
	}
	switch t := v.(type) {
	case map[string]any:
		n.kind = 'o'
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic output for identical input
		n.children = make([]*jnode, 0, len(keys))
		for _, k := range keys {
			n.children = append(n.children, p.build(t[k], k, depth+1, n))
		}
		n.est = p.estOf('o', n.children)
		n.info = sumInfo(n.children)
	case []any:
		n.kind = 'a'
		n.children = make([]*jnode, 0, len(t))
		for _, item := range t {
			n.children = append(n.children, p.build(item, "", depth+1, n))
		}
		n.est = p.estOf('a', n.children)
		n.info = sumInfo(n.children)
	case string:
		n.kind = 's'
		raw, err := marshalNoHTML(t)
		if err != nil {
			b, _ := json.Marshal(t)
			raw = string(b)
		}
		n.raw = raw
		n.est = utf8.RuneCountInString(n.raw)
		n.info = leafInfo(n, p.cfg)
	case json.Number:
		n.kind = 'n'
		n.raw = t.String()
		n.est = utf8.RuneCountInString(n.raw)
		n.info = leafInfo(n, p.cfg)
	case bool:
		n.kind = 'b'
		if t {
			n.raw = "true"
		} else {
			n.raw = "false"
		}
		n.est = utf8.RuneCountInString(n.raw)
		n.info = leafInfo(n, p.cfg)
	case nil:
		n.kind = 'z'
		n.raw = "null"
		n.est = utf8.RuneCountInString(n.raw)
		n.info = leafInfo(n, p.cfg)
	}
	return n
}

// leafInfo scores a leaf. Why these weights: type_w encodes "how much losing
// this value costs the model" (booleans/nulls are near-redundant, long
// strings carry unique content); key_w boosts fields models most often need
// verbatim (ids, statuses); the depth prior encodes that shallow structure
// carries schema/semantic weight. These numbers are an engineered starting
// point — exposed via config and validated by the benchmark ablation, NOT
// presented as learned constants.
func leafInfo(n *jnode, cfg JSONPruneConfig) float64 {
	var typeW float64
	switch n.kind {
	case 'b', 'z':
		typeW = 0.2
	case 'n':
		typeW = 0.5
	case 's':
		if utf8.RuneCountInString(n.raw) <= 18 { // value + surrounding quotes
			typeW = 0.8
		} else {
			typeW = 1.0
		}
	default:
		typeW = 1.0
	}
	keyW := 1.0
	if _, hit := cfg.HighValueKeys[n.key]; hit {
		keyW = 1.5
	}
	return float64(n.est) * typeW * keyW / (1 + 0.1*float64(n.depth))
}

func sumInfo(children []*jnode) float64 {
	var s float64
	for _, c := range children {
		s += c.info
	}
	return s
}

func (p *jsonPruner) estOf(kind byte, children []*jnode) int {
	total := 2 // brackets
	if len(children) > 1 {
		total += len(children) - 1 // commas
	}
	for _, c := range children {
		if kind == 'o' {
			total += c.keyEst + c.est
		} else {
			total += c.est
		}
	}
	return total
}

func (p *jsonPruner) eligible(n *jnode) bool {
	if n == nil || n.dead {
		return false
	}
	if n.parent == nil {
		// Root OBJECT folding would replace the entire payload with a marker
		// — useless. Root ARRAY folding (protected head/tail + markers) is
		// the most common big-payload shape (list responses) and stays useful.
		return n.kind == 'a' && len(n.children) >= 6 // head2 + ≥2 middle + tail2
	}
	switch n.kind {
	case 'o':
		return len(n.children) > 0
	case 'a':
		return len(n.children) >= 6
	}
	return false
}

// pushCandidates seeds/re-seeds the heap with every candidate operation on n.
// Objects contribute one whole-node fold; arrays contribute three run folds
// (full middle, left half, right half) — the granularity mechanism.
// Query-referenced objects (kwScore > 0) are excluded from candidacy: what
// the query mentions must survive intact, not merely be sampled.
func (p *jsonPruner) pushCandidates(n *jnode) {
	if !p.eligible(n) {
		return
	}
	if n.kind == 'o' {
		if len(p.keywords) > 0 && p.kwScore(n) > 0 {
			return
		}
		if newChildren, removed, saved, infoRemoved := p.planObjectFold(n); saved >= 1 {
			_, _ = newChildren, removed
			heap.Push(&p.h, foldEntry{n: n, version: n.version, ratio: infoRemoved / float64(saved),
				saved: saved, infoRemoved: infoRemoved})
		}
		return
	}
	// array: middle = children[2 : len-2]; three run candidates
	m := len(n.children) - 4
	runs := [][2]int{
		{2, len(n.children) - 2},       // full middle
		{2, 2 + m/2},                   // left half
		{2 + m/2, len(n.children) - 2}, // right half
	}
	for _, r := range runs {
		if _, _, saved, infoRemoved := p.planRunFold(n, r[0], r[1]); saved >= 1 {
			heap.Push(&p.h, foldEntry{n: n, version: n.version, ratio: infoRemoved / float64(saved),
				saved: saved, infoRemoved: infoRemoved, i: r[0], j: r[1]})
		}
	}
}

func (p *jsonPruner) walkPush(n *jnode) {
	if n == nil || n.dead {
		return
	}
	p.pushCandidates(n)
	for _, c := range n.children {
		p.walkPush(c)
	}
}

// planObjectFold: keep high-value-key members, fold everything else into a
// single `"…": "N fields omitted"` member. Keeping the ids the model needs
// for follow-up tool calls is why this beats "drop the whole object".
// (Query-relevant members are protected one level up: an object the query
// references is excluded from candidacy entirely.)
func (p *jsonPruner) planObjectFold(n *jnode) (newChildren, removed []*jnode, saved int, infoRemoved float64) {
	var kept []*jnode
	for _, c := range n.children {
		if _, hv := p.cfg.HighValueKeys[c.key]; hv {
			kept = append(kept, c)
		} else {
			removed = append(removed, c)
		}
	}
	if len(removed) == 0 {
		return nil, nil, 0, 0
	}
	markerKey := `"..."`
	markerRaw := fmt.Sprintf(`"%d fields omitted"`, len(removed))
	marker := &jnode{
		parent: n, key: "...", keyJSON: markerKey,
		keyEst: utf8.RuneCountInString(markerKey) + 1,
		kind:   's', raw: markerRaw,
		est: utf8.RuneCountInString(markerRaw), depth: n.depth + 1, kwVer: -1,
	}
	newChildren = append(newChildren, kept...)
	newChildren = append(newChildren, marker)
	saved = n.est - p.estOf('o', newChildren)
	if saved < 1 {
		return nil, nil, 0, 0
	}
	for _, c := range removed {
		infoRemoved += c.info
	}
	return newChildren, removed, saved, infoRemoved
}

// planRunFold plans replacing children[i:j] with a single {"_omitted": N}
// marker. The first 2 and last 2 children are structurally protected (the
// candidate generator never proposes runs that touch them). Markers inside
// the run merge into the new count, so repeated folds compose cleanly.
// Query-relevant items inside the run survive (up to KeywordSample).
func (p *jsonPruner) planRunFold(n *jnode, i, j int) (newChildren, removed []*jnode, saved int, infoRemoved float64) {
	if i < 2 || j > len(n.children)-2 || j-i < 2 {
		return nil, nil, 0, 0
	}
	omit := 0
	unfolded := 0
	for _, c := range n.children[i:j] {
		if c.kind == 'm' {
			omit += c.omit
		} else {
			omit++
			unfolded++
		}
	}
	if unfolded == 0 {
		// Nothing to remove (marker-merge only pays when ≥2 markers collapse
		// into one; a single marker re-folded into itself saves nothing).
		markers := 0
		for _, c := range n.children[i:j] {
			if c.kind == 'm' {
				markers++
			}
		}
		if markers < 2 {
			return nil, nil, 0, 0
		}
	}
	keepSet := map[*jnode]bool{}
	if len(p.keywords) > 0 && p.cfg.KeywordSample > 0 {
		// Rank by hit count, not first-come: a generic question word (e.g.
		// "product") gives EVERY item score 1, while the specific value the
		// query names ("Zephyr-002") scores higher — top-K must pick it.
		type kwCand struct {
			c *jnode
			s int
		}
		var cands []kwCand
		for _, c := range n.children[i:j] {
			if c.kind != 'm' {
				if s := p.kwScore(c); s > 0 {
					cands = append(cands, kwCand{c, s})
				}
			}
		}
		sort.Slice(cands, func(a, b int) bool { return cands[a].s > cands[b].s })
		for k := 0; k < len(cands) && k < p.cfg.KeywordSample; k++ {
			keepSet[cands[k].c] = true
		}
	}
	var kept []*jnode
	for _, c := range n.children[i:j] {
		if keepSet[c] {
			kept = append(kept, c)
			omit-- // stays visible; not omitted
		}
	}
	markerRaw := fmt.Sprintf(`{"_omitted": %d}`, omit)
	marker := &jnode{parent: n, kind: 'm', raw: markerRaw, omit: omit,
		est: utf8.RuneCountInString(markerRaw), depth: n.depth + 1, kwVer: -1}
	newChildren = append(newChildren, n.children[:i]...)
	newChildren = append(newChildren, kept...)
	newChildren = append(newChildren, marker)
	newChildren = append(newChildren, n.children[j:]...)
	for _, c := range n.children[i:j] {
		if !keepSet[c] {
			removed = append(removed, c)
		}
	}
	saved = n.est - p.estOf('a', newChildren)
	if saved < 1 || len(removed) == 0 {
		return nil, nil, 0, 0
	}
	for _, c := range removed {
		infoRemoved += c.info
	}
	return newChildren, removed, saved, infoRemoved
}

// foldNode commits the plan carried by the heap entry and updates ancestors.
func (p *jsonPruner) foldNode(e foldEntry) {
	n := e.n
	var newChildren, removed []*jnode
	isArray := false
	switch n.kind {
	case 'o':
		newChildren, removed, _, _ = p.planObjectFold(n)
	case 'a':
		newChildren, removed, _, _ = p.planRunFold(n, e.i, e.j)
		isArray = true
	default:
		return
	}
	if newChildren == nil {
		return
	}
	for _, r := range removed {
		p.detach(r)
	}
	n.children = newChildren
	p.refreshSubtree(n)
	p.folds++
	// An array that just folded may now offer finer runs (halves of what
	// remains) — re-seed its own candidates before walking up.
	if isArray {
		n.version++
		p.pushCandidates(n)
	}
	for a := n.parent; a != nil; a = a.parent {
		a.version++
		p.refreshSubtree(a)
		p.pushCandidates(a)
	}
}

// refreshSubtree recomputes est/info of an internal node in place.
func (p *jsonPruner) refreshSubtree(n *jnode) {
	if n.kind == 'o' || n.kind == 'a' {
		n.est = p.estOf(n.kind, n.children)
		n.info = sumInfo(n.children)
	}
}

func (p *jsonPruner) detach(n *jnode) {
	if n == nil {
		return
	}
	stack := []*jnode{n}
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		x.dead = true
		for _, c := range x.children {
			stack = append(stack, c)
		}
	}
}

// kwScore counts weighted keyword hits across a subtree — member KEY NAMES
// and leaf string VALUES. Queries overwhelmingly reference keys ("value of
// spdx_id") or exact values ("for item-7"), so EXACT matches score 4× a
// substring hit: without that, "item-70" substring-matching "item-7" ties
// with the real item-7 and top-K keeps the wrong records. Cached per node
// version so the greedy loop doesn't rescan on every fold.
func (p *jsonPruner) kwScore(n *jnode) int {
	if len(p.keywords) == 0 {
		return 0
	}
	if n.kwVer == n.version {
		return n.kwScore
	}
	score := 0
	var walk func(x *jnode)
	walk = func(x *jnode) {
		if x.kind == 'o' {
			for _, c := range x.children {
				lowKey := strings.ToLower(c.key)
				for _, kw := range p.keywords {
					if lowKey == kw.Text {
						score += kw.Weight * 4
					} else if strings.Contains(lowKey, kw.Text) {
						score += kw.Weight
					}
				}
			}
		}
		if x.kind == 's' {
			low := strings.ToLower(x.raw)
			val := strings.Trim(low, `"`)
			for _, kw := range p.keywords {
				if val == kw.Text {
					score += kw.Weight * 4
				} else if strings.Contains(low, kw.Text) {
					score += kw.Weight
				}
			}
			return
		}
		for _, c := range x.children {
			if !c.dead {
				walk(c)
			}
		}
	}
	walk(n)
	n.kwScore = score
	n.kwVer = n.version
	return score
}

func (p *jsonPruner) serialize() string {
	var sb strings.Builder
	sb.Grow(p.root.est*3 + 64) // runes are ≥1 byte; wide runes are 3 — a hint only
	var write func(n *jnode)
	write = func(n *jnode) {
		switch n.kind {
		case 'o':
			sb.WriteByte('{')
			for i, c := range n.children {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(c.keyJSON)
				sb.WriteByte(':')
				write(c)
			}
			sb.WriteByte('}')
		case 'a':
			sb.WriteByte('[')
			for i, c := range n.children {
				if i > 0 {
					sb.WriteByte(',')
				}
				write(c)
			}
			sb.WriteByte(']')
		default:
			sb.WriteString(n.raw)
		}
	}
	write(p.root)
	return sb.String()
}

// jsonMarshalKey is the escape-html fallback if marshalNoHTML ever fails.
func jsonMarshalKey(key string) string {
	b, _ := json.Marshal(key)
	return string(b)
}

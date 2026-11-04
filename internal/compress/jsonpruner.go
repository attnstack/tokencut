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
	// Incremental aggregate state for internal nodes ('o'/'a'): maintained on
	// every structural change so ancestor updates are O(1) per fold instead
	// of O(siblings) re-scans. est derives from these in O(1) — see estFromSum.
	childEstSum  int     // Σ contrib(c): c.est (+ c.keyEst when this node is an object)
	childInfoSum float64 // Σ c.info
	childCount   int
	depth        int
	version      int  // bumped whenever the subtree changes; heap entries snapshot it
	dead         bool // removed from the tree (inside a folded node)
	omit         int  // array markers: number of omitted items
	kwScore      int  // cached keyword hit count
	kwVer        int  // kwScore cache validity marker
}

type foldEntry struct {
	n           *jnode
	version     int
	ratio       float64 // infoRemoved / saved for THIS operation
	saved       int     // runes this operation frees
	infoRemoved float64 // info this operation loses
	i, j        int     // array run bounds [i, j); i==j==0 means whole-node (object) fold
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
	cfg        JSONPruneConfig
	keywords   []Keyword
	root       *jnode
	h          foldHeap
	folds      int
	nodeChunks [][]jnode // slab arena: nodes are tree-lifetime, so chunked
	// preallocation replaces ~50k individual mallocs on large payloads;
	// chunk caps keep node pointers stable across growth
}

const nodeChunkSize = 1024

func (p *jsonPruner) newNode() *jnode {
	n := len(p.nodeChunks)
	if n == 0 || len(p.nodeChunks[n-1]) == nodeChunkSize {
		p.nodeChunks = append(p.nodeChunks, make([]jnode, 0, nodeChunkSize))
		n = len(p.nodeChunks)
	}
	ch := &p.nodeChunks[n-1]
	*ch = append(*ch, jnode{kwVer: -1})
	return &(*ch)[len(*ch)-1]
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
	return pruneFromValue(root, input, rep, budgetTokens, keywords, cfg)
}

// PruneJSONValue is PruneJSON for an already-decoded document (from
// CanonicalJSONValue), saving a full re-decode of the canonical form.
// input is returned unchanged on the Kept path; origEst must be the token
// estimate of that input.
func PruneJSONValue(root any, input string, origEst, budgetTokens int, keywords []Keyword, cfg JSONPruneConfig) (string, *PruneReport, error) {
	return pruneFromValue(root, input, &PruneReport{OriginalTokens: origEst}, budgetTokens, keywords, cfg)
}

func pruneFromValue(root any, input string, rep *PruneReport, budgetTokens int, keywords []Keyword, cfg JSONPruneConfig) (string, *PruneReport, error) {
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
	n := p.newNode()
	n.parent = parent
	n.key = key
	n.depth = depth
	if parent != nil && parent.kind == 'o' {
		if kb, ok := quoteStringFast(key); ok {
			n.keyJSON = kb
		} else {
			kb, err := marshalNoHTML(key)
			if err != nil {
				kb = jsonMarshalKey(key)
			}
			n.keyJSON = kb
		}
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
			c := p.build(t[k], k, depth+1, n)
			n.children = append(n.children, c)
			n.childEstSum += c.keyEst + c.est
			n.childInfoSum += c.info
			n.childCount++
		}
		n.est = estFromSum(n.childCount, n.childEstSum)
		n.info = n.childInfoSum
	case []any:
		n.kind = 'a'
		n.children = make([]*jnode, 0, len(t))
		for _, item := range t {
			c := p.build(item, "", depth+1, n)
			n.children = append(n.children, c)
			n.childEstSum += c.est
			n.childInfoSum += c.info
			n.childCount++
		}
		n.est = estFromSum(n.childCount, n.childEstSum)
		n.info = n.childInfoSum
	case string:
		n.kind = 's'
		if raw, ok := quoteStringFast(t); ok {
			n.raw = raw
		} else if raw, err := marshalNoHTML(t); err == nil {
			n.raw = raw
		} else {
			b, _ := json.Marshal(t)
			n.raw = string(b)
		}
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

// estFromSum is the closed form of the old O(children) estOf: for an
// internal node with count children and contribution sum Σ,
//   est = 2                      (count == 0)
//   est = 2 + (count-1) + Σ      (count ≥ 1)  == 1 + count + Σ
// Values are identical to the old scan; this just makes them O(1).
func estFromSum(count, sum int) int {
	if count == 0 {
		return 2
	}
	return 1 + count + sum
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
		if saved, infoRemoved := p.objectFoldSaved(n); saved >= 1 {
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
		if saved, infoRemoved := p.runFoldSaved(n, r[0], r[1]); saved >= 1 {
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

// objectFoldSaved computes, allocation-free, what objectFoldCommit would
// achieve right now: keep high-value-key members, fold everything else into
// a single `"...": "N fields omitted"` member. Keeping the ids the model
// needs for follow-up tool calls is why this beats "drop the whole object".
// (Query-relevant members are protected one level up: an object the query
// references is excluded from candidacy entirely.)
func (p *jsonPruner) objectFoldSaved(n *jnode) (saved int, infoRemoved float64) {
	removedEst, removedInfo, removedCount := 0, 0.0, 0
	for _, c := range n.children {
		if _, hv := p.cfg.HighValueKeys[c.key]; !hv {
			removedEst += c.keyEst + c.est
			removedInfo += c.info
			removedCount++
		}
	}
	if removedCount == 0 {
		return 0, 0
	}
	markerKey := `"..."`
	markerRaw := fmt.Sprintf(`"%d fields omitted"`, removedCount)
	newEst := estFromSum(n.childCount-removedCount+1,
		n.childEstSum-removedEst+utf8.RuneCountInString(markerKey)+1+utf8.RuneCountInString(markerRaw))
	saved = n.est - newEst
	if saved < 1 {
		return 0, 0
	}
	return saved, removedInfo
}

// objectFoldCommit applies the fold objectFoldSaved described. The children
// slice is compacted in place (kept members forward, marker appended), so no
// new slice is allocated; aggregate sums are maintained incrementally.
func (p *jsonPruner) objectFoldCommit(n *jnode) bool {
	removedEst, removedInfo, removedCount := 0, 0.0, 0
	w := 0
	var removed []*jnode
	for _, c := range n.children {
		if _, hv := p.cfg.HighValueKeys[c.key]; hv {
			n.children[w] = c
			w++
			continue
		}
		removedEst += c.keyEst + c.est
		removedInfo += c.info
		removedCount++
		removed = append(removed, c)
	}
	if removedCount == 0 {
		return false
	}
	markerKey := `"..."`
	markerRaw := fmt.Sprintf(`"%d fields omitted"`, removedCount)
	marker := p.newNode()
	marker.parent = n
	marker.key = "..."
	marker.keyJSON = markerKey
	marker.keyEst = utf8.RuneCountInString(markerKey) + 1
	marker.kind = 's'
	marker.raw = markerRaw
	marker.est = utf8.RuneCountInString(markerRaw)
	marker.depth = n.depth + 1
	n.children[w] = marker
	n.children = n.children[:w+1]
	for _, r := range removed {
		p.detach(r)
	}
	n.childCount = w + 1
	n.childEstSum += (marker.keyEst + marker.est) - removedEst
	n.childInfoSum -= removedInfo
	n.est = estFromSum(n.childCount, n.childEstSum)
	n.info = n.childInfoSum
	return true
}

// runFoldSaved computes, allocation-free except for the keyword candidate
// list (only when keywords are set), what runFoldCommit would achieve right
// now: replace children[i:j] with a single {"_omitted": N} marker, keeping
// protected head/tail untouched and query-relevant items visible.
func (p *jsonPruner) runFoldSaved(n *jnode, i, j int) (saved int, infoRemoved float64) {
	if i < 2 || j > len(n.children)-2 || j-i < 2 {
		return 0, 0
	}
	omit, unfolded, markers := 0, 0, 0
	itemEst, itemInfo := 0, 0.0
	markerEstSum := 0
	type kwCand struct {
		c *jnode
		s int
	}
	var cands []kwCand
	for r := i; r < j; r++ {
		c := n.children[r]
		if c.kind == 'm' {
			omit += c.omit
			markers++
			markerEstSum += c.est
			continue
		}
		omit++
		unfolded++
		itemEst += c.est
		itemInfo += c.info
		if len(p.keywords) > 0 && p.cfg.KeywordSample > 0 {
			if s := p.kwScore(c); s > 0 {
				cands = append(cands, kwCand{c, s})
			}
		}
	}
	if unfolded == 0 && markers < 2 {
		// Nothing to remove (marker-merge only pays when ≥2 markers collapse
		// into one; a single marker re-folded into itself saves nothing).
		return 0, 0
	}
	keptCount, keptEst, keptInfo := 0, 0, 0.0
	if len(p.keywords) > 0 && p.cfg.KeywordSample > 0 && len(cands) > 0 {
		// Rank by hit count, not first-come: a generic question word (e.g.
		// "product") gives EVERY item score 1, while the specific value the
		// query names ("Zephyr-002") scores higher — top-K must pick it.
		sort.Slice(cands, func(a, b int) bool { return cands[a].s > cands[b].s })
		for k := 0; k < len(cands) && k < p.cfg.KeywordSample; k++ {
			keptCount++
			keptEst += cands[k].c.est
			keptInfo += cands[k].c.info
		}
	}
	omit -= keptCount
	markerRaw := fmt.Sprintf(`{"_omitted": %d}`, omit)
	removedCount := (j - i) - keptCount
	newEst := estFromSum(n.childCount-removedCount,
		n.childEstSum-markerEstSum-itemEst+keptEst+utf8.RuneCountInString(markerRaw))
	saved = n.est - newEst
	if saved < 1 || removedCount == 0 {
		return 0, 0
	}
	return saved, itemInfo - keptInfo
}

// runFoldCommit applies the fold runFoldSaved described, compacting the
// children slice in place: [0,i) untouched, kept items forward, marker, then
// the old tail copied over the consumed region. Aggregate sums are
// maintained incrementally; the old planRunFold's full-slice rebuild and
// O(children) est rescan are gone.
func (p *jsonPruner) runFoldCommit(n *jnode, i, j int) bool {
	if i < 2 || j > len(n.children)-2 || j-i < 2 {
		return false
	}
	keepSet := map[*jnode]bool{}
	if len(p.keywords) > 0 && p.cfg.KeywordSample > 0 {
		type kwCand struct {
			c *jnode
			s int
		}
		var cands []kwCand
		for r := i; r < j; r++ {
			c := n.children[r]
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
	omit, removedEst, removedInfo, removedCount := 0, 0, 0.0, 0
	var removed []*jnode
	for r := i; r < j; r++ {
		c := n.children[r]
		if keepSet[c] {
			continue
		}
		if c.kind == 'm' {
			omit += c.omit
		} else {
			omit++
		}
		removedEst += c.est
		removedInfo += c.info
		removedCount++
		removed = append(removed, c)
	}
	if removedCount == 0 {
		return false
	}
	markerRaw := fmt.Sprintf(`{"_omitted": %d}`, omit)
	marker := p.newNode()
	marker.parent = n
	marker.kind = 'm'
	marker.raw = markerRaw
	marker.omit = omit
	marker.est = utf8.RuneCountInString(markerRaw)
	marker.depth = n.depth + 1
	// Defensive re-check (O(1)): the caller verified stats at the same
	// version, but never let a fold grow the node even if state drifted.
	newEst := estFromSum(n.childCount-removedCount, n.childEstSum-removedEst+marker.est)
	if n.est-newEst < 1 {
		return false
	}
	// in-place compact: [i, w) = kept items, then marker, then old tail
	w := i
	for r := i; r < j; r++ {
		if keepSet[n.children[r]] {
			n.children[w] = n.children[r]
			w++
		}
	}
	n.children[w] = marker
	w++
	copy(n.children[w:], n.children[j:])
	n.children = n.children[:w+len(n.children)-j]
	for _, r := range removed {
		p.detach(r)
	}
	n.childCount = len(n.children)
	n.childEstSum += marker.est - removedEst
	n.childInfoSum -= removedInfo
	n.est = estFromSum(n.childCount, n.childEstSum)
	n.info = n.childInfoSum
	return true
}

// foldNode commits the fold carried by the heap entry and updates ancestors
// incrementally: each ancestor's aggregate sums change by exactly the folded
// node's est/info delta, so the walk is O(depth) with O(1) per level —
// the old code re-scanned every ancestor's full children list per fold,
// which is what made large payloads superlinear.
func (p *jsonPruner) foldNode(e foldEntry) {
	n := e.n
	isArray := false
	switch n.kind {
	case 'o':
		isArray = false
	case 'a':
		isArray = true
	default:
		return
	}
	oldEst, oldInfo := n.est, n.info
	var committed bool
	if isArray {
		committed = p.runFoldCommit(n, e.i, e.j)
	} else {
		committed = p.objectFoldCommit(n)
	}
	if !committed {
		return
	}
	p.folds++
	// An array that just folded may now offer finer runs (halves of what
	// remains) — re-seed its own candidates before walking up.
	if isArray {
		n.version++
		p.pushCandidates(n)
	}
	// Propagate the folded node's delta up the ancestor chain: each
	// ancestor's child contribution changes by (new − old) of n.
	dEst, dInfo := n.est-oldEst, n.info-oldInfo
	for a := n.parent; a != nil; a = a.parent {
		a.childEstSum += dEst
		a.childInfoSum += dInfo
		a.est = estFromSum(a.childCount, a.childEstSum)
		a.info = a.childInfoSum
		a.version++
		p.pushCandidates(a)
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

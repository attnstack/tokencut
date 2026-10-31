// keywords.go — cheap query-keyword extraction. The pruner uses keywords to
// protect query-relevant items from folding, which is the difference between
// "compressed" and "compressed the part the model actually needed".
//
// Why keywords carry WEIGHTS: a quoted span in the question ("Zephyr-002")
// is an explicit reference and must outrank incidental words from the
// question template ("product", "records") that also appear in the payload.
// Without weights, a generic word gives every item score 1 and the specific
// value survives only by tie-break luck.
package compress

import (
	"regexp"
	"strings"
)

var (
	quotedRe = regexp.MustCompile(`"([^"\n]{2,64})"`)
	identRe  = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_.\-]{2,30}`)
)

// Keyword weights: quoted spans are explicit references (3), bare
// identifiers are weak signals (1).
const (
	quotedKeywordWeight = 3
	identKeywordWeight  = 1
)

type Keyword struct {
	Text   string
	Weight int
}

var stopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "are": {}, "but": {}, "not": {}, "you": {},
	"all": {}, "any": {}, "can": {}, "her": {}, "was": {}, "one": {}, "our": {},
	"out": {}, "day": {}, "get": {}, "has": {}, "him": {}, "his": {}, "how": {},
	"man": {}, "new": {}, "now": {}, "old": {}, "see": {}, "two": {}, "way": {},
	"who": {}, "did": {}, "its": {}, "let": {}, "put": {}, "say": {}, "she": {},
	"too": {}, "use": {}, "what": {}, "when": {}, "where": {}, "which": {},
	"this": {}, "that": {}, "with": {}, "from": {}, "into": {}, "only": {},
	"based": {}, "below": {}, "above": {}, "answer": {}, "value": {},
	"question": {}, "context": {}, "response": {}, "message": {}, "following": {},
}

// KeywordsOf extracts up to max weighted lowercase query keywords: quoted
// spans first (weight 3), then identifiers (weight 1). Pure numbers and
// stopwords are dropped; duplicates merge keeping the max weight.
func KeywordsOf(text string, max int) []Keyword {
	if max <= 0 {
		return nil
	}
	weightOf := map[string]int{}
	var order []string
	add := func(s string, w int) {
		s = strings.ToLower(strings.Trim(s, " \t.,:;!?()[]{}"))
		if len(s) < 3 {
			return
		}
		if _, stop := stopWords[s]; stop {
			return
		}
		if _, dup := weightOf[s]; dup {
			if w > weightOf[s] {
				weightOf[s] = w
			}
			return
		}
		weightOf[s] = w
		order = append(order, s)
	}
	for _, m := range quotedRe.FindAllStringSubmatch(text, 8) {
		add(m[1], quotedKeywordWeight)
	}
	for _, m := range identRe.FindAllString(text, 64) {
		add(m, identKeywordWeight)
	}
	out := make([]Keyword, 0, max)
	for _, s := range order {
		out = append(out, Keyword{Text: s, Weight: weightOf[s]})
		if len(out) >= max {
			break
		}
	}
	return out
}

// SimpleKeywords wraps plain strings as weight-1 keywords (tests, callers
// that already know what matters).
func SimpleKeywords(ss ...string) []Keyword {
	out := make([]Keyword, 0, len(ss))
	for _, s := range ss {
		out = append(out, Keyword{Text: strings.ToLower(s), Weight: 1})
	}
	return out
}

func lowercaseKeywords(in []Keyword) []Keyword {
	out := make([]Keyword, len(in))
	for i, k := range in {
		out[i] = Keyword{Text: strings.ToLower(k.Text), Weight: k.Weight}
	}
	return out
}

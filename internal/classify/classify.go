// Package classify sniffs message content into JSON/log/code/html/table/text
// so the compression pipeline can route it. Misclassification is cheap by
// design: low-confidence results fall back to lossless-only handling
// upstream (see budget.Controller), so a wrong guess never corrupts data —
// it only misses a compression opportunity.
package classify

import (
	"encoding/json"
	"regexp"
	"strings"
)

type Kind string

const (
	JSON  Kind = "json"
	Log   Kind = "log"
	Code  Kind = "code"
	HTML  Kind = "html"
	Table Kind = "table"
	Text  Kind = "text"
)

type Result struct {
	Kind       Kind
	Confidence float64
}

var (
	tsPatterns = []*regexp.Regexp{
		regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}`),
		regexp.MustCompile(`^\[\d{2}:\d{2}:\d{2}\]`),
		regexp.MustCompile(`^\d{2}/\d{2}/\d{2,4} \d{2}:\d{2}:\d{2}`),
		regexp.MustCompile(`^[A-Z][a-z]{2} +\d{1,2} \d{2}:\d{2}:\d{2}`),
		regexp.MustCompile(`^\d{2}:\d{2}:\d{2}\.\d{3}`),
	}
	fenceRe = regexp.MustCompile("(?m)^```")
	defRe   = regexp.MustCompile(`(?m)^\s*(def |class |import |from \w+ import|func |package |public |private |#include|using )`)
	tagRe   = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9]*)[\s>/]`)
)

// Sniff returns the content type and a confidence in [0,1]. Confidence below
// 0.7 means "handle conservatively" in the pipeline.
func Sniff(s string) Result {
	t := strings.TrimSpace(s)
	if t == "" {
		return Result{Text, 0.5}
	}
	// Fast path: full-string JSON validity. Only object/array roots are worth
	// compressing structurally; a bare JSON string adds nothing.
	if json.Valid([]byte(t)) && (t[0] == '{' || t[0] == '[') {
		return Result{JSON, 0.95}
	}
	// Sniff a window, not the whole payload: patterns are front/back-loaded.
	window := s
	if len(window) > 10*1024 {
		window = s[:8*1024] + "\n" + s[len(s)-2*1024:]
	}
	lower := strings.ToLower(window)

	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") {
		return Result{HTML, 0.85}
	}
	lines := strings.Split(window, "\n")
	n := len(lines)
	tagCount := len(tagRe.FindAllString(window, -1))
	if tagCount > 3 && n > 0 && tagCount > n/2 {
		// Tag-dense but no doctype: probably HTML, low confidence → conservative.
		return Result{HTML, 0.6}
	}
	if fenceRe.MatchString(window) {
		return Result{Code, 0.85}
	}
	if n >= 10 {
		tsLines := 0
		tokenCounts := map[int]int{}
		for _, ln := range lines {
			tl := strings.TrimLeft(ln, " \t")
			for _, re := range tsPatterns {
				if re.MatchString(tl) {
					tsLines++
					break
				}
			}
			tokenCounts[len(strings.Fields(ln))]++
		}
		if float64(tsLines)/float64(n) >= 0.3 {
			maxShare := 0
			for _, c := range tokenCounts {
				if c > maxShare {
					maxShare = c
				}
			}
			if float64(maxShare)/float64(n) >= 0.6 {
				return Result{Log, 0.8}
			}
			return Result{Log, 0.55}
		}
	}
	if n >= 3 {
		for _, d := range []string{"|", "\t", ","} {
			if cols, ok := consistentColumns(lines, d); ok && cols >= 3 {
				return Result{Table, 0.8}
			}
		}
	}
	if len(defRe.FindAllString(window, -1)) >= 2 {
		return Result{Code, 0.7}
	}
	return Result{Text, 0.5}
}

// consistentColumns reports whether most lines share the same column count
// under the given delimiter.
func consistentColumns(lines []string, delim string) (int, bool) {
	counts := map[int]int{}
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		c := strings.Count(ln, delim) + 1
		if c > 64 {
			return 0, false
		}
		counts[c]++
	}
	best, bestN := 0, 0
	for c, k := range counts {
		if k > bestN || (k == bestN && c > best) {
			best, bestN = c, k
		}
	}
	if best >= 3 && bestN >= len(lines)*2/3 {
		return best, true
	}
	return 0, false
}

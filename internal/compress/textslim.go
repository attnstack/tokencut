// textslim.go — conservative code slimmer: drops FULL-LINE comments and
// collapses blank runs. Inline comments, docstrings and string literals are
// never touched, which keeps the transformation safe enough for v1 (a
// tree-sitter based AST slimmer is the v2 plan — see README "roadmap").
package compress

import (
	"strings"

	"tokencut/internal/tokenest"
)

func SlimCode(text string) (string, bool) {
	if text == "" {
		return "", false
	}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	out := make([]string, 0, len(lines))
	dropped := 0
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			out = append(out, "")
			continue
		}
		if isCommentLine(t) {
			dropped++
			continue
		}
		out = append(out, strings.TrimRight(ln, " \t"))
	}
	// Collapse blank runs >1 to a single blank line.
	collapsed := make([]string, 0, len(out))
	blanks := 0
	for _, ln := range out {
		if ln == "" {
			blanks++
			if blanks > 1 {
				continue
			}
		} else {
			blanks = 0
		}
		collapsed = append(collapsed, ln)
	}
	// Trim leading/trailing blanks.
	for len(collapsed) > 0 && collapsed[0] == "" {
		collapsed = collapsed[1:]
	}
	for len(collapsed) > 0 && collapsed[len(collapsed)-1] == "" {
		collapsed = collapsed[:len(collapsed)-1]
	}
	if len(collapsed) == 0 || dropped == 0 {
		return text, false
	}
	result := strings.Join(collapsed, "\n")
	if 20*tokenest.Estimate(result) > 19*tokenest.Estimate(text) {
		// <~5% saved: not worth mutating the message.
		return text, false
	}
	return result, true
}

func isCommentLine(t string) bool {
	if strings.HasPrefix(t, "#!") {
		return false // shebang is load-bearing
	}
	for _, p := range []string{"#", "//", "/*", "*", "<!--", "--"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

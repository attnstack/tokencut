// diff.go — intra-request version diff. When a multi-turn agent re-reads a
// file after editing it, the resent history contains both versions; sending
// the second one as a minimal change set removes the redundancy WITHOUT
// cross-request state. Why intra-request: the OpenAI API has no conversation
// id, so "the previous version" is only well-defined inside the request
// body itself (the client resends full history every turn).
package compress

import (
	"fmt"
	"strings"

	"tokencut/internal/tokenest"
)

const (
	diffMaxLines    = 4000 // per side; beyond this, skip (LCS memory guard)
	diffMaxMiddle   = 600  // max non-common middle after prefix/suffix trim
	diffMinSim      = 0.55 // below this the two versions are different content
	diffMaxOutRatio = 0.6  // if the diff is >60% of the full text, send the full text
)

type diffOp struct {
	kind byte // ' ', '-', '+'
	a, b int  // 1-based line numbers
	text string
}

// TryDiff returns a unified-style diff of cur relative to prev, or ok=false
// when the diff is not clearly better than the full text.
func TryDiff(prev, cur string) (string, bool) {
	if prev == cur {
		return "", false
	}
	a := splitLines(prev)
	b := splitLines(cur)
	if len(a) > diffMaxLines || len(b) > diffMaxLines {
		return "", false
	}
	// Trim common prefix/suffix first: "same head, edited tail" is the
	// dominant real shape, and it shrinks the DP to the changed region.
	pi := 0
	for pi < len(a) && pi < len(b) && a[pi] == b[pi] {
		pi++
	}
	si := 0
	for si < len(a)-pi && si < len(b)-pi && a[len(a)-1-si] == b[len(b)-1-si] {
		si++
	}
	am := a[pi : len(a)-si]
	bm := b[pi : len(b)-si]
	if len(am) > diffMaxMiddle || len(bm) > diffMaxMiddle {
		return "", false
	}
	// Similarity over the FULL texts (common prefix/suffix + LCS of the
	// middle). A single-line edit inside a 200-line file is ~99% similar —
	// measuring only the trimmed middle would wrongly reject it.
	common := pi + si + lcsLen(am, bm)
	if float64(2*common) < diffMinSim*float64(len(a)+len(b)) {
		return "", false
	}
	ops := alignOps(am, bm, pi)
	out := renderOps(ops)
	if float64(tokenest.Estimate(out)) >= diffMaxOutRatio*float64(tokenest.Estimate(cur)) {
		return "", false
	}
	return out, true
}

func splitLines(s string) []string {
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

func lcsLen(a, b []string) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	prev := make([]int32, len(b)+1)
	cur := make([]int32, len(b)+1)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				cur[j] = prev[j+1] + 1
			} else if prev[j] >= cur[j+1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j+1]
			}
		}
		prev, cur = cur, prev
	}
	return int(prev[0])
}

// alignOps walks the LCS alignment and returns ordered ops with absolute
// 1-based line numbers (offset by the trimmed common prefix).
func alignOps(am, bm []string, pi int) []diffOp {
	dp := make([][]int32, len(am)+1)
	for i := range dp {
		dp[i] = make([]int32, len(bm)+1)
	}
	for i := len(am) - 1; i >= 0; i-- {
		for j := len(bm) - 1; j >= 0; j-- {
			if am[i] == bm[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < len(am) && j < len(bm) {
		switch {
		case am[i] == bm[j]:
			ops = append(ops, diffOp{' ', pi + i + 1, pi + j + 1, am[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{'-', pi + i + 1, pi + j + 1, am[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', pi + i + 1, pi + j + 1, bm[j]})
			j++
		}
	}
	for ; i < len(am); i++ {
		ops = append(ops, diffOp{'-', pi + i + 1, pi + j + 1, am[i]})
	}
	for ; j < len(bm); j++ {
		ops = append(ops, diffOp{'+', pi + i + 1, pi + j + 1, bm[j]})
	}
	return ops
}

// renderOps groups changes into hunks with 2 context lines, merging change
// runs that are close together.
func renderOps(ops []diffOp) string {
	const ctx = 2
	var sb strings.Builder
	sb.WriteString("[tokencut-diff] changes relative to an earlier version of this content (the full earlier version is elsewhere in this conversation):\n")
	n := len(ops)
	k := 0
	for k < n {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		start := k
		end := k
		for end+1 < n && ops[end+1].kind != ' ' {
			end++
		}
		// Merge the next change run if it's within the context window.
		for {
			nxt := end + 1
			limit := nxt + 2*ctx
			hasChange := false
			for x := nxt; x < n && x <= limit; x++ {
				if ops[x].kind != ' ' {
					hasChange = true
					break
				}
			}
			if !hasChange {
				break
			}
			end = nxt
			for end+1 < n && ops[end+1].kind != ' ' {
				end++
			}
		}
		lo := start - ctx
		if lo < 0 {
			lo = 0
		}
		hi := end + ctx
		if hi >= n {
			hi = n - 1
		}
		aStart, bStart, aCount, bCount := 0, 0, 0, 0
		first := true
		for x := lo; x <= hi; x++ {
			switch ops[x].kind {
			case ' ':
				aCount++
				bCount++
			case '-':
				aCount++
			case '+':
				bCount++
			}
			if first {
				aStart, bStart = ops[x].a, ops[x].b
				first = false
			}
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", aStart, aCount, bStart, bCount)
		for x := lo; x <= hi; x++ {
			fmt.Fprintf(&sb, "%c%s\n", ops[x].kind, ops[x].text)
		}
		k = hi + 1
	}
	return sb.String()
}

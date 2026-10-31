package compress

import (
	"fmt"
	"strings"
	"testing"

	"tokencut/internal/tokenest"
)

func lines(prefix string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%s %03d\n", prefix, i)
	}
	return b.String()
}

func TestDiffSingleLineEdit(t *testing.T) {
	prev := lines("line", 200)
	cur := strings.Replace(prev, "line 100\n", "CHANGED LINE\n", 1)
	out, ok := TryDiff(prev, cur)
	if !ok {
		t.Fatalf("expected diff for single-line edit")
	}
	if !strings.Contains(out, "-line 100") || !strings.Contains(out, "+CHANGED LINE") {
		t.Errorf("diff missing the edit:\n%s", out)
	}
	if tokenest.Estimate(out) >= tokenest.Estimate(cur) {
		t.Errorf("diff not smaller than full text")
	}
}

func TestDiffTailAppend(t *testing.T) {
	prev := lines("line", 100)
	cur := prev + lines("appended", 20)
	out, ok := TryDiff(prev, cur)
	if !ok {
		t.Fatalf("expected diff for append")
	}
	if !strings.Contains(out, "+appended") {
		t.Errorf("append missing: %s", out)
	}
}

func TestDiffRejectsDifferentContent(t *testing.T) {
	a := lines("alpha", 100)
	b := lines("omega", 100)
	if _, ok := TryDiff(a, b); ok {
		t.Errorf("unrelated contents must not diff")
	}
}

func TestDiffRejectsIdentical(t *testing.T) {
	a := lines("same", 50)
	if _, ok := TryDiff(a, a); ok {
		t.Errorf("identical contents must not diff")
	}
}

func TestDiffTooLarge(t *testing.T) {
	a := lines("line", 5000)
	b := strings.Replace(a, "line 2500", "x", 1)
	if _, ok := TryDiff(a, b); ok {
		t.Errorf("oversized inputs must be rejected")
	}
}

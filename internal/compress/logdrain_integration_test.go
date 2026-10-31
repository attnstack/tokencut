package compress

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// TestDrainFaultSurvivesWithSamples is the regression test for the T2
// benchmark failure: the injected fault's host:port must survive mining as
// the top-weighted value in the compressed output, recoverable by a reader
// (model or reference solver) without the original log.
func TestDrainFaultSurvivesWithSamples(t *testing.T) {
	host, port := "10.0.29.80", "6379"
	var b strings.Builder
	for i := 0; i < 4000; i++ {
		ts := fmt.Sprintf("2026-10-31 09:%02d:%02d.%03d", (i/60)%60, i%60, i%1000)
		switch {
		case i%13 == 0:
			fmt.Fprintf(&b, "%s INFO healthcheck ok\n", ts)
		case i > 3000 && i < 3440: // 440 injected fault lines
			fmt.Fprintf(&b, "%s ERROR Failed to connect to %s:%s (timeout after 2000ms)\n", ts, host, port)
		case i%40 == 0:
			fmt.Fprintf(&b, "%s WARN slow upstream 10.9.%d.%d:%d responded in 900ms\n", ts, i%9, i%7, 6000+i%999)
		default:
			fmt.Fprintf(&b, "%s INFO serving request req_id=%d path=/api/v1/items\n", ts, i)
		}
	}
	res := DrainCompress(b.String(), DefaultDrainConfig())
	if !res.OK {
		t.Fatalf("drain failed: %s", res.Reason)
	}
	if !strings.Contains(res.Output, "Failed to connect to <*>") {
		t.Fatalf("fault template missing:\n%.400s", res.Output)
	}
	if !strings.Contains(res.Output, host+":"+port) {
		t.Fatalf("host:port sample missing from output:\n%s", res.Output)
	}

	// Reference-solver extraction (mirrors mockupstream.solveLogFault).
	connectFail := regexp.MustCompile(`Failed to connect to ([^\s:]+):(\d+)`)
	hostPortRe := regexp.MustCompile(`(\d{1,3}(?:\.\d{1,3}){3}):(\d{2,5})`)
	tplCount := regexp.MustCompile(`^x(\d+) `)
	counts := map[string]int{}
	weight := 1
	for _, line := range strings.Split(res.Output, "\n") {
		if m := tplCount.FindStringSubmatch(line); m != nil {
			fmt.Sscanf(m[1], "%d", &weight)
		}
		for _, mm := range connectFail.FindAllStringSubmatch(line, -1) {
			counts[mm[1]+":"+mm[2]] += weight
		}
		if strings.HasPrefix(strings.TrimSpace(line), "vars:") {
			for _, mm := range hostPortRe.FindAllStringSubmatch(line, -1) {
				counts[mm[1]+":"+mm[2]] += weight
			}
		}
	}
	best, bestN := "", 0
	for k, n := range counts {
		if n > bestN {
			best, bestN = k, n
		}
	}
	if best != host+":"+port {
		t.Fatalf("solver picked %q (%d), want %s:%s; counts=%v", best, bestN, host, port, counts)
	}
}

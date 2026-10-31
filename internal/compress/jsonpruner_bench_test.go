package compress

import (
	"fmt"
	"strings"
	"testing"
)

// Micro-benchmarks for the numbers in README's benchmark table. These measure
// the CPU cost of the compression itself on synthetic payloads — the proxy's
// end-to-end added latency is measured separately in proxy_test.go
// (BenchmarkProxyRoundTrip).

func benchPayload(items int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < items; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id": %d, "name": "item-%d", "status": "active", "payload": "%s", "meta": {"created": "2026-10-%02d", "score": %d}}`,
			i, i, strings.Repeat("x", 60), i%28+1, i*7%100)
	}
	b.WriteByte(']')
	return b.String()
}

func BenchmarkPruneJSON(b *testing.B) {
	for _, size := range []int{10, 100, 1000} {
		payload := benchPayload(size)
		b.Run(fmt.Sprintf("%ditems", size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for i := 0; i < b.N; i++ {
				if _, _, err := PruneJSON(payload, len(payload)/8, nil, DefaultJSONPruneConfig()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkCanonicalJSON(b *testing.B) {
	payload := benchPayload(100)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		if _, err := CanonicalJSON(payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDrain(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&sb, "2026-10-31 10:%02d:%02d INFO GET /api/users/%d 200 %dms\n", (i/60)%60, i%60, i, i%900)
	}
	log := sb.String()
	b.ReportAllocs()
	b.SetBytes(int64(len(log)))
	for i := 0; i < b.N; i++ {
		if r := DrainCompress(log, DefaultDrainConfig()); !r.OK {
			b.Fatal("drain failed")
		}
	}
}

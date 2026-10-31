// dashboard.go — embedded single-page observability panel (no external
// assets, no build step). Reads /api/stats and renders cumulative savings,
// per-strategy breakdown and the latest requests.
package dashboard

import (
	_ "embed"
	"net/http"

	"tokencut/internal/stats"
)

//go:embed static/index.html
var indexHTML []byte

// Handler serves the dashboard page.
func Handler(rec *stats.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	}
}

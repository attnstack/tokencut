// cached.go — cache value type for the JSON pruner result cache.
package compress

// CachedPrune is what the budget controller stores per canonical-JSON
// fingerprint: the compressed output and its token cost.
type CachedPrune struct {
	Output      string
	FinalTokens int
}

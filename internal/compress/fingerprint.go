// fingerprint.go — content fingerprints and canonicalization.
// Same content must always compress to the same output (multi-turn
// consistency), so the cache key is a fingerprint over a canonical form:
// JSON is re-serialized with sorted keys and compact separators; free text
// is whitespace-collapsed. Collisions at 64 bits are ~2^-64 per pair —
// irrelevant at LLM-request scale, and FNV-1a keeps us dependency-free
// (swapping in xxhash is a one-line change if throughput ever demands it).
package compress

import (
	"encoding/json"
	"hash/fnv"
	"io"
	"strings"

	"tokencut/internal/classify"
)

func Fingerprint(content string, kind classify.Kind) uint64 {
	h := fnv.New64a()
	_, _ = io.WriteString(h, normalizeForFingerprint(content, kind))
	return h.Sum64()
}

func normalizeForFingerprint(content string, kind classify.Kind) string {
	if kind == classify.JSON {
		if canon, err := CanonicalJSON(content); err == nil {
			return canon
		}
	}
	return collapseWhitespace(content)
}

// marshalNoHTML encodes v without HTML escaping. Why it matters: Go's
// json.Marshal turns & < > into \u0026 \u003c \u003e, which silently GROWS
// payloads and changes bytes we promised not to change.
func marshalNoHTML(v any) (string, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(sb.String(), "\n"), nil
}

// CanonicalJSON re-serializes JSON with sorted keys, no insignificant
// whitespace and no HTML escaping. This is the "lossless" stage: zero
// information change, often 20–40% fewer characters on pretty-printed
// payloads.
func CanonicalJSON(s string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber() // preserve numeric literals bit-for-bit (no float64 rounding)
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	// Reject trailing garbage after the first JSON value.
	if _, err := dec.Token(); err != io.EOF {
		return "", err
	}
	return marshalNoHTML(v)
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

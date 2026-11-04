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

// quoteStringFast produces the JSON encoding of s when s is plain printable
// ASCII (no quote, backslash, DEL or control bytes): the encoding is then
// exactly the string wrapped in quotes, which lets the tree builder skip
// json.Encoder entirely for the overwhelmingly common alphanumeric case.
// Returns ok=false for anything exotic; callers fall back to marshalNoHTML,
// whose output this must match byte-for-byte.
func quoteStringFast(s string) (string, bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7E || c == '"' || c == '\\' {
			return "", false
		}
	}
	return `"` + s + `"`, true
}

// CanonicalJSON re-serializes JSON with sorted keys, no insignificant
// whitespace and no HTML escaping. This is the "lossless" stage: zero
// information change, often 20–40% fewer characters on pretty-printed
// payloads. CanonicalJSONValue additionally returns the decoded value so the
// JSON pruner can skip its own re-decode of the canonical form.
func CanonicalJSON(s string) (string, error) {
	out, _, err := CanonicalJSONValue(s)
	return out, err
}

func CanonicalJSONValue(s string) (string, any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber() // preserve numeric literals bit-for-bit (no float64 rounding)
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", nil, err
	}
	// Reject trailing garbage after the first JSON value.
	if _, err := dec.Token(); err != io.EOF {
		return "", nil, err
	}
	out, err := marshalNoHTML(v)
	if err != nil {
		return "", nil, err
	}
	return out, v, nil
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

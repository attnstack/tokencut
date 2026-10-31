// mock.go — an offline OpenAI-compatible stub upstream for demos, integration
// tests and the dry-run benchmark. It is a REFERENCE SOLVER, not a language
// model: it answers "what is the value of X" by looking X up in the context
// the request carried. That makes the dry-run Pareto curve honest in a
// specific, limited way: if compression drops the field the question needs,
// the solver fails exactly like a model would. It does NOT measure model
// quality — headline numbers come from real-model runs (see
// bench/benchmark-methodology.md).
package mockupstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"tokencut/internal/tokenest"
)

type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// Handler returns the mock chat-completions endpoint.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil || json.Unmarshal(body, &req) != nil {
			http.Error(w, `{"error":{"message":"mock: bad request"}}`, http.StatusBadRequest)
			return
		}
		var contexts []string
		question := ""
		for i, m := range req.Messages {
			var s string
			if json.Unmarshal(m.Content, &s) != nil {
				continue
			}
			if i == len(req.Messages)-1 && m.Role == "user" {
				question = s
			} else {
				contexts = append(contexts, s)
			}
		}
		answer := solve(question, contexts)
		promptTok, completionTok := tokenest.Estimate(question), tokenest.Estimate(answer)
		for _, c := range contexts {
			promptTok += tokenest.Estimate(c)
		}
		now := time.Now().Unix()
		if req.Stream {
			streamAnswer(w, req.Model, answer, promptTok, completionTok, now)
			return
		}
		resp := map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion",
			"created": now,
			"model":   req.Model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": answer},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     promptTok,
				"completion_tokens": completionTok,
				"total_tokens":      promptTok + completionTok,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
}

// streamAnswer emits the answer in 3 SSE chunks then [DONE], flushing each —
// same shape real OpenAI streams have, so clients and tests exercise the
// passthrough path identically.
func streamAnswer(w http.ResponseWriter, model, answer string, promptTok, completionTok int, now int64) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	chunk := func(delta string, finish any) {
		payload := map[string]any{
			"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": now, "model": model,
			"choices": []map[string]any{{
				"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": finish,
			}},
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	runes := []rune(answer)
	part := (len(runes) + 2) / 3
	for i := 0; i < 3; i++ {
		lo, hi := i*part, (i+1)*part
		if hi > len(runes) {
			hi = len(runes)
		}
		if lo > len(runes) {
			lo = len(runes)
		}
		chunk(string(runes[lo:hi]), nil)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

var (
	quotedRe    = regexp.MustCompile(`"([^"\n]{2,64})"`)
	connectFail = regexp.MustCompile(`Failed to connect to ([^\s:]+):(\d+)`)
)

// solve: quoted spans in the question drive the lookup. Two+ quotes means a
// scoped query ("value of KEY for the product SCOPE") — find the object whose
// string value equals the scope, then read KEY from that same object. One
// quote is a plain key lookup. No quotes → log-fault solver. Failure is
// explicit, never guessed.
func solve(question string, contexts []string) string {
	notFound := "not found in provided context"
	var quoted []string
	for _, m := range quotedRe.FindAllStringSubmatch(question, 8) {
		quoted = append(quoted, m[1])
	}
	switch {
	case len(quoted) >= 2:
		key, scope := quoted[0], quoted[len(quoted)-1]
		for _, ctx := range contexts {
			if v, ok := findScoped(ctx, scope, key); ok {
				return v
			}
		}
		return notFound
	case len(quoted) == 1:
		key := quoted[0]
		for _, ctx := range contexts {
			if v, ok := findInJSON(ctx, key); ok {
				return v
			}
		}
		for _, ctx := range contexts {
			if v, ok := findKeyValue(ctx, key); ok {
				return v
			}
		}
		return notFound
	default:
		if v, ok := solveLogFault(contexts); ok {
			return v
		}
		return notFound
	}
}

// decodeEmbeddedJSON decodes the first JSON value in a text that may carry
// prose prefixes ("API response:") and suffixes. json.Decoder tolerates
// trailing data, so we only need to skip the leading prose.
func decodeEmbeddedJSON(text string) (any, bool) {
	start := strings.IndexAny(text, "{[")
	if start < 0 {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(text[start:]))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// findInJSON returns the first value bound to the given key anywhere in the
// embedded JSON.
func findInJSON(text, key string) (string, bool) {
	v, ok := decodeEmbeddedJSON(text)
	if !ok {
		return "", false
	}
	return findKey(v, key)
}

// findScoped locates the innermost map containing the scope value, then reads
// key from that same map (record-scoped lookup for T4-style questions).
func findScoped(text, scope, key string) (string, bool) {
	v, ok := decodeEmbeddedJSON(text)
	if !ok {
		return "", false
	}
	var walk func(x any) (string, bool)
	walk = func(x any) (string, bool) {
		switch t := x.(type) {
		case map[string]any:
			for _, val := range t {
				if s, ok := val.(string); ok && s == scope {
					if out, ok := t[key]; ok {
						return renderScalar(out), true
					}
				}
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if out, ok := walk(t[k]); ok {
					return out, true
				}
			}
		case []any:
			for _, item := range t {
				if out, ok := walk(item); ok {
					return out, true
				}
			}
		}
		return "", false
	}
	return walk(v)
}

func findKey(v any, key string) (string, bool) {
	switch t := v.(type) {
	case map[string]any:
		if val, ok := t[key]; ok {
			return renderScalar(val), true
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic walk
		for _, k := range keys {
			if out, ok := findKey(t[k], key); ok {
				return out, true
			}
		}
	case []any:
		for _, item := range t {
			if out, ok := findKey(item, key); ok {
				return out, true
			}
		}
	}
	return "", false
}

func renderScalar(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func findKeyValue(text, key string) (string, bool) {
	esc := regexp.QuoteMeta(key)
	re := regexp.MustCompile(`"` + esc + `"\s*:\s*("([^"]*)"|[-\d.eE+]+|true|false|null)`)
	m := re.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	if m[2] != "" {
		return m[2], true
	}
	return m[1], true
}

// solveLogFault answers the T2-style incident question: the host:port with
// the most (weighted) connection failures. Weighting handles both input
// shapes: raw logs (each line counts 1) and tokencut's own drain output,
// where the fault survives as a template `x<count> ... Failed to connect to
// <*>:<*> ...` plus a `vars:` sample line carrying the concrete values —
// reading those samples back is exactly the recovery path a model would use.
func solveLogFault(contexts []string) (string, bool) {
	counts := map[string]int{}
	for _, ctx := range contexts {
		weight := 1
		for _, line := range strings.Split(ctx, "\n") {
			if m := templateCountRe.FindStringSubmatch(line); m != nil {
				fmt.Sscanf(m[1], "%d", &weight)
			}
			for _, mm := range connectFail.FindAllStringSubmatch(line, -1) {
				counts[mm[1]+":"+mm[2]] += weight
			}
			// vars: sample lines carry space-joined variable tuples; the
			// host:port appears as a substring, extract with a loose pattern.
			if strings.HasPrefix(strings.TrimSpace(line), "vars:") {
				for _, mm := range hostPortRe.FindAllStringSubmatch(line, -1) {
					counts[mm[1]+":"+mm[2]] += weight
				}
			}
		}
	}
	best, bestN := "", 0
	for k, n := range counts {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	if bestN == 0 {
		return "", false
	}
	return best, true
}

var (
	templateCountRe = regexp.MustCompile(`^x(\d+) `)
	hostPortRe      = regexp.MustCompile(`(\d{1,3}(?:\.\d{1,3}){3}):(\d{2,5})`)
)

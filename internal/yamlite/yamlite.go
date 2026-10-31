// Package yamlite parses the small YAML subset tokencut's config files use:
// nested block maps (spaces only), scalars, quoted strings, single-line flow
// maps/lists, and comments. Block lists are intentionally unsupported — use
// flow lists. Why hand-rolled: tokencut ships as a zero-dependency single
// binary; swapping in gopkg.in/yaml.v3 later is a drop-in change behind
// config.Load.
package yamlite

import (
	"fmt"
	"strconv"
	"strings"
)

// Parse parses src and returns the top-level mapping.
func Parse(src string) (map[string]any, error) {
	lines, err := scanLines(src)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return map[string]any{}, nil
	}
	p := &parser{lines: lines}
	m, err := p.parseBlock(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.lines) {
		return nil, fmt.Errorf("yamlite: unexpected content at line %d", p.lines[p.pos].num)
	}
	return m, nil
}

type rawLine struct {
	num    int
	indent int
	text   string
}

func scanLines(src string) ([]rawLine, error) {
	var out []rawLine
	for i, l := range strings.Split(src, "\n") {
		l = strings.TrimRight(l, "\r")
		l = stripComment(l)
		if strings.TrimSpace(l) == "" {
			continue
		}
		indent := 0
		for indent < len(l) && l[indent] == ' ' {
			indent++
		}
		if indent < len(l) && l[indent] == '\t' {
			return nil, fmt.Errorf("yamlite: tab indentation not supported (line %d)", i+1)
		}
		out = append(out, rawLine{num: i + 1, indent: indent, text: strings.TrimRight(l[indent:], " ")})
	}
	return out, nil
}

// stripComment removes a trailing comment: '#' not inside quotes and preceded
// by whitespace or line start (YAML's own rule, which keeps "a#b" intact).
func stripComment(l string) string {
	inS, inD := false, false
	for i := 0; i < len(l); i++ {
		switch l[i] {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case '#':
			if !inS && !inD && (i == 0 || l[i-1] == ' ' || l[i-1] == '\t') {
				return l[:i]
			}
		}
	}
	return l
}

type parser struct {
	lines []rawLine
	pos   int
}

func (p *parser) parseBlock(indent int) (map[string]any, error) {
	m := map[string]any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("yamlite: bad indentation at line %d", l.num)
		}
		key, rest, ok := splitKey(l.text)
		if !ok {
			return nil, fmt.Errorf("yamlite: expected 'key: value' at line %d", l.num)
		}
		p.pos++
		if strings.TrimSpace(rest) == "" {
			// Nested block map (or an explicit empty value when the next line
			// is not deeper).
			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				child, err := p.parseBlock(p.lines[p.pos].indent)
				if err != nil {
					return nil, err
				}
				m[key] = child
			} else {
				m[key] = nil
			}
			continue
		}
		v, err := parseScalar(rest)
		if err != nil {
			return nil, fmt.Errorf("yamlite: line %d: %w", l.num, err)
		}
		m[key] = v
	}
	return m, nil
}

// splitKey splits "key: value" at the first ':' that is outside quotes and
// followed by a space or end-of-line, so values like URLs survive.
func splitKey(s string) (key, rest string, ok bool) {
	inS, inD := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case ':':
			if !inS && !inD && (i+1 == len(s) || s[i+1] == ' ') {
				return strings.TrimSpace(unquoteIfNeeded(s[:i])), strings.TrimSpace(s[i+1:]), true
			}
		}
	}
	return "", "", false
}

func unquoteIfNeeded(s string) string {
	if len(s) > 1 && (s[0] == '"' || s[0] == '\'') {
		if u, ok := unquote(s); ok {
			return u
		}
	}
	return s
}

func parseScalar(s string) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	switch s[0] {
	case '{':
		f := &flowParser{src: s}
		v, err := f.parseMap()
		if err != nil {
			return nil, err
		}
		f.skipWS()
		if f.pos != len(f.src) {
			return nil, fmt.Errorf("trailing content after flow map: %q", s)
		}
		return v, nil
	case '[':
		f := &flowParser{src: s}
		v, err := f.parseList()
		if err != nil {
			return nil, err
		}
		f.skipWS()
		if f.pos != len(f.src) {
			return nil, fmt.Errorf("trailing content after flow list: %q", s)
		}
		return v, nil
	case '"', '\'':
		u, ok := unquote(s)
		if !ok {
			return nil, fmt.Errorf("unterminated quoted string: %q", s)
		}
		return u, nil
	}
	return parseBareScalar(s), nil
}

func parseBareScalar(s string) any {
	switch s {
	case "true", "True":
		return true
	case "false", "False":
		return false
	case "null", "~":
		return nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if fl, err := strconv.ParseFloat(s, 64); err == nil {
		return fl
	}
	return s
}

func unquote(s string) (string, bool) {
	if len(s) < 2 {
		return "", false
	}
	q := s[0]
	if s[len(s)-1] != q {
		return "", false
	}
	body := s[1 : len(s)-1]
	if q == '\'' {
		return strings.ReplaceAll(body, "''", "'"), true
	}
	var sb strings.Builder
	sb.Grow(len(body))
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			sb.WriteByte(c)
			continue
		}
		i++
		if i >= len(body) {
			return "", false
		}
		switch body[i] {
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		case 'r':
			sb.WriteByte('\r')
		case '"':
			sb.WriteByte('"')
		case '\\':
			sb.WriteByte('\\')
		case '/':
			sb.WriteByte('/')
		default:
			return "", false
		}
	}
	return sb.String(), true
}

// flowParser parses single-line flow collections: {k: v, ...} and [a, b, ...].
// Bare scalars stop at ',', '}', ']' or a ':' followed by a delimiter — the
// last rule keeps unquoted URLs like https://api.openai.com intact.
type flowParser struct {
	src string
	pos int
}

func (f *flowParser) skipWS() {
	for f.pos < len(f.src) && (f.src[f.pos] == ' ' || f.src[f.pos] == '\t') {
		f.pos++
	}
}

func (f *flowParser) parseMap() (map[string]any, error) {
	f.pos++ // consume '{'
	m := map[string]any{}
	for {
		f.skipWS()
		if f.pos >= len(f.src) {
			return nil, fmt.Errorf("unterminated flow map")
		}
		if f.src[f.pos] == '}' {
			f.pos++
			return m, nil
		}
		keyAny, err := f.parseValue()
		if err != nil {
			return nil, err
		}
		key, ok := keyAny.(string)
		if !ok {
			return nil, fmt.Errorf("flow map key must be a string, got %T", keyAny)
		}
		f.skipWS()
		if f.pos >= len(f.src) || f.src[f.pos] != ':' {
			return nil, fmt.Errorf("expected ':' after key %q", key)
		}
		f.pos++
		v, err := f.parseValue()
		if err != nil {
			return nil, err
		}
		m[key] = v
		f.skipWS()
		if f.pos < len(f.src) && f.src[f.pos] == ',' {
			f.pos++
			continue
		}
		if f.pos < len(f.src) && f.src[f.pos] == '}' {
			f.pos++
			return m, nil
		}
		return nil, fmt.Errorf("expected ',' or '}' at offset %d in flow map", f.pos)
	}
}

func (f *flowParser) parseList() ([]any, error) {
	f.pos++ // consume '['
	out := []any{}
	for {
		f.skipWS()
		if f.pos >= len(f.src) {
			return nil, fmt.Errorf("unterminated flow list")
		}
		if f.src[f.pos] == ']' {
			f.pos++
			return out, nil
		}
		v, err := f.parseValue()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		f.skipWS()
		if f.pos < len(f.src) && f.src[f.pos] == ',' {
			f.pos++
			continue
		}
		if f.pos < len(f.src) && f.src[f.pos] == ']' {
			f.pos++
			return out, nil
		}
		return nil, fmt.Errorf("expected ',' or ']' at offset %d in flow list", f.pos)
	}
}

func (f *flowParser) parseValue() (any, error) {
	f.skipWS()
	if f.pos >= len(f.src) {
		return nil, fmt.Errorf("unexpected end of flow value")
	}
	switch f.src[f.pos] {
	case '{':
		return f.parseMap()
	case '[':
		return f.parseList()
	case '"', '\'':
		start := f.pos
		q := f.src[f.pos]
		f.pos++
		for f.pos < len(f.src) {
			if q == '"' && f.src[f.pos] == '\\' {
				f.pos += 2
				continue
			}
			if f.src[f.pos] == q {
				break
			}
			f.pos++
		}
		if f.pos >= len(f.src) {
			return nil, fmt.Errorf("unterminated quote in flow value")
		}
		f.pos++
		v, ok := unquote(f.src[start:f.pos])
		if !ok {
			return nil, fmt.Errorf("bad quoted string in flow value")
		}
		return v, nil
	default:
		start := f.pos
		for f.pos < len(f.src) {
			c := f.src[f.pos]
			if c == ',' || c == '}' || c == ']' {
				break
			}
			if c == ':' && (f.pos+1 >= len(f.src) || f.src[f.pos+1] == ' ' || f.src[f.pos+1] == ',' || f.src[f.pos+1] == '}' || f.src[f.pos+1] == ']') {
				break
			}
			f.pos++
		}
		raw := strings.TrimSpace(f.src[start:f.pos])
		if raw == "" {
			return nil, fmt.Errorf("empty flow scalar at offset %d", start)
		}
		return parseBareScalar(raw), nil
	}
}

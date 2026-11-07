package server

import (
	"fmt"
	"strconv"
	"strings"
)

// parseYAMLSubset parses the small YAML subset docs/openapi.yaml is written in, so
// the tests can read the spec without a YAML dependency: block mappings and block
// sequences (items indented under their key, "- key: value" items allowed), plain,
// single-quoted and double-quoted scalars, the empty flow collections {} and [], full
// line comments and nothing else (no anchors, tags, multi-line scalars or non-empty
// flow collections; they are errors, not silently misread). Mappings become
// map[string]any, sequences []any, numbers float64, and true, false and null the
// JSON values, so the result marshals to the JSON of the document.
func parseYAMLSubset(src string) (any, error) {
	var lines []yamlLine
	for i, raw := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimLeft(raw, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(raw, "\t") || strings.HasPrefix(strings.TrimLeft(raw, " "), "\t") {
			return nil, fmt.Errorf("line %d: tab indentation", i+1)
		}
		lines = append(lines, yamlLine{no: i + 1, indent: len(raw) - len(trimmed), text: strings.TrimRight(trimmed, " ")})
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	p := &yamlParser{lines: lines}
	v, err := p.block(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.lines) {
		return nil, fmt.Errorf("line %d: unexpected indentation", p.lines[p.pos].no)
	}
	return v, nil
}

type yamlLine struct {
	no, indent int
	text       string
}

type yamlParser struct {
	lines []yamlLine
	pos   int
}

// block parses the mapping or sequence whose lines are indented by indent.
func (p *yamlParser) block(indent int) (any, error) {
	if isSeqItem(p.lines[p.pos].text) {
		return p.sequence(indent)
	}
	return p.mapping(indent)
}

func isSeqItem(text string) bool { return text == "-" || strings.HasPrefix(text, "- ") }

func (p *yamlParser) sequence(indent int) (any, error) {
	out := []any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent != indent || !isSeqItem(l.text) {
			if l.indent > indent {
				return nil, fmt.Errorf("line %d: unexpected indentation", l.no)
			}
			break
		}
		rest := strings.TrimLeft(strings.TrimPrefix(l.text, "-"), " ")
		switch {
		case rest == "":
			p.pos++
			child, err := p.child(indent)
			if err != nil {
				return nil, err
			}
			out = append(out, child)
		case isMapEntry(rest):
			// "- key: value": the entry opens a mapping indented past the dash.
			inner := indent + (len(l.text) - len(rest))
			p.lines[p.pos] = yamlLine{no: l.no, indent: inner, text: rest}
			v, err := p.mapping(inner)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		default:
			v, err := scalar(rest, l.no)
			if err != nil {
				return nil, err
			}
			p.pos++
			out = append(out, v)
		}
	}
	return out, nil
}

func (p *yamlParser) mapping(indent int) (any, error) {
	out := map[string]any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", l.no)
		}
		if isSeqItem(l.text) {
			break
		}
		key, rest, ok := splitMapEntry(l.text)
		if !ok {
			return nil, fmt.Errorf("line %d: expected \"key: value\", got %q", l.no, l.text)
		}
		k, err := scalar(key, l.no)
		if err != nil {
			return nil, err
		}
		ks := fmt.Sprint(k)
		if s, isString := k.(string); isString {
			ks = s
		} else if f, isNum := k.(float64); isNum {
			ks = strconv.FormatFloat(f, 'f', -1, 64)
		}
		if _, dup := out[ks]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", l.no, ks)
		}
		p.pos++
		if rest == "" {
			child, childErr := p.child(indent)
			if childErr != nil {
				return nil, childErr
			}
			out[ks] = child
			continue
		}
		v, scalarErr := scalar(rest, l.no)
		if scalarErr != nil {
			return nil, scalarErr
		}
		out[ks] = v
	}
	return out, nil
}

// child parses the block nested under a key or dash at parentIndent; no nested
// lines mean null.
func (p *yamlParser) child(parentIndent int) (any, error) {
	if p.pos >= len(p.lines) || p.lines[p.pos].indent <= parentIndent {
		return nil, nil
	}
	return p.block(p.lines[p.pos].indent)
}

// isMapEntry reports whether text is "key: value" or "key:".
func isMapEntry(text string) bool {
	_, _, ok := splitMapEntry(text)
	return ok
}

// splitMapEntry splits "key: value" at the first colon outside quotes that ends the
// text or is followed by a space.
func splitMapEntry(text string) (key, rest string, ok bool) {
	var quote byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				if quote == '\'' && i+1 < len(text) && text[i+1] == '\'' {
					i++
				} else {
					quote = 0
				}
			}
		case (c == '\'' || c == '"') && i == 0:
			quote = c
		case c == ':' && (i+1 == len(text) || text[i+1] == ' '):
			return strings.TrimSpace(text[:i]), strings.TrimSpace(text[i+1:]), true
		}
	}
	return "", "", false
}

func scalar(s string, no int) (any, error) {
	switch {
	case s == "{}":
		return map[string]any{}, nil
	case s == "[]":
		return []any{}, nil
	case s[0] == '"':
		v, err := strconv.Unquote(s)
		if err != nil {
			return nil, fmt.Errorf("line %d: bad double-quoted string %s", no, s)
		}
		return v, nil
	case s[0] == '\'':
		if len(s) < 2 || s[len(s)-1] != '\'' {
			return nil, fmt.Errorf("line %d: bad single-quoted string %s", no, s)
		}
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	case strings.ContainsAny(s[:1], "[{&*!|>%@`"):
		return nil, fmt.Errorf("line %d: unsupported YAML %q", no, s)
	case s == "true":
		return true, nil
	case s == "false":
		return false, nil
	case s == "null" || s == "~":
		return nil, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !strings.ContainsAny(s, "xXoOnNiI_") {
		return f, nil
	}
	if strings.Contains(s, " #") {
		return nil, fmt.Errorf("line %d: trailing comments are not supported: %q", no, s)
	}
	return s, nil
}

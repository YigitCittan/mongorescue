package web

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// jsLiteral is a minimal parser for the object literals that hold the dashboard's
// translations: nested objects whose leaves are string literals (single, double or
// template quotes, optionally joined with +), identifier or string keys, trailing
// commas and comments. It parses the file text directly (no JavaScript engine), so
// anything outside that subset fails the test loudly instead of being skipped.
type jsLiteral struct {
	src []rune
	pos int
}

func (p *jsLiteral) fail(format string, args ...any) {
	panic(fmt.Sprintf("offset %d: %s", p.pos, fmt.Sprintf(format, args...)))
}

func (p *jsLiteral) skip() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			p.pos++
		case c == '/' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '/':
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
		case c == '/' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '*':
			end := strings.Index(string(p.src[p.pos+2:]), "*/")
			if end < 0 {
				p.fail("unterminated comment")
			}
			p.pos += 2 + len([]rune(string(p.src[p.pos+2:])[:end])) + 2
		default:
			return
		}
	}
}

func (p *jsLiteral) str() {
	q := p.src[p.pos]
	p.pos++
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case '\\':
			p.pos += 2
			continue
		case q:
			p.pos++
			return
		case '\n':
			if q != '`' {
				p.fail("newline in string")
			}
		}
		p.pos++
	}
	p.fail("unterminated string")
}

func (p *jsLiteral) key() string {
	p.skip()
	if p.pos >= len(p.src) {
		p.fail("unexpected end")
	}
	start := p.pos
	if c := p.src[p.pos]; c == '"' || c == '\'' {
		p.str()
		return string(p.src[start+1 : p.pos-1])
	}
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '_' || c == '$' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			p.pos++
			continue
		}
		break
	}
	if p.pos == start {
		p.fail("expected a key, found %q", p.src[p.pos])
	}
	return string(p.src[start:p.pos])
}

// object parses {...} at the current position and adds the dotted path of every
// string leaf to out (prefixed with prefix).
func (p *jsLiteral) object(prefix string, out map[string]bool) {
	p.skip()
	if p.pos >= len(p.src) || p.src[p.pos] != '{' {
		p.fail("expected {")
	}
	p.pos++
	for {
		p.skip()
		if p.pos >= len(p.src) {
			p.fail("unterminated object")
		}
		if p.src[p.pos] == '}' {
			p.pos++
			return
		}
		k := p.key()
		p.skip()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			p.fail("expected : after key %q", k)
		}
		p.pos++
		p.skip()
		full := k
		if prefix != "" {
			full = prefix + "." + k
		}
		switch c := p.src[p.pos]; c {
		case '{':
			p.object(full, out)
		case '"', '\'', '`':
			p.str()
			for {
				p.skip()
				if p.pos < len(p.src) && p.src[p.pos] == '+' {
					p.pos++
					p.skip()
					if c := p.src[p.pos]; c != '"' && c != '\'' && c != '`' {
						p.fail("only string concatenation is supported")
					}
					p.str()
					continue
				}
				break
			}
			if out[full] {
				p.fail("duplicate key %s", full)
			}
			out[full] = true
		default:
			p.fail("unsupported value for key %q", full)
		}
		p.skip()
		switch {
		case p.pos < len(p.src) && p.src[p.pos] == ',':
			p.pos++
		case p.pos < len(p.src) && p.src[p.pos] == '}':
		default:
			p.fail("expected , or } after %q", full)
		}
	}
}

// languageKeys parses the translations table declared as `const name = {` in src
// and returns the leaf keys of each language block.
func languageKeys(src, name string) (langs map[string]map[string]bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s: %v", name, r)
		}
	}()
	loc := regexp.MustCompile(`(?m)^const ` + regexp.QuoteMeta(name) + ` = \{`).FindStringIndex(src)
	if loc == nil {
		return nil, fmt.Errorf("const %s not found", name)
	}
	p := &jsLiteral{src: []rune(src[loc[1]-1:])}
	p.pos = 1
	langs = map[string]map[string]bool{}
	for {
		p.skip()
		if p.src[p.pos] == '}' {
			return langs, nil
		}
		lang := p.key()
		p.skip()
		if p.src[p.pos] != ':' {
			p.fail("expected : after language %q", lang)
		}
		p.pos++
		keys := map[string]bool{}
		p.object("", keys)
		if langs[lang] != nil {
			p.fail("duplicate language %s", lang)
		}
		langs[lang] = keys
		p.skip()
		if p.src[p.pos] == ',' {
			p.pos++
		}
	}
}

func diffKeys(want, got map[string]bool) (missing, extra []string) {
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

// checkParity fails for every language of the table whose key set differs from en.
func checkParity(t *testing.T, file, name string, wantLangs []string) {
	t.Helper()
	langs, err := languageKeys(readStatic(t, file), name)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	en := langs["en"]
	if len(en) == 0 {
		t.Fatalf("%s: no en keys parsed", file)
	}
	for _, l := range wantLangs {
		if langs[l] == nil {
			t.Errorf("%s: language %s is missing", file, l)
		}
	}
	for l, keys := range langs {
		if l == "en" {
			continue
		}
		missing, extra := diffKeys(en, keys)
		if len(missing) > 0 {
			t.Errorf("%s: %s lacks %d key(s) of en: %s", file, l, len(missing), strings.Join(missing, ", "))
		}
		if len(extra) > 0 {
			t.Errorf("%s: %s has %d key(s) that en lacks: %s", file, l, len(extra), strings.Join(extra, ", "))
		}
	}
}

// TestTranslationKeyParity makes a key missing from (or extra in) any language of
// i18n.js a failure: a missing key silently falls back to English in the
// dashboard. The per-feature *_TRANSLATIONS overlays of the other scripts are
// deliberately partial (they only add keys for some languages) and are not checked;
// add a language's strings to i18n.js itself.
func TestTranslationKeyParity(t *testing.T) {
	checkParity(t, "i18n.js", "translations", []string{"en", "tr", "de", "es", "fr", "zh", "ja", "ru"})
}

// TestTranslationParserCatchesDrift keeps the parser honest on a small table.
func TestTranslationParserCatchesDrift(t *testing.T) {
	src := "const T = {\n en: { a: \"x\", /* c */ b: { c: 'y' + \"z\" }, \"d-e\": `q`, },\n // note\n tr: { a: \"x\", b: { f: \"w\" }, },\n};\n"
	langs, err := languageKeys(src, "T")
	if err != nil {
		t.Fatal(err)
	}
	missing, extra := diffKeys(langs["en"], langs["tr"])
	if strings.Join(missing, ",") != "b.c,d-e" || strings.Join(extra, ",") != "b.f" {
		t.Fatalf("missing %v extra %v", missing, extra)
	}
	if _, err := languageKeys("const T = {\n en: { a: 1 },\n};\n", "T"); err == nil {
		t.Fatal("unsupported value was accepted")
	}
}

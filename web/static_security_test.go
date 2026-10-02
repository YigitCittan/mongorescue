package web

import (
	"regexp"
	"strings"
	"testing"
)

// readStatic returns an embedded dashboard file with LF line endings (a Windows
// checkout may have CRLF).
func readStatic(t *testing.T, name string) string {
	t.Helper()
	b, err := StaticFS.ReadFile("static/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// TestHostPatternHasNoOverlappingRanges extracts the host pattern of the URI builder
// (forms.js) and checks that its character classes have no overlapping ranges (a
// sign of a typo such as [A-z], which also admits [\]^_`) and that it still accepts
// and refuses the same hosts. The pattern only uses syntax Go's regexp shares with
// JavaScript.
func TestHostPatternHasNoOverlappingRanges(t *testing.T) {
	src := readStatic(t, "forms.js")
	m := regexp.MustCompile(`if \(!/(\^.*\$)/\.test\(h\.host\)\)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("host pattern not found in forms.js")
	}
	pattern := m[1]
	for _, class := range regexp.MustCompile(`\[(?:\\.|[^\]\\])+\]`).FindAllString(pattern, -1) {
		var ranges [][2]rune
		body := []rune(class[1 : len(class)-1])
		for i := 0; i+2 < len(body); i++ {
			if body[i] == '\\' {
				i++
				continue
			}
			if body[i+1] == '-' && body[i+2] != '\\' {
				ranges = append(ranges, [2]rune{body[i], body[i+2]})
				i += 2
			}
		}
		for i, a := range ranges {
			if a[0] > a[1] {
				t.Errorf("class %s: inverted range %c-%c", class, a[0], a[1])
			}
			for _, b := range ranges[i+1:] {
				if a[0] <= b[1] && b[0] <= a[1] {
					t.Errorf("class %s: ranges %c-%c and %c-%c overlap", class, a[0], a[1], b[0], b[1])
				}
			}
		}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("host pattern %q: %v", pattern, err)
	}
	for host, ok := range map[string]bool{
		"db.example.com":     true,
		"mongo-1":            true,
		"10.0.0.5":           true,
		"[::1]":              true,
		"[fe80::1%25eth0]":   true,
		"[2001:DB8::Ab]":     true,
		"":                   false,
		"db example.com":     false,
		"db/example":         false,
		"[::1]x":             false,
		"[a_b]":              false,
		"[a^b]":              false,
		"host\nlevel=ERROR":  false,
		"user@db.example.co": false,
	} {
		if got := re.MatchString(host); got != ok {
			t.Errorf("host %q: match = %v; want %v", host, got, ok)
		}
	}
}

// TestMergeTranslationsSkipsPrototypeKeys checks that the translation merge of
// trust.js refuses the keys that reach Object.prototype before it copies anything.
func TestMergeTranslationsSkipsPrototypeKeys(t *testing.T) {
	src := readStatic(t, "trust.js")
	start := strings.Index(src, "function mergeTranslations(dst, src) {")
	if start < 0 {
		t.Fatal("mergeTranslations not found in trust.js")
	}
	body := src[start:]
	end := strings.Index(body, "\n}\n")
	if end < 0 {
		t.Fatal("end of mergeTranslations not found in trust.js")
	}
	body = body[:end]
	guard := strings.Index(body, `if (k === "__proto__" || k === "constructor" || k === "prototype") return;`)
	if guard < 0 {
		t.Fatal("mergeTranslations must skip __proto__, constructor and prototype")
	}
	if write := strings.Index(body, "dst[k] ="); write < 0 || write < guard {
		t.Fatal("the prototype-key guard must come before the first write to dst")
	}
}

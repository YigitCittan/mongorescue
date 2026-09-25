// Package logsafe prepares values that may come from clients (names, IDs derived from
// them, error messages quoting them) for structured logs: control characters are
// removed, so a value can never start a new log line or hide text with terminal
// escapes, and long values are truncated. Secrets are the redact package's concern;
// logsafe does not mask them.
package logsafe

import (
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLength is the number of bytes String keeps of a value.
const MaxLength = 1024

// String returns v without control characters (CR and LF included), cut to
// MaxLength bytes on a rune boundary with "…" appended when it was longer.
func String(v string) string {
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, v)
	if len(v) > MaxLength {
		cut := MaxLength
		for cut > 0 && !utf8.RuneStart(v[cut]) {
			cut--
		}
		v = v[:cut] + "…"
	}
	// Line breaks are gone already; the explicit replacements keep that obvious to
	// static analysis (they are the log-injection sanitizers it recognises).
	return strings.ReplaceAll(strings.ReplaceAll(v, "\n", ""), "\r", "")
}

// Attr returns a string attribute whose value went through String.
func Attr(key, v string) slog.Attr {
	return slog.String(key, String(v))
}

// Error returns the "error" attribute of err, its message through String. A nil
// error gives an empty value.
func Error(err error) slog.Attr {
	if err == nil {
		return slog.String("error", "")
	}
	return slog.String("error", String(err.Error()))
}

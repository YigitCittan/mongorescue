package cli

import (
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// ANSI escape sequences used for statuses.
const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

// isTerminal reports whether w is a terminal that takes escape sequences: an
// *os.File that is a console (on Windows with virtual terminal processing on) or a
// tty. Pipes, files and other writers are not.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isTerminalFile(f)
}

// colorEnabled reports whether the text output of this run is coloured: only on a
// terminal, and never with --json or --quiet, --no-color, a non-empty NO_COLOR
// (https://no-color.org) or TERM=dumb.
func (s *session) colorEnabled() bool {
	switch {
	case s.opts.json, s.opts.quiet, s.opts.noColor:
		return false
	case s.getenv("NO_COLOR") != "", s.getenv("TERM") == "dumb":
		return false
	}
	detect := isTerminal
	if s.app != nil && s.app.IsTerminal != nil {
		detect = s.app.IsTerminal
	}
	return detect(s.stdout)
}

// statusColors maps the status words of backups, restores, runs, checks and
// readiness to a colour: green for success, yellow for running or doubtful, red for
// failure. Other words stay plain.
var statusColors = map[string]string{
	"completed": ansiGreen, "ok": ansiGreen, "pass": ansiGreen, "passed": ansiGreen, "verified": ansiGreen, "healthy": ansiGreen,
	"failed": ansiRed, "fail": ansiRed, "mismatch": ansiRed, "missing": ansiRed, "error": ansiRed,
	"in_progress": ansiYellow, "running": ansiYellow, "pending": ansiYellow, "partial": ansiYellow, "warn": ansiYellow,
	"cancelled": ansiYellow, "pruned": ansiYellow, "degraded": ansiYellow,
}

// paint returns a status word in its colour when the run colours its output, and
// the word as is otherwise.
func (s *session) paint(status string) string {
	if !s.color {
		return status
	}
	if c, ok := statusColors[strings.ToLower(status)]; ok {
		return c + status + ansiReset
	}
	return status
}

// visibleWidth is the number of runes of text without its ANSI colour sequences.
func visibleWidth(text string) int {
	n := 0
	for i := 0; i < len(text); {
		if text[i] == 0x1b && i+1 < len(text) && text[i+1] == '[' {
			j := i + 2
			for j < len(text) && (text[j] < '@' || text[j] > '~') {
				j++
			}
			i = j + 1
			continue
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		i += size
		n++
	}
	return n
}

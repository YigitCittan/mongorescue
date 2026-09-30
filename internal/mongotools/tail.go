package mongotools

import (
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/yigitcittan/mongorescue/internal/redact"
)

// StderrLimit is how much of a tool's stderr the engines keep: the last 64 KiB, which
// holds the summary lines mongorestore prints at the end.
const StderrLimit = 64 << 10

const (
	// QuotedTailLines is the number of trailing stderr lines QuoteTail keeps.
	QuotedTailLines = 5
	// QuotedLineMax bounds each quoted line, so values from documents (for example
	// the key of a duplicate key error, possibly personal data) are not stored at
	// length.
	QuotedLineMax = 300
	// QuotedTailMax bounds the whole quoted tail.
	QuotedTailMax = 1024
)

// ellipsis marks text cut by TailLines and QuoteTail.
const ellipsis = "…"

// TailBuffer is an io.Writer that keeps only the last Limit bytes written to it, so
// capturing a tool's stderr never grows with the size of a dump (a flood of duplicate
// key lines, for example). It is safe for concurrent use.
type TailBuffer struct {
	// Limit is the number of bytes kept; zero means StderrLimit.
	Limit int

	mu      sync.Mutex
	buf     []byte
	dropped int64
}

// Write implements io.Writer; it never fails.
func (t *TailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	limit := t.Limit
	if limit <= 0 {
		limit = StderrLimit
	}
	if t.buf == nil {
		t.buf = make([]byte, 0, limit)
	}
	n := len(p)
	if len(p) >= limit {
		t.dropped += int64(len(t.buf) + len(p) - limit)
		t.buf = append(t.buf[:0], p[len(p)-limit:]...)
		return n, nil
	}
	if over := len(t.buf) + len(p) - limit; over > 0 {
		t.dropped += int64(over)
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// Dropped returns how many bytes were discarded to stay within the limit.
func (t *TailBuffer) Dropped() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dropped
}

// String returns the kept bytes, starting at a line boundary when earlier output was
// dropped (or at least at a character boundary when the kept bytes hold no newline).
func (t *TailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := string(t.buf)
	if t.dropped > 0 {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		} else {
			for s != "" && !utf8.RuneStart(s[0]) {
				s = s[1:]
			}
		}
	}
	return s
}

// TailLines returns the last n lines of s joined with " | ", each cut to lineMax
// bytes (on a character boundary) and marked with "…", so a quoted tool message
// cannot carry long document values (such as a duplicate key's value) into records
// and logs.
func TailLines(s string, n, lineMax int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	lines = lines[max(len(lines)-n, 0):]
	for i, l := range lines {
		if len(l) > lineMax {
			lines[i] = cutPrefix(l, lineMax) + ellipsis
		}
	}
	return strings.Join(lines, " | ")
}

// QuoteTail returns the last lines of a tool's stderr for an error message: redacted
// first (so no cut can split a connection string before it is masked), then bounded
// by QuotedTailLines, QuotedLineMax and QuotedTailMax.
func QuoteTail(stderr string) string {
	tail := TailLines(redact.Text(stderr), QuotedTailLines, QuotedLineMax)
	if len(tail) > QuotedTailMax {
		tail = ellipsis + cutSuffix(tail, QuotedTailMax)
	}
	return tail
}

// cutPrefix returns at most the first n bytes of s without splitting a character.
func cutPrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutSuffix returns at most the last n bytes of s without splitting a character.
func cutSuffix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

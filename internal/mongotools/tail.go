package mongotools

import (
	"strings"
	"sync"
)

// StderrLimit is how much of a tool's stderr the engines keep: the last 64 KiB, which
// holds the summary lines mongorestore prints at the end.
const StderrLimit = 64 << 10

// TailBuffer is an io.Writer that keeps only the last Limit bytes written to it, so
// capturing a tool's stderr never grows with the size of a dump. It is safe for
// concurrent use.
type TailBuffer struct {
	// Limit is the number of bytes kept; zero means StderrLimit.
	Limit int

	mu        sync.Mutex
	buf       []byte
	truncated bool
}

// Write implements io.Writer; it never fails.
func (t *TailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	limit := t.Limit
	if limit <= 0 {
		limit = StderrLimit
	}
	n := len(p)
	if len(p) >= limit {
		t.buf = append(t.buf[:0], p[len(p)-limit:]...)
		t.truncated = true
		return n, nil
	}
	if over := len(t.buf) + len(p) - limit; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.truncated = true
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// String returns the kept bytes, starting at a line boundary when earlier output was
// dropped.
func (t *TailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := string(t.buf)
	if t.truncated {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return s
}

// TailLines returns the last n lines of s joined with " | ", each cut to lineMax
// bytes, so a quoted tool message cannot carry long document values (such as a
// duplicate key's value) into records and logs.
func TailLines(s string, n, lineMax int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	lines = lines[max(len(lines)-n, 0):]
	for i, l := range lines {
		if len(l) > lineMax {
			lines[i] = l[:lineMax] + "..."
		}
	}
	return strings.Join(lines, " | ")
}

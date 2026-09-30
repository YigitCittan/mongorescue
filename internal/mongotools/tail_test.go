package mongotools

import (
	"io"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTailBufferKeepsTheEnd(t *testing.T) {
	tb := &TailBuffer{Limit: 16}
	_, _ = tb.Write([]byte("first line\n"))
	_, _ = tb.Write([]byte("second\nthird\n"))
	if got := tb.String(); got != "second\nthird\n" {
		t.Fatalf("String() = %q; want the complete lines that fit", got)
	}

	big := &TailBuffer{Limit: 8}
	_, _ = big.Write([]byte(strings.Repeat("x", 100) + "\nabc\n"))
	if got := big.String(); got != "abc\n" {
		t.Fatalf("String() = %q; want %q", got, "abc\n")
	}

	small := &TailBuffer{}
	_, _ = small.Write([]byte("a\nb\n"))
	if got := small.String(); got != "a\nb\n" {
		t.Fatalf("String() = %q; want everything below the limit", got)
	}
}

func TestTailLines(t *testing.T) {
	in := "one\ntwo\n" + strings.Repeat("v", 50) + "\nfour\n"
	got := TailLines(in, 3, 10)
	want := "two | vvvvvvvvvv… | four"
	if got != want {
		t.Fatalf("TailLines = %q; want %q", got, want)
	}

	// A cut never splits a multi-byte character.
	if got := TailLines("aéé", 1, 2); got != "a…" || !utf8.ValidString(got) {
		t.Fatalf("TailLines = %q; want %q", got, "a…")
	}
}

// TestTailBufferBoundsAFlood streams 50 MB of duplicate-key lines through a pipe, as
// a failing mongorestore would print them: the buffer stays at its limit, counts what
// it dropped and still holds the summary line printed last.
func TestTailBufferBoundsAFlood(t *testing.T) {
	const total = 50 << 20
	line := "continuing through error: E11000 duplicate key error collection: db.users index: email_1 dup key: { email: \"" +
		strings.Repeat("x", 200) + "\" }\n"
	chunk := []byte(strings.Repeat(line, (1<<20)/len(line)))
	summary := "2026-09-30T12:00:00.000+0000\t0 document(s) restored successfully. 123456 document(s) failed to restore.\n"

	pr, pw := io.Pipe()
	var written int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for written < total {
			n, err := pw.Write(chunk)
			written += int64(n)
			if err != nil {
				return
			}
		}
		n, _ := pw.Write([]byte(summary))
		written += int64(n)
		_ = pw.Close()
	}()

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	tb := &TailBuffer{}
	if _, err := io.Copy(tb, pr); err != nil {
		t.Fatalf("copy: %v", err)
	}
	runtime.ReadMemStats(&after)
	<-done

	if len(tb.buf) != StderrLimit || cap(tb.buf) > StderrLimit {
		t.Fatalf("buffer len=%d cap=%d; want both bounded by %d", len(tb.buf), cap(tb.buf), StderrLimit)
	}
	if got, want := tb.Dropped(), written-StderrLimit; got != want {
		t.Fatalf("Dropped() = %d; want %d", got, want)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Fatalf("capturing %d bytes allocated %d bytes; want bounded memory", written, alloc)
	}
	out := tb.String()
	if !strings.HasSuffix(out, summary) || !strings.HasPrefix(out, "continuing") {
		t.Fatalf("String() does not start at a line and end with the summary: %q...%q", out[:40], out[len(out)-40:])
	}
}

func TestTailBufferDroppedAndCharBoundary(t *testing.T) {
	tb := &TailBuffer{Limit: 4}
	_, _ = tb.Write([]byte("ab"))
	if tb.Dropped() != 0 {
		t.Fatalf("Dropped() = %d; want 0 below the limit", tb.Dropped())
	}
	_, _ = tb.Write([]byte("cééf")) // the last 4 bytes start inside the first "é"
	if got := tb.Dropped(); got != 4 {
		t.Fatalf("Dropped() = %d; want 4", got)
	}
	if got := tb.String(); got != "éf" {
		t.Fatalf("String() = %q; want %q", got, "éf")
	}
}

func TestQuoteTailRedactsAndBounds(t *testing.T) {
	key := strings.Repeat("k", 5000)
	in := "start\n" +
		"Failed: mongodb://admin:hunter2@db.example:27017/?authSource=admin " + key + "\n" +
		strings.Repeat("dup key: { email: \""+key+"\" }\n", 3) +
		"error at mongodb://u:" + strings.Repeat("p", 400) + "@h/\n"
	got := QuoteTail(in)
	if strings.Contains(got, "hunter2") || strings.Contains(got, "ppp") {
		t.Fatalf("QuoteTail leaked a credential: %q", got)
	}
	if len(got) > QuotedTailMax+len("…") {
		t.Fatalf("QuoteTail kept %d bytes; want at most %d", len(got), QuotedTailMax+len("…"))
	}
	for _, l := range strings.Split(strings.TrimPrefix(got, "…"), " | ") {
		if len(l) > QuotedLineMax+len("…") {
			t.Fatalf("quoted line of %d bytes: %q", len(l), l)
		}
	}
	if !utf8.ValidString(got) || !strings.Contains(got, "error at mongodb://") {
		t.Fatalf("QuoteTail = %q", got)
	}
}

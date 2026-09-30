package mongotools

import (
	"strings"
	"testing"
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
	want := "two | vvvvvvvvvv... | four"
	if got != want {
		t.Fatalf("TailLines = %q; want %q", got, want)
	}
}

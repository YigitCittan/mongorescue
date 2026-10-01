package runlog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readLog(t *testing.T, d *Dir, id string) string {
	t.Helper()
	r, err := d.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var buf bytes.Buffer
	if _, err := r.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestLogRedactsAndCutsLines(t *testing.T) {
	d := NewDir(t.TempDir())
	w, err := d.Create("bkp_shop_1")
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 2*ToolLineMax)
	fmt.Fprintf(w, "2026-10-01T10:00:00.000+0000\tconnecting to mongodb://admin:hunter2@db.internal:27017/?authSource=admin\n")
	fmt.Fprintf(w, "E11000 duplicate key error collection: shop.users index: email_1 dup key: { email: \"alice@example.com\" }\n")
	fmt.Fprintf(w, "%s\npartial ", long)
	fmt.Fprintf(w, "line with mongodb://u:s3cret@h:1/?x=1\n\n")
	w.Printf("phase: %s with mongodb+srv://u:p4ss@cluster.example.net", "dumping")
	_, _ = w.Write([]byte("no newline at the end"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("late\n")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close = %v", err)
	}

	log := readLog(t, d, "bkp_shop_1")
	for _, secret := range []string{"hunter2", "alice@example.com", "s3cret", "p4ss"} {
		if strings.Contains(log, secret) {
			t.Errorf("log leaks %q:\n%s", secret, log)
		}
	}
	lines := strings.Split(strings.TrimSuffix(log, "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("got %d lines, want 6 (empty lines dropped):\n%s", len(lines), log)
	}
	if !strings.Contains(lines[1], "dup key: { ****** }") {
		t.Errorf("dup key line = %q", lines[1])
	}
	if len(lines[2]) > ToolLineMax+len("…") || !strings.HasSuffix(lines[2], "…") {
		t.Errorf("long line kept %d bytes, want it cut to %d with an ellipsis", len(lines[2]), ToolLineMax)
	}
	if lines[3] != "partial line with mongodb://u:******@h:1/?x=1" {
		t.Errorf("split line = %q", lines[3])
	}
	if !strings.Contains(lines[4], "\t[mongorescue] phase: dumping with mongodb+srv://u:******@cluster.example.net") {
		t.Errorf("own line = %q", lines[4])
	}
	if lines[5] != "no newline at the end" {
		t.Errorf("final partial line = %q", lines[5])
	}
}

func TestLogIsCappedWithHeadAndTail(t *testing.T) {
	dir := t.TempDir()
	d := NewDir(dir)
	w, err := d.Create("bkp_big")
	if err != nil {
		t.Fatal(err)
	}
	// 12 MiB of numbered lines: far beyond the cap.
	const lines = 120000
	line := strings.Repeat("y", 90)
	for i := 0; i < lines; i++ {
		fmt.Fprintf(w, "%06d %s\n", i, line)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "bkp_big.log"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > MaxFileBytes {
		t.Fatalf("log is %d bytes, cap is %d", info.Size(), MaxFileBytes)
	}
	if info.Size() < HeadBytes+TailBytes/2 {
		t.Fatalf("log is %d bytes; the head and at least half the tail budget must be kept", info.Size())
	}
	if segs, _ := filepath.Glob(filepath.Join(dir, "*"+segmentInfix+"*")); len(segs) != 0 {
		t.Fatalf("tail segments left behind: %v", segs)
	}
	log := readLog(t, d, "bkp_big")
	if !strings.HasPrefix(log, "000000 ") {
		t.Fatalf("the head must be kept, log starts %q", log[:20])
	}
	if !strings.HasSuffix(log, fmt.Sprintf("%06d %s\n", lines-1, line)) {
		t.Fatal("the last line must be kept")
	}
	marker := strings.Index(log, "… ")
	if marker < 0 || !strings.Contains(log[marker:marker+64], " bytes truncated …") {
		t.Fatal("no truncation marker between head and tail")
	}
	var dropped int64
	if _, err := fmt.Sscanf(log[marker:], "… %d bytes truncated …", &dropped); err != nil || dropped <= 0 {
		t.Fatalf("marker = %q (%v)", log[marker:marker+40], err)
	}
	total := int64(lines * (len(line) + 8))
	if kept := info.Size() - int64(len(marker0(dropped))); kept+dropped != total {
		t.Fatalf("kept %d + dropped %d != written %d", kept, dropped, total)
	}
}

// marker0 returns the marker line for dropped bytes.
func marker0(dropped int64) string { return marker(dropped) }

func TestLiveReaderAndTail(t *testing.T) {
	d := NewDir(t.TempDir())
	w, err := d.Create("rst_live")
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("z", 100)
	for i := 0; i < 30000; i++ { // ~3 MiB: head full, tail segments in use
		fmt.Fprintf(w, "%05d %s\n", i, line)
	}
	r, err := w.Reader()
	if err != nil {
		t.Fatal(err)
	}
	tail, err := r.Tail(3)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("29997 %s\n29998 %s\n29999 %s\n", line, line, line)
	if string(tail) != want {
		t.Fatalf("tail = %q", tail)
	}
	var full bytes.Buffer
	if _, err = r.WriteTo(&full); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(full.String(), "00000 ") || !strings.HasSuffix(full.String(), want) {
		t.Fatal("the live log must hold the head and the newest lines")
	}
	_ = r.Close()
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Reader(); !errors.Is(err, ErrClosed) {
		t.Fatalf("reader after close = %v", err)
	}
	fin, err := d.Open("rst_live")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fin.Close() })
	all, _ := fin.Tail(MaxTailLines * 10)
	if !bytes.HasSuffix(full.Bytes(), all) || bytes.Count(all, []byte("\n")) != MaxTailLines {
		t.Fatalf("tail is capped at %d lines of the log the live reader showed; got %d", MaxTailLines, bytes.Count(all, []byte("\n")))
	}
	small, _ := d.Create("rst_small")
	small.Printf("one")
	_ = small.Close()
	sr, err := d.Open("rst_small")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sr.Close() })
	if got, _ := sr.Tail(50); !strings.HasSuffix(string(got), "[mongorescue] one\n") || bytes.Count(got, []byte("\n")) != 1 {
		t.Fatalf("tail of a short log = %q", got)
	}
	if one, _ := fin.Tail(1); string(one) != fmt.Sprintf("29999 %s\n", line) {
		t.Fatalf("tail 1 = %q", one)
	}
}

func TestRemovePruneAndInvalidIDs(t *testing.T) {
	dir := t.TempDir()
	d := NewDir(dir)
	for _, id := range []string{"bkp_old", "bkp_new", "bkp_active"} {
		w, err := d.Create(id)
		if err != nil {
			t.Fatal(err)
		}
		w.Printf("hello")
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// A segment left behind by a crash belongs to its run.
	if err := os.WriteFile(filepath.Join(dir, "bkp_old.log"+segmentInfix+"3"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"bkp_old.log", "bkp_old.log" + segmentInfix + "3", "bkp_active.log"} {
		if err := os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	n, err := d.Prune(24*time.Hour, func(id string) bool { return id == "bkp_active" })
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v; want the old log and its segment", n, err)
	}
	if _, err := d.Open("bkp_old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old log still there: %v", err)
	}
	for _, id := range []string{"bkp_new", "bkp_active"} {
		r, err := d.Open(id)
		if err != nil {
			t.Fatalf("%s pruned: %v", id, err)
		}
		_ = r.Close()
	}
	if n, _ := d.Prune(0, nil); n != 0 {
		t.Fatal("keep 0 must keep every log")
	}
	if err := d.Remove("bkp_new"); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove("bkp_new"); err != nil {
		t.Fatalf("removing a missing log = %v", err)
	}
	for _, id := range []string{"", "..", "../etc/passwd", `a\b`, ".hidden", "a/b"} {
		if _, err := d.Create(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Create(%q) = %v, want ErrInvalidID", id, err)
		}
	}
	legacy, createErr := d.Create("bkp_café_20260101_000000")
	if createErr != nil {
		t.Fatalf("a legacy ID with database characters must be accepted: %v", createErr)
	}
	// An open writer keeps its file busy on Windows: close it before TempDir cleanup.
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMissingDirectory(t *testing.T) {
	d := NewDir(filepath.Join(t.TempDir(), "missing"))
	if n, err := d.Prune(time.Hour, nil); n != 0 || err != nil {
		t.Fatalf("prune of a missing directory = %d, %v", n, err)
	}
	if _, err := d.Open("bkp_x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open = %v", err)
	}
}

// TestRemoveAfterReadersClose checks that a log read through the API (whole file,
// tail and live reader) can be deleted once its readers are closed, and that Remove
// waits briefly for a reader that closes meanwhile (an open file cannot be deleted on
// Windows).
func TestRemoveAfterReadersClose(t *testing.T) {
	d := NewDir(t.TempDir())
	w, err := d.Create("bkp_read")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30000; i++ { // head full: the live reader holds segment files too
		fmt.Fprintf(w, "%05d %s\n", i, strings.Repeat("r", 100))
	}
	live, err := w.Reader()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = live.Tail(5); err != nil {
		t.Fatal(err)
	}
	if err = live.Close(); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := d.Open("bkp_read")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.WriteTo(io.Discard); err != nil {
		t.Fatal(err)
	}
	// A download still open: Remove retries while the reader is being closed.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		time.Sleep(30 * time.Millisecond)
		_ = r.Close()
	}()
	err = d.Remove("bkp_read")
	<-closed
	if err != nil {
		t.Fatalf("remove after the reader closed = %v", err)
	}
	if _, err := d.Open("bkp_read"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("log still there after Remove: %v", err)
	}
	if segs, _ := filepath.Glob(filepath.Join(d.Path(), "*"+segmentInfix+"*")); len(segs) != 0 {
		t.Fatalf("segments left: %v", segs)
	}
}

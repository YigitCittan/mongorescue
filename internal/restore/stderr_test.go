package restore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// floodStderr returns a pipe that yields about total bytes of line, then last, as a
// tool that prints a flood of errors would.
func floodStderr(t *testing.T, total int, line, last string) io.Reader {
	t.Helper()
	pr, pw := io.Pipe()
	chunk := []byte(strings.Repeat(line, max((1<<20)/len(line), 1)))
	go func() {
		for written := 0; written < total; written += len(chunk) {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(pw, last)
		_ = pw.Close()
	}()
	t.Cleanup(func() { _ = pr.Close() })
	return pr
}

// TestRestoreBoundsAStderrFlood covers mongorestore printing 50 MB of duplicate key
// errors: the summary line at the end is still found, and the quoted output is
// redacted, with each line (holding document key values) cut short.
func TestRestoreBoundsAStderrFlood(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	key := strings.Repeat("x", 2000)
	line := "continuing through error: E11000 duplicate key error collection: db.users index: email_1 dup key: { email: \"" + key + "\" }\n"
	last := "reconnecting to mongodb://admin:hunter2@db.example:27017/ " + key + "\n" +
		"2026-09-30T12:00:00.000+0000\t0 document(s) restored successfully. 654321 document(s) failed to restore.\n"
	runner := func(_ context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		_, _ = io.Copy(io.Discard, stdin)
		return floodStderr(t, 50<<20, line, last), func() error { return nil }, nil
	}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner))
	record, err := engine.Run(context.Background(), inPlaceRequest(src.ID), src)
	if !errors.Is(err, ErrDocumentsFailed) {
		t.Fatalf("expected ErrDocumentsFailed, got %v", err)
	}
	msg := record.ErrorMessage
	if record.Status != models.RestoreStatusFailed || !strings.Contains(msg, "654321 document(s) failed to restore") {
		t.Fatalf("unexpected record: %s %q", record.Status, msg)
	}
	if strings.Contains(msg, "hunter2") {
		t.Fatalf("credential leaked: %q", msg)
	}
	if strings.Contains(msg, strings.Repeat("x", mongotools.QuotedLineMax)) || !strings.Contains(msg, "…") {
		t.Fatalf("quoted lines are not cut: %d bytes", len(msg))
	}
	if len(msg) > 4<<10 {
		t.Fatalf("error message is %d bytes; want it bounded", len(msg))
	}
}

func TestStderrTailRedactsBeforeCutting(t *testing.T) {
	// The quoted tail is cut to its last 1024 bytes right after "mongodb:/", so a
	// redaction after the cut would no longer see the scheme and miss the password.
	uri := "mongodb://admin:hunter2@db/"
	in := "start\n" + uri + "\n" + strings.Repeat("y", 248) + "\n" + strings.Repeat("y", 248) + "\n" +
		strings.Repeat("y", 249) + "\n" + strings.Repeat("y", 249) + "\n"
	if joined := len(uri) + 4*len(" | ") + 248 + 248 + 249 + 249; joined-mongotools.QuotedTailMax != len("mongodb:/") {
		t.Fatalf("test setup: cut at %d", joined-mongotools.QuotedTailMax)
	}
	if got := stderrTail(in); strings.Contains(got, "hunter2") {
		t.Fatalf("stderrTail leaked a credential: %q", got)
	}
}

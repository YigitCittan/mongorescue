package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestBackupBoundsAStderrFlood covers mongodump printing 50 MB to stderr before
// failing: the error quotes only a short, redacted tail that ends with the last line.
func TestBackupBoundsAStderrFlood(t *testing.T) {
	long := strings.Repeat("z", 2000)
	line := "2026-09-30T12:00:00.000+0000\twriting db.events " + long + "\n"
	last := "Failed: connection to mongodb://admin:hunter2@db.example:27017/ lost " + long + "\n"

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close() })
	chunk := []byte(strings.Repeat(line, (1<<20)/len(line)))
	go func() {
		for written := 0; written < 50<<20; written += len(chunk) {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(pw, last)
		_ = pw.Close()
	}()

	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader(nil)), pr, func() error { return errors.New("exit status 1") }, nil
	}
	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(runner))
	record, err := engine.Run(context.Background(), models.BackupOptions{Database: "db"})
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, msg := range []string{err.Error(), record.ErrorMessage} {
		if strings.Contains(msg, "hunter2") {
			t.Fatalf("credential leaked: %q", msg)
		}
		if !strings.Contains(msg, "Failed: connection to mongodb://") {
			t.Fatalf("last stderr line missing: %q", msg)
		}
		if len(msg) > 4<<10 || strings.Contains(msg, strings.Repeat("z", mongotools.QuotedLineMax)) {
			t.Fatalf("stderr quote is not bounded: %d bytes", len(msg))
		}
	}
}

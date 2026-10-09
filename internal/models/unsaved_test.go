package models

import (
	"strings"
	"testing"
	"time"
)

func TestFailUnsaved(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	done := &BackupRecord{Status: StatusCompleted, StorageKey: "shop/1.archive.gz",
		Copies: []BackupCopy{{TargetID: "s3", Status: CopyPending}}}
	done.FailUnsaved("database or disk is full", now)
	if done.Status != StatusFailed || !done.ArchiveCleanupPending || done.CompletedAt == nil || !done.CompletedAt.Equal(now) {
		t.Fatalf("completed backup: %+v", done)
	}
	if done.ErrorMessage != ErrRecordNotSaved+" (database or disk is full)" {
		t.Fatalf("error %q", done.ErrorMessage)
	}
	if done.Copies[0].Status != CopyFailed {
		t.Fatalf("pending copy not abandoned: %+v", done.Copies[0])
	}
	// Settling twice keeps one prefix.
	done.FailUnsaved("again", now)
	if strings.Count(done.ErrorMessage, ErrRecordNotSaved) != 1 {
		t.Fatalf("error %q", done.ErrorMessage)
	}

	cancelled := &BackupRecord{Status: StatusCancelled, ErrorMessage: "cancelled by alice"}
	cancelled.FailUnsaved("disk full", now)
	if cancelled.Status != StatusCancelled || cancelled.ArchiveCleanupPending ||
		!strings.HasSuffix(cancelled.ErrorMessage, "the run itself ended with: cancelled by alice") {
		t.Fatalf("cancelled backup: %+v", cancelled)
	}
}

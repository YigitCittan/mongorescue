package runs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGoRunsDetachedAndGuardsKeys(t *testing.T) {
	m := NewManager(nil)
	release := make(chan struct{})
	finished := make(chan struct{})

	if err := m.Go(BackupKey("c", "shop"), func(ctx context.Context) {
		defer close(finished)
		select {
		case <-release:
		case <-ctx.Done():
			t.Error("operation cancelled unexpectedly")
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !m.Running(BackupKey("c", "shop")) {
		t.Fatal("key should be held while running")
	}
	if err := m.Go(BackupKey("c", "shop"), func(context.Context) {}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second run with same key: got %v, want ErrBusy", err)
	}
	if _, err := m.Acquire(BackupKey("c", "shop")); !errors.Is(err, ErrBusy) {
		t.Fatalf("Acquire of held key: got %v, want ErrBusy", err)
	}
	other, err := m.Acquire(BackupKey("c", "other"))
	if err != nil {
		t.Fatalf("different key must not conflict: %v", err)
	}
	if got := m.Active(); len(got) != 2 || got[0] != BackupKey("c", "other") || got[1] != BackupKey("c", "shop") {
		t.Fatalf("Active() = %v; want both keys sorted", got)
	}
	other()
	other() // idempotent

	close(release)
	<-finished
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Running(BackupKey("c", "shop")) {
		t.Fatal("key must be released after the operation returns")
	}
}

func TestShutdownCancelsAndWaits(t *testing.T) {
	m := NewManager(nil)
	var cleanedUp bool
	started := make(chan struct{})
	if err := m.Go("", func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // simulate reaping a subprocess
		cleanedUp = true
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !cleanedUp {
		t.Fatal("Shutdown returned before the operation finished")
	}
	if err := m.Go("", func(context.Context) {}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Go after Shutdown: got %v", err)
	}
	if _, err := m.Acquire("k"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Acquire after Shutdown: got %v", err)
	}
}

func TestShutdownIsBounded(t *testing.T) {
	m := NewManager(nil)
	stuck := make(chan struct{})
	defer close(stuck)
	_ = m.Go("", func(context.Context) { <-stuck }) // ignores cancellation

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := m.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}

func TestPanicReleasesKey(t *testing.T) {
	m := NewManager(nil)
	_ = m.Go("k", func(context.Context) { panic("boom") })
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Running("k") {
		t.Fatal("key leaked after panic")
	}
}

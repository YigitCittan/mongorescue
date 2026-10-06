package runs

import (
	"context"
	"testing"
	"time"
)

// TestIdleCountsUnkeyedOperations proves Idle sees operations started without a key
// (such as a run of a multi-database job) as well as held keys.
func TestIdleCountsUnkeyedOperations(t *testing.T) {
	m := NewManager(nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	if !m.Idle() {
		t.Fatal("a new Manager is not idle")
	}
	release := make(chan struct{})
	if err := m.Go("", func(context.Context) { <-release }); err != nil {
		t.Fatal(err)
	}
	if m.Idle() || len(m.Active()) != 0 {
		t.Fatalf("Idle() = %v with an unkeyed operation running (Active %v)", m.Idle(), m.Active())
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for !m.Idle() {
		if time.Now().After(deadline) {
			t.Fatal("Idle() stayed false after the operation returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
	done, err := m.Acquire(BackupKey("c", "db"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Idle() {
		t.Fatal("Idle() = true while a key is held")
	}
	done()
	if !m.Idle() {
		t.Fatal("Idle() = false after the key was released")
	}
}

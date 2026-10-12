package runs

import (
	"context"
	"testing"
	"time"
)

func TestCriticalSteps(t *testing.T) {
	m := NewManager(nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	if m.CriticalCount() != 0 || !m.WaitCritical(context.Background()) {
		t.Fatal("a new manager has critical steps")
	}
	end := m.Critical()
	other := m.Critical()
	if m.CriticalCount() != 2 {
		t.Fatalf("count = %d", m.CriticalCount())
	}
	other()
	other() // idempotent
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if m.WaitCritical(ctx) {
		t.Fatal("WaitCritical returned while a step runs")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		end()
	}()
	if !m.WaitCritical(context.Background()) || m.CriticalCount() != 0 {
		t.Fatalf("count = %d after the step ended", m.CriticalCount())
	}
}

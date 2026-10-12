package runs

import (
	"context"
	"time"
)

// criticalPoll is how often WaitCritical looks at the critical steps.
const criticalPoll = 100 * time.Millisecond

// Critical marks the start of a step that a shutdown must not interrupt, such as
// the swap of an in-place point-in-time restore, which would leave a database
// half replaced: the step runs under a context of its own (not the Manager's,
// which Shutdown cancels), and the application waits for it (WaitCritical)
// before it kills the remaining tool processes. end must be called exactly once
// when the step is over.
func (m *Manager) Critical() (end func()) {
	m.mu.Lock()
	m.critical++
	m.mu.Unlock()
	done := false
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !done {
			done = true
			m.critical--
		}
	}
}

// CriticalCount returns how many critical steps run.
func (m *Manager) CriticalCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.critical
}

// WaitCritical waits until no critical step runs, or ctx ends; it reports
// whether none runs.
func (m *Manager) WaitCritical(ctx context.Context) bool {
	ticker := time.NewTicker(criticalPoll)
	defer ticker.Stop()
	for {
		if m.CriticalCount() == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return m.CriticalCount() == 0
		case <-ticker.C:
		}
	}
}

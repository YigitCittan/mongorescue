// Package runs executes long-running operations (manual backups, job runs, restores)
// in the background under the application's lifecycle instead of an HTTP request's.
//
// A Manager owns every goroutine it starts: Shutdown cancels their shared context and
// waits for them (bounded by the caller's context), so no operation is ever orphaned.
// Operations may carry a key (e.g. "backup:<db>"); at most one operation per key runs
// at a time, which lets the API answer 409 Conflict instead of starting a duplicate.
package runs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
)

// Sentinel errors returned by Manager.
var (
	// ErrBusy indicates that an operation with the same key is already running.
	ErrBusy = errors.New("runs: operation already running")

	// ErrShuttingDown indicates that the Manager no longer accepts new operations:
	// after Shutdown, or while Refuse is in effect.
	ErrShuttingDown = errors.New("runs: shutting down")
)

// BackupKey is the concurrency key of a backup of database on connection connectionID:
// at most one backup (manual, job-triggered or scheduled) of a database runs at a time.
func BackupKey(connectionID, database string) string {
	return "backup:" + connectionID + "/" + database
}

// PITRBaseKey is the concurrency key of a PITR base backup (a dump of the whole
// instance) of connection connectionID: at most one runs at a time, while the
// per-database backups of the connection keep running.
func PITRBaseKey(connectionID string) string {
	return "pitr-base:" + connectionID
}

// PITRDeletionKey is the deletion lock (see LockDeletion) of a PITR stream's
// retention: base backups and oplog chunks of streamID are deleted under it.
func PITRDeletionKey(streamID string) string {
	return "pitr:" + streamID
}

// RestoreKey is the concurrency key of a restore into database on connection connectionID.
func RestoreKey(connectionID, database string) string {
	return "restore:" + connectionID + "/" + database
}

// Manager runs keyed background operations under a shared lifecycle context.
// It is safe for concurrent use. The zero value is not usable; call NewManager.
type Manager struct {
	ctx    context.Context
	cancel context.CancelFunc
	logger *slog.Logger

	mu     sync.Mutex
	active map[string]struct{}
	// slots holds the counted slots of AcquireSlot (per-connection backup limits).
	slots  map[string]*slotQueue
	closed bool
	// refusing makes Acquire and Go return ErrShuttingDown until Accept.
	refusing bool
	// started counts the operations started with Go that have not returned, keyed
	// or not.
	started int
	wg      sync.WaitGroup
}

// NewManager returns a Manager whose operations run under a context detached from any
// request; it is cancelled only by Shutdown.
func NewManager(logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{ctx: ctx, cancel: cancel, logger: logger, active: make(map[string]struct{})}
}

// Acquire reserves key for an operation that runs outside the Manager (e.g. a cron
// run with its own lifecycle). The returned release function must be called exactly
// once when the operation ends. An empty key is never contended.
func (m *Manager) Acquire(key string) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.refusing {
		return nil, ErrShuttingDown
	}
	if err := m.reserveLocked(key); err != nil {
		return nil, err
	}
	return m.releaseFunc(key), nil
}

// Go starts fn in a goroutine owned by the Manager. fn receives the lifecycle context,
// which is cancelled by Shutdown. Go returns ErrBusy (wrapped with the key) if key is
// already in use and ErrShuttingDown after Shutdown has begun.
func (m *Manager) Go(key string, fn func(ctx context.Context)) error {
	m.mu.Lock()
	if m.closed || m.refusing {
		m.mu.Unlock()
		return ErrShuttingDown
	}
	if err := m.reserveLocked(key); err != nil {
		m.mu.Unlock()
		return err
	}
	m.wg.Add(1)
	m.started++
	m.mu.Unlock()

	release := m.releaseFunc(key)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			m.started--
			m.mu.Unlock()
		}()
		defer release()
		defer func() {
			if r := recover(); r != nil {
				m.logger.Error("background operation panicked", slog.String("key", key), slog.Any("panic", r))
			}
		}()
		fn(m.ctx)
	}()
	return nil
}

// Refuse makes Acquire and Go return ErrShuttingDown, like after Shutdown, until
// Accept; running operations continue. A host that quits once the running
// operations end (the desktop app's soft quit) refuses new ones meanwhile.
func (m *Manager) Refuse() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refusing = true
}

// Accept undoes Refuse; it does not reopen a Manager after Shutdown.
func (m *Manager) Accept() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refusing = false
}

// Running reports whether an operation holds key.
func (m *Manager) Running(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.active[key]
	return ok
}

// Idle reports whether no operation runs: no key is held and every operation started
// with Go, with or without a key (such as a run of a multi-database job), returned.
func (m *Manager) Idle() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active) == 0 && m.started == 0
}

// Active returns the keys of the operations currently running, sorted. Operations
// started without a key are not listed.
func (m *Manager) Active() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(maps.Keys(m.active))
}

// Shutdown stops accepting operations, cancels the running ones and waits for them to
// return. It returns ctx.Err() if ctx ends first; the operations then keep unwinding
// (their subprocesses have already been signalled) but are no longer waited for.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("runs: waiting for background operations: %w", ctx.Err())
	}
}

// reserveLocked marks key active. Caller must hold m.mu.
func (m *Manager) reserveLocked(key string) error {
	if key == "" {
		return nil
	}
	if _, busy := m.active[key]; busy {
		return fmt.Errorf("%w: %s", ErrBusy, key)
	}
	m.active[key] = struct{}{}
	return nil
}

// releaseFunc returns an idempotent function freeing key.
func (m *Manager) releaseFunc(key string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if key == "" {
				return
			}
			m.mu.Lock()
			delete(m.active, key)
			m.mu.Unlock()
		})
	}
}

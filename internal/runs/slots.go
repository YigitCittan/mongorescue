package runs

import (
	"context"
	"sync"
)

// ConnectionKey is the slot key of the backups that read from connection
// connectionID (see Manager.AcquireSlot and models.Connection.MaxConcurrentBackups).
func ConnectionKey(connectionID string) string {
	return "connection:" + connectionID
}

// slotQueue is the state of one slot key: how many holders it has, the newest
// limit and the callers waiting, first come first served.
type slotQueue struct {
	held    int
	limit   int
	waiters []chan struct{}
}

// AcquireSlot takes one of limit slots of key, waiting (in arrival order) while
// all are held: runs queue instead of failing. waiting, when not nil, is called
// once before the caller starts to wait. The returned release frees the slot (or
// hands it to the next waiter) and is idempotent. A limit of 0 or less, or an
// empty key, never waits.
//
// AcquireSlot returns ctx's error when ctx ends first and ErrShuttingDown when the
// Manager shuts down meanwhile. The newest limit applies: raising it lets waiters
// in at once, lowering it lets the next waiter in only once enough holders have
// left.
func (m *Manager) AcquireSlot(ctx context.Context, key string, limit int, waiting func()) (release func(), err error) {
	if limit <= 0 || key == "" {
		return func() {}, nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrShuttingDown
	}
	if m.slots == nil {
		m.slots = make(map[string]*slotQueue)
	}
	q := m.slots[key]
	if q == nil {
		q = &slotQueue{}
		m.slots[key] = q
	}
	q.limit = limit
	m.admitLocked(q)
	if q.held < q.limit && len(q.waiters) == 0 {
		q.held++
		m.mu.Unlock()
		return m.slotRelease(key, q), nil
	}
	ch := make(chan struct{})
	q.waiters = append(q.waiters, ch)
	m.mu.Unlock()

	if waiting != nil {
		waiting()
	}
	select {
	case <-ch:
		return m.slotRelease(key, q), nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-m.ctx.Done():
		err = ErrShuttingDown
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, w := range q.waiters {
		if w == ch {
			q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
			m.dropIdleLocked(key, q)
			return nil, err
		}
	}
	// The slot was handed over while ctx ended: pass it on.
	m.freeLocked(key, q)
	return nil, err
}

// slotRelease returns the idempotent release of one slot of q.
func (m *Manager) slotRelease(key string, q *slotQueue) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.freeLocked(key, q)
		})
	}
}

// freeLocked gives up one held slot of q: the next waiter gets it when the limit
// allows. Caller must hold m.mu.
func (m *Manager) freeLocked(key string, q *slotQueue) {
	q.held--
	m.admitLocked(q)
	m.dropIdleLocked(key, q)
}

// admitLocked hands free slots of q to its waiters, first come first served.
// Caller must hold m.mu.
func (m *Manager) admitLocked(q *slotQueue) {
	for q.held < q.limit && len(q.waiters) > 0 {
		next := q.waiters[0]
		q.waiters = q.waiters[1:]
		q.held++
		close(next)
	}
}

// dropIdleLocked forgets q once nobody holds or waits for it. Caller must hold m.mu.
func (m *Manager) dropIdleLocked(key string, q *slotQueue) {
	if q.held <= 0 && len(q.waiters) == 0 && m.slots[key] == q {
		delete(m.slots, key)
	}
}

// SlotWaiters returns how many callers wait for a slot of key.
func (m *Manager) SlotWaiters(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if q := m.slots[key]; q != nil {
		return len(q.waiters)
	}
	return 0
}

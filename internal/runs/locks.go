package runs

import (
	"context"
	"slices"
	"sync"
)

// KeyLocks is a set of blocking mutexes keyed by string. A key's mutex exists only
// while it is held or awaited. It is safe for concurrent use; the zero value is not
// usable, call NewKeyLocks.
type KeyLocks struct {
	mu   sync.Mutex
	held map[string]*keyLock
}

// keyLock is the mutex of one key: a token in a one-slot channel, and the number of
// holders and waiters (the entry is dropped when it reaches zero).
type keyLock struct {
	ch   chan struct{}
	refs int
}

// NewKeyLocks returns an empty KeyLocks.
func NewKeyLocks() *KeyLocks {
	return &KeyLocks{held: map[string]*keyLock{}}
}

// Lock acquires the mutexes of keys in the order given (duplicates once), waiting as
// long as needed or until ctx ends. It returns the function that releases them all,
// or ctx's error with nothing held. Callers that take several keys must always take
// them in the same order.
func (k *KeyLocks) Lock(ctx context.Context, keys ...string) (unlock func(), err error) {
	var taken []string
	release := func() {
		for i := len(taken) - 1; i >= 0; i-- {
			k.unlock(taken[i])
		}
	}
	for _, key := range keys {
		if slices.Contains(taken, key) {
			continue
		}
		if err = k.lock(ctx, key); err != nil {
			release()
			return nil, err
		}
		taken = append(taken, key)
	}
	var once sync.Once
	return func() { once.Do(release) }, nil
}

func (k *KeyLocks) lock(ctx context.Context, key string) error {
	k.mu.Lock()
	l := k.held[key]
	if l == nil {
		l = &keyLock{ch: make(chan struct{}, 1)}
		k.held[key] = l
	}
	l.refs++
	k.mu.Unlock()
	select {
	case l.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		k.mu.Lock()
		k.drop(key, l)
		k.mu.Unlock()
		return ctx.Err()
	}
}

func (k *KeyLocks) unlock(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	l := k.held[key]
	<-l.ch
	k.drop(key, l)
}

// drop forgets one holder or waiter of l; k.mu must be held.
func (k *KeyLocks) drop(key string, l *keyLock) {
	if l.refs--; l.refs == 0 {
		delete(k.held, key)
	}
}

// deletionLocks serialises every deletion of backups in the process (see
// LockDeletion). It is process-wide on purpose: the single and bulk deletes of the
// operations service and the scheduler's retention must exclude each other however
// they are wired.
var deletionLocks = NewKeyLocks()

// DeletionKey is the deletion lock of the backups of job jobID, or, for backups
// without a job, of database on connection connectionID. The protections of a backup
// (its job's last good and last verified backup) are decided per job, so deletions
// of one job's backups never interleave.
func DeletionKey(jobID, connectionID, database string) string {
	if jobID != "" {
		return "delete:job:" + jobID
	}
	return "delete:db:" + connectionID + "/" + database
}

// ArchiveKey is the lock of the archive key on storage target targetID, held around
// "check its references, delete the record, delete the object if unreferenced".
func ArchiveKey(targetID, key string) string {
	return "archive:" + targetID + "\x00" + key
}

// LockDeletion acquires the process-wide deletion locks of keys (DeletionKey first,
// then ArchiveKey; never the other way round) and returns their release function.
func LockDeletion(ctx context.Context, keys ...string) (unlock func(), err error) {
	return deletionLocks.Lock(ctx, keys...)
}

// DeletionLockUsers reports how many callers hold or wait for the deletion lock key
// (for tests and diagnostics).
func DeletionLockUsers(key string) int {
	deletionLocks.mu.Lock()
	defer deletionLocks.mu.Unlock()
	if l := deletionLocks.held[key]; l != nil {
		return l.refs
	}
	return 0
}

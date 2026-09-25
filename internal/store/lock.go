package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// LockFileName is the advisory lock file taken in the data directory.
const LockFileName = "mongorescue.lock"

// ErrDataDirLocked is returned by LockDataDir when another process (or another open
// in this process) already holds the data directory lock.
var ErrDataDirLocked = errors.New("store: data directory is locked by another MongoRescue instance")

// DirLock is an exclusive advisory lock on a data directory. It guarantees that a
// single MongoRescue instance runs schedules and writes metadata for the directory.
type DirLock struct {
	file *os.File
	once sync.Once
	err  error
}

// LockDataDir creates dir if needed and takes an exclusive, non-blocking advisory lock
// on dir/mongorescue.lock (flock on Unix, LockFileEx on Windows). It fails fast with
// an error wrapping ErrDataDirLocked when the lock is held elsewhere. The lock is
// released by Release or when the process exits.
func LockDataDir(dir string) (*DirLock, error) {
	if err := os.MkdirAll(dir, dbDirMode); err != nil {
		return nil, fmt.Errorf("store: create data directory: %w", err)
	}
	path := filepath.Join(dir, LockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, dbFileMode)
	if err != nil {
		return nil, fmt.Errorf("store: open lock file: %w", err)
	}
	locked, err := tryLock(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("store: lock %s: %w", path, err)
	}
	if !locked {
		_ = f.Close()
		return nil, fmt.Errorf("%w: another MongoRescue instance is using %s", ErrDataDirLocked, dir)
	}
	return &DirLock{file: f}, nil
}

// Release unlocks and closes the lock file. It is safe to call more than once.
func (l *DirLock) Release() error {
	l.once.Do(func() {
		unlockErr := unlock(l.file)
		closeErr := l.file.Close()
		if err := errors.Join(unlockErr, closeErr); err != nil {
			l.err = fmt.Errorf("store: release data directory lock: %w", err)
		}
	})
	return l.err
}

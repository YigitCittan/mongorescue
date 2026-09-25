//go:build windows

package store

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes a non-blocking exclusive LockFileEx lock on the first byte of f.
// Windows locks belong to the handle, so a second open conflicts even in one process.
func tryLock(f *os.File) (bool, error) {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return false, nil
	default:
		return false, err
	}
}

// unlock releases the lock taken by tryLock.
func unlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

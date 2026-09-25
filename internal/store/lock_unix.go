//go:build !windows

package store

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes a non-blocking exclusive flock on f. flock locks belong to the open
// file description, so a second open of the same file conflicts even in one process.
func tryLock(f *os.File) (bool, error) {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) //nolint:gosec // G115: file descriptors fit in int.
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}

// unlock releases the flock on f.
func unlock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN) //nolint:gosec // G115: file descriptors fit in int.
}

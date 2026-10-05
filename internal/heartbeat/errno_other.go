//go:build !windows

package heartbeat

import (
	"errors"
	"syscall"
)

// isConnRefused reports whether err is a refused connection.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// isConnTimeout reports whether err is a connection attempt that timed out at the
// socket level.
func isConnTimeout(err error) bool {
	return errors.Is(err, syscall.ETIMEDOUT)
}

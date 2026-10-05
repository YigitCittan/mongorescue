//go:build windows

package heartbeat

import (
	"errors"
	"syscall"
)

// Winsock error codes: the Windows dialer reports them as syscall.Errno values that
// differ from the POSIX constants of the syscall package.
const (
	wsaETIMEDOUT    syscall.Errno = 10060
	wsaECONNREFUSED syscall.Errno = 10061
)

// isConnRefused reports whether err is a refused connection.
func isConnRefused(err error) bool {
	return errors.Is(err, wsaECONNREFUSED) || errors.Is(err, syscall.ECONNREFUSED)
}

// isConnTimeout reports whether err is a connection attempt that timed out at the
// socket level.
func isConnTimeout(err error) bool {
	return errors.Is(err, wsaETIMEDOUT) || errors.Is(err, syscall.ETIMEDOUT)
}

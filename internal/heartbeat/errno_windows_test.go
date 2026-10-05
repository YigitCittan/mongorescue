//go:build windows

package heartbeat

import "syscall"

// refusedErrno is the error the dialer reports for a refused connection.
func refusedErrno() syscall.Errno { return wsaECONNREFUSED }

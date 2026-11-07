//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTerminalFile reports whether f is a terminal that takes escape sequences.
func isTerminalFile(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA) //nolint:gosec // G115: file descriptors fit an int.
	return err == nil
}

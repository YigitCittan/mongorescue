//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package cli

import "os"

// isTerminalFile reports false: there is no terminal detection on this platform, so
// the output is never coloured.
func isTerminalFile(*os.File) bool { return false }

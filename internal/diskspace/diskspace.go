// Package diskspace reports the free disk space of a local filesystem. Restore
// preflights use it when the target MongoDB server runs on the same host and does not
// report its own filesystem usage.
package diskspace

import "errors"

// ErrUnsupported is returned on platforms without a free-space probe.
var ErrUnsupported = errors.New("diskspace: not supported on this platform")

// Free returns the bytes available to unprivileged users on the filesystem holding
// path.
func Free(path string) (uint64, error) {
	return free(path)
}

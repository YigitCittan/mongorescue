//go:build !linux && !darwin && !freebsd && !windows

package diskspace

// free reports ErrUnsupported.
func free(string) (uint64, error) {
	return 0, ErrUnsupported
}

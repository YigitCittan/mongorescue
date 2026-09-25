//go:build !windows

package secretbox

import "os"

// syncDir flushes the directory entry changes of dir (a rename into it) to disk.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: dir is the configured data directory.
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

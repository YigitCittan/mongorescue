//go:build linux || darwin || freebsd

package diskspace

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// free implements Free with statfs(2).
func free(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("diskspace: statfs: %w", err)
	}
	if st.Bsize <= 0 {
		return 0, fmt.Errorf("diskspace: statfs reported block size %d", st.Bsize)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

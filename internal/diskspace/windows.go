//go:build windows

package diskspace

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// free implements Free with GetDiskFreeSpaceExW.
func free(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("diskspace: %w", err)
	}
	var avail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &totalFree); err != nil {
		return 0, fmt.Errorf("diskspace: GetDiskFreeSpaceEx: %w", err)
	}
	return avail, nil
}

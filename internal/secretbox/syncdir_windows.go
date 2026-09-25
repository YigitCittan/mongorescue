//go:build windows

package secretbox

// syncDir is a no-op on Windows, where directories cannot be opened for syncing and
// NTFS journals the rename metadata itself.
func syncDir(string) error { return nil }

//go:build !windows

package desktop

import (
	"fmt"
	"os"
	"os/exec"
)

// openLocked opens path for reading. Unlike on Windows, other processes of the user
// can still change the file; the desktop app only launches installers on Windows.
func openLocked(path string) (*os.File, error) {
	return os.Open(path)
}

// LaunchInstaller starts the program at path without a shell and returns once it has
// started; the program outlives the app. The desktop app only launches installers
// on Windows.
func LaunchInstaller(path string) error {
	cmd := exec.Command(path) //nolint:noctx // the installer must outlive the app, so it is not bound to a context.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", path, err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release %s: %w", path, err)
	}
	return nil
}

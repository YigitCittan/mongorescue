//go:build !windows

package desktop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// openLocked opens path for reading. Unlike on Windows, other processes of the user
// can still change the file; the desktop app only launches installers on Windows.
func openLocked(path string) (*os.File, error) {
	return os.Open(path)
}

// LaunchInstaller starts the program at path with args without a shell and returns
// once it has started; the program outlives the app. The desktop app only launches
// installers on Windows.
func LaunchInstaller(path string, args []string) error {
	return StartDetached(path, args)
}

// StartDetached starts exe with args in its own directory without a shell and
// returns once it runs; the process outlives the app. The desktop app only
// restarts itself this way on Windows.
func StartDetached(exe string, args []string) error {
	cmd := exec.Command(exe, args...) //nolint:noctx // the program must outlive the app, so it is not bound to a context.
	cmd.Dir = filepath.Dir(exe)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", exe, err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release %s: %w", exe, err)
	}
	return nil
}

// WaitForProcessExit waits up to timeout for the process pid to exit and reports
// whether it has, polling every 100 ms.
func WaitForProcessExit(pid int, timeout time.Duration) bool {
	if pid <= 0 || pid == os.Getpid() {
		return true
	}
	deadline := time.Now().Add(timeout)
	for {
		p, err := os.FindProcess(pid)
		if err != nil || p.Signal(syscall.Signal(0)) != nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// LegacyUninstaller returns "": per-machine copies in Program Files exist only on
// Windows.
func LegacyUninstaller() string {
	return ""
}

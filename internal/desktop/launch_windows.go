package desktop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// openLocked opens path for reading with FILE_SHARE_READ only: while the handle is
// open, nobody can open the file for writing or deletion, so it cannot be changed,
// replaced or renamed; it can still be read and executed.
func openLocked(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// LaunchInstaller starts the installer at path with args, in its own directory,
// through ShellExecute. The installer's manifest requests administrator rights
// (per-machine install in Program Files) and the app runs as the invoker, so
// ShellExecute shows the UAC prompt, where CreateProcess would fail with
// ERROR_ELEVATION_REQUIRED. It returns once the installer has started, or
// ErrInstallerCancelled when the user declines the prompt. No shell or command
// interpreter is involved, and the installer, a separate process, outlives the app.
func LaunchInstaller(path string, args []string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("installer path: %w", err)
	}
	dir, err := windows.UTF16PtrFromString(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("installer directory: %w", err)
	}
	var params *uint16
	if len(args) > 0 {
		if params, err = windows.UTF16PtrFromString(installerParams(args)); err != nil {
			return fmt.Errorf("installer arguments: %w", err)
		}
	}
	if err := windows.ShellExecute(0, verb, file, params, dir, windows.SW_SHOWNORMAL); err != nil {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return fmt.Errorf("%w (UAC prompt declined)", ErrInstallerCancelled)
		}
		return fmt.Errorf("ShellExecute %s: %w", path, err)
	}
	return nil
}

// installerParams returns args as a Windows command line, quoting where needed. A
// last /D=<dir> argument is appended as is: NSIS reads the install directory from
// the rest of the command line and does not accept quotes around it.
func installerParams(args []string) string {
	n := len(args)
	if n == 0 || !strings.HasPrefix(args[n-1], installDirArg) {
		return windows.ComposeCommandLine(args)
	}
	if n == 1 {
		return args[0]
	}
	return windows.ComposeCommandLine(args[:n-1]) + " " + args[n-1]
}

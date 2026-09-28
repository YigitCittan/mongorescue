package desktop

import (
	"fmt"
	"os"

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

// LaunchInstaller starts the installer at path through ShellExecute, which shows the
// UAC prompt for an installer that requires elevation (CreateProcess would fail with
// ERROR_ELEVATION_REQUIRED). It returns once the installer has started; no shell or
// command interpreter is involved, and the installer outlives the app.
func LaunchInstaller(path string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("installer path: %w", err)
	}
	if err := windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		return fmt.Errorf("ShellExecute %s: %w", path, err)
	}
	return nil
}

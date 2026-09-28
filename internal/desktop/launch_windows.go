package desktop

import (
	"fmt"

	"golang.org/x/sys/windows"
)

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

package desktop

import (
	"errors"
	"fmt"
	"strings"
)

// AutostartValueName is the value of the Windows Run key
// (HKCU\Software\Microsoft\Windows\CurrentVersion\Run) that starts the app when
// the user signs in.
const AutostartValueName = "MongoRescue"

// ErrInvalidExecutable is returned when the executable path cannot be written as
// an autostart command.
var ErrInvalidExecutable = errors.New("desktop: invalid executable path for autostart")

// AutostartCommand returns the command line of the autostart entry: the quoted
// executable path and HiddenFlag, as in `"C:\Apps\MongoRescue.exe" --hidden`.
// The path is always quoted, so a path with spaces stays one argument; a path
// that is empty or holds a quote or a line break is refused.
func AutostartCommand(exe string) (string, error) {
	if exe == "" || strings.ContainsAny(exe, "\"\r\n\x00") {
		return "", fmt.Errorf("%w: %q", ErrInvalidExecutable, exe)
	}
	return `"` + exe + `" ` + HiddenFlag, nil
}

// RunKey reads and writes the string values of a registry key; on Windows the
// Run key of the current user implements it.
type RunKey interface {
	// Get returns the value name and whether it exists.
	Get(name string) (value string, ok bool, err error)
	// Set writes the value name.
	Set(name, value string) error
	// Delete removes the value name; a missing value is not an error.
	Delete(name string) error
}

// Autostart turns the start with Windows on and off.
type Autostart struct {
	// Key is the Run key.
	Key RunKey
	// Executable returns the running executable; nil means ExecutablePath.
	Executable func() (string, error)
}

// Enabled reports whether the autostart entry exists.
func (a *Autostart) Enabled() (bool, error) {
	v, ok, err := a.Key.Get(AutostartValueName)
	if err != nil {
		return false, fmt.Errorf("read the autostart entry: %w", err)
	}
	return ok && strings.TrimSpace(v) != "", nil
}

// SetEnabled writes the autostart entry for the running executable, or removes
// it.
func (a *Autostart) SetEnabled(on bool) error {
	if !on {
		if err := a.Key.Delete(AutostartValueName); err != nil {
			return fmt.Errorf("remove the autostart entry: %w", err)
		}
		return nil
	}
	executable := a.Executable
	if executable == nil {
		executable = ExecutablePath
	}
	exe, err := executable()
	if err != nil {
		return fmt.Errorf("find the executable: %w", err)
	}
	cmd, err := AutostartCommand(exe)
	if err != nil {
		return err
	}
	if err = a.Key.Set(AutostartValueName, cmd); err != nil {
		return fmt.Errorf("write the autostart entry: %w", err)
	}
	return nil
}

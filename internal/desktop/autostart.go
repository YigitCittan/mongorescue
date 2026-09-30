package desktop

import (
	"errors"
	"fmt"
	"path/filepath"
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

// Enabled reports whether the autostart entry starts the running executable. An
// entry for another path (another copy, an old install location) reports false,
// so turning autostart on rewrites it for this copy.
func (a *Autostart) Enabled() (bool, error) {
	v, ok, err := a.Key.Get(AutostartValueName)
	if err != nil {
		return false, fmt.Errorf("read the autostart entry: %w", err)
	}
	if !ok || strings.TrimSpace(v) == "" {
		return false, nil
	}
	exe, err := a.executable()()
	if err != nil {
		return false, fmt.Errorf("find the executable: %w", err)
	}
	return sameExecutable(commandExecutable(v), exe), nil
}

// executable returns the Executable function or ExecutablePath.
func (a *Autostart) executable() func() (string, error) {
	if a.Executable != nil {
		return a.Executable
	}
	return ExecutablePath
}

// commandExecutable returns the program of a Run key command line: the quoted
// first part, or the part before the first space.
func commandExecutable(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if rest, ok := strings.CutPrefix(cmd, `"`); ok {
		if i := strings.IndexByte(rest, '"'); i >= 0 {
			return rest[:i]
		}
		return rest
	}
	if i := strings.IndexAny(cmd, " \t"); i >= 0 {
		return cmd[:i]
	}
	return cmd
}

// sameExecutable compares two Windows paths, which are case-insensitive.
func sameExecutable(a, b string) bool {
	return a != "" && strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
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
	exe, err := a.executable()()
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

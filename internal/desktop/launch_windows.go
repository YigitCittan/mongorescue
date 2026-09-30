package desktop

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
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

// LaunchInstaller starts the installer, or uninstaller, at path with args, in its
// own directory, through ShellExecute. When the program's manifest requests
// administrator rights (a per-machine copy in Program Files), ShellExecute shows
// the UAC prompt, where CreateProcess would fail with ERROR_ELEVATION_REQUIRED; the
// per-user installer starts without one. It returns once the program has started,
// or ErrInstallerCancelled when the user declines the prompt. No shell or command
// interpreter is involved, and the program, a separate process, outlives the app.
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
	if err = windows.ShellExecute(0, verb, file, params, dir, windows.SW_SHOWNORMAL); err != nil {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return fmt.Errorf("%w (UAC prompt declined)", ErrInstallerCancelled)
		}
		return fmt.Errorf("ShellExecute %s: %w", path, err)
	}
	return nil
}

// installerParams returns args as a command line for an NSIS program. NSIS options
// (/S, /RELAUNCH=<user>, /WAITPID=<pid>) are passed as they are, since NSIS
// GetOptions does not strip quotes around a whole option; any other argument is
// quoted where needed.
func installerParams(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "/") && !strings.ContainsAny(a, "\"\t\r\n") {
			parts[i] = a
		} else {
			parts[i] = windows.EscapeArg(a)
		}
	}
	return strings.Join(parts, " ")
}

// StartDetached starts exe with args in its own directory as a new process of the
// current user, without a shell or console, and returns once it runs; the process
// outlives the app. Only the standard handles are inherited, so the new process
// does not hold the data directory lock.
func StartDetached(exe string, args []string) error {
	cmd := exec.Command(exe, args...) //nolint:noctx // the new version must outlive the app, so it is not bound to a context.
	cmd.Dir = filepath.Dir(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", exe, err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release %s: %w", exe, err)
	}
	return nil
}

// WaitForProcessExit waits up to timeout for the process pid to exit and reports
// whether it has. A process that cannot be opened is taken as gone, and so is one
// created after the calling process, which reuses the ID of the process waited for.
func WaitForProcessExit(pid int, timeout time.Duration) bool {
	if pid <= 0 || int64(pid) > math.MaxUint32 || pid == os.Getpid() {
		return true
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return true
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if createdAfterSelf(h) {
		return true
	}
	ms := timeout.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	ev, err := windows.WaitForSingleObject(h, uint32(min(ms, int64(windows.INFINITE-1))))
	return err == nil && ev == windows.WAIT_OBJECT_0
}

// createdAfterSelf reports whether the process h was created after the calling
// process.
func createdAfterSelf(h windows.Handle) bool {
	var created, self, exit, kernel, usr windows.Filetime
	if windows.GetProcessTimes(h, &created, &exit, &kernel, &usr) != nil ||
		windows.GetProcessTimes(windows.CurrentProcess(), &self, &exit, &kernel, &usr) != nil {
		return false
	}
	return created.Nanoseconds() > self.Nanoseconds()
}

// legacyUninstallKey is the uninstall entry the Wails NSIS installer writes; a
// per-machine install writes it under HKLM, the per-user one under HKCU.
const legacyUninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\MongoRescueMongoRescue`

// LegacyUninstaller returns the uninstaller of a per-machine copy of the app
// registered under HKLM (an install in Program Files by an earlier version), or ""
// when there is none.
func LegacyUninstaller() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, legacyUninstallKey, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer func() { _ = k.Close() }()
	s, _, err := k.GetStringValue("UninstallString")
	if err != nil {
		return ""
	}
	return parseUninstallString(s)
}

// ShowError shows a native error dialog with title and text and returns once it
// is closed. It serves errors before the window exists, which a GUI process could
// otherwise only log.
func ShowError(title, text string) {
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	m, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	_, _ = windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR|windows.MB_SETFOREGROUND)
}

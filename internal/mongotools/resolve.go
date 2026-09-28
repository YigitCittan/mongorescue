package mongotools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// EnvToolsDir names the environment variable (and, as -tools-dir, the flag) that
// points at a directory holding the MongoDB Database Tools binaries.
const EnvToolsDir = "MONGORESCUE_TOOLS_DIR"

// BundledToolsDirName is the directory next to the MongoRescue executable (or, in a
// macOS .app bundle, under Contents/Resources) searched for bundled tools.
const BundledToolsDirName = "tools"

// ErrToolNotFound is returned (wrapped in a *ToolNotFoundError) when a MongoDB
// Database Tools binary cannot be found in any searched location.
var ErrToolNotFound = errors.New("mongotools: tool not found")

// ToolNotFoundError reports a tool that is missing from every searched location. It
// matches ErrToolNotFound with errors.Is.
type ToolNotFoundError struct {
	// Name is the tool that was looked up, e.g. "mongodump".
	Name string
	// Searched lists the candidate paths and PATH lookups that were tried, in order.
	// It is meant for logs only: it may contain local paths (and user names), so
	// Error leaves it out of the message that ends up in records and API responses.
	Searched []string
}

// Error returns an actionable message without the searched paths.
func (e *ToolNotFoundError) Error() string {
	return fmt.Sprintf("%s not found: install MongoDB Database Tools or set %s", e.Name, EnvToolsDir)
}

// Unwrap returns ErrToolNotFound.
func (e *ToolNotFoundError) Unwrap() error { return ErrToolNotFound }

// LogNotFound logs a warning with the searched locations when err is a
// *ToolNotFoundError, and does nothing otherwise. The locations stay in the log and
// out of error messages.
func LogNotFound(ctx context.Context, logger *slog.Logger, err error) {
	var nf *ToolNotFoundError
	if !errors.As(err, &nf) {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.WarnContext(ctx, "MongoDB Database Tools binary not found",
		slog.String("tool", nf.Name), slog.String("searched", strings.Join(nf.Searched, ", ")),
		slog.String("hint", "install MongoDB Database Tools or set "+EnvToolsDir))
}

// Resolver locates MongoDB Database Tools binaries. The zero value searches the
// bundled locations and PATH; the function fields exist so tests can resolve
// hermetically and default to the os/exec and runtime implementations when nil.
//
// Search order:
//  1. Dir, when set and absolute (MONGORESCUE_TOOLS_DIR / -tools-dir); a relative
//     Dir is ignored so that the working directory can never supply a binary;
//  2. <directory of the executable>/tools;
//  3. on darwin, when the executable is <bundle>/Contents/MacOS/<name>,
//     <bundle>/Contents/Resources/tools (inside a .app);
//  4. exec.LookPath (PATH).
//
// Directory lookups append ".exe" on Windows and accept only regular files (outside
// Windows, only those with an execute bit).
type Resolver struct {
	// Dir is the configured tools directory; empty or relative skips it.
	Dir string
	// Executable returns the path of the running binary (default os.Executable).
	Executable func() (string, error)
	// LookPath searches PATH (default exec.LookPath).
	LookPath func(string) (string, error)
	// GOOS is the target operating system (default runtime.GOOS).
	GOOS string
}

// NewResolver returns a Resolver that searches dir first (empty means none).
func NewResolver(dir string) *Resolver {
	return &Resolver{Dir: dir}
}

// Resolve locates name with a Resolver that has no configured directory.
func Resolve(name string) (string, error) {
	return NewResolver("").Resolve(name)
}

// Resolve returns the absolute path of the tool name (e.g. "mongodump"), or an error
// wrapping ErrToolNotFound. A name that already contains a path separator is only
// checked, not searched for. A nil Resolver behaves like the zero value.
func (r *Resolver) Resolve(name string) (string, error) {
	if r == nil {
		r = &Resolver{}
	}
	goos := r.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if strings.ContainsAny(name, `/\`) {
		if isExecutableFile(name) {
			return absOrSelf(name), nil
		}
		return "", &ToolNotFoundError{Name: name, Searched: []string{name}}
	}

	file := name
	if goos == "windows" && !strings.EqualFold(filepath.Ext(name), ".exe") {
		file += ".exe"
	}
	var searched []string
	for _, dir := range r.searchDirs(goos) {
		candidate := filepath.Join(dir, file)
		searched = append(searched, candidate)
		if isExecutableFile(candidate) {
			return absOrSelf(candidate), nil
		}
	}

	lookPath := r.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	searched = append(searched, "PATH")
	if p, err := lookPath(name); err == nil && p != "" {
		return absOrSelf(p), nil
	}
	return "", &ToolNotFoundError{Name: name, Searched: searched}
}

// searchDirs returns the directories searched before PATH, in order.
func (r *Resolver) searchDirs(goos string) []string {
	var dirs []string
	if d := strings.TrimSpace(r.Dir); d != "" && filepath.IsAbs(d) {
		dirs = append(dirs, filepath.Clean(d))
	}
	executable := r.Executable
	if executable == nil {
		executable = os.Executable
	}
	exe, err := executable()
	if err != nil || exe == "" {
		return dirs
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	exeDir := filepath.Dir(exe)
	dirs = append(dirs, filepath.Join(exeDir, BundledToolsDirName))
	if contents := filepath.Dir(exeDir); goos == "darwin" &&
		filepath.Base(exeDir) == "MacOS" && filepath.Base(contents) == "Contents" {
		dirs = append(dirs, filepath.Join(contents, "Resources", BundledToolsDirName))
	}
	return dirs
}

// isExecutableFile reports whether path is a regular file that may be executed: any
// regular file on Windows (which has no execute bit), one with an execute bit
// elsewhere. It depends on the host file system, so it uses runtime.GOOS.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0
}

// absOrSelf returns path made absolute, or path unchanged if that fails.
func absOrSelf(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

package mongotools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
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
// bundled locations, PATH and well-known install directories; the function fields exist so tests can resolve
// hermetically and default to the os/exec and runtime implementations when nil.
//
// Search order:
//  1. Dir, when set and absolute (MONGORESCUE_TOOLS_DIR / -tools-dir); a relative
//     Dir is ignored so that the working directory can never supply a binary;
//  2. <directory of the executable>/tools;
//  3. on darwin, when the executable is <bundle>/Contents/MacOS/<name>,
//     <bundle>/Contents/Resources/tools (inside a .app);
//  4. exec.LookPath (PATH);
//  5. well-known install directories (WellKnownDirs, default DefaultWellKnownDirs),
//     for processes started with a minimal PATH, such as a macOS app launched from
//     Finder that does not see Homebrew's /opt/homebrew/bin.
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
	// WellKnownDirs returns the install directories searched after PATH for goos
	// (default DefaultWellKnownDirs).
	WellKnownDirs func(goos string) []string
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

	wellKnownDirs := r.WellKnownDirs
	if wellKnownDirs == nil {
		wellKnownDirs = DefaultWellKnownDirs
	}
	for _, dir := range wellKnownDirs(goos) {
		// As with Dir, a relative directory would let the working directory supply
		// a binary.
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, file)
		searched = append(searched, candidate)
		if isExecutableFile(candidate) {
			return absOrSelf(candidate), nil
		}
	}
	return "", &ToolNotFoundError{Name: name, Searched: searched}
}

// DefaultWellKnownDirs returns the directories where package managers and installers
// usually put the MongoDB Database Tools on goos, searched after PATH:
// /opt/homebrew/bin, /usr/local/bin and /opt/local/bin (MacPorts) on darwin;
// <Program Files>\MongoDB\Tools\<version>\bin on windows, highest version first,
// under %ProgramW6432% (the 64-bit Program Files, set for 32-bit processes too) and
// then %ProgramFiles%, each once, or C:\Program Files when neither is set;
// /usr/local/bin, /usr/bin and /snap/bin elsewhere. Resolve ignores relative
// directories.
func DefaultWellKnownDirs(goos string) []string {
	switch goos {
	case "darwin":
		return []string{"/opt/homebrew/bin", "/usr/local/bin", "/opt/local/bin"}
	case "windows":
		var bases []string
		for _, env := range []string{"ProgramW6432", "ProgramFiles"} {
			base := strings.TrimSpace(os.Getenv(env))
			if base != "" && !slices.ContainsFunc(bases, func(b string) bool { return strings.EqualFold(b, base) }) {
				bases = append(bases, base)
			}
		}
		if len(bases) == 0 {
			bases = []string{`C:\Program Files`}
		}
		var dirs []string
		for _, base := range bases {
			dirs = append(dirs, windowsToolsDirs(base)...)
		}
		return dirs
	default:
		return []string{"/usr/local/bin", "/usr/bin", "/snap/bin"}
	}
}

// windowsToolsDirs returns <programFiles>/MongoDB/Tools/<version>/bin for every
// version directory, highest version first.
func windowsToolsDirs(programFiles string) []string {
	toolsDir := filepath.Join(programFiles, "MongoDB", "Tools")
	entries, err := os.ReadDir(toolsDir)
	if err != nil {
		return nil
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	slices.SortFunc(versions, func(a, b string) int { return compareVersions(b, a) })
	dirs := make([]string, 0, len(versions))
	for _, v := range versions {
		dirs = append(dirs, filepath.Join(toolsDir, v, "bin"))
	}
	return dirs
}

// compareVersions compares dot-separated versions such as "100" and "99.1" segment
// by segment: numerically where both segments are numbers, a number above a
// non-number, and as strings otherwise.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aErr := strconv.Atoi(as[i])
		bn, bErr := strconv.Atoi(bs[i])
		var c int
		switch {
		case aErr == nil && bErr == nil:
			c = cmp.Compare(an, bn)
		case aErr == nil:
			c = 1
		case bErr == nil:
			c = -1
		default:
			c = strings.Compare(as[i], bs[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(as), len(bs))
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

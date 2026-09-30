package desktop

import (
	"archive/zip"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/update"
)

// The in-app update of a Windows copy whose directory the user can write to (the
// per-user install in %LOCALAPPDATA%\Programs\MongoRescue, or a portable copy):
// the verified portable archive is unpacked to a staging directory next to the
// executable, the running MongoRescue.exe and tools\ are renamed to *.old (Windows
// allows renaming a running executable) and the new ones moved in, the new version
// is started with AfterUpdateFlag and the app quits. The new version waits for the
// old process to exit and removes the leftovers.

// AfterUpdateFlag is the command-line flag, with the PID of the old process as its
// value (--after-update=<pid>), that the in-app update starts the new version
// with. See ParseAfterUpdate.
const AfterUpdateFlag = "after-update"

// AfterUpdateWait bounds how long a version started with AfterUpdateFlag waits for
// the old process to exit and release the data directory.
const AfterUpdateWait = 60 * time.Second

// Names used by the in-app update, in the executable's directory.
const (
	// stagingPrefix starts the directory the archive is unpacked to
	// (.update-<version>).
	stagingPrefix = ".update-"
	// oldSuffix marks the executable and tools directory the update replaced.
	oldSuffix = ".old"
	// toolsDirName is the bundled MongoDB Database Tools directory.
	toolsDirName = "tools"
	// archiveExe is the executable's name in the portable archive.
	archiveExe = "MongoRescue.exe"
)

// Limits of the portable archive: it holds the executable and a few tools.
const (
	maxArchiveEntries = 16
	maxEntrySize      = 512 << 20
	maxUnpackedSize   = update.MaxAssetSize
)

// archiveTools are the files allowed in the archive's tools/ directory.
var archiveTools = map[string]bool{
	"mongodump.exe":       true,
	"mongorestore.exe":    true,
	"LICENSE.md":          true,
	"THIRD-PARTY-NOTICES": true,
}

// ErrBadUpdateArchive is returned when the portable archive holds an unexpected,
// unsafe or oversized entry, or lacks the executable.
var ErrBadUpdateArchive = errors.New("unexpected update archive")

// ExecutablePath returns the path of the running executable with symlinks
// resolved.
func ExecutablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find the executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// CurrentUserName returns the account the app runs as (DOMAIN\user on Windows), or
// "" when it is unknown.
func CurrentUserName() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}

// DirWritable reports whether the current user can create files in dir: it creates
// and removes a temporary file there.
func DirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".mongorescue-write-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name) == nil
}

// ParseAfterUpdate removes AfterUpdateFlag (-after-update=<pid>, --after-update=<pid>
// or the flag followed by the PID) from args and returns the PID, or 0 when the flag
// is missing or its value is not a positive number, and the other arguments.
func ParseAfterUpdate(args []string) (int, []string) {
	pid := 0
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if !strings.HasPrefix(args[i], "-") || name != AfterUpdateFlag {
			rest = append(rest, args[i])
			continue
		}
		if !hasValue && i+1 < len(args) {
			i++
			value = args[i]
		}
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			pid = n
		}
	}
	return pid, rest
}

// relaunchArgs returns the arguments the new version is started with: the running
// app's args, without an earlier AfterUpdateFlag, and AfterUpdateFlag with pid.
func relaunchArgs(args []string, pid int) []string {
	_, rest := ParseAfterUpdate(args)
	return append(rest, "--"+AfterUpdateFlag+"="+strconv.Itoa(pid))
}

// stagingDir returns the directory the archive of version v is unpacked to.
func stagingDir(appDir string, v update.Version) string {
	return filepath.Join(appDir, stagingPrefix+v.String())
}

// verifyAndExtract opens file.Path with openLocked, so it cannot be changed while
// it is used, checks its SHA-256 again through that handle and unpacks it to
// staging (see extractArchive), naming the executable exeName.
func verifyAndExtract(file update.File, staging, exeName string) error {
	f, err := openLocked(file.Path)
	if err != nil {
		return fmt.Errorf("open %s: %w", file.Path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return fmt.Errorf("read %s: %w", file.Path, err)
	}
	if len(file.SHA256) != sha256.Size || subtle.ConstantTimeCompare(h.Sum(nil), file.SHA256) != 1 {
		return fmt.Errorf("%s: %w", filepath.Base(file.Path), update.ErrChecksumMismatch)
	}
	zr, err := zip.NewReader(f, size)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		// Insecure names are refused entry by entry below.
		return fmt.Errorf("%s: %w: %w", filepath.Base(file.Path), ErrBadUpdateArchive, err)
	}
	return extractArchive(zr, staging, exeName)
}

// archiveTarget returns the path below staging that the archive entry name (with
// / or \ separators) unpacks to, "" for the tools/ directory entry, or an error
// wrapping ErrBadUpdateArchive for any other entry: only MongoRescue.exe and the
// archiveTools files in tools/ are accepted, which also rules out absolute paths,
// "..", drive letters and nested directories.
func archiveTarget(name, exeName string) (string, error) {
	n := strings.ReplaceAll(name, `\`, "/")
	switch {
	case n == archiveExe:
		return exeName, nil
	case n == toolsDirName+"/":
		return "", nil
	}
	if base, ok := strings.CutPrefix(n, toolsDirName+"/"); ok && archiveTools[base] {
		return filepath.Join(toolsDirName, base), nil
	}
	return "", fmt.Errorf("%w: entry %q", ErrBadUpdateArchive, name)
}

// extractArchive unpacks the portable archive zr to staging (created with mode
// 0700), streaming each file to disk: the executable as exeName and the tools to
// staging/tools. It accepts only the entries of archiveTarget, each at most once,
// no links, at most maxArchiveEntries entries, maxEntrySize per file and
// maxUnpackedSize in total, and requires the executable.
func extractArchive(zr *zip.Reader, staging, exeName string) error {
	if len(zr.File) > maxArchiveEntries {
		return fmt.Errorf("%w: %d entries", ErrBadUpdateArchive, len(zr.File))
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", staging, err)
	}
	seen := map[string]bool{}
	var total int64
	for _, zf := range zr.File {
		target, err := archiveTarget(zf.Name, exeName)
		if err != nil {
			return err
		}
		mode := zf.Mode()
		if target == "" {
			if !mode.IsDir() {
				return fmt.Errorf("%w: entry %q", ErrBadUpdateArchive, zf.Name)
			}
			continue
		}
		if !mode.IsRegular() {
			return fmt.Errorf("%w: %q is not a regular file", ErrBadUpdateArchive, zf.Name)
		}
		key := strings.ToLower(target)
		if seen[key] {
			return fmt.Errorf("%w: %q appears twice", ErrBadUpdateArchive, zf.Name)
		}
		seen[key] = true
		if zf.UncompressedSize64 > maxEntrySize {
			return fmt.Errorf("%w: %q is too large", ErrBadUpdateArchive, zf.Name)
		}
		n, err := extractFile(zf, filepath.Join(staging, target))
		if err != nil {
			return err
		}
		if total += n; total > maxUnpackedSize {
			return fmt.Errorf("%w: too large", ErrBadUpdateArchive)
		}
	}
	if !seen[strings.ToLower(exeName)] {
		return fmt.Errorf("%w: no %s", ErrBadUpdateArchive, archiveExe)
	}
	return nil
}

// extractFile streams the archive entry zf to the new file dst and returns its
// size. The zip reader checks the entry's CRC-32 at its end.
func extractFile(zf *zip.File, dst string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
	}
	rc, err := zf.Open()
	if err != nil {
		return 0, fmt.Errorf("open %s in the archive: %w", zf.Name, err)
	}
	defer func() { _ = rc.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", dst, err)
	}
	n, err := io.Copy(out, io.LimitReader(rc, maxEntrySize+1))
	if err == nil && n > maxEntrySize {
		err = fmt.Errorf("%w: %q is too large", ErrBadUpdateArchive, zf.Name)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, fmt.Errorf("unpack %s: %w", zf.Name, err)
	}
	return n, nil
}

// fileOps are the file system calls of the swap and the cleanup; tests replace
// them to fail at a given step.
type fileOps struct {
	rename    func(oldpath, newpath string) error
	removeAll func(path string) error
	lstat     func(path string) (fs.FileInfo, error)
	readDir   func(path string) ([]fs.DirEntry, error)
}

// osFileOps returns the real file system calls. Renames are retried briefly: on
// Windows a virus scanner or the indexer can hold a new file for a moment.
func osFileOps() fileOps {
	return fileOps{
		rename: func(oldpath, newpath string) error {
			var err error
			for i := 0; i < 5; i++ {
				if err = os.Rename(oldpath, newpath); err == nil {
					return nil
				}
				time.Sleep(100 * time.Millisecond)
			}
			return err
		},
		removeAll: os.RemoveAll,
		lstat:     os.Lstat,
		readDir:   os.ReadDir,
	}
}

// swap records the renames of a swap, so they can be undone.
type swap struct {
	ops   fileOps
	moves [][2]string // from, to
}

// move renames from to to and records it.
func (s *swap) move(from, to string) error {
	if err := s.ops.rename(from, to); err != nil {
		return fmt.Errorf("move %s to %s: %w", from, to, err)
	}
	s.moves = append(s.moves, [2]string{from, to})
	return nil
}

// rollback undoes the recorded renames, the last first, and returns the renames
// that could not be undone.
func (s *swap) rollback() error {
	var errs []error
	for i := len(s.moves) - 1; i >= 0; i-- {
		m := s.moves[i]
		if err := s.ops.rename(m[1], m[0]); err != nil {
			errs = append(errs, fmt.Errorf("move %s back to %s: %w", m[1], m[0], err))
		}
	}
	s.moves = nil
	return errors.Join(errs...)
}

// swapInFiles replaces the executable exe and the tools directory next to it with
// the ones in staging: exe becomes exe.old and staging's executable moves to exe,
// then, when staging has tools, tools becomes tools.old and staging's tools move in.
// Earlier *.old leftovers are removed first. On failure every rename is undone and
// the error returned; on success the returned swap can still be rolled back.
func swapInFiles(ops fileOps, exe, staging string) (*swap, error) {
	dir := filepath.Dir(exe)
	tools := filepath.Join(dir, toolsDirName)
	newExe := filepath.Join(staging, filepath.Base(exe))
	newTools := filepath.Join(staging, toolsDirName)
	for _, p := range []string{exe + oldSuffix, tools + oldSuffix} {
		if err := ops.removeAll(p); err != nil {
			return nil, fmt.Errorf("remove %s: %w", p, err)
		}
	}
	if _, err := ops.lstat(newExe); err != nil {
		return nil, fmt.Errorf("staged executable: %w", err)
	}
	s := &swap{ops: ops}
	err := s.move(exe, exe+oldSuffix)
	if err == nil {
		err = s.move(newExe, exe)
	}
	if fi, statErr := ops.lstat(newTools); err == nil && statErr == nil && fi.IsDir() {
		if _, oldErr := ops.lstat(tools); oldErr == nil {
			err = s.move(tools, tools+oldSuffix)
		}
		if err == nil {
			err = s.move(newTools, tools)
		}
	}
	if err != nil {
		if rbErr := s.rollback(); rbErr != nil {
			err = errors.Join(err, fmt.Errorf("roll back: %w", rbErr))
		}
		return nil, err
	}
	return s, nil
}

// removeLeftovers removes, best effort, what an in-app update leaves next to the
// executable exe: exe.old, tools.old and .update-* staging directories. The old
// executable can only be removed once its process has exited. Other files, such
// as unrelated *.old files next to a portable copy, are kept. It returns the
// removed paths.
func removeLeftovers(ops fileOps, exe string) []string {
	dir := filepath.Dir(exe)
	candidates := []string{exe + oldSuffix, filepath.Join(dir, toolsDirName+oldSuffix)}
	if entries, err := ops.readDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), stagingPrefix) {
				candidates = append(candidates, filepath.Join(dir, e.Name()))
			}
		}
	}
	var removed []string
	for _, p := range candidates {
		if _, err := ops.lstat(p); err != nil {
			continue
		}
		if err := ops.removeAll(p); err == nil {
			removed = append(removed, p)
		}
	}
	return removed
}

// RemoveUpdateLeftovers removes, on Windows and best effort, the old executable,
// the old tools directory and the staging directories an in-app update left next
// to the running executable. It does nothing on other platforms.
func RemoveUpdateLeftovers(logger *slog.Logger) {
	if runtime.GOOS != "windows" {
		return
	}
	exe, err := ExecutablePath()
	if err != nil {
		return
	}
	for _, p := range removeLeftovers(osFileOps(), exe) {
		logger.Info("removed a leftover of the last update", slog.String("path", p))
	}
}

// silentInstallerArgs returns the installer's command-line arguments for the
// update of a copy the user cannot write to: /S runs the NSIS installer without
// its wizard, /WAITPID=<pid> makes it wait for the app to exit, and
// /RELAUNCH=<user> makes it start the new version when it runs as user (see
// build/windows/installer/project.nsi); without a usable user name the new version
// is not started, so it never runs as another account.
func silentInstallerArgs(user string, pid int) []string {
	args := []string{"/S"}
	if relaunchUserOK(user) {
		args = append(args, "/RELAUNCH="+user)
	}
	if pid > 0 {
		args = append(args, "/WAITPID="+strconv.Itoa(pid))
	}
	return args
}

// relaunchUserOK reports whether user can be passed as /RELAUNCH=<user>: NSIS
// GetOptions reads the value up to the next "/", and quotes or control characters
// would change the command line. Windows account names contain none of them.
func relaunchUserOK(user string) bool {
	if strings.TrimSpace(user) == "" || strings.ContainsAny(user, `"/`) {
		return false
	}
	for _, r := range user {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// sameWindowsDir reports whether the Windows paths a and b name the same
// directory, ignoring case, separators and a trailing separator.
func sameWindowsDir(a, b string) bool {
	norm := func(p string) string {
		return strings.TrimRight(strings.ReplaceAll(p, "/", `\`), `\`)
	}
	return a != "" && strings.EqualFold(norm(a), norm(b))
}

// windowsDir returns the directory part of the Windows path p.
func windowsDir(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[:i]
	}
	return ""
}

// parseUninstallString returns the uninstaller path of an UninstallString registry
// value ("C:\...\uninstall.exe", quoted or not, possibly with arguments), or ""
// when it does not name an uninstall.exe.
func parseUninstallString(s string) string {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, `"`); ok {
		s, _, _ = strings.Cut(rest, `"`)
	} else if i := strings.Index(strings.ToLower(s), ".exe"); i >= 0 {
		s = s[:i+len(".exe")]
	}
	base := s[strings.LastIndexAny(s, `\/`)+1:]
	if !strings.EqualFold(base, "uninstall.exe") || windowsDir(s) == "" {
		return ""
	}
	return s
}

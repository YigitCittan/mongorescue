package desktop

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/update"
)

// Update endpoints served by Updater.Handler. They are only reachable from the app's
// own webview: Handler must never wrap a handler on a network listener.
const (
	// UpdatePath returns the update status (GET).
	UpdatePath = "/desktop/update"
	// UpdateInstallPath downloads, verifies and installs the update (POST).
	UpdateInstallPath = "/desktop/update/install"
	// UpdateReleasePagePath opens the release page in the system browser (POST).
	UpdateReleasePagePath = "/desktop/update/release-page"
	// UpdateRemoveLegacyPath starts the uninstaller of a per-machine copy left in
	// Program Files (POST; Windows).
	UpdateRemoveLegacyPath = "/desktop/update/remove-legacy"
	// UpdateHeader must be "1" on the POST requests: a page cannot send a custom
	// header cross-origin without a CORS preflight, which is never granted.
	UpdateHeader = "X-MongoRescue-Desktop"
)

// UpdateCheckInterval is how often the updater checks again after the startup
// check, while the app runs.
const UpdateCheckInterval = 6 * time.Hour

// Update states reported in UpdateStatus.State.
const (
	UpdateIdle     = "idle"
	UpdateChecking = "checking"
	// UpdateDownloading reports the download; UpdateStatus.Percent has the progress.
	UpdateDownloading = "downloading"
	UpdateReady       = "ready"
	// UpdateInstalling reports that the verified update is being unpacked and
	// swapped in (ActionSwap), or that the verified installer is being started or
	// runs (ActionLaunch).
	UpdateInstalling = "installing"
	// UpdateRestarting reports that the new version was started and the app quits
	// (ActionSwap).
	UpdateRestarting = "restarting"
	UpdateError      = "error"
)

// Update actions reported in UpdateStatus.Action: what Install does with the file.
const (
	// ActionSwap updates the app in place, from the portable archive, and restarts
	// it (Windows, when the user can write to the app's directory).
	ActionSwap = "swap"
	// ActionLaunch runs the installer silently and quits the app; the installer
	// starts the new version once it is done (Windows, for a copy the user cannot
	// write to, such as one in Program Files).
	ActionLaunch = "launch"
	// ActionReveal saves the archive and shows it in the file manager (macOS, Linux).
	ActionReveal = "reveal"
)

// States of a legacy copy's removal, reported in LegacyStatus.State.
const (
	// LegacyIdle means no removal was requested.
	LegacyIdle = ""
	// LegacyRemoving means the uninstaller is being started (UAC prompt).
	LegacyRemoving = "removing"
	// LegacyStarted means the uninstaller runs; the copy disappears once it is done.
	LegacyStarted = "started"
	// LegacyError means the uninstaller did not start; LegacyStatus.Error says why.
	LegacyError = "error"
)

// Updater errors.
var (
	// ErrNoUpdate is returned by Install when no newer version is available.
	ErrNoUpdate = errors.New("no update available")
	// ErrUpdateBusy is returned by Install while a check, a download or an install
	// runs.
	ErrUpdateBusy = errors.New("update check or download in progress")
	// ErrUpdaterNotStarted is returned by Install before Start.
	ErrUpdaterNotStarted = errors.New("updater not started")
	// ErrNotInstallable is reported when the newest release has no verifiable file
	// for this platform (yet).
	ErrNotInstallable = errors.New("the release has no verified file for this platform yet")
	// ErrInstallerCancelled is reported when the user declines the Windows UAC
	// prompt for the installer or uninstaller.
	ErrInstallerCancelled = errors.New("the installation was cancelled")
	// ErrNoLegacyCopy is returned by RemoveLegacy when no per-machine copy is
	// registered.
	ErrNoLegacyCopy = errors.New("no older copy in Program Files")
)

// UpdateSource looks up and downloads releases; *update.Checker implements it.
type UpdateSource interface {
	Check(ctx context.Context, current string) (update.Result, error)
	DownloadAsset(ctx context.Context, res update.Result, asset update.Asset, dir string, progress update.Progress) (update.File, error)
}

// UpdaterOptions configures NewUpdater. Nil functions get the real implementations.
type UpdaterOptions struct {
	// Source is the release source; nil means a zero *update.Checker.
	Source UpdateSource
	// Version is the running version. A version that is not a final release, such
	// as "dev", disables the updater.
	Version string
	// Logger receives the updater's log records; nil means slog.Default().
	Logger *slog.Logger
	// GOOS selects the install action; "" means the running platform.
	GOOS string
	// Interval is the time between checks after the startup check; 0 means
	// UpdateCheckInterval.
	Interval time.Duration
	// Dir returns the directory an update is downloaded to; nil means UpdateDir.
	Dir func(goos string) (string, error)
	// Executable returns the running executable, symlinks resolved; nil means
	// ExecutablePath.
	Executable func() (string, error)
	// Writable reports whether the current user can create files in a directory;
	// on Windows, a writable executable directory selects ActionSwap. Nil means
	// DirWritable.
	Writable func(dir string) bool
	// StartApp starts the updated executable with args as a detached process of the
	// current user (ActionSwap); nil means StartDetached.
	StartApp func(exe string, args []string) error
	// Args are the running app's arguments, passed on to the updated app together
	// with AfterUpdateFlag; nil means os.Args[1:].
	Args []string
	// PID is the running process, which the updated app or the installer waits for;
	// 0 means os.Getpid().
	PID int
	// User returns the account the app runs as, which the installer must run as to
	// start the new version (ActionLaunch); nil means CurrentUserName.
	User func() string
	// Launch starts the downloaded installer, or a legacy copy's uninstaller, with
	// args (Windows). It returns once the program runs, or with an error when it
	// could not be started, such as ErrInstallerCancelled after a declined UAC
	// prompt; nil means LaunchInstaller.
	Launch func(path string, args []string) error
	// LegacyUninstaller returns the uninstaller of a per-machine copy registered
	// under HKLM, or ""; nil means LegacyUninstaller.
	LegacyUninstaller func() string
	// Reveal shows the downloaded archive (macOS, Linux); nil means RevealFile.
	Reveal func(ctx context.Context, path string) error
	// Quit asks the app to quit once the new version or the installer runs, so it
	// can take over; it is not called when that did not start. It must not block on
	// the updater (Wait); nil does nothing.
	Quit func()
	// OpenURL opens the release page in the system browser; nil does nothing.
	OpenURL func(url string)

	// fs replaces the file system calls of the swap in tests.
	fs *fileOps
}

// UpdateStatus is the JSON body of GET UpdatePath.
type UpdateStatus struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	Mandatory bool   `json:"mandatory"`
	// Installable reports whether the release has a verifiable file for this
	// platform; without it only the release page is offered.
	Installable bool   `json:"installable"`
	Notes       string `json:"notes"`
	HTMLURL     string `json:"html_url"`
	State       string `json:"state"`
	Error       string `json:"error"`
	// Percent is the download progress, 0 to 100, while State is
	// UpdateDownloading; -1 when the size is unknown.
	Percent int `json:"percent"`
	// Action is ActionSwap, ActionLaunch or ActionReveal.
	Action string `json:"action"`
	// File is the base name of the downloaded file once State is UpdateReady.
	File string `json:"file"`
	// Legacy reports a per-machine copy left in Program Files (Windows).
	Legacy LegacyStatus `json:"legacy"`
}

// LegacyStatus reports a per-machine copy of the app registered under HKLM, left by
// an earlier version's installer in Program Files, other than the running copy.
// It shares the user's data, so it can simply be uninstalled.
type LegacyStatus struct {
	// Found reports whether such a copy is installed.
	Found bool `json:"found"`
	// Dir is its install directory.
	Dir string `json:"dir"`
	// State is LegacyIdle, LegacyRemoving, LegacyStarted or LegacyError.
	State string `json:"state"`
	// Error says why the uninstaller did not start.
	Error string `json:"error"`
}

// Updater checks for a new desktop release at startup and then every Interval, and
// installs it on request. Its goroutines are bound to the context passed to Start;
// Wait returns once they have finished. It is safe for concurrent use.
type Updater struct {
	opts    UpdaterOptions
	fs      fileOps
	goos    string
	enabled bool

	wg  sync.WaitGroup // the check loop
	ops sync.WaitGroup // installs, reveals and uninstaller starts

	mu     sync.Mutex
	ctx    context.Context // set by Start
	status UpdateStatus
	result update.Result
	path   string // the verified download, once ready
	legacy LegacyStatus
}

// NewUpdater returns an idle updater for opts.Version.
func NewUpdater(opts UpdaterOptions) *Updater {
	if opts.Source == nil {
		opts.Source = &update.Checker{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Interval <= 0 {
		opts.Interval = UpdateCheckInterval
	}
	if opts.Dir == nil {
		opts.Dir = UpdateDir
	}
	if opts.Executable == nil {
		opts.Executable = ExecutablePath
	}
	if opts.Writable == nil {
		opts.Writable = DirWritable
	}
	if opts.StartApp == nil {
		opts.StartApp = StartDetached
	}
	if opts.Args == nil {
		opts.Args = os.Args[1:]
	}
	if opts.PID <= 0 {
		opts.PID = os.Getpid()
	}
	if opts.User == nil {
		opts.User = CurrentUserName
	}
	if opts.Launch == nil {
		opts.Launch = LaunchInstaller
	}
	if opts.LegacyUninstaller == nil {
		opts.LegacyUninstaller = LegacyUninstaller
	}
	if opts.Reveal == nil {
		opts.Reveal = RevealFile
	}
	goos := opts.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	fsOps := osFileOps()
	if opts.fs != nil {
		fsOps = *opts.fs
	}
	u := &Updater{opts: opts, fs: fsOps, goos: goos, status: UpdateStatus{Current: opts.Version, State: UpdateIdle, Action: ActionReveal}}
	if goos == "windows" {
		u.status.Action = ActionLaunch
		if _, ok := u.swapTarget(); ok {
			u.status.Action = ActionSwap
		}
	}
	_, err := update.ParseVersion(opts.Version)
	u.enabled = err == nil
	return u
}

// swapTarget returns the running executable and whether the user can write to its
// directory, so the app can update itself in place (ActionSwap).
func (u *Updater) swapTarget() (string, bool) {
	exe, err := u.opts.Executable()
	if err != nil || exe == "" {
		return "", false
	}
	return exe, u.opts.Writable(filepath.Dir(exe))
}

// Start begins the checks in a goroutine bound to ctx: one now, then one every
// Interval. Later downloads are bound to ctx too. It does nothing for a development
// version or when called again.
func (u *Updater) Start(ctx context.Context) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.ctx != nil {
		return
	}
	u.ctx = ctx
	if !u.enabled {
		u.opts.Logger.Info("update check disabled for a development build", slog.String("version", u.opts.Version))
		return
	}
	u.status.State = UpdateChecking
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.run(ctx)
	}()
}

// Wait blocks until the updater's goroutines have returned. Cancel the context
// passed to Start first.
func (u *Updater) Wait() {
	u.wg.Wait()
	u.ops.Wait()
}

// run checks now and then every Interval until ctx ends.
func (u *Updater) run(ctx context.Context) {
	u.check(ctx, true)
	t := time.NewTicker(u.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.check(ctx, false)
		}
	}
}

// check runs a release check. A failure (typically offline) is logged at info
// level and keeps the previous result. A periodic check is skipped, and its result
// dropped, while a check or a download runs or a download is ready.
func (u *Updater) check(ctx context.Context, startup bool) {
	if !startup && u.busy() {
		return
	}
	res, err := u.opts.Source.Check(ctx, u.opts.Version)
	u.mu.Lock()
	defer u.mu.Unlock()
	if startup {
		u.status.State = UpdateIdle
	}
	if err != nil {
		u.opts.Logger.Info("update check failed", slog.Any("error", err))
		return
	}
	if !startup && u.busyLocked() {
		return
	}
	u.apply(res)
}

// busy reports whether a check, a download or an install runs or a download is
// ready.
func (u *Updater) busy() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.busyLocked()
}

// busyLocked is busy with u.mu held.
func (u *Updater) busyLocked() bool {
	switch u.status.State {
	case UpdateChecking, UpdateDownloading, UpdateReady, UpdateInstalling, UpdateRestarting:
		return true
	}
	return false
}

// apply records a check result. u.mu must be held.
func (u *Updater) apply(res update.Result) {
	changed := res.Latest != u.result.Latest || res.Installable != u.result.Installable || u.status.Latest == ""
	u.result = res
	u.status.Latest = res.Latest.String()
	u.status.Available = res.Available
	u.status.Mandatory = res.Mandatory
	u.status.Installable = res.Installable
	u.status.HTMLURL = res.HTMLURL
	u.status.Notes = ""
	if res.Available {
		u.status.Notes = res.Notes
	}
	if !changed {
		return
	}
	if res.Available {
		u.opts.Logger.Info("update available", slog.String("current", u.opts.Version),
			slog.String("latest", u.status.Latest), slog.Bool("mandatory", res.Mandatory), slog.Bool("installable", res.Installable))
	} else {
		u.opts.Logger.Info("the desktop app is up to date", slog.String("version", u.opts.Version))
	}
}

// Status returns the current update status, with a fresh look for a legacy copy
// on Windows.
func (u *Updater) Status() UpdateStatus {
	legacy := u.lookupLegacy()
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.status
	s.Legacy = u.legacy
	s.Legacy.Found, s.Legacy.Dir = legacy != "", windowsDir(legacy)
	if legacy == "" {
		s.Legacy.State, s.Legacy.Error = LegacyIdle, ""
	}
	return s
}

// lookupLegacy returns the uninstaller of a per-machine copy other than the
// running one, or "" (always on other platforms than Windows).
func (u *Updater) lookupLegacy() string {
	if u.goos != "windows" {
		return ""
	}
	uninstaller := u.opts.LegacyUninstaller()
	if uninstaller == "" {
		return ""
	}
	if exe, err := u.opts.Executable(); err == nil && sameWindowsDir(windowsDir(uninstaller), windowsDir(exe)) {
		return "" // the running copy is the per-machine one
	}
	return uninstaller
}

// RemoveLegacy starts the uninstaller of the per-machine copy (see LegacyStatus)
// silently (/S) in a goroutine bound to the Start context; Windows asks for
// administrator rights. The result is reported in Status().Legacy. It returns
// ErrNoLegacyCopy when there is no such copy.
func (u *Updater) RemoveLegacy() error {
	uninstaller := u.lookupLegacy()
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.ctx == nil:
		return ErrUpdaterNotStarted
	case u.ctx.Err() != nil:
		return fmt.Errorf("updater stopped: %w", u.ctx.Err())
	case uninstaller == "":
		return ErrNoLegacyCopy
	case u.legacy.State == LegacyRemoving:
		return ErrUpdateBusy
	}
	u.legacy = LegacyStatus{State: LegacyRemoving}
	u.ops.Add(1)
	go func() {
		defer u.ops.Done()
		err := u.opts.Launch(uninstaller, []string{"/S"})
		u.mu.Lock()
		defer u.mu.Unlock()
		if err != nil {
			u.opts.Logger.Warn("the older copy's uninstaller did not start", slog.String("path", uninstaller), slog.Any("error", err))
			u.legacy = LegacyStatus{State: LegacyError, Error: err.Error()}
			return
		}
		u.opts.Logger.Info("started the older copy's uninstaller", slog.String("path", uninstaller))
		u.legacy = LegacyStatus{State: LegacyStarted}
	}()
	return nil
}

// Install downloads and verifies the update in a goroutine bound to the Start
// context, then installs it. On Windows, when the user can write to the app's
// directory (the per-user install or a portable copy), it unpacks the portable
// archive next to the executable, swaps the executable and tools\ in (every rename
// is undone on failure), starts the new version and quits (ActionSwap); for a
// copy the user cannot write to, it starts the installer silently and quits, and
// the installer starts the new version once it is done (ActionLaunch). If the new
// version or the installer does not start, the app keeps running and reports the
// error. Elsewhere it leaves the file in the download directory and reveals it.
// After a failure, or when the last check found no installable file, it checks
// for the release again first. Progress and errors are reported through Status.
// Once ready, Install reveals the file again.
func (u *Updater) Install() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.ctx == nil:
		return ErrUpdaterNotStarted
	case u.ctx.Err() != nil:
		return fmt.Errorf("updater stopped: %w", u.ctx.Err())
	case u.status.State == UpdateChecking || u.status.State == UpdateDownloading ||
		u.status.State == UpdateInstalling || u.status.State == UpdateRestarting:
		return ErrUpdateBusy
	case !u.status.Available:
		return ErrNoUpdate
	}
	ctx, res, path := u.ctx, u.result, u.path
	if u.status.State == UpdateReady && path != "" && u.goos != "windows" {
		u.ops.Add(1)
		go func() {
			defer u.ops.Done()
			u.reveal(ctx, path)
		}()
		return nil
	}
	recheck := u.status.State == UpdateError || !res.Installable
	u.status.State, u.status.Error, u.status.File, u.status.Percent = UpdateDownloading, "", "", 0
	if recheck {
		u.status.State = UpdateChecking
	}
	u.ops.Add(1)
	go func() {
		defer u.ops.Done()
		u.install(ctx, res, recheck)
	}()
	return nil
}

// install checks again when recheck is set, then downloads, verifies and installs
// res.
func (u *Updater) install(ctx context.Context, res update.Result, recheck bool) {
	if recheck {
		fresh, err := u.opts.Source.Check(ctx, u.opts.Version)
		if err != nil {
			u.fail(fmt.Errorf("check for the update: %w", err))
			return
		}
		u.mu.Lock()
		u.apply(fresh)
		switch {
		case !fresh.Available:
			u.status.State = UpdateIdle
		case !fresh.Installable:
			u.status.State, u.status.Error = UpdateError, ErrNotInstallable.Error()
		default:
			u.status.State = UpdateDownloading
		}
		state := u.status.State
		u.mu.Unlock()
		if state != UpdateDownloading {
			return
		}
		res = fresh
	}
	if u.goos == "windows" {
		if exe, ok := u.swapTarget(); ok && res.Portable.URL != "" {
			u.setAction(ActionSwap)
			u.swapInstall(ctx, res, exe)
			return
		}
		u.setAction(ActionLaunch)
	}
	dir, err := u.opts.Dir(u.goos)
	if err != nil {
		u.fail(fmt.Errorf("update directory: %w", err))
		return
	}
	file, err := u.opts.Source.DownloadAsset(ctx, res, res.Asset, dir, u.progress)
	if err != nil {
		u.fail(err)
		return
	}
	u.opts.Logger.Info("update downloaded and verified", slog.String("path", file.Path))

	if u.goos != "windows" {
		u.ready(file.Path)
		u.reveal(ctx, file.Path)
		return
	}
	// The app keeps running when the installer does not start (UAC prompt declined,
	// file changed): the error is shown with a retry and the release page.
	u.setState(UpdateInstalling, file.Path)
	if err := verifyAndLaunch(file, silentInstallerArgs(u.opts.User(), u.opts.PID), u.opts.Launch); err != nil {
		u.fail(fmt.Errorf("start the installer: %w", err))
		return
	}
	u.opts.Logger.Info("installer started, quitting for the update", slog.String("path", file.Path))
	if u.opts.Quit != nil {
		u.opts.Quit()
	}
}

// swapInstall downloads the portable archive of res, unpacks it next to exe,
// swaps it in, starts the new version and quits. Every failure leaves the running
// version in place and reports the error.
func (u *Updater) swapInstall(ctx context.Context, res update.Result, exe string) {
	dir, err := u.opts.Dir(u.goos)
	if err != nil {
		u.fail(fmt.Errorf("update directory: %w", err))
		return
	}
	file, err := u.opts.Source.DownloadAsset(ctx, res, res.Portable, dir, u.progress)
	if err != nil {
		u.fail(err)
		return
	}
	u.opts.Logger.Info("update downloaded and verified", slog.String("path", file.Path))
	u.setState(UpdateInstalling, file.Path)

	staging := stagingDir(filepath.Dir(exe), res.Latest)
	cleanup := func() {
		if rmErr := u.fs.removeAll(staging); rmErr != nil {
			u.opts.Logger.Warn("could not remove the update's staging directory", slog.String("path", staging), slog.Any("error", rmErr))
		}
	}
	if err = u.fs.removeAll(staging); err != nil {
		u.fail(fmt.Errorf("remove %s: %w", staging, err))
		return
	}
	if err = verifyAndExtract(file, staging, filepath.Base(exe)); err != nil {
		cleanup()
		u.fail(fmt.Errorf("unpack the update: %w", err))
		return
	}
	sw, err := swapInFiles(u.fs, exe, staging)
	if err != nil {
		cleanup()
		u.fail(fmt.Errorf("install the update: %w", err))
		return
	}
	u.setState(UpdateRestarting, file.Path)
	if err = u.opts.StartApp(exe, relaunchArgs(u.opts.Args, u.opts.PID)); err != nil {
		err = fmt.Errorf("start the new version: %w", err)
		if rbErr := sw.rollback(); rbErr != nil {
			u.opts.Logger.Error("could not restore the running version after a failed update", slog.Any("error", rbErr))
			err = errors.Join(err, rbErr)
		}
		cleanup()
		u.fail(err)
		return
	}
	cleanup()
	u.opts.Logger.Info("new version started, quitting for the update", slog.String("version", res.Latest.String()), slog.String("path", exe))
	if u.opts.Quit != nil {
		u.opts.Quit()
	}
}

// progress records the download progress.
func (u *Updater) progress(done, total int64) {
	pct := -1
	if total > 0 {
		pct = int(min(max(done*100/total, 0), 100))
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status.Percent = pct
}

// setAction records the install action.
func (u *Updater) setAction(action string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status.Action = action
}

// ready records the verified download at path.
func (u *Updater) ready(path string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.path = path
	u.status.State, u.status.File = UpdateReady, filepath.Base(path)
}

// setState records state for the verified download at path.
func (u *Updater) setState(state, path string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.path = path
	u.status.State, u.status.File = state, filepath.Base(path)
}

// reveal shows path in the file manager; a failure is logged only, since the file
// is saved.
func (u *Updater) reveal(ctx context.Context, path string) {
	if err := u.opts.Reveal(ctx, path); err != nil {
		u.opts.Logger.Warn("could not show the downloaded update", slog.String("path", path), slog.Any("error", err))
	}
}

// fail records err as the update error.
func (u *Updater) fail(err error) {
	u.opts.Logger.Warn("update failed", slog.Any("error", err))
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status.State, u.status.Error = UpdateError, err.Error()
}

// verifyAndLaunch opens file.Path with openLocked, which on Windows keeps others
// from writing, replacing or deleting it while the handle is open, checks its
// SHA-256 again through that handle and launches it with args before closing the
// handle. The file cannot be swapped between the check and the launch.
func verifyAndLaunch(file update.File, args []string, launch func(string, []string) error) error {
	f, err := openLocked(file.Path)
	if err != nil {
		return fmt.Errorf("open %s: %w", file.Path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return fmt.Errorf("read %s: %w", file.Path, err)
	}
	if len(file.SHA256) != sha256.Size || subtle.ConstantTimeCompare(h.Sum(nil), file.SHA256) != 1 {
		return fmt.Errorf("%s: %w", filepath.Base(file.Path), update.ErrChecksumMismatch)
	}
	return launch(file.Path, args)
}

// openReleasePage opens the release page, when known, in the system browser.
func (u *Updater) openReleasePage() {
	u.mu.Lock()
	page := u.status.HTMLURL
	u.mu.Unlock()
	if page == "" {
		page = update.DefaultReleasesPrefix + "latest"
	}
	if u.opts.OpenURL != nil {
		u.opts.OpenURL(page)
	}
}

// Handler serves the update endpoints for same-origin requests (see SameOrigin) and
// passes every other request to next. POST requests also need UpdateHeader set to
// "1".
func (u *Updater) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var method string
		switch r.URL.Path {
		case UpdatePath:
			method = http.MethodGet
		case UpdateInstallPath, UpdateReleasePagePath, UpdateRemoveLegacyPath:
			method = http.MethodPost
		default:
			next.ServeHTTP(w, r)
			return
		}
		if !SameOrigin(r) {
			writeJSONError(w, http.StatusForbidden, "cross-origin request")
			return
		}
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if method == http.MethodPost && r.Header.Get(UpdateHeader) != "1" {
			writeJSONError(w, http.StatusForbidden, "missing "+UpdateHeader+" header")
			return
		}
		switch r.URL.Path {
		case UpdateInstallPath:
			if err := u.Install(); err != nil {
				writeJSONError(w, http.StatusConflict, err.Error())
				return
			}
			writeJSON(w, http.StatusAccepted, u.Status())
		case UpdateRemoveLegacyPath:
			if err := u.RemoveLegacy(); err != nil {
				writeJSONError(w, http.StatusConflict, err.Error())
				return
			}
			writeJSON(w, http.StatusAccepted, u.Status())
		case UpdateReleasePagePath:
			u.openReleasePage()
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSON(w, http.StatusOK, u.Status())
		}
	})
}

// writeJSON writes v as a JSON response that is never cached.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONError writes {"error": msg}.
func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// updateDirPrefix starts the name of each per-download directory on Windows.
const updateDirPrefix = "update-"

// UpdateDir returns, creating it if needed, the directory an update for goos is
// downloaded to. On Windows, where the installer runs from it, it is a new
// directory (created with mode 0700) under <user cache dir>/MongoRescue/updates
// (%LocalAppData%), and the directories of earlier downloads are removed. Elsewhere,
// where the user unpacks the archive, it is the user's Downloads directory
// ($XDG_DOWNLOAD_DIR on Linux when it is an absolute path).
func UpdateDir(goos string) (string, error) {
	if goos == "windows" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("cache directory: %w", err)
		}
		base := filepath.Join(cache, AppDirName, "updates")
		if err = os.MkdirAll(base, 0o700); err != nil {
			return "", fmt.Errorf("create %s: %w", base, err)
		}
		removeOldUpdateDirs(base)
		dir, err := os.MkdirTemp(base, updateDirPrefix)
		if err != nil {
			return "", fmt.Errorf("create update directory: %w", err)
		}
		return dir, nil
	}
	dir := ""
	if x := os.Getenv("XDG_DOWNLOAD_DIR"); goos == "linux" && filepath.IsAbs(x) {
		dir = x
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("home directory: %w", err)
		}
		dir = filepath.Join(home, "Downloads")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// removeOldUpdateDirs removes the per-download directories left in base, best
// effort: an installer still running keeps its file, and its directory, in use.
func removeOldUpdateDirs(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), updateDirPrefix) {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
}

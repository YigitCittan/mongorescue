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
	// UpdateCheckPath checks for a new release now and answers with the status once
	// the check is done (POST; see Updater.CheckNow).
	UpdateCheckPath = "/desktop/update/check"
	// UpdateRemoveLegacyPath starts the uninstaller of a per-machine copy left in
	// Program Files (POST; Windows).
	UpdateRemoveLegacyPath = "/desktop/update/remove-legacy"
	// UpdateHeader must be "1" on the POST requests: a page cannot send a custom
	// header cross-origin without a CORS preflight, which is never granted.
	UpdateHeader = "X-MongoRescue-Desktop"
)

// Check timing of the updater.
const (
	// UpdateCheckInterval is how often the updater checks again after the startup
	// check while the app runs, which can be for days in the tray: GitHub allows
	// 60 unauthenticated API requests an hour per address.
	UpdateCheckInterval = 30 * time.Minute
	// UpdateCheckMaxBackoff bounds the wait after failed checks (offline, rate
	// limited), which doubles from twice UpdateCheckInterval with each failure in a
	// row; the first successful check returns to UpdateCheckInterval.
	UpdateCheckMaxBackoff = 6 * time.Hour
	// UpdateRecheckAge is how old the last check must be for Updater.WindowShown
	// to check again.
	UpdateRecheckAge = 5 * time.Minute
)

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
	// UpdateWaiting reports that the verified update waits for the running backups
	// and restores to finish before it is installed (Windows; see
	// UpdaterOptions.Busy).
	UpdateWaiting = "waiting"
	UpdateError   = "error"
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
	// ErrUpdaterNotStarted is returned by Install and CheckNow before Start.
	ErrUpdaterNotStarted = errors.New("updater not started")
	// ErrUpdatesDisabled is returned by CheckNow for a development build, which
	// never checks for updates.
	ErrUpdatesDisabled = errors.New("update checks are disabled for a development build")
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
	// MaxBackoff bounds the wait after failed checks; 0 means
	// UpdateCheckMaxBackoff. It is at least Interval.
	MaxBackoff time.Duration
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
	// Busy reports whether a backup or restore runs. On Windows the update waits,
	// as UpdateWaiting, until it reports false before it swaps the files or starts
	// the installer, since quitting would cancel the run; nil is never busy.
	Busy func() bool
	// IdlePoll is how often Busy is checked again while the update waits; 0 means
	// DefaultIdlePoll.
	IdlePoll time.Duration

	// fs replaces the file system calls of the swap in tests.
	fs *fileOps
	// clock replaces the time source of the check loop in tests.
	clock clock
}

// clock is the time source of the check loop.
type clock interface {
	Now() time.Time
	// NewTimer returns a channel that receives once d has passed, and a function
	// that stops the timer.
	NewTimer(d time.Duration) (<-chan time.Time, func() bool)
}

// realClock is the system clock.
type realClock struct{}

// Now implements clock.
func (realClock) Now() time.Time { return time.Now() }

// NewTimer implements clock.
func (realClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
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
	// Checking reports that a release check of the check loop runs (the startup,
	// periodic or requested one; see Updater.CheckNow).
	Checking bool   `json:"checking"`
	Error    string `json:"error"`
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

// Updater checks for a new desktop release at startup and then every Interval,
// backing off after failures, checks on request (CheckNow) and installs an update
// on request. All checks run in one loop. Its goroutines are bound to the context
// passed to Start; Wait returns once they have finished. It is safe for concurrent
// use.
type Updater struct {
	opts    UpdaterOptions
	fs      fileOps
	goos    string
	enabled bool
	clock   clock
	trigger chan struct{} // wakes the check loop for a requested check

	wg  sync.WaitGroup // the check loop
	ops sync.WaitGroup // installs, reveals and uninstaller starts

	mu     sync.Mutex
	ctx    context.Context // set by Start
	status UpdateStatus
	result update.Result
	path   string // the verified download, once ready
	legacy LegacyStatus

	round     *checkRound // the requested check, until it is done
	checking  bool        // a check of the loop runs
	lastCheck time.Time   // when the last check of the loop was done
	failures  int         // failed checks in a row
	next      time.Time   // when the loop checks next
	// limited is when GitHub accepts requests again after a rate limit, and
	// limitErr the error of that check: checks before then do not ask GitHub.
	limited  time.Time
	limitErr error
}

// checkRound is a requested check shared by every request made until it is done.
type checkRound struct {
	done     chan struct{} // closed once the check is done
	err      error         // the check's error, set before done is closed
	requests int           // the requests it serves
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
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = UpdateCheckMaxBackoff
	}
	opts.MaxBackoff = max(opts.MaxBackoff, opts.Interval)
	if opts.clock == nil {
		opts.clock = realClock{}
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
	if opts.IdlePoll <= 0 {
		opts.IdlePoll = DefaultIdlePoll
	}
	goos := opts.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	fsOps := osFileOps()
	if opts.fs != nil {
		fsOps = *opts.fs
	}
	u := &Updater{opts: opts, fs: fsOps, goos: goos, clock: opts.clock, trigger: make(chan struct{}, 1),
		status: UpdateStatus{Current: opts.Version, State: UpdateIdle, Action: ActionReveal}}
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
// Interval (see UpdaterOptions.MaxBackoff for failures), and one whenever CheckNow
// asks. Later downloads are bound to ctx too. It does nothing for a development
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

// run checks now, then again after the wait each check returns or when CheckNow
// asks, until ctx ends.
func (u *Updater) run(ctx context.Context) {
	wait := u.check(ctx, true)
	for {
		c, stop := u.clock.NewTimer(wait)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-c:
		case <-u.trigger:
			stop()
		}
		wait = u.check(ctx, false)
	}
}

// check runs a release check and returns the wait until the next one. A failure
// (typically offline) is logged at info level and keeps the previous result. A
// later check is skipped, and its result dropped, while a check or a download runs
// or a download is ready; until a rate limit ends, it fails at once with the rate
// limit's error. Either way it completes the requested check (see CheckNow).
func (u *Updater) check(ctx context.Context, startup bool) time.Duration {
	u.mu.Lock()
	if !startup && u.busyLocked() {
		u.finishRoundLocked(nil)
		u.next = u.clock.Now().Add(u.opts.Interval)
		u.mu.Unlock()
		return u.opts.Interval
	}
	if now := u.clock.Now(); now.Before(u.limited) {
		u.finishRoundLocked(u.limitErr)
		wait := max(u.next.Sub(now), u.limited.Sub(now))
		u.mu.Unlock()
		return wait
	}
	u.checking = true
	u.mu.Unlock()

	res, err := u.opts.Source.Check(ctx, u.opts.Version)

	u.mu.Lock()
	defer u.mu.Unlock()
	u.checking = false
	if startup {
		u.status.State = UpdateIdle
	}
	wait := u.backoffLocked(err)
	u.finishRoundLocked(err)
	if err != nil {
		u.opts.Logger.Info("update check failed", slog.Any("error", err), slog.Duration("next_check", wait))
		return wait
	}
	if !startup && u.busyLocked() {
		return wait
	}
	u.apply(res)
	return wait
}

// backoffLocked records a check's outcome and returns the wait until the next
// check: Interval after a success, and after failures in a row twice Interval,
// doubled with each further failure, up to MaxBackoff. A rate limit that ends
// later extends the wait to its end, up to MaxBackoff. u.mu must be held.
func (u *Updater) backoffLocked(err error) time.Duration {
	now := u.clock.Now()
	u.lastCheck = now
	if err == nil {
		u.failures, u.limited, u.limitErr = 0, time.Time{}, nil
		u.next = now.Add(u.opts.Interval)
		return u.opts.Interval
	}
	u.failures++
	wait := u.opts.Interval
	for i := 0; i < u.failures && wait < u.opts.MaxBackoff; i++ {
		wait *= 2
	}
	var rl *update.RateLimitError
	if errors.As(err, &rl) && rl.Reset.After(now) {
		wait = max(wait, rl.Reset.Sub(now))
		u.limited, u.limitErr = now.Add(min(rl.Reset.Sub(now), u.opts.MaxBackoff)), err
	}
	wait = min(wait, u.opts.MaxBackoff)
	u.next = now.Add(wait)
	return wait
}

// finishRoundLocked completes the requested check, if any, with err. u.mu must be
// held.
func (u *Updater) finishRoundLocked(err error) {
	select {
	case <-u.trigger:
	default:
	}
	if u.round == nil {
		return
	}
	u.round.err = err
	close(u.round.done)
	u.round = nil
}

// CheckNow asks the check loop for a release check now and waits until it is done
// or ctx ends; it returns the check's error. Requests made while a check is asked
// for or runs share that check: no second check, and no second loop, is started.
// A check is skipped, returning nil, while an update is downloaded or installed;
// until a rate limit ends it returns the rate limit's error without asking GitHub.
// It returns ErrUpdaterNotStarted before Start and ErrUpdatesDisabled for a
// development build. It is safe to call from any goroutine.
func (u *Updater) CheckNow(ctx context.Context) error {
	u.mu.Lock()
	r, err := u.requestLocked()
	loop := u.ctx
	u.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	case <-loop.Done():
		return fmt.Errorf("updater stopped: %w", loop.Err())
	}
}

// WindowShown asks for a check without waiting for it (see CheckNow) when the
// last check was done more than UpdateRecheckAge ago and none runs, and reports
// whether it asked. Call it when the window is shown again, such as from the tray
// or by a second launch. It does not block.
func (u *Updater) WindowShown() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.checking || u.round != nil || u.lastCheck.IsZero() || u.clock.Now().Sub(u.lastCheck) <= UpdateRecheckAge {
		return false
	}
	_, err := u.requestLocked()
	return err == nil
}

// requestLocked returns the requested check, asking the loop for one when none is
// pending. u.mu must be held.
func (u *Updater) requestLocked() (*checkRound, error) {
	switch {
	case u.ctx == nil:
		return nil, ErrUpdaterNotStarted
	case !u.enabled:
		return nil, ErrUpdatesDisabled
	case u.ctx.Err() != nil:
		return nil, fmt.Errorf("updater stopped: %w", u.ctx.Err())
	}
	if u.round == nil {
		u.round = &checkRound{done: make(chan struct{})}
		select {
		case u.trigger <- struct{}{}:
		default:
		}
	}
	u.round.requests++
	return u.round, nil
}

// busyLocked is busy with u.mu held.
func (u *Updater) busyLocked() bool {
	switch u.status.State {
	case UpdateChecking, UpdateDownloading, UpdateReady, UpdateInstalling, UpdateRestarting, UpdateWaiting:
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
	s.Checking = u.checking
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
		u.status.State == UpdateInstalling || u.status.State == UpdateRestarting || u.status.State == UpdateWaiting:
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
	if err = u.waitIdle(ctx, file.Path); err != nil {
		u.fail(err)
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
	if err = u.waitIdle(ctx, file.Path); err != nil {
		cleanup()
		u.fail(err)
		return
	}
	u.setState(UpdateInstalling, file.Path)
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

// waitIdle waits, as UpdateWaiting, until no backup or restore runs (see
// UpdaterOptions.Busy), or returns an error when ctx ends first.
func (u *Updater) waitIdle(ctx context.Context, path string) error {
	busy := u.opts.Busy
	if busy == nil || !busy() {
		return nil
	}
	u.opts.Logger.Info("the update waits for the running backups and restores to finish")
	u.setState(UpdateWaiting, path)
	if err := WaitIdle(ctx, busy, u.opts.IdlePoll); err != nil {
		return fmt.Errorf("wait for the running backups and restores: %w", err)
	}
	u.opts.Logger.Info("no backup or restore runs any more, installing the update")
	return nil
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

// OpenReleasePage opens the release page, when known, in the system browser.
func (u *Updater) OpenReleasePage() {
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
		case UpdateInstallPath, UpdateReleasePagePath, UpdateRemoveLegacyPath, UpdateCheckPath:
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
		case UpdateCheckPath:
			u.serveCheck(w, r)
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
			u.OpenReleasePage()
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSON(w, http.StatusOK, u.Status())
		}
	})
}

// serveCheck answers POST UpdateCheckPath: it checks now (CheckNow, bound to the
// request) and writes the status; 409 when the updater does not check (not
// started, a development build, stopped or the request ended) and 502 when the
// check failed.
func (u *Updater) serveCheck(w http.ResponseWriter, r *http.Request) {
	err := u.CheckNow(r.Context())
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, u.Status())
	case errors.Is(err, ErrUpdaterNotStarted), errors.Is(err, ErrUpdatesDisabled), errors.Is(err, context.Canceled):
		writeJSONError(w, http.StatusConflict, err.Error())
	default:
		writeJSONError(w, http.StatusBadGateway, err.Error())
	}
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

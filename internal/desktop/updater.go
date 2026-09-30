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
	// UpdateHeader must be "1" on the POST requests: a page cannot send a custom
	// header cross-origin without a CORS preflight, which is never granted.
	UpdateHeader = "X-MongoRescue-Desktop"
)

// UpdateCheckInterval is how often the updater checks again after the startup
// check, while the app runs.
const UpdateCheckInterval = 6 * time.Hour

// Update states reported in UpdateStatus.State.
const (
	UpdateIdle        = "idle"
	UpdateChecking    = "checking"
	UpdateDownloading = "downloading"
	UpdateReady       = "ready"
	// UpdateInstalling reports that the verified installer is being started, or
	// runs (Windows): the app quits and the installer starts the new version.
	UpdateInstalling = "installing"
	UpdateError      = "error"
)

// Update actions reported in UpdateStatus.Action: what Install does with the file.
const (
	// ActionLaunch runs the installer silently and quits the app; the installer
	// starts the new version once it is done (Windows).
	ActionLaunch = "launch"
	// ActionReveal saves the archive and shows it in the file manager (macOS, Linux).
	ActionReveal = "reveal"
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
	// prompt for the installer.
	ErrInstallerCancelled = errors.New("the installation was cancelled")
)

// silentInstallerArgs returns the installer's command-line arguments: /S runs the
// NSIS installer without its wizard, and /RELAUNCH makes it start the new version
// once the files are installed (see build/windows/installer/project.nsi).
func silentInstallerArgs() []string {
	return []string{"/S", "/RELAUNCH"}
}

// UpdateSource looks up and downloads releases; *update.Checker implements it.
type UpdateSource interface {
	Check(ctx context.Context, current string) (update.Result, error)
	Download(ctx context.Context, res update.Result, dir string) (update.File, error)
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
	// Launch starts the downloaded installer with args (Windows). It returns once
	// the installer runs, or with an error when it could not be started, such as
	// ErrInstallerCancelled after a declined UAC prompt; nil means LaunchInstaller.
	Launch func(path string, args []string) error
	// Reveal shows the downloaded archive (macOS, Linux); nil means RevealFile.
	Reveal func(ctx context.Context, path string) error
	// Quit asks the app to quit once the installer runs, so the installer can
	// replace its files; it is not called when the installer did not start. It must
	// not block on the updater (Wait); nil does nothing.
	Quit func()
	// OpenURL opens the release page in the system browser; nil does nothing.
	OpenURL func(url string)
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
	// Action is ActionLaunch or ActionReveal.
	Action string `json:"action"`
	// File is the base name of the downloaded file once State is UpdateReady.
	File string `json:"file"`
}

// Updater checks for a new desktop release at startup and then every Interval, and
// installs it on request. Its goroutines are bound to the context passed to Start;
// Wait returns once they have finished. It is safe for concurrent use.
type Updater struct {
	opts    UpdaterOptions
	goos    string
	enabled bool

	wg  sync.WaitGroup // the check loop
	ops sync.WaitGroup // installs and reveals

	mu     sync.Mutex
	ctx    context.Context // set by Start
	status UpdateStatus
	result update.Result
	path   string // the verified download, once ready
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
	if opts.Launch == nil {
		opts.Launch = LaunchInstaller
	}
	if opts.Reveal == nil {
		opts.Reveal = RevealFile
	}
	goos := opts.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	u := &Updater{opts: opts, goos: goos, status: UpdateStatus{Current: opts.Version, State: UpdateIdle, Action: ActionReveal}}
	if goos == "windows" {
		u.status.Action = ActionLaunch
	}
	_, err := update.ParseVersion(opts.Version)
	u.enabled = err == nil
	return u
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
	case UpdateChecking, UpdateDownloading, UpdateReady, UpdateInstalling:
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

// Status returns the current update status.
func (u *Updater) Status() UpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}

// Install downloads and verifies the update in a goroutine bound to the Start
// context, then installs it: on Windows it starts the installer silently (/S
// /RELAUNCH, after the UAC prompt) and quits the app, and the installer starts the
// new version once it is done; if the installer does not start, the app keeps
// running and reports the error. Elsewhere it leaves the file in the download directory and reveals it. After a
// failure, or when the last check found no installable file, it checks for the
// release again first. Progress and errors are reported through Status. Once
// ready, Install reveals the file again. Install never replaces files itself: the
// installer, or the user unpacking the archive, installs the app together with its
// bundled MongoDB Database Tools (tools/, or Contents/Resources/tools in the .app).
func (u *Updater) Install() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.ctx == nil:
		return ErrUpdaterNotStarted
	case u.ctx.Err() != nil:
		return fmt.Errorf("updater stopped: %w", u.ctx.Err())
	case u.status.State == UpdateChecking || u.status.State == UpdateDownloading || u.status.State == UpdateInstalling:
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
	u.status.State, u.status.Error, u.status.File = UpdateDownloading, "", ""
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
	dir, err := u.opts.Dir(u.goos)
	if err != nil {
		u.fail(fmt.Errorf("update directory: %w", err))
		return
	}
	file, err := u.opts.Source.Download(ctx, res, dir)
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
	u.installing(file.Path)
	if err := verifyAndLaunch(file, silentInstallerArgs(), u.opts.Launch); err != nil {
		u.fail(fmt.Errorf("start the installer: %w", err))
		return
	}
	u.opts.Logger.Info("installer started, quitting for the update", slog.String("path", file.Path))
	if u.opts.Quit != nil {
		u.opts.Quit()
	}
}

// ready records the verified download at path.
func (u *Updater) ready(path string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.path = path
	u.status.State, u.status.File = UpdateReady, filepath.Base(path)
}

// installing records that the verified installer at path is being started.
func (u *Updater) installing(path string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.path = path
	u.status.State, u.status.File = UpdateInstalling, filepath.Base(path)
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
	if _, err := io.Copy(h, f); err != nil {
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
		case UpdateInstallPath, UpdateReleasePagePath:
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

package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"

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

// Update states reported in UpdateStatus.State.
const (
	UpdateIdle        = "idle"
	UpdateChecking    = "checking"
	UpdateDownloading = "downloading"
	UpdateReady       = "ready"
	UpdateError       = "error"
)

// Update actions reported in UpdateStatus.Action: what Install does with the file.
const (
	// ActionLaunch runs the installer and quits the app (Windows).
	ActionLaunch = "launch"
	// ActionReveal saves the archive and shows it in the file manager (macOS, Linux).
	ActionReveal = "reveal"
)

// Updater errors.
var (
	// ErrNoUpdate is returned by Install when no newer version is available.
	ErrNoUpdate = errors.New("no update available")
	// ErrUpdateBusy is returned by Install while a check or a download runs.
	ErrUpdateBusy = errors.New("update check or download in progress")
	// ErrUpdaterNotStarted is returned by Install before Start.
	ErrUpdaterNotStarted = errors.New("updater not started")
)

// UpdateSource looks up and downloads releases; *update.Checker implements it.
type UpdateSource interface {
	Check(ctx context.Context, current string) (update.Result, error)
	Download(ctx context.Context, res update.Result, dir string) (string, error)
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
	// Dir returns the directory the update is downloaded to; nil means UpdateDir.
	Dir func(goos string) (string, error)
	// Launch starts the downloaded installer (Windows); nil means LaunchInstaller.
	Launch func(path string) error
	// Reveal shows the downloaded archive (macOS, Linux); nil means RevealFile.
	Reveal func(ctx context.Context, path string) error
	// Quit asks the app to quit once the installer runs. It must not block on the
	// updater (Wait); nil does nothing.
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
	Notes     string `json:"notes"`
	HTMLURL   string `json:"html_url"`
	State     string `json:"state"`
	Error     string `json:"error"`
	// Action is ActionLaunch or ActionReveal.
	Action string `json:"action"`
	// File is the base name of the downloaded file once State is UpdateReady.
	File string `json:"file"`
}

// Updater checks for a new desktop release once at startup and installs it on
// request. Its goroutines are bound to the context passed to Start; Wait returns
// once they have finished. It is safe for concurrent use.
type Updater struct {
	opts    UpdaterOptions
	goos    string
	enabled bool

	wg sync.WaitGroup

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

// Start begins the startup check in a goroutine bound to ctx; later downloads are
// bound to ctx too. It does nothing for a development version or when called again.
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
		u.check(ctx)
	}()
}

// Wait blocks until the updater's goroutines have returned. Cancel the context
// passed to Start first.
func (u *Updater) Wait() {
	u.wg.Wait()
}

// check runs the release check. A failure (typically offline) is logged at info
// level and leaves no update available.
func (u *Updater) check(ctx context.Context) {
	res, err := u.opts.Source.Check(ctx, u.opts.Version)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status.State = UpdateIdle
	if err != nil {
		u.opts.Logger.Info("update check failed", slog.Any("error", err))
		return
	}
	u.result = res
	u.status.Latest = res.Latest.String()
	u.status.Available = res.Available
	u.status.Mandatory = res.Mandatory
	u.status.HTMLURL = res.HTMLURL
	if res.Available {
		u.status.Notes = res.Notes
		u.opts.Logger.Info("update available", slog.String("current", u.opts.Version),
			slog.String("latest", u.status.Latest), slog.Bool("mandatory", res.Mandatory))
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
// context, then installs it: on Windows it launches the installer and quits the app,
// elsewhere it leaves the file in the download directory and reveals it. Progress
// and errors are reported through Status. Once ready, Install reveals the file
// again.
func (u *Updater) Install() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.ctx == nil:
		return ErrUpdaterNotStarted
	case u.ctx.Err() != nil:
		return fmt.Errorf("updater stopped: %w", u.ctx.Err())
	case u.status.State == UpdateChecking || u.status.State == UpdateDownloading:
		return ErrUpdateBusy
	case !u.status.Available:
		return ErrNoUpdate
	}
	ctx, res, path := u.ctx, u.result, u.path
	if u.status.State == UpdateReady && path != "" && u.goos != "windows" {
		u.wg.Add(1)
		go func() {
			defer u.wg.Done()
			u.reveal(ctx, path)
		}()
		return nil
	}
	u.status.State, u.status.Error, u.status.File = UpdateDownloading, "", ""
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.install(ctx, res)
	}()
	return nil
}

// install downloads, verifies and installs res.
func (u *Updater) install(ctx context.Context, res update.Result) {
	dir, err := u.opts.Dir(u.goos)
	if err != nil {
		u.fail(fmt.Errorf("update directory: %w", err))
		return
	}
	path, err := u.opts.Source.Download(ctx, res, dir)
	if err != nil {
		u.fail(err)
		return
	}
	u.mu.Lock()
	u.path = path
	u.status.State, u.status.File = UpdateReady, filepath.Base(path)
	u.mu.Unlock()
	u.opts.Logger.Info("update downloaded and verified", slog.String("path", path))

	if u.goos != "windows" {
		u.reveal(ctx, path)
		return
	}
	if err := u.opts.Launch(path); err != nil {
		u.fail(fmt.Errorf("start the installer: %w", err))
		return
	}
	u.opts.Logger.Info("installer started, quitting", slog.String("path", path))
	if u.opts.Quit != nil {
		u.opts.Quit()
	}
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

// UpdateDir returns, creating it if needed, the directory an update for goos is
// downloaded to: a MongoRescue-update directory in the temporary directory on
// Windows, where the installer runs from it, and the user's Downloads directory
// elsewhere, where the user unpacks the archive ($XDG_DOWNLOAD_DIR on Linux when it
// is an absolute path).
func UpdateDir(goos string) (string, error) {
	var dir string
	switch goos {
	case "windows":
		dir = filepath.Join(os.TempDir(), "MongoRescue-update")
	default:
		if x := os.Getenv("XDG_DOWNLOAD_DIR"); goos == "linux" && filepath.IsAbs(x) {
			dir = x
			break
		}
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

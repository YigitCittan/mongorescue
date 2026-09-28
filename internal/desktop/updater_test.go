package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/update"
)

// fakeSource is an UpdateSource with canned results. Downloads wait for release
// when it is not nil.
type fakeSource struct {
	res      update.Result
	checkErr error
	dlErr    error
	release  chan struct{}
	badSum   bool

	mu      sync.Mutex
	dlDirs  []string
	current string
	checks  int
}

func (f *fakeSource) Check(_ context.Context, current string) (update.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = current
	f.checks++
	return f.res, f.checkErr
}

// set replaces the canned check result.
func (f *fakeSource) set(res update.Result, checkErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.res, f.checkErr = res, checkErr
}

// checkCount returns the number of checks so far.
func (f *fakeSource) checkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

// Download writes the asset into dir and returns it with its SHA-256 (a wrong one
// when badSum is set).
func (f *fakeSource) Download(ctx context.Context, res update.Result, dir string) (update.File, error) {
	f.mu.Lock()
	f.dlDirs = append(f.dlDirs, dir)
	f.mu.Unlock()
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return update.File{}, ctx.Err()
		}
	}
	if f.dlErr != nil {
		return update.File{}, f.dlErr
	}
	p := filepath.Join(dir, res.Asset.Name)
	content := []byte("installer " + res.Latest.String())
	if err := os.WriteFile(p, content, 0o600); err != nil {
		return update.File{}, err
	}
	sum := sha256.Sum256(content)
	if f.badSum {
		sum = sha256.Sum256([]byte("swapped"))
	}
	return update.File{Path: p, SHA256: sum[:]}, nil
}

// recorder records the calls of the injected install functions.
type recorder struct {
	dir string // the download directory

	mu        sync.Mutex
	launched  []string
	revealed  []string
	quits     int
	opened    []string
	launchErr error
}

// newRecorder returns a recorder with a temporary download directory.
func newRecorder(t *testing.T) *recorder {
	return &recorder{dir: t.TempDir()}
}

func (r *recorder) options(src UpdateSource, version, goos string) UpdaterOptions {
	return UpdaterOptions{
		Source:  src,
		Version: version,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		GOOS:    goos,
		Dir:     func(string) (string, error) { return r.dir, nil },
		Launch: func(p string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.launched = append(r.launched, p)
			return r.launchErr
		},
		Reveal: func(_ context.Context, p string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.revealed = append(r.revealed, p)
			return nil
		},
		Quit: func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.quits++
		},
		OpenURL: func(u string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.opened = append(r.opened, u)
		},
	}
}

func available(mandatory bool) update.Result {
	latest := update.Version{Major: 1, Minor: 1}
	if mandatory {
		latest = update.Version{Major: 2}
	}
	return update.Result{
		Current:      update.Version{Major: 1},
		Latest:       latest,
		Available:    true,
		Mandatory:    mandatory,
		Installable:  true,
		Notes:        "## Added\n- x",
		HTMLURL:      "https://github.com/YigitCittan/mongorescue/releases/tag/v" + latest.String(),
		Asset:        update.Asset{Name: "MongoRescue-desktop_" + latest.String() + "_linux_amd64.tar.gz", URL: "https://github.com/x"},
		ChecksumsURL: "https://github.com/sums",
	}
}

// notInstallable returns an available release without files for this platform.
func notInstallable() update.Result {
	res := available(false)
	res.Installable, res.Asset, res.ChecksumsURL = false, update.Asset{}, ""
	return res
}

// started returns an updater whose startup check has finished.
func started(t *testing.T, opts UpdaterOptions) (*Updater, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	u := NewUpdater(opts)
	u.Start(ctx)
	waitState(t, u, func(s UpdateStatus) bool { return s.State != UpdateChecking })
	t.Cleanup(func() {
		cancel()
		u.Wait()
	})
	return u, cancel
}

func TestUpdaterDevVersionSkipsCheck(t *testing.T) {
	src := &fakeSource{res: available(true)}
	r := newRecorder(t)
	u, _ := started(t, r.options(src, "dev", "linux"))
	if s := u.Status(); s.State != UpdateIdle || s.Available || s.Current != "dev" {
		t.Fatalf("status = %+v", s)
	}
	if src.current != "" {
		t.Error("dev build checked for updates")
	}
	if err := u.Install(); !errors.Is(err, ErrNoUpdate) {
		t.Errorf("Install = %v", err)
	}
}

func TestUpdaterCheckFailureLeavesNoUpdate(t *testing.T) {
	src := &fakeSource{checkErr: errors.New("offline")}
	r := newRecorder(t)
	u, _ := started(t, r.options(src, "1.0.0", "linux"))
	if s := u.Status(); s.State != UpdateIdle || s.Available || s.Error != "" {
		t.Fatalf("status = %+v", s)
	}
	if src.current != "1.0.0" {
		t.Errorf("checked %q", src.current)
	}
}

func TestUpdaterBeforeStart(t *testing.T) {
	u := NewUpdater(UpdaterOptions{Version: "1.0.0", Source: &fakeSource{}})
	if err := u.Install(); !errors.Is(err, ErrUpdaterNotStarted) {
		t.Errorf("Install = %v", err)
	}
	if s := u.Status(); s.State != UpdateIdle {
		t.Errorf("state = %q", s.State)
	}
}

func TestUpdaterRevealsOnUnix(t *testing.T) {
	src := &fakeSource{res: available(false), release: make(chan struct{})}
	r := newRecorder(t)
	u, _ := started(t, r.options(src, "1.0.0", "darwin"))
	s := u.Status()
	if !s.Available || s.Mandatory || s.Latest != "1.1.0" || s.Notes == "" || s.Action != ActionReveal {
		t.Fatalf("status = %+v", s)
	}
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	if st := u.Status(); st.State != UpdateDownloading {
		t.Errorf("state = %q", st.State)
	}
	if err := u.Install(); !errors.Is(err, ErrUpdateBusy) {
		t.Errorf("second Install = %v", err)
	}
	close(src.release)
	u.ops.Wait()
	s = u.Status()
	if s.State != UpdateReady || s.File != "MongoRescue-desktop_1.1.0_linux_amd64.tar.gz" || s.Error != "" {
		t.Fatalf("status = %+v", s)
	}
	want := filepath.Join(r.dir, s.File)
	if len(r.revealed) != 1 || r.revealed[0] != want || len(r.launched) != 0 || r.quits != 0 {
		t.Fatalf("revealed %v launched %v quits %d", r.revealed, r.launched, r.quits)
	}
	// Install once ready reveals the file again without downloading it again.
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	u.ops.Wait()
	if len(r.revealed) != 2 || len(src.dlDirs) != 1 {
		t.Errorf("revealed %v downloads %v", r.revealed, src.dlDirs)
	}
}

func TestUpdaterLaunchesOnWindows(t *testing.T) {
	src := &fakeSource{res: available(true)}
	r := newRecorder(t)
	u, _ := started(t, r.options(src, "1.0.0", "windows"))
	if s := u.Status(); !s.Mandatory || s.Action != ActionLaunch {
		t.Fatalf("status = %+v", s)
	}
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	u.ops.Wait()
	if len(r.launched) != 1 || r.quits != 1 || len(r.revealed) != 0 {
		t.Fatalf("launched %v quits %d revealed %v", r.launched, r.quits, r.revealed)
	}
	if s := u.Status(); s.State != UpdateReady {
		t.Errorf("state = %q", s.State)
	}
}

func TestUpdaterSurfacesErrors(t *testing.T) {
	src := &fakeSource{res: available(false), dlErr: update.ErrChecksumMismatch}
	r := newRecorder(t)
	u, _ := started(t, r.options(src, "1.0.0", "windows"))
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	u.ops.Wait()
	if s := u.Status(); s.State != UpdateError || !strings.Contains(s.Error, "checksum mismatch") {
		t.Fatalf("status = %+v", s)
	}
	if len(r.launched) != 0 || r.quits != 0 {
		t.Fatal("installer launched after a failed download")
	}

	src.dlErr = nil
	r.launchErr = errors.New("denied")
	if err := u.Install(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	u.ops.Wait()
	if s := u.Status(); s.State != UpdateError || !strings.Contains(s.Error, "denied") || r.quits != 0 {
		t.Fatalf("status = %+v quits %d", s, r.quits)
	}
}

func TestUpdaterStopsWithContext(t *testing.T) {
	src := &fakeSource{res: available(false), release: make(chan struct{})}
	r := newRecorder(t)
	u, cancel := started(t, r.options(src, "1.0.0", "linux"))
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	cancel()
	u.Wait()
	if s := u.Status(); s.State != UpdateError {
		t.Errorf("state = %q", s.State)
	}
	if err := u.Install(); !errors.Is(err, context.Canceled) {
		t.Errorf("Install after stop = %v", err)
	}
}

func TestUpdaterHandler(t *testing.T) {
	src := &fakeSource{res: available(false)}
	r := newRecorder(t)
	u, _ := started(t, r.options(src, "1.0.0", "linux"))
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := u.Handler(next)
	do := func(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
		return serveUpdate(h, method, path, hdr)
	}
	desktopHdr := map[string]string{UpdateHeader: "1", "Origin": "http://wails.localhost"}

	rec := do(http.MethodGet, UpdatePath, map[string]string{"Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET status = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"current", "latest", "available", "mandatory", "installable", "notes", "html_url", "state", "error"} {
		if _, ok := got[k]; !ok {
			t.Errorf("status lacks %q: %s", k, rec.Body)
		}
	}
	if got["latest"] != "1.1.0" || got["available"] != true || got["state"] != UpdateIdle {
		t.Errorf("status = %s", rec.Body)
	}

	forbidden := []struct {
		method, path string
		hdr          map[string]string
	}{
		{http.MethodGet, UpdatePath, map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{http.MethodGet, UpdatePath, map[string]string{"Origin": "https://evil.example"}},
		{http.MethodPost, UpdateInstallPath, map[string]string{"Origin": "http://wails.localhost"}},
		{http.MethodPost, UpdateInstallPath, map[string]string{UpdateHeader: "true"}},
		{http.MethodPost, UpdateInstallPath, map[string]string{UpdateHeader: "1", "Origin": "https://evil.example"}},
		{http.MethodPost, UpdateReleasePagePath, nil},
	}
	for _, c := range forbidden {
		if rec := do(c.method, c.path, c.hdr); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s %v = %d; want 403", c.method, c.path, c.hdr, rec.Code)
		}
	}
	if rec := do(http.MethodGet, UpdateInstallPath, nil); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("GET install = %d", rec.Code)
	}
	if rec := do(http.MethodPost, UpdatePath, desktopHdr); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d", rec.Code)
	}
	if rec := do(http.MethodGet, "/api/v1/status", nil); rec.Code != http.StatusTeapot {
		t.Errorf("other path = %d; want next", rec.Code)
	}
	if len(src.dlDirs) != 0 || len(r.opened) != 0 {
		t.Fatal("a refused request had an effect")
	}

	if rec := do(http.MethodPost, UpdateReleasePagePath, desktopHdr); rec.Code != http.StatusNoContent {
		t.Errorf("release page = %d", rec.Code)
	}
	if len(r.opened) != 1 || r.opened[0] != "https://github.com/YigitCittan/mongorescue/releases/tag/v1.1.0" {
		t.Errorf("opened %v", r.opened)
	}
	if rec := do(http.MethodPost, UpdateInstallPath, desktopHdr); rec.Code != http.StatusAccepted {
		t.Errorf("install = %d %s", rec.Code, rec.Body)
	}
	u.ops.Wait()
	if s := u.Status(); s.State != UpdateReady || len(r.revealed) != 1 {
		t.Errorf("status %+v revealed %v", s, r.revealed)
	}

	u2, _ := started(t, r.options(&fakeSource{}, "1.0.0", "linux"))
	if rec := serveUpdate(u2.Handler(next), http.MethodPost, UpdateInstallPath, desktopHdr); rec.Code != http.StatusConflict {
		t.Errorf("install without update = %d", rec.Code)
	}
}

// serveUpdate sends method path with the headers hdr to h.
func serveUpdate(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://wails.localhost"+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestUpdateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DOWNLOAD_DIR", "")
	dir, err := UpdateDir("darwin")
	if err != nil || dir != filepath.Join(home, "Downloads") {
		t.Fatalf("darwin: %q %v", dir, err)
	}
	if st, statErr := os.Stat(dir); statErr != nil || !st.IsDir() {
		t.Fatalf("not created: %v", statErr)
	}
	xdg := filepath.Join(home, "dl")
	t.Setenv("XDG_DOWNLOAD_DIR", xdg)
	if dir, err = UpdateDir("linux"); err != nil || dir != xdg {
		t.Errorf("linux: %q %v", dir, err)
	}
	t.Setenv("XDG_DOWNLOAD_DIR", "relative")
	if dir, err = UpdateDir("linux"); err != nil || dir != filepath.Join(home, "Downloads") {
		t.Errorf("linux relative: %q %v", dir, err)
	}
	t.Setenv("LocalAppData", filepath.Join(home, "local"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(cache, AppDirName, "updates")
	first, err := UpdateDir("windows")
	if err != nil || filepath.Dir(first) != base || !strings.HasPrefix(filepath.Base(first), updateDirPrefix) {
		t.Fatalf("windows: %q %v; want a new directory in %s", first, err, base)
	}
	if st, statErr := os.Stat(first); statErr != nil || (os.PathSeparator == '/' && st.Mode().Perm() != 0o700) {
		t.Fatalf("windows dir: %v %v", st, statErr)
	}
	if err = os.WriteFile(filepath.Join(first, "old.exe"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(base, "other")
	if err = os.Mkdir(keep, 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := UpdateDir("windows")
	if err != nil || second == first {
		t.Fatalf("second windows dir: %q %v", second, err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Errorf("earlier download directory kept: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("unrelated directory removed: %v", err)
	}
}

// waitState waits until the updater's status satisfies ok.
func waitState(t *testing.T, u *Updater, ok func(UpdateStatus) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok(u.Status()) {
		if time.Now().After(deadline) {
			t.Fatalf("status %+v never reached", u.Status())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestUpdaterRechecksWhenNotInstallable(t *testing.T) {
	src := &fakeSource{res: notInstallable()}
	r := newRecorder(t)
	u, _ := started(t, r.options(src, "1.0.0", "linux"))
	if s := u.Status(); !s.Available || s.Installable || s.Mandatory {
		t.Fatalf("status = %+v", s)
	}
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	u.ops.Wait()
	if s := u.Status(); s.State != UpdateError || s.Error != ErrNotInstallable.Error() || len(src.dlDirs) != 0 {
		t.Fatalf("status = %+v downloads %v", s, src.dlDirs)
	}
	if src.checkCount() != 2 {
		t.Errorf("checks = %d; want 2", src.checkCount())
	}

	// The files are attached now: "Try again" checks, downloads and reveals.
	src.set(available(false), nil)
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	u.ops.Wait()
	if s := u.Status(); s.State != UpdateReady || !s.Installable || len(r.revealed) != 1 || src.checkCount() != 3 {
		t.Fatalf("status = %+v revealed %v checks %d", s, r.revealed, src.checkCount())
	}

	// A failed check on retry is reported.
	src2 := &fakeSource{res: available(false), dlErr: errors.New("reset")}
	u2, _ := started(t, r.options(src2, "1.0.0", "linux"))
	if err := u2.Install(); err != nil {
		t.Fatal(err)
	}
	u2.ops.Wait()
	src2.set(update.Result{}, errors.New("offline"))
	if err := u2.Install(); err != nil {
		t.Fatal(err)
	}
	u2.ops.Wait()
	if s := u2.Status(); s.State != UpdateError || !strings.Contains(s.Error, "offline") {
		t.Errorf("status = %+v", s)
	}
}

func TestUpdaterChecksPeriodically(t *testing.T) {
	src := &fakeSource{res: update.Result{Latest: update.Version{Major: 1}}}
	r := newRecorder(t)
	opts := r.options(src, "1.0.0", "linux")
	opts.Interval = 5 * time.Millisecond
	u, _ := started(t, opts)
	if s := u.Status(); s.Available {
		t.Fatalf("status = %+v", s)
	}
	src.set(update.Result{}, errors.New("offline"))
	n := src.checkCount()
	waitState(t, u, func(UpdateStatus) bool { return src.checkCount() > n+1 })
	if s := u.Status(); s.Available || s.State != UpdateIdle || s.Error != "" {
		t.Fatalf("a failed periodic check changed the status: %+v", s)
	}
	src.set(available(true), nil)
	waitState(t, u, func(s UpdateStatus) bool { return s.Mandatory && s.Latest == "2.0.0" })
}

func TestUpdaterRefusesSwappedInstaller(t *testing.T) {
	src := &fakeSource{res: available(true), badSum: true}
	r := newRecorder(t)
	u, _ := started(t, r.options(src, "1.0.0", "windows"))
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	u.ops.Wait()
	if s := u.Status(); s.State != UpdateError || !strings.Contains(s.Error, "checksum mismatch") {
		t.Fatalf("status = %+v", s)
	}
	if len(r.launched) != 0 || r.quits != 0 {
		t.Fatal("a file that no longer matches its checksum was launched")
	}
}

func TestVerifyAndLaunch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "setup.exe")
	if err := os.WriteFile(p, []byte("installer"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("installer"))
	var launched []string
	launch := func(path string) error {
		launched = append(launched, path)
		return nil
	}
	if err := verifyAndLaunch(update.File{Path: p, SHA256: sum[:]}, launch); err != nil || len(launched) != 1 || launched[0] != p {
		t.Fatalf("err %v launched %v", err, launched)
	}
	if err := os.WriteFile(p, []byte("swapped"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyAndLaunch(update.File{Path: p, SHA256: sum[:]}, launch); !errors.Is(err, update.ErrChecksumMismatch) || len(launched) != 1 {
		t.Errorf("swapped file: err %v launched %v", err, launched)
	}
	if err := verifyAndLaunch(update.File{Path: p}, launch); !errors.Is(err, update.ErrChecksumMismatch) {
		t.Errorf("no checksum: %v", err)
	}
	if err := verifyAndLaunch(update.File{Path: p + ".missing", SHA256: sum[:]}, launch); err == nil {
		t.Error("missing file launched")
	}
}

func TestRevealCommand(t *testing.T) {
	p := filepath.Join("/home/u/Downloads", "a.tar.gz")
	cases := map[string]string{
		"darwin":  "open -R " + p,
		"linux":   "xdg-open " + filepath.Dir(p),
		"windows": "explorer /select," + p,
	}
	for goos, want := range cases {
		name, args := revealCommand(goos, p)
		if got := strings.Join(append([]string{name}, args...), " "); got != want {
			t.Errorf("%s: %q; want %q", goos, got, want)
		}
	}
}

func TestUpdateScript(t *testing.T) {
	s := UpdateScript()
	for _, want := range []string{`"status":"/desktop/update"`, `"install":"/desktop/update/install"`, `"release":"/desktop/update/release-page"`, `"X-MongoRescue-Desktop"`, "mongorescue_lang", "Update required", "Güncelleme gerekli"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	for _, bad := range []string{"__PATHS__", "__HEADER__", "innerHTML", "insertAdjacentHTML", "eval("} {
		if strings.Contains(s, bad) {
			t.Errorf("script contains %q", bad)
		}
	}
}

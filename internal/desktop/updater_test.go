package desktop

import (
	"context"
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

	"github.com/yigitcittan/mongorescue/internal/update"
)

// fakeSource is an UpdateSource with canned results. Downloads wait for release
// when it is not nil.
type fakeSource struct {
	res      update.Result
	checkErr error
	dlErr    error
	release  chan struct{}

	mu      sync.Mutex
	dlDirs  []string
	current string
}

func (f *fakeSource) Check(_ context.Context, current string) (update.Result, error) {
	f.mu.Lock()
	f.current = current
	f.mu.Unlock()
	return f.res, f.checkErr
}

func (f *fakeSource) Download(ctx context.Context, res update.Result, dir string) (string, error) {
	f.mu.Lock()
	f.dlDirs = append(f.dlDirs, dir)
	f.mu.Unlock()
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.dlErr != nil {
		return "", f.dlErr
	}
	return filepath.Join(dir, res.Asset.Name), nil
}

// recorder records the calls of the injected install functions.
type recorder struct {
	mu        sync.Mutex
	launched  []string
	revealed  []string
	quits     int
	opened    []string
	launchErr error
}

func (r *recorder) options(src UpdateSource, version, goos string) UpdaterOptions {
	return UpdaterOptions{
		Source:  src,
		Version: version,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		GOOS:    goos,
		Dir:     func(string) (string, error) { return "/downloads", nil },
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
		Current:   update.Version{Major: 1},
		Latest:    latest,
		Available: true,
		Mandatory: mandatory,
		Notes:     "## Added\n- x",
		HTMLURL:   "https://github.com/YigitCittan/mongorescue/releases/tag/v" + latest.String(),
		Asset:     update.Asset{Name: "MongoRescue-desktop_" + latest.String() + "_linux_amd64.tar.gz", URL: "https://github.com/x"},
	}
}

// started returns an updater whose startup check has finished.
func started(t *testing.T, opts UpdaterOptions) (*Updater, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	u := NewUpdater(opts)
	u.Start(ctx)
	u.Wait()
	t.Cleanup(func() {
		cancel()
		u.Wait()
	})
	return u, cancel
}

func TestUpdaterDevVersionSkipsCheck(t *testing.T) {
	src := &fakeSource{res: available(true)}
	var r recorder
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
	var r recorder
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
	var r recorder
	u, _ := started(t, r.options(src, "1.0.0", "darwin"))
	s := u.Status()
	if !s.Available || s.Mandatory || s.Latest != "1.1.0" || s.Notes == "" || s.Action != ActionReveal {
		t.Fatalf("status = %+v", s)
	}
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	if s := u.Status(); s.State != UpdateDownloading {
		t.Errorf("state = %q", s.State)
	}
	if err := u.Install(); !errors.Is(err, ErrUpdateBusy) {
		t.Errorf("second Install = %v", err)
	}
	close(src.release)
	u.Wait()
	s = u.Status()
	if s.State != UpdateReady || s.File != "MongoRescue-desktop_1.1.0_linux_amd64.tar.gz" || s.Error != "" {
		t.Fatalf("status = %+v", s)
	}
	want := filepath.Join("/downloads", s.File)
	if len(r.revealed) != 1 || r.revealed[0] != want || len(r.launched) != 0 || r.quits != 0 {
		t.Fatalf("revealed %v launched %v quits %d", r.revealed, r.launched, r.quits)
	}
	// Install once ready reveals the file again without downloading it again.
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	u.Wait()
	if len(r.revealed) != 2 || len(src.dlDirs) != 1 {
		t.Errorf("revealed %v downloads %v", r.revealed, src.dlDirs)
	}
}

func TestUpdaterLaunchesOnWindows(t *testing.T) {
	src := &fakeSource{res: available(true)}
	var r recorder
	u, _ := started(t, r.options(src, "1.0.0", "windows"))
	if s := u.Status(); !s.Mandatory || s.Action != ActionLaunch {
		t.Fatalf("status = %+v", s)
	}
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	u.Wait()
	if len(r.launched) != 1 || r.quits != 1 || len(r.revealed) != 0 {
		t.Fatalf("launched %v quits %d revealed %v", r.launched, r.quits, r.revealed)
	}
	if s := u.Status(); s.State != UpdateReady {
		t.Errorf("state = %q", s.State)
	}
}

func TestUpdaterSurfacesErrors(t *testing.T) {
	src := &fakeSource{res: available(false), dlErr: update.ErrChecksumMismatch}
	var r recorder
	u, _ := started(t, r.options(src, "1.0.0", "windows"))
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	u.Wait()
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
	u.Wait()
	if s := u.Status(); s.State != UpdateError || !strings.Contains(s.Error, "denied") || r.quits != 0 {
		t.Fatalf("status = %+v quits %d", s, r.quits)
	}
}

func TestUpdaterStopsWithContext(t *testing.T) {
	src := &fakeSource{res: available(false), release: make(chan struct{})}
	var r recorder
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
	var r recorder
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
	for _, k := range []string{"current", "latest", "available", "mandatory", "notes", "html_url", "state", "error"} {
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
	u.Wait()
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
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("not created: %v", err)
	}
	xdg := filepath.Join(home, "dl")
	t.Setenv("XDG_DOWNLOAD_DIR", xdg)
	if dir, err := UpdateDir("linux"); err != nil || dir != xdg {
		t.Errorf("linux: %q %v", dir, err)
	}
	t.Setenv("XDG_DOWNLOAD_DIR", "relative")
	if dir, err := UpdateDir("linux"); err != nil || dir != filepath.Join(home, "Downloads") {
		t.Errorf("linux relative: %q %v", dir, err)
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	if dir, err := UpdateDir("windows"); err != nil || dir != filepath.Join(tmp, "MongoRescue-update") {
		t.Errorf("windows: %q %v", dir, err)
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

package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/update"
)

// fakeSource is an UpdateSource with canned results. Downloads wait for release
// when it is not nil. A .zip asset gets the content zip.
type fakeSource struct {
	res      update.Result
	checkErr error
	dlErr    error
	release  chan struct{}
	badSum   bool
	zip      []byte

	mu      sync.Mutex
	gate    chan struct{} // checks wait for it when not nil
	dlDirs  []string
	current string
	checks  int
}

func (f *fakeSource) Check(ctx context.Context, current string) (update.Result, error) {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return update.Result{}, ctx.Err()
		}
	}
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

// DownloadAsset writes the asset into dir, reporting the progress in two steps,
// and returns it with its SHA-256 (a wrong one when badSum is set).
func (f *fakeSource) DownloadAsset(ctx context.Context, res update.Result, asset update.Asset, dir string, progress update.Progress) (update.File, error) {
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
	p := filepath.Join(dir, asset.Name)
	content := []byte("installer " + res.Latest.String())
	if strings.HasSuffix(asset.Name, ".zip") && f.zip != nil {
		content = f.zip
	}
	if progress != nil {
		progress(0, int64(len(content)))
		progress(int64(len(content))/2, int64(len(content)))
		progress(int64(len(content)), int64(len(content)))
	}
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
	exe string // the running executable

	mu        sync.Mutex
	writable  bool // whether exe's directory is writable (in-app update)
	launched  []string
	args      [][]string // the arguments of each launch
	revealed  []string
	quits     int
	opened    []string
	launchErr error
	started   []string   // the executables started (in-app update)
	startArgs [][]string // their arguments
	startErr  error
	legacy    string // the registered per-machine uninstaller
}

// newRecorder returns a recorder with a temporary download directory and a
// running executable in a temporary directory.
func newRecorder(t *testing.T) *recorder {
	return &recorder{dir: t.TempDir(), exe: filepath.Join(t.TempDir(), "MongoRescue.exe")}
}

func (r *recorder) options(src UpdateSource, version, goos string) UpdaterOptions {
	return UpdaterOptions{
		Source:     src,
		Version:    version,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		GOOS:       goos,
		Dir:        func(string) (string, error) { return r.dir, nil },
		Executable: func() (string, error) { return r.exe, nil },
		Writable: func(string) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.writable
		},
		StartApp: func(exe string, args []string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.started = append(r.started, exe)
			r.startArgs = append(r.startArgs, args)
			return r.startErr
		},
		Args: []string{"-data-dir", `D:\data`},
		PID:  4242,
		User: func() string { return `PC\tester` },
		LegacyUninstaller: func() string {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.legacy
		},
		Launch: func(p string, args []string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.launched = append(r.launched, p)
			r.args = append(r.args, args)
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
	opts := r.options(src, "1.0.0", "windows")
	var u *Updater
	var during UpdateStatus
	quitsDuring := -1
	launch := opts.Launch
	opts.Launch = func(p string, args []string) error {
		during = u.Status()
		r.mu.Lock()
		quitsDuring = r.quits
		r.mu.Unlock()
		return launch(p, args)
	}
	u, _ = started(t, opts)
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
	if got := strings.Join(r.args[0], "|"); got != `/S|/RELAUNCH=PC\tester|/WAITPID=4242` {
		t.Errorf("installer args = %q; want silent install, relaunch as the user and a wait for the app", got)
	}
	if len(r.started) != 0 {
		t.Errorf("started %v; an installer update must not start the app itself", r.started)
	}
	if during.State != UpdateInstalling || during.File != filepath.Base(r.launched[0]) || quitsDuring != 0 {
		t.Errorf("while launching: status %+v quits %d", during, quitsDuring)
	}
	if s := u.Status(); s.State != UpdateInstalling || s.Error != "" {
		t.Errorf("status = %+v", s)
	}
	if err := u.Install(); !errors.Is(err, ErrUpdateBusy) {
		t.Errorf("Install while installing = %v; want ErrUpdateBusy", err)
	}
}

func TestUpdaterKeepsRunningWhenInstallerDoesNotStart(t *testing.T) {
	src := &fakeSource{res: available(true)}
	r := newRecorder(t)
	r.launchErr = ErrInstallerCancelled
	u, _ := started(t, r.options(src, "1.0.0", "windows"))
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	u.ops.Wait()
	s := u.Status()
	if s.State != UpdateError || !strings.Contains(s.Error, ErrInstallerCancelled.Error()) || !s.Available || s.HTMLURL == "" {
		t.Fatalf("status = %+v", s)
	}
	if len(r.launched) != 1 || r.quits != 0 {
		t.Fatalf("launched %v quits %d; the app must not quit", r.launched, r.quits)
	}

	// "Try again" checks again, downloads and starts the installer.
	r.mu.Lock()
	r.launchErr = nil
	r.mu.Unlock()
	if err := u.Install(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	u.ops.Wait()
	if s := u.Status(); s.State != UpdateInstalling || len(r.launched) != 2 || r.quits != 1 {
		t.Fatalf("after retry: status %+v launched %v quits %d", s, r.launched, r.quits)
	}
}

// withPortable adds the Windows portable archive to res.
func withPortable(res update.Result) update.Result {
	res.Portable = update.Asset{Name: "MongoRescue-desktop_" + res.Latest.String() + "_windows_amd64_portable.zip", URL: "https://github.com/p"}
	return res
}

// inAppRecorder returns a recorder whose executable is an installed copy of
// version 1 in a writable directory.
func inAppRecorder(t *testing.T) *recorder {
	r := newRecorder(t)
	r.exe = installCopy(t, "1")
	r.writable = true
	return r
}

func TestUpdaterSwapsInPlace(t *testing.T) {
	src := &fakeSource{res: withPortable(available(true)), zip: portableZip(t, "2")}
	r := inAppRecorder(t)
	opts := r.options(src, "1.0.0", "windows")
	var u *Updater
	var atStart UpdateStatus
	quitsAtStart := -1
	src.release = make(chan struct{})
	start := opts.StartApp
	opts.StartApp = func(exe string, args []string) error {
		atStart = u.Status()
		r.mu.Lock()
		quitsAtStart = r.quits
		r.mu.Unlock()
		return start(exe, args)
	}
	u, _ = started(t, opts)
	if s := u.Status(); s.Action != ActionSwap || !s.Mandatory {
		t.Fatalf("status = %+v; want ActionSwap", s)
	}
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	if s := u.Status(); s.State != UpdateDownloading || s.Percent != 0 {
		t.Errorf("status = %+v", s)
	}
	close(src.release)
	u.ops.Wait()

	if len(r.started) != 1 || r.started[0] != r.exe || r.quits != 1 || len(r.launched) != 0 {
		t.Fatalf("started %v quits %d launched %v", r.started, r.quits, r.launched)
	}
	if got := strings.Join(r.startArgs[0], "|"); got != `-data-dir|D:\data|--after-update=4242` {
		t.Errorf("relaunch args = %q", got)
	}
	if atStart.State != UpdateRestarting || quitsAtStart != 0 {
		t.Errorf("when starting the new version: status %+v quits %d", atStart, quitsAtStart)
	}
	if s := u.Status(); s.State != UpdateRestarting || s.Error != "" || s.Percent != 100 {
		t.Errorf("status = %+v", s)
	}
	assertFiles(t, filepath.Dir(r.exe), map[string]string{
		"MongoRescue.exe":            "exe 2",
		"MongoRescue.exe.old":        "exe 1",
		"tools/mongodump.exe":        "dump 2",
		"tools/mongorestore.exe":     "restore 2",
		"tools/LICENSE.md":           "license",
		"tools/THIRD-PARTY-NOTICES":  "notices",
		"tools.old/mongodump.exe":    "dump 1",
		"tools.old/mongorestore.exe": "restore 1",
	})
	if err := u.Install(); !errors.Is(err, ErrUpdateBusy) {
		t.Errorf("Install while restarting = %v; want ErrUpdateBusy", err)
	}
}

func TestUpdaterWaitsForRunningBackups(t *testing.T) {
	for _, writable := range []bool{true, false} {
		t.Run(fmt.Sprint("writable=", writable), func(t *testing.T) {
			src := &fakeSource{res: withPortable(available(false)), zip: portableZip(t, "2")}
			r := inAppRecorder(t)
			r.writable = writable
			var running atomic.Bool
			running.Store(true)
			opts := r.options(src, "1.0.0", "windows")
			opts.Busy = running.Load
			opts.IdlePoll = time.Millisecond
			u, _ := started(t, opts)
			if err := u.Install(); err != nil {
				t.Fatal(err)
			}
			waitState(t, u, func(s UpdateStatus) bool { return s.State == UpdateWaiting })
			r.mu.Lock()
			early := len(r.started) + len(r.launched) + r.quits
			r.mu.Unlock()
			if early != 0 {
				t.Fatal("the update went ahead while a backup runs")
			}
			if err := u.Install(); !errors.Is(err, ErrUpdateBusy) {
				t.Errorf("Install while waiting = %v", err)
			}
			running.Store(false)
			u.ops.Wait()
			if r.quits != 1 || len(r.started)+len(r.launched) != 1 {
				t.Fatalf("started %v launched %v quits %d", r.started, r.launched, r.quits)
			}
		})
	}
}

func TestUpdaterStopsWaitingWithContext(t *testing.T) {
	src := &fakeSource{res: withPortable(available(false)), zip: portableZip(t, "2")}
	r := inAppRecorder(t)
	opts := r.options(src, "1.0.0", "windows")
	opts.Busy = func() bool { return true }
	opts.IdlePoll = time.Millisecond
	u, cancel := started(t, opts)
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	waitState(t, u, func(s UpdateStatus) bool { return s.State == UpdateWaiting })
	cancel()
	u.Wait()
	if s := u.Status(); s.State != UpdateError || len(r.started) != 0 || r.quits != 0 {
		t.Fatalf("status %+v started %v quits %d", s, r.started, r.quits)
	}
	assertFiles(t, filepath.Dir(r.exe), map[string]string{
		"MongoRescue.exe":        "exe 1",
		"tools/mongodump.exe":    "dump 1",
		"tools/mongorestore.exe": "restore 1",
	})
}

func TestUpdaterReportsDownloadProgress(t *testing.T) {
	src := &fakeSource{res: withPortable(available(false)), zip: portableZip(t, "2")}
	r := inAppRecorder(t)
	opts := r.options(src, "1.0.0", "windows")
	var u *Updater
	var seen []int
	src.release = make(chan struct{})
	u, _ = started(t, opts)
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	close(src.release)
	u.ops.Wait()
	u.progress(50, 200)
	seen = append(seen, u.Status().Percent)
	u.progress(10, 0)
	seen = append(seen, u.Status().Percent)
	u.progress(300, 200)
	seen = append(seen, u.Status().Percent)
	if fmt.Sprint(seen) != "[25 -1 100]" {
		t.Errorf("percent = %v", seen)
	}
}

func TestUpdaterSwapRollsBackWhenTheNewVersionDoesNotStart(t *testing.T) {
	src := &fakeSource{res: withPortable(available(true)), zip: portableZip(t, "2")}
	r := inAppRecorder(t)
	r.startErr = errors.New("blocked by policy")
	u, _ := started(t, r.options(src, "1.0.0", "windows"))
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	u.ops.Wait()
	s := u.Status()
	if s.State != UpdateError || !strings.Contains(s.Error, "blocked by policy") || r.quits != 0 {
		t.Fatalf("status %+v quits %d; the app must keep running", s, r.quits)
	}
	// The running version is back in place and the staging directory is gone.
	assertFiles(t, filepath.Dir(r.exe), map[string]string{
		"MongoRescue.exe":        "exe 1",
		"tools/mongodump.exe":    "dump 1",
		"tools/mongorestore.exe": "restore 1",
	})

	// "Try again" works once the new version starts.
	r.mu.Lock()
	r.startErr = nil
	r.mu.Unlock()
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	u.ops.Wait()
	if s := u.Status(); s.State != UpdateRestarting || r.quits != 1 {
		t.Fatalf("after retry: %+v quits %d", s, r.quits)
	}
}

func TestUpdaterSwapFailures(t *testing.T) {
	cases := map[string]func(*fakeSource, *UpdaterOptions){
		"checksum mismatch": func(src *fakeSource, _ *UpdaterOptions) { src.badSum = true },
		"zip slip": func(src *fakeSource, _ *UpdaterOptions) {
			src.zip = zipBytes(t, zipEntry{name: "MongoRescue.exe", body: "exe 2"}, zipEntry{name: "../evil.exe", body: "x"})
		},
		"rename fails": func(_ *fakeSource, opts *UpdaterOptions) {
			calls := 0
			ops := failingOps(2, &calls)
			opts.fs = &ops
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			src := &fakeSource{res: withPortable(available(false)), zip: portableZip(t, "2")}
			r := inAppRecorder(t)
			opts := r.options(src, "1.0.0", "windows")
			setup(src, &opts)
			u, _ := started(t, opts)
			if err := u.Install(); err != nil {
				t.Fatal(err)
			}
			u.ops.Wait()
			if s := u.Status(); s.State != UpdateError || s.Error == "" {
				t.Fatalf("status = %+v", s)
			}
			if len(r.started) != 0 || r.quits != 0 || len(r.launched) != 0 {
				t.Fatalf("started %v quits %d launched %v", r.started, r.quits, r.launched)
			}
			assertFiles(t, filepath.Dir(r.exe), map[string]string{
				"MongoRescue.exe":        "exe 1",
				"tools/mongodump.exe":    "dump 1",
				"tools/mongorestore.exe": "restore 1",
			})
			if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(r.exe)), "evil.exe")); err == nil {
				t.Error("an archive entry was written outside the staging directory")
			}
		})
	}
}

func TestUpdaterChoosesSwapOrInstaller(t *testing.T) {
	cases := []struct {
		name     string
		writable bool
		portable bool
		want     string
	}{
		{"writable per-user or portable copy", true, true, ActionSwap},
		{"copy in Program Files", false, true, ActionLaunch},
		{"release without a portable archive", true, false, ActionLaunch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := available(false)
			if c.portable {
				res = withPortable(res)
			}
			src := &fakeSource{res: res, zip: portableZip(t, "2")}
			r := inAppRecorder(t)
			r.writable = c.writable
			var probed []string
			opts := r.options(src, "1.0.0", "windows")
			writable := opts.Writable
			opts.Writable = func(dir string) bool {
				probed = append(probed, dir)
				return writable(dir)
			}
			u, _ := started(t, opts)
			if err := u.Install(); err != nil {
				t.Fatal(err)
			}
			u.ops.Wait()
			s := u.Status()
			if s.Action != c.want || len(probed) == 0 || probed[0] != filepath.Dir(r.exe) {
				t.Fatalf("action %q probed %v; want %q", s.Action, probed, c.want)
			}
			if c.want == ActionSwap && (len(r.started) != 1 || len(r.launched) != 0) {
				t.Errorf("started %v launched %v", r.started, r.launched)
			}
			if c.want == ActionLaunch && (len(r.started) != 0 || len(r.launched) != 1 || s.State != UpdateInstalling) {
				t.Errorf("started %v launched %v status %+v", r.started, r.launched, s)
			}
		})
	}
	// macOS and Linux keep revealing the archive.
	r := inAppRecorder(t)
	u, _ := started(t, r.options(&fakeSource{res: withPortable(available(false))}, "1.0.0", "linux"))
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	u.ops.Wait()
	if s := u.Status(); s.Action != ActionReveal || s.State != UpdateReady || len(r.started) != 0 {
		t.Errorf("linux: %+v started %v", s, r.started)
	}
}

func TestUpdaterLegacyCopy(t *testing.T) {
	src := &fakeSource{res: available(false)}
	r := newRecorder(t)
	r.exe = `C:\Users\a\AppData\Local\Programs\MongoRescue\MongoRescue.exe`
	u, _ := started(t, r.options(src, "1.0.0", "windows"))
	if s := u.Status(); s.Legacy.Found {
		t.Fatalf("legacy = %+v without a registered copy", s.Legacy)
	}
	if err := u.RemoveLegacy(); !errors.Is(err, ErrNoLegacyCopy) {
		t.Errorf("RemoveLegacy = %v", err)
	}

	uninstaller := `C:\Program Files\MongoRescue\MongoRescue\uninstall.exe`
	r.mu.Lock()
	r.legacy = uninstaller
	r.launchErr = ErrInstallerCancelled
	r.mu.Unlock()
	if s := u.Status(); !s.Legacy.Found || s.Legacy.Dir != `C:\Program Files\MongoRescue\MongoRescue` || s.Legacy.State != LegacyIdle {
		t.Fatalf("legacy = %+v", s.Legacy)
	}
	if err := u.RemoveLegacy(); err != nil {
		t.Fatal(err)
	}
	u.ops.Wait()
	if s := u.Status(); s.Legacy.State != LegacyError || !strings.Contains(s.Legacy.Error, "cancelled") {
		t.Fatalf("legacy = %+v", s.Legacy)
	}
	r.mu.Lock()
	r.launchErr = nil
	r.mu.Unlock()
	if err := u.RemoveLegacy(); err != nil {
		t.Fatal(err)
	}
	u.ops.Wait()
	if s := u.Status(); s.Legacy.State != LegacyStarted {
		t.Fatalf("legacy = %+v", s.Legacy)
	}
	if len(r.launched) != 2 || r.launched[1] != uninstaller || strings.Join(r.args[1], " ") != "/S" {
		t.Fatalf("launched %v args %v", r.launched, r.args)
	}
	// Once the uninstaller is done, the copy is gone.
	r.mu.Lock()
	r.legacy = ""
	r.mu.Unlock()
	if s := u.Status(); s.Legacy.Found || s.Legacy.State != LegacyIdle {
		t.Errorf("legacy = %+v", s.Legacy)
	}
	if r.quits != 0 || len(src.dlDirs) != 0 {
		t.Error("removing the older copy touched the update")
	}

	// The running copy is never offered for removal, nor on other platforms.
	r2 := newRecorder(t)
	r2.exe = `c:\program files\mongorescue\mongorescue\MongoRescue.exe`
	r2.legacy = uninstaller
	u2, _ := started(t, r2.options(src, "1.0.0", "windows"))
	if s := u2.Status(); s.Legacy.Found {
		t.Errorf("the running per-machine copy is reported as legacy: %+v", s.Legacy)
	}
	r3 := newRecorder(t)
	r3.legacy = uninstaller
	u3, _ := started(t, r3.options(src, "1.0.0", "darwin"))
	if s := u3.Status(); s.Legacy.Found {
		t.Errorf("legacy on macOS: %+v", s.Legacy)
	}
}

func TestUpdaterHandlerRemoveLegacy(t *testing.T) {
	r := newRecorder(t)
	r.legacy = `C:\Program Files\MongoRescue\MongoRescue\uninstall.exe`
	u, _ := started(t, r.options(&fakeSource{}, "1.0.0", "windows"))
	h := u.Handler(http.NotFoundHandler())
	if rec := serveUpdate(h, http.MethodPost, UpdateRemoveLegacyPath, map[string]string{"Origin": "https://evil.example", UpdateHeader: "1"}); rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin = %d", rec.Code)
	}
	if rec := serveUpdate(h, http.MethodPost, UpdateRemoveLegacyPath, nil); rec.Code != http.StatusForbidden {
		t.Errorf("without header = %d", rec.Code)
	}
	if rec := serveUpdate(h, http.MethodGet, UpdateRemoveLegacyPath, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", rec.Code)
	}
	if len(r.launched) != 0 {
		t.Fatal("a refused request started the uninstaller")
	}
	rec := serveUpdate(h, http.MethodPost, UpdateRemoveLegacyPath, map[string]string{UpdateHeader: "1", "Origin": "http://wails.localhost"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body)
	}
	u.ops.Wait()
	if len(r.launched) != 1 {
		t.Errorf("launched %v", r.launched)
	}
	r.mu.Lock()
	r.legacy = ""
	r.mu.Unlock()
	if rec := serveUpdate(h, http.MethodPost, UpdateRemoveLegacyPath, map[string]string{UpdateHeader: "1", "Origin": "http://wails.localhost"}); rec.Code != http.StatusConflict {
		t.Errorf("remove without a copy = %d", rec.Code)
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
	var gotArgs []string
	launch := func(path string, args []string) error {
		launched = append(launched, path)
		gotArgs = args
		return nil
	}
	args := []string{"/S", "/RELAUNCH"}
	if err := verifyAndLaunch(update.File{Path: p, SHA256: sum[:]}, args, launch); err != nil || len(launched) != 1 || launched[0] != p {
		t.Fatalf("err %v launched %v", err, launched)
	}
	if strings.Join(gotArgs, " ") != "/S /RELAUNCH" {
		t.Errorf("args = %q", gotArgs)
	}
	if err := os.WriteFile(p, []byte("swapped"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyAndLaunch(update.File{Path: p, SHA256: sum[:]}, args, launch); !errors.Is(err, update.ErrChecksumMismatch) || len(launched) != 1 {
		t.Errorf("swapped file: err %v launched %v", err, launched)
	}
	if err := verifyAndLaunch(update.File{Path: p}, args, launch); !errors.Is(err, update.ErrChecksumMismatch) {
		t.Errorf("no checksum: %v", err)
	}
	if err := verifyAndLaunch(update.File{Path: p + ".missing", SHA256: sum[:]}, args, launch); err == nil {
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
	for _, want := range []string{`"status":"/desktop/update"`, `"install":"/desktop/update/install"`, `"release":"/desktop/update/release-page"`, `"X-MongoRescue-Desktop"`, "mongorescue_lang", "Update required", "Güncelleme gerekli", "mr-update-header", "Update to v{latest}", ".topbar-actions", `case "installing":`, `state === "installing"`, "restarts on the new version", "yeni sürümle yeniden açılacak",
		`"removeLegacy":"/desktop/update/remove-legacy"`, `case "restarting":`, "Downloading {percent}%", "İndiriliyor %{percent}", "Restarting…", "Yeniden başlatılıyor…", `case "waiting":`, "Update will install after the running backup finishes", "çalışan yedekleme bitince kurulacak",
		"mr-legacy-bar", "mongorescue_legacy_dismissed", "installed in Program Files", "Program Files klasöründe kurulu",
		`"check":"/desktop/update/check"`, `"app-version"`, "mr-update-popover", "Check for updates", "Güncellemeleri denetle", "Up to date ({version})", "Güncel ({version})",
		"s.checking", "visibilitychange", "window.mrAnnounce",
		// The dashboard API used by the command palette (web/static/nav.js).
		"window.__mongorescueUpdate = {", "check: function () { return requestCheck(); }",
		"install: function () { return startUpdate(true); }", "status: function () { return status; }",
		// Toasts (web/static/nav.css) stay above the bars.
		`"--mr-bottom-offset"`} {
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

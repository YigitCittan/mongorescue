//go:build desktop_e2e

package updatee2e

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/desktop"
	"github.com/yigitcittan/mongorescue/internal/update"
)

// Environment set by scripts/test-desktop-update-e2e.sh.
const (
	// artifactsEnv is the directory holding <version>/ with the release files of
	// each build.
	artifactsEnv  = "MONGORESCUE_E2E_ARTIFACTS"
	oldVersionEnv = "MONGORESCUE_E2E_OLD_VERSION"
	newVersionEnv = "MONGORESCUE_E2E_NEW_VERSION"
)

// Time limits.
const (
	harnessTimeout = 4 * time.Minute
	restartWait    = 2 * time.Minute
	commandTimeout = time.Minute
)

// result mirrors <results>/result.json (cmd/mongorescue-desktop/e2e.go).
type result struct {
	Version string               `json:"version"`
	PID     int                  `json:"pid"`
	Check   int                  `json:"check"`
	Install int                  `json:"install"`
	Status  desktop.UpdateStatus `json:"status"`
	Quit    bool                 `json:"quit"`
	Error   string               `json:"error"`
}

// restarted mirrors <results>/restarted.json.
type restarted struct {
	Version   string `json:"version"`
	PID       int    `json:"pid"`
	OldPID    int    `json:"old_pid"`
	OldExited bool   `json:"old_exited"`
	Exe       string `json:"exe"`
}

// suite is the two builds under test.
type suite struct {
	artifacts  string
	oldV, newV string
	// asset is the release file the update downloads; package the archive the old
	// version is installed from.
	asset, oldPackage string
	checksums         string
}

func TestDesktopUpdate(t *testing.T) {
	s := newSuite(t)
	t.Run("update", s.testUpdate)
	t.Run("tampered", s.testTampered)
}

func newSuite(t *testing.T) *suite {
	t.Helper()
	dir := os.Getenv(artifactsEnv)
	if dir == "" {
		t.Skip("set " + artifactsEnv + "; scripts/test-desktop-update-e2e.sh builds the app and runs this test")
	}
	s := &suite{artifacts: dir, oldV: envOr(oldVersionEnv, "0.0.1"), newV: envOr(newVersionEnv, "0.0.2")}
	oldV, newV := mustVersion(t, s.oldV), mustVersion(t, s.newV)
	var err error
	if runtime.GOOS == "windows" {
		s.asset, err = update.PortableAssetName(runtime.GOOS, runtime.GOARCH, newV)
		if err == nil {
			s.oldPackage, err = update.PortableAssetName(runtime.GOOS, runtime.GOARCH, oldV)
		}
	} else {
		s.asset, err = update.AssetName(runtime.GOOS, runtime.GOARCH, newV)
		if err == nil {
			s.oldPackage, err = update.AssetName(runtime.GOOS, runtime.GOARCH, oldV)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	s.checksums = update.ChecksumsName(newV)
	return s
}

// testUpdate updates the old install to the new version.
func (s *suite) testUpdate(t *testing.T) {
	in := s.install(t)
	before := snapshot(t, in.dir)
	rel := s.serve(t, false)

	res := in.runUpdate(t, rel.url())

	in.wantNoRecord(t, "installer-launched", "opened-url")
	if res.Error != "" || res.Check != http.StatusOK || res.Install != http.StatusAccepted {
		t.Fatalf("headless update: error %q, check %d, install %d, status %+v", res.Error, res.Check, res.Install, res.Status)
	}
	if !res.Status.Available || !res.Status.Installable || res.Status.Latest != s.newV {
		t.Fatalf("status %+v: want v%s available and installable", res.Status, s.newV)
	}
	if got := rel.hits(s.checksums); got < 1 {
		t.Errorf("the checksums file was fetched %d times, want at least once", got)
	}
	if got := rel.hits(s.asset); got != 1 {
		t.Errorf("%s was downloaded %d times, want once", s.asset, got)
	}
	if runtime.GOOS == "windows" {
		s.checkSwap(t, in, rel, res)
	} else {
		s.checkReveal(t, in, res, before)
	}
}

// checkSwap checks the in-place update on Windows: the new version runs from the
// same path, the tools were replaced and nothing of the update is left.
func (s *suite) checkSwap(t *testing.T, in *install, rel *fakeRelease, res result) {
	t.Helper()
	if res.Status.Action != desktop.ActionSwap || res.Status.State != desktop.UpdateRestarting || !res.Quit {
		t.Fatalf("status %+v, quit %v: want action %q, state %q and a quit", res.Status, res.Quit, desktop.ActionSwap, desktop.UpdateRestarting)
	}
	installer, err := update.AssetName(runtime.GOOS, runtime.GOARCH, mustVersion(t, s.newV))
	if err != nil {
		t.Fatal(err)
	}
	if got := rel.hits(installer); got != 0 {
		t.Errorf("the installer was downloaded %d times, want never", got)
	}
	in.wantNoRecord(t, "revealed")

	r := in.waitRestarted(t)
	if r.Version != s.newV || r.OldPID != res.PID || !r.OldExited {
		t.Errorf("restarted %+v: want version %s after old process %d exited", r, s.newV, res.PID)
	}
	if !sameFile(r.Exe, in.exe) {
		t.Errorf("the new version runs from %s, want %s", r.Exe, in.exe)
	}
	if !desktop.WaitForProcessExit(r.PID, commandTimeout) {
		t.Errorf("the restarted version (pid %d) did not exit", r.PID)
	}
	wantVersion(t, in.exe, s.newV)
	tool, err := os.ReadFile(filepath.Join(in.dir, "tools", "mongodump.exe"))
	if err != nil || !strings.Contains(string(tool), "placeholder mongodump "+s.newV) {
		t.Errorf("tools/mongodump.exe = %q, %v: want the new version's", tool, err)
	}
	entries, err := os.ReadDir(in.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".old") || strings.HasPrefix(e.Name(), ".update-") {
			t.Errorf("leftover of the update: %s", e.Name())
		}
	}
}

// checkReveal checks the update on macOS and Linux: the verified archive of the new
// version is saved in the download directory and shown, and the installed app is
// untouched.
func (s *suite) checkReveal(t *testing.T, in *install, res result, before map[string]string) {
	t.Helper()
	if res.Status.Action != desktop.ActionReveal || res.Status.State != desktop.UpdateReady || res.Status.File != s.asset || res.Quit {
		t.Fatalf("status %+v, quit %v: want action %q, state %q, file %s and no quit", res.Status, res.Quit, desktop.ActionReveal, desktop.UpdateReady, s.asset)
	}
	downloaded := filepath.Join(in.downloads, s.asset)
	revealed := in.record(t, "revealed")
	if !sameFile(revealed, downloaded) {
		t.Errorf("revealed %q, want %s", revealed, downloaded)
	}
	sum := fileSHA256(t, downloaded)
	if want := s.checksum(t, s.asset); sum != want {
		t.Errorf("%s has SHA-256 %s, want %s from the checksums file", s.asset, sum, want)
	}
	if left := listFiles(t, in.downloads); !slices.Equal(left, []string{s.asset}) {
		t.Errorf("download directory holds %q, want only %s", left, s.asset)
	}
	in.wantUnchanged(t, before)
	wantVersion(t, in.exe, s.oldV)
	in.wantNoRecord(t, "restarted.json", "installer-launched")

	// The saved archive is the new build: unpacked as users do, it reports the
	// new version.
	unpacked := filepath.Join(in.root, "unpacked")
	if err := os.MkdirAll(unpacked, 0o700); err != nil {
		t.Fatal(err)
	}
	switch runtime.GOOS {
	case "darwin":
		reportQuarantine(t, downloaded, unpacked)
		app := filepath.Join(unpacked, "MongoRescue.app")
		wantPlistVersion(t, app, s.newV)
		wantVersion(t, filepath.Join(app, "Contents", "MacOS", "MongoRescue"), s.newV)
	default:
		runCmd(t, "tar", "-xzf", downloaded, "-C", unpacked)
		wantVersion(t, filepath.Join(unpacked, "MongoRescue"), s.newV)
	}
}

// testTampered serves an asset that does not match the checksums file: the update
// must fail and leave the old version as it was.
func (s *suite) testTampered(t *testing.T) {
	in := s.install(t)
	before := snapshot(t, in.dir)
	rel := s.serve(t, true)

	res := in.runUpdate(t, rel.url())

	if res.Status.State != desktop.UpdateError || !strings.Contains(res.Status.Error, update.ErrChecksumMismatch.Error()) {
		t.Errorf("status %+v: want state %q with %q", res.Status, desktop.UpdateError, update.ErrChecksumMismatch)
	}
	if res.Quit {
		t.Error("the app was asked to quit after a failed update")
	}
	if got := rel.hits(s.asset); got != 1 {
		t.Errorf("%s was downloaded %d times, want once", s.asset, got)
	}
	in.wantUnchanged(t, before)
	wantVersion(t, in.exe, s.oldV)
	// A restart would have been started before the quit; give it a moment anyway.
	time.Sleep(time.Second)
	in.wantNoRecord(t, "restarted.json", "installer-launched", "revealed", "opened-url")
	if left := listFiles(t, in.downloads); len(left) != 0 {
		t.Errorf("the failed download left %q", left)
	}
}

// install is one installed copy of the old version, with its own home and
// download directories.
type install struct {
	root string
	// dir is the installed app: MongoRescue.app on macOS, the directory holding
	// the executable and tools/ elsewhere.
	dir string
	exe string
	// results is the directory of the headless update's records.
	results string
	// downloads is where the update saves the archive: ~/Downloads on macOS,
	// $XDG_DOWNLOAD_DIR on Linux, the per-download directories under
	// %LOCALAPPDATA%\MongoRescue\updates on Windows.
	downloads string
	env       []string
}

// install unpacks the old version's release archive as users do: MongoRescue.app
// into an Applications folder on macOS, the tarball into a directory on Linux,
// the portable archive into a per-user Programs folder on Windows.
func (s *suite) install(t *testing.T) *install {
	t.Helper()
	root := t.TempDir()
	in := &install{root: root, results: filepath.Join(root, "results")}
	home := filepath.Join(root, "home")
	for _, d := range []string{in.results, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pkg := filepath.Join(s.artifacts, s.oldV, s.oldPackage)
	in.env = append(os.Environ(), "HOME="+home)
	switch runtime.GOOS {
	case "darwin":
		apps := filepath.Join(root, "Applications")
		runCmd(t, "ditto", "-x", "-k", pkg, apps)
		in.dir = filepath.Join(apps, "MongoRescue.app")
		in.exe = filepath.Join(in.dir, "Contents", "MacOS", "MongoRescue")
		in.downloads = filepath.Join(home, "Downloads")
		wantPlistVersion(t, in.dir, s.oldV)
	case "linux":
		in.dir = filepath.Join(root, "opt", "MongoRescue")
		if err := os.MkdirAll(in.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		runCmd(t, "tar", "-xzf", pkg, "-C", in.dir)
		in.exe = filepath.Join(in.dir, "MongoRescue")
		in.downloads = filepath.Join(root, "downloads")
		in.env = append(in.env, "XDG_DOWNLOAD_DIR="+in.downloads)
	case "windows":
		in.dir = filepath.Join(root, "Programs", "MongoRescue")
		unzip(t, pkg, in.dir)
		in.exe = filepath.Join(in.dir, "MongoRescue.exe")
		local := filepath.Join(root, "localappdata")
		in.downloads = filepath.Join(local, desktop.AppDirName, "updates")
		in.env = append(in.env, "LOCALAPPDATA="+local)
	default:
		t.Skipf("no desktop release for %s", runtime.GOOS)
	}
	wantVersion(t, in.exe, s.oldV)
	t.Cleanup(func() {
		if t.Failed() {
			in.dumpRecords(t)
		}
	})
	return in
}

// runUpdate runs the installed app's headless update against baseURL and returns
// result.json.
func (in *install) runUpdate(t *testing.T, baseURL string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), harnessTimeout)
	defer cancel()
	out, err := os.Create(filepath.Join(in.results, "output.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, in.exe, "--e2e-update="+in.results)
	cmd.Env = append(slices.Clone(in.env), desktop.E2EUpdateBaseURLEnv+"="+baseURL)
	cmd.Stdout, cmd.Stderr = out, out
	err = cmd.Run()
	_ = out.Close()
	if err != nil {
		t.Errorf("headless update: %v", err)
	}
	var res result
	readJSON(t, filepath.Join(in.results, "result.json"), &res)
	t.Logf("result: %+v", res)
	return res
}

// waitRestarted waits for restarted.json, written by the version the update
// started.
func (in *install) waitRestarted(t *testing.T) restarted {
	t.Helper()
	p := filepath.Join(in.results, "restarted.json")
	deadline := time.Now().Add(restartWait)
	for {
		if _, err := os.Stat(p); err == nil {
			time.Sleep(200 * time.Millisecond) // written in one call; let it finish
			var r restarted
			readJSON(t, p, &r)
			t.Logf("restarted: %+v", r)
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new version did not start within %v", restartWait)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// record returns the trimmed content of a record of the headless update.
func (in *install) record(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(in.results, name))
	if err != nil {
		t.Fatalf("record %s: %v", name, err)
	}
	return strings.TrimSpace(string(b))
}

// wantNoRecord fails for each named record that exists.
func (in *install) wantNoRecord(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if b, err := os.ReadFile(filepath.Join(in.results, name)); err == nil {
			t.Errorf("unexpected %s: %s", name, strings.TrimSpace(string(b)))
		}
	}
}

// wantUnchanged fails when the installed app differs from before.
func (in *install) wantUnchanged(t *testing.T, before map[string]string) {
	t.Helper()
	after := snapshot(t, in.dir)
	for p, v := range before {
		if after[p] != v {
			t.Errorf("installed %s changed: %q -> %q", p, v, after[p])
		}
	}
	for p, v := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("installed %s appeared: %q", p, v)
		}
	}
}

// dumpRecords logs the records and logs of the headless update.
func (in *install) dumpRecords(t *testing.T) {
	entries, err := os.ReadDir(in.results)
	if err != nil {
		return
	}
	for _, e := range entries {
		if b, rerr := os.ReadFile(filepath.Join(in.results, e.Name())); rerr == nil {
			t.Logf("--- %s\n%s", e.Name(), b)
		}
	}
}

// fakeRelease serves the new version as GitHub's latest release:
// /repos/<owner>/<repo>/releases/latest and /download/v<version>/<file>.
type fakeRelease struct {
	srv *httptest.Server
	mu  sync.Mutex
	hit map[string]int
}

// serve starts the fake release of the new version, with the asset's bytes
// changed when tamper is set (the checksums file stays the release's).
func (s *suite) serve(t *testing.T, tamper bool) *fakeRelease {
	t.Helper()
	dir := filepath.Join(s.artifacts, s.newV)
	files := map[string][]byte{}
	for _, name := range listFiles(t, dir) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = b
	}
	if _, ok := files[s.asset]; !ok {
		t.Fatalf("%s lacks %s", dir, s.asset)
	}
	if tamper {
		b := bytes.Clone(files[s.asset])
		b[len(b)/2] ^= 0xff
		files[s.asset] = b
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	tag := "v" + s.newV
	fr := &fakeRelease{hit: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/"+update.Repository+"/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		type asset struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int    `json:"size"`
		}
		assets := make([]asset, 0, len(names))
		for _, name := range names {
			assets = append(assets, asset{Name: name, URL: fr.url() + "/download/" + tag + "/" + name, Size: len(files[name])})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tag, "body": "Desktop update end-to-end test", "draft": false, "prerelease": false,
			"html_url": fr.url() + "/releases/tag/" + tag, "assets": assets,
		})
	})
	mux.HandleFunc("GET /download/"+tag+"/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		b, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fr.mu.Lock()
		fr.hit[name]++
		fr.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(b)
	})
	fr.srv = httptest.NewServer(mux)
	t.Cleanup(fr.srv.Close)
	return fr
}

// url returns the server's base URL.
func (fr *fakeRelease) url() string { return fr.srv.URL }

// hits returns how often the file name was downloaded.
func (fr *fakeRelease) hits(name string) int {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return fr.hit[name]
}

// checksum returns the SHA-256 the release's checksums file lists for name.
func (s *suite) checksum(t *testing.T, name string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(s.artifacts, s.newV, s.checksums))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if sum, file, ok := strings.Cut(sc.Text(), "  "); ok && file == name {
			return sum
		}
	}
	t.Fatalf("%s does not list %s", s.checksums, name)
	return ""
}

// reportQuarantine logs the extended attributes of the downloaded archive and of
// MongoRescue.app unpacked from it with ditto (as Archive Utility does) into
// unpacked, and whether either carries com.apple.quarantine, which decides whether
// Gatekeeper checks the updated app again. It is logged, not asserted.
func reportQuarantine(t *testing.T, archive, unpacked string) {
	t.Helper()
	zipAttrs := runCmd(t, "xattr", "-l", archive)
	runCmd(t, "ditto", "-x", "-k", archive, unpacked)
	appAttrs := runCmd(t, "xattr", "-lr", filepath.Join(unpacked, "MongoRescue.app"))
	zipQ := strings.Contains(zipAttrs, "com.apple.quarantine")
	appQ := strings.Contains(appAttrs, "com.apple.quarantine")
	t.Logf("xattr -l %s:\n%s", filepath.Base(archive), orNone(zipAttrs))
	t.Logf("xattr -lr MongoRescue.app (unpacked with ditto -x -k):\n%s", orNone(appAttrs))
	summary := fmt.Sprintf("QUARANTINE: downloaded zip com.apple.quarantine=%v, unpacked MongoRescue.app com.apple.quarantine=%v", zipQ, appQ)
	t.Log(summary)
	if p := os.Getenv("GITHUB_STEP_SUMMARY"); p != "" {
		md := "### macOS quarantine after the in-app download\n\n" + summary + "\n\n```\n$ xattr -l " + filepath.Base(archive) + "\n" + orNone(zipAttrs) +
			"\n$ xattr -lr MongoRescue.app\n" + orNone(appAttrs) + "\n```\n"
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.WriteString(md)
			_ = f.Close()
		}
	}
}

// orNone returns s, or "(none)" when it is empty.
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// wantVersion fails unless exe --version reports version v.
func wantVersion(t *testing.T, exe, v string) {
	t.Helper()
	out := strings.TrimSpace(runCmd(t, exe, desktop.VersionFlag))
	if want := "MongoRescue " + v + " (e2e)"; out != want {
		t.Errorf("%s %s = %q, want %q", exe, desktop.VersionFlag, out, want)
	}
}

// wantPlistVersion fails unless the bundle's CFBundleShortVersionString is v.
func wantPlistVersion(t *testing.T, app, v string) {
	t.Helper()
	got := strings.TrimSpace(runCmd(t, "plutil", "-extract", "CFBundleShortVersionString", "raw", "-o", "-", filepath.Join(app, "Contents", "Info.plist")))
	if got != v {
		t.Errorf("%s CFBundleShortVersionString = %q, want %q", app, got, v)
	}
}

// runCmd runs name with args and returns its standard output; it fails the test
// when the command fails.
func runCmd(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s%s", name, strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// snapshot returns every entry below dir with its kind, permissions and, for a
// file, its SHA-256, by slash-separated relative path.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			m[filepath.ToSlash(rel)] = "link " + target
		case d.IsDir():
			m[filepath.ToSlash(rel)] = fmt.Sprintf("dir %v", info.Mode().Perm())
		default:
			m[filepath.ToSlash(rel)] = fmt.Sprintf("file %v %s", info.Mode().Perm(), fileSHA256(t, p))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// listFiles returns the names of the regular files below dir (relative,
// slash-separated, sorted); none when dir does not exist.
func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, rerr := filepath.Rel(dir, p)
			if rerr != nil {
				return rerr
			}
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	slices.Sort(names)
	return names
}

// fileSHA256 returns the hex SHA-256 of the file p.
func fileSHA256(t *testing.T, p string) string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sameFile reports whether a and b name the same existing file.
func sameFile(a, b string) bool {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}

// unzip unpacks the release's portable archive into dir, as Explorer's "Extract
// all" does. The archive comes from the build under test.
func unzip(t *testing.T, archive, dir string) {
	t.Helper()
	zr, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	root, err := os.OpenRoot(mustMkdir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, zf := range zr.File {
		name := strings.TrimSuffix(strings.ReplaceAll(zf.Name, `\`, "/"), "/")
		if zf.FileInfo().IsDir() {
			if err = root.MkdirAll(name, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if d := filepath.Dir(name); d != "." {
			if err = root.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		rc, oerr := zf.Open()
		if oerr != nil {
			t.Fatal(oerr)
		}
		out, cerr := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if cerr != nil {
			t.Fatal(cerr)
		}
		_, err = io.Copy(out, io.LimitReader(rc, update.MaxAssetSize))
		_ = rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// mustMkdir creates dir and returns it.
func mustMkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// mustVersion parses v.
func mustVersion(t *testing.T, v string) update.Version {
	t.Helper()
	pv, err := update.ParseVersion(v)
	if err != nil {
		t.Fatal(err)
	}
	return pv
}

// envOr returns the environment variable name, or def when it is empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// readJSON decodes the JSON file p into v.
func readJSON(t *testing.T, p string, v any) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
}

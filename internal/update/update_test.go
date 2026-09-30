package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	good := map[string]Version{
		"1.2.3":     {1, 2, 3},
		"v0.3.2":    {0, 3, 2},
		"v10.0.100": {10, 0, 100},
	}
	for in, want := range good {
		got, err := ParseVersion(in)
		if err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v", in, got, err, want)
		}
		if got.String() != strings.TrimPrefix(in, "v") {
			t.Errorf("String() = %q", got.String())
		}
	}
	for _, in := range []string{"", "dev", "1.2", "1.2.3.4", "v1.2.3-rc.1", "1.2.3+build", "01.2.3", "1.-2.3", "vv1.2.3", "1..3", "1.2.x", "1.2.3 "} {
		if _, err := ParseVersion(in); !errors.Is(err, ErrInvalidVersion) {
			t.Errorf("ParseVersion(%q) err = %v; want ErrInvalidVersion", in, err)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.1.0", "1.0.9", 1},
		{"2.0.0", "1.9.9", 1},
		{"0.3.2", "0.10.0", -1},
	}
	for _, c := range cases {
		a, _ := ParseVersion(c.a)
		b, _ := ParseVersion(c.b)
		if got := a.Compare(b); got != c.want {
			t.Errorf("%s vs %s = %d; want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAssetName(t *testing.T) {
	v := Version{1, 2, 3}
	cases := map[[2]string]string{
		{"windows", "amd64"}: "MongoRescue-desktop_1.2.3_windows_amd64_installer.exe",
		{"darwin", "arm64"}:  "MongoRescue-desktop_1.2.3_macos_universal.zip",
		{"darwin", "amd64"}:  "MongoRescue-desktop_1.2.3_macos_universal.zip",
		{"linux", "amd64"}:   "MongoRescue-desktop_1.2.3_linux_amd64.tar.gz",
	}
	for p, want := range cases {
		if got, err := AssetName(p[0], p[1], v); err != nil || got != want {
			t.Errorf("AssetName(%v) = %q, %v; want %q", p, got, err, want)
		}
	}
	for _, p := range [][2]string{{"linux", "arm64"}, {"windows", "arm64"}, {"freebsd", "amd64"}} {
		if _, err := AssetName(p[0], p[1], v); !errors.Is(err, ErrUnsupportedPlatform) {
			t.Errorf("AssetName(%v) err = %v; want ErrUnsupportedPlatform", p, err)
		}
	}
	if got := ChecksumsName(v); got != "MongoRescue-desktop_1.2.3_checksums.txt" {
		t.Errorf("ChecksumsName = %q", got)
	}
}

// fakeGitHub serves the releases/latest API and the release downloads.
type fakeGitHub struct {
	srv       *httptest.Server
	tag       string
	status    int
	pre       bool
	files     map[string][]byte // download name -> content
	checksums string            // "" : generated from files
	userAgent atomic.Value
	extraURL  string // an asset URL added verbatim
}

func newFakeGitHub(t *testing.T, tag string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{tag: tag, status: http.StatusOK, files: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/YigitCittan/mongorescue/releases/latest", f.latest)
	mux.HandleFunc("GET /YigitCittan/mongorescue/releases/download/{tag}/{name}", f.download)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) checker(goos, goarch string) *Checker {
	return &Checker{
		Client:         f.srv.Client(),
		DownloadClient: f.srv.Client(),
		BaseURL:        f.srv.URL,
		DownloadPrefix: f.srv.URL + "/YigitCittan/mongorescue/releases/download/",
		ReleasesPrefix: f.srv.URL + "/YigitCittan/mongorescue/releases/",
		GOOS:           goos,
		GOARCH:         goarch,
	}
}

func (f *fakeGitHub) dl(name string) string {
	return f.srv.URL + "/YigitCittan/mongorescue/releases/download/" + f.tag + "/" + name
}

func (f *fakeGitHub) latest(w http.ResponseWriter, r *http.Request) {
	f.userAgent.Store(r.Header.Get("User-Agent"))
	if f.status != http.StatusOK {
		w.WriteHeader(f.status)
		return
	}
	type asset struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int    `json:"size"`
	}
	rel := struct {
		Tag        string  `json:"tag_name"`
		Body       string  `json:"body"`
		HTMLURL    string  `json:"html_url"`
		Prerelease bool    `json:"prerelease"`
		Assets     []asset `json:"assets"`
	}{Tag: f.tag, Body: "## Added\n- things", HTMLURL: f.srv.URL + "/YigitCittan/mongorescue/releases/tag/" + f.tag, Prerelease: f.pre}
	for name := range f.files {
		rel.Assets = append(rel.Assets, asset{name, f.dl(name), len(f.files[name])})
	}
	if f.extraURL != "" {
		rel.Assets = append(rel.Assets, asset{"MongoRescue-desktop_" + strings.TrimPrefix(f.tag, "v") + "_linux_amd64.tar.gz", f.extraURL, 0})
	}
	_ = json.NewEncoder(w).Encode(rel)
}

func (f *fakeGitHub) download(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if strings.HasSuffix(name, "_checksums.txt") {
		if f.checksums != "" {
			_, _ = w.Write([]byte(f.checksums))
			return
		}
		var b strings.Builder
		for n, c := range f.files {
			if !strings.HasSuffix(n, "_checksums.txt") {
				sum := sha256.Sum256(c)
				fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), n)
			}
		}
		_, _ = w.Write([]byte(b.String()))
		return
	}
	c, ok := f.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(c)
}

// release adds the linux asset and a checksums file to f.
func (f *fakeGitHub) release(content string) string {
	v := strings.TrimPrefix(f.tag, "v")
	name := "MongoRescue-desktop_" + v + "_linux_amd64.tar.gz"
	f.files[name] = []byte(content)
	f.files["MongoRescue-desktop_"+v+"_checksums.txt"] = nil
	f.files["MongoRescue-desktop_"+v+"_windows_amd64_installer.exe"] = []byte("exe")
	return name
}

func TestCheckPolicy(t *testing.T) {
	cases := []struct {
		current, tag         string
		available, mandatory bool
	}{
		{"1.2.3", "v1.2.3", false, false},
		{"1.2.3", "v1.2.4", true, false},
		{"1.2.3", "v1.3.0", true, false},
		{"v1.2.3", "v2.0.0", true, true},
		{"2.0.0", "v1.9.9", false, false},
	}
	for _, c := range cases {
		f := newFakeGitHub(t, c.tag)
		name := f.release("data")
		res, err := f.checker("linux", "amd64").Check(context.Background(), c.current)
		if err != nil {
			t.Fatalf("%s->%s: %v", c.current, c.tag, err)
		}
		if res.Available != c.available || res.Mandatory != c.mandatory {
			t.Errorf("%s->%s: available=%v mandatory=%v; want %v %v", c.current, c.tag, res.Available, res.Mandatory, c.available, c.mandatory)
		}
		if res.Asset.Name != name || res.Asset.URL != f.dl(name) || res.ChecksumsURL == "" {
			t.Errorf("asset = %+v checksums = %q", res.Asset, res.ChecksumsURL)
		}
		if res.Notes != "## Added\n- things" || !strings.HasSuffix(res.HTMLURL, "/releases/tag/"+c.tag) {
			t.Errorf("notes %q url %q", res.Notes, res.HTMLURL)
		}
		if ua, _ := f.userAgent.Load().(string); ua != "MongoRescue/"+strings.TrimPrefix(c.current, "v") {
			t.Errorf("User-Agent = %q", ua)
		}
	}
}

func TestCheckErrors(t *testing.T) {
	f := newFakeGitHub(t, "v1.0.0")
	ck := f.checker("linux", "amd64")
	if _, err := ck.Check(context.Background(), "dev"); !errors.Is(err, ErrInvalidVersion) {
		t.Errorf("dev: %v", err)
	}
	f.status = http.StatusNotFound
	if _, err := ck.Check(context.Background(), "1.0.0"); !errors.Is(err, ErrNoRelease) {
		t.Errorf("404: %v", err)
	}
	f.status = http.StatusForbidden
	if _, err := ck.Check(context.Background(), "1.0.0"); !errors.Is(err, ErrUnexpectedStatus) {
		t.Errorf("403: %v", err)
	}
	f.status = http.StatusOK
	f.tag = "v2.0.0-rc.1"
	if _, err := ck.Check(context.Background(), "1.0.0"); !errors.Is(err, ErrNoRelease) {
		t.Errorf("pre-release tag: %v", err)
	}
	f.tag, f.pre = "v2.0.0", true
	if _, err := ck.Check(context.Background(), "1.0.0"); !errors.Is(err, ErrNoRelease) {
		t.Errorf("prerelease flag: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ck.Check(ctx, "1.0.0"); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}

func TestCheckIgnoresUntrustedURLs(t *testing.T) {
	f := newFakeGitHub(t, "v1.1.0")
	f.extraURL = "https://evil.example/YigitCittan/mongorescue/releases/download/v1.1.0/x"
	res, err := f.checker("linux", "amd64").Check(context.Background(), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if res.Asset.URL != "" {
		t.Errorf("untrusted asset accepted: %+v", res.Asset)
	}
	ck := &Checker{}
	for _, u := range []string{
		"https://github.com/YigitCittan/mongorescue/releases/download/v1/a.zip",
	} {
		if !ck.trusted(u, DefaultDownloadPrefix) {
			t.Errorf("rejected %s", u)
		}
	}
	for _, u := range []string{
		"http://github.com/YigitCittan/mongorescue/releases/download/v1/a.zip",
		"https://github.com.evil/YigitCittan/mongorescue/releases/download/v1/a.zip",
		"https://github.com/YigitCittan/mongorescue/releases/download/../../../other/a.zip",
		"https://github.com/YigitCittan/mongorescue/releases/download/",
		"https://github.com/Other/mongorescue/releases/download/v1/a.zip",
		"https://user@github.com/YigitCittan/mongorescue/releases/download/v1/a.zip",
		"https://github.com/YigitCittan/mongorescue/releases/download/v1%2Fa.zip",
		"https://github.com/YigitCittan/mongorescue/releases/download/v1/a.zip?x=1",
		"/YigitCittan/mongorescue/releases/download/v1/a.zip",
	} {
		if ck.trusted(u, DefaultDownloadPrefix) {
			t.Errorf("accepted %s", u)
		}
	}
}

func TestCheckUnsupportedPlatform(t *testing.T) {
	f := newFakeGitHub(t, "v1.1.0")
	f.release("x")
	ck := f.checker("linux", "arm64")
	res, err := ck.Check(context.Background(), "1.0.0")
	if err != nil || !res.Available || res.Asset.URL != "" {
		t.Fatalf("res %+v err %v", res, err)
	}
	if _, err := ck.Download(context.Background(), res, t.TempDir()); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("Download err = %v", err)
	}
}

func TestMandatoryNeedsInstallableFiles(t *testing.T) {
	f := newFakeGitHub(t, "v2.0.0")
	f.release("x")
	res, err := f.checker("linux", "amd64").Check(context.Background(), "1.0.0")
	if err != nil || !res.Mandatory || !res.Installable {
		t.Fatalf("complete release: %+v %v", res, err)
	}
	// No asset for this platform.
	res, err = f.checker("linux", "arm64").Check(context.Background(), "1.0.0")
	if err != nil || !res.Available || res.Mandatory || res.Installable {
		t.Errorf("no platform asset: %+v %v", res, err)
	}
	// Assets uploaded, checksums not yet.
	delete(f.files, "MongoRescue-desktop_2.0.0_checksums.txt")
	res, err = f.checker("linux", "amd64").Check(context.Background(), "1.0.0")
	if err != nil || !res.Available || res.Mandatory || res.Installable || res.Asset.URL == "" {
		t.Errorf("no checksums: %+v %v", res, err)
	}
	// No desktop files at all.
	f.files = map[string][]byte{}
	res, err = f.checker("windows", "amd64").Check(context.Background(), "1.0.0")
	if err != nil || !res.Available || res.Mandatory || res.Installable {
		t.Errorf("no files: %+v %v", res, err)
	}
}

func TestCheckRedirect(t *testing.T) {
	req := func(raw string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	via := []*http.Request{req("https://github.com/x")}
	for _, u := range []string{
		"https://github.com/YigitCittan/mongorescue/releases/download/v1/a.zip",
		"https://objects.githubusercontent.com/github-production-release-asset/1",
		"https://release-assets.githubusercontent.com/x",
		"https://GitHub.com:443/x",
	} {
		if err := CheckRedirect(req(u), via); err != nil {
			t.Errorf("%s refused: %v", u, err)
		}
	}
	for _, u := range []string{
		"http://objects.githubusercontent.com/x",
		"https://evil.example/x",
		"https://githubusercontent.com.evil.example/x",
		"https://evilgithubusercontent.com/x",
		"https://api.github.com.evil/x",
		"https://user@github.com/x",
		"https://github.com:8443/x",
	} {
		if err := CheckRedirect(req(u), via); !errors.Is(err, ErrRedirect) {
			t.Errorf("%s: err = %v; want ErrRedirect", u, err)
		}
	}
	long := make([]*http.Request, maxRedirects)
	if err := CheckRedirect(req("https://github.com/x"), long); !errors.Is(err, ErrRedirect) {
		t.Errorf("hop limit: %v", err)
	}
	if err := CheckRedirect(req("https://github.com/x"), long[:maxRedirects-1]); err != nil {
		t.Errorf("under the hop limit: %v", err)
	}
	for _, c := range []*http.Client{defaultClient, defaultDownloadClient} {
		if c.CheckRedirect == nil {
			t.Error("a default client has no redirect policy")
		}
	}
}

func TestDownloadVerifies(t *testing.T) {
	f := newFakeGitHub(t, "v1.1.0")
	name := f.release("archive content")
	ck := f.checker("linux", "amd64")
	res, err := ck.Check(context.Background(), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, name), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	f2, err := ck.Download(context.Background(), res, dir)
	if err != nil {
		t.Fatal(err)
	}
	p := f2.Path
	if sum := sha256.Sum256([]byte("archive content")); hex.EncodeToString(f2.SHA256) != hex.EncodeToString(sum[:]) {
		t.Errorf("SHA256 = %x", f2.SHA256)
	}
	if p != filepath.Join(dir, name) {
		t.Errorf("path = %q", p)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "archive content" {
		t.Errorf("content %q err %v", b, err)
	}
	if st, _ := os.Stat(p); st != nil && st.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Errorf("mode = %v", st.Mode())
	}
	assertOnly(t, dir, name)
}

func TestDownloadFailures(t *testing.T) {
	f := newFakeGitHub(t, "v1.1.0")
	name := f.release("archive content")
	ck := f.checker("linux", "amd64")
	res, err := ck.Check(context.Background(), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	wrong := sha256.Sum256([]byte("other"))
	cases := []struct {
		checksums string
		want      error
	}{
		{hex.EncodeToString(wrong[:]) + "  " + name + "\n", ErrChecksumMismatch},
		{hex.EncodeToString(wrong[:]) + "  other.tar.gz\n", ErrNoChecksum},
		{"zz  " + name + "\n", ErrNoChecksum},
	}
	for _, c := range cases {
		f.checksums = c.checksums
		dir := t.TempDir()
		if _, err := ck.Download(context.Background(), res, dir); !errors.Is(err, c.want) {
			t.Errorf("checksums %q: err = %v; want %v", c.checksums, err, c.want)
		}
		assertOnly(t, dir)
	}
	f.checksums = ""

	noSums := res
	noSums.ChecksumsURL = ""
	if _, err := ck.Download(context.Background(), noSums, t.TempDir()); !errors.Is(err, ErrNoChecksum) {
		t.Errorf("no checksums: %v", err)
	}
	evil := res
	evil.Asset.URL = "https://evil.example/YigitCittan/mongorescue/releases/download/v1.1.0/" + name
	if _, err := ck.Download(context.Background(), evil, t.TempDir()); !errors.Is(err, ErrUntrustedURL) {
		t.Errorf("untrusted: %v", err)
	}
	renamed := res
	renamed.Asset.Name = "../" + name
	if _, err := ck.Download(context.Background(), renamed, t.TempDir()); !errors.Is(err, ErrUntrustedURL) {
		t.Errorf("renamed: %v", err)
	}
	missing := res
	missing.Asset.URL = f.dl("MongoRescue-desktop_9.9.9_linux_amd64.tar.gz")
	dir := t.TempDir()
	if _, err := ck.Download(context.Background(), missing, dir); !errors.Is(err, ErrUnexpectedStatus) {
		t.Errorf("404 asset: %v", err)
	}
	assertOnly(t, dir)
}

func TestPortableAssetName(t *testing.T) {
	v := Version{1, 2, 3}
	if got, err := PortableAssetName("windows", "amd64", v); err != nil || got != "MongoRescue-desktop_1.2.3_windows_amd64_portable.zip" {
		t.Errorf("windows = %q, %v", got, err)
	}
	for _, p := range [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}, {"windows", "arm64"}} {
		if _, err := PortableAssetName(p[0], p[1], v); !errors.Is(err, ErrUnsupportedPlatform) {
			t.Errorf("PortableAssetName(%v) err = %v", p, err)
		}
	}
}

func TestDownloadPortableWithProgress(t *testing.T) {
	f := newFakeGitHub(t, "v1.1.0")
	f.release("x")
	name := "MongoRescue-desktop_1.1.0_windows_amd64_portable.zip"
	content := strings.Repeat("z", 100000)
	f.files[name] = []byte(content)
	ck := f.checker("windows", "amd64")
	res, err := ck.Check(context.Background(), "1.0.0")
	if err != nil || res.Portable.Name != name || res.Portable.URL == "" || res.Asset.Name == name {
		t.Fatalf("res %+v err %v", res, err)
	}
	var last, total int64
	calls := 0
	file, err := ck.DownloadAsset(context.Background(), res, res.Portable, t.TempDir(), func(done, size int64) {
		calls++
		last, total = done, size
	})
	if err != nil || filepath.Base(file.Path) != name {
		t.Fatalf("file %+v err %v", file, err)
	}
	if calls < 2 || last != int64(len(content)) || total != int64(len(content)) {
		t.Errorf("progress calls %d last %d total %d", calls, last, total)
	}
	// Another asset of the release is refused.
	other := Asset{Name: "MongoRescue-desktop_1.1.0_checksums.txt", URL: res.ChecksumsURL}
	if _, err = ck.DownloadAsset(context.Background(), res, other, t.TempDir(), nil); !errors.Is(err, ErrUntrustedURL) {
		t.Errorf("other asset: %v", err)
	}
	// The portable archive is Windows-only.
	lin := f.checker("linux", "amd64")
	linRes, err := lin.Check(context.Background(), "1.0.0")
	if err != nil || linRes.Portable.URL != "" {
		t.Fatalf("linux: %+v %v", linRes, err)
	}
	if _, err := lin.DownloadAsset(context.Background(), linRes, res.Portable, t.TempDir(), nil); !errors.Is(err, ErrUntrustedURL) {
		t.Errorf("portable on linux: %v", err)
	}
}

func TestFindChecksumBinaryMode(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	in := "# comment\n" + hex.EncodeToString(sum[:]) + " *a.zip\n"
	got, err := findChecksum(strings.NewReader(in), "a.zip")
	if err != nil || hex.EncodeToString(got) != hex.EncodeToString(sum[:]) {
		t.Errorf("got %x err %v", got, err)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abc", 5); got != "abc" {
		t.Errorf("got %q", got)
	}
	if got := truncate("aşb", 2); got != "a…" {
		t.Errorf("got %q", got)
	}
}

// assertOnly fails unless dir holds exactly the named files.
func assertOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Errorf("dir holds %v; want %v", got, names)
	}
}

func TestCheckRateLimited(t *testing.T) {
	type reply struct {
		status int
		hdr    map[string]string
	}
	var current atomic.Pointer[reply]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r := current.Load()
		for k, v := range r.hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(r.status)
	}))
	t.Cleanup(srv.Close)
	ck := &Checker{Client: srv.Client(), BaseURL: srv.URL}

	reset := time.Now().Add(42 * time.Minute).Truncate(time.Second)
	cases := []struct {
		name  string
		reply reply
		want  func(time.Time) bool // checks the reset time; nil: not a rate limit
	}{
		{"primary", reply{http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}},
			func(got time.Time) bool { return got.Equal(reset) }},
		{"secondary", reply{http.StatusTooManyRequests, map[string]string{"Retry-After": "120"}},
			func(got time.Time) bool { d := time.Until(got); return d > 100*time.Second && d <= 120*time.Second }},
		{"unknown reset", reply{http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}},
			func(got time.Time) bool { return got.IsZero() }},
		{"other 403", reply{http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "12"}}, nil},
	}
	for _, c := range cases {
		current.Store(&c.reply)
		_, err := ck.Check(context.Background(), "1.0.0")
		if !errors.Is(err, ErrUnexpectedStatus) {
			t.Errorf("%s: %v; want ErrUnexpectedStatus", c.name, err)
		}
		var rl *RateLimitError
		isLimit := errors.As(err, &rl)
		if isLimit != (c.want != nil) || isLimit != errors.Is(err, ErrRateLimited) {
			t.Errorf("%s: rate limit = %v (%v)", c.name, isLimit, err)
			continue
		}
		if c.want != nil && !c.want(rl.Reset) {
			t.Errorf("%s: reset %v", c.name, rl.Reset)
		}
		if c.want != nil && !strings.Contains(err.Error(), "rate limit") {
			t.Errorf("%s: message %q", c.name, err)
		}
	}
}

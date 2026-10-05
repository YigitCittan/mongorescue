package targets

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestCheckedPath covers the last check before a local target path reaches the file
// system: absolute, cleaned, no NUL or control characters and no ".." element.
func TestCheckedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"/backups", "/backups", true},
		{"/backups//daily/", "/backups/daily", true},
		{"/backups/./daily", "/backups/daily", true},
		{"/backups/v1..v2", "/backups/v1..v2", true},
		{"/backups/...", "/backups/...", true},
		{"/backups/..hidden", "/backups/..hidden", true},
		{"/backups/../etc", "", false},
		{"/backups/..", "", false},
		{"/..", "", false},
		{`/backups\..\etc`, "", false},
		{"/backups\x00/etc", "", false},
		{"/backups\n", "", false},
		{"backups", "", false},
		{"./backups", "", false},
		{"", "", false},
	} {
		got, err := checkedPath(tc.in)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Errorf("checkedPath(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("checkedPath(%q) = %q, %v; want ErrInvalid", tc.in, got, err)
		}
	}
}

// TestCleanLocalPathDotDotElements checks that only ".." elements are refused, with
// either separator, and names containing two dots are kept.
func TestCleanLocalPathDotDotElements(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"/data/backups", true},
		{" /data/v1..v2 ", true},
		{"/data/.../x", true},
		{"..", false},
		{"../backups", false},
		{"/data/../backups", false},
		{"/data/..", false},
		{`C:\data\..\backups`, false},
		{`data\..`, false},
		{"/data/\x00", false},
	} {
		_, err := cleanLocalPath(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("cleanLocalPath(%q) error = %v; want ok=%v", tc.in, err, tc.ok)
		}
	}
}

// TestLocalLocationsFoldCaseAndSymlinks checks that local locations compare equal
// through symbolic links, and through case where the file system ignores it.
func TestLocalLocationsFoldCaseAndSymlinks(t *testing.T) {
	base := t.TempDir()
	svc := NewService(nil, nil, filepath.Join(base, "data"))
	local := func(p string) *models.StorageTarget {
		return &models.StorageTarget{Type: models.StorageLocal, Local: &models.LocalTarget{Path: p}}
	}
	realDir := filepath.Join(base, "Backups")
	if err := os.MkdirAll(filepath.Join(realDir, "daily"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if a, b := svc.baseLocation(local(realDir)), svc.baseLocation(local(link)); a != b {
		t.Errorf("symlink: %q and %q; want equal", a, b)
	}
	if a, b := svc.baseLocation(local(filepath.Join(realDir, "daily"))), svc.baseLocation(local(filepath.Join(link, "daily"))); a != b {
		t.Errorf("symlink, nested: %q and %q; want equal", a, b)
	}
	defer func(v bool) { foldLocalCase = v }(foldLocalCase)
	foldLocalCase = true
	upper, lower := svc.baseLocation(local(filepath.Join(base, "Archive", "X"))), svc.baseLocation(local(filepath.Join(base, "archive", "x")))
	if upper != lower {
		t.Errorf("case-insensitive: %q and %q; want equal", upper, lower)
	}
	if !strings.HasSuffix(svc.objectKey(local(realDir), "Shop/A.archive"), "shop/a.archive") {
		t.Error("object keys of local targets are not folded")
	}
	foldLocalCase = false
	if runtime.GOOS == "linux" {
		upper, lower = svc.baseLocation(local(filepath.Join(base, "Archive"))), svc.baseLocation(local(filepath.Join(base, "archive")))
		if upper == lower {
			t.Errorf("case-sensitive: %q equals %q", upper, lower)
		}
	}
}

// TestEndpointKey covers the S3 endpoint normalisation behind the overlap check and
// the purge's physical-object check.
func TestEndpointKey(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"", "https://s3.eu-west-1.amazonaws.com"},
		{"", "https://s3.amazonaws.com"},
		{"aws", "https://s3.dualstack.us-east-1.amazonaws.com/"},
		{"", "https://bucket-a.s3.eu-west-1.amazonaws.com"},
		{"", "https://S3.AmazonAWS.com.:443"},
		{"https://minio:9000", "http://minio:9000"},
		{"https://minio:9000", "minio:9000/"},
		{"https://x", "https://x:443"},
		{"https://x", "http://x:80"},
		{"https://X.example.com.", "https://x.example.com//"},
		{"http://[::1]:9000", "https://[::1]:9000/"},
	} {
		if ka, kb := endpointKey(tc.a), endpointKey(tc.b); ka != kb {
			t.Errorf("endpointKey(%q) = %q, endpointKey(%q) = %q; want equal", tc.a, ka, tc.b, kb)
		}
	}
	for _, tc := range []struct{ a, b string }{
		{"", "https://minio:9000"},
		{"https://minio:9000", "https://minio:9001"},
		{"https://x", "https://x:8443"},
		{"https://x/a", "https://x/b"},
		{"https://s3.example.com", "https://s3.amazonaws.example.com"},
	} {
		if ka, kb := endpointKey(tc.a), endpointKey(tc.b); ka == kb {
			t.Errorf("endpointKey(%q) = endpointKey(%q) = %q; want different", tc.a, tc.b, ka)
		}
	}
}

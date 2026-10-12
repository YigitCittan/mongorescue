package config

import (
	"errors"
	"net/url"
	"path/filepath"
	"runtime"
	"testing"
)

// TestFileTargetPathOnEveryOS reads file:// locations as Windows and as Unix would,
// on every OS: drive letters, UNC shares and localhost on Windows; absolute paths
// only elsewhere.
func TestFileTargetPathOnEveryOS(t *testing.T) {
	for _, tc := range []struct {
		url     string
		windows string // "" when refused
		unix    string // "" when refused
	}{
		{"file:///srv/mongorescue-ha", "", "/srv/mongorescue-ha"},
		{"file://localhost/srv/ha", "", "/srv/ha"},
		{"file:///srv/ha/../ha2/", "", "/srv/ha2"},
		// On Unix "/C:/dir" is an ordinary absolute path (a directory named "C:").
		{"file:///C:/mongorescue-ha", `C:\mongorescue-ha`, "/C:/mongorescue-ha"},
		{"file:///c:/Program%20Data/ha", `c:\Program Data\ha`, "/c:/Program Data/ha"},
		{"file:///D:/a/../b/", `D:\b`, "/D:/b"},
		{"file://C:/mongorescue-ha", `C:\mongorescue-ha`, ""},
		{"file://localhost/C:/ha", `C:\ha`, "/C:/ha"},
		{"file://fileserver/share/mongorescue-ha", `\\fileserver\share\mongorescue-ha`, ""},
		{"file://fileserver/share", `\\fileserver\share`, ""},
		{"file://fileserver/", "", ""},
		{"file:///C:", "", "/C:"},
		{"file:///C:/..", "", "/"},
		{"file://relative/dir", `\\relative\dir`, ""},
		{"file:relative/dir", "", ""},
	} {
		u, err := url.Parse(tc.url)
		if err != nil {
			t.Errorf("url.Parse(%q): %v", tc.url, err)
			continue
		}
		for _, os := range []struct{ goos, want string }{{"windows", tc.windows}, {"linux", tc.unix}} {
			got, err := fileTargetPath(u.Host, u.Path, os.goos)
			switch {
			case os.want == "" && !errors.Is(err, ErrInvalidHA):
				t.Errorf("%s on %s = %q, %v; want refused", tc.url, os.goos, got, err)
			case os.want != "" && (err != nil || got != os.want):
				t.Errorf("%s on %s = %q, %v; want %q", tc.url, os.goos, got, err, os.want)
			}
		}
	}
}

// TestFileURLRoundTrip checks that FileURL builds, for a directory of this OS, a
// location ParseHATarget reads back as the same directory.
func TestFileURLRoundTrip(t *testing.T) {
	dirs := []string{t.TempDir()}
	if runtime.GOOS == "windows" {
		dirs = append(dirs, `C:\mongorescue-ha`, `\\fileserver\share\mongorescue-ha`)
	} else {
		dirs = append(dirs, "/srv/mongorescue-ha")
	}
	for _, dir := range dirs {
		raw := FileURL(dir)
		tg, err := ParseHATarget(raw)
		if err != nil {
			t.Errorf("ParseHATarget(FileURL(%q) = %q): %v", dir, raw, err)
			continue
		}
		if tg.Scheme != "file" || tg.Path != filepath.Clean(dir) {
			t.Errorf("ParseHATarget(%q) = %+v; want the path %q", raw, tg, filepath.Clean(dir))
		}
		if tg.String() != raw {
			t.Errorf("label %q; want %q", tg.String(), raw)
		}
	}
}

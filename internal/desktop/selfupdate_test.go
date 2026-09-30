package desktop

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/update"
)

// zipEntry is a file of a test archive; a name ending in "/" is a directory.
type zipEntry struct {
	name, body string
	mode       fs.FileMode
}

// zipBytes returns an archive with entries.
func zipBytes(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		switch {
		case e.mode != 0:
			h.SetMode(e.mode)
		case strings.HasSuffix(e.name, "/"):
			h.SetMode(fs.ModeDir | 0o755)
		default:
			h.SetMode(0o644)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// portableZip returns a portable archive of version v.
func portableZip(t *testing.T, v string) []byte {
	t.Helper()
	return zipBytes(t,
		zipEntry{name: "MongoRescue.exe", body: "exe " + v},
		zipEntry{name: "tools/"},
		zipEntry{name: "tools/mongodump.exe", body: "dump " + v},
		zipEntry{name: `tools\mongorestore.exe`, body: "restore " + v},
		zipEntry{name: "tools/LICENSE.md", body: "license"},
		zipEntry{name: "tools/THIRD-PARTY-NOTICES", body: "notices"},
	)
}

// writeArchive writes b to dir/name and returns it as a verified update.File.
func writeArchive(t *testing.T, dir, name string, b []byte) update.File {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return update.File{Path: p, SHA256: sum[:]}
}

// install creates an installed copy in a new directory: exe and tools/.
func installCopy(t *testing.T, v string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Programs", "MongoRescue")
	writeFiles(t, dir, map[string]string{
		"MongoRescue.exe":        "exe " + v,
		"tools/mongodump.exe":    "dump " + v,
		"tools/mongorestore.exe": "restore " + v,
	})
	return filepath.Join(dir, "MongoRescue.exe")
}

// writeFiles writes the files (slash-separated names) below dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// assertFiles fails unless the files below dir (slash-separated, recursive) are
// exactly want.
func assertFiles(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		got[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("files in %s:\n got %v\nwant %v", dir, got, want)
	}
}

func TestParseAfterUpdate(t *testing.T) {
	cases := []struct {
		in   []string
		pid  int
		rest string
	}{
		{nil, 0, ""},
		{[]string{"-data-dir", "D"}, 0, "-data-dir D"},
		{[]string{"--after-update=42", "-log-level", "debug"}, 42, "-log-level debug"},
		{[]string{"-after-update", "7"}, 7, ""},
		{[]string{"-data-dir=X", "--after-update=abc"}, 0, "-data-dir=X"},
		{[]string{"--after-update=-3"}, 0, ""},
	}
	for _, c := range cases {
		pid, rest := ParseAfterUpdate(c.in)
		if pid != c.pid || strings.Join(rest, " ") != c.rest {
			t.Errorf("ParseAfterUpdate(%q) = %d %q; want %d %q", c.in, pid, rest, c.pid, c.rest)
		}
	}
}

func TestRelaunchArgs(t *testing.T) {
	got := relaunchArgs([]string{"-data-dir", `D:\data`, "--after-update=11"}, 4242)
	if strings.Join(got, "|") != `-data-dir|D:\data|--after-update=4242` {
		t.Errorf("relaunchArgs = %q", got)
	}
	if got := relaunchArgs(nil, 1); strings.Join(got, "|") != "--after-update=1" {
		t.Errorf("relaunchArgs(nil) = %q", got)
	}
}

func TestSilentInstallerArgs(t *testing.T) {
	cases := []struct {
		user string
		pid  int
		want string
	}{
		{`PC\alice`, 42, `/S|/RELAUNCH=PC\alice|/WAITPID=42`},
		{`CORP\John Smith`, 7, `/S|/RELAUNCH=CORP\John Smith|/WAITPID=7`},
		// Without a usable user name the installer is not asked to start the app,
		// so it can never start it as another account.
		{"", 42, "/S|/WAITPID=42"},
		{"  ", 42, "/S|/WAITPID=42"},
		{`PC\a"b`, 42, "/S|/WAITPID=42"},
		{`PC\a /S`, 42, "/S|/WAITPID=42"},
		{"PC\\a\nb", 0, "/S"},
	}
	for _, c := range cases {
		if got := strings.Join(silentInstallerArgs(c.user, c.pid), "|"); got != c.want {
			t.Errorf("silentInstallerArgs(%q, %d) = %q; want %q", c.user, c.pid, got, c.want)
		}
	}
}

func TestDirWritable(t *testing.T) {
	dir := t.TempDir()
	if !DirWritable(dir) {
		t.Fatal("temporary directory not writable")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("probe left %v", entries)
	}
	if DirWritable(filepath.Join(dir, "missing")) {
		t.Error("missing directory reported writable")
	}
}

func TestVerifyAndExtract(t *testing.T) {
	file := writeArchive(t, t.TempDir(), "portable.zip", portableZip(t, "2"))
	staging := filepath.Join(t.TempDir(), ".update-2.0.0")
	if err := verifyAndExtract(file, staging, "MongoRescue.exe"); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, staging, map[string]string{
		"MongoRescue.exe":           "exe 2",
		"tools/mongodump.exe":       "dump 2",
		"tools/mongorestore.exe":    "restore 2",
		"tools/LICENSE.md":          "license",
		"tools/THIRD-PARTY-NOTICES": "notices",
	})

	// The executable is named after the running one.
	renamed := filepath.Join(t.TempDir(), "s")
	if err := verifyAndExtract(file, renamed, "MongoRescue (1).exe"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(renamed, "MongoRescue (1).exe")); err != nil {
		t.Error(err)
	}
}

func TestVerifyAndExtractChecksumMismatch(t *testing.T) {
	file := writeArchive(t, t.TempDir(), "portable.zip", portableZip(t, "2"))
	if err := os.WriteFile(file.Path, portableZip(t, "evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(t.TempDir(), "staging")
	if err := verifyAndExtract(file, staging, "MongoRescue.exe"); !errors.Is(err, update.ErrChecksumMismatch) {
		t.Fatalf("err = %v; want ErrChecksumMismatch", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging created for a mismatching archive: %v", err)
	}
	if err := verifyAndExtract(update.File{Path: file.Path}, staging, "MongoRescue.exe"); !errors.Is(err, update.ErrChecksumMismatch) {
		t.Errorf("no checksum: %v", err)
	}
	notZip := writeArchive(t, t.TempDir(), "portable.zip", []byte("not a zip"))
	if err := verifyAndExtract(notZip, staging, "MongoRescue.exe"); !errors.Is(err, ErrBadUpdateArchive) {
		t.Errorf("not a zip: %v", err)
	}
}

func TestExtractRejectsUnexpectedEntries(t *testing.T) {
	exe := zipEntry{name: "MongoRescue.exe", body: "exe"}
	cases := map[string][]zipEntry{
		"zip slip":           {exe, {name: "../evil.exe", body: "x"}},
		"zip slip in tools":  {exe, {name: "tools/../../evil.exe", body: "x"}},
		"backslash slip":     {exe, {name: `..\evil.exe`, body: "x"}},
		"absolute":           {exe, {name: "/etc/evil", body: "x"}},
		"drive letter":       {exe, {name: "C:/Windows/evil.exe", body: "x"}},
		"unknown file":       {exe, {name: "evil.dll", body: "x"}},
		"unknown tool":       {exe, {name: "tools/evil.exe", body: "x"}},
		"nested dir":         {exe, {name: "tools/sub/mongodump.exe", body: "x"}},
		"other dir":          {exe, {name: "sub/"}},
		"duplicate":          {exe, {name: "MongoRescue.exe", body: "again"}},
		"case duplicate":     {exe, {name: "tools/mongodump.exe", body: "a"}, {name: "tools/mongodump.exe", body: "b"}},
		"symlink":            {{name: "MongoRescue.exe", body: "/bin/sh", mode: fs.ModeSymlink | 0o777}},
		"exe as a directory": {{name: "MongoRescue.exe", mode: fs.ModeDir | 0o755}},
		"no executable":      {{name: "tools/mongodump.exe", body: "x"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			file := writeArchive(t, root, "portable.zip", zipBytes(t, entries...))
			staging := filepath.Join(root, "app", ".update-2.0.0")
			if err := verifyAndExtract(file, staging, "MongoRescue.exe"); !errors.Is(err, ErrBadUpdateArchive) {
				t.Fatalf("err = %v; want ErrBadUpdateArchive", err)
			}
			for _, p := range []string{filepath.Join(root, "evil.exe"), filepath.Join(root, "app", "evil.exe"), filepath.Join(root, "evil.dll")} {
				if _, err := os.Lstat(p); err == nil {
					t.Errorf("%s written outside the staging directory", p)
				}
			}
		})
	}

	many := []zipEntry{exe}
	for i := 0; i < maxArchiveEntries; i++ {
		many = append(many, zipEntry{name: "tools/"})
	}
	file := writeArchive(t, t.TempDir(), "portable.zip", zipBytes(t, many...))
	if err := verifyAndExtract(file, filepath.Join(t.TempDir(), "s"), "MongoRescue.exe"); !errors.Is(err, ErrBadUpdateArchive) {
		t.Errorf("too many entries: %v", err)
	}
}

// failingOps returns the real file system calls, with the n-th rename (from 1)
// failing; n = 0 never fails. calls counts the renames.
func failingOps(n int, calls *int) fileOps {
	ops := osFileOps()
	ops.rename = func(oldpath, newpath string) error {
		*calls++
		if *calls == n {
			return fmt.Errorf("rename %s: %w", oldpath, fs.ErrPermission)
		}
		return os.Rename(oldpath, newpath)
	}
	return ops
}

func TestSwapInFiles(t *testing.T) {
	exe := installCopy(t, "1")
	dir := filepath.Dir(exe)
	staging := stagingDir(dir, update.Version{Major: 2})
	writeFiles(t, staging, map[string]string{"MongoRescue.exe": "exe 2", "tools/mongodump.exe": "dump 2"})
	// A leftover of an earlier update is replaced.
	writeFiles(t, dir, map[string]string{"MongoRescue.exe.old": "exe 0"})
	calls := 0
	sw, err := swapInFiles(failingOps(0, &calls), exe, staging)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Errorf("renames = %d; want 4", calls)
	}
	assertFiles(t, dir, map[string]string{
		"MongoRescue.exe":            "exe 2",
		"MongoRescue.exe.old":        "exe 1",
		"tools/mongodump.exe":        "dump 2",
		"tools.old/mongodump.exe":    "dump 1",
		"tools.old/mongorestore.exe": "restore 1",
	})
	// A rollback after the swap (the new version did not start) restores the old one.
	if err = sw.rollback(); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, map[string]string{
		"MongoRescue.exe":                   "exe 1",
		"tools/mongodump.exe":               "dump 1",
		"tools/mongorestore.exe":            "restore 1",
		".update-2.0.0/MongoRescue.exe":     "exe 2",
		".update-2.0.0/tools/mongodump.exe": "dump 2",
	})
}

func TestSwapInFilesWithoutTools(t *testing.T) {
	// A portable copy without tools gets the new tools; an archive without tools
	// leaves the old ones.
	root := t.TempDir()
	exe := filepath.Join(root, "MongoRescue.exe")
	writeFiles(t, root, map[string]string{"MongoRescue.exe": "exe 1", ".update-2.0.0/MongoRescue.exe": "exe 2", ".update-2.0.0/tools/mongodump.exe": "dump 2"})
	calls := 0
	if _, err := swapInFiles(failingOps(0, &calls), exe, filepath.Join(root, ".update-2.0.0")); err != nil || calls != 3 {
		t.Fatalf("err %v renames %d", err, calls)
	}
	assertFiles(t, root, map[string]string{"MongoRescue.exe": "exe 2", "MongoRescue.exe.old": "exe 1", "tools/mongodump.exe": "dump 2"})

	exe = installCopy(t, "1")
	staging := filepath.Join(filepath.Dir(exe), ".update-2.0.0")
	writeFiles(t, staging, map[string]string{"MongoRescue.exe": "exe 2"})
	calls = 0
	if _, err := swapInFiles(failingOps(0, &calls), exe, staging); err != nil || calls != 2 {
		t.Fatalf("err %v renames %d", err, calls)
	}
	if b, _ := os.ReadFile(filepath.Join(filepath.Dir(exe), "tools", "mongodump.exe")); string(b) != "dump 1" {
		t.Errorf("tools = %q; want the old ones", b)
	}
}

func TestSwapInFilesRollsBackEachStep(t *testing.T) {
	for step := 1; step <= 4; step++ {
		t.Run(fmt.Sprint("rename ", step), func(t *testing.T) {
			exe := installCopy(t, "1")
			dir := filepath.Dir(exe)
			staging := stagingDir(dir, update.Version{Major: 2})
			writeFiles(t, staging, map[string]string{"MongoRescue.exe": "exe 2", "tools/mongodump.exe": "dump 2"})
			calls := 0
			sw, err := swapInFiles(failingOps(step, &calls), exe, staging)
			if err == nil || sw != nil || !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("err = %v", err)
			}
			assertFiles(t, dir, map[string]string{
				"MongoRescue.exe":                   "exe 1",
				"tools/mongodump.exe":               "dump 1",
				"tools/mongorestore.exe":            "restore 1",
				".update-2.0.0/MongoRescue.exe":     "exe 2",
				".update-2.0.0/tools/mongodump.exe": "dump 2",
			})
		})
	}
}

func TestSwapInFilesReportsFailedRollback(t *testing.T) {
	exe := installCopy(t, "1")
	staging := stagingDir(filepath.Dir(exe), update.Version{Major: 2})
	writeFiles(t, staging, map[string]string{"MongoRescue.exe": "exe 2", "tools/mongodump.exe": "dump 2"})
	ops := osFileOps()
	calls := 0
	ops.rename = func(oldpath, newpath string) error {
		calls++
		if calls >= 3 { // the tools move and every rollback rename fail
			return fs.ErrPermission
		}
		return os.Rename(oldpath, newpath)
	}
	if _, err := swapInFiles(ops, exe, staging); err == nil || !strings.Contains(err.Error(), "roll back") {
		t.Fatalf("err = %v; want the rollback failure reported", err)
	}
}

func TestSwapInFilesNeedsStagedExecutable(t *testing.T) {
	exe := installCopy(t, "1")
	calls := 0
	if _, err := swapInFiles(failingOps(0, &calls), exe, filepath.Join(filepath.Dir(exe), ".update-2.0.0")); err == nil || calls != 0 {
		t.Fatalf("err %v renames %d", err, calls)
	}
}

func TestRemoveLeftovers(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"MongoRescue.exe":                 "exe 2",
		"MongoRescue.exe.old":             "exe 1",
		"tools/mongodump.exe":             "dump 2",
		"tools.old/mongodump.exe":         "dump 1",
		".update-2.0.0/MongoRescue.exe":   "exe 2",
		".update-1.9.0/tools/LICENSE.md":  "x",
		"notes.old":                       "user file",
		"Other.exe.old":                   "user file",
		".update-notes":                   "a file, not a staging directory",
		"backups/.update-3.0.0/keep.txt":  "nested",
		"MongoRescue.exe.old.config/keep": "user dir",
	})
	removed := removeLeftovers(osFileOps(), filepath.Join(root, "MongoRescue.exe"))
	if len(removed) != 4 {
		t.Errorf("removed %v", removed)
	}
	assertFiles(t, root, map[string]string{
		"MongoRescue.exe":                 "exe 2",
		"tools/mongodump.exe":             "dump 2",
		"notes.old":                       "user file",
		"Other.exe.old":                   "user file",
		".update-notes":                   "a file, not a staging directory",
		"backups/.update-3.0.0/keep.txt":  "nested",
		"MongoRescue.exe.old.config/keep": "user dir",
	})
	// Best effort: a file that cannot be removed (the old process still runs) is
	// skipped.
	writeFiles(t, root, map[string]string{"MongoRescue.exe.old": "exe 1"})
	ops := osFileOps()
	ops.removeAll = func(string) error { return fs.ErrPermission }
	if removed := removeLeftovers(ops, filepath.Join(root, "MongoRescue.exe")); len(removed) != 0 {
		t.Errorf("removed %v", removed)
	}
}

func TestParseUninstallString(t *testing.T) {
	cases := map[string]string{
		`"C:\Program Files\MongoRescue\MongoRescue\uninstall.exe"`:    `C:\Program Files\MongoRescue\MongoRescue\uninstall.exe`,
		`"C:\Program Files\MongoRescue\MongoRescue\uninstall.exe" /S`: `C:\Program Files\MongoRescue\MongoRescue\uninstall.exe`,
		`C:\Program Files\MongoRescue\Uninstall.EXE`:                  `C:\Program Files\MongoRescue\Uninstall.EXE`,
		`C:\Apps\uninstall.exe /S`:                                    `C:\Apps\uninstall.exe`,
		`"C:\Windows\System32\cmd.exe" /c evil`:                       "",
		`uninstall.exe`:                                               "",
		``:                                                            "",
		`"C:\x\uninstall.exe`:                                         `C:\x\uninstall.exe`,
	}
	for in, want := range cases {
		if got := parseUninstallString(in); got != want {
			t.Errorf("parseUninstallString(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestSameWindowsDir(t *testing.T) {
	if !sameWindowsDir(`C:\Program Files\MongoRescue\MongoRescue`, `c:/program files/mongorescue/MongoRescue\`) {
		t.Error("same directory not matched")
	}
	if sameWindowsDir(`C:\Program Files\MongoRescue`, `C:\Users\a\AppData\Local\Programs\MongoRescue`) || sameWindowsDir("", "") {
		t.Error("different directories matched")
	}
}

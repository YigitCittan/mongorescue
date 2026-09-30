package desktop

import (
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fuzzArchiveNames are archive entry names from the table tests plus tricky shapes.
var fuzzArchiveNames = []string{
	"MongoRescue.exe", "tools/", "tools/mongodump.exe", "tools/mongorestore",
	"../evil.exe", "tools/../../evil.exe", `..\evil.exe`, `tools\mongodump.exe`, "/etc/evil",
	"C:/Windows/evil.exe", "C:evil", "evil.dll", "tools/CON", "tools/nul.txt", "tools/com1.tar.gz",
	"tools/a.exe:evil", "tools/.hidden", "tools/mongodump.exe.", "tools/..", "tools/a b.exe",
	"tools/sub/mongodump.exe", "sub/", "tools//x", "tools/./x", "./MongoRescue.exe",
	"TOOLS/x", "mongorescue.exe", "tools/x\x00y",
}

// FuzzArchiveTarget checks that every entry name archiveTarget accepts maps to the
// executable or to a plain file directly in tools/, a local path without "..".
func FuzzArchiveTarget(f *testing.F) {
	for _, n := range fuzzArchiveNames {
		f.Add(n)
	}
	f.Fuzz(func(t *testing.T, name string) {
		target, err := archiveTarget(name, "MongoRescue.exe")
		if err != nil {
			if !errors.Is(err, ErrBadUpdateArchive) {
				t.Fatalf("archiveTarget(%q): unexpected error %v", name, err)
			}
			return
		}
		if target == "" || target == "MongoRescue.exe" {
			return
		}
		base, ok := strings.CutPrefix(target, toolsDirName+"/")
		if !ok || !validToolName(base) || strings.ContainsAny(base, `/\:`) || !filepath.IsLocal(target) ||
			strings.Contains(target, "..") {
			t.Fatalf("archiveTarget(%q) = %q, not a tool file", name, target)
		}
	})
}

// fuzzZip returns an archive with a file for each name ("/"-suffixed names are
// directories); body is the content of every file.
func fuzzZip(t testing.TB, body []byte, names ...string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		h := &zip.FileHeader{Name: n, Method: zip.Deflate}
		if strings.HasSuffix(n, "/") {
			h.SetMode(fs.ModeDir | 0o755)
		} else {
			h.SetMode(0o644)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(n, "/") {
			if _, err := w.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// checkExtract unpacks data as a portable archive into a staging directory inside
// a fresh temporary directory and fails if anything was written outside staging,
// or if a successful extraction produced files other than the allowed ones.
func checkExtract(t *testing.T, data []byte) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return
	}
	root := t.TempDir()
	app := filepath.Join(root, "app")
	staging := filepath.Join(app, ".update-2.0.0")
	extractErr := extractArchive(zr, staging, "MongoRescue.exe")

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == root || p == app || p == staging {
			return nil
		}
		rel, err := filepath.Rel(staging, p)
		if err != nil || !filepath.IsLocal(rel) {
			t.Errorf("%s written outside the staging directory", p)
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			t.Errorf("symlink %s created", rel)
		}
		if extractErr == nil {
			switch rel = filepath.ToSlash(rel); {
			case rel == "MongoRescue.exe", rel == stagingMarker, rel == completeMarker, rel == toolsDirName:
			case strings.HasPrefix(rel, toolsDirName+"/") && validToolName(strings.TrimPrefix(rel, toolsDirName+"/")):
			default:
				t.Errorf("unexpected file %s after a successful extraction", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if extractErr == nil {
		if _, err := os.Stat(filepath.Join(staging, "MongoRescue.exe")); err != nil {
			t.Errorf("successful extraction without the executable: %v", err)
		}
	}
}

// FuzzExtractArchive checks that no archive, however malformed, writes outside the
// staging directory.
func FuzzExtractArchive(f *testing.F) {
	f.Add(fuzzZip(f, []byte("exe"), "MongoRescue.exe", "tools/", "tools/mongodump.exe"))
	f.Add(fuzzZip(f, []byte("x"), "MongoRescue.exe", "../evil.exe"))
	f.Add(fuzzZip(f, []byte("x"), "MongoRescue.exe", `..\evil.exe`))
	f.Add(fuzzZip(f, []byte("x"), "tools/../../evil.exe", "MongoRescue.exe"))
	f.Add(fuzzZip(f, nil, "MongoRescue.exe", "tools/Mongodump.exe", "tools/mongodump.EXE"))
	f.Add([]byte("PK\x05\x06\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		checkExtract(t, data)
	})
}

// FuzzExtractArchiveNames builds well-formed archives from newline-separated entry
// names, so the fuzzer explores names rather than the zip format.
func FuzzExtractArchiveNames(f *testing.F) {
	f.Add("MongoRescue.exe\ntools/\ntools/mongodump.exe", []byte("exe"))
	for _, n := range fuzzArchiveNames {
		f.Add("MongoRescue.exe\n"+n, []byte("x"))
	}
	f.Fuzz(func(t *testing.T, names string, body []byte) {
		list := strings.Split(names, "\n")
		if len(list) > maxArchiveEntries+1 {
			return
		}
		checkExtract(t, fuzzZip(t, body, list...))
	})
}

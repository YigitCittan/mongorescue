package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// newTestLocalStorage returns a LocalStorage rooted in a fresh temporary directory.
func newTestLocalStorage(t *testing.T) (*LocalStorage, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	return store, dir
}

// dirEntries returns the names of all files below dir (relative, slash-separated).
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return names
}

func TestNewLocalStorageErrors(t *testing.T) {
	if _, err := NewLocalStorage("  "); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("empty base dir: expected ErrInvalidKey, got %v", err)
	}

	// A regular file cannot become the storage root.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalStorage(filepath.Join(file, "sub")); err == nil {
		t.Fatal("expected an error when the base directory cannot be created")
	}
}

func TestLocalStorageMissingKey(t *testing.T) {
	store, _ := newTestLocalStorage(t)
	ctx := context.Background()

	tests := []struct {
		name string
		op   func(key string) error
	}{
		{"Retrieve", func(k string) error { _, err := store.Retrieve(ctx, k); return err }},
		{"Stat", func(k string) error { _, err := store.Stat(ctx, k); return err }},
		{"Delete", func(k string) error { return store.Delete(ctx, k) }},
	}
	for _, tt := range tests {
		for _, key := range []string{"missing.gz", "db/2026/09/missing.gz"} {
			t.Run(tt.name+"/"+key, func(t *testing.T) {
				if err := tt.op(key); !errors.Is(err, ErrNotFound) {
					t.Fatalf("expected ErrNotFound, got %v", err)
				}
			})
		}
	}
}

func TestLocalStorageRejectsUnsafeKeys(t *testing.T) {
	store, dir := newTestLocalStorage(t)
	ctx := context.Background()

	tests := []struct {
		key     string
		wantErr error
	}{
		{"", ErrInvalidKey},
		{"   ", ErrInvalidKey},
		{".", ErrInvalidKey},
		{"db/..", ErrInvalidKey},
		{"..", ErrPathTraversal},
		{"../escape.gz", ErrPathTraversal},
		{"db/../../escape.gz", ErrPathTraversal},
		{"/etc/passwd", ErrPathTraversal},
		{"/", ErrPathTraversal},
	}

	ops := map[string]func(key string) error{
		"Save": func(k string) error {
			_, err := store.Save(ctx, k, strings.NewReader("malicious"))
			return err
		},
		"Retrieve": func(k string) error { _, err := store.Retrieve(ctx, k); return err },
		"Stat":     func(k string) error { _, err := store.Stat(ctx, k); return err },
		"Delete":   func(k string) error { return store.Delete(ctx, k) },
	}

	for _, tt := range tests {
		for name, op := range ops {
			t.Run(name+"/"+tt.key, func(t *testing.T) {
				if err := op(tt.key); !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
			})
		}
	}

	if names := dirEntries(t, dir); len(names) != 0 {
		t.Fatalf("rejected keys must not create files, found %v", names)
	}
	if names := dirEntries(t, filepath.Dir(dir)); len(names) != 0 {
		t.Fatalf("rejected keys must not create files next to the root, found %v", names)
	}
}

func TestLocalStorageKeyNormalization(t *testing.T) {
	store, dir := newTestLocalStorage(t)
	ctx := context.Background()

	obj, err := store.Save(ctx, " db/./2026//x.gz ", strings.NewReader("data"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := dirEntries(t, dir); !slices.Equal(got, []string{"db/2026/x.gz"}) {
		t.Fatalf("stored files = %v, want [db/2026/x.gz]", got)
	}
	if obj.SizeBytes != 4 {
		t.Fatalf("size = %d, want 4", obj.SizeBytes)
	}

	stat, err := store.Stat(ctx, "db/2026/x.gz")
	if err != nil || stat.Key != "db/2026/x.gz" || stat.SizeBytes != 4 {
		t.Fatalf("Stat = %+v, %v", stat, err)
	}
}

func TestLocalStorageSaveOverwrites(t *testing.T) {
	store, _ := newTestLocalStorage(t)
	ctx := context.Background()

	for _, content := range []string{"first version", "v2"} {
		if _, err := store.Save(ctx, "db/x.gz", strings.NewReader(content)); err != nil {
			t.Fatalf("Save %q: %v", content, err)
		}
	}
	rc, err := store.Retrieve(ctx, "db/x.gz")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(data) != "v2" {
		t.Fatalf("content = %q, %v; want v2", data, err)
	}
}

func TestLocalStorageSaveParentIsFile(t *testing.T) {
	store, _ := newTestLocalStorage(t)
	ctx := context.Background()

	if _, err := store.Save(ctx, "db", strings.NewReader("file")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := store.Save(ctx, "db/child.gz", strings.NewReader("x")); err == nil {
		t.Fatal("expected an error when a parent path component is a file")
	}
}

// failingReader yields data once and then fails with err.
type failingReader struct {
	data []byte
	err  error
	done bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.done {
		return 0, f.err
	}
	f.done = true
	return copy(p, f.data), nil
}

// cancelingReader cancels its context after the first chunk and keeps producing data.
type cancelingReader struct {
	cancel context.CancelFunc
}

func (c *cancelingReader) Read(p []byte) (int, error) {
	c.cancel()
	return copy(p, "chunk"), nil
}

func TestLocalStorageSaveCleansUpOnFailure(t *testing.T) {
	readErr := errors.New("upstream mongodump crashed")

	tests := []struct {
		name    string
		reader  func(cancel context.CancelFunc) io.Reader
		wantErr error
	}{
		{
			name:    "reader error mid-stream",
			reader:  func(context.CancelFunc) io.Reader { return &failingReader{data: []byte("partial"), err: readErr} },
			wantErr: readErr,
		},
		{
			name:    "context canceled mid-stream",
			reader:  func(cancel context.CancelFunc) io.Reader { return &cancelingReader{cancel: cancel} },
			wantErr: context.Canceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, dir := newTestLocalStorage(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			_, err := store.Save(ctx, "db/2026/09/x.gz", tt.reader(cancel))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, err)
			}
			if names := dirEntries(t, dir); len(names) != 0 {
				t.Fatalf("failed save must leave no files behind, found %v", names)
			}
			if _, err := store.Stat(context.Background(), "db/2026/09/x.gz"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("expected ErrNotFound after failed save, got %v", err)
			}
		})
	}
}

func TestLocalStorageList(t *testing.T) {
	store, dir := newTestLocalStorage(t)
	ctx := context.Background()

	keys := []string{
		"orders/2026/08/a.gz",
		"orders/2026/09/b.gz",
		"orders/2026/09/c.gz",
		"orders_archive/2026/09/d.gz",
		"users/2026/09/e.gz",
		"root.gz",
	}
	for _, k := range keys {
		if _, err := store.Save(ctx, k, strings.NewReader(k)); err != nil {
			t.Fatalf("Save %s: %v", k, err)
		}
	}
	// In-flight temporary files and empty directories are not objects.
	if err := os.WriteFile(filepath.Join(dir, "orders", "2026", "09", "f.gz.tmp.123"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "empty", "dir"), 0o750); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		prefix string
		want   []string
	}{
		{"", keys},
		{"orders/", []string{"orders/2026/08/a.gz", "orders/2026/09/b.gz", "orders/2026/09/c.gz"}},
		{"orders", []string{"orders/2026/08/a.gz", "orders/2026/09/b.gz", "orders/2026/09/c.gz", "orders_archive/2026/09/d.gz"}},
		{"orders/2026/09", []string{"orders/2026/09/b.gz", "orders/2026/09/c.gz"}},
		{"/users/", []string{"users/2026/09/e.gz"}},
		{"root", []string{"root.gz"}},
		{"nothing/", nil},
	}
	for _, tt := range tests {
		t.Run("prefix="+tt.prefix, func(t *testing.T) {
			objs, err := store.List(ctx, tt.prefix)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			got := make([]string, 0, len(objs))
			for _, o := range objs {
				if o.SizeBytes != int64(len(o.Key)) {
					t.Errorf("%s: size = %d, want %d", o.Key, o.SizeBytes, len(o.Key))
				}
				got = append(got, o.Key)
			}
			want := slices.Clone(tt.want)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("List(%q) = %v, want %v", tt.prefix, got, want)
			}
		})
	}
}

func TestLocalStorageListMissingRoot(t *testing.T) {
	store, dir := newTestLocalStorage(t)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(context.Background(), ""); err == nil {
		t.Fatal("expected an error when the storage root has disappeared")
	}
}

func TestLocalStorageCanceledContext(t *testing.T) {
	store, _ := newTestLocalStorage(t)
	if _, err := store.Save(context.Background(), "db/x.gz", strings.NewReader("data")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ops := map[string]func() error{
		"Save":     func() error { _, err := store.Save(ctx, "db/y.gz", strings.NewReader("data")); return err },
		"Retrieve": func() error { _, err := store.Retrieve(ctx, "db/x.gz"); return err },
		"Stat":     func() error { _, err := store.Stat(ctx, "db/x.gz"); return err },
		"Delete":   func() error { return store.Delete(ctx, "db/x.gz") },
		"List":     func() error { _, err := store.List(ctx, ""); return err },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			if err := op(); !errors.Is(err, context.Canceled) {
				t.Fatalf("expected context.Canceled, got %v", err)
			}
		})
	}

	// Nothing was deleted or written by the cancelled calls.
	objs, err := store.List(context.Background(), "")
	if err != nil || len(objs) != 1 || objs[0].Key != "db/x.gz" {
		t.Fatalf("List after cancelled calls = %v, %v", objs, err)
	}
}

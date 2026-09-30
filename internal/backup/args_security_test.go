package backup

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// captureRunner records the tool name and arguments of every run.
type captureRunner struct {
	mu   sync.Mutex
	name string
	args []string
	runs int
}

func (c *captureRunner) run(_ context.Context, name string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.name, c.args, c.runs = name, slices.Clone(args), c.runs+1
	return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
}

// TestDumpArgumentsNeverCarryInjectedFlags runs backups of databases and collections
// whose names look like shell syntax or flags, and checks the argument slice
// mongodump receives: every element is one of the engine's own flags, each name sits
// inside its own "--flag=value" element, and the connection string is never in argv.
func TestDumpArgumentsNeverCarryInjectedFlags(t *testing.T) {
	const password = "argv-must-not-see-this"
	uri := "mongodb://admin:" + password + "@db.internal:27017/"
	for _, tc := range []struct {
		name     string
		db       string
		colls    []string
		excludes []string
	}{
		{"flag-like database", "--drop", nil, nil},
		{"dash database", "-h", nil, nil},
		{"shell metacharacters", "shop;rm", []string{"orders`id`", "a|b", "x&&y"}, []string{"c>d", "e<f"}},
		{"flag-like collections", "shop", []string{"--out=/etc/cron.d/x", "-d evil", "--uri=mongodb://attacker"}, []string{"--gzip", "--archive=/tmp/x"}},
		{"quotes and spaces in collections", "shop", []string{`orders" --drop "`, "it's here"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &captureRunner{}
			e := NewEngine(storage.NewMockStorage(), "", WithRunner(runner.run))
			rec, err := e.Run(context.Background(), models.BackupOptions{
				Database: tc.db, Collections: tc.colls, ExcludeCollections: tc.excludes, MongoURI: uri, Gzip: true,
			})
			if err != nil || rec.Status != models.StatusCompleted {
				t.Fatalf("backup = %+v, %v", rec, err)
			}
			if runner.name != "mongodump" {
				t.Fatalf("ran %q", runner.name)
			}
			want := []string{"--archive", "--gzip", "--db=" + tc.db}
			for _, c := range tc.colls {
				want = append(want, "--collection="+c)
			}
			for _, c := range tc.excludes {
				want = append(want, "--excludeCollection="+c)
			}
			got := runner.args
			if len(got) == 0 || !strings.HasPrefix(got[0], "--config=") {
				t.Fatalf("first argument %v; want the --config file", got)
			}
			if !slices.Equal(got[1:], want) {
				t.Fatalf("args = %q\nwant  %q", got[1:], want)
			}
			for _, a := range got {
				if strings.Contains(a, password) || strings.Contains(a, "mongodb://admin") {
					t.Fatalf("argument %q carries the connection string", a)
				}
			}
		})
	}
}

// TestInvalidNamespacesNeverReachMongodump checks that names MongoDB cannot have, and
// those with newlines, NUL or wildcards, are refused before any tool runs.
func TestInvalidNamespacesNeverReachMongodump(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  models.BackupOptions
		field string
	}{
		{"newline in database", models.BackupOptions{Database: "shop\n--drop"}, "database"},
		{"carriage return in database", models.BackupOptions{Database: "shop\r"}, "database"},
		{"NUL in database", models.BackupOptions{Database: "shop\x00admin"}, "database"},
		{"space in database", models.BackupOptions{Database: "shop --drop"}, "database"},
		{"dot in database", models.BackupOptions{Database: "shop.orders"}, "database"},
		{"slash in database", models.BackupOptions{Database: "../etc"}, "database"},
		{"backslash in database", models.BackupOptions{Database: `..\etc`}, "database"},
		{"dollar in database", models.BackupOptions{Database: "$(id)"}, "database"},
		{"quote in database", models.BackupOptions{Database: `shop"`}, "database"},
		{"wildcard database", models.BackupOptions{Database: "*"}, "database"},
		{"overlong database", models.BackupOptions{Database: strings.Repeat("d", models.MaxDatabaseNameLength+1)}, "database"},
		{"newline in collection", models.BackupOptions{Database: "shop", Collections: []string{"orders\n--drop"}}, "collections"},
		{"NUL in collection", models.BackupOptions{Database: "shop", Collections: []string{"a\x00b"}}, "collections"},
		{"dollar in collection", models.BackupOptions{Database: "shop", Collections: []string{"$cmd"}}, "collections"},
		{"newline in excluded collection", models.BackupOptions{Database: "shop", ExcludeCollections: []string{"x\ny"}}, "exclude_collections"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &captureRunner{}
			e := NewEngine(storage.NewMockStorage(), "mongodb://h", WithRunner(runner.run))
			_, err := e.Run(context.Background(), tc.opts)
			if !errors.Is(err, models.ErrInvalidNamespace) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("Run = %v; want ErrInvalidNamespace naming %s", err, tc.field)
			}
			if runner.runs != 0 {
				t.Fatalf("mongodump ran with %q", runner.args)
			}
		})
	}
}

// TestStorageKeyStaysUnderTheDatabase checks that the storage key derived from a
// valid database name is a plain relative key below that name.
func TestStorageKeyStaysUnderTheDatabase(t *testing.T) {
	e := NewEngine(storage.NewMockStorage(), "mongodb://h")
	for _, db := range []string{"shop", "--drop", "a;b", "-", "ümlaut"} {
		rec, err := e.Prepare(models.BackupOptions{Database: db})
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(rec.StorageKey, "/")
		if len(parts) != 4 || parts[0] != db || strings.Contains(rec.StorageKey, "..") || strings.HasPrefix(rec.StorageKey, "/") {
			t.Fatalf("storage key for %q = %q", db, rec.StorageKey)
		}
	}
}

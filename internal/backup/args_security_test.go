package backup

import (
	"context"
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
			// Several collections are expressed as exclusions of the others, so the
			// database listing holds the requested names and one more.
			lister := func(context.Context, string, string) ([]string, error) {
				return append(slices.Clone(tc.colls), "--other"), nil
			}
			e := NewEngine(storage.NewMockStorage(), "", WithRunner(runner.run), WithCollectionLister(lister))
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
			excludes := slices.Clone(tc.excludes)
			switch {
			case len(tc.colls) == 1:
				want = append(want, "--collection="+tc.colls[0])
			case len(tc.colls) > 1:
				excludes = append(excludes, "--other")
			}
			for _, c := range excludes {
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

// TestEngineRunsExistingJobsWhateverTheirNames checks that the engine itself does not
// validate names: client input is validated by the operations layer, and a scheduled
// run of an existing job (possibly created before validation existed) must not start
// failing. Such names still reach mongodump only inside --db=.
func TestEngineRunsExistingJobsWhateverTheirNames(t *testing.T) {
	for _, db := range []string{"legacy db", "shop\n--drop", "--drop"} {
		runner := &captureRunner{}
		e := NewEngine(storage.NewMockStorage(), "mongodb://h", WithRunner(runner.run))
		rec, err := e.Run(context.Background(), models.BackupOptions{Database: db})
		if err != nil || rec.Status != models.StatusCompleted {
			t.Fatalf("run of %q = %+v, %v", db, rec, err)
		}
		if !slices.Contains(runner.args, "--db="+db) {
			t.Fatalf("args %q lack --db=%s", runner.args, db)
		}
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

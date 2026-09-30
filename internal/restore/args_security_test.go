package restore

import (
	"bytes"
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

func (c *captureRunner) run(_ context.Context, name string, stdin io.Reader, args ...string) (io.Reader, func() error, error) {
	_, _ = io.Copy(io.Discard, stdin)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.name, c.args, c.runs = name, slices.Clone(args), c.runs+1
	return strings.NewReader(""), func() error { return nil }, nil
}

// restoreFixture returns an engine over a mock storage holding one archive of db.
func restoreFixture(t *testing.T, db string) (*Engine, *captureRunner, *models.BackupRecord) {
	t.Helper()
	mock := storage.NewMockStorage()
	key := "k/bkp.archive"
	if _, err := mock.Save(context.Background(), key, bytes.NewReader([]byte("archive"))); err != nil {
		t.Fatal(err)
	}
	runner := &captureRunner{}
	e := NewEngine(mock, "mongodb://admin:argv-must-not-see-this@db.internal:27017/", WithRunner(runner.run))
	return e, runner, &models.BackupRecord{ID: "bkp_1", Database: db, StorageKey: key, Status: models.StatusCompleted}
}

// TestRestoreArgumentsNeverCarryInjectedFlags restores archives of databases and
// collections whose names look like flags or shell syntax, and checks that each name
// reaches mongorestore only inside its own "--nsX=value" element.
func TestRestoreArgumentsNeverCarryInjectedFlags(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		req    models.RestoreRequest
		want   func(target string) []string
	}{
		{"safe clone of a flag-like database", "--drop", models.RestoreRequest{},
			func(target string) []string {
				return []string{"--nsFrom=--drop.*", "--nsTo=" + target + ".*", "--nsInclude=--drop.*"}
			}},
		{"flag-like selected collections", "shop", models.RestoreRequest{SelectedCollections: []string{"--drop", "a;b", "x --dir=/"}},
			func(target string) []string {
				return []string{"--nsFrom=shop.*", "--nsTo=" + target + ".*", "--nsInclude=shop.--drop", "--nsInclude=shop.a;b", "--nsInclude=shop.x --dir=/"}
			}},
		{"in place into a flag-like target", "shop", models.RestoreRequest{SafeClone: new(bool), ConfirmInPlace: true, TargetDatabase: "--dir=x", Verify: new(bool)},
			func(string) []string {
				return []string{"--nsFrom=shop.*", "--nsTo=--dir=x.*", "--nsInclude=shop.*"}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, runner, src := restoreFixture(t, tc.source)
			req := tc.req
			req.BackupID = src.ID
			rec, err := e.Run(context.Background(), req, src)
			if err != nil || rec.Status != models.RestoreStatusCompleted {
				t.Fatalf("restore = %+v, %v", rec, err)
			}
			if runner.name != "mongorestore" || len(runner.args) < 2 || !strings.HasPrefix(runner.args[0], "--config=") || runner.args[1] != "--archive" {
				t.Fatalf("ran %s %q", runner.name, runner.args)
			}
			if got, want := runner.args[2:], tc.want(rec.TargetDatabase); !slices.Equal(got, want) {
				t.Fatalf("args = %q\nwant  %q", got, want)
			}
			for _, a := range runner.args {
				if strings.Contains(a, "argv-must-not-see-this") {
					t.Fatalf("argument %q carries the connection string", a)
				}
			}
		})
	}
}

// TestExistingBackupsRestoreWhateverTheirNames checks that the engine does not
// validate the backup's own database name: every existing backup stays restorable.
func TestExistingBackupsRestoreWhateverTheirNames(t *testing.T) {
	for _, db := range []string{"legacy db", "a*b"} {
		e, runner, src := restoreFixture(t, db)
		rec, err := e.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src)
		if err != nil || rec.Status != models.RestoreStatusCompleted {
			t.Fatalf("restore of %q = %+v, %v", db, rec, err)
		}
		if !slices.Contains(runner.args, "--nsFrom="+db+".*") {
			t.Fatalf("args %q", runner.args)
		}
	}
}

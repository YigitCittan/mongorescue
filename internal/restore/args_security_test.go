package restore

import (
	"bytes"
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

// TestInvalidRestoreNamespacesAreRefused checks that a target database or selected
// collection MongoDB cannot have (newlines, NUL, dots, wildcards) is refused before
// mongorestore runs.
func TestInvalidRestoreNamespacesAreRefused(t *testing.T) {
	inPlace := func(target string) models.RestoreRequest {
		return models.RestoreRequest{SafeClone: new(bool), ConfirmInPlace: true, TargetDatabase: target}
	}
	for _, tc := range []struct {
		name string
		req  models.RestoreRequest
	}{
		{"newline in target", inPlace("shop\n--drop")},
		{"NUL in target", inPlace("shop\x00")},
		{"dot in target", inPlace("shop.orders")},
		{"wildcard target", inPlace("*")},
		{"space in target", inPlace("shop --drop")},
		{"newline in collection", models.RestoreRequest{SelectedCollections: []string{"orders\n--drop"}}},
		{"wildcard collection", models.RestoreRequest{SelectedCollections: []string{"*"}}},
		{"dollar collection", models.RestoreRequest{SelectedCollections: []string{"$cmd"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, runner, src := restoreFixture(t, "shop")
			req := tc.req
			req.BackupID = src.ID
			if _, err := e.Run(context.Background(), req, src); !errors.Is(err, models.ErrInvalidNamespace) {
				t.Fatalf("Run = %v; want ErrInvalidNamespace", err)
			}
			if runner.runs != 0 {
				t.Fatalf("mongorestore ran with %q", runner.args)
			}
		})
	}
}

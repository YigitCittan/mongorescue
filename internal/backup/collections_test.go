package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// argsRunner records mongodump's arguments and emits a tiny archive.
type argsRunner struct {
	called bool
	args   []string
}

func (a *argsRunner) run(_ context.Context, _ string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
	a.called = true
	a.args = args
	return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
}

func (a *argsRunner) values(flag string) []string {
	var out []string
	for _, arg := range a.args {
		if v, ok := strings.CutPrefix(arg, flag+"="); ok {
			out = append(out, v)
		}
	}
	return out
}

// TestBackupSeveralCollectionsWithoutListerFails guards against the silent data loss
// of passing several --collection flags: mongodump would dump only the last one.
func TestBackupSeveralCollectionsWithoutListerFails(t *testing.T) {
	runner := &argsRunner{}
	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(runner.run))
	record, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", Collections: []string{"orders", "customers"}})
	if !errors.Is(err, ErrCollectionFilter) {
		t.Fatalf("expected ErrCollectionFilter, got %v", err)
	}
	if runner.called || record.Status != models.StatusFailed {
		t.Fatalf("mongodump must not run: called=%v status=%s", runner.called, record.Status)
	}
}

func TestBackupSeveralCollectionsBecomeExclusions(t *testing.T) {
	runner := &argsRunner{}
	var listedDB string
	lister := func(_ context.Context, _ string, db string) ([]string, error) {
		listedDB = db
		return []string{"orders", "customers", "logs", "audit_view", "system.views", "system.js", "system.buckets.metrics", "metrics"}, nil
	}
	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(runner.run), WithCollectionLister(lister))
	record, err := engine.Run(context.Background(), models.BackupOptions{
		Database:           "shop",
		Collections:        []string{" orders", "customers", "orders"},
		ExcludeCollections: []string{"customers_tmp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if listedDB != "shop" {
		t.Fatalf("listed database %q; want shop", listedDB)
	}
	if got := runner.values("--collection"); len(got) != 0 {
		t.Fatalf("--collection must not be passed with several collections: %v", got)
	}
	want := []string{"customers_tmp", "logs", "audit_view", "system.js", "system.buckets.metrics", "metrics"}
	if got := runner.values("--excludeCollection"); !slices.Equal(got, want) {
		t.Fatalf("--excludeCollection = %v; want %v", got, want)
	}
	if !slices.Equal(record.Collections, []string{" orders", "customers", "orders"}) {
		t.Fatalf("the record keeps the requested collections, got %v", record.Collections)
	}
}

func TestBackupSingleCollectionNeedsNoLister(t *testing.T) {
	runner := &argsRunner{}
	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(runner.run))
	if _, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", Collections: []string{"orders", " "}}); err != nil {
		t.Fatal(err)
	}
	if got := runner.values("--collection"); !slices.Equal(got, []string{"orders"}) {
		t.Fatalf("--collection = %v; want [orders]", got)
	}
}

func TestBackupCollectionListerErrorFailsTheRun(t *testing.T) {
	runner := &argsRunner{}
	boom := errors.New("listCollections: unauthorized")
	lister := func(context.Context, string, string) ([]string, error) { return nil, boom }
	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(runner.run), WithCollectionLister(lister))
	_, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", Collections: []string{"a", "b"}})
	if !errors.Is(err, ErrCollectionFilter) || !errors.Is(err, boom) || runner.called {
		t.Fatalf("expected ErrCollectionFilter wrapping the lister error without a dump, got %v (called=%v)", err, runner.called)
	}
}

func TestBackupAllCollectionsIncludedDumpsWholeDatabase(t *testing.T) {
	runner := &argsRunner{}
	lister := func(context.Context, string, string) ([]string, error) { return []string{"a", "b"}, nil }
	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(runner.run), WithCollectionLister(lister))
	if _, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", Collections: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if len(runner.values("--collection"))+len(runner.values("--excludeCollection")) != 0 {
		t.Fatalf("unexpected filter args: %v", runner.args)
	}
}

// TestBackupMissingCollectionFails keeps a typo from producing an empty or partial
// backup that reports success.
func TestBackupMissingCollectionFails(t *testing.T) {
	lister := func(context.Context, string, string) ([]string, error) {
		return []string{"orders", "metrics", "system.buckets.metrics"}, nil
	}
	for _, cols := range [][]string{{"ordres"}, {"orders", "customers"}} {
		runner := &argsRunner{}
		engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(runner.run), WithCollectionLister(lister))
		rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", Collections: cols})
		if !errors.Is(err, ErrCollectionFilter) || !strings.Contains(err.Error(), "not found") || runner.called || rec.Status != models.StatusFailed {
			t.Fatalf("%v: want a failed backup naming the missing collection, got %v (called=%v)", cols, err, runner.called)
		}
	}
}

// TestBackupRequestedTimeSeriesKeepsItsBuckets checks the buckets of a requested
// time-series collection are not excluded.
func TestBackupRequestedTimeSeriesKeepsItsBuckets(t *testing.T) {
	lister := func(context.Context, string, string) ([]string, error) {
		return []string{"orders", "metrics", "system.buckets.metrics", "other", "system.buckets.other"}, nil
	}
	runner := &argsRunner{}
	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(runner.run), WithCollectionLister(lister))
	if _, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", Collections: []string{"orders", "metrics"}}); err != nil {
		t.Fatal(err)
	}
	if got, want := runner.values("--excludeCollection"), []string{"other", "system.buckets.other"}; !slices.Equal(got, want) {
		t.Fatalf("--excludeCollection = %v; want %v", got, want)
	}
}

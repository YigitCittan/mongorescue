package restore

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestParallelArgs checks the mongorestore flags of the parallelism a restore ran
// with: nothing for unset values and mongorestore's own defaults, a flag per other
// value, bounded.
func TestParallelArgs(t *testing.T) {
	for _, tc := range []struct {
		collections, workers int
		want                 []string
	}{
		{0, 0, nil},
		{4, 1, nil},
		{8, 0, []string{"--numParallelCollections=8"}},
		{0, 6, []string{"--numInsertionWorkersPerCollection=6"}},
		{16, 32, []string{"--numParallelCollections=16", "--numInsertionWorkersPerCollection=32"}},
		{1000, 1000, []string{"--numParallelCollections=16", "--numInsertionWorkersPerCollection=32"}},
	} {
		if got := parallelArgs(tc.collections, tc.workers); !slices.Equal(got, tc.want) {
			t.Errorf("parallelArgs(%d, %d) = %q; want %q", tc.collections, tc.workers, got, tc.want)
		}
	}
}

// TestPrepareRecordsEffectiveParallelism checks the precedence of the request, the
// configured default and mongorestore's own, and that out-of-range values are
// refused before anything runs.
func TestPrepareRecordsEffectiveParallelism(t *testing.T) {
	src := &models.BackupRecord{ID: "bkp_1", Database: "shop"}
	newReq := func(c, w int) models.RestoreRequest {
		return models.RestoreRequest{BackupID: src.ID, MongoURI: "mongodb://localhost", NumParallelCollections: c, InsertionWorkersPerCollection: w}
	}
	e := NewEngine(nil, "mongodb://localhost")
	if rec, err := e.Prepare(newReq(0, 0), src); err != nil || rec.NumParallelCollections != 4 || rec.InsertionWorkersPerCollection != 1 {
		t.Fatalf("defaults: %+v, %v", rec, err)
	}
	e = NewEngine(nil, "mongodb://localhost", WithRunConfig(func() RunConfig {
		return RunConfig{NumParallelCollections: 8, InsertionWorkersPerCollection: 2}
	}))
	if rec, err := e.Prepare(newReq(0, 0), src); err != nil || rec.NumParallelCollections != 8 || rec.InsertionWorkersPerCollection != 2 {
		t.Fatalf("settings: %+v, %v", rec, err)
	}
	if rec, err := e.Prepare(newReq(12, 0), src); err != nil || rec.NumParallelCollections != 12 || rec.InsertionWorkersPerCollection != 2 {
		t.Fatalf("request over settings: %+v, %v", rec, err)
	}
	if _, err := e.Prepare(newReq(17, 0), src); !errors.Is(err, models.ErrInvalidRestoreParallelCollections) {
		t.Fatalf("17 collections: %v", err)
	}
	if _, err := e.Prepare(newReq(0, 33), src); !errors.Is(err, models.ErrInvalidInsertionWorkers) {
		t.Fatalf("33 workers: %v", err)
	}
	if _, err := e.Prepare(newReq(-1, 0), src); !errors.Is(err, models.ErrInvalidRestoreParallelCollections) {
		t.Fatalf("-1 collections: %v", err)
	}
}

// TestRunPassesParallelismToMongorestore checks that a restore started with
// parallelism options runs mongorestore with the matching flags (as arguments, never
// through a shell), and records the values.
func TestRunPassesParallelismToMongorestore(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	var got []string
	runner := func(_ context.Context, _ string, stdin io.Reader, args ...string) (io.Reader, func() error, error) {
		got = slices.Clone(args)
		_, _ = io.Copy(io.Discard, stdin)
		return strings.NewReader("1 document(s) restored successfully. 0 document(s) failed to restore.\n"), func() error { return nil }, nil
	}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner))
	req := models.RestoreRequest{BackupID: src.ID, NumParallelCollections: 8, InsertionWorkersPerCollection: 3}
	record, err := engine.Run(context.Background(), req, src)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"--numParallelCollections=8", "--numInsertionWorkersPerCollection=3"} {
		if !slices.Contains(got, want) {
			t.Errorf("mongorestore args %q lack %q", got, want)
		}
	}
	if record.NumParallelCollections != 8 || record.InsertionWorkersPerCollection != 3 {
		t.Errorf("record parallelism = %d, %d; want 8, 3", record.NumParallelCollections, record.InsertionWorkersPerCollection)
	}
}

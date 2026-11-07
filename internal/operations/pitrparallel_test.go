package operations_test

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestEstimatePITRReflectsParallelism checks that the RTO estimate is made for the
// parallelism a restore would use: it averages only restores that ran with the same
// values once one was timed, reports them, and falls back to the other restores'
// rates (ParallelismMatched false) until then. Records of earlier releases ran with
// mongorestore's defaults.
func TestEstimatePITRReflectsParallelism(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	manager := runs.NewManager(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = manager.Shutdown(ctx) })
	svc := operations.New(operations.Config{Store: st, Backup: backup.NewEngine(storage.NewMockStorage(), ""),
		Restore: restore.NewEngine(storage.NewMockStorage(), ""), Runs: manager, Logger: slog.New(slog.DiscardHandler)})
	near := func(got, want float64) bool { return math.Abs(got-want) <= want*1e-9 }

	n := 0
	save := func(collections, workers int, baseMiB int64, seconds float64) {
		t.Helper()
		n++
		rec := &models.RestoreRecord{ID: fmt.Sprintf("pitr_%02d", n), Status: models.RestoreStatusCompleted,
			StartedAt:              time.Date(2026, 10, 1, n, 0, 0, 0, time.UTC),
			NumParallelCollections: collections, InsertionWorkersPerCollection: workers,
			PITR: &models.PITRRestore{StreamID: "str_a", BaseBytes: baseMiB << 20, BaseSeconds: seconds}}
		if err := st.SaveRestoreRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	// An earlier release's restore (no recorded values, so mongorestore's 4 and 1):
	// 100 MiB in 10 s. A tuned one: 8 collections, 4 workers, 100 MiB in 2 s.
	save(0, 0, 100, 10)
	save(8, 4, 100, 2)

	e := svc.EstimatePITRWith(ctx, "str_a", 200<<20, 0, 0, 0, 0)
	if e.NumParallelCollections != 4 || e.InsertionWorkersPerCollection != 1 || !e.ParallelismMatched ||
		!near(e.BaseBytesPerSecond, 10<<20) || !near(e.Seconds, 20) {
		t.Fatalf("defaults: %+v", e)
	}
	e = svc.EstimatePITRWith(ctx, "str_a", 200<<20, 0, 0, 8, 4)
	if e.NumParallelCollections != 8 || e.InsertionWorkersPerCollection != 4 || !e.ParallelismMatched ||
		!near(e.BaseBytesPerSecond, 50<<20) || !near(e.Seconds, 4) {
		t.Fatalf("tuned: %+v", e)
	}
	// Nothing ran with 16 and 2: the other restores' rates stand in, flagged.
	e = svc.EstimatePITRWith(ctx, "str_a", 200<<20, 0, 0, 16, 2)
	if e.ParallelismMatched || e.Source != models.PITREstimateMeasured || e.Samples != 2 || e.NumParallelCollections != 16 {
		t.Fatalf("unmatched: %+v", e)
	}
	// The setting-driven entry point resolves the defaults: none configured is 4 and 1.
	if e = svc.EstimatePITR(ctx, "str_a", 200<<20, 0, 0); e.NumParallelCollections != 4 || !e.ParallelismMatched {
		t.Fatalf("EstimatePITR: %+v", e)
	}
}

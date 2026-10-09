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

// TestEstimatePITRUsesMeasuredRates checks the RTO estimate of point-in-time
// restores: default rates without history, the overall rate of an older chain
// test without pass timings, and otherwise rolling averages (totals over total
// seconds) of the passes of the stream's five newest completed restores and chain
// tests, ignoring failed ones and other streams; the replay takes the longer of
// its bytes and its entries.
func TestEstimatePITRUsesMeasuredRates(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	manager := runs.NewManager(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = manager.Shutdown(ctx) })
	svc := operations.New(operations.Config{Store: st, Backup: backup.NewEngine(storage.NewMockStorage(), ""),
		Restore: restore.NewEngine(storage.NewMockStorage(), ""), Runs: manager, Logger: slog.New(slog.DiscardHandler)})
	near := func(got, want float64) bool { return math.Abs(got-want) <= want*1e-9 }

	e := svc.EstimatePITR(ctx, "str_a", 100<<20, 8<<20, 0)
	if e.Source != models.PITREstimateDefault || e.Samples != 0 || !near(e.Seconds, 2+2) {
		t.Fatalf("without history: %+v", e)
	}
	// Many small entries: the replay takes the longer of bytes and entries (20,000
	// entries at the default 2,000 per second).
	if e = svc.EstimatePITR(ctx, "str_a", 100<<20, 8<<20, 20_000); !near(e.Seconds, 2+10) || e.OplogEntries != 20_000 {
		t.Fatalf("without history, by entries: %+v", e)
	}

	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	save := func(status models.RestoreStatus, info models.PITRRestore, duration float64) {
		t.Helper()
		n++
		if info.StreamID == "" {
			info.StreamID = "str_a"
		}
		rec := &models.RestoreRecord{ID: fmt.Sprintf("pitr_%02d", n), Status: status, StartedAt: start.Add(time.Duration(n) * time.Hour),
			DurationSeconds: duration, PITR: &info}
		if err := st.SaveRestoreRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	// An older chain test without pass timings: its overall rate (10 MiB/s).
	save(models.RestoreStatusCompleted, models.PITRRestore{ChainTest: true, BaseBytes: 80 << 20, OplogBytes: 20 << 20}, 10)
	e = svc.EstimatePITR(ctx, "str_a", 100<<20, 0, 0)
	if e.Source != models.PITREstimateChainTest || !near(e.Seconds, 10) {
		t.Fatalf("from an older chain test: %+v", e)
	}

	// Timed restores: the base at 20 and 60 MiB/s (rolling: 160 MiB in 4 s), the
	// replay at 1 and 3 MiB/s (8 MiB in 4 s), 1,000 + 5,000 entries in 4 s.
	save(models.RestoreStatusCompleted, models.PITRRestore{BaseBytes: 40 << 20, BaseSeconds: 2, OplogBytes: 2 << 20, ReplaySeconds: 2, OpsReplayed: 1000}, 5)
	save(models.RestoreStatusCompleted, models.PITRRestore{ChainTest: true, BaseBytes: 120 << 20, BaseSeconds: 2, OplogBytes: 6 << 20, ReplaySeconds: 2, OpsReplayed: 5000}, 5)
	// Ignored: a failed restore and another stream's.
	save(models.RestoreStatusFailed, models.PITRRestore{BaseBytes: 1, BaseSeconds: 100, OplogBytes: 1, ReplaySeconds: 100}, 200)
	save(models.RestoreStatusCompleted, models.PITRRestore{StreamID: "other", BaseBytes: 1, BaseSeconds: 100, OplogBytes: 1, ReplaySeconds: 100}, 200)
	e = svc.EstimatePITR(ctx, "str_a", 400<<20, 20<<20, 0)
	if e.Source != models.PITREstimateMeasured || e.Samples != 2 || !near(e.BaseBytesPerSecond, 40<<20) ||
		!near(e.ReplayBytesPerSecond, 2<<20) || !near(e.ReplayEntriesPerSecond, 1500) || !near(e.Seconds, 10+10) {
		t.Fatalf("measured: %+v", e)
	}
	// 30,000 entries at the measured 1,500 per second outlast the bytes.
	if e = svc.EstimatePITR(ctx, "str_a", 400<<20, 20<<20, 30_000); !near(e.Seconds, 10+20) {
		t.Fatalf("measured, by entries: %+v", e)
	}

	// Only the five newest timed restores count: five slow ones push the fast
	// ones out.
	for range 5 {
		save(models.RestoreStatusCompleted, models.PITRRestore{BaseBytes: 10 << 20, BaseSeconds: 1, OplogBytes: 1 << 20, ReplaySeconds: 1, OpsReplayed: 100}, 2)
	}
	e = svc.EstimatePITR(ctx, "str_a", 100<<20, 10<<20, 0)
	if e.Samples != 5 || !near(e.BaseBytesPerSecond, 10<<20) || !near(e.ReplayBytesPerSecond, 1<<20) || !near(e.Seconds, 10+10) {
		t.Fatalf("rolling window: %+v", e)
	}
}

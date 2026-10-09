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

	// Timed restores whose replays tell entries and bytes apart: 1,000 entries in
	// 1 MiB took 1.5 s, 1,000 entries in 3 MiB 2.5 s, so 1 ms per entry plus 0.5 s
	// per MiB. The base: 160 MiB in 4 s. Ignored: a failed restore and another
	// stream's.
	save(models.RestoreStatusCompleted, models.PITRRestore{BaseBytes: 40 << 20, BaseSeconds: 2, OplogBytes: 1 << 20, ReplaySeconds: 1.5, OpsReplayed: 1000}, 5)
	save(models.RestoreStatusCompleted, models.PITRRestore{ChainTest: true, BaseBytes: 120 << 20, BaseSeconds: 2, OplogBytes: 3 << 20, ReplaySeconds: 2.5, OpsReplayed: 1000}, 5)
	save(models.RestoreStatusFailed, models.PITRRestore{BaseBytes: 1, BaseSeconds: 100, OplogBytes: 1, ReplaySeconds: 100, OpsReplayed: 1}, 200)
	save(models.RestoreStatusCompleted, models.PITRRestore{StreamID: "other", BaseBytes: 1, BaseSeconds: 100, OplogBytes: 1, ReplaySeconds: 100, OpsReplayed: 1}, 200)
	e = svc.EstimatePITR(ctx, "str_a", 400<<20, 20<<20, 30_000)
	if e.Source != models.PITREstimateMeasured || e.Samples != 2 || e.ReplayModel != models.PITRReplayModelEntriesAndBytes ||
		!near(e.BaseBytesPerSecond, 40<<20) || !near(e.Seconds, 10+30+10) {
		t.Fatalf("measured, entries and bytes: %+v", e)
	}

	// Large entries after small ones. Five replays of one workload (200-byte
	// entries, 2,500 per second, 0.5 MB/s) cannot tell entries from bytes, so the
	// fit keeps the entries: a 1 GiB oplog of 10 KB entries takes its 104,858
	// entries at 2,500 per second (about 42 s), not 1 GiB at 0.5 MB/s, which the
	// larger of the two rates gave (about 2,150 s, 50 times too long). The five
	// newest replace the older ones.
	for range 5 {
		save(models.RestoreStatusCompleted, models.PITRRestore{BaseBytes: 10 << 20, BaseSeconds: 1, OplogBytes: 500_000, ReplaySeconds: 1, OpsReplayed: 2500}, 2)
	}
	e = svc.EstimatePITR(ctx, "str_a", 0, 1<<30, 104_858)
	if e.Samples != 5 || e.ReplayModel != models.PITRReplayModelEntries || !near(e.Seconds, 104_858.0/2500) {
		t.Fatalf("large entries after small ones: %+v", e)
	}
	if !near(e.ReplayEntriesPerSecond, 2500) || !near(e.ReplayBytesPerSecond, 500_000) || !near(e.BaseBytesPerSecond, 10<<20) {
		t.Fatalf("averages of the five newest: %+v", e)
	}
	// Without an entry count (an older caller), the measured byte rate.
	if e = svc.EstimatePITR(ctx, "str_a", 0, 5_000_000, 0); !near(e.Seconds, 10) {
		t.Fatalf("without an entry count: %+v", e)
	}
}

// TestFitReplayPrefersTheModelThatFits checks the one-term fallbacks of the
// replay fit: bytes when they explain the runs better than entries, entries on a
// tie.
func TestFitReplayPrefersTheModelThatFits(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) <= math.Abs(want)*1e-6 }
	// Time follows the bytes; the entries go against it.
	byBytes := []operations.ReplayRunForTest{{Entries: 1000, Bytes: 1 << 20, Seconds: 1}, {Entries: 4000, Bytes: 2 << 20, Seconds: 2},
		{Entries: 500, Bytes: 3 << 20, Seconds: 3}}
	if model, perEntry, perByte := operations.FitReplayForTest(byBytes); model != models.PITRReplayModelBytes || perEntry != 0 || !near(perByte, 1.0/(1<<20)) {
		t.Fatalf("by bytes: %s %v %v", model, perEntry, perByte)
	}
	// One run: both one-term models fit exactly; entries win the tie.
	if model, perEntry, _ := operations.FitReplayForTest(byBytes[:1]); model != models.PITRReplayModelEntries || !near(perEntry, 1.0/1000) {
		t.Fatalf("one run: %s %v", model, perEntry)
	}
}

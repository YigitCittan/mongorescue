package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/cost"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// usageBackup returns a backup record of the usage tests.
func usageBackup(id, job, db, target, key string, size int64, status models.BackupStatus, at time.Time) *models.BackupRecord {
	return &models.BackupRecord{ID: id, JobID: job, Database: db, StorageTargetID: target, StorageKey: key,
		SizeBytes: size, Status: status, StartedAt: at}
}

func TestLiveUsageAggregatesArchivesCopiesAndChunks(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	at := time.Now().UTC().Add(-time.Hour)
	done := func(target, key string, retained bool) models.BackupCopy {
		return models.BackupCopy{TargetID: target, StorageKey: key, Status: models.CopyDone, Retained: retained}
	}

	b1 := usageBackup("bkp_1", "job_1", "a", "stg_1", "a/k1", 100, models.StatusCompleted, at)
	b1.Copies = []models.BackupCopy{done("stg_2", "a/k1", false), {TargetID: "stg_3", StorageKey: "a/k1", Status: models.CopyPending}}
	b2 := usageBackup("bkp_2", "job_1", "a", "stg_1", "a/k2", 50, models.StatusCompleted, at)
	// A soft-deleted backup keeps its archive until the purge.
	b3 := usageBackup("bkp_3", "job_1", "b", "stg_1", "b/k3", 10, models.StatusDeleted, at)
	// A purged primary whose retained copy outlives it.
	b4 := usageBackup("bkp_4", "job_2", "b", "stg_1", "b/k4", 999, models.StatusPurged, at)
	b4.Copies = []models.BackupCopy{done("stg_2", "b/k4", true)}
	// A second record naming the archive of bkp_1 counts once.
	b5 := usageBackup("bkp_5", "job_1", "a", "stg_1", "a/k1", 100, models.StatusCompleted, at)
	b6 := usageBackup("bkp_6", "job_1", "a", "stg_1", "a/k6", 7, models.StatusFailed, at)
	// A record from before storage targets: it belongs to the default target.
	b7 := usageBackup("bkp_7", "", "c", "", "c/k7", 5, models.StatusCompleted, at)
	for _, b := range []*models.BackupRecord{b1, b2, b3, b4, b5, b6, b7} {
		if err := s.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.CreateStream(ctx, newStream("pst_a", "conn_a")); err != nil {
		t.Fatal(err)
	}
	if err := s.StartChain(ctx, "pst_a", "ch1", pitr.OpTime{TS: pos(100, 1), Term: 1}, at); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*pitr.Chunk{
		chunk("c1", "ch1", pos(100, 1), pos(160, 1), 1),
		chunk("c2", "ch1", pos(160, 1), pos(220, 1), 1),
		chunk("c3", "ch1", pos(220, 1), pos(280, 1), 1),
	} {
		c.CreatedAt = at
		if err := s.CommitChunk(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// c3 is pruned: its object is gone.
	if _, err := s.DeleteChunks(ctx, []string{"c3"}, at, at); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkChunkPruned(ctx, "c3"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlanChunkCopies(ctx, "pst_a", []models.CopyTarget{{ID: "tgt_dr", Name: "dr"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateChunkCopy(ctx, "c1", "tgt_dr", func(c *models.ChunkCopy) error {
		c.Status = models.CopyDone
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.LiveUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[cost.Row]bool{}
	for _, r := range rows {
		got[r] = true
	}
	want := []cost.Row{
		{TargetID: "stg_1", Kind: cost.KindBackup, JobID: "job_1", Database: "a", Objects: 2, Bytes: 150},
		{TargetID: "stg_1", Kind: cost.KindBackup, JobID: "job_1", Database: "b", Objects: 1, Bytes: 10},
		{TargetID: "", Kind: cost.KindBackup, Database: "c", Objects: 1, Bytes: 5},
		{TargetID: "stg_2", Kind: cost.KindCopy, JobID: "job_1", Database: "a", Objects: 1, Bytes: 100},
		{TargetID: "stg_2", Kind: cost.KindRetained, JobID: "job_2", Database: "b", Objects: 1, Bytes: 999},
		{TargetID: "tgt_local", Kind: cost.KindPITR, StreamID: "pst_a", Objects: 2, Bytes: 1024},
		{TargetID: "tgt_dr", Kind: cost.KindCopy, StreamID: "pst_a", Objects: 1, Bytes: 512},
	}
	if len(rows) != len(want) {
		t.Errorf("LiveUsage returned %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing row %+v in %+v", w, rows)
		}
	}
}

func TestUsageSnapshotIsIdempotentPerDayAndPrunes(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	row := func(bytes int64) []cost.Row {
		return []cost.Row{{TargetID: "stg_1", Kind: cost.KindBackup, JobID: "job_1", Database: "a", Objects: 1, Bytes: bytes},
			{TargetID: "stg_1", Kind: cost.KindPITR, StreamID: "pst_a", Objects: 3, Bytes: 30}}
	}
	for _, step := range []struct {
		day   string
		bytes int64
	}{{"2026-01-01", 10}, {"2026-01-02", 20}, {"2026-01-02", 25}, {"2026-01-02", 25}} {
		if err := s.ReplaceUsageDay(ctx, step.day, row(step.bytes)); err != nil {
			t.Fatal(err)
		}
	}
	hist, err := s.UsageHistory(ctx, "2026-01-01", "2026-01-31")
	if err != nil {
		t.Fatal(err)
	}
	var days []string
	var jan2 int64
	for _, h := range hist {
		days = append(days, h.Day)
		if h.Day == "2026-01-02" && h.Kind == cost.KindBackup {
			jan2 += h.Bytes
		}
	}
	if want := []string{"2026-01-01", "2026-01-01", "2026-01-02", "2026-01-02"}; !slices.Equal(days, want) {
		t.Errorf("days = %v, want %v (one set of rows per day)", days, want)
	}
	if jan2 != 25 {
		t.Errorf("the second day holds %d bytes, want the last snapshot's 25", jan2)
	}
	if only, _ := s.UsageHistory(ctx, "2026-01-02", "2026-01-02"); len(only) != 2 {
		t.Errorf("range read returned %d rows, want 2", len(only))
	}

	// Replacing a day with nothing empties it.
	if err = s.ReplaceUsageDay(ctx, "2026-01-02", nil); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneUsage(ctx, "2026-01-02"); err != nil || n != 2 {
		t.Fatalf("PruneUsage = %d, %v; want the 2 rows of the first day", n, err)
	}
	if hist, _ = s.UsageHistory(ctx, "2000-01-01", "2100-01-01"); len(hist) != 0 {
		t.Errorf("history after the prune = %+v", hist)
	}
}

func TestEgressReadsCountVerificationCopiesAndRestoreTests(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	now := time.Now().UTC()
	since := now.Add(-48 * time.Hour)
	recent, old := now.Add(-time.Hour), now.Add(-10*24*time.Hour)

	b1 := usageBackup("bkp_1", "job_1", "a", "stg_1", "a/k1", 100, models.StatusCompleted, old)
	b1.VerifiedAt = &recent
	b1.Copies = []models.BackupCopy{
		{TargetID: "stg_2", StorageKey: "a/k1", Status: models.CopyDone, CopiedAt: &recent, VerifiedAt: &recent},
		// A copy made and verified before the window is not counted.
		{TargetID: "stg_3", StorageKey: "a/k1", Status: models.CopyDone, CopiedAt: &old, VerifiedAt: &old},
	}
	b2 := usageBackup("bkp_2", "job_1", "a", "stg_1", "a/k2", 40, models.StatusCompleted, old)
	b2.VerifiedAt = &old
	for _, b := range []*models.BackupRecord{b1, b2} {
		if err := s.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, rt := range []*models.RestoreTestResult{
		{ID: "rt_1", JobID: "job_1", BackupID: "bkp_1", StartedAt: recent, Status: models.RestoreTestOK},
		{ID: "rt_2", JobID: "job_1", BackupID: "bkp_1", StartedAt: recent, Status: models.RestoreTestOK, SourceTargetID: "stg_2"},
		{ID: "rt_3", JobID: "job_1", BackupID: "bkp_1", StartedAt: recent, Status: models.RestoreTestError},
		{ID: "rt_4", JobID: "job_1", BackupID: "bkp_2", StartedAt: old, Status: models.RestoreTestOK},
	} {
		if err := s.SaveRestoreTest(ctx, rt); err != nil {
			t.Fatal(err)
		}
	}

	reads, err := s.EgressReads(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	sum := map[string]int64{}
	for _, r := range reads {
		sum[r.TargetID+"/"+string(r.Source)] += r.Bytes
	}
	want := map[string]int64{
		"stg_1/verification": 100, // bkp_1's last verification
		"stg_2/verification": 100, // the copy's verification
		"stg_1/copy":         100, // the read of the primary for the copy to stg_2
		"stg_1/restore_test": 100, // rt_1 read the primary
		"stg_2/restore_test": 100, // rt_2 was a drill on the copy target
	}
	if len(sum) != len(want) {
		t.Errorf("reads = %v, want %v", sum, want)
	}
	for k, v := range want {
		if sum[k] != v {
			t.Errorf("%s = %d, want %d (all: %v)", k, sum[k], v, sum)
		}
	}
}

package operations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
)

// withCopyChain gives stream str_a of pitrService the copy target tgt_dr, a
// completed copy of base b1 there and completed copies of the chunks done.
func withCopyChain(t *testing.T, svc *operations.Service, done ...string) {
	t.Helper()
	ctx := context.Background()
	st := svcStore(t, svc)
	stream, err := st.GetStream(ctx, "str_a")
	if err != nil {
		t.Fatal(err)
	}
	stream.CopyTargets = []string{"tgt_dr"}
	if err = st.UpdateStream(ctx, stream); err != nil {
		t.Fatal(err)
	}
	if _, err = st.UpdateBackupRecord(ctx, "b1", func(r *models.BackupRecord) error {
		r.SHA256 = "bb"
		r.PlanCopies([]models.CopyTarget{{ID: "tgt_dr", Name: "DR"}}, "")
		c := &r.Copies[0]
		c.Status, c.SHA256OK, c.SHA256 = models.CopyDone, true, "bb"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.PlanChunkCopies(ctx, "str_a", []models.CopyTarget{{ID: "tgt_dr"}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range done {
		if _, err = st.UpdateChunkCopy(ctx, id, "tgt_dr", func(c *models.ChunkCopy) error {
			c.Status, c.SHA256OK, c.SHA256, c.VersionID = models.CopyDone, true, "ab", "v-"+id
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// lastRun waits for the restore fake to run and returns its run.
func lastRun(t *testing.T, fake *fakePITR) restore.PITRRun {
	t.Helper()
	select {
	case <-fake.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the restore did not run")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.runs[len(fake.runs)-1]
}

// TestPITRRestoreFromACopyChain proves that a point-in-time restore can read the
// base and every chunk from a copy target, and that it is refused while one chunk
// of the range has no completed copy there.
func TestPITRRestoreFromACopyChain(t *testing.T) {
	svc, _ := pitrService(t, nil)
	withCopyChain(t, svc, "k110", "k120")
	req := pitrAt(125, "shop")
	req.SourceTargetID = "tgt_dr"
	if _, err := svc.StartRestore(admin(), req); !errors.Is(err, operations.ErrPITRNotRestorable) {
		t.Fatalf("restore from an incomplete copy chain = %v; want ErrPITRNotRestorable", err)
	}

	svc, fake := pitrService(t, nil)
	withCopyChain(t, svc, "k110", "k120", "k130")
	rec, err := svc.StartRestore(admin(), req)
	if err != nil {
		t.Fatalf("restore from a complete copy chain: %v", err)
	}
	if rec.SourceTargetID != "tgt_dr" || rec.SourceFallback != "" {
		t.Fatalf("record source = %q (%q); want tgt_dr", rec.SourceTargetID, rec.SourceFallback)
	}
	run := lastRun(t, fake)
	if run.Base.StorageTargetID != "tgt_dr" || len(run.Plan.Chunks) != 3 {
		t.Fatalf("run base on %s with %d chunks", run.Base.StorageTargetID, len(run.Plan.Chunks))
	}
	for _, c := range run.Plan.Chunks {
		if c.TargetID != "tgt_dr" || c.VersionID != "v-"+c.ID {
			t.Fatalf("chunk %+v is not read from its copy", c)
		}
	}
}

// TestPITRRestoreFallsBackToACopyChain proves that a restore whose primary base is
// missing reads a complete copy chain by itself and says so.
func TestPITRRestoreFallsBackToACopyChain(t *testing.T) {
	svc, fake := pitrService(t, nil)
	withCopyChain(t, svc, "k110", "k120", "k130")
	if _, err := svcStore(t, svc).UpdateBackupRecord(context.Background(), "b1", func(r *models.BackupRecord) error {
		r.Status = models.StatusMissing
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := svc.StartRestore(admin(), pitrAt(125, "shop"))
	if err != nil {
		t.Fatalf("restore with a missing primary base: %v", err)
	}
	if rec.SourceTargetID != "tgt_dr" || rec.SourceFallback == "" {
		t.Fatalf("record source = %q (%q); want a fallback to tgt_dr", rec.SourceTargetID, rec.SourceFallback)
	}
	if run := lastRun(t, fake); run.Base.StorageTargetID != "tgt_dr" || run.Plan.Chunks[0].TargetID != "tgt_dr" {
		t.Fatalf("run reads base from %s, chunks from %s", run.Base.StorageTargetID, run.Plan.Chunks[0].TargetID)
	}
}

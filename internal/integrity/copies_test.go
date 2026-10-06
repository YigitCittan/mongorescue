package integrity

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestSweepVerifiesCopiesAndRequeuesDamagedOnes proves that the sweep re-reads
// every completed copy, and that a damaged copy is queued for a new copy with a
// backup.copy_failed event while a healthy one is recorded ok.
func TestSweepVerifiesCopiesAndRequeuesDamagedOnes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	good, bad := storage.NewMockStorage(), storage.NewMockStorage()
	f.targets.drivers["tgt_good"], f.targets.drivers["tgt_bad"] = good, bad
	data := []byte("archive with two copies")
	rec := f.putBackup(t, "bkp_copies", "", f.now.Add(-time.Hour), data, func(r *models.BackupRecord) {
		r.PlanCopies([]models.CopyTarget{{ID: "tgt_good", Name: "good"}, {ID: "tgt_bad", Name: "bad"}}, "")
		r.Copies[0].Status, r.Copies[1].Status = models.CopyDone, models.CopyDone
	})
	if _, err := good.Save(ctx, rec.StorageKey, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Save(ctx, rec.StorageKey, bytes.NewReader([]byte("damaged copy bytes!!!!"))); err != nil {
		t.Fatal(err)
	}
	st, err := f.svc.Sweep(ctx, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	if st.OK != 1 || st.CopiesOK != 1 || st.CopiesMismatch != 1 {
		t.Fatalf("sweep = %+v", st)
	}
	got := f.get(t, "bkp_copies")
	if c := got.Copies[0]; c.Status != models.CopyDone || c.Verification != models.VerificationOK {
		t.Fatalf("healthy copy = %+v", c)
	}
	if c := got.Copies[1]; c.Status != models.CopyPending || c.Verification != models.VerificationMismatch || c.Error == "" {
		t.Fatalf("damaged copy = %+v", c)
	}
	if got.Verification != models.VerificationOK {
		t.Fatalf("the primary's outcome must stay its own: %q", got.Verification)
	}
	found := false
	f.pub.mu.Lock()
	defer f.pub.mu.Unlock()
	for _, e := range f.pub.got {
		if e.Type == events.BackupCopyFailed && e.TargetID == "tgt_bad" {
			found = true
		}
	}
	if !found {
		t.Fatal("no backup.copy_failed event for the damaged copy")
	}
}

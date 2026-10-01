package integrity

import (
	"context"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestCancelledRestoreTestDropsItsDatabase(t *testing.T) {
	f, job, rec := restoreTestFixture(t)
	// The run is cancelled the way run control cancels runs: with a Cancellation
	// as the context's cause, while mongorestore runs.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	f.restorer.execute = func(runCtx context.Context, _ models.RestoreRequest) error {
		cancel(&runs.Cancellation{By: "alice", Kind: runs.ActorUser})
		<-runCtx.Done()
		return runCtx.Err()
	}
	res := f.svc.runRestoreTest(ctx, job, rec, TriggerManual)
	if res.Status != models.RestoreTestError || !res.Dropped || len(f.admin.dropped) != 1 || f.admin.dropped[0] != res.TempDatabase {
		t.Fatalf("cancelled test = %+v, dropped %v", res, f.admin.dropped)
	}
	neverTouchedSource(t, f, "shop")
}

func TestUnreadableRowsOwnTheirArchives(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.putBackup(t, "bkp_bad", "", f.now, []byte("archive"), nil)
	// The row no longer decodes (a field of the wrong type): record lists skip it.
	storetest.CorruptRow(t, f.st, "backups", rec.ID, `{"id":"bkp_bad","storage_key":"`+rec.StorageKey+`","status":42}`)
	report, err := f.svc.ScanTarget(ctx, "tgt_local", TriggerManual)
	if err != nil || report.OrphanCount != 0 || report.Unreadable != 1 || report.MissingCount != 0 {
		t.Fatalf("scan = %+v, %v; an unreadable row's archive is neither orphan nor missing", report, err)
	}
	if _, err := f.svc.StartImport(ctx, "tgt_local", rec.StorageKey); err == nil {
		t.Fatal("importing over an unreadable row's archive must be refused")
	}
}

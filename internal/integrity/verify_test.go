package integrity

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

func TestVerifyOKAndMismatch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.putBackup(t, "bkp_a", "job_1", f.now.Add(-time.Hour), []byte("archive a"), func(r *models.BackupRecord) {
		r.Pinned = true
	})

	got, err := f.svc.Verify(ctx, rec.ID, events.VerificationOnDemand, 0)
	if err != nil || got.Verification != models.VerificationOK || got.VerifiedAt == nil || !got.Pinned {
		t.Fatalf("verify ok = %+v, %v", got, err)
	}

	f.corrupt(t, rec)
	got, err = f.svc.Verify(ctx, rec.ID, events.VerificationSweep, 0)
	if err != nil || got.Verification != models.VerificationMismatch || got.Status != models.StatusCompleted {
		t.Fatalf("verify mismatch = %+v, %v; the record stays completed, flagged", got, err)
	}
	if types := f.pub.types(); !slices.Equal(types, []events.EventType{events.VerificationSucceeded, events.VerificationFailed}) {
		t.Fatalf("events = %v", types)
	}

	if _, err := f.svc.Verify(ctx, "nope", events.VerificationOnDemand, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown backup = %v", err)
	}
	failed := f.putBackup(t, "bkp_failed", "", f.now, []byte("x"), func(r *models.BackupRecord) { r.Status = models.StatusFailed })
	if _, err := f.svc.StartVerify(ctx, failed.ID); !errors.Is(err, ErrNotVerifiable) {
		t.Fatalf("failed backup = %v", err)
	}
}

func TestStartVerifyRunsInBackground(t *testing.T) {
	f := newFixture(t)
	rec := f.putBackup(t, "bkp_a", "", f.now, []byte("archive"), nil)
	snapshot, err := f.svc.StartVerify(context.Background(), rec.ID)
	if err != nil || snapshot.ID != rec.ID {
		t.Fatalf("start = %+v, %v", snapshot, err)
	}
	f.waitIdle(t)
	if got := f.get(t, rec.ID); got.Verification != models.VerificationOK {
		t.Fatalf("after background verify: %+v", got)
	}
}

func TestSweepOrder(t *testing.T) {
	at := func(h int) *time.Time { v := time.Date(2026, 9, 1, h, 0, 0, 0, time.UTC); return &v }
	recs := []*models.BackupRecord{
		{ID: "verified_late", Status: models.StatusCompleted, SHA256: "a", StorageKey: "k", StartedAt: *at(1), VerifiedAt: at(20)},
		{ID: "never_new", Status: models.StatusCompleted, SHA256: "a", StorageKey: "k", StartedAt: *at(9)},
		{ID: "failed", Status: models.StatusFailed, SHA256: "a", StorageKey: "k", StartedAt: *at(0)},
		{ID: "verified_early", Status: models.StatusCompleted, SHA256: "a", StorageKey: "k", StartedAt: *at(8), VerifiedAt: at(10)},
		{ID: "never_old", Status: models.StatusCompleted, SHA256: "a", StorageKey: "k", StartedAt: *at(2)},
		{ID: "no_checksum", Status: models.StatusCompleted, StorageKey: "k", StartedAt: *at(3)},
		{ID: "missing", Status: models.StatusMissing, SHA256: "a", StorageKey: "k", StartedAt: *at(4)},
	}
	var ids []string
	for _, r := range SweepOrder(recs) {
		ids = append(ids, r.ID)
	}
	if want := []string{"never_old", "never_new", "verified_early", "verified_late"}; !slices.Equal(ids, want) {
		t.Fatalf("order = %v, want %v", ids, want)
	}
}

func TestSweepVerifiesEverythingAndRecordsStatus(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.putBackup(t, "bkp_1", "", f.now.Add(-3*time.Hour), []byte("one"), nil)
	bad := f.putBackup(t, "bkp_2", "", f.now.Add(-2*time.Hour), []byte("two"), nil)
	f.putBackup(t, "bkp_3", "", f.now.Add(-time.Hour), []byte("three"), func(r *models.BackupRecord) { r.StorageKey = "shop/gone.archive" })
	f.corrupt(t, bad)

	st, err := f.svc.Sweep(ctx, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	if st.Running || st.Total != 3 || st.Done != 3 || st.OK != 1 || st.Mismatch != 1 || st.Errors != 1 || st.FinishedAt == nil {
		t.Fatalf("status = %+v", st)
	}
	stored := f.svc.SweepStatus(ctx)
	if stored.Done != 3 || stored.Trigger != TriggerManual || stored.Schedule != string(settings.SweepOff) || stored.NextRunAt != nil {
		t.Fatalf("stored status = %+v", stored)
	}

	// The next sweep starts with the least recently verified backup.
	f.advance(time.Minute)
	recs, _ := f.st.ListBackupRecords(ctx, "")
	order := SweepOrder(recs)
	if order[0].ID != "bkp_1" {
		t.Fatalf("after a sweep the oldest-verified backup comes first: %s", order[0].ID)
	}
}

func TestSweepIsSingleFlightAndCancellable(t *testing.T) {
	f := newFixture(t)
	f.putBackup(t, "bkp_1", "", f.now, []byte("one"), nil)
	if !f.svc.sweepMu.TryLock() {
		t.Fatal("lock")
	}
	if _, err := f.svc.Sweep(context.Background(), TriggerManual); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent sweep = %v", err)
	}
	if _, err := f.svc.StartSweep(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent start = %v", err)
	}
	f.svc.sweepMu.Unlock()

	// The sweep is cancelled (shutdown) while it reads the first archive.
	ctx, cancel := context.WithCancel(context.Background())
	f.targets.drivers["tgt_local"] = &cancellingStorage{MockStorage: f.mem, cancel: cancel}
	st, err := f.svc.Sweep(ctx, TriggerScheduled)
	if !errors.Is(err, context.Canceled) || st.Interrupted == "" || st.Done != 0 {
		t.Fatalf("cancelled sweep = %+v, %v", st, err)
	}
	if got := f.get(t, "bkp_1"); got.Verification != "" {
		t.Fatalf("a cancelled sweep must not record outcomes: %+v", got)
	}
}

// cancellingStorage cancels a context when an archive is read.
type cancellingStorage struct {
	*storage.MockStorage
	cancel context.CancelFunc
}

func (c *cancellingStorage) Retrieve(ctx context.Context, key string) (io.ReadCloser, error) {
	c.cancel()
	return c.MockStorage.Retrieve(context.WithoutCancel(ctx), key)
}

func TestScheduledSweepIsDueAndLoopStops(t *testing.T) {
	f := newFixture(t)
	f.putBackup(t, "bkp_1", "", f.now, []byte("one"), nil)
	f.settings.Integrity.SweepSchedule = settings.SweepDaily
	f.settings.Integrity.StorageScan = false
	ctx := context.Background()
	if next := f.svc.SweepStatus(ctx).NextRunAt; next == nil || next.After(f.now) {
		t.Fatalf("a never-run daily sweep is due now: %v", next)
	}
	f.svc.RunDue(ctx)
	if got := f.get(t, "bkp_1"); got.Verification != models.VerificationOK {
		t.Fatalf("RunDue did not sweep: %+v", got)
	}
	if next := f.svc.SweepStatus(ctx).NextRunAt; next == nil || !next.Equal(f.now.Add(24*time.Hour)) {
		t.Fatalf("next sweep = %v", next)
	}

	// An interrupted sweep found at start-up is marked so.
	if err := f.st.SaveIntegrityState(ctx, stateSweep, SweepStatus{Running: true, Current: "bkp_1"}); err != nil {
		t.Fatal(err)
	}
	f.svc.cfg.StartDelay, f.svc.cfg.CheckInterval = time.Hour, time.Hour
	f.svc.Start(ctx)
	f.svc.Start(ctx) // idempotent
	f.svc.Stop()
	f.svc.Stop()
	if st := f.svc.SweepStatus(ctx); st.Running || st.Interrupted == "" {
		t.Fatalf("recovered status = %+v", st)
	}
}

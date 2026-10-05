package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

const testGrace = 7 * 24 * time.Hour

// saveDeleted stores a backup deleted at deletedAt with grace, its archive on mock.
func saveDeleted(t *testing.T, st *store.SQLiteStore, mock *storage.MockStorage, id string, deletedAt time.Time, grace time.Duration, mutate func(*models.BackupRecord)) {
	t.Helper()
	ctx := context.Background()
	r := &models.BackupRecord{ID: id, JobID: "job_p", Database: "shop", Status: models.StatusCompleted,
		StorageKey: "shop/" + id + ".archive", StartedAt: deletedAt.Add(-time.Hour)}
	r.MarkDeleted(models.SoftDelete{At: deletedAt, PurgeAfter: deletedAt.Add(grace), By: "admin"})
	if mutate != nil {
		mutate(r)
	}
	if err := st.SaveBackupRecord(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := mock.Save(ctx, r.StorageKey, strings.NewReader("archive")); err != nil {
		t.Fatal(err)
	}
}

func fixedStorage(m storage.Storage) StorageFunc {
	return func(context.Context, string) (storage.Storage, error) { return m, nil }
}

// TestPurgeWaitsForTheGracePeriod proves that nothing a deletion keeps becomes
// unrecoverable before its grace period: before purge_after the archive stays and the
// record is still deleted (undeletable); once it has passed, the purge removes it.
func TestPurgeWaitsForTheGracePeriod(t *testing.T) {
	ctx := context.Background()
	st, mock := storetest.New(t), storage.NewMockStorage()
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	saveDeleted(t, st, mock, "bkp_a", t0, testGrace, nil)
	saveDeleted(t, st, mock, "bkp_pinned", t0, testGrace, func(r *models.BackupRecord) { r.Pinned = true })
	saveDeleted(t, st, mock, "bkp_new", t0.Add(6*24*time.Hour), testGrace, nil)
	keep := &models.BackupRecord{ID: "bkp_live", Database: "shop", Status: models.StatusCompleted, StorageKey: "shop/live.archive", StartedAt: t0}
	if err := st.SaveBackupRecord(ctx, keep); err != nil {
		t.Fatal(err)
	}
	_, _ = mock.Save(ctx, keep.StorageKey, strings.NewReader("archive"))

	for _, at := range []time.Time{t0, t0.Add(testGrace - time.Second)} {
		purged, err := PurgeDeleted(ctx, at, testGrace, st, fixedStorage(mock), nil, nil)
		if err != nil || len(purged) != 0 {
			t.Fatalf("purge at %s = %v, %v; want nothing before the grace period ends", at, purged, err)
		}
	}
	if _, err := mock.Stat(ctx, "shop/bkp_a.archive"); err != nil {
		t.Fatalf("archive gone before the grace period: %v", err)
	}

	var outcomes []PurgeOutcome
	purged, err := PurgeDeleted(ctx, t0.Add(testGrace), testGrace, st, fixedStorage(mock), nil,
		func(_ context.Context, o PurgeOutcome) { outcomes = append(outcomes, o) })
	if err != nil || !slices.Equal(purged, []string{"bkp_a"}) {
		t.Fatalf("purge = %v, %v; want only bkp_a (not pinned, not newer than the grace period)", purged, err)
	}
	if len(outcomes) != 1 || !outcomes[0].ArchiveDeleted {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	if _, err := mock.Stat(ctx, "shop/bkp_a.archive"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("purged archive still there: %v", err)
	}
	for _, key := range []string{"shop/bkp_pinned.archive", "shop/bkp_new.archive", "shop/live.archive"} {
		if _, err := mock.Stat(ctx, key); err != nil {
			t.Errorf("%s must stay: %v", key, err)
		}
	}
	rec, _ := st.GetBackupRecord(ctx, "bkp_a")
	if rec.Status != models.StatusPurged || rec.PurgedAt == nil {
		t.Fatalf("bkp_a = %+v; want purged", rec)
	}
}

// TestPurgeHonoursARaisedGracePeriod proves that raising the grace period protects
// deletions made before: they are purged grace after their deletion, not at the
// purge_after recorded then.
func TestPurgeHonoursARaisedGracePeriod(t *testing.T) {
	ctx := context.Background()
	st, mock := storetest.New(t), storage.NewMockStorage()
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	saveDeleted(t, st, mock, "bkp_a", t0, 24*time.Hour, nil)
	if purged, _ := PurgeDeleted(ctx, t0.Add(2*24*time.Hour), testGrace, st, fixedStorage(mock), nil, nil); len(purged) != 0 {
		t.Fatalf("purged %v under the raised grace period", purged)
	}
	if purged, _ := PurgeDeleted(ctx, t0.Add(testGrace), testGrace, st, fixedStorage(mock), nil, nil); len(purged) != 1 {
		t.Fatalf("purged %v; want bkp_a once the raised grace period ended", purged)
	}
}

// failingStorage refuses deletions.
type failingStorage struct{ *storage.MockStorage }

func (failingStorage) Delete(context.Context, string) error { return errors.New("access denied") }

// TestPurgeRetriesWhenTheArchiveCannotBeDeleted proves that a backup whose archive
// cannot be deleted stays deleted (still undeletable) and is retried.
func TestPurgeRetriesWhenTheArchiveCannotBeDeleted(t *testing.T) {
	ctx := context.Background()
	st, mock := storetest.New(t), storage.NewMockStorage()
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	saveDeleted(t, st, mock, "bkp_a", t0, testGrace, nil)
	purged, err := PurgeDeleted(ctx, t0.Add(testGrace), testGrace, st, fixedStorage(failingStorage{mock}), nil, nil)
	if err == nil || len(purged) != 0 {
		t.Fatalf("purge = %v, %v; want an error and nothing purged", purged, err)
	}
	if rec, _ := st.GetBackupRecord(ctx, "bkp_a"); rec.Status != models.StatusDeleted {
		t.Fatalf("bkp_a = %s; want still deleted", rec.Status)
	}
	if purged, err = PurgeDeleted(ctx, t0.Add(testGrace), testGrace, st, fixedStorage(mock), nil, nil); err != nil || len(purged) != 1 {
		t.Fatalf("retry = %v, %v", purged, err)
	}
}

// TestPurgeSkipsABackupUndeletedMeanwhile proves that the purge re-reads the record
// under the deletion lock.
func TestPurgeSkipsABackupUndeletedMeanwhile(t *testing.T) {
	ctx := context.Background()
	st, mock := storetest.New(t), storage.NewMockStorage()
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	saveDeleted(t, st, mock, "bkp_a", t0, testGrace, nil)
	listed, err := st.GetBackupRecord(ctx, "bkp_a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.UpdateBackupRecord(ctx, "bkp_a", func(r *models.BackupRecord) error { r.Undelete(); return nil }); err != nil {
		t.Fatal(err)
	}
	run := purgeRun{now: t0.Add(testGrace), grace: func() time.Duration { return testGrace }, store: st, storages: fixedStorage(mock),
		logger: slog.New(slog.DiscardHandler)}
	if _, err = run.purgeOne(ctx, listed); !errors.Is(err, errPurgeSkip) {
		t.Fatalf("purge of an undeleted backup = %v; want skipped", err)
	}
	if _, err = mock.Stat(ctx, "shop/bkp_a.archive"); err != nil {
		t.Fatalf("the archive of an undeleted backup must stay: %v", err)
	}
	if rec, _ := st.GetBackupRecord(ctx, "bkp_a"); rec.Status != models.StatusCompleted {
		t.Fatalf("bkp_a = %s; want completed again", rec.Status)
	}
}

// TestPurgeReadsTheGracePeriodUnderTheLock proves that a grace period raised after
// the purge listed a backup protects it: the value is read again per backup.
func TestPurgeReadsTheGracePeriodUnderTheLock(t *testing.T) {
	ctx := context.Background()
	st, mock := storetest.New(t), storage.NewMockStorage()
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	saveDeleted(t, st, mock, "bkp_a", t0, testGrace, nil)
	reads := 0
	run := purgeRun{now: t0.Add(testGrace), store: st, storages: fixedStorage(mock), logger: slog.New(slog.DiscardHandler),
		grace: func() time.Duration {
			// The listing sees 7 days, the locked re-check 30.
			if reads++; reads == 1 {
				return testGrace
			}
			return 30 * 24 * time.Hour
		}}
	purged, err := run.run(ctx)
	if err != nil || len(purged) != 0 {
		t.Fatalf("purge = %v, %v; want nothing under the raised grace period", purged, err)
	}
	if _, err = mock.Stat(ctx, "shop/bkp_a.archive"); err != nil {
		t.Fatalf("archive removed: %v", err)
	}
}

// aliasLocator maps two targets onto one place, like two storage targets naming the
// same bucket and prefix.
func aliasLocator(_ context.Context, _, key string) (string, error) { return "s3:bucket|" + key, nil }

// TestPurgeKeepsAnObjectAnotherTargetAliases proves the purge checks every target:
// target T2 aliases T1, so a deleted backup on T1 must not take the object a live
// backup on T2 resolves to.
func TestPurgeKeepsAnObjectAnotherTargetAliases(t *testing.T) {
	ctx := context.Background()
	st, mock := storetest.New(t), storage.NewMockStorage()
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	saveDeleted(t, st, mock, "bkp_t1", t0, testGrace, func(r *models.BackupRecord) {
		r.StorageTargetID, r.StorageKey = "tgt_1", "shop/same.archive"
	})
	alias := &models.BackupRecord{ID: "bkp_t2", Database: "shop", Status: models.StatusCompleted, StorageTargetID: "tgt_2",
		StorageKey: "shop/same.archive", StartedAt: t0}
	if err := st.SaveBackupRecord(ctx, alias); err != nil {
		t.Fatal(err)
	}
	run := purgeRun{now: t0.Add(testGrace), grace: func() time.Duration { return testGrace }, store: st, storages: fixedStorage(mock),
		locate: aliasLocator, logger: slog.New(slog.DiscardHandler)}
	var outcomes []PurgeOutcome
	run.onPurged = func(_ context.Context, o PurgeOutcome) { outcomes = append(outcomes, o) }
	if purged, err := run.run(ctx); err != nil || len(purged) != 1 {
		t.Fatalf("purge = %v, %v", purged, err)
	}
	if len(outcomes) != 1 || outcomes[0].ArchiveDeleted || outcomes[0].ArchiveKept != "bkp_t2" {
		t.Fatalf("outcome = %+v; want the archive kept for bkp_t2 on the other target", outcomes)
	}
	if _, err := mock.Stat(ctx, "shop/same.archive"); err != nil {
		t.Fatalf("the aliased object was deleted: %v", err)
	}
}

// TestPurgeLocatesEveryRecordOncePerRun proves the purge builds its index of
// physical objects once per run: every record is located once (plus once per purged
// backup), not once per purged backup and record, and the archives nobody else
// holds are still deleted.
func TestPurgeLocatesEveryRecordOncePerRun(t *testing.T) {
	ctx := context.Background()
	st, mock := storetest.New(t), storage.NewMockStorage()
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	const deleted, live = 6, 20
	for i := range deleted {
		saveDeleted(t, st, mock, fmt.Sprintf("bkp_d%02d", i), t0, testGrace, func(r *models.BackupRecord) { r.StorageTargetID = "tgt_1" })
	}
	for i := range live {
		r := &models.BackupRecord{ID: fmt.Sprintf("bkp_l%02d", i), Database: "shop", Status: models.StatusCompleted, StorageTargetID: "tgt_2",
			StorageKey: fmt.Sprintf("shop/live-%02d.archive", i), StartedAt: t0}
		if err := st.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int64
	locate := func(ctx context.Context, target, key string) (string, error) {
		calls.Add(1)
		return aliasLocator(ctx, target, key)
	}
	run := purgeRun{now: t0.Add(testGrace), grace: func() time.Duration { return testGrace }, store: st, storages: fixedStorage(mock),
		locate: locate, logger: slog.New(slog.DiscardHandler)}
	if purged, err := run.run(ctx); err != nil || len(purged) != deleted {
		t.Fatalf("purge = %v, %v", purged, err)
	}
	if n := calls.Load(); n > deleted+live+deleted {
		t.Fatalf("locate called %d times; want at most %d (one index per run)", n, deleted+live+deleted)
	}
	for i := range deleted {
		if _, err := mock.Stat(ctx, fmt.Sprintf("shop/bkp_d%02d.archive", i)); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("archive %d after the purge: %v; want deleted", i, err)
		}
	}
}

// TestPurgeLocatesThroughTheTargetNormalisation wires the purge to the real
// targets.Service locator: two targets spelling Amazon S3 differently (no endpoint
// and a regional endpoint) on the same bucket and prefix name one object, so the
// purge keeps it for the live backup on the other target.
func TestPurgeLocatesThroughTheTargetNormalisation(t *testing.T) {
	ctx := context.Background()
	st, mock := storetest.New(t), storage.NewMockStorage()
	for id, endpoint := range map[string]string{"tgt_1": "", "tgt_2": "https://S3.eu-west-1.amazonaws.com:443/"} {
		tg := &models.StorageTarget{ID: id, Name: id, Type: models.StorageS3,
			S3: &models.S3Target{Endpoint: endpoint, Region: "eu-west-1", Bucket: "bucket", Prefix: "team/"}}
		if err := st.CreateStorageTarget(ctx, tg); err != nil {
			t.Fatal(err)
		}
	}
	svc := targets.NewService(st, func(context.Context, *models.StorageTarget, string) (storage.Storage, error) {
		return mock, nil
	}, t.TempDir())
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	saveDeleted(t, st, mock, "bkp_t1", t0, testGrace, func(r *models.BackupRecord) {
		r.StorageTargetID, r.StorageKey = "tgt_1", "shop/same.archive"
	})
	alias := &models.BackupRecord{ID: "bkp_t2", Database: "shop", Status: models.StatusCompleted, StorageTargetID: "tgt_2",
		StorageKey: "/shop/same.archive", StartedAt: t0}
	if err := st.SaveBackupRecord(ctx, alias); err != nil {
		t.Fatal(err)
	}
	run := purgeRun{now: t0.Add(testGrace), grace: func() time.Duration { return testGrace }, store: st, storages: fixedStorage(mock),
		locate: svc.ObjectLocation, logger: slog.New(slog.DiscardHandler)}
	var outcomes []PurgeOutcome
	run.onPurged = func(_ context.Context, o PurgeOutcome) { outcomes = append(outcomes, o) }
	if purged, err := run.run(ctx); err != nil || len(purged) != 1 {
		t.Fatalf("purge = %v, %v", purged, err)
	}
	if len(outcomes) != 1 || outcomes[0].ArchiveDeleted || outcomes[0].ArchiveKept != "bkp_t2" {
		t.Fatalf("outcome = %+v; want the archive kept for bkp_t2 on the aliasing target", outcomes)
	}
}

// TestMaintainPurgesWithTheConfiguredClockAndGrace runs the scheduler's maintenance
// with an injected clock and grace period: the purge is audited and published, the
// maintenance hooks run afterwards.
func TestMaintainPurgesWithTheConfiguredClockAndGrace(t *testing.T) {
	ctx := context.Background()
	st, mock := storetest.New(t), storage.NewMockStorage()
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	saveDeleted(t, st, mock, "bkp_a", t0, 2*24*time.Hour, nil)
	var now atomic.Int64
	now.Store(t0.Add(24 * time.Hour).UnixNano())
	pub, aud := &recordingPublisher{}, &recordingAuditor{}
	hooks := 0
	sched := NewScheduler(st, nil, mock, nil, WithPublisher(pub), WithAuditor(aud),
		WithClock(func() time.Time { return time.Unix(0, now.Load()).UTC() }),
		WithDeleteGrace(func() time.Duration { return 2 * 24 * time.Hour }),
		WithMaintenance(func(context.Context) { hooks++ }))
	sched.Maintain(ctx)
	if rec, _ := st.GetBackupRecord(ctx, "bkp_a"); rec.Status != models.StatusDeleted || hooks != 1 {
		t.Fatalf("after one day: %s, hooks %d; want deleted and the hook run", rec.Status, hooks)
	}
	now.Store(t0.Add(2 * 24 * time.Hour).UnixNano())
	sched.Maintain(ctx)
	if rec, _ := st.GetBackupRecord(ctx, "bkp_a"); rec.Status != models.StatusPurged {
		t.Fatalf("after the grace period: %s; want purged", rec.Status)
	}
	if len(aud.entries) != 1 || aud.entries[0].Tool != AuditToolPurge || aud.entries[0].Transport != audit.TransportSystem {
		t.Fatalf("audit = %+v; want one purge entry", aud.entries)
	}
	found := false
	for _, e := range pub.got {
		if e.Type == events.SecurityDestructiveAction && e.Action == "purge" && e.BackupID == "bkp_a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("events = %+v; want a security.destructive_action purge", pub.got)
	}
}

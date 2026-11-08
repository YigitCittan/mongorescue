package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// copyRetFixture is a job with a primary target, a long-term copy target and an
// override keeping the copies of the last two months.
type copyRetFixture struct {
	t       *testing.T
	st      *store.SQLiteStore
	primary *storage.MockStorage
	lt      storage.Storage
	ltMock  *storage.MockStorage
	job     *models.Job
}

func newCopyRetFixture(t *testing.T, lt storage.Storage, ltMock *storage.MockStorage) *copyRetFixture {
	t.Helper()
	f := &copyRetFixture{t: t, st: storetest.New(t), primary: storage.NewMockStorage(), lt: lt, ltMock: ltMock}
	f.job = &models.Job{
		ID: "job_c", Database: "shop", CronExpression: "@daily", Enabled: true, RetentionCount: 1,
		CopyTargets: []string{"tgt_lt"}, CopyRetention: map[string]models.RetentionPolicy{"tgt_lt": {KeepMonthly: 2}},
	}
	if err := f.st.SaveJob(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *copyRetFixture) storages(_ context.Context, id string) (storage.Storage, error) {
	switch id {
	case "tgt_p":
		return f.primary, nil
	case "tgt_lt":
		return f.lt, nil
	}
	return nil, errors.New("unknown target")
}

// add stores a completed scheduled backup of the job with archives on both targets.
func (f *copyRetFixture) add(id string, at time.Time) *models.BackupRecord {
	f.t.Helper()
	ctx := context.Background()
	r := &models.BackupRecord{ID: id, JobID: f.job.ID, Database: "shop", Status: models.StatusCompleted, StorageTargetID: "tgt_p",
		StorageKey: "shop/" + id + ".archive", StartedAt: at, SHA256: "aa"}
	r.PlanCopies([]models.CopyTarget{{ID: "tgt_lt"}}, "")
	r.Copies[0].Status, r.Copies[0].SHA256OK, r.Copies[0].SHA256 = models.CopyDone, true, "aa"
	if err := f.st.SaveBackupRecord(ctx, r); err != nil {
		f.t.Fatal(err)
	}
	for _, d := range []storage.Storage{f.primary, f.ltMock} {
		if _, err := d.Save(ctx, r.StorageKey, strings.NewReader("archive")); err != nil {
			f.t.Fatal(err)
		}
	}
	return r
}

func (f *copyRetFixture) record(id string) *models.BackupRecord {
	f.t.Helper()
	r, err := f.st.GetBackupRecord(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// retain runs the retention of the job at now: the copies its overrides keep are
// decided first, then the primary policy deletes (see Scheduler.applyRetention).
func (f *copyRetFixture) retain(now time.Time) []string {
	f.t.Helper()
	ctx := context.Background()
	all, err := f.st.ListBackupRecords(ctx, "shop")
	if err != nil {
		f.t.Fatal(err)
	}
	var keeps CopyKeeps
	if len(f.job.CopyRetention) > 0 {
		keeps = PlanCopyRetention(now, f.job, all)
		releaseCopies(ctx, f.st, f.job, keeps, all, slog.Default())
	}
	ids, err := pruneJob(ctx, now, testGrace, f.job.RetentionPolicy(), f.job.RetentionLocation(), keeps,
		JobRetentionHistory(f.job, "tgt_p", all), f.st, nil, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	slices.Sort(ids)
	return ids
}

func (f *copyRetFixture) purge(at time.Time) []string {
	f.t.Helper()
	purged, err := PurgeDeleted(context.Background(), at, testGrace, f.st, f.storages, nil, nil)
	if err != nil {
		f.t.Fatalf("purge at %s: %v", at, err)
	}
	return purged
}

func exists(t *testing.T, d storage.Storage, key string) bool {
	t.Helper()
	_, err := d.Stat(context.Background(), key)
	switch {
	case err == nil:
		return true
	case errors.Is(err, storage.ErrNotFound):
		return false
	}
	t.Fatal(err)
	return false
}

// TestCopyOutlivesItsPrimary proves the lifecycle of a copy whose target has a
// retention override: retention deletes the primaries (count 1), the copies the
// override keeps are marked retained and survive the purge of their primary, the
// record stays deleted (restorable from the copy) and, once the override stops
// keeping them, the next retention run releases them and the purge deletes them.
func TestCopyOutlivesItsPrimary(t *testing.T) {
	ctx := context.Background()
	lt := storage.NewMockStorage()
	f := newCopyRetFixture(t, lt, lt)
	sep := time.Date(2026, 9, 15, 2, 0, 0, 0, time.UTC)
	oct := time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC)
	nov := time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC)
	f.add("bkp_sep", sep)
	f.add("bkp_oct", oct)
	f.add("bkp_nov", nov)

	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	if got := f.retain(now); !slices.Equal(got, []string{"bkp_oct", "bkp_sep"}) {
		t.Fatalf("retention deleted %v, want bkp_oct and bkp_sep", got)
	}
	if c := f.record("bkp_oct").Copies[0]; !c.Retained || c.RetainedReason != "keep_monthly 2026-10" {
		t.Fatalf("the October copy = %+v, want retained for keep_monthly 2026-10", c)
	}
	if f.record("bkp_sep").Copies[0].Retained {
		t.Fatal("the September copy is outside the last two months and must follow its primary")
	}

	// Within the grace period nothing is purged; after it, the primaries go.
	if got := f.purge(now.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("purged %v inside the grace period", got)
	}
	f.purge(now.Add(testGrace + time.Hour))
	sepRec, octRec := f.record("bkp_sep"), f.record("bkp_oct")
	if sepRec.Status != models.StatusPurged || sepRec.Copies[0].Status != models.CopyPurged {
		t.Fatalf("September: %s, copy %s; want purged with its copy", sepRec.Status, sepRec.Copies[0].Status)
	}
	if octRec.Status != models.StatusDeleted || octRec.PrimaryPurgedAt == nil || octRec.Copies[0].Status != models.CopyDone || !octRec.Copies[0].Retained {
		t.Fatalf("October after the purge: %s, primary purged %v, copy %+v; want deleted, primary purged, copy retained", octRec.Status, octRec.PrimaryPurgedAt, octRec.Copies[0])
	}
	if exists(t, f.primary, "shop/bkp_sep.archive") || exists(t, lt, "shop/bkp_sep.archive") || exists(t, f.primary, "shop/bkp_oct.archive") {
		t.Fatal("the September archives and the October primary must be gone")
	}
	if !exists(t, lt, "shop/bkp_oct.archive") {
		t.Fatal("the October copy must outlive its primary")
	}
	// A repeated purge leaves the retained copy alone and does not purge the record.
	if got := f.purge(now.Add(testGrace + 2*time.Hour)); len(got) != 0 {
		t.Fatalf("a second purge reported %v", got)
	}
	if r := f.record("bkp_oct"); r.Status != models.StatusDeleted || !exists(t, lt, "shop/bkp_oct.archive") {
		t.Fatalf("October after a second purge: %s", r.Status)
	}
	// The retained copy is restorable by naming its target.
	view := f.record("bkp_oct").RetainedView("tgt_lt")
	if view == nil || view.Status != models.StatusCompleted || view.StorageTargetID != "tgt_lt" || view.StorageKey != "shop/bkp_oct.archive" {
		t.Fatalf("retained view = %+v", view)
	}
	if f.record("bkp_oct").RetainedView("tgt_p") != nil || f.record("bkp_sep").RetainedView("tgt_lt") != nil {
		t.Fatal("only a retained copy gives a view")
	}

	// A month later the override keeps one month: October's copy is released and
	// purged, November's primary and copy are untouched.
	f.add("bkp_dec", time.Date(2026, 12, 5, 2, 0, 0, 0, time.UTC))
	f.job.CopyRetention = map[string]models.RetentionPolicy{"tgt_lt": {KeepMonthly: 1}}
	dec := time.Date(2026, 12, 8, 12, 0, 0, 0, time.UTC)
	got := f.retain(dec)
	if !slices.Equal(got, []string{"bkp_nov"}) {
		t.Fatalf("December's retention deleted %v, want bkp_nov", got)
	}
	if f.record("bkp_oct").Copies[0].Retained {
		t.Fatal("the October copy must be released when the override no longer keeps it")
	}
	if f.record("bkp_nov").Copies[0].Retained {
		t.Fatal("the November copy is not kept by keep_monthly 1 (December is)")
	}
	f.purge(dec.Add(testGrace + time.Hour))
	if r := f.record("bkp_oct"); r.Status != models.StatusPurged || r.Copies[0].Status != models.CopyPurged {
		t.Fatalf("October after the release: %s, copy %s; want purged", r.Status, r.Copies[0].Status)
	}
	if exists(t, lt, "shop/bkp_oct.archive") {
		t.Fatal("the released copy must be deleted")
	}
	if !exists(t, lt, "shop/bkp_dec.archive") {
		t.Fatal("the live backup's copy must stay")
	}
	_ = ctx
}

// TestCopyWithoutOverrideFollowsItsPrimary proves a job with copies but no override
// deletes every copy together with its primary, as before.
func TestCopyWithoutOverrideFollowsItsPrimary(t *testing.T) {
	lt := storage.NewMockStorage()
	f := newCopyRetFixture(t, lt, lt)
	f.job.CopyRetention = nil
	f.add("bkp_old", time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC))
	f.add("bkp_new", time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC))
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	if got := f.retain(now); !slices.Equal(got, []string{"bkp_old"}) {
		t.Fatalf("deleted %v", got)
	}
	if f.record("bkp_old").Copies[0].Retained {
		t.Fatal("a copy without an override is never retained")
	}
	f.purge(now.Add(testGrace + time.Hour))
	if f.record("bkp_old").Status != models.StatusPurged || exists(t, lt, "shop/bkp_old.archive") || exists(t, f.primary, "shop/bkp_old.archive") {
		t.Fatal("the copy must be deleted with its primary")
	}
}

// TestRetainedCopyOfADeletedJobOrOverrideIsReleased proves the copies of deleted
// primaries are handled when nothing keeps them any more: the job's override was
// removed, the job was deleted, or the copy target was dropped from the job.
func TestRetainedCopyOfADeletedJobOrOverrideIsReleased(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		change func(f *copyRetFixture)
	}{
		{"override removed", func(f *copyRetFixture) {
			f.job.CopyRetention = nil
			if err := f.st.UpdateJob(ctx, f.job); err != nil {
				t.Fatal(err)
			}
		}},
		{"copy target dropped from the job", func(f *copyRetFixture) {
			f.job.CopyTargets = nil
			if err := f.st.UpdateJob(ctx, f.job); err != nil {
				t.Fatal(err)
			}
		}},
		{"job deleted", func(f *copyRetFixture) {
			if err := f.st.DeleteJob(ctx, f.job.ID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lt := storage.NewMockStorage()
			f := newCopyRetFixture(t, lt, lt)
			f.add("bkp_oct", time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC))
			f.add("bkp_nov", time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC))
			now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
			f.retain(now)
			f.purge(now.Add(testGrace + time.Hour))
			if r := f.record("bkp_oct"); r.Status != models.StatusDeleted || !r.Copies[0].Retained || !exists(t, lt, "shop/bkp_oct.archive") {
				t.Fatalf("setup: %s, copy %+v", r.Status, r.Copies[0])
			}
			tc.change(f)
			f.purge(now.Add(testGrace + 2*time.Hour))
			r := f.record("bkp_oct")
			if r.Status != models.StatusPurged || r.Copies[0].Status != models.CopyPurged || exists(t, lt, "shop/bkp_oct.archive") {
				t.Fatalf("after the change: %s, copy %+v; want purged and deleted", r.Status, r.Copies[0])
			}
		})
	}
}

// TestObjectLockOnTheCopyTargetStillWins proves a locked copy is never deleted
// before its lock ends: a retained copy under a long lock does not hold back the
// purge of its primary, and once the override releases it the purge waits for the
// lock before deleting it.
func TestObjectLockOnTheCopyTargetStillWins(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	until := t0.Add(400 * 24 * time.Hour)
	bucket := &lockedBucket{MockStorage: storage.NewMockStorage(), until: map[string]time.Time{"v-bkp_oct": until}, now: t0}
	f := newCopyRetFixture(t, bucket, bucket.MockStorage)
	oct := f.add("bkp_oct", time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC))
	f.add("bkp_nov", time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC))
	// The copy is under S3 Object Lock for 400 days (recorded on the copy).
	if _, err := f.st.UpdateBackupRecord(context.Background(), oct.ID, func(r *models.BackupRecord) error {
		u := until
		r.Copies[0].RetainUntil, r.Copies[0].ObjectLockMode = &u, models.ObjectLockCompliance
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.retain(t0)
	setNow := func(at time.Time) {
		bucket.mu.Lock()
		bucket.now = at
		bucket.mu.Unlock()
	}
	at := t0.Add(testGrace + time.Hour)
	setNow(at)
	f.purge(at)
	r := f.record("bkp_oct")
	if r.Status != models.StatusDeleted || r.PrimaryPurgedAt == nil || exists(t, f.primary, "shop/bkp_oct.archive") {
		t.Fatalf("a retained locked copy must not hold the primary's purge back: %s, primary purged %v", r.Status, r.PrimaryPurgedAt)
	}

	// The override stops keeping it while the lock is in force: nothing is deleted.
	f.job.CopyRetention = map[string]models.RetentionPolicy{"tgt_lt": {KeepMonthly: 1}}
	f.add("bkp_dec", time.Date(2026, 12, 5, 2, 0, 0, 0, time.UTC))
	dec := time.Date(2026, 12, 8, 12, 0, 0, 0, time.UTC)
	f.retain(dec)
	setNow(dec.Add(testGrace + time.Hour))
	f.purge(dec.Add(testGrace + time.Hour))
	if !exists(t, bucket, "shop/bkp_oct.archive") || slices.ContainsFunc(bucket.deleted, func(s string) bool { return strings.Contains(s, "bkp_oct") }) || len(bucket.markers) != 0 {
		t.Fatalf("a locked copy was deleted: deleted %v, markers %v", bucket.deleted, bucket.markers)
	}
	if st := f.record("bkp_oct").Status; st != models.StatusDeleted {
		t.Fatalf("status %s while the copy is locked", st)
	}
	// Once the lock has ended the copy is deleted and the record purged.
	setNow(until)
	f.purge(until)
	if r := f.record("bkp_oct"); r.Status != models.StatusPurged || exists(t, bucket, "shop/bkp_oct.archive") {
		t.Fatalf("after the lock: %s", r.Status)
	}
}

// TestUserDeletedBackupTakesItsCopiesAlong proves an explicit delete wins over the
// override: only retention marks copies retained, so a backup an operator deleted
// is purged with every copy.
func TestUserDeletedBackupTakesItsCopiesAlong(t *testing.T) {
	lt := storage.NewMockStorage()
	f := newCopyRetFixture(t, lt, lt)
	rec := f.add("bkp_x", time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC))
	del := time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC)
	if _, err := f.st.UpdateBackupRecord(context.Background(), rec.ID, func(r *models.BackupRecord) error {
		r.MarkDeleted(models.SoftDelete{At: del, PurgeAfter: del.Add(testGrace), By: "admin"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.purge(del.Add(testGrace + time.Hour))
	if r := f.record("bkp_x"); r.Status != models.StatusPurged || exists(t, lt, "shop/bkp_x.archive") {
		t.Fatalf("status %s; the copy must be deleted with the backup", r.Status)
	}
}

// TestPlanCopyRetentionPerTarget proves overrides are evaluated per copy target, on
// the copies' own state: a failed or missing copy is no candidate.
func TestPlanCopyRetentionPerTarget(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	job := &models.Job{ID: "job_c", CopyTargets: []string{"a", "b"}, CopyRetention: map[string]models.RetentionPolicy{
		"a": {KeepMonthly: 3}, "b": {RetentionDays: 20},
	}}
	mk := func(id string, at time.Time, copies ...models.BackupCopy) *models.BackupRecord {
		return &models.BackupRecord{ID: id, JobID: "job_c", Database: "db", Status: models.StatusCompleted, StartedAt: at, Copies: copies}
	}
	done := func(target string) models.BackupCopy {
		return models.BackupCopy{TargetID: target, Status: models.CopyDone}
	}
	failed := models.BackupCopy{TargetID: "a", Status: models.CopyFailed}
	recs := []*models.BackupRecord{
		mk("r_nov", time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC), done("a"), done("b")),
		mk("r_oct", time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC), done("a"), done("b")),
		mk("r_sep", time.Date(2026, 9, 15, 2, 0, 0, 0, time.UTC), failed, done("b")),
		mk("r_none", time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC)),
	}
	keeps := PlanCopyRetention(now, job, recs)
	ids := func(target string) []string {
		var out []string
		for id := range keeps[target] {
			out = append(out, id)
		}
		slices.Sort(out)
		return out
	}
	if got := ids("a"); !slices.Equal(got, []string{"r_nov", "r_oct"}) {
		t.Errorf("target a keeps %v, want r_nov and r_oct (r_sep's copy failed)", got)
	}
	// Target b: days 20 keeps r_nov (and r_oct is 24 days old: dropped), r_sep dropped.
	if got := ids("b"); !slices.Equal(got, []string{"r_nov"}) {
		t.Errorf("target b keeps %v, want r_nov", got)
	}
}

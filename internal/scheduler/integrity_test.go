package scheduler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// seedScheduled stores n completed scheduled backups of job_r, one every 2 days, the
// newest 2 days old, with archives on mock. mutate may adjust each record (index 0 is
// the newest).
func seedScheduled(t *testing.T, st *store.SQLiteStore, mock *storage.MockStorage, n int, mutate func(i int, r *models.BackupRecord)) []*models.BackupRecord {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	var out []*models.BackupRecord
	for i := range n {
		r := &models.BackupRecord{
			ID: fmt.Sprintf("bkp_%02d", i), JobID: "job_r", Trigger: models.TriggerScheduled, Database: "shop",
			Status: models.StatusCompleted, StorageKey: fmt.Sprintf("shop/%02d.archive", i), SizeBytes: 7,
			StartedAt: now.Add(-time.Duration(i+1) * 48 * time.Hour),
		}
		if mutate != nil {
			mutate(i, r)
		}
		if err := st.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
		if _, err := mock.Save(ctx, r.StorageKey, strings.NewReader("archive")); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func decisionIDs(plan RetentionPlan) []string {
	ids := make([]string, 0, len(plan.Delete))
	for _, d := range plan.Delete {
		ids = append(ids, d.Backup.ID)
	}
	return ids
}

func TestRetentionPreviewEqualsDeletion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		days, count int
	}{
		{"days", 7, 0},
		{"count", 0, 3},
		{"both", 5, 4},
		{"none", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, mock := storetest.New(t), storage.NewMockStorage()
			records := seedScheduled(t, st, mock, 8, func(i int, r *models.BackupRecord) {
				if i == 5 {
					r.Trigger = models.TriggerManual // never considered
				}
			})
			plan := PlanRetention(time.Now().UTC(), tc.days, tc.count, records)
			pruned, err := PruneBackups(context.Background(), tc.days, tc.count, records, st, mock, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := decisionIDs(plan)
			slices.Sort(want)
			slices.Sort(pruned)
			if !slices.Equal(want, pruned) {
				t.Fatalf("preview %v != deleted %v", want, pruned)
			}
			if slices.Contains(pruned, "bkp_05") {
				t.Fatal("a manual backup must never be pruned")
			}
		})
	}
}

func TestRetentionPlanReasonsAndOrder(t *testing.T) {
	st, mock := storetest.New(t), storage.NewMockStorage()
	records := seedScheduled(t, st, mock, 5, nil)
	plan := PlanRetention(time.Now().UTC(), 5, 0, records)
	// Ages are 2, 4, 6, 8 and 10 days: the three older than 5 days go, oldest first.
	if got := decisionIDs(plan); !slices.Equal(got, []string{"bkp_04", "bkp_03", "bkp_02"}) {
		t.Fatalf("delete = %v", got)
	}
	if plan.Delete[0].Reason != models.RetentionMaxAge || plan.Delete[0].Detail != "older than 5 days" || plan.Considered != 5 {
		t.Fatalf("plan = %+v", plan)
	}
	plan = PlanRetention(time.Now().UTC(), 0, 2, records)
	if plan.Delete[0].Reason != models.RetentionMaxCount || len(plan.Delete) != 3 {
		t.Fatalf("count plan = %+v", plan)
	}
}

func TestRetentionSkipsPinnedAndLastVerified(t *testing.T) {
	st, mock := storetest.New(t), storage.NewMockStorage()
	records := seedScheduled(t, st, mock, 6, func(i int, r *models.BackupRecord) {
		switch i {
		case 3:
			r.Pinned, r.PinNote = true, "legal hold"
		case 4:
			r.Verification = models.VerificationOK // the newest verified backup
		case 5:
			r.Verification = models.VerificationOK
		}
	})
	plan := PlanRetention(time.Now().UTC(), 0, 1, records)
	if got := decisionIDs(plan); !slices.Equal(got, []string{"bkp_05", "bkp_02", "bkp_01"}) {
		t.Fatalf("delete = %v", got)
	}
	wantProtected := []ProtectedBackup{{BackupID: "bkp_03", Reason: ProtectedPinned}, {BackupID: "bkp_04", Reason: ProtectedLastVerified}}
	if !slices.Equal(plan.Protected, wantProtected) {
		t.Fatalf("protected = %+v", plan.Protected)
	}
	pruned, err := PruneBackups(context.Background(), 0, 1, records, st, mock, nil)
	if err != nil || len(pruned) != 3 {
		t.Fatalf("pruned = %v, %v", pruned, err)
	}
	for _, id := range []string{"bkp_03", "bkp_04"} {
		rec, _ := st.GetBackupRecord(context.Background(), id)
		if rec.Status != models.StatusCompleted {
			t.Errorf("%s = %s; a pinned or last verified backup must be kept", id, rec.Status)
		}
	}
}

func TestRetentionRechecksPinBeforeDeleting(t *testing.T) {
	st, mock := storetest.New(t), storage.NewMockStorage()
	records := seedScheduled(t, st, mock, 3, nil)
	// The backup is pinned after the records were listed: the stale copy says
	// unpinned, the store says pinned.
	if _, err := st.UpdateBackupRecord(context.Background(), "bkp_02", func(r *models.BackupRecord) error {
		r.Pinned = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pruned, err := PruneBackups(context.Background(), 0, 1, records, st, mock, nil)
	if err != nil || !slices.Equal(pruned, []string{"bkp_01"}) {
		t.Fatalf("pruned = %v, %v", pruned, err)
	}
	if _, err := mock.Stat(context.Background(), "shop/02.archive"); err != nil {
		t.Fatalf("the archive of a backup pinned meanwhile must stay: %v", err)
	}
}

type recordingAuditor struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (a *recordingAuditor) Record(_ context.Context, e audit.Entry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

func TestScheduledRetentionRecordsLogAuditAndEvents(t *testing.T) {
	s, st, job := retentionFixture(t)
	metaStore := st.(*store.SQLiteStore)
	pub := &recordingPublisher{}
	aud := &recordingAuditor{}
	s.publisher, s.auditor, s.retentionLog = pub, aud, metaStore
	var afterCalls int
	s.afterBackup = func(_ context.Context, j *models.Job, rec *models.BackupRecord) {
		if j.ID == job.ID && rec.Status == models.StatusCompleted {
			afterCalls++
		}
	}
	if _, err := s.runBackupForJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	log, err := metaStore.ListRetentionLog(context.Background(), job.ID, 100)
	if err != nil || len(log) == 0 {
		t.Fatalf("retention log = %v, %v", log, err)
	}
	if len(aud.entries) != len(log) || aud.entries[0].Tool != AuditToolRetention || aud.entries[0].Transport != audit.TransportSystem {
		t.Fatalf("audit = %+v", aud.entries)
	}
	if n := countEvents(pub, events.RetentionDeleted); n != len(log) {
		t.Fatalf("retention events = %d, log = %d", n, len(log))
	}
	if afterCalls != 1 {
		t.Fatalf("after-backup hook ran %d times", afterCalls)
	}
}

// countEvents counts the events of type typ p received.
func countEvents(p *recordingPublisher, typ events.EventType) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.got {
		if e.Type == typ {
			n++
		}
	}
	return n
}

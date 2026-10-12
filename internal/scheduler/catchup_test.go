package scheduler

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestCatchUpRunsEachMissedJobOnce covers the missed-schedule catch-up of an HA
// takeover for an hourly job, at 10:20 local time (its fire at 10:00 is the most
// recent one).
func TestCatchUpRunsEachMissedJobOnce(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 10, 12, 0, 0, 0, 0, time.Local)
	at := func(h, m, s int) time.Time {
		return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
	}

	verified := func(context.Context, string) error { return nil }
	scanFailed := func(context.Context, string) error { return errors.New("the storage scan failed") }
	for name, tc := range map[string]struct {
		now      time.Time
		lastRun  time.Time // the newest scheduled run (zero: none)
		created  time.Time
		disabled bool
		backup   *models.BackupRecord // a recorded backup since the fire
		// imports is the import check (verified when nil and noCheck is false).
		imports ImportCheck
		noCheck bool
		// multi makes the job a multi-database job with these known databases.
		multi       bool
		known       []string
		wantDumps   int32
		wantSkipped bool
	}{
		// Unknown means covered: no catch-up, a warning instead.
		"the target scan failed":        {now: at(10, 20, 0), lastRun: at(8, 0, 1), imports: scanFailed, wantSkipped: true},
		"no snapshot, nothing imported": {now: at(10, 20, 0), lastRun: at(8, 0, 1), noCheck: true, wantSkipped: true},
		"databases unknown":             {now: at(10, 20, 0), lastRun: at(8, 0, 1), multi: true, wantSkipped: true},
		"multi-database job, verified":  {now: at(10, 20, 0), lastRun: at(8, 0, 1), multi: true, known: []string{"shop", "crm"}, wantDumps: 1},
		"multi-database job, an imported archive covers it": {now: at(10, 20, 0), lastRun: at(8, 0, 1), multi: true, known: []string{"shop", "crm"},
			backup: &models.BackupRecord{ID: "bkp_imp", Database: "crm", Imported: true, Status: models.StatusPending, StartedAt: at(10, 0, 3).UTC()}},
		"missed fire":                    {now: at(10, 20, 0), lastRun: at(8, 0, 1), wantDumps: 1},
		"fire already ran":               {now: at(10, 20, 0), lastRun: at(10, 0, 1), wantDumps: 0},
		"several missed fires, one run":  {now: at(10, 20, 0), lastRun: at(3, 0, 1), wantDumps: 1},
		"next fire is close":             {now: at(10, 50, 0), lastRun: at(8, 0, 1), wantDumps: 0},
		"disabled job":                   {now: at(10, 20, 0), lastRun: at(8, 0, 1), disabled: true, wantDumps: 0},
		"never ran, created before fire": {now: at(10, 20, 0), created: at(9, 30, 0), wantDumps: 1},
		"never ran, created after fire":  {now: at(10, 20, 0), created: at(10, 5, 0), wantDumps: 0},
		"imported archive covers the fire": {now: at(10, 20, 0), lastRun: at(8, 0, 1),
			backup: &models.BackupRecord{ID: "bkp_imp", Database: "shop", Imported: true, Status: models.StatusPending, StartedAt: at(10, 0, 3).UTC()}},
		"the job's own backup covers the fire": {now: at(10, 20, 0), lastRun: at(8, 0, 1),
			backup: &models.BackupRecord{ID: "bkp_own", JobID: "job_hourly", Database: "shop", Status: models.StatusCompleted, StartedAt: at(10, 0, 2).UTC()}},
		"a failed backup does not cover it": {now: at(10, 20, 0), lastRun: at(8, 0, 1), wantDumps: 1,
			backup: &models.BackupRecord{ID: "bkp_failed", JobID: "job_hourly", Database: "shop", Status: models.StatusFailed, StartedAt: at(10, 0, 2).UTC()}},
	} {
		t.Run(name, func(t *testing.T) {
			metaStore := storetest.New(t)
			var dumps atomic.Int32
			runner := func(context.Context, string, ...string) (io.ReadCloser, io.Reader, func() error, error) {
				dumps.Add(1)
				return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
			}
			engine := backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", backup.WithRunner(runner))
			pub := &catchUpEvents{}
			s := NewScheduler(metaStore, engine, storage.NewMockStorage(), nil, WithPublisher(pub))
			job := &models.Job{ID: "job_hourly", Database: "shop", CronExpression: "0 * * * *", Enabled: !tc.disabled, CreatedAt: tc.created.UTC()}
			if tc.multi {
				// The multi-database run is not executed here: the catch-up decision is.
				job.Database, job.KnownDatabases = "", tc.known
			}
			if err := metaStore.SaveJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if !tc.lastRun.IsZero() {
				prev := newRunOf(job.ID, models.TriggerScheduled)
				prev.StartedAt, prev.Status = tc.lastRun.UTC(), models.JobRunOK
				if err := metaStore.SaveJobRun(ctx, prev); err != nil {
					t.Fatal(err)
				}
			}
			if tc.backup != nil {
				if err := metaStore.SaveBackupRecord(ctx, tc.backup); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Start(ctx); err != nil {
				t.Fatal(err)
			}
			imports := tc.imports
			if imports == nil && !tc.noCheck {
				imports = verified
			}
			started := s.CatchUp(ctx, tc.now, imports)
			s.inflight.Wait() // the catch-up runs
			s.Stop()
			if len(started) != int(tc.wantDumps) || (!tc.multi && dumps.Load() != tc.wantDumps) {
				t.Fatalf("dumps = %d, started %v; want %d", dumps.Load(), started, tc.wantDumps)
			}
			if skipped := pub.count(events.HACatchUpSkipped); skipped != boolInt(tc.wantSkipped) {
				t.Fatalf("%d %s events; want %d", skipped, events.HACatchUpSkipped, boolInt(tc.wantSkipped))
			}
		})
	}
}

func TestCatchUpNeedsAStartedScheduler(t *testing.T) {
	s := NewScheduler(storetest.New(t), backup.NewEngine(storage.NewMockStorage(), "mongodb://x"), storage.NewMockStorage(), nil)
	if s.startCatchUp("job") {
		t.Fatal("a catch-up run started before Start")
	}
}

// catchUpEvents records published events.
type catchUpEvents struct {
	mu   sync.Mutex
	seen []events.Event
}

func (p *catchUpEvents) Publish(_ context.Context, e events.Event) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, e)
	return true
}

func (p *catchUpEvents) count(t events.EventType) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.seen {
		if e.Type == t {
			n++
		}
	}
	return n
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

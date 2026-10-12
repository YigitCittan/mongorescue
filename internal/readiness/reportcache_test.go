package readiness_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// countingReports counts the job listings of report computations (and checks);
// while gate is set they wait for it. It keeps the store's change stamp.
type countingReports struct {
	unedited
	calls atomic.Int32
	gate  chan struct{}
}

func (c *countingReports) ListJobs(ctx context.Context) ([]*models.Job, error) {
	c.calls.Add(1)
	if c.gate != nil {
		<-c.gate
	}
	return c.unedited.ListJobs(ctx)
}

// TestReportCacheSharesPerAccessAndFollowsEveryWrite checks that concurrent
// readers share one computation per connection access, and that every write to
// what the report reads (straight to the store, as the scheduler writes, and
// deletions), every event and every kick drop it.
func TestReportCacheSharesPerAccessAndFollowsEveryWrite(t *testing.T) {
	st := storetest.New(t)
	cs := &countingReports{unedited: unedited{st}}
	clk := &clock{now: t0.Add(48 * time.Hour)}
	svc := readiness.New(readiness.Config{Store: cs, Now: clk.Now, ReportCacheTTL: time.Hour})
	ctx := context.Background()
	saveJob(t, st, &models.Job{ID: "job_1", Name: "a", Database: "shop", CronExpression: "@daily", Enabled: true, ConnectionID: "c1"})
	saveJob(t, st, &models.Job{ID: "job_2", Name: "b", Database: "crm", CronExpression: "@daily", Enabled: true, ConnectionID: "c2"})

	cs.gate = make(chan struct{})
	var wg sync.WaitGroup
	reports := make([]*readiness.Report, 16)
	for i := range reports {
		wg.Go(func() {
			r, err := svc.Report(ctx)
			if err != nil {
				t.Error(err)
			}
			reports[i] = r
		})
	}
	for cs.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(cs.gate)
	wg.Wait()
	if n := cs.calls.Load(); n != 1 {
		t.Fatalf("%d computations for 16 concurrent readers; want 1", n)
	}
	first := reports[0]
	for _, r := range reports {
		if r != first {
			t.Fatal("readers got different reports")
		}
	}
	if len(first.Rows) != 2 || first.Summary.Warn != 2 {
		t.Fatalf("rows %+v", first.Rows)
	}

	// A caller limited to c1 gets its own report, computed once.
	limited := auth.WithPrincipal(ctx, &auth.Principal{Method: auth.MethodSession, Scope: auth.ScopeRead, Connections: auth.OnlyConnections("c1")})
	for range 2 {
		if r, err := svc.Report(limited); err != nil || len(r.Rows) != 1 || r.Rows[0].ConnectionID != "c1" {
			t.Fatalf("limited report: %+v, %v", r, err)
		}
	}
	if n := cs.calls.Load(); n != 2 {
		t.Fatalf("%d computations after the limited caller; want 2", n)
	}
	if r, _ := svc.Report(ctx); r != first {
		t.Fatal("the unlimited report was dropped by another caller's")
	}

	current := first
	expectFresh := func(what string, check func(*readiness.Report) bool) {
		t.Helper()
		before := cs.calls.Load()
		r, err := svc.Report(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r == current || cs.calls.Load() != before+1 || (check != nil && !check(r)) {
			t.Fatalf("after %s: a cached report (%d computations)", what, cs.calls.Load()-before)
		}
		current = r
		if again, _ := svc.Report(ctx); again != r {
			t.Fatalf("after %s: the new report was not shared", what)
		}
	}
	// A backup the scheduler stored: the failure is visible at once.
	saveBackup(t, st, &models.BackupRecord{ID: "b1", JobID: "job_1", Database: "shop", ConnectionID: "c1", StartedAt: clk.Now().Add(-time.Hour)})
	expectFresh("a scheduler backup", func(r *readiness.Report) bool {
		return r.Rows[0].LastGoodBackup != nil || r.Rows[1].LastGoodBackup != nil
	})
	saveBackup(t, st, &models.BackupRecord{ID: "b2", JobID: "job_2", Database: "crm", ConnectionID: "c2", Status: models.StatusFailed,
		StartedAt: clk.Now().Add(-time.Minute)})
	expectFresh("a failed backup", nil)
	if err := st.DeleteJob(ctx, "job_2"); err != nil {
		t.Fatal(err)
	}
	expectFresh("a job deletion", func(r *readiness.Report) bool { return len(r.Rows) == 1 })
	if _, err := st.AddRPOBreach(ctx, models.RPOBreach{JobID: "job_1", Database: "shop", Since: clk.Now()}); err != nil {
		t.Fatal(err)
	}
	expectFresh("a recorded breach", nil)
	svc.Kick()
	expectFresh("a kick", nil)
	svc.HandleEvent(ctx, events.Event{Type: events.RestoreTestFailed})
	expectFresh("an event", nil)
}

// TestReportWithoutCacheComputesEveryTime checks that ReportCacheTTL 0 keeps the
// report uncached.
func TestReportWithoutCacheComputesEveryTime(t *testing.T) {
	st := storetest.New(t)
	cs := &countingReports{unedited: unedited{st}}
	svc := readiness.New(readiness.Config{Store: cs, Now: (&clock{now: t0}).Now})
	a, _ := svc.Report(context.Background())
	b, _ := svc.Report(context.Background())
	if a == b || cs.calls.Load() != 2 {
		t.Fatalf("without a TTL: shared %v, %d computations", a == b, cs.calls.Load())
	}
}

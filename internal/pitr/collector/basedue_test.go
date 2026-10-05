package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

func TestBaseDueFollowsEligibility(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.svc.cfg.Bases = fx.repo.ListBaseBackups
	fx.svc.cfg.NextRun = func(string, time.Time) (time.Time, bool) { return fx.clock.Now().Add(24 * time.Hour), true }
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	fx.f.add(t, 5)
	fx.step(w)
	chain := fx.state().ChainID
	cs := fx.chunks(chain)
	due := func() bool {
		t.Helper()
		st, err := fx.repo.GetStream(ctx, fx.stream.ID)
		if err != nil {
			t.Fatal(err)
		}
		status, err := fx.svc.Status(ctx, st.ID)
		if err != nil {
			t.Fatal(err)
		}
		bases, err := fx.repo.ListBaseBackups(ctx, st.ID)
		if err != nil {
			t.Fatal(err)
		}
		return fx.svc.baseDue(st, status, bases, fx.clock.Now())
	}
	save := func(b *models.BackupRecord) {
		t.Helper()
		b.Scope, b.PITRStreamID = models.ScopeInstance, fx.stream.ID
		if err := fx.repo.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}

	if !due() {
		t.Fatal("no base yet: not due")
	}
	// A base taken before the chain started is not eligible for it: still due.
	fx.clock.advance(time.Minute)
	save(&models.BackupRecord{ID: "b_old", Status: models.StatusCompleted, StartedAt: fx.clock.Now(),
		TBefore: &pitr.OpTime{TS: pitr.Timestamp{T: 1}, Term: 1}, TAfter: &pitr.OpTime{TS: pitr.Timestamp{T: 2}, Term: 1}})
	if !due() {
		t.Fatal("the newest base is not eligible for the current chain: not due")
	}
	// A running base: not due.
	fx.clock.advance(time.Minute)
	save(&models.BackupRecord{ID: "b_run", Status: models.StatusInProgress, StartedAt: fx.clock.Now()})
	if due() {
		t.Fatal("due while a base runs")
	}
	// It completed, but the chain has not reached its T_after yet: wait for it.
	save(&models.BackupRecord{ID: "b_run", Status: models.StatusCompleted, StartedAt: fx.clock.Now(),
		TBefore: &pitr.OpTime{TS: cs[1].From, Term: 1}, TAfter: &pitr.OpTime{TS: pitr.Timestamp{T: cs[1].To.T + 30, I: 1}, Term: 1}})
	if due() {
		t.Fatal("due while the chain catches up with the newest base")
	}
	// Covered and eligible: the cron schedule decides (tomorrow).
	fx.f.add(t, 40)
	for range 5 {
		fx.step(w)
	}
	if due() {
		t.Fatal("due with an open window before the cron activation")
	}
}

func TestBusyBaseAfterABreakIsRetriedBySchedule(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	busy := true
	starts := 0
	fx.svc.cfg.Bases = fx.repo.ListBaseBackups
	fx.svc.cfg.NextRun = func(string, time.Time) (time.Time, bool) { return fx.clock.Now().Add(24 * time.Hour), true }
	fx.svc.cfg.StartBase = func(context.Context, string, models.BackupTrigger) (*models.BackupRecord, error) {
		starts++
		if busy {
			return nil, errors.New("a base backup of this connection is already running")
		}
		return &models.BackupRecord{ID: "bkp_new"}, nil
	}
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	fx.f.add(t, 20)
	fx.f.truncate(pitr.Timestamp{T: fx.f.newest().T - 5, I: 1})
	fx.step(w) // the gap: the base after the break is refused (busy)
	if starts != 1 {
		t.Fatalf("%d base starts after the break", starts)
	}
	busy = false
	fx.svc.runBaseSchedule(ctx)
	if starts != 2 {
		t.Fatalf("the schedule did not retry the base of the new chain (%d starts)", starts)
	}
}

func TestBaseDueIgnoresSkippedAndRetriesInterruptedBases(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.svc.cfg.Bases = fx.repo.ListBaseBackups
	fx.svc.cfg.NextRun = func(string, time.Time) (time.Time, bool) { return fx.clock.Now().Add(24 * time.Hour), true }
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	due := func() bool {
		t.Helper()
		st, err := fx.repo.GetStream(ctx, fx.stream.ID)
		if err != nil {
			t.Fatal(err)
		}
		status, err := fx.svc.Status(ctx, st.ID)
		if err != nil {
			t.Fatal(err)
		}
		bases, err := fx.repo.ListBaseBackups(ctx, st.ID)
		if err != nil {
			t.Fatal(err)
		}
		return fx.svc.baseDue(st, status, bases, fx.clock.Now())
	}
	save := func(b *models.BackupRecord) {
		t.Helper()
		b.Scope, b.PITRStreamID = models.ScopeInstance, fx.stream.ID
		if err := fx.repo.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	// A base waiting for a slot of its connection is in progress: not due.
	save(&models.BackupRecord{ID: "b_wait", Status: models.StatusInProgress, StartedAt: fx.clock.Now()})
	if due() {
		t.Fatal("due while a base waits for a slot")
	}
	// Interrupted by a shutdown while it waited: it never ran, so it is due again
	// at once, not after an hour like a failed base.
	save(&models.BackupRecord{ID: "b_wait", Status: models.StatusCancelled, StartedAt: fx.clock.Now(), CancelledBy: runs.SystemActor})
	if !due() {
		t.Fatal("an interrupted base is not retried")
	}
	// A newer skipped record does not hide the state of the bases before it.
	fx.clock.advance(time.Minute)
	save(&models.BackupRecord{ID: "b_wait", Status: models.StatusInProgress, StartedAt: fx.clock.Now().Add(-time.Minute)})
	save(&models.BackupRecord{ID: "b_skip", Status: models.StatusSkipped, StartedAt: fx.clock.Now()})
	if due() {
		t.Fatal("a skipped record hides the running base")
	}
}

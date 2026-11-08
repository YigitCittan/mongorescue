package operations_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// fakeLedger is an in-memory models.SwapLedger that records its calls, with the
// fence of Begin.
type fakeLedger struct {
	mu       sync.Mutex
	markers  map[string]models.SwapMarker
	calls    []string
	beginErr error
	openErr  error
	fence    *stubFence
}

func newFakeLedger(f *stubFence) *fakeLedger {
	return &fakeLedger{markers: map[string]models.SwapMarker{}, fence: f}
}

func (l *fakeLedger) note(c string) { l.calls = append(l.calls, c) }

func (l *fakeLedger) Begin(_ context.Context, m models.SwapMarker) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.note("begin")
	if l.beginErr != nil {
		return l.beginErr
	}
	m.Phase = models.SwapPhaseSwapping
	l.markers[m.RestoreID] = m
	if err := l.fence.Check(); err != nil {
		m.Phase, m.Outcome = models.SwapPhaseFinished, models.SwapAborted
		l.markers[m.RestoreID] = m
		return err
	}
	return nil
}

func (l *fakeLedger) Finish(_ context.Context, id string, outcome models.SwapOutcome, swapped []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.note("finish:" + string(outcome))
	m := l.markers[id]
	m.Phase, m.Outcome, m.Swapped = models.SwapPhaseFinished, outcome, slices.Clone(swapped)
	l.markers[id] = m
	return nil
}

func (l *fakeLedger) Open(context.Context) ([]models.SwapMarker, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.openErr != nil {
		return nil, l.openErr
	}
	var out []models.SwapMarker
	for _, m := range l.markers {
		out = append(out, m)
	}
	return out, nil
}

func (l *fakeLedger) Resolve(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.note("resolve")
	delete(l.markers, id)
	return nil
}

func (l *fakeLedger) order() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

func (l *fakeLedger) marker(id string) (models.SwapMarker, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.markers[id]
	return m, ok
}

func (l *fakeLedger) put(m models.SwapMarker) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.markers[m.RestoreID] = m
}

// stubFence is a models.Fence that can lose the lease.
type stubFence struct {
	mu    sync.Mutex
	err   error
	epoch uint64
}

func (f *stubFence) Check() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *stubFence) Epoch() uint64 { return f.epoch }

func (f *stubFence) lose() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = errors.New("ha: this instance does not hold the HA lease")
}

// loseAfterSwap makes the swap of fakes lose the lease once it has run: the old
// active instance whose swap outlasts its lease.
type loseAfterSwap struct {
	*inPlaceFakes
	fence *stubFence
}

func (s loseAfterSwap) SwapPITRClones(ctx context.Context, req models.RestoreRequest, rec *models.RestoreRecord, swapped func(string)) error {
	err := s.inPlaceFakes.SwapPITRClones(ctx, req, rec, swapped)
	s.fence.lose()
	return err
}

func haEnv(t *testing.T, ledger *fakeLedger, fence *stubFence, mutate ...func(*operations.Config)) *inPlaceEnv {
	t.Helper()
	return newInPlaceEnv(t, false, append([]func(*operations.Config){func(c *operations.Config) {
		c.HAMode, c.SwapLedger, c.Fence = true, ledger, fence
		c.SwapHandoverPoll = 10 * time.Millisecond
	}}, mutate...)...)
}

// An in-place restore in HA mode announces its swap on the HA target before it
// replaces anything, tells it ended before the stream is enabled again, and
// removes the marker once it settled the restore itself.
func TestPITRInPlaceInHAModeAnnouncesTheSwap(t *testing.T) {
	fence := &stubFence{epoch: 4}
	ledger := newFakeLedger(fence)
	env := haEnv(t, ledger, fence)
	rec, err := env.svc.StartRestore(admin(), pitrInPlace("shop"))
	if err != nil {
		t.Fatal(err)
	}
	final := env.wait(t, rec.ID)
	if final.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore %s: %s", final.Status, final.ErrorMessage)
	}
	if got, want := ledger.order(), []string{"begin", "finish:completed", "resolve"}; !slices.Equal(got, want) {
		t.Fatalf("ledger calls = %v, want %v", got, want)
	}
	if got := env.fakes.order(); !slices.Equal(got, []string{"restore", "post_restore_on_clones", "snapshot", "pause", "swap", "drop_clones", "resume"}) {
		t.Fatalf("order = %v", got)
	}
	if _, left := ledger.marker(rec.ID); left {
		t.Fatal("the marker was not removed")
	}
}

// The marker carries what another instance needs to settle the swap, under the
// epoch of the instance that wrote it.
func TestPITRInPlaceMarkerContent(t *testing.T) {
	fence := &stubFence{epoch: 4}
	ledger := newFakeLedger(fence)
	started := make(chan struct{})
	gate := make(chan struct{})
	env := haEnv(t, ledger, fence)
	env.fakes.swapStarted, env.fakes.swapGate = started, gate
	rec, err := env.svc.StartRestore(admin(), pitrInPlace("shop", "crm"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	m, ok := ledger.marker(rec.ID)
	close(gate)
	if !ok || m.Phase != models.SwapPhaseSwapping || m.Epoch != 4 || m.StreamID != "str_a" || m.ConnectionID != "conn_a" ||
		!slices.Equal(m.Databases, []string{"shop", "crm"}) || len(m.Clones) != 2 || !m.StreamWasEnabled || len(m.SafetyBackupIDs) != 2 {
		t.Fatalf("marker during the swap = %+v (%v)", m, ok)
	}
	env.wait(t, rec.ID)
}

// A swap the ledger refuses never starts: the originals stay untouched and the
// stream is enabled again.
func TestPITRInPlaceInHAModeLedgerRefusalLeavesTheOriginals(t *testing.T) {
	fence := &stubFence{epoch: 4}
	ledger := newFakeLedger(fence)
	ledger.beginErr = errors.New("storage down")
	env := haEnv(t, ledger, fence)
	rec, err := env.svc.StartRestore(admin(), pitrInPlace("shop"))
	if err != nil {
		t.Fatal(err)
	}
	final := env.wait(t, rec.ID)
	if final.Status != models.RestoreStatusFailed || !strings.Contains(final.ErrorMessage, "the original databases are untouched") ||
		!strings.Contains(final.ErrorMessage, "the swap was not started") {
		t.Fatalf("record = %s: %s", final.Status, final.ErrorMessage)
	}
	if slices.Contains(env.fakes.order(), "swap") || !slices.Contains(env.fakes.order(), "resume") {
		t.Fatalf("order = %v; want no swap and a resumed stream", env.fakes.order())
	}
}

// A lease lost right after the marker was written aborts the swap, and a fenced
// instance neither enables the stream nor removes the marker: the instance that
// holds the lease settles the restore from it.
func TestPITRInPlaceInHAModeFencedAfterBegin(t *testing.T) {
	fence := &stubFence{epoch: 4}
	ledger := newFakeLedger(fence)
	env := haEnv(t, ledger, fence, func(c *operations.Config) {
		c.PITRStreams = losePause{inPlaceFakes: c.PITRStreams.(*inPlaceFakes), fence: fence}
	})
	rec, err := env.svc.StartRestore(admin(), pitrInPlace("shop"))
	if err != nil {
		t.Fatal(err)
	}
	final := env.wait(t, rec.ID)
	if final.Status != models.RestoreStatusFailed || !strings.Contains(final.ErrorMessage, "the swap was not started") ||
		!strings.Contains(final.ErrorMessage, "the original databases are untouched") {
		t.Fatalf("record = %s: %s", final.Status, final.ErrorMessage)
	}
	if got := env.fakes.order(); slices.Contains(got, "swap") || slices.Contains(got, "resume") {
		t.Fatalf("a fenced instance swapped or resumed the stream: %v", got)
	}
	m, ok := ledger.marker(rec.ID)
	if !ok || !m.Finished() || m.Outcome != models.SwapAborted {
		t.Fatalf("marker = %+v (%v); want it left, aborted, for the instance that holds the lease", m, ok)
	}
	if got := ledger.order(); !slices.Equal(got, []string{"begin"}) {
		t.Fatalf("ledger calls = %v", got)
	}
}

// losePause makes the stream pause lose the lease: the lease is gone by the time
// the swap is announced.
type losePause struct {
	*inPlaceFakes
	fence *stubFence
}

func (p losePause) PauseStream(ctx context.Context, id string) (bool, error) {
	was, err := p.inPlaceFakes.PauseStream(ctx, id)
	p.fence.lose()
	return was, err
}

// An instance that loses the lease while it swaps finishes the swap and says so on
// the HA target, but leaves the stream and the marker to the instance that holds
// the lease now.
func TestPITRInPlaceInHAModeLeaseLostDuringTheSwap(t *testing.T) {
	fence := &stubFence{epoch: 4}
	ledger := newFakeLedger(fence)
	// The swap loses the lease once it ran.
	env := haEnv(t, ledger, fence, func(c *operations.Config) {
		c.PITRSwap = loseAfterSwap{inPlaceFakes: c.PITRSwap.(*inPlaceFakes), fence: fence}
	})
	rec, err := env.svc.StartRestore(admin(), pitrInPlace("shop"))
	if err != nil {
		t.Fatal(err)
	}
	env.wait(t, rec.ID)
	if got, want := ledger.order(), []string{"begin", "finish:completed"}; !slices.Equal(got, want) {
		t.Fatalf("ledger calls = %v, want %v (no resolve once fenced)", got, want)
	}
	if slices.Contains(env.fakes.order(), "resume") {
		t.Fatalf("a fenced instance enabled the stream: %v", env.fakes.order())
	}
	if m, ok := ledger.marker(rec.ID); !ok || m.Outcome != models.SwapCompleted {
		t.Fatalf("marker = %+v (%v); want it finished and left for the new active instance", m, ok)
	}
}

// A swap whose stream pause failed was never announced.
func TestPITRInPlaceInHAModeNoMarkerBeforeTheSwap(t *testing.T) {
	fence := &stubFence{epoch: 4}
	ledger := newFakeLedger(fence)
	env := haEnv(t, ledger, fence)
	env.fakes.snapshotErr = errors.New("clone lists nothing")
	rec, err := env.svc.StartRestore(admin(), pitrInPlace("shop"))
	if err != nil {
		t.Fatal(err)
	}
	env.wait(t, rec.ID)
	if got := ledger.order(); len(got) != 0 {
		t.Fatalf("ledger calls before the swap = %v", got)
	}
}

// swapMarkerOf is the marker of the swap of interruptedInPlace, written by an
// instance of an older epoch.
func swapMarkerOf(phase models.SwapPhase) models.SwapMarker {
	const suffix = "_rescue_20261005_120000_ab12"
	return models.SwapMarker{RestoreID: "rst_pitr_crash", StreamID: "str_a", ConnectionID: "conn_a", Databases: []string{"shop"},
		Clones: []string{"shop" + suffix}, StreamWasEnabled: true, Epoch: 3, Phase: phase, SafetyBackupIDs: []string{"bkp_safety"}}
}

func saveInterrupted(t *testing.T, env *inPlaceEnv, stage string) {
	t.Helper()
	if err := env.st.SaveRestoreRecord(context.Background(), interruptedInPlace(stage, stage != models.PITRInPlaceRestoring)); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The new active instance leaves a swap of an older epoch alone: its record stays
// as the snapshot had it, its clones, databases and stream are not touched and its
// keys are reserved, until the process that swapped reports its outcome; only
// then is the restore settled and the stream enabled again.
func TestAdoptSwapsLeavesAnInFlightSwapAloneUntilItIsResolved(t *testing.T) {
	fence := &stubFence{epoch: 4}
	ledger := newFakeLedger(fence)
	ledger.put(swapMarkerOf(models.SwapPhaseSwapping))
	env := haEnv(t, ledger, fence, func(c *operations.Config) { c.SwapHandoverTimeout = time.Hour })
	saveInterrupted(t, env, models.PITRInPlaceSafetyBackup)

	adopted, err := env.svc.AdoptSwaps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !adopted["rst_pitr_crash"] || env.svc.HeldSwaps() != 1 {
		t.Fatalf("adopted = %v, held = %d", adopted, env.svc.HeldSwaps())
	}
	// The stream is paused so the collector does not start; nothing resumed it and
	// no clone was dropped.
	if got := env.fakes.order(); !slices.Equal(got, []string{"pause"}) {
		t.Fatalf("calls = %v; want only the pause", got)
	}
	// Its databases, its connection and its stream's base backup are reserved.
	for _, key := range []string{runs.BackupKey("conn_a", "shop"), runs.RestoreKey("conn_a", "shop"), runs.PITRBaseKey("conn_a"), runs.InstanceKey("conn_a")} {
		if release, acquireErr := env.manager.Acquire(key); !errors.Is(acquireErr, runs.ErrBusy) {
			if release != nil {
				release()
			}
			t.Fatalf("Acquire(%s) = %v; want ErrBusy while the swap is held", key, acquireErr)
		}
	}
	// While the process reports nothing the record stays in progress.
	time.Sleep(100 * time.Millisecond)
	rec, err := env.st.GetRestoreRecord(context.Background(), "rst_pitr_crash")
	if err != nil || rec.Status != models.RestoreStatusInProgress {
		t.Fatalf("record = %+v, %v; want in progress", rec, err)
	}
	if slices.Contains(env.fakes.order(), "resume") || slices.Contains(env.fakes.order(), "drop_clones") {
		t.Fatalf("a held swap was touched: %v", env.fakes.order())
	}

	// The swapping process reports it finished: the restore is settled.
	if err = ledger.Finish(context.Background(), "rst_pitr_crash", models.SwapCompleted, []string{"shop"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the restore to be settled", func() bool {
		r, getErr := env.st.GetRestoreRecord(context.Background(), "rst_pitr_crash")
		return getErr == nil && r.Status == models.RestoreStatusCompleted
	})
	waitFor(t, "the marker to be resolved", func() bool { _, ok := ledger.marker("rst_pitr_crash"); return !ok })
	waitFor(t, "the stream to be enabled again", func() bool { return slices.Contains(env.fakes.order(), "resume") })
	waitFor(t, "the keys to be released", func() bool {
		release, acquireErr := env.manager.Acquire(runs.BackupKey("conn_a", "shop"))
		if acquireErr == nil {
			release()
		}
		return acquireErr == nil
	})
	if env.svc.HeldSwaps() != 0 || env.svc.SwapHandoverCounts()[operations.SwapHandoverCompleted] != 1 {
		t.Fatalf("held = %d, counts = %v", env.svc.HeldSwaps(), env.svc.SwapHandoverCounts())
	}
	final, _ := env.st.GetRestoreRecord(context.Background(), "rst_pitr_crash")
	if final.PITR.InPlace.Stage != models.PITRInPlaceDone || !strings.Contains(final.Warning, "settled by the instance that took over") {
		t.Fatalf("settled record = %+v (%s)", final.PITR.InPlace, final.Warning)
	}
	if ev := env.inPlaceEvent(); ev == nil || ev.Status != events.InPlacePITRSucceeded {
		t.Fatalf("restore.in_place_pitr = %+v", ev)
	}
}

// A swap whose process never reports is marked interrupted after the timeout, with
// an alert, and never resumed: the clones and the safety backup are kept and
// named, the databases are not touched, and the stream is enabled again.
func TestAdoptSwapsTimesOutAnUnresolvedSwap(t *testing.T) {
	fence := &stubFence{epoch: 4}
	ledger := newFakeLedger(fence)
	ledger.put(swapMarkerOf(models.SwapPhaseSwapping))
	env := haEnv(t, ledger, fence, func(c *operations.Config) { c.SwapHandoverTimeout = 150 * time.Millisecond })
	saveInterrupted(t, env, models.PITRInPlaceSwapping)
	if _, err := env.svc.AdoptSwaps(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the swap to time out", func() bool {
		r, err := env.st.GetRestoreRecord(context.Background(), "rst_pitr_crash")
		return err == nil && r.Status == models.RestoreStatusFailed
	})
	final, _ := env.st.GetRestoreRecord(context.Background(), "rst_pitr_crash")
	for _, want := range []string{"interrupted", "shop_rescue_20261005_120000_ab12", "bkp_safety", "may still be writing", "epoch 3"} {
		if !strings.Contains(final.ErrorMessage, want) {
			t.Fatalf("the message lacks %q: %s", want, final.ErrorMessage)
		}
	}
	if slices.Contains(env.fakes.order(), "drop_clones") || slices.Contains(env.fakes.order(), "swap") {
		t.Fatalf("a timed-out swap was touched: %v", env.fakes.order())
	}
	waitFor(t, "the stream to be enabled again", func() bool { return slices.Contains(env.fakes.order(), "resume") })
	waitFor(t, "the marker to be resolved", func() bool { _, ok := ledger.marker("rst_pitr_crash"); return !ok })
	if env.svc.SwapHandoverCounts()[operations.SwapHandoverTimeout] != 1 {
		t.Fatalf("counts = %v", env.svc.SwapHandoverCounts())
	}
	if ev := env.inPlaceEvent(); ev == nil || ev.Status != events.InPlacePITRFailed {
		t.Fatalf("restore.in_place_pitr = %+v", ev)
	}
}

// A new active instance that lost the lease meanwhile settles nothing, however
// long it waited.
func TestAdoptSwapsFencedInstanceSettlesNothing(t *testing.T) {
	fence := &stubFence{epoch: 4}
	ledger := newFakeLedger(fence)
	ledger.put(swapMarkerOf(models.SwapPhaseSwapping))
	env := haEnv(t, ledger, fence, func(c *operations.Config) { c.SwapHandoverTimeout = 20 * time.Millisecond })
	saveInterrupted(t, env, models.PITRInPlaceSwapping)
	if _, err := env.svc.AdoptSwaps(context.Background()); err != nil {
		t.Fatal(err)
	}
	fence.lose()
	time.Sleep(200 * time.Millisecond)
	rec, _ := env.st.GetRestoreRecord(context.Background(), "rst_pitr_crash")
	if rec.Status != models.RestoreStatusInProgress || env.svc.HeldSwaps() != 1 {
		t.Fatalf("a fenced instance settled: %s, held %d", rec.Status, env.svc.HeldSwaps())
	}
}

// The takeover is abandoned (so the startup sweep never runs blind) when the
// ledger cannot be read.
func TestAdoptSwapsFailsWhenTheLedgerCannotBeRead(t *testing.T) {
	fence := &stubFence{epoch: 4}
	ledger := newFakeLedger(fence)
	ledger.openErr = errors.New("storage down")
	env := haEnv(t, ledger, fence)
	if _, err := env.svc.AdoptSwaps(context.Background()); err == nil {
		t.Fatal("AdoptSwaps succeeded with an unreadable ledger")
	}
	// Without a ledger there is nothing to adopt.
	plain := newInPlaceEnv(t, false)
	if adopted, err := plain.svc.AdoptSwaps(context.Background()); err != nil || adopted != nil {
		t.Fatalf("AdoptSwaps without a ledger = %v, %v", adopted, err)
	}
}

// A swap that finished (or was aborted) before the takeover is settled at once;
// an aborted swap's clones are dropped, since the originals are untouched.
func TestAdoptSwapsSettlesFinishedMarkersAtOnce(t *testing.T) {
	for _, c := range []struct {
		outcome    models.SwapOutcome
		status     models.RestoreStatus
		wantDrop   bool
		wantInText string
	}{
		{models.SwapCompleted, models.RestoreStatusCompleted, false, ""},
		{models.SwapFailed, models.RestoreStatusFailed, false, "the swap stopped on the instance that ran it"},
		{models.SwapAborted, models.RestoreStatusFailed, true, "lost the HA lease before it replaced a database"},
	} {
		fence := &stubFence{epoch: 4}
		ledger := newFakeLedger(fence)
		m := swapMarkerOf(models.SwapPhaseFinished)
		m.Outcome = c.outcome
		if c.outcome != models.SwapAborted {
			m.Swapped = []string{"shop"}
		}
		ledger.put(m)
		env := haEnv(t, ledger, fence)
		saveInterrupted(t, env, models.PITRInPlacePausing)
		adopted, err := env.svc.AdoptSwaps(context.Background())
		if err != nil || !adopted["rst_pitr_crash"] || env.svc.HeldSwaps() != 0 {
			t.Fatalf("%s: adopted %v, %v, held %d", c.outcome, adopted, err, env.svc.HeldSwaps())
		}
		final, _ := env.st.GetRestoreRecord(context.Background(), "rst_pitr_crash")
		if final.Status != c.status || !strings.Contains(final.ErrorMessage+final.Warning, c.wantInText) {
			t.Fatalf("%s: record %s: %s / %s", c.outcome, final.Status, final.ErrorMessage, final.Warning)
		}
		if slices.Contains(env.fakes.order(), "drop_clones") != c.wantDrop {
			t.Fatalf("%s: clone handling %v", c.outcome, env.fakes.order())
		}
		if _, left := ledger.marker("rst_pitr_crash"); left {
			t.Fatalf("%s: the marker was not removed", c.outcome)
		}
	}
}

// A swap whose restore record the metadata snapshot lacks (it predates the
// restore) is still held and settled: the events and the stream carry it.
func TestAdoptSwapsWithoutARecord(t *testing.T) {
	fence := &stubFence{epoch: 4}
	ledger := newFakeLedger(fence)
	ledger.put(swapMarkerOf(models.SwapPhaseFinished))
	m, _ := ledger.marker("rst_pitr_crash")
	m.Outcome, m.Swapped = models.SwapCompleted, []string{"shop"}
	ledger.put(m)
	env := haEnv(t, ledger, fence)
	if _, err := env.svc.AdoptSwaps(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ev := env.inPlaceEvent(); ev == nil || ev.RestoreID != "rst_pitr_crash" || ev.Status != events.InPlacePITRSucceeded {
		t.Fatalf("restore.in_place_pitr = %+v", ev)
	}
	waitFor(t, "the stream to be enabled again", func() bool { return slices.Contains(env.fakes.order(), "resume") })
	if _, left := ledger.marker("rst_pitr_crash"); left {
		t.Fatal("the marker was not removed")
	}
}

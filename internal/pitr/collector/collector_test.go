package collector

import (
	"context"
	"errors"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// checkContinuity asserts that the committed chunks of chain follow each other
// and returns the entries they hold, in order.
func checkContinuity(fx *fixture, chain string) []pitr.Timestamp {
	fx.t.Helper()
	var all []pitr.Timestamp
	cs := fx.chunks(chain)
	for i, c := range cs {
		if i > 0 && c.From != cs[i-1].To {
			fx.t.Fatalf("chunk %d starts at %s, the previous one ends at %s", i, c.From, cs[i-1].To)
		}
		got := fx.entries(c)
		if int64(len(got)) != c.Entries {
			fx.t.Fatalf("chunk %s holds %d entries, recorded %d", c.ID, len(got), c.Entries)
		}
		all = append(all, got...)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Compare(all[i-1]) <= 0 {
			fx.t.Fatalf("entry %s follows %s: duplicate or out of order", all[i], all[i-1])
		}
	}
	return all
}

var keyPattern = regexp.MustCompile(`^_mongorescue/oplog/conn_a/rs0/ch_[0-9TZ]+_[0-9a-f]{16}/\d{10}\.\d{10}-\d{10}\.\d{10}\.bson\.gz\.age$`)

func TestSteadyCollection(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w) // starts the chain at the newest majority entry
	st := fx.state()
	if st.Last.TS != fx.f.newest() {
		t.Fatalf("the chain starts at %s, want %s", st.Last.TS, fx.f.newest())
	}
	start := st.Last.TS
	if d := fx.step(w); d != time.Minute {
		t.Fatalf("steady delay %s", d)
	}
	fx.f.add(t, 5)
	fx.step(w)
	chain := fx.state().ChainID
	cs := fx.chunks(chain)
	if len(cs) != 2 {
		t.Fatalf("%d chunks", len(cs))
	}
	got := checkContinuity(fx, chain)
	if len(got) != 6 || got[0] != start || got[5] != fx.f.newest() {
		t.Fatalf("entries %v (start %s)", got, start)
	}
	for _, c := range cs {
		if !keyPattern.MatchString(c.StorageKey) || !c.Encrypted || c.SHA256 == "" || c.Status != pitr.ChunkCommitted {
			t.Fatalf("chunk %+v", c)
		}
	}
	if fx.state().Last.TS != fx.f.newest() {
		t.Fatal("the state does not follow the last chunk")
	}
}

func TestCatchUpCapsChunksAtOneInterval(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	fx.f.add(t, 300) // five minutes behind
	steps := 0
	for {
		d := fx.step(w)
		steps++
		if d != 0 {
			break
		}
		if steps > 20 {
			t.Fatal("catch-up does not end")
		}
	}
	chain := fx.state().ChainID
	cs := fx.chunks(chain)
	if len(cs) < 6 {
		t.Fatalf("%d chunks for five minutes at a one-minute interval", len(cs))
	}
	for _, c := range cs[1:] {
		if span := c.To.T - c.From.T; span > 61 {
			t.Fatalf("chunk %s-%s spans %ds", c.From, c.To, span)
		}
	}
	if got := checkContinuity(fx, chain); len(got) != 301 {
		t.Fatalf("%d entries after catch-up, want 301", len(got))
	}
}

func TestGapEndsTheChainAndTakesABase(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	old := fx.state()
	fx.f.add(t, 20)
	fx.f.truncate(pitr.Timestamp{T: fx.f.newest().T - 5, I: 1}) // the window was overrun
	fx.step(w)

	chains := fx.chains()
	if len(chains) != 2 || chains[0].EndReason != pitr.EndGap || chains[0].End != old.Last.TS || !chains[1].Open() {
		t.Fatalf("chains %+v", chains)
	}
	st := fx.state()
	if st.ChainID != chains[1].ChainID || st.Last.TS != (pitr.Timestamp{T: fx.f.newest().T - 5, I: 1}) {
		t.Fatalf("new chain state %+v", st)
	}
	if fx.events.count(events.PITRChainBroken) != 1 || len(fx.bases) != 1 {
		t.Fatalf("events %d, bases %v", fx.events.count(events.PITRChainBroken), fx.bases)
	}
	fx.step(w) // the first chunk of the new chain keeps its start entry
	if got := checkContinuity(fx, st.ChainID); len(got) != 6 || got[0] != st.Last.TS {
		t.Fatalf("new chain entries %v", got)
	}
}

func TestReplicaSetChangeEndsTheChain(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	fx.f.rsID = "id2"
	fx.f.add(t, 1)
	fx.step(w)
	ended := 0
	for _, c := range fx.chains() {
		if c.EndReason == pitr.EndReplicaSetChanged {
			ended++
		}
	}
	if len(fx.chains()) != 2 || ended != 1 {
		t.Fatalf("%d chains, %d ended by the replica set change", len(fx.chains()), ended)
	}
}

func TestDivergenceSupersedesTheLaterChunks(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	fx.f.add(t, 3)
	fx.step(w)
	good := fx.state().Last.TS
	fx.f.add(t, 3)
	fx.step(w)
	chain := fx.state().ChainID
	if n := len(fx.chunks(chain)); n != 3 {
		t.Fatalf("%d chunks", n)
	}
	// A forced reconfiguration rolled back the last chunk's entries.
	fx.f.rewriteFrom(t, pitr.Timestamp{T: good.T + 1, I: 1}, 4)

	restarted := fx.worker()
	fx.step(restarted)
	chains := fx.chains()
	if len(chains) != 2 || chains[0].EndReason != pitr.EndDiverged || chains[0].End != good {
		t.Fatalf("chains %+v (good point %s)", chains, good)
	}
	cs := fx.chunks(chain)
	if cs[2].Status != pitr.ChunkSuperseded || cs[1].Status != pitr.ChunkCommitted {
		t.Fatalf("statuses %s %s", cs[1].Status, cs[2].Status)
	}
	if fx.events.count(events.PITRDiverged) != 1 {
		t.Fatal("no pitr.diverged event")
	}
	fx.step(restarted)
	if got := checkContinuity(fx, fx.state().ChainID); len(got) != 5 || got[0] != good {
		t.Fatalf("new chain entries %v", got)
	}
}

func TestIntervalHalvesAboveTheSizeLimit(t *testing.T) {
	fx := newFixture(t)
	fx.svc.cfg.MaxChunkBytes = 200
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	fx.f.add(t, 20)
	if d := fx.step(w); d != 30*time.Second || w.interval != 30*time.Second {
		t.Fatalf("delay %s, interval %s", d, w.interval)
	}
	fx.svc.cfg.MaxChunkBytes = 1 << 20
	fx.f.add(t, 1)
	fx.step(w)
	if w.interval != time.Minute {
		t.Fatalf("interval %s after a small chunk", w.interval)
	}
}

func TestCrashInTheMiddleOfAChunkLeavesNothing(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	before := fx.state()
	objects := fx.objects()
	fx.f.add(t, 10)
	fx.f.mu.Lock()
	fx.f.failAfter, fx.f.failErr = 4, context.Canceled
	fx.f.mu.Unlock()
	if _, err := w.step(context.Background()); err == nil {
		t.Fatal("the interrupted chunk succeeded")
	}
	if fx.objects() != objects || fx.state().Last != before.Last {
		t.Fatalf("an interrupted chunk left %d objects (had %d), state %+v", fx.objects(), objects, fx.state().Last)
	}
	restarted := fx.worker()
	fx.step(restarted)
	if got := checkContinuity(fx, before.ChainID); len(got) != 11 {
		t.Fatalf("%d entries after the restart, want 11", len(got))
	}
}

func TestEmptyRangeWritesOneEmptyChunk(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w) // the first chunk [M, M] keeps its start entry
	fx.step(w) // nothing new: its key is taken, so no empty chunk at M
	chain := fx.state().ChainID
	if n := len(fx.chunks(chain)); n != 1 {
		t.Fatalf("%d chunks after the first idle step", n)
	}
	fx.f.add(t, 2)
	fx.step(w)
	fx.step(w) // nothing new: an empty chunk
	cs := fx.chunks(chain)
	if len(cs) != 3 || cs[2].Entries != 0 || cs[2].From != cs[2].To || cs[2].FirstTerm != 1 {
		t.Fatalf("chunks %+v", cs)
	}
	fx.step(w)
	fx.step(fx.worker()) // also not again after a restart
	if n := len(fx.chunks(chain)); n != 3 {
		t.Fatalf("%d chunks after idle steps", n)
	}
	fx.f.add(t, 2)
	fx.step(w)
	checkContinuity(fx, chain)
}

func TestLagAlertsArePersisted(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	// The majority view advances by ten minutes while the collector reads nothing
	// (the member lags: every range read ends early).
	fx.f.add(t, 600)
	fx.f.majority = fx.f.newest()
	w.observeLag(context.Background(), fx.state(), mustWindow(t, fx), fx.svc.now())
	if fx.events.count(events.PITRLagHigh) != 1 || fx.state().LagSince == nil {
		t.Fatalf("lag_high %d, lag_since %v", fx.events.count(events.PITRLagHigh), fx.state().LagSince)
	}
	restarted := fx.worker()
	restarted.lagSince = fx.state().LagSince
	restarted.observeLag(context.Background(), fx.state(), mustWindow(t, fx), fx.svc.now())
	if fx.events.count(events.PITRLagHigh) != 1 {
		t.Fatal("lag_high raised again after a restart")
	}
	for range 20 {
		if d := fx.step(restarted); d != 0 {
			break
		}
	}
	if fx.events.count(events.PITRLagRecovered) != 1 || fx.state().LagSince != nil {
		t.Fatalf("lag_recovered %d, lag_since %v", fx.events.count(events.PITRLagRecovered), fx.state().LagSince)
	}
	// The fake oplog holds minutes, not hours: the headroom is low.
	if fx.events.count(events.PITRWindowLow) == 0 {
		t.Fatalf("window_low %d", fx.events.count(events.PITRWindowLow))
	}
}

func mustWindow(t *testing.T, fx *fixture) pitr.OplogWindow {
	t.Helper()
	w, err := member{fx.f}.OplogWindow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestCollectorFailedAndRecoveredOncePerEpisode(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	w.sess = nil
	fx.openErr = errors.New("connection refused")
	ctx := context.Background()
	w.tick(ctx)
	w.tick(ctx)
	if fx.events.count(events.PITRCollectorFailed) != 1 || fx.state().Status != pitr.CollectorFailed {
		t.Fatalf("collector_failed %d, status %s", fx.events.count(events.PITRCollectorFailed), fx.state().Status)
	}
	fx.mu.Lock()
	fx.openErr = nil
	fx.mu.Unlock()
	w.tick(ctx)
	w.tick(ctx)
	if fx.events.count(events.PITRCollectorRecovered) != 1 || fx.state().Status != pitr.CollectorRunning {
		t.Fatalf("collector_recovered %d, status %s", fx.events.count(events.PITRCollectorRecovered), fx.state().Status)
	}
}

func TestEncryptionIsRequired(t *testing.T) {
	fx := newFixture(t)
	fx.svc.cfg.Encryptor = func() *encryption.Encryptor { return nil }
	w := fx.worker()
	fx.step(w)
	if _, err := w.step(context.Background()); !errors.Is(err, ErrEncryptionRequired) {
		t.Fatalf("step without encryption: %v", err)
	}
	if fx.objects() != 0 {
		t.Fatal("a chunk was written without encryption")
	}
}

func TestServiceRunsOnlyEnabledStreams(t *testing.T) {
	fx := newFixture(t)
	fx.svc.cfg.Clock = realClock{}
	fx.svc.cfg.ReconcileInterval = 10 * time.Millisecond
	baseline := runtime.NumGoroutine()
	fx.svc.Start(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for !fx.svc.Running(fx.stream.ID) || len(fx.chunks(fx.stateChain())) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the collector of the enabled stream did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, err := fx.repo.GetStream(context.Background(), fx.stream.ID)
	if err != nil {
		t.Fatal(err)
	}
	st.Enabled = false
	if err := fx.repo.UpdateStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	fx.svc.Reload()
	for fx.svc.Running(fx.stream.ID) {
		if time.Now().After(deadline) {
			t.Fatal("the collector of a disabled stream keeps running")
		}
		time.Sleep(10 * time.Millisecond)
	}
	fx.svc.Stop()
	fx.svc.Stop()
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline {
		t.Fatalf("%d goroutines after Stop, %d before Start", n, baseline)
	}
}

// stateChain returns the current chain, or "" before the first one.
func (fx *fixture) stateChain() string {
	st, err := fx.repo.LoadState(context.Background(), fx.stream.ID)
	if err != nil {
		return "none"
	}
	return st.ChainID
}

func TestReplicaSetIDChangeAcrossARestartIsAGap(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	if id := fx.state().ReplicaSetID; id != "id1" {
		t.Fatalf("stored replica set ID %q, want id1", id)
	}
	// The set is re-initiated under the same name while the collector is stopped.
	fx.f.rsID = "id2"
	fx.f.add(t, 2)
	restarted := fx.worker()
	fx.step(restarted)
	var ended *pitr.Chain
	for _, c := range fx.chains() {
		if !c.Open() {
			ended = c
		}
	}
	if ended == nil || ended.EndReason != pitr.EndReplicaSetChanged || len(fx.chains()) != 2 {
		t.Fatalf("chains after the restart %+v", fx.chains())
	}
	if fx.events.count(events.PITRChainBroken) != 1 || len(fx.bases) != 1 || fx.state().ReplicaSetID != "id2" {
		t.Fatalf("chain_broken %d, bases %v, stored ID %q", fx.events.count(events.PITRChainBroken), fx.bases, fx.state().ReplicaSetID)
	}
	// The new ID is the reference from now on: another restart breaks nothing.
	fx.step(fx.worker())
	if len(fx.chains()) != 2 {
		t.Fatalf("%d chains after a second restart", len(fx.chains()))
	}
}

func TestWindowLowIsNotRaisedAgainAfterARestart(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w) // the fake oplog holds seconds, far below six hours of headroom
	if fx.events.count(events.PITRWindowLow) != 1 || fx.state().WindowLowSince == nil {
		t.Fatalf("window_low %d, window_low_since %v", fx.events.count(events.PITRWindowLow), fx.state().WindowLowSince)
	}
	restarted := fx.worker()
	fx.step(restarted)
	fx.step(restarted)
	if n := fx.events.count(events.PITRWindowLow); n != 1 {
		t.Fatalf("window_low raised %d times across a restart", n)
	}
	// Plenty of headroom again clears the stored episode.
	st := fx.state()
	st.Last.TS = pitr.Timestamp{T: 100_000, I: 1} // a day ahead of the oldest entry
	restarted.observeLag(context.Background(), st, pitr.OplogWindow{Oldest: pitr.Timestamp{T: 1}, Newest: st.Last.TS,
		MajorityOpTime: pitr.OpTime{TS: st.Last.TS}}, fx.svc.now())
	if fx.state().WindowLowSince != nil {
		t.Fatal("window_low_since stays after the headroom recovered")
	}
}

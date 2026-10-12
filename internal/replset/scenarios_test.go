//go:build replset3

package replset

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// assertOneChain checks that the stream still has the one open chain it started
// with, that no chunk was superseded, that the chunks run from the chain's start
// without a hole, an overlap or a duplicate entry, and that they hold exactly the
// oplog of the current primary: nothing lost, nothing added. No chain break and
// no divergence was raised. It returns the stored entries.
func (r *rig) assertOneChain(t *testing.T) []oplogEntry {
	t.Helper()
	last := r.waitCovered(t)
	chains := r.chains(t)
	if len(chains) != 1 || !chains[0].Open() {
		t.Fatalf("%d chains (first %+v): the chain broke", len(chains), chains[0])
	}
	chain := chains[0]
	chunks := r.chunks(t, chain.ChainID, true)
	if len(chunks) == 0 || chunks[0].From != chain.Start {
		t.Fatalf("the chain starts at %s, its first of %d chunks elsewhere", chain.Start, len(chunks))
	}
	for _, c := range chunks {
		if c.Status != pitr.ChunkCommitted {
			t.Fatalf("chunk %s is %s", c.ID, c.Status)
		}
	}
	entries := r.chainEntries(t, chain, false)
	primary := r.c.primary(t)
	assertSameHistory(t, "chain against the primary "+primary, entries, r.c.direct(t, primary), chain.Start, last)
	for _, typ := range []events.EventType{events.PITRChainBroken, events.PITRDiverged} {
		if n := r.events.count(typ); n != 0 {
			t.Fatalf("%d %s event(s)", n, typ)
		}
	}
	return entries
}

// TestFailoverDuringCollection steps the primary down and then kills the new one
// while a writer inserts with majority write concern and the collector reads
// one-second chunks: the chain stays one unbroken run that holds exactly the
// oplog of the final primary, and every acknowledged insert exactly once.
func TestFailoverDuringCollection(t *testing.T) {
	c := requireCluster(t)
	r := newRig(t, c, rigOptions{})
	db := r.uniqueDB(t, "failover")
	w := startWriter(t, c, db, 0, 10*time.Millisecond, 256)
	time.Sleep(3 * time.Second)

	// A clean failover: the primary steps down.
	_, current := c.stepDown(t)
	time.Sleep(3 * time.Second)
	// A hard one: the new primary dies.
	c.kill(t, current)
	next := c.newPrimary(t, current)
	t.Logf("%s is primary", next)
	time.Sleep(3 * time.Second)
	c.start(t, current)
	c.waitHealthy(t)
	time.Sleep(2 * time.Second)
	w.halt()
	if w.ackedCount() == 0 {
		t.Fatal("no insert was acknowledged")
	}

	entries := r.assertOneChain(t)
	w.check(t, db+".events", entries)
	t.Logf("%d entries in the chain, %d inserts acknowledged, %d failed", len(entries), w.ackedCount(), w.errors)
}

// TestFailoverDuringRestore kills the primary while a point-in-time restore runs
// on the replica set, once while it restores the base and once while it replays
// the oplog. The restore either completes with clones that match the target
// state or fails, never leaving an unrecorded clone; a retry after the failover
// restores the target state; and the collector's chain survives.
func TestFailoverDuringRestore(t *testing.T) {
	c := requireCluster(t)
	r := newRig(t, c, rigOptions{})
	db := r.uniqueDB(t, "restorefo")
	ctx := r.ctx
	coll := c.client.Database(db).Collection("docs")
	// About 20 MiB of base and 20,000 oplog entries: the restore takes long enough
	// for a failover in either pass (the replay applies one entry at a time with
	// majority write concern, a few hundred entries per second on three members).
	pad := strings.Repeat("d", 1000)
	const docs = 20_000
	batch := make([]any, 0, 1000)
	for i := range docs {
		batch = append(batch, bson.D{{Key: "_id", Value: int64(i)}, {Key: "v", Value: int64(0)}, {Key: "pad", Value: pad}})
		if len(batch) == cap(batch) {
			if _, err := coll.InsertMany(ctx, batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	r.base(t)
	// Oplog to replay: every document updated once, one entry each.
	if _, err := coll.UpdateMany(ctx, bson.D{}, bson.D{{Key: "$inc", Value: bson.D{{Key: "v", Value: int64(1)}}}}); err != nil {
		t.Fatal(err)
	}
	want := state(t, c.client, db)
	at := r.nextSecond(t)
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: int64(-1)}, {Key: "after", Value: true}}); err != nil {
		t.Fatal(err)
	}
	r.waitCovered(t)

	target := time.Unix(int64(at), 0).UTC()
	req := models.RestoreRequest{PITR: &models.PITRTarget{StreamID: streamID, At: &target}, Databases: []string{db}}
	for _, pass := range []struct {
		name string
		// writing reports that the pass writes into the clone of docs.
		writing bson.D
	}{
		{"base", bson.D{}},
		{"replay", bson.D{{Key: "v", Value: bson.D{{Key: "$gte", Value: int64(1)}}}}},
	} {
		t.Run(pass.name, func(t *testing.T) {
			r.restoreThroughFailover(t, req, db, want, pass.writing)
		})
	}
	r.assertOneChain(t)
}

// restoreThroughFailover starts the restore req and kills the primary as soon as
// the clone of db.docs holds a document matching writing; the restore must then
// complete with the state want or fail cleanly, and a retry must restore want.
func (r *rig) restoreThroughFailover(t *testing.T, req models.RestoreRequest, db string, want dbState, writing bson.D) {
	c := r.c
	rec := r.startRestore(t, req)
	clone := c.client.Database(db + rec.PITR.CloneSuffix).Collection("docs")
	deadline := time.Now().Add(10 * time.Minute)
	for {
		got, err := r.repo.GetRestoreRecord(r.ctx, rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != models.RestoreStatusInProgress && got.Status != models.RestoreStatusPending {
			t.Fatalf("the restore ended (%s) before the failover; make it slower", got.Status)
		}
		ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
		n, _ := clone.CountDocuments(ctx, writing, options.Count().SetLimit(1))
		cancel()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the restore did not start writing")
		}
		time.Sleep(100 * time.Millisecond)
	}
	old := c.primary(t)
	c.kill(t, old)
	c.newPrimary(t, old)
	first := r.waitRestore(t, rec.ID)
	c.start(t, old)
	c.waitHealthy(t)

	switch first.Status {
	case models.RestoreStatusCompleted:
		t.Logf("the restore completed despite the failover")
		assertSameState(t, "clone after a failover", state(t, c.client, db+first.PITR.CloneSuffix), want)
		if first.PITR.OpsApplied == nil || *first.PITR.OpsApplied != first.PITR.OpsReplayed {
			t.Fatalf("mongorestore applied %v operations, the filter wrote %d", first.PITR.OpsApplied, first.PITR.OpsReplayed)
		}
	case models.RestoreStatusFailed:
		t.Logf("the restore failed on the failover: %s", first.ErrorMessage)
		if first.ErrorMessage == "" {
			t.Fatal("the failed restore has no error message")
		}
		for _, name := range r.databases(t) {
			if strings.HasSuffix(name, first.PITR.CloneSuffix) && !slices.Contains(first.PITR.Clones, name) {
				t.Fatalf("clone %s is not recorded on the failed restore", name)
			}
		}
		if !strings.Contains(first.ErrorMessage, "kept for inspection") {
			for _, name := range r.databases(t) {
				if strings.HasSuffix(name, first.PITR.CloneSuffix) {
					t.Fatalf("the failed restore left clone %s without saying so", name)
				}
			}
		}
	default:
		t.Fatalf("restore %s: %s", first.Status, first.ErrorMessage)
	}

	// The retry after the failover.
	time.Sleep(time.Second) // a new clone suffix
	again := r.waitRestore(t, r.startRestore(t, req).ID)
	if again.Status != models.RestoreStatusCompleted {
		t.Fatalf("the retry: %s %s", again.Status, again.ErrorMessage)
	}
	assertSameState(t, "clone of the retry", state(t, c.client, db+again.PITR.CloneSuffix), want)
	if again.PITR.OpsApplied == nil || *again.PITR.OpsApplied != again.PITR.OpsReplayed {
		t.Fatalf("mongorestore applied %v operations, the filter wrote %d", again.PITR.OpsApplied, again.PITR.OpsReplayed)
	}
	// The measured rates of a three-member replica set (docs/pitr.md#restore-time-rto).
	perSec, bytesPerSec, ok := again.PITR.PITRReplayRate()
	if !ok || again.PITR.BaseSeconds <= 0 {
		t.Fatalf("the restore did not time its passes: %+v", again.PITR)
	}
	t.Logf("replay: %d entries, %d stored bytes in %.1fs (%.0f entries/s, %.2f MiB/s); base: %d bytes in %.1fs (%.2f MiB/s)",
		again.PITR.OpsReplayed, again.PITR.OplogBytes, again.PITR.ReplaySeconds, perSec, bytesPerSec/(1<<20),
		again.PITR.BaseBytes, again.PITR.BaseSeconds, float64(again.PITR.BaseBytes)/again.PITR.BaseSeconds/(1<<20))
}

// TestRollbackThroughNetworkIsolation cuts the primary off the network, writes to
// it with w:1 while the others elect a new primary and take more writes, and puts
// it back: it rolls its writes back, and none of them appears in any chunk, while
// the chain stays unbroken.
func TestRollbackThroughNetworkIsolation(t *testing.T) {
	c := requireCluster(t)
	r := newRig(t, c, rigOptions{})
	db := r.uniqueDB(t, "rollback")
	w := startWriter(t, c, db, 0, 10*time.Millisecond, 64)
	time.Sleep(3 * time.Second)
	w.halt()
	old := c.primary(t)
	// The same insertMany, majority-committed: these must be found in the chunks,
	// so the search for the rolled-back ones below is not vacuous.
	const kept = 50
	if out := c.shell(t, old, fmt.Sprintf(`
		const res = db.getSiblingDB(%q).events.insertMany(
			Array.from({length: %d}, (_, i) => ({keep: NumberLong(i)})), {writeConcern: {w: "majority"}});
		print(Object.keys(res.insertedIds).length)`, db, kept)); !strings.HasSuffix(out, fmt.Sprint(kept)) {
		t.Fatalf("the majority writes failed: %s", out)
	}
	r.waitCovered(t)

	c.isolate(t, old)
	const lost = 50
	out := c.shell(t, old, fmt.Sprintf(`
		const res = db.getSiblingDB(%q).events.insertMany(
			Array.from({length: %d}, (_, i) => ({rb: NumberLong(i)})), {writeConcern: {w: 1}});
		print(Object.keys(res.insertedIds).length)`, db, lost))
	if !strings.HasSuffix(out, fmt.Sprint(lost)) {
		t.Fatalf("the isolated primary did not take the writes: %s", out)
	}
	count := func() string {
		return c.shell(t, old, fmt.Sprintf(`db.getMongo().setReadPref("nearest");
			print(db.getSiblingDB(%q).events.countDocuments({rb: {$exists: true}}))`, db))
	}
	if got := count(); !strings.HasSuffix(got, fmt.Sprint(lost)) {
		t.Fatalf("the isolated member holds %s of the %d writes", got, lost)
	}
	current := c.newPrimary(t, old)
	t.Logf("%s is primary, %s is isolated", current, old)
	w2 := startWriter(t, c, db, 1_000_000, 10*time.Millisecond, 64)
	time.Sleep(3 * time.Second)
	w2.halt()

	c.reconnect(t, old)
	c.waitHealthy(t)
	deadline := time.Now().Add(2 * time.Minute)
	for !strings.HasSuffix(count(), "\n0") && count() != "0" {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not roll back its writes", old)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("%s rolled back %d writes", old, lost)

	entries := r.assertOneChain(t)
	found := 0
	for _, e := range entries {
		if n := e.markers("rb", anyNS); len(n) > 0 {
			t.Fatalf("rolled-back writes %v (%s) are in a chunk", n, e.key())
		}
		found += len(e.markers("keep", anyNS))
	}
	if found != kept {
		t.Fatalf("the chunks hold %d of the %d majority-committed inserts", found, kept)
	}
	ns := db + ".events"
	w.check(t, ns, entries)
	w2.check(t, ns, entries)
}

// TestDivergenceAfterForcedReconfig loses the majority that holds the newest
// stored writes: a secondary is cut off, the other two take writes the collector
// stores, both die, and the cut-off member is forced into a one-member set. The
// collector ends the chain where the surviving history ends, supersedes the
// chunks of lost writes, raises pitr.diverged, continues in a new chain from that
// point with a new base, and the chains hold exactly the survivor's oplog; a
// point-in-time restore of the new chain works.
func TestDivergenceAfterForcedReconfig(t *testing.T) {
	c := requireCluster(t)
	r := newRig(t, c, rigOptions{})
	db := r.uniqueDB(t, "diverge")
	w := startWriter(t, c, db, 0, 10*time.Millisecond, 64)
	time.Sleep(2 * time.Second)
	r.base(t)
	time.Sleep(2 * time.Second)
	w.halt()
	r.waitCovered(t)

	primary := c.primary(t)
	var survivor, other string
	for _, h := range c.hosts {
		switch {
		case h == primary:
		case survivor == "":
			survivor = h
		default:
			other = h
		}
	}
	c.isolate(t, survivor)
	// Majority-committed by the primary and the other secondary, and stored.
	coll := c.client.Database(db).Collection("events")
	lostDocs := make([]any, 0, 50)
	for i := range 50 {
		lostDocs = append(lostDocs, bson.D{{Key: "lost", Value: int64(i)}})
	}
	if _, err := coll.InsertMany(r.ctx, lostDocs); err != nil {
		t.Fatal(err)
	}
	r.waitCovered(t)
	c.kill(t, primary)
	c.kill(t, other)
	c.reconnect(t, survivor)
	out := c.shell(t, survivor, fmt.Sprintf(`
		const cfg = rs.conf();
		cfg.members = cfg.members.filter(m => m.host === %q);
		printjson(rs.reconfig(cfg, {force: true}).ok)`, survivor))
	t.Logf("forced reconfiguration on %s: %s", survivor, out)
	if got := c.newPrimary(t, ""); got != survivor {
		t.Fatalf("%s is primary, want %s", got, survivor)
	}
	w2 := startWriter(t, c, db, 1_000_000, 10*time.Millisecond, 64)
	t.Cleanup(func() {
		// The two dead members hold the lost history; they stay out of the set.
		c.start(t, primary)
		c.start(t, other)
	})

	deadline := time.Now().Add(3 * time.Minute)
	var chains []*pitr.Chain
	for {
		chains = r.chains(t)
		if len(chains) == 2 {
			break
		}
		if time.Now().After(deadline) {
			st, _ := r.repo.LoadState(r.ctx, streamID)
			dumpCollector(t)
			t.Fatalf("no divergence was detected: %d chain(s), state %+v", len(chains), st)
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(3 * time.Second)
	w2.halt()
	first, second := chains[0], chains[1]
	if first.EndReason != pitr.EndDiverged || !second.Open() {
		t.Fatalf("chains %+v and %+v", first, second)
	}
	if second.Start != first.End {
		t.Fatalf("the new chain starts at %s, the diverged one ended at %s", second.Start, first.End)
	}
	if n := r.events.count(events.PITRDiverged); n != 1 {
		t.Fatalf("%d pitr.diverged events", n)
	}
	last := r.waitCovered(t)

	// The stored history: the first chain's committed chunks and the new chain.
	committed := r.chainEntries(t, first, true)
	next := r.chainEntries(t, second, false)
	// The new chain's first chunk keeps the entry it starts at: the end of the
	// diverged chain.
	if len(committed) == 0 || len(next) == 0 || next[0].key() != committed[len(committed)-1].key() {
		t.Fatalf("the new chain does not start at the last entry of the diverged one (%d and %d entries)", len(committed), len(next))
	}
	entries := append(committed, next[1:]...)
	assertSameHistory(t, "chains against the survivor", entries, c.direct(t, survivor), first.Start, last)
	for _, e := range entries {
		if n := e.markers("lost", anyNS); len(n) > 0 {
			t.Fatalf("lost writes %v (%s) are in a live chunk", n, e.key())
		}
	}
	// The lost writes were stored, in superseded chunks only.
	superseded, lostStored := 0, 0
	for _, ch := range r.chunks(t, first.ChainID, true) {
		hasLost := false
		for _, e := range r.readChunk(t, ch, ch.From == first.Start) {
			if n := e.markers("lost", anyNS); len(n) > 0 {
				hasLost = true
				lostStored += len(n)
			}
		}
		if ch.Status == pitr.ChunkSuperseded {
			superseded++
		} else if hasLost {
			t.Fatalf("committed chunk %s holds lost writes", ch.ID)
		}
	}
	if superseded == 0 || lostStored != 50 {
		t.Fatalf("%d superseded chunk(s) hold %d of the 50 lost writes", superseded, lostStored)
	}
	ns := db + ".events"
	w.check(t, ns, entries)
	w2.check(t, ns, entries)

	// The new chain gets its base (base on gap) and can be restored.
	newBase := r.waitBaseAfter(t, second.Start)
	t.Logf("base %s covers the new chain", newBase.ID)
	if _, err := coll.InsertOne(r.ctx, bson.D{{Key: "after", Value: true}}); err != nil {
		t.Fatal(err)
	}
	want := state(t, c.client, db)
	at := r.nextSecond(t)
	if _, err := coll.InsertOne(r.ctx, bson.D{{Key: "later", Value: true}}); err != nil {
		t.Fatal(err)
	}
	r.waitCovered(t)
	target := time.Unix(int64(at), 0).UTC()
	rec := r.waitRestore(t, r.startRestore(t, models.RestoreRequest{
		PITR: &models.PITRTarget{StreamID: streamID, At: &target}, Databases: []string{db}}).ID)
	if rec.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore after the divergence: %s %s", rec.Status, rec.ErrorMessage)
	}
	if rec.PITR.ChainID != second.ChainID {
		t.Fatalf("the restore used chain %s, want %s", rec.PITR.ChainID, second.ChainID)
	}
	assertSameState(t, "clone after the divergence", state(t, c.client, db+rec.PITR.CloneSuffix), want)
}

// dumpCollector logs the stacks of the collector's goroutines, to see where it
// waits when it does not react.
func dumpCollector(t *testing.T) {
	t.Helper()
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "pitr/collector") {
			t.Log(g)
		}
	}
}

// waitBaseAfter waits for a completed base backup whose t_before is after from.
func (r *rig) waitBaseAfter(t *testing.T, from pitr.Timestamp) *models.BackupRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		bases, err := r.repo.ListBaseBackups(r.ctx, streamID)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range bases {
			if b.Status == models.StatusCompleted && b.TBefore != nil && b.TBefore.TS.Compare(from) >= 0 {
				return b
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no base backup after %s", from)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

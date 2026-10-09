//go:build replset3 && soak

package replset

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// soakConfig is the configuration of a soak run, from MONGORESCUE_SOAK_*.
type soakConfig struct {
	// Duration is how long the collector runs under load (DURATION, default 10m).
	Duration time.Duration `json:"duration"`
	// ChunkSeconds is the chunk interval (CHUNK_SECONDS, default 15).
	ChunkSeconds int `json:"chunk_seconds"`
	// BaseEvery is the base backup interval (BASE_EVERY, default Duration/5, at
	// least 2m); it becomes the stream's cron schedule.
	BaseEvery time.Duration `json:"base_every"`
	// KeepBases is the base retention by count (KEEP_BASES, default 2).
	KeepBases int `json:"keep_bases"`
	// Rate is the target number of writes per second (RATE, default 200), half
	// inserts and half deletes of the oldest documents, so the data stays bounded.
	Rate int `json:"rate"`
	// Sample is how often the invariants are checked (SAMPLE, default 30s).
	Sample time.Duration `json:"sample"`
	// Report is a file the JSON report is written to (REPORT, optional).
	Report string `json:"-"`
}

// loadSoakConfig reads the configuration from the environment.
func loadSoakConfig(t *testing.T) soakConfig {
	t.Helper()
	env := func(name string) string { return os.Getenv("MONGORESCUE_SOAK_" + name) }
	dur := func(name string, def time.Duration) time.Duration {
		if v := env(name); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				t.Fatalf("MONGORESCUE_SOAK_%s=%q: not a positive duration", name, v)
			}
			return d
		}
		return def
	}
	num := func(name string, def int) int {
		if v := env(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				t.Fatalf("MONGORESCUE_SOAK_%s=%q: not a positive number", name, v)
			}
			return n
		}
		return def
	}
	c := soakConfig{Duration: dur("DURATION", 10*time.Minute)}
	c.ChunkSeconds = num("CHUNK_SECONDS", 15)
	c.BaseEvery = dur("BASE_EVERY", max(c.Duration/5, 2*time.Minute)).Truncate(time.Minute)
	c.KeepBases = num("KEEP_BASES", 2)
	c.Rate = num("RATE", 200)
	c.Sample = dur("SAMPLE", 30*time.Second)
	c.Report = env("REPORT")
	return c
}

// cron returns the base schedule: every BaseEvery, in whole minutes below an hour
// and whole hours above.
func (c soakConfig) cron() string {
	if c.BaseEvery < time.Hour {
		return fmt.Sprintf("*/%d * * * *", int(c.BaseEvery/time.Minute))
	}
	return fmt.Sprintf("0 */%d * * *", min(int(c.BaseEvery/time.Hour), 23))
}

// soakReport is what a soak run measured.
type soakReport struct {
	Config soakConfig `json:"config"`
	// Started and Finished bound the run.
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	// Writes counts the acknowledged writes and WriteErrors the failed ones.
	Writes      int64 `json:"writes"`
	WriteErrors int64 `json:"write_errors"`
	// Samples counts the checks; MaxChunkObjects, MaxLiveBases and MaxLagSeconds
	// are the largest values seen, ChunkObjectBound the allowed maximum.
	Samples          int     `json:"samples"`
	MaxChunkObjects  int     `json:"max_chunk_objects"`
	ChunkObjectBound int     `json:"chunk_object_bound"`
	MaxLiveBases     int     `json:"max_live_bases"`
	MaxLagSeconds    float64 `json:"max_lag_seconds"`
	// BasesTaken counts the completed bases, ChunksStored every chunk committed and
	// ChunksDeleted those retention deleted.
	BasesTaken    int `json:"bases_taken"`
	ChunksStored  int `json:"chunks_stored"`
	ChunksDeleted int `json:"chunks_deleted"`
	// EntriesCompared is how many stored entries were compared with the oplog.
	EntriesCompared int `json:"entries_compared"`
	// ChainTest is the outcome of the final chain test and its replay rates.
	ChainTest *chainTestReport `json:"chain_test,omitempty"`
}

// chainTestReport is the outcome of a chain test.
type chainTestReport struct {
	ID              string  `json:"id"`
	Verification    string  `json:"verification"`
	DurationSeconds float64 `json:"duration_seconds"`
	BaseBytes       int64   `json:"base_bytes"`
	OplogBytes      int64   `json:"oplog_bytes"`
	OpsReplayed     int64   `json:"ops_replayed"`
	ReplaySeconds   float64 `json:"replay_seconds"`
	// EntriesPerSecond and MiBPerSecond are the replay throughput (pass 2).
	EntriesPerSecond float64 `json:"entries_per_second"`
	MiBPerSecond     float64 `json:"mib_per_second"`
}

// soakLoad writes rate operations per second into db.events: inserts of new
// documents and deletes of the oldest ones, so the collection stays near window
// documents.
type soakLoad struct {
	mu            sync.Mutex
	writes, fails int64
	stop          chan struct{}
	wg            sync.WaitGroup
}

// startSoakLoad starts the load: four workers that each insert a document and
// delete their oldest one beyond window/4, rate writes per second in total.
func startSoakLoad(c *cluster, db string, rate, window int) *soakLoad {
	coll := c.client.Database(db).Collection("events", options.Collection().SetWriteConcern(writeconcern.Majority()))
	l := &soakLoad{stop: make(chan struct{})}
	pad := strings.Repeat("s", 512)
	const workers = 4
	every := time.Duration(int64(time.Second) * workers * 2 / int64(rate))
	for w := range workers {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			tick := time.NewTicker(every)
			defer tick.Stop()
			for n := int64(0); ; n++ {
				select {
				case <-l.stop:
					return
				case <-tick.C:
				}
				id := int64(w)<<40 | n
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "w", Value: int64(w)}, {Key: "n", Value: n}, {Key: "pad", Value: pad}})
				if err == nil && n >= int64(window/workers) {
					_, err = coll.DeleteOne(ctx, bson.D{{Key: "_id", Value: int64(w)<<40 | (n - int64(window/workers))}})
				}
				cancel()
				l.mu.Lock()
				if err != nil {
					l.fails++
				} else {
					l.writes += 2
				}
				l.mu.Unlock()
			}
		}()
	}
	return l
}

// halt stops the load and returns its counts.
func (l *soakLoad) halt() (writes, fails int64) {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	l.wg.Wait()
	return l.counts()
}

// counts returns the acknowledged and the failed writes so far.
func (l *soakLoad) counts() (writes, fails int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writes, l.fails
}

// TestSoak runs the collector with base backups on a schedule and retention under
// a steady write load for MONGORESCUE_SOAK_DURATION and checks, every sample and
// at the end: one chain and no superseded chunk (no gap, no false break), a
// healthy collector that keeps up, a bounded number of chunk objects and live
// bases, and that retention deletes and purges what falls out of the window. At
// the end the stored entries are compared with the primary's oplog and a chain
// test restores one base to the next.
func TestSoak(t *testing.T) {
	c := requireCluster(t)
	cfg := loadSoakConfig(t)
	t.Logf("soak: %+v (base schedule %q)", cfg, cfg.cron())
	r := newRig(t, c, rigOptions{chunkSeconds: cfg.ChunkSeconds, baseCron: cfg.cron(), keepBases: cfg.KeepBases, countOnly: true,
		retention: max(cfg.Sample, 30*time.Second)})
	db := r.uniqueDB(t, "soak")
	report := &soakReport{Config: cfg, Started: time.Now().UTC()}
	defer func() {
		report.Finished = time.Now().UTC()
		writeReport(t, cfg.Report, report)
	}()
	load := startSoakLoad(c, db, cfg.Rate, 20_000)
	t.Cleanup(func() { load.halt() })

	interval := time.Duration(cfg.ChunkSeconds) * time.Second
	// Chunks of the kept bases' span plus one base interval of slack; a chunk can
	// be shorter than the interval while the collector catches up.
	report.ChunkObjectBound = (cfg.KeepBases+2)*int(cfg.BaseEvery/interval) + 60
	lagLimit := max(5*time.Minute, 5*interval)
	end := time.Now().Add(cfg.Duration)
	for time.Now().Before(end) {
		time.Sleep(min(cfg.Sample, time.Until(end)))
		report.Writes, report.WriteErrors = load.counts()
		r.soakSample(t, report, lagLimit)
	}
	report.Writes, report.WriteErrors = load.halt()
	if report.Writes == 0 {
		t.Fatal("the load generator wrote nothing")
	}
	last := r.waitCovered(t)
	r.soakSample(t, report, lagLimit)

	// Retention ran: with more bases than it keeps, chunks were deleted, and once
	// purged, every chunk object belongs to a live chunk.
	if report.BasesTaken > cfg.KeepBases+1 && report.ChunksDeleted == 0 {
		t.Fatalf("%d bases taken, %d kept, but retention deleted no chunk", report.BasesTaken, cfg.KeepBases)
	}
	for range 2 {
		if err := r.col.RetainStream(r.ctx, streamID); err != nil {
			t.Fatal(err)
		}
	}
	chain := r.chains(t)[0]
	live := r.chunks(t, chain.ChainID, true)
	if objects := r.chunkObjects(t); objects != len(live) {
		t.Fatalf("%d chunk objects for %d live chunks after the purge", objects, len(live))
	}
	r.assertRetainedFromOldestBase(t, chain, live)

	// The stored history equals the primary's oplog where both still have it.
	entries := r.chainEntries(t, chain, false)
	primary := c.direct(t, c.primary(t))
	win, err := r.prober.OplogWindow(r.ctx, c.uri)
	if err != nil {
		t.Fatal(err)
	}
	from := live[0].From
	if win.Oldest.Compare(from) > 0 {
		from = win.Oldest
	}
	var tail []oplogEntry
	for _, e := range entries {
		if (pitr.Timestamp{T: e.TS.T, I: e.TS.I}).Compare(from) > 0 {
			tail = append(tail, e)
		}
	}
	report.EntriesCompared = len(tail)
	after := pitr.Timestamp{T: from.T, I: from.I + 1}
	assertSameHistory(t, "retained chain against the primary", tail, primary, after, last)

	// A chain test restores the base before the newest to the newest's point.
	report.ChainTest = r.soakChainTest(t)
}

// soakSample checks the invariants once and updates the report.
func (r *rig) soakSample(t *testing.T, report *soakReport, lagLimit time.Duration) {
	t.Helper()
	report.Samples++
	st, err := r.repo.LoadState(r.ctx, streamID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != pitr.CollectorRunning || st.LastError != "" {
		t.Fatalf("collector %s: %s", st.Status, st.LastError)
	}
	chains := r.chains(t)
	if len(chains) != 1 || !chains[0].Open() {
		t.Fatalf("%d chains: a false break (%+v)", len(chains), chains[len(chains)-1])
	}
	for _, typ := range []events.EventType{events.PITRChainBroken, events.PITRDiverged} {
		if n := r.events.count(typ); n != 0 {
			t.Fatalf("%d %s event(s)", n, typ)
		}
	}
	all, err := r.repo.ListChunks(r.ctx, pitr.ChunkQuery{StreamID: streamID, ChainID: chains[0].ChainID})
	if err != nil {
		t.Fatal(err)
	}
	deleted := 0
	var newest time.Time
	for _, ch := range all {
		if ch.Status == pitr.ChunkSuperseded {
			t.Fatalf("chunk %s is superseded without a divergence", ch.ID)
		}
		if ch.DeletedAt != nil {
			deleted++
		}
		if ch.CreatedAt.After(newest) {
			newest = ch.CreatedAt
		}
	}
	report.ChunksStored, report.ChunksDeleted = len(all), deleted
	lastEnd := time.Unix(int64(st.Last.TS.T), 0)
	lag := time.Since(lastEnd)
	report.MaxLagSeconds = max(report.MaxLagSeconds, lag.Seconds())
	if lag > lagLimit {
		t.Fatalf("the collector is %s behind (limit %s)", lag.Round(time.Second), lagLimit)
	}
	objects := r.chunkObjects(t)
	report.MaxChunkObjects = max(report.MaxChunkObjects, objects)
	if objects > report.ChunkObjectBound {
		t.Fatalf("%d chunk objects, more than the bound %d: retention does not keep up", objects, report.ChunkObjectBound)
	}
	bases, err := r.repo.ListBaseBackups(r.ctx, streamID)
	if err != nil {
		t.Fatal(err)
	}
	liveBases, taken := 0, 0
	for _, b := range bases {
		if b.Status == models.StatusCompleted || b.Status.Deleted() {
			taken++
		}
		if b.Status == models.StatusCompleted {
			liveBases++
		}
	}
	report.BasesTaken = taken
	report.MaxLiveBases = max(report.MaxLiveBases, liveBases)
	// The newest eligible base is always kept on top of the count.
	if liveBases > report.Config.KeepBases+1 {
		t.Fatalf("%d live bases, retention keeps %d", liveBases, report.Config.KeepBases)
	}
	t.Logf("sample %d: lag %s, %d chunk objects, %d chunks stored, %d deleted, %d live bases of %d",
		report.Samples, lag.Round(time.Second), objects, len(all), deleted, liveBases, taken)
}

// chunkObjects counts the chunk objects in storage.
func (r *rig) chunkObjects(t *testing.T) int {
	t.Helper()
	objs, err := r.st.List(r.ctx, "_mongorescue/oplog/")
	if err != nil {
		t.Fatal(err)
	}
	return len(objs)
}

// assertRetainedFromOldestBase checks the retention rule: no live chunk ends
// before the t_before of the oldest kept eligible base.
func (r *rig) assertRetainedFromOldestBase(t *testing.T, chain *pitr.Chain, live []*pitr.Chunk) {
	t.Helper()
	bases, err := r.repo.ListBaseBackups(r.ctx, streamID)
	if err != nil {
		t.Fatal(err)
	}
	var oldest *pitr.OpTime
	for _, b := range bases {
		if b.Status != models.StatusCompleted || b.TBefore == nil {
			continue
		}
		if oldest == nil || b.TBefore.TS.Compare(oldest.TS) < 0 {
			oldest = b.TBefore
		}
	}
	if oldest == nil {
		t.Fatal("no live base")
	}
	if live[0].From != chain.Start && live[0].To.Compare(oldest.TS) < 0 {
		t.Fatalf("live chunk %s ends at %s, before the oldest kept base's t_before %s", live[0].ID, live[0].To, oldest.TS)
	}
}

// soakChainTest runs a chain test and returns its outcome.
func (r *rig) soakChainTest(t *testing.T) *chainTestReport {
	t.Helper()
	rec, err := r.ops.StartChainTest(auth.WithPrincipal(r.ctx, auth.SystemPrincipal()), streamID)
	if err != nil {
		t.Fatalf("StartChainTest: %v", err)
	}
	final := r.waitRestore(t, rec.ID)
	deadline := time.Now().Add(5 * time.Minute)
	for final.Verification == nil && time.Now().Before(deadline) {
		time.Sleep(time.Second)
		final = r.waitRestore(t, rec.ID)
	}
	if final.Status != models.RestoreStatusCompleted || final.Verification == nil ||
		final.Verification.Status != models.RestoreVerificationPassed {
		t.Fatalf("chain test %s: %s %s %+v", final.ID, final.Status, final.ErrorMessage, final.Verification)
	}
	out := &chainTestReport{ID: final.ID, Verification: string(final.Verification.Status), DurationSeconds: final.DurationSeconds,
		BaseBytes: final.PITR.BaseBytes, OplogBytes: final.PITR.OplogBytes, OpsReplayed: final.PITR.OpsReplayed,
		ReplaySeconds: final.PITR.ReplaySeconds}
	perSec, bytesPerSec, ok := final.PITR.PITRReplayRate()
	if !ok || final.PITR.BaseSeconds <= 0 {
		t.Fatalf("the chain test did not time its passes: %+v", final.PITR)
	}
	out.EntriesPerSecond, out.MiBPerSecond = perSec, bytesPerSec/(1<<20)
	t.Logf("chain test %s passed in %.1fs: base %d bytes in %.1fs, oplog %d bytes and %d entries replayed in %.1fs (%.0f entries/s, %.2f MiB/s)",
		final.ID, final.DurationSeconds, final.PITR.BaseBytes, final.PITR.BaseSeconds, final.PITR.OplogBytes, final.PITR.OpsReplayed,
		final.PITR.ReplaySeconds, perSec, bytesPerSec/(1<<20))
	return out
}

// writeReport writes the report to path (and the log).
func writeReport(t *testing.T, path string, report *soakReport) {
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Error(err)
		return
	}
	t.Logf("soak report: %s", raw)
	if path == "" {
		return
	}
	if err = os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Errorf("write the soak report: %v", err)
	}
}

//go:build replset3

package replset

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/oplog"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// Fixed IDs of the rig's stream, connection and storage target.
const (
	streamID     = "str_rs"
	connectionID = "conn_rs"
	targetID     = "tgt_rs"
)

// logger returns the logger of the components under test: warnings and errors,
// everything with MONGORESCUE_RS_VERBOSE=1.
func logger() *slog.Logger {
	level := slog.LevelWarn
	if os.Getenv("MONGORESCUE_RS_VERBOSE") == "1" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// connections serves the rig's one connection to the operations service.
type connections map[string]*models.Connection

func (c connections) Resolve(_ context.Context, id string) (*models.Connection, error) {
	if conn, ok := c[id]; ok {
		cp := *conn
		return &cp, nil
	}
	return nil, operations.ErrUnknownConnection
}

func (c connections) Get(ctx context.Context, id string) (*models.Connection, error) {
	return c.Resolve(ctx, id)
}

func (c connections) List(context.Context) ([]*models.Connection, error) { return nil, nil }

// session adapts *mongoconn.OplogSession to collector.Session, as internal/app
// does.
type session struct{ *mongoconn.OplogSession }

func (s session) Pin(ctx context.Context, notBefore pitr.Timestamp) (collector.Member, error) {
	m, err := s.OplogSession.Pin(ctx, notBefore)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s session) Primary() collector.Member { return s.OplogSession.Primary() }

// eventLog records the events the collector publishes.
type eventLog struct {
	mu     sync.Mutex
	events []events.Event
}

func (l *eventLog) Publish(_ context.Context, e events.Event) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return true
}

// count returns how many events of type typ were published.
func (l *eventLog) count(typ events.EventType) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// rigOptions configure a rig.
type rigOptions struct {
	// chunkSeconds is the chunk interval (default 1).
	chunkSeconds int
	// baseCron schedules base backups; "" takes them only on demand and after a
	// gap or a divergence.
	baseCron string
	// keepBases is the base retention by count (default 7).
	keepBases int
	// countOnly turns base retention by age off, so only keepBases counts (the
	// stream default also keeps every base younger than 14 days).
	countOnly bool
	// retention is how often retention runs (default: the collector's).
	retention time.Duration
}

// rig is a running collector on the replica set, with the engines and the
// operations service that back up and restore through it.
type rig struct {
	c       *cluster
	ctx     context.Context
	repo    *store.SQLiteStore
	prober  *mongoconn.Prober
	stream  *pitr.Stream
	st      storage.Storage
	dec     *encryption.Decryptor
	backups *backup.Engine
	ops     *operations.Service
	col     *collector.Service
	events  *eventLog
	conn    *models.Connection

	// baseMu guards baseRunning; bases runs the background base backups.
	baseMu      sync.Mutex
	baseRunning bool
	bases       sync.WaitGroup
}

// newRig starts a collector on c and waits for its chain.
func newRig(t *testing.T, c *cluster, o rigOptions) *rig {
	t.Helper()
	if o.chunkSeconds == 0 {
		o.chunkSeconds = 1
	}
	if o.keepBases == 0 {
		o.keepBases = 7
	}
	ctx, cancel := context.WithCancel(context.Background())
	identity, recipient, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.NewX25519Encryptor([]string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := encryption.NewDecryptor(encryption.DecryptorConfig{Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{c: c, ctx: ctx, repo: storetest.New(t), prober: mongoconn.New(), st: st, dec: dec, events: &eventLog{},
		conn: &models.Connection{ID: connectionID, Name: "replica set", URI: c.uri}}
	// Right after rs.initiate, a new connection may fail its handshake until every
	// member has the cluster's signing keys.
	var win pitr.OplogWindow
	for deadline := time.Now().Add(opTimeout); ; time.Sleep(time.Second) {
		if win, err = r.prober.OplogWindow(ctx, c.uri); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	r.stream = &pitr.Stream{ID: streamID, ConnectionID: connectionID, ReplicaSet: win.ReplicaSet, TargetID: targetID, Enabled: true,
		BaseCron: o.baseCron, BaseKeepCount: o.keepBases, BaseKeepDays: 14, ChunkSeconds: o.chunkSeconds, BaseOnGap: true}
	if o.countOnly {
		r.stream.BaseKeepDays = 0
	}
	if r.stream.BaseCron == "" {
		// Never due on its own within a test.
		r.stream.BaseCron = "0 0 1 1 *"
	}
	if err = r.repo.CreateStream(ctx, r.stream); err != nil {
		t.Fatal(err)
	}

	names := func(ctx context.Context, uri string) ([]string, error) {
		dbs, listErr := r.prober.ListDatabases(ctx, uri)
		out := make([]string, 0, len(dbs))
		for _, d := range dbs {
			out = append(out, d.Name)
		}
		return out, listErr
	}
	collections := func(ctx context.Context, uri, database string) ([]string, error) {
		cols, listErr := r.prober.ListCollections(ctx, uri, database)
		out := make([]string, 0, len(cols))
		for _, col := range cols {
			out = append(out, col.Name)
		}
		return out, listErr
	}
	log := logger()
	r.backups = backup.NewEngine(st, c.uri, backup.WithLogger(log), backup.WithCollectionLister(collections),
		backup.WithEncryptor(enc), backup.WithOpTimeReader(r.prober.WriteOpTimes),
		backup.WithManifestCapturer(r.prober.Manifest), backup.WithDatabaseLister(names))
	restores := restore.NewEngine(st, c.uri, restore.WithLogger(log),
		restore.WithValidationBypassCheck(r.prober.CanBypassDocumentValidation), restore.WithDatabaseAdmin(r.prober),
		restore.WithDecryptor(dec), restore.WithDatabaseLister(names),
		restore.WithServerVersion(func(ctx context.Context, uri string) (string, error) {
			info, pingErr := r.prober.Ping(ctx, uri)
			return info.Version, pingErr
		}))
	manager := runs.NewManager(log)
	r.ops = operations.New(operations.Config{
		Store: r.repo, Backup: r.backups, Restore: restores, Runs: manager,
		Connections: connections{connectionID: r.conn},
		PITR:        r.repo, PITRRestore: restores, PITRBases: r.repo.ListBaseBackups, Inspector: r.prober,
		ToolsVersion: func(ctx context.Context) (string, error) {
			return mongotools.NewResolver("").ToolVersion(ctx, "mongorestore")
		},
		Logger: log,
	})
	r.col = collector.New(collector.Config{
		Repo: r.repo,
		Open: func(ctx context.Context, s *pitr.Stream) (collector.Session, error) {
			sess, openErr := r.prober.OpenOplogSession(ctx, c.uri, s.ReadPreference)
			if openErr != nil {
				return nil, openErr
			}
			return session{sess}, nil
		},
		Storage:   func(context.Context, string) (storage.Storage, error) { return st, nil },
		Encryptor: func() *encryption.Encryptor { return enc },
		Decryptor: func() *encryption.Decryptor { return dec },
		StartBase: r.startBase,
		Bases:     r.repo.ListBaseBackups,
		NextRun: func(expr string, from time.Time) (time.Time, bool) {
			next := scheduler.NextRuns(expr, from, 1)
			if len(next) == 0 {
				return time.Time{}, false
			}
			return next[0], true
		},
		UpdateBase:        r.repo.UpdateBackupRecord,
		DeleteGrace:       func() time.Duration { return 0 },
		Publisher:         r.events,
		Logger:            log,
		ReconcileInterval: 200 * time.Millisecond,
		RetentionInterval: o.retention,
	})
	r.col.Start(ctx)
	// Stopped in reverse order: the collector, the base backups, the runs.
	t.Cleanup(func() {
		r.col.Stop()
		cancel()
		r.bases.Wait()
		_ = manager.Shutdown(context.Background())
	})
	deadline := time.Now().Add(time.Minute)
	for {
		if _, loadErr := r.repo.LoadState(ctx, streamID); loadErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the collector did not start a chain")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return r
}

// errBaseBusy refuses a base backup while another runs, like the operations
// service's concurrency key.
var errBaseBusy = errors.New("a base backup is running")

// startBase is the collector's BaseStarter: it starts an instance-scope base
// backup in the background.
func (r *rig) startBase(ctx context.Context, _ string, trigger models.BackupTrigger) (*models.BackupRecord, error) {
	r.baseMu.Lock()
	if r.baseRunning {
		r.baseMu.Unlock()
		return nil, errBaseBusy
	}
	r.baseRunning = true
	r.baseMu.Unlock()
	done := func() {
		r.baseMu.Lock()
		r.baseRunning = false
		r.baseMu.Unlock()
	}
	opts := r.baseOptions(trigger)
	rec, err := r.backups.Prepare(opts)
	if err == nil {
		err = r.repo.SaveBackupRecord(ctx, rec)
	}
	if err != nil {
		done()
		return nil, err
	}
	r.bases.Add(1)
	go func() {
		defer r.bases.Done()
		defer done()
		final, runErr := r.backups.Execute(r.ctx, opts, rec)
		if final == nil {
			final = rec
		}
		if runErr != nil && r.ctx.Err() == nil {
			logger().Error("base backup failed", slog.String("error", runErr.Error()))
		}
		_ = r.repo.SaveBackupRecord(context.WithoutCancel(r.ctx), final)
	}()
	return rec, nil
}

// baseOptions are the options of a base backup of the rig's stream.
func (r *rig) baseOptions(trigger models.BackupTrigger) models.BackupOptions {
	return models.BackupOptions{
		Scope: models.ScopeInstance, ConnectionID: connectionID, ReplicaSet: r.stream.ReplicaSet, PITRStreamID: streamID,
		MongoURI: r.c.uri, StorageTargetID: targetID, Gzip: true, Trigger: trigger,
	}
}

// base takes a base backup now and waits for it.
func (r *rig) base(t *testing.T) *models.BackupRecord {
	t.Helper()
	deadline := time.Now().Add(opTimeout)
	for {
		rec, err := r.startBase(r.ctx, streamID, models.TriggerManual)
		if err == nil {
			return r.waitBase(t, rec.ID)
		}
		if !errors.Is(err, errBaseBusy) || time.Now().After(deadline) {
			t.Fatalf("base backup: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitBase waits for base backup id to finish successfully.
func (r *rig) waitBase(t *testing.T, id string) *models.BackupRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for {
		rec, err := r.repo.GetBackupRecord(r.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		switch rec.Status {
		case models.StatusCompleted:
			return rec
		case models.StatusFailed, models.StatusCancelled:
			t.Fatalf("base backup %s: %s %s", id, rec.Status, rec.ErrorMessage)
		}
		if time.Now().After(deadline) {
			t.Fatalf("base backup %s did not finish", id)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// majorityNewest returns the newest majority-committed oplog position.
func (r *rig) majorityNewest(t *testing.T) pitr.Timestamp {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		w, err := r.prober.OplogWindow(r.ctx, r.c.uri)
		if err == nil && (w.MajorityOpTime.TS == w.Newest || time.Now().After(deadline)) {
			return w.MajorityOpTime.TS
		}
		if err != nil && time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitCovered waits until the collector has stored the newest majority-committed
// oplog entry.
func (r *rig) waitCovered(t *testing.T) pitr.Timestamp {
	t.Helper()
	target := r.majorityNewest(t)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		s, err := r.repo.LoadState(r.ctx, streamID)
		if err == nil && s.Last.TS.Compare(target) >= 0 {
			return s.Last.TS
		}
		if time.Now().After(deadline) {
			t.Fatalf("the collector did not reach %s (state %+v, %v)", target, s, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// chains returns the stream's chains, oldest first.
func (r *rig) chains(t *testing.T) []*pitr.Chain {
	t.Helper()
	chains, err := r.repo.ListChains(r.ctx, streamID)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(chains, func(a, b *pitr.Chain) int { return a.Start.Compare(b.Start) })
	return chains
}

// chunks returns the chunks of a chain in order; live drops deleted ones.
func (r *rig) chunks(t *testing.T, chainID string, live bool) []*pitr.Chunk {
	t.Helper()
	out, err := r.repo.ListChunks(r.ctx, pitr.ChunkQuery{StreamID: streamID, ChainID: chainID, Live: live})
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(out, func(a, b *pitr.Chunk) int { return a.From.Compare(b.From) })
	return out
}

// readChunk decrypts, gunzips and splits the object of c, checking the entry
// count and that every entry lies in (From, To], or [From, To] for the first
// chunk of a chain (first), which keeps the entry the chain starts at.
func (r *rig) readChunk(t *testing.T, c *pitr.Chunk, first bool) []oplogEntry {
	t.Helper()
	rc, err := r.st.Retrieve(r.ctx, c.StorageKey)
	if err != nil {
		t.Fatalf("chunk %s: %v", c.StorageKey, err)
	}
	defer rc.Close()
	plain, err := r.dec.Decrypt(rc)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		t.Fatal(err)
	}
	var out []oplogEntry
	rd := oplog.NewReader(gz)
	for {
		doc, nextErr := rd.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		var e oplogEntry
		if err = bson.Unmarshal(doc, &e); err != nil {
			t.Fatal(err)
		}
		e.O = append(bson.Raw(nil), e.O...)
		ts := pitr.Timestamp{T: e.TS.T, I: e.TS.I}
		low := ts.Compare(c.From)
		if low < 0 || (low == 0 && !first) || ts.Compare(c.To) > 0 {
			t.Fatalf("chunk %s (%s, %s] holds an entry at %s", c.ID, c.From, c.To, ts)
		}
		out = append(out, e)
	}
	if int64(len(out)) != c.Entries {
		t.Fatalf("chunk %s holds %d entries, recorded %d", c.ID, len(out), c.Entries)
	}
	return out
}

// chainEntries reads the live chunks of a chain (committed ones only when
// committed is set), checks that they follow each other without a hole or an
// overlap and that the entries strictly increase (no duplicate), and returns
// the entries. The first chunk of a chain keeps the entry at the chain's start.
func (r *rig) chainEntries(t *testing.T, chain *pitr.Chain, committed bool) []oplogEntry {
	t.Helper()
	var out []oplogEntry
	prev := chain.Start
	for i, c := range r.chunks(t, chain.ChainID, true) {
		if committed && c.Status != pitr.ChunkCommitted {
			continue
		}
		if i > 0 && c.From != prev {
			t.Fatalf("chain %s: chunk %s starts at %s, the previous one ends at %s", chain.ChainID, c.ID, c.From, prev)
		}
		for _, e := range r.readChunk(t, c, c.From == chain.Start) {
			if n := len(out); n > 0 && compareTS(out[n-1].TS, e.TS) >= 0 {
				t.Fatalf("chain %s: entry %s follows %s (duplicate or out of order)", chain.ChainID, e.key(), out[n-1].key())
			}
			out = append(out, e)
		}
		prev = c.To
	}
	return out
}

// compareTS compares two oplog timestamps.
func compareTS(a, b bson.Timestamp) int {
	return pitr.Timestamp{T: a.T, I: a.I}.Compare(pitr.Timestamp{T: b.T, I: b.I})
}

// assertSameHistory checks that got (the stored entries of a chain from its
// start) is exactly the oplog of member client in [from, to]: nothing lost,
// nothing added, same terms.
func assertSameHistory(t *testing.T, what string, got []oplogEntry, client *mongo.Client, from, to pitr.Timestamp) {
	t.Helper()
	// The first chunk of a chain keeps the entry at its start.
	before := bson.Timestamp{T: from.T, I: from.I - 1}
	if from.I == 0 {
		before = bson.Timestamp{T: from.T - 1, I: ^uint32(0)}
	}
	want := readOplog(t, client, before, bson.Timestamp{T: to.T, I: to.I})
	if len(want) == 0 {
		t.Fatalf("%s: the member's oplog holds nothing in (%s, %s]", what, from, to)
	}
	for i := 0; i < len(got) || i < len(want); i++ {
		switch {
		case i >= len(got):
			t.Fatalf("%s: %d entries stored, the oplog holds %d; first missing %s", what, len(got), len(want), want[i].key())
		case i >= len(want):
			t.Fatalf("%s: %d entries stored, the oplog holds %d; first extra %s", what, len(got), len(want), got[i].key())
		case got[i].key() != want[i].key():
			t.Fatalf("%s: entry %d is %s, the oplog has %s", what, i, got[i].key(), want[i].key())
		}
	}
}

// writer is a load generator: it inserts documents with an increasing field n
// into one collection with majority write concern and remembers which inserts
// were acknowledged and which were only attempted.
type writer struct {
	mu    sync.Mutex
	acked map[int64]bool
	tried map[int64]bool
	// first and next bound the numbers of the writer's inserts.
	first, next int64
	errors      int
	stop        chan struct{}
	finished    chan struct{}
}

// writerRange is the span of insert numbers one writer owns: writers into one
// collection start at multiples of it.
const writerRange = 1_000_000

// startWriter starts a writer into db.events, numbering its inserts from first,
// with a pause between inserts and pad bytes of padding per document.
func startWriter(t *testing.T, c *cluster, db string, first int64, pause time.Duration, pad int) *writer {
	t.Helper()
	coll := c.client.Database(db).Collection("events",
		options.Collection().SetWriteConcern(writeconcern.Majority()))
	w := &writer{acked: map[int64]bool{}, tried: map[int64]bool{}, first: first, next: first, stop: make(chan struct{}), finished: make(chan struct{})}
	payload := strings.Repeat("p", pad)
	go func() {
		defer close(w.finished)
		for {
			select {
			case <-w.stop:
				return
			default:
			}
			w.mu.Lock()
			n := w.next
			w.next++
			w.tried[n] = true
			w.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			_, err := coll.InsertOne(ctx, bson.D{{Key: "n", Value: n}, {Key: "pad", Value: payload}})
			cancel()
			w.mu.Lock()
			if err == nil {
				w.acked[n] = true
			} else {
				w.errors++
			}
			w.mu.Unlock()
			if err != nil {
				time.Sleep(200 * time.Millisecond)
			} else if pause > 0 {
				time.Sleep(pause)
			}
		}
	}()
	t.Cleanup(w.halt)
	return w
}

// halt stops the writer and waits for it.
func (w *writer) halt() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.finished
}

// ackedCount returns the number of acknowledged inserts.
func (w *writer) ackedCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.acked)
}

// check checks the writer's inserts into ns among entries (those numbered from
// its first on, before the next writer's range): every acknowledged one appears
// exactly once, and nothing that was never attempted appears.
func (w *writer) check(t *testing.T, ns string, entries []oplogEntry) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := map[int64]int{}
	inNS := func(s string) bool { return s == ns }
	for _, e := range entries {
		for _, n := range e.markers("n", inNS) {
			if n >= w.first && n < w.first+writerRange {
				seen[n]++
			}
		}
	}
	for n, k := range seen {
		if k != 1 {
			t.Fatalf("insert %d appears %d times", n, k)
		}
		if !w.tried[n] {
			t.Fatalf("insert %d appears but was never attempted", n)
		}
	}
	for n := range w.acked {
		if seen[n] != 1 {
			t.Fatalf("acknowledged insert %d appears %d times (%d acknowledged, %d errors)", n, seen[n], len(w.acked), w.errors)
		}
	}
	if len(w.acked) == 0 {
		t.Fatal("no insert was acknowledged")
	}
}

// dbState is a digest of a database: per collection, the number of documents and
// the SHA-256 of all of them in _id order.
type dbState map[string]string

// state returns the digest of database db read through client.
func state(t *testing.T, client *mongo.Client, db string) dbState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	d := client.Database(db)
	names, err := d.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	out := dbState{}
	for _, name := range names {
		if strings.HasPrefix(name, "system.") {
			continue
		}
		cur, findErr := d.Collection(name).Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
		if findErr != nil {
			t.Fatal(findErr)
		}
		h := sha256.New()
		n := 0
		for cur.Next(ctx) {
			h.Write(cur.Current)
			n++
		}
		if cur.Err() != nil {
			t.Fatal(cur.Err())
		}
		_ = cur.Close(ctx)
		out[name] = fmt.Sprintf("%d:%s", n, hex.EncodeToString(h.Sum(nil)))
	}
	return out
}

// assertSameState compares two digests.
func assertSameState(t *testing.T, what string, got, want dbState) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s: the expected state is empty", what)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: state %v, want %v", what, got, want)
	}
}

// uniqueDB returns a fresh database name and drops it on cleanup.
func (r *rig) uniqueDB(t *testing.T, tag string) string {
	t.Helper()
	name := fmt.Sprintf("%s_%d", tag, time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = r.c.client.Database(name).Drop(ctx)
	})
	return name
}

// nextSecond waits until the server's clock has left the second of the newest
// majority-committed entry and returns that second: a target at it restores every
// write so far and none made from now on.
func (r *rig) nextSecond(t *testing.T) uint32 {
	t.Helper()
	sec := r.majorityNewest(t).T
	for time.Now().Unix() <= int64(sec) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	return sec
}

// startRestore starts a point-in-time restore as the system principal.
func (r *rig) startRestore(t *testing.T, req models.RestoreRequest) *models.RestoreRecord {
	t.Helper()
	rec, err := r.ops.StartRestore(auth.WithPrincipal(r.ctx, auth.SystemPrincipal()), req)
	var pre *operations.PreflightError
	if errors.As(err, &pre) {
		t.Fatalf("preflight refused the restore: %+v", pre.Result.Checks)
	}
	if err != nil {
		t.Fatalf("StartRestore: %v", err)
	}
	t.Cleanup(func() { r.dropSuffix(rec.PITR.CloneSuffix) })
	return rec
}

// waitRestore waits for restore id to end and returns its record.
func (r *rig) waitRestore(t *testing.T, id string) *models.RestoreRecord {
	t.Helper()
	deadline := time.Now().Add(15 * time.Minute)
	for {
		got, err := r.repo.GetRestoreRecord(r.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != models.RestoreStatusInProgress && got.Status != models.RestoreStatusPending {
			if strings.Contains(got.ErrorMessage+got.Warning, r.c.password) {
				t.Fatal("the restore record leaks the password")
			}
			return got
		}
		if time.Now().After(deadline) {
			t.Fatal("the restore did not finish")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// dropSuffix drops every database whose name ends in suffix.
func (r *rig) dropSuffix(suffix string) {
	if suffix == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	names, err := r.c.client.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		return
	}
	for _, name := range names {
		if strings.HasSuffix(name, suffix) {
			_ = r.c.client.Database(name).Drop(ctx)
		}
	}
}

// databases lists the databases of the replica set.
func (r *rig) databases(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	names, err := r.c.client.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

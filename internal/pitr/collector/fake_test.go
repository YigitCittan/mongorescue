package collector

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/oplog"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// fakeEntry is one entry of the fake oplog.
type fakeEntry struct {
	op  pitr.OpTime
	doc []byte
}

// fakeOplog is an in-memory replica set oplog served through fake sessions.
type fakeOplog struct {
	mu       sync.Mutex
	entries  []fakeEntry
	majority pitr.Timestamp // zero: the newest entry
	rs, rsID string
	term     int64
	// failAfter makes the next ReadOplog fail after writing that many entries
	// (a crash in the middle of a chunk); -1 disables it.
	failAfter int
	failErr   error
	reads     int
}

func newFakeOplog() *fakeOplog {
	return &fakeOplog{rs: "rs0", rsID: "id1", term: 1, failAfter: -1}
}

// add appends n entries, one per second after the newest (starting at t=1000).
func (f *fakeOplog) add(t *testing.T, n int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	next := pitr.Timestamp{T: 1000, I: 1}
	if len(f.entries) > 0 {
		next = pitr.Timestamp{T: f.entries[len(f.entries)-1].op.TS.T + 1, I: 1}
	}
	for i := range n {
		ts := pitr.Timestamp{T: next.T + uint32(i), I: 1} //nolint:gosec // small test values
		doc, err := bson.Marshal(bson.D{{Key: "ts", Value: bson.Timestamp{T: ts.T, I: ts.I}}, {Key: "t", Value: f.term},
			{Key: "op", Value: "i"}, {Key: "ns", Value: "shop.items"}, {Key: "o", Value: bson.D{{Key: "_id", Value: int64(ts.T)}}}})
		if err != nil {
			t.Fatal(err)
		}
		f.entries = append(f.entries, fakeEntry{op: pitr.OpTime{TS: ts, Term: f.term}, doc: doc})
	}
}

// truncate drops the entries before ts (the oplog was capped).
func (f *fakeOplog) truncate(ts pitr.Timestamp) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.entries) > 1 && f.entries[0].op.TS.Compare(ts) < 0 {
		f.entries = f.entries[1:]
	}
}

// rewriteFrom replaces the entries at and after ts by entries of a new term (a
// rollback followed by writes in another history).
func (f *fakeOplog) rewriteFrom(t *testing.T, ts pitr.Timestamp, n int) {
	t.Helper()
	f.mu.Lock()
	keep := 0
	for keep < len(f.entries) && f.entries[keep].op.TS.Compare(ts) < 0 {
		keep++
	}
	f.entries = f.entries[:keep]
	f.term++
	f.mu.Unlock()
	f.add(t, n)
}

func (f *fakeOplog) newest() pitr.Timestamp {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entries[len(f.entries)-1].op.TS
}

// session is a fake Session over the oplog.
type session struct{ f *fakeOplog }

func (s session) Close() {}

func (s session) OplogWindow(context.Context) (pitr.OplogWindow, error) {
	f := s.f
	f.mu.Lock()
	defer f.mu.Unlock()
	w := pitr.OplogWindow{Oldest: f.entries[0].op.TS, Newest: f.entries[len(f.entries)-1].op.TS, ReplicaSet: f.rs, ReplicaSetID: f.rsID}
	w.MajorityOpTime = f.entries[len(f.entries)-1].op
	if !f.majority.IsZero() {
		for _, e := range f.entries {
			if e.op.TS == f.majority {
				w.MajorityOpTime = e.op
			}
		}
	}
	return w, nil
}

func (s session) ReadOplog(ctx context.Context, r pitr.OplogRange, w io.Writer) (pitr.OplogStats, error) {
	f := s.f
	f.mu.Lock()
	var sel []fakeEntry
	for _, e := range f.entries {
		if e.op.TS.Compare(r.From) >= 0 && e.op.TS.Compare(r.To) <= 0 {
			sel = append(sel, e)
		}
	}
	failAfter, failErr := f.failAfter, f.failErr
	f.failAfter = -1
	f.reads++
	f.mu.Unlock()
	var stats pitr.OplogStats
	if len(sel) == 0 || sel[0].op.TS != r.From {
		return stats, fmt.Errorf("%w: no entry at %s", pitr.ErrOplogGap, r.From)
	}
	if r.CheckTerm && sel[0].op.Term != r.FromTerm {
		return stats, fmt.Errorf("%w: term %d", pitr.ErrOplogGap, sel[0].op.Term)
	}
	if !r.StartInclusive {
		sel = sel[1:]
	}
	for _, e := range sel {
		if failAfter >= 0 && stats.Entries == failAfter {
			return stats, failErr
		}
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if _, err := w.Write(e.doc); err != nil {
			return stats, err
		}
		if stats.Entries == 0 {
			stats.First = e.op
		}
		stats.Last = e.op
		stats.Entries++
	}
	last := r.From
	if stats.Entries > 0 {
		last = stats.Last.TS
	}
	if last != r.To {
		return stats, pitr.ErrOplogBehind
	}
	return stats, nil
}

func (s session) EntryAt(_ context.Context, ts pitr.Timestamp) (int64, bool, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	for _, e := range s.f.entries {
		if e.op.TS == ts {
			return e.op.Term, true, nil
		}
	}
	return 0, false, nil
}

func (s session) EntryAtOrAfter(_ context.Context, ts pitr.Timestamp) (pitr.OpTime, bool, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	for _, e := range s.f.entries {
		if e.op.TS.Compare(ts) >= 0 {
			return e.op, true, nil
		}
	}
	return pitr.OpTime{}, false, nil
}

// fakeClock is a settable clock; After never fires.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// recorder captures published events.
type recorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recorder) Publish(_ context.Context, e events.Event) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return true
}

func (r *recorder) count(t events.EventType) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Type == t {
			n++
		}
	}
	return n
}

// fixture is a collector service over a fake oplog, a real store and mock storage.
type fixture struct {
	t       *testing.T
	f       *fakeOplog
	repo    *store.SQLiteStore
	storage *storage.MockStorage
	clock   *fakeClock
	events  *recorder
	svc     *Service
	stream  *pitr.Stream
	dec     *encryption.Decryptor
	bases   []string
	openErr error
	mu      sync.Mutex
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
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
	fx := &fixture{t: t, f: newFakeOplog(), repo: storetest.New(t), storage: storage.NewMockStorage(),
		clock: &fakeClock{now: time.Unix(2000, 0).UTC()}, events: &recorder{}, dec: dec}
	fx.f.add(t, 10)
	fx.stream = &pitr.Stream{ID: "str_a", ConnectionID: "conn_a", ReplicaSet: "rs0", TargetID: "tgt_a", Enabled: true,
		BaseCron: "@daily", BaseKeepCount: 7, BaseKeepDays: 14, ChunkSeconds: 60, BaseOnGap: true}
	if err := fx.repo.CreateStream(context.Background(), fx.stream); err != nil {
		t.Fatal(err)
	}
	fx.svc = New(Config{
		Repo: fx.repo,
		Open: func(context.Context, *pitr.Stream) (Session, error) {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			if fx.openErr != nil {
				return nil, fx.openErr
			}
			return session{fx.f}, nil
		},
		Storage:   func(context.Context, string) (storage.Storage, error) { return fx.storage, nil },
		Encryptor: func() *encryption.Encryptor { return enc },
		StartBase: func(_ context.Context, id string, _ models.BackupTrigger) (*models.BackupRecord, error) {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			fx.bases = append(fx.bases, id)
			return &models.BackupRecord{ID: "bkp_" + id}, nil
		},
		Publisher: fx.events,
		Logger:    slog.New(slog.DiscardHandler),
		Clock:     fx.clock,
	})
	return fx
}

// worker returns a fresh worker of the stream (a restarted collector).
func (fx *fixture) worker() *worker {
	st, err := fx.repo.GetStream(context.Background(), fx.stream.ID)
	if err != nil {
		fx.t.Fatal(err)
	}
	return newWorker(fx.svc, st)
}

// step runs one step that must succeed.
func (fx *fixture) step(w *worker) time.Duration {
	fx.t.Helper()
	d, err := w.step(context.Background())
	if err != nil {
		fx.t.Fatalf("step: %v", err)
	}
	return d
}

func (fx *fixture) state() *pitr.State {
	fx.t.Helper()
	st, err := fx.repo.LoadState(context.Background(), fx.stream.ID)
	if err != nil {
		fx.t.Fatal(err)
	}
	return st
}

func (fx *fixture) chunks(chain string) []*pitr.Chunk {
	fx.t.Helper()
	cs, err := fx.repo.ListChunks(context.Background(), pitr.ChunkQuery{StreamID: fx.stream.ID, ChainID: chain})
	if err != nil {
		fx.t.Fatal(err)
	}
	return cs
}

func (fx *fixture) chains() []*pitr.Chain {
	fx.t.Helper()
	cs, err := fx.repo.ListChains(context.Background(), fx.stream.ID)
	if err != nil {
		fx.t.Fatal(err)
	}
	return cs
}

// entries decrypts, gunzips and walks the object of c and returns its positions.
func (fx *fixture) entries(c *pitr.Chunk) []pitr.Timestamp {
	fx.t.Helper()
	r, err := fx.storage.Retrieve(context.Background(), c.StorageKey)
	if err != nil {
		fx.t.Fatalf("chunk object %s: %v", c.StorageKey, err)
	}
	defer r.Close()
	plain, err := fx.dec.Decrypt(r)
	if err != nil {
		fx.t.Fatal(err)
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		fx.t.Fatal(err)
	}
	var out []pitr.Timestamp
	rd := oplog.NewReader(gz)
	for {
		doc, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			fx.t.Fatal(err)
		}
		tt, ii, _ := doc.Lookup("ts").TimestampOK()
		out = append(out, pitr.Timestamp{T: tt, I: ii})
	}
}

// objects counts the stored chunk objects.
func (fx *fixture) objects() int {
	fx.t.Helper()
	objs, err := fx.storage.List(context.Background(), KeyPrefix)
	if err != nil {
		fx.t.Fatal(err)
	}
	return len(objs)
}

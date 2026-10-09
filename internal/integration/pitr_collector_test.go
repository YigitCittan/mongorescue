//go:build integration

package integration

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/oplog"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestPITRCollectorSurvivesARestart runs the collector against the replica set,
// stops it while writes go on and starts it again: the chunks of the chain follow
// each other and hold every insert exactly once.
func TestPITRCollectorSurvivesARestart(t *testing.T) {
	env := requireMongo(t)
	requireReplicaSet(t, env)
	db := env.uniqueDB(t, "pitrcol")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

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
	repo := storetest.New(t)
	prober := mongoconn.New()
	win, err := prober.OplogWindow(ctx, env.URI)
	if err != nil {
		t.Fatal(err)
	}
	stream := &pitr.Stream{ID: "str_it", ConnectionID: "conn_it", ReplicaSet: win.ReplicaSet, TargetID: "tgt_it", Enabled: true,
		BaseCron: "@daily", BaseKeepCount: 7, BaseKeepDays: 14, ChunkSeconds: 1}
	if err = repo.CreateStream(ctx, stream); err != nil {
		t.Fatal(err)
	}
	newService := func() *collector.Service {
		return collector.New(collector.Config{
			Repo: repo,
			Open: func(ctx context.Context, s *pitr.Stream) (collector.Session, error) {
				sess, openErr := prober.OpenOplogSession(ctx, env.URI, s.ReadPreference)
				if openErr != nil {
					return nil, openErr
				}
				return itSession{sess}, nil
			},
			Storage:           func(context.Context, string) (storage.Storage, error) { return st, nil },
			Encryptor:         func() *encryption.Encryptor { return enc },
			Logger:            slog.New(slog.DiscardHandler),
			ReconcileInterval: 100 * time.Millisecond,
		})
	}
	coll := env.Client.Database(db).Collection("events")
	inserted := 0
	insert := func(n int) {
		for range n {
			if _, insertErr := coll.InsertOne(ctx, bson.D{{Key: "n", Value: int32(inserted)}}); insertErr != nil { //nolint:gosec // a few dozen inserts
				t.Fatal(insertErr)
			}
			inserted++
		}
	}
	waitCovered := func() {
		t.Helper()
		target := mustNewest(ctx, t, prober, env.URI)
		deadline := time.Now().Add(time.Minute)
		for {
			if s, loadErr := repo.LoadState(ctx, stream.ID); loadErr == nil && s.Last.TS.Compare(target) >= 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the collector did not reach %s", target)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	first := newService()
	first.Start(ctx)
	deadline := time.Now().Add(time.Minute)
	for {
		if _, loadErr := repo.LoadState(ctx, stream.ID); loadErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the collector did not start a chain")
		}
		time.Sleep(50 * time.Millisecond)
	}
	insert(20)
	waitCovered()
	first.Stop() // killed between chunks or in the middle of one
	insert(20)
	second := newService()
	second.Start(ctx)
	insert(20)
	waitCovered()
	second.Stop()

	state, err := repo.LoadState(ctx, stream.ID)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := repo.ListChunks(ctx, pitr.ChunkQuery{StreamID: stream.ID, ChainID: state.ChainID})
	if err != nil {
		t.Fatal(err)
	}
	if chains, _ := repo.ListChains(ctx, stream.ID); len(chains) != 1 {
		t.Fatalf("%d chains: the restart broke the chain", len(chains))
	}
	ns := db + ".events"
	seen := map[int32]int{}
	var prevTS pitr.Timestamp
	for i, c := range chunks {
		if i > 0 && c.From != chunks[i-1].To {
			t.Fatalf("chunk %d starts at %s, the previous ends at %s", i, c.From, chunks[i-1].To)
		}
		for _, doc := range readChunk(ctx, t, st, dec, c) {
			tt, ii, _ := doc.Lookup("ts").TimestampOK()
			ts := pitr.Timestamp{T: tt, I: ii}
			if ts.Compare(prevTS) <= 0 {
				t.Fatalf("entry %s after %s: duplicate", ts, prevTS)
			}
			prevTS = ts
			if doc.Lookup("ns").StringValue() == ns && doc.Lookup("op").StringValue() == "i" {
				n, _ := doc.Lookup("o", "n").Int32OK()
				seen[n]++
			}
		}
	}
	if len(seen) != inserted {
		t.Fatalf("the chunks hold %d of %d inserts", len(seen), inserted)
	}
	for n, k := range seen {
		if k != 1 {
			t.Fatalf("insert %d appears %d times", n, k)
		}
	}
}

// mustNewest returns the newest majority-committed oplog position.
func mustNewest(ctx context.Context, t *testing.T, p *mongoconn.Prober, uri string) pitr.Timestamp {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		w, err := p.OplogWindow(ctx, uri)
		if err != nil {
			t.Fatal(err)
		}
		if w.MajorityOpTime.TS == w.Newest || time.Now().After(deadline) {
			return w.MajorityOpTime.TS
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// readChunk decrypts, gunzips and splits the object of c.
func readChunk(ctx context.Context, t *testing.T, st storage.Storage, dec *encryption.Decryptor, c *pitr.Chunk) []bson.Raw {
	t.Helper()
	r, err := st.Retrieve(ctx, c.StorageKey)
	if err != nil {
		t.Fatalf("chunk %s: %v", c.StorageKey, err)
	}
	defer r.Close()
	plain, err := dec.Decrypt(r)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		t.Fatal(err)
	}
	var out []bson.Raw
	rd := oplog.NewReader(gz)
	for {
		doc, err := rd.Next()
		if errors.Is(err, io.EOF) {
			if int64(len(out)) != c.Entries {
				t.Fatalf("chunk %s holds %d entries, recorded %d", c.ID, len(out), c.Entries)
			}
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, append(bson.Raw(nil), doc...))
	}
}

// itSession adapts *mongoconn.OplogSession to collector.Session, as internal/app
// does.
type itSession struct{ *mongoconn.OplogSession }

func (s itSession) Pin(ctx context.Context, notBefore pitr.Timestamp) (collector.Member, error) {
	m, err := s.OplogSession.Pin(ctx, notBefore)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s itSession) Primary() collector.Member { return s.OplogSession.Primary() }

// TestOplogSessionPinsOneMember checks the member readers the collector uses: a
// pinned member and the primary's majority reader agree on the window and on the
// entry at its newest position.
func TestOplogSessionPinsOneMember(t *testing.T) {
	env := requireMongo(t)
	requireReplicaSet(t, env)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := mongoconn.New().OpenOplogSession(ctx, env.URI, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	win := majorityWindow(ctx, t, s)
	m, err := s.Pin(ctx, win.MajorityOpTime.TS)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if m.Host() == "" {
		t.Fatal("the pinned reader names no member")
	}
	for name, r := range map[string]collector.Member{"pinned": m, "primary": s.Primary()} {
		w, err := r.OplogWindow(ctx)
		if err != nil {
			t.Fatalf("%s window: %v", name, err)
		}
		if w.ReplicaSet != win.ReplicaSet || w.Oldest.Compare(win.MajorityOpTime.TS) > 0 {
			t.Fatalf("%s window %+v, session window %+v", name, w, win)
		}
		term, found, err := r.EntryAt(ctx, win.MajorityOpTime.TS)
		if err != nil || !found || term != win.MajorityOpTime.Term {
			t.Fatalf("%s EntryAt(%s) = %d, %v, %v", name, win.MajorityOpTime.TS, term, found, err)
		}
	}
}

package copies_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/copies"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// chunkEnv is a PITR stream on tgt_a copying to tgt_b, with two committed chunks
// stored on tgt_a.
type chunkEnv struct {
	st     *store.SQLiteStore
	a, b   *storage.MockStorage
	svc    *copies.Service
	now    time.Time
	chunks []*pitr.Chunk
}

func newChunkEnv(t *testing.T) *chunkEnv {
	t.Helper()
	ctx := context.Background()
	env := &chunkEnv{st: storetest.New(t), a: storage.NewMockStorage(), b: storage.NewMockStorage(),
		now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	stream := &pitr.Stream{ID: "pst_a", ConnectionID: "conn_a", ReplicaSet: "rs0", TargetID: "tgt_a", Enabled: true,
		BaseCron: "0 3 * * *", BaseKeepCount: 7, ChunkSeconds: 60, CopyTargets: []string{"tgt_b"}}
	if err := env.st.CreateStream(ctx, stream); err != nil {
		t.Fatal(err)
	}
	if err := env.st.StartChain(ctx, "pst_a", "ch1", pitr.OpTime{TS: pitr.Timestamp{T: 100, I: 1}, Term: 1}, env.now); err != nil {
		t.Fatal(err)
	}
	from := pitr.Timestamp{T: 100, I: 1}
	for i, body := range [][]byte{bytes.Repeat([]byte("oplog-1 "), 512), bytes.Repeat([]byte("oplog-2 "), 256)} {
		to := pitr.Timestamp{T: from.T + 60, I: 1}
		k := "_mongorescue/oplog/conn_a/rs0/ch1/" + from.String() + "-" + to.String() + ".bson.gz.age"
		if _, err := env.a.Save(ctx, k, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		c := &pitr.Chunk{ID: "chk_" + string(rune('1'+i)), StreamID: "pst_a", ChainID: "ch1", TargetID: "tgt_a", StorageKey: k,
			From: from, To: to, FirstTerm: 1, LastTerm: 1, Entries: 3, SizeBytes: int64(len(body)), SHA256: sum(body),
			Encrypted: true, EncryptionMode: "x25519", CreatedAt: env.now}
		if err := env.st.CommitChunk(ctx, c); err != nil {
			t.Fatal(err)
		}
		env.chunks = append(env.chunks, c)
		from = to
	}
	env.svc = copies.New(copies.Config{
		Store: env.st,
		Storages: func(_ context.Context, id string) (storage.Storage, error) {
			switch id {
			case "tgt_a":
				return env.a, nil
			case "tgt_b":
				return env.b, nil
			}
			return nil, errors.New("unknown target")
		},
		Now: func() time.Time { return env.now },
	})
	return env
}

// copiesOf returns the copy of every chunk of env on tgt_b, by chunk ID.
func (env *chunkEnv) copiesOf(t *testing.T) map[string]*models.ChunkCopy {
	t.Helper()
	ids := []string{env.chunks[0].ID, env.chunks[1].ID}
	m, err := env.st.ChunkCopiesOf(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*models.ChunkCopy{}
	for id, list := range m {
		for _, c := range list {
			if c.TargetID == "tgt_b" {
				out[id] = c
			}
		}
	}
	return out
}

// TestChunksAreCopiedToTheStreamsCopyTargets proves that the queue copies every
// live chunk of a stream to its copy target, checked against the chunk's checksum,
// and that a corrupted primary chunk is not copied.
func TestChunksAreCopiedToTheStreamsCopyTargets(t *testing.T) {
	env := newChunkEnv(t)
	ctx := context.Background()
	// The second chunk's object is damaged on the primary.
	if _, err := env.a.Save(ctx, env.chunks[1].StorageKey, bytes.NewReader([]byte("damaged"))); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.RunDue(ctx); err == nil {
		t.Fatal("RunDue reported no error for the damaged chunk")
	}
	got := env.copiesOf(t)
	if c := got["chk_1"]; c == nil || c.Status != models.CopyDone || !c.SHA256OK || c.SHA256 != env.chunks[0].SHA256 {
		t.Fatalf("copy of chk_1 = %+v; want done", c)
	}
	rc, err := env.b.Retrieve(ctx, env.chunks[0].StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if sum(body) != env.chunks[0].SHA256 {
		t.Fatal("the copied chunk differs from the primary")
	}
	if c := got["chk_2"]; c == nil || c.Status != models.CopyFailed || c.NextAttemptAt == nil {
		t.Fatalf("copy of the damaged chk_2 = %+v; want failed with a retry", c)
	}
	if _, err = env.b.Stat(ctx, env.chunks[1].StorageKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the damaged chunk reached the copy target: %v", err)
	}
	if env.svc.QueueDepth() != 1 {
		t.Fatalf("queue depth = %d; want the failed copy", env.svc.QueueDepth())
	}
}

// TestChunkCopiesArePurgedWithTheirChunks proves that the copy of a pruned chunk
// is deleted from the copy target and marked purged.
func TestChunkCopiesArePurgedWithTheirChunks(t *testing.T) {
	env := newChunkEnv(t)
	ctx := context.Background()
	if err := env.svc.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := env.st.DeleteChunks(ctx, []string{"chk_1"}, env.now, env.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if c := env.copiesOf(t)["chk_1"]; c.Status != models.CopyDone {
		t.Fatalf("copy purged during the grace period: %+v", c)
	}
	env.now = env.now.Add(2 * time.Hour)
	if err := env.svc.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if c := env.copiesOf(t)["chk_1"]; c.Status != models.CopyPurged || c.PurgedAt == nil {
		t.Fatalf("copy after the grace period = %+v; want purged", c)
	}
	if _, err := env.b.Stat(ctx, env.chunks[0].StorageKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the purged copy is still stored: %v", err)
	}
	if c := env.copiesOf(t)["chk_2"]; c.Status != models.CopyDone {
		t.Fatalf("the live chunk's copy = %+v; want done", c)
	}
}

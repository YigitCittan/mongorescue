package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// newStream returns a valid stream of connection conn.
func newStream(id, conn string) *pitr.Stream {
	return &pitr.Stream{
		ID: id, ConnectionID: conn, ReplicaSet: "rs0", TargetID: "tgt_local", Enabled: true,
		BaseCron: "0 3 * * *", BaseKeepCount: 7, BaseKeepDays: 14, ChunkSeconds: 60, BaseOnGap: true,
	}
}

func testPITRStreams(t *testing.T, s backend) {
	ctx := context.Background()
	if err := s.CreateStream(ctx, &pitr.Stream{ID: "pst_x"}); !errors.Is(err, store.ErrInvalidRecord) {
		t.Errorf("CreateStream without a connection = %v; want ErrInvalidRecord", err)
	}
	a := newStream("pst_a", "conn_a")
	a.ReadPreference = "secondary"
	a.OplogMaxDays = 30
	if err := s.CreateStream(ctx, a); err != nil {
		t.Fatalf("CreateStream: %v", err)
	}
	if a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() {
		t.Fatalf("CreateStream must set CreatedAt and UpdatedAt: %+v", a)
	}
	if err := s.CreateStream(ctx, newStream("pst_a", "conn_z")); !errors.Is(err, pitr.ErrAlreadyExists) {
		t.Errorf("CreateStream with a taken ID = %v; want ErrAlreadyExists", err)
	}
	if err := s.CreateStream(ctx, newStream("pst_b", "conn_a")); !errors.Is(err, pitr.ErrConnectionTaken) {
		t.Errorf("CreateStream on a connection with a stream = %v; want ErrConnectionTaken", err)
	}
	if err := s.CreateStream(ctx, newStream("pst_b", "conn_b")); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetStream(ctx, "pst_a")
	if err != nil {
		t.Fatalf("GetStream: %v", err)
	}
	if *got != *a {
		t.Errorf("GetStream = %+v; want %+v", got, a)
	}
	if byConn, connErr := s.GetStreamByConnection(ctx, "conn_b"); connErr != nil || byConn.ID != "pst_b" {
		t.Errorf("GetStreamByConnection = %+v, %v; want pst_b", byConn, connErr)
	}
	for _, get := range []func() (*pitr.Stream, error){
		func() (*pitr.Stream, error) { return s.GetStream(ctx, "pst_none") },
		func() (*pitr.Stream, error) { return s.GetStreamByConnection(ctx, "conn_none") },
	} {
		if _, err = get(); !errors.Is(err, pitr.ErrNotFound) {
			t.Errorf("get of an unknown stream = %v; want ErrNotFound", err)
		}
	}
	list, err := s.ListStreams(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "pst_a" || list[1].ID != "pst_b" {
		t.Fatalf("ListStreams = %+v, %v; want pst_a, pst_b", list, err)
	}

	upd := *a
	upd.CreatedAt = time.Time{}
	upd.Enabled, upd.ChunkSeconds, upd.BaseOnGap, upd.ReadPreference = false, 120, false, ""
	if err = s.UpdateStream(ctx, &upd); err != nil {
		t.Fatalf("UpdateStream: %v", err)
	}
	if !upd.CreatedAt.Equal(a.CreatedAt) {
		t.Errorf("UpdateStream CreatedAt = %v; want the stored %v", upd.CreatedAt, a.CreatedAt)
	}
	if got, err = s.GetStream(ctx, "pst_a"); err != nil || *got != upd {
		t.Errorf("GetStream after update = %+v, %v; want %+v", got, err, upd)
	}
	upd.ConnectionID = "conn_b"
	if err = s.UpdateStream(ctx, &upd); !errors.Is(err, pitr.ErrConnectionTaken) {
		t.Errorf("UpdateStream to a connection with a stream = %v; want ErrConnectionTaken", err)
	}
	if err = s.UpdateStream(ctx, newStream("pst_none", "conn_none")); !errors.Is(err, pitr.ErrNotFound) {
		t.Errorf("UpdateStream of an unknown stream = %v; want ErrNotFound", err)
	}

	if err = s.DeleteStream(ctx, "pst_b"); err != nil {
		t.Fatalf("DeleteStream: %v", err)
	}
	if err = s.DeleteStream(ctx, "pst_b"); !errors.Is(err, pitr.ErrNotFound) {
		t.Errorf("DeleteStream twice = %v; want ErrNotFound", err)
	}
	// The connection is free again.
	if err = s.CreateStream(ctx, newStream("pst_c", "conn_b")); err != nil {
		t.Errorf("CreateStream on a freed connection: %v", err)
	}
}

// pos returns the timestamp (sec, ord).
func pos(sec, ord uint32) pitr.Timestamp { return pitr.Timestamp{T: sec, I: ord} }

// chunk returns a chunk of chain chain covering (from, to].
func chunk(id, chain string, from, to pitr.Timestamp, term int64) *pitr.Chunk {
	return &pitr.Chunk{
		ID: id, StreamID: "pst_a", ChainID: chain, TargetID: "tgt_local",
		StorageKey: fmt.Sprintf("_mongorescue/oplog/conn_a/rs0/%s/%s-%s.bson.gz.age", chain, from, to),
		From:       from, To: to, FirstTerm: term, LastTerm: term, Entries: 10, SizeBytes: 512,
		SHA256: "ab12", Encrypted: true, EncryptionMode: "x25519",
	}
}

func testPITRChainsAndChunks(t *testing.T, s backend) {
	ctx := context.Background()
	at := ts(0)
	if err := s.StartChain(ctx, "pst_a", "ch1", pitr.OpTime{TS: pos(100, 1), Term: 1}, at); !errors.Is(err, pitr.ErrNotFound) {
		t.Errorf("StartChain of an unknown stream = %v; want ErrNotFound", err)
	}
	if err := s.CreateStream(ctx, newStream("pst_a", "conn_a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadState(ctx, "pst_a"); !errors.Is(err, pitr.ErrNotFound) {
		t.Errorf("LoadState before the first chain = %v; want ErrNotFound", err)
	}
	if err := s.CommitChunk(ctx, chunk("chk_0", "ch1", pos(100, 1), pos(160, 1), 1)); !errors.Is(err, pitr.ErrNotFound) {
		t.Errorf("CommitChunk without a state = %v; want ErrNotFound", err)
	}
	if err := s.StartChain(ctx, "pst_a", "ch1", pitr.OpTime{TS: pos(100, 1), Term: 1}, at); err != nil {
		t.Fatalf("StartChain: %v", err)
	}
	if err := s.StartChain(ctx, "pst_a", "ch2", pitr.OpTime{TS: pos(100, 1), Term: 1}, at); !errors.Is(err, pitr.ErrChainOpen) {
		t.Errorf("StartChain with an open chain = %v; want ErrChainOpen", err)
	}
	st, err := s.LoadState(ctx, "pst_a")
	if err != nil || st.ChainID != "ch1" || st.Last != (pitr.OpTime{TS: pos(100, 1), Term: 1}) ||
		st.Status != pitr.CollectorRunning || !st.UpdatedAt.Equal(at) {
		t.Fatalf("LoadState after StartChain = %+v, %v", st, err)
	}

	// Chunks must continue the stored position exactly.
	for _, c := range []*pitr.Chunk{
		chunk("chk_gap", "ch1", pos(101, 0), pos(160, 1), 1),
		chunk("chk_overlap", "ch1", pos(99, 0), pos(160, 1), 1),
	} {
		if err = s.CommitChunk(ctx, c); !errors.Is(err, pitr.ErrDiscontinuous) {
			t.Errorf("CommitChunk %s = %v; want ErrDiscontinuous", c.ID, err)
		}
	}
	if err = s.CommitChunk(ctx, chunk("chk_back", "ch1", pos(100, 1), pos(90, 0), 1)); !errors.Is(err, store.ErrInvalidRecord) {
		t.Errorf("CommitChunk ending before its start = %v; want ErrInvalidRecord", err)
	}
	if err = s.CommitChunk(ctx, chunk("chk_other", "ch9", pos(100, 1), pos(160, 1), 1)); !errors.Is(err, pitr.ErrChainEnded) {
		t.Errorf("CommitChunk on another chain = %v; want ErrChainEnded", err)
	}

	c1 := chunk("chk_1", "ch1", pos(100, 1), pos(160, 4), 1)
	c1.CreatedAt = ts(1)
	if err = s.CommitChunk(ctx, c1); err != nil {
		t.Fatalf("CommitChunk: %v", err)
	}
	if c1.Status != pitr.ChunkCommitted {
		t.Errorf("CommitChunk status = %q; want committed", c1.Status)
	}
	c2 := chunk("chk_2", "ch1", pos(160, 4), pos(220, 2), 2)
	c2.FirstTerm = 1
	if err = s.CommitChunk(ctx, c2); err != nil {
		t.Fatal(err)
	}
	empty := chunk("chk_3", "ch1", pos(220, 2), pos(220, 2), 2)
	empty.Entries = 0
	if err = s.CommitChunk(ctx, empty); err != nil {
		t.Fatalf("CommitChunk of an empty range: %v", err)
	}
	st, err = s.LoadState(ctx, "pst_a")
	if err != nil || st.Last != (pitr.OpTime{TS: pos(220, 2), Term: 2}) || st.ChainID != "ch1" {
		t.Fatalf("LoadState after commits = %+v, %v; want (220,2) term 2", st, err)
	}

	// A failed commit changes neither the chunks nor the state.
	dup := chunk("chk_4", "ch1", pos(220, 2), pos(280, 1), 2)
	dup.StorageKey = c1.StorageKey
	if err = s.CommitChunk(ctx, dup); !errors.Is(err, pitr.ErrAlreadyExists) {
		t.Errorf("CommitChunk with a recorded storage key = %v; want ErrAlreadyExists", err)
	}
	if after, loadErr := s.LoadState(ctx, "pst_a"); loadErr != nil || after.Last != st.Last {
		t.Errorf("state after a failed commit = %+v, %v; want %+v", after, loadErr, st.Last)
	}

	ids := func(q pitr.ChunkQuery) string {
		t.Helper()
		q.StreamID, q.ChainID = "pst_a", "ch1"
		list, listErr := s.ListChunks(ctx, q)
		if listErr != nil {
			t.Fatalf("ListChunks(%+v): %v", q, listErr)
		}
		out := ""
		for _, c := range list {
			out += c.ID + " "
		}
		return out
	}
	for _, tc := range []struct {
		q    pitr.ChunkQuery
		want string
	}{
		{pitr.ChunkQuery{}, "chk_1 chk_2 chk_3 "},
		{pitr.ChunkQuery{After: pos(160, 4)}, "chk_2 chk_3 "},
		{pitr.ChunkQuery{After: pos(160, 3)}, "chk_1 chk_2 chk_3 "},
		{pitr.ChunkQuery{Until: pos(160, 4)}, "chk_1 "},
		{pitr.ChunkQuery{After: pos(150, 0), Until: pos(200, 0)}, "chk_1 chk_2 "},
		{pitr.ChunkQuery{After: pos(220, 2)}, ""},
	} {
		if got := ids(tc.q); got != tc.want {
			t.Errorf("ListChunks(after %s, until %s) = %q; want %q", tc.q.After, tc.q.Until, got, tc.want)
		}
	}
	list, err := s.ListChunks(ctx, pitr.ChunkQuery{StreamID: "pst_a", ChainID: "ch1", Until: pos(160, 4)})
	if err != nil || len(list) != 1 {
		t.Fatalf("ListChunks = %+v, %v", list, err)
	}
	if got := list[0]; *got != *c1 {
		t.Errorf("stored chunk = %+v; want %+v", got, c1)
	}
	if _, err = s.ListChunks(ctx, pitr.ChunkQuery{StreamID: "pst_a"}); !errors.Is(err, store.ErrInvalidRecord) {
		t.Errorf("ListChunks without a chain = %v; want ErrInvalidRecord", err)
	}

	// Divergence: the chain ends, later chunks are superseded, a new chain starts.
	n, err := s.SupersedeChunks(ctx, "pst_a", "ch1", pos(160, 4))
	if err != nil || n != 2 {
		t.Errorf("SupersedeChunks = %d, %v; want 2", n, err)
	}
	if n, err = s.SupersedeChunks(ctx, "pst_a", "ch1", pos(160, 4)); err != nil || n != 0 {
		t.Errorf("SupersedeChunks again = %d, %v; want 0", n, err)
	}
	if got := ids(pitr.ChunkQuery{Status: pitr.ChunkCommitted}); got != "chk_1 " {
		t.Errorf("committed chunks = %q; want chk_1", got)
	}
	if got := ids(pitr.ChunkQuery{Status: pitr.ChunkSuperseded}); got != "chk_2 chk_3 " {
		t.Errorf("superseded chunks = %q; want chk_2 chk_3", got)
	}
	if err = s.EndChain(ctx, "pst_a", "ch1", pos(160, 4), "", ts(2)); !errors.Is(err, store.ErrInvalidRecord) {
		t.Errorf("EndChain without a reason = %v; want ErrInvalidRecord", err)
	}
	if err = s.EndChain(ctx, "pst_a", "ch9", pos(160, 4), pitr.EndDiverged, ts(2)); !errors.Is(err, pitr.ErrNotFound) {
		t.Errorf("EndChain of an unknown chain = %v; want ErrNotFound", err)
	}
	if err = s.EndChain(ctx, "pst_a", "ch1", pos(160, 4), pitr.EndDiverged, ts(2)); err != nil {
		t.Fatalf("EndChain: %v", err)
	}
	if err = s.EndChain(ctx, "pst_a", "ch1", pos(160, 4), pitr.EndDiverged, ts(2)); !errors.Is(err, pitr.ErrChainEnded) {
		t.Errorf("EndChain twice = %v; want ErrChainEnded", err)
	}
	if err = s.CommitChunk(ctx, chunk("chk_5", "ch1", pos(220, 2), pos(280, 1), 2)); !errors.Is(err, pitr.ErrChainEnded) {
		t.Errorf("CommitChunk on an ended chain = %v; want ErrChainEnded", err)
	}
	if err = s.StartChain(ctx, "pst_a", "ch1", pitr.OpTime{TS: pos(300, 1), Term: 3}, ts(3)); !errors.Is(err, pitr.ErrAlreadyExists) {
		t.Errorf("StartChain with a taken chain ID = %v; want ErrAlreadyExists", err)
	}
	if err = s.SetCollectorStatus(ctx, "pst_a", pitr.CollectorFailed, "diverged", nil, ts(3)); err != nil {
		t.Fatal(err)
	}
	if err = s.StartChain(ctx, "pst_a", "ch2", pitr.OpTime{TS: pos(300, 1), Term: 3}, ts(3)); err != nil {
		t.Fatalf("StartChain after the end: %v", err)
	}
	if err = s.CommitChunk(ctx, chunk("chk_6", "ch2", pos(300, 1), pos(360, 1), 3)); err != nil {
		t.Fatalf("CommitChunk on the new chain: %v", err)
	}
	st, err = s.LoadState(ctx, "pst_a")
	if err != nil || st.ChainID != "ch2" || st.Last != (pitr.OpTime{TS: pos(360, 1), Term: 3}) ||
		st.Status != pitr.CollectorRunning || st.LastError != "" {
		t.Errorf("state on the new chain = %+v, %v", st, err)
	}

	chains, err := s.ListChains(ctx, "pst_a")
	if err != nil || len(chains) != 2 {
		t.Fatalf("ListChains = %+v, %v", chains, err)
	}
	ended := ts(2)
	if c := chains[0]; c.ChainID != "ch1" || c.Start != pos(100, 1) || c.End != pos(160, 4) || c.EndReason != pitr.EndDiverged ||
		c.EndedAt == nil || !c.EndedAt.Equal(ended) || c.Open() {
		t.Errorf("ended chain = %+v", c)
	}
	if c := chains[1]; c.ChainID != "ch2" || c.Start != pos(300, 1) || !c.Open() || c.EndReason != "" || !c.End.IsZero() {
		t.Errorf("open chain = %+v", c)
	}

	// Collector status changes keep the position.
	lag := ts(4)
	if err = s.SetCollectorStatus(ctx, "pst_a", pitr.CollectorFailed, "server selection timeout", &lag, ts(5)); err != nil {
		t.Fatalf("SetCollectorStatus: %v", err)
	}
	st, err = s.LoadState(ctx, "pst_a")
	if err != nil || st.Status != pitr.CollectorFailed || st.LastError != "server selection timeout" ||
		st.LagSince == nil || !st.LagSince.Equal(lag) || !st.UpdatedAt.Equal(ts(5)) || st.Last.TS != pos(360, 1) {
		t.Errorf("state after SetCollectorStatus = %+v, %v", st, err)
	}
	if err = s.SetCollectorStatus(ctx, "pst_none", pitr.CollectorRunning, "", nil, ts(5)); !errors.Is(err, pitr.ErrNotFound) {
		t.Errorf("SetCollectorStatus of an unknown stream = %v; want ErrNotFound", err)
	}

	// A stream with chunks in storage cannot be deleted.
	if err = s.DeleteStream(ctx, "pst_a"); !errors.Is(err, pitr.ErrInUse) {
		t.Errorf("DeleteStream with chunks = %v; want ErrInUse", err)
	}
	if _, err = s.GetStream(ctx, "pst_a"); err != nil {
		t.Errorf("a refused delete removed the stream: %v", err)
	}
}

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestOplogChunkRetentionAndVerification(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	at := time.Unix(1_800_000_000, 0).UTC()
	if err := s.CreateStream(ctx, newStream("pst_a", "conn_a")); err != nil {
		t.Fatal(err)
	}
	if err := s.StartChain(ctx, "pst_a", "ch1", pitr.OpTime{TS: pos(100, 1), Term: 1}, at); err != nil {
		t.Fatal(err)
	}
	for i, c := range []*pitr.Chunk{
		chunk("c1", "ch1", pos(100, 1), pos(160, 1), 1),
		chunk("c2", "ch1", pos(160, 1), pos(220, 1), 1),
		chunk("c3", "ch1", pos(220, 1), pos(280, 1), 1),
	} {
		c.CreatedAt = at.Add(time.Duration(i) * time.Minute)
		if err := s.CommitChunk(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	spans, err := s.ChainSpans(ctx, "pst_a")
	if err != nil || len(spans) != 1 || spans[0].From != pos(100, 1) || spans[0].To != pos(280, 1) || spans[0].Chunks != 3 || spans[0].SizeBytes != 1536 {
		t.Fatalf("spans = %+v, %v", spans, err)
	}
	page, total, err := s.ListStreamChunks(ctx, "pst_a", 2, 0)
	if err != nil || total != 3 || len(page) != 2 || page[0].ID != "c3" {
		t.Fatalf("page = %v, %d, %v", page, total, err)
	}

	toVerify, err := s.ListChunksToVerify(ctx, at.Add(time.Hour), 10)
	if err != nil || len(toVerify) != 3 {
		t.Fatalf("to verify = %d, %v", len(toVerify), err)
	}
	if err = s.MarkChunkVerified(ctx, "c1", at.Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkChunkVerified(ctx, "c_missing", at, ""); !errors.Is(err, pitr.ErrNotFound) {
		t.Fatalf("verify unknown chunk: %v", err)
	}
	if toVerify, _ = s.ListChunksToVerify(ctx, at.Add(time.Hour), 10); len(toVerify) != 2 || toVerify[0].ID != "c2" {
		t.Fatalf("to verify after c1 = %+v", toVerify)
	}

	purgeAfter := at.Add(7 * 24 * time.Hour)
	if n, delErr := s.DeleteChunks(ctx, []string{"c1", "c2", "c1"}, at, purgeAfter); delErr != nil || n != 2 {
		t.Fatalf("DeleteChunks = %d, %v", n, delErr)
	}
	if spans, _ = s.ChainSpans(ctx, "pst_a"); spans[0].From != pos(220, 1) || spans[0].Chunks != 1 {
		t.Fatalf("spans after the delete = %+v", spans)
	}
	if due, _ := s.ListPurgeableChunks(ctx, "pst_a", purgeAfter.Add(-time.Second), 10); len(due) != 0 {
		t.Fatalf("chunks due before their grace ends: %d", len(due))
	}
	due, err := s.ListPurgeableChunks(ctx, "pst_a", purgeAfter, 10)
	if err != nil || len(due) != 2 {
		t.Fatalf("due = %d, %v", len(due), err)
	}
	if err := s.MarkChunkPruned(ctx, "c3"); !errors.Is(err, pitr.ErrNotFound) {
		t.Fatalf("pruning a live chunk: %v", err)
	}
	for _, c := range due {
		if err := s.MarkChunkPruned(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	if due, _ = s.ListPurgeableChunks(ctx, "pst_a", purgeAfter, 10); len(due) != 0 {
		t.Fatalf("pruned chunks still due: %d", len(due))
	}
	if live, _ := s.ListChunks(ctx, pitr.ChunkQuery{StreamID: "pst_a", ChainID: "ch1", Live: true}); len(live) != 1 {
		t.Fatalf("live chunks = %d", len(live))
	}
}

func TestListBaseBackups(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	now := time.Now().UTC()
	for _, r := range []*models.BackupRecord{
		{ID: "b_old", Scope: models.ScopeInstance, PITRStreamID: "pst_a", Status: models.StatusCompleted, StartedAt: now.Add(-2 * time.Hour)},
		{ID: "b_new", Scope: models.ScopeInstance, PITRStreamID: "pst_a", Status: models.StatusCompleted, StartedAt: now.Add(-time.Hour)},
		{ID: "b_other", Scope: models.ScopeInstance, PITRStreamID: "pst_b", Status: models.StatusCompleted, StartedAt: now},
		{ID: "b_db", Database: "shop", Status: models.StatusCompleted, StartedAt: now},
	} {
		if err := s.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	bases, err := s.ListBaseBackups(ctx, "pst_a")
	if err != nil || len(bases) != 2 || bases[0].ID != "b_new" || bases[1].ID != "b_old" {
		t.Fatalf("bases = %v, %v", bases, err)
	}
}

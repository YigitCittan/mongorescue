package pitr_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// planRepo serves the chains and chunks PlanRestore reads; other methods panic.
type planRepo struct {
	pitr.Repository
	chains []*pitr.Chain
	chunks map[string][]*pitr.Chunk
}

func (r *planRepo) ListChains(context.Context, string) ([]*pitr.Chain, error) { return r.chains, nil }

func (r *planRepo) ListChunks(_ context.Context, q pitr.ChunkQuery) ([]*pitr.Chunk, error) {
	return r.chunks[q.ChainID], nil
}

func ts(t uint32) pitr.Timestamp { return pitr.Timestamp{T: t, I: 1} }

// chain returns chunks of chainID over consecutive bounds, every one verified.
func chain(chainID string, bounds ...uint32) []*pitr.Chunk {
	var out []*pitr.Chunk
	now := time.Now()
	for i := 1; i < len(bounds); i++ {
		out = append(out, &pitr.Chunk{
			ID: chainID + "-" + ts(bounds[i]).String(), ChainID: chainID, From: ts(bounds[i-1]), To: ts(bounds[i]),
			FirstTerm: 1, LastTerm: 1, SizeBytes: 10, SHA256: "ab", Encrypted: true, EncryptionMode: "x25519",
			Status: pitr.ChunkCommitted, VerifiedAt: &now,
		})
	}
	return out
}

func base(id string, before, after uint32) pitr.Base {
	return pitr.Base{ID: id, TBefore: pitr.OpTime{TS: ts(before), Term: 1}, TAfter: pitr.OpTime{TS: ts(after), Term: 1},
		SizeBytes: 1000, Encrypted: true, EncryptionMode: "x25519"}
}

func anyKey(string) bool { return true }

func at(sec uint32) pitr.Target { return pitr.Target{At: time.Unix(int64(sec), 0)} }

func TestTargetLimit(t *testing.T) {
	l, err := at(100).Limit()
	if err != nil || l != (pitr.Timestamp{T: 101}) {
		t.Fatalf("At limit = %v, %v; want 101:0", l, err)
	}
	exact := pitr.Timestamp{T: 100, I: 7}
	if l, err = (pitr.Target{TS: &exact}).Limit(); err != nil || l != exact {
		t.Fatalf("TS limit = %v, %v; want the timestamp", l, err)
	}
	for name, tgt := range map[string]pitr.Target{
		"neither": {},
		"both":    {At: time.Unix(5, 0), TS: &exact},
		"zero ts": {TS: &pitr.Timestamp{}},
		"before":  {At: time.Unix(-5, 0)},
	} {
		if _, err := tgt.Limit(); !errors.Is(err, pitr.ErrInvalidTarget) {
			t.Errorf("%s: err = %v, want ErrInvalidTarget", name, err)
		}
	}
}

func TestPlanRestore(t *testing.T) {
	st := &pitr.Stream{ID: "s1"}
	repo := &planRepo{
		chains: []*pitr.Chain{{StreamID: "s1", ChainID: "c1"}},
		chunks: map[string][]*pitr.Chunk{"c1": chain("c1", 100, 110, 120, 130, 140, 150)},
	}
	src := pitr.PlanSource{Repo: repo, Bases: []pitr.Base{base("b1", 105, 108), base("b2", 125, 128)}, HasKey: anyKey}

	t.Run("newest base before the target", func(t *testing.T) {
		plan, err := pitr.PlanRestore(context.Background(), st, src, at(135))
		if err != nil {
			t.Fatal(err)
		}
		if plan.Base.ID != "b2" || plan.ChainID != "c1" || plan.Limit != (pitr.Timestamp{T: 136}) {
			t.Fatalf("plan = %+v", plan)
		}
		// From the chunk holding T_before (120-130) to the one reaching the limit (130-140).
		if plan.ChunkCount != 2 || plan.Chunks[0].From != ts(120) || plan.Chunks[1].To != ts(140) || plan.OplogBytes != 20 {
			t.Fatalf("chunks = %d (%v..%v), bytes %d", plan.ChunkCount, plan.Chunks[0].From, plan.Chunks[len(plan.Chunks)-1].To, plan.OplogBytes)
		}
		if !plan.TargetTime.Equal(time.Unix(135, 0)) {
			t.Fatalf("target time = %v", plan.TargetTime)
		}
	})
	t.Run("older base when the newest is after the target", func(t *testing.T) {
		plan, err := pitr.PlanRestore(context.Background(), st, src, at(126))
		if err != nil {
			t.Fatal(err)
		}
		if plan.Base.ID != "b1" || plan.ChunkCount != 3 {
			t.Fatalf("base %s with %d chunks, want b1 with 3", plan.Base.ID, plan.ChunkCount)
		}
	})
	t.Run("exact timestamp", func(t *testing.T) {
		exact := pitr.Timestamp{T: 140, I: 1}
		plan, err := pitr.PlanRestore(context.Background(), st, src, pitr.Target{TS: &exact})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Limit != exact || plan.Chunks[len(plan.Chunks)-1].To != exact {
			t.Fatalf("limit %v, last chunk to %v", plan.Limit, plan.Chunks[len(plan.Chunks)-1].To)
		}
	})
	t.Run("before every base", func(t *testing.T) {
		if _, err := pitr.PlanRestore(context.Background(), st, src, at(107)); !errors.Is(err, pitr.ErrOutsideWindow) {
			t.Fatalf("err = %v, want ErrOutsideWindow", err)
		}
	})
	t.Run("after the newest chunk", func(t *testing.T) {
		// 150:1 is the newest entry; (150+1):0 is past it.
		if _, err := pitr.PlanRestore(context.Background(), st, src, at(150)); !errors.Is(err, pitr.ErrOutsideWindow) {
			t.Fatalf("err = %v, want ErrOutsideWindow", err)
		}
	})
	t.Run("invalid target", func(t *testing.T) {
		if _, err := pitr.PlanRestore(context.Background(), st, src, pitr.Target{}); !errors.Is(err, pitr.ErrInvalidTarget) {
			t.Fatalf("err = %v, want ErrInvalidTarget", err)
		}
	})
}

func TestPlanRestoreRefusals(t *testing.T) {
	st := &pitr.Stream{ID: "s1"}
	ctx := context.Background()

	t.Run("chain break in the range", func(t *testing.T) {
		chunks := chain("c1", 100, 110, 120)
		chunks = append(chunks, chain("c1", 125, 140)...) // 120 -> 125 is missing
		repo := &planRepo{chains: []*pitr.Chain{{ChainID: "c1"}}, chunks: map[string][]*pitr.Chunk{"c1": chunks}}
		src := pitr.PlanSource{Repo: repo, Bases: []pitr.Base{base("b1", 105, 108)}, HasKey: anyKey}
		if _, err := pitr.PlanRestore(ctx, st, src, at(130)); !errors.Is(err, pitr.ErrChainBreak) {
			t.Fatalf("err = %v, want ErrChainBreak", err)
		}
	})
	t.Run("gap between chains", func(t *testing.T) {
		repo := &planRepo{
			chains: []*pitr.Chain{{ChainID: "c1"}, {ChainID: "c2"}},
			chunks: map[string][]*pitr.Chunk{"c1": chain("c1", 100, 110, 120), "c2": chain("c2", 130, 140)},
		}
		src := pitr.PlanSource{Repo: repo, Bases: []pitr.Base{base("b1", 105, 108)}, HasKey: anyKey}
		if _, err := pitr.PlanRestore(ctx, st, src, at(135)); !errors.Is(err, pitr.ErrChainBreak) {
			t.Fatalf("err = %v, want ErrChainBreak", err)
		}
	})
	t.Run("base in the newer chain", func(t *testing.T) {
		repo := &planRepo{
			chains: []*pitr.Chain{{ChainID: "c1"}, {ChainID: "c2"}},
			chunks: map[string][]*pitr.Chunk{"c1": chain("c1", 100, 110, 120), "c2": chain("c2", 130, 140, 150)},
		}
		src := pitr.PlanSource{Repo: repo, Bases: []pitr.Base{base("b1", 105, 108), base("b2", 135, 138)}, HasKey: anyKey}
		plan, err := pitr.PlanRestore(ctx, st, src, at(145))
		if err != nil || plan.ChainID != "c2" || plan.Base.ID != "b2" {
			t.Fatalf("plan = %+v, err = %v; want b2 on c2", plan, err)
		}
	})
	t.Run("failed chunk", func(t *testing.T) {
		chunks := chain("c1", 100, 110, 120, 130)
		chunks[1].VerifyError = "sha256 mismatch"
		repo := &planRepo{chains: []*pitr.Chain{{ChainID: "c1"}}, chunks: map[string][]*pitr.Chunk{"c1": chunks}}
		src := pitr.PlanSource{Repo: repo, Bases: []pitr.Base{base("b1", 105, 108)}, HasKey: anyKey}
		if _, err := pitr.PlanRestore(ctx, st, src, at(125)); !errors.Is(err, pitr.ErrChunkFailed) {
			t.Fatalf("err = %v, want ErrChunkFailed", err)
		}
	})
	t.Run("chunk without a checksum", func(t *testing.T) {
		chunks := chain("c1", 100, 110, 120)
		chunks[1].SHA256 = ""
		repo := &planRepo{chains: []*pitr.Chain{{ChainID: "c1"}}, chunks: map[string][]*pitr.Chunk{"c1": chunks}}
		src := pitr.PlanSource{Repo: repo, Bases: []pitr.Base{base("b1", 105, 108)}, HasKey: anyKey}
		if _, err := pitr.PlanRestore(ctx, st, src, at(115)); !errors.Is(err, pitr.ErrChunkFailed) {
			t.Fatalf("err = %v, want ErrChunkFailed", err)
		}
	})
	t.Run("unverified chunks are accepted", func(t *testing.T) {
		chunks := chain("c1", 100, 110, 120)
		chunks[1].VerifiedAt = nil
		repo := &planRepo{chains: []*pitr.Chain{{ChainID: "c1"}}, chunks: map[string][]*pitr.Chunk{"c1": chunks}}
		src := pitr.PlanSource{Repo: repo, Bases: []pitr.Base{base("b1", 105, 108)}, HasKey: anyKey}
		plan, err := pitr.PlanRestore(ctx, st, src, at(115))
		if err != nil || plan.UnverifiedChunks != 1 {
			t.Fatalf("plan = %+v, err = %v; want one unverified chunk", plan, err)
		}
	})
	t.Run("missing key", func(t *testing.T) {
		chunks := chain("c1", 100, 110, 120)
		chunks[1].EncryptionMode = "scrypt"
		repo := &planRepo{chains: []*pitr.Chain{{ChainID: "c1"}}, chunks: map[string][]*pitr.Chunk{"c1": chunks}}
		src := pitr.PlanSource{Repo: repo, Bases: []pitr.Base{base("b1", 105, 108)}, HasKey: func(m string) bool { return m == "x25519" }}
		if _, err := pitr.PlanRestore(ctx, st, src, at(115)); !errors.Is(err, pitr.ErrKeyMissing) {
			t.Fatalf("err = %v, want ErrKeyMissing", err)
		}
		src.HasKey = nil
		if _, err := pitr.PlanRestore(ctx, st, src, at(115)); !errors.Is(err, pitr.ErrKeyMissing) {
			t.Fatalf("no keys: err = %v, want ErrKeyMissing", err)
		}
	})
	t.Run("consistent point rolled back", func(t *testing.T) {
		chunks := chain("c1", 100, 110, 120)
		b := base("b1", 105, 108)
		b.TAfter.Term = 7
		repo := &planRepo{chains: []*pitr.Chain{{ChainID: "c1"}}, chunks: map[string][]*pitr.Chunk{"c1": chunks}}
		src := pitr.PlanSource{Repo: repo, Bases: []pitr.Base{b}, HasKey: anyKey}
		if _, err := pitr.PlanRestore(ctx, st, src, at(115)); !errors.Is(err, pitr.ErrChainBreak) {
			t.Fatalf("err = %v, want ErrChainBreak", err)
		}
	})
	t.Run("no chunks", func(t *testing.T) {
		repo := &planRepo{chains: nil}
		src := pitr.PlanSource{Repo: repo, Bases: []pitr.Base{base("b1", 105, 108)}, HasKey: anyKey}
		if _, err := pitr.PlanRestore(ctx, st, src, at(115)); !errors.Is(err, pitr.ErrOutsideWindow) {
			t.Fatalf("err = %v, want ErrOutsideWindow", err)
		}
	})
	t.Run("pruned chunks are skipped", func(t *testing.T) {
		chunks := chain("c1", 100, 110, 120)
		chunks[0].Status = pitr.ChunkPruned
		repo := &planRepo{chains: []*pitr.Chain{{ChainID: "c1"}}, chunks: map[string][]*pitr.Chunk{"c1": chunks}}
		src := pitr.PlanSource{Repo: repo, Bases: []pitr.Base{base("b1", 105, 108)}, HasKey: anyKey}
		if _, err := pitr.PlanRestore(ctx, st, src, at(115)); !errors.Is(err, pitr.ErrChainBreak) {
			t.Fatalf("err = %v, want ErrChainBreak", err)
		}
	})
}

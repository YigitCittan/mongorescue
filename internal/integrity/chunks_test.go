package integrity

import (
	"context"
	"errors"
	"testing"
)

func TestSweepRunsTheChunkItem(t *testing.T) {
	f := newFixture(t)
	calls := 0
	f.svc.cfg.VerifyChunks = func(context.Context) (ChunkSweep, error) {
		calls++
		return ChunkSweep{Verified: 3, Failed: 1, Breaks: 1}, nil
	}
	st, err := f.svc.Sweep(context.Background(), TriggerManual)
	if err != nil || calls != 1 || st.Chunks == nil || st.Chunks.Verified != 3 || st.Chunks.Failed != 1 {
		t.Fatalf("sweep = %+v (chunks %+v), %v after %d calls", st, st.Chunks, err, calls)
	}
	if stored := f.svc.SweepStatus(context.Background()); stored.Chunks == nil || stored.Chunks.Breaks != 1 {
		t.Fatalf("stored status chunks = %+v", stored.Chunks)
	}

	f.svc.cfg.VerifyChunks = func(context.Context) (ChunkSweep, error) {
		return ChunkSweep{}, errors.New("store unavailable")
	}
	if st, err = f.svc.Sweep(context.Background(), TriggerManual); err != nil || st.Chunks == nil || st.Chunks.Error == "" {
		t.Fatalf("failing chunk item: %+v, %v", st.Chunks, err)
	}
}

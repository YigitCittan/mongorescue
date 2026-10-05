package integrity

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestScanReportsOldUncommittedChunks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const (
		committed = "_mongorescue/oplog/conn_a/rs0/ch_1/0000000100.0000000001-0000000160.0000000001.bson.gz.age"
		orphan    = "_mongorescue/oplog/conn_a/rs0/ch_1/0000000160.0000000001-0000000220.0000000001.bson.gz.age"
	)
	for _, k := range []string{committed, orphan} {
		if _, err := f.mem.Save(ctx, k, bytes.NewReader([]byte("chunk"))); err != nil {
			t.Fatal(err)
		}
	}
	if !IsArchiveKey(orphan) || !IsChunkKey(orphan) || IsChunkKey("shop/2026/10/bkp_x.archive.gz") {
		t.Fatal("chunk keys are not recognised")
	}
	f.svc.cfg.ChunkKeys = func(context.Context, string) (map[string]bool, error) {
		return map[string]bool{committed: true}, nil
	}

	// Freshly written: an upload may still be committing it.
	f.mu.Lock()
	f.now = time.Now().Add(10 * time.Minute)
	f.mu.Unlock()
	report, err := f.svc.ScanTarget(ctx, "tgt_local", TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	if report.OrphanCount != 0 || report.Objects != 2 {
		t.Fatalf("young chunk: %+v", report)
	}

	f.mu.Lock()
	f.now = time.Now().Add(2 * time.Hour)
	f.mu.Unlock()
	if report, err = f.svc.ScanTarget(ctx, "tgt_local", TriggerManual); err != nil {
		t.Fatal(err)
	}
	if report.OrphanCount != 1 || len(report.Orphans) != 1 || report.Orphans[0].Key != orphan {
		t.Fatalf("old uncommitted chunk: %+v", report)
	}
}

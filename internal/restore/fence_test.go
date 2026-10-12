package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// testFence is a models.Fence with a fixed outcome.
type testFence struct {
	err   error
	epoch uint64
}

func (f testFence) Check() error  { return f.err }
func (f testFence) Epoch() uint64 { return f.epoch }

func TestFenceGuardsRestoreCompletion(t *testing.T) {
	errFenced := errors.New("lost the lease")
	for _, tc := range []struct {
		name  string
		fence testFence
		want  models.RestoreStatus
	}{
		{"holding the lease", testFence{epoch: 3}, models.RestoreStatusCompleted},
		{"lost the lease", testFence{err: errFenced, epoch: 3}, models.RestoreStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := storage.NewMockStorage()
			key := "shop/2026/10/bkp_shop_20261012_100000.archive"
			_, _ = st.Save(ctx, key, bytes.NewReader([]byte("archive")))
			src := &models.BackupRecord{ID: "bkp_shop_20261012_100000", Database: "shop", StorageKey: key}
			runner := func(_ context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
				_, _ = io.Copy(io.Discard, stdin)
				return strings.NewReader(""), func() error { return nil }, nil
			}
			engine := NewEngine(st, "mongodb://localhost:27017", WithRunner(runner), WithFence(tc.fence))
			record, err := engine.Run(ctx, models.RestoreRequest{BackupID: src.ID}, src)
			if record.Status != tc.want || record.HAEpoch != 3 {
				t.Fatalf("status %s epoch %d (err %v); want %s with epoch 3", record.Status, record.HAEpoch, err, tc.want)
			}
			if tc.fence.err != nil && !errors.Is(err, errFenced) {
				t.Fatalf("error = %v; want the fence's", err)
			}
		})
	}
}

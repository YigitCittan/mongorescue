package backup

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

var errFenced = errors.New("lost the lease")

func TestFenceStampsAndGuardsCompletion(t *testing.T) {
	payload := []byte("archive-bytes")
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader(payload)), strings.NewReader(""), func() error { return nil }, nil
	}
	for _, tc := range []struct {
		name  string
		fence testFence
		want  models.BackupStatus
	}{
		{"holding the lease", testFence{epoch: 7}, models.StatusCompleted},
		{"lost the lease", testFence{err: errFenced, epoch: 7}, models.StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := storage.NewMockStorage()
			engine := NewEngine(st, "mongodb://localhost:27017", WithRunner(runner), WithFence(tc.fence))
			record, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop"})
			if record.Status != tc.want || record.HAEpoch != 7 {
				t.Fatalf("status %s epoch %d (err %v); want %s with epoch 7", record.Status, record.HAEpoch, err, tc.want)
			}
			if tc.fence.err != nil {
				if !errors.Is(err, errFenced) {
					t.Fatalf("error = %v; want the fence's", err)
				}
				// The archive is kept for a storage scan; nothing is deleted on a guess.
				if _, statErr := st.Stat(context.Background(), record.StorageKey); statErr != nil {
					t.Fatalf("the archive of a fenced backup was removed: %v", statErr)
				}
			}
		})
	}
}

package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// partitionedStorage is a driver whose uploads, while partitioned, accept one chunk
// and then block until cancelled, under the driver watchdog (storage.StallWatch).
type partitionedStorage struct {
	*storage.MockStorage
	partitioned atomic.Bool
	stall       storage.StallWatch
}

func (p *partitionedStorage) Save(ctx context.Context, key string, r io.Reader) (*models.StorageObject, error) {
	if !p.partitioned.Load() {
		return p.MockStorage.Save(ctx, key, r)
	}
	ctx, r, finish := p.stall.Start(ctx, r)
	if _, err := r.Read(make([]byte, 16)); err != nil {
		return nil, finish(err)
	}
	<-ctx.Done()
	return nil, finish(ctx.Err())
}

// TestBackupStorageStallFailsRun covers #136: a backup whose storage stops accepting
// bytes fails (not cancelled, not completed) within the stall timeout, although
// mongodump keeps writing, and the next run succeeds.
func TestBackupStorageStallFailsRun(t *testing.T) {
	payload := bytes.Repeat([]byte("bson"), 64*1024)
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader(payload)), strings.NewReader(""), func() error { return nil }, nil
	}
	store := &partitionedStorage{
		MockStorage: storage.NewMockStorage(),
		stall:       storage.StallWatch{Target: "offsite", Timeout: func() time.Duration { return 100 * time.Millisecond }},
	}
	store.partitioned.Store(true)
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner), WithTimeout(time.Minute))

	start := time.Now()
	record, err := runWithDeadline(t, func() (*models.BackupRecord, error) {
		return engine.Run(context.Background(), models.BackupOptions{Database: "db"})
	})
	if !errors.Is(err, storage.ErrStorageStalled) {
		t.Fatalf("Run error = %v; want ErrStorageStalled", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the stall was detected after %v", elapsed)
	}
	if record.Status != models.StatusFailed || !strings.Contains(record.ErrorMessage, `no upload progress for 100ms to target "offsite"`) {
		t.Fatalf("record: status=%s msg=%q; want failed with the stall", record.Status, record.ErrorMessage)
	}

	store.partitioned.Store(false)
	record, err = engine.Run(context.Background(), models.BackupOptions{Database: "db"})
	if err != nil || record.Status != models.StatusCompleted {
		t.Fatalf("next run: status=%s err=%v; want completed", record.Status, err)
	}
}

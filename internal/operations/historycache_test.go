package operations_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// countingStore counts the history aggregations; while gate is set they wait for
// it, and fail is the error of the next one.
type countingStore struct {
	store.Store
	calls atomic.Int32
	gate  chan struct{}
	mu    sync.Mutex
	fail  error
}

func (c *countingStore) BackupHistory(ctx context.Context, q store.BackupHistoryQuery) (*store.BackupHistory, error) {
	c.calls.Add(1)
	if c.gate != nil {
		<-c.gate
	}
	c.mu.Lock()
	err := c.fail
	c.fail = nil
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return c.Store.BackupHistory(ctx, q)
}

func newCachedService(t *testing.T, ttl time.Duration) (*operations.Service, *store.SQLiteStore, *countingStore) {
	t.Helper()
	st := storetest.New(t)
	cs := &countingStore{Store: st}
	mock := storage.NewMockStorage()
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	svc := operations.New(operations.Config{
		Store: cs, Backup: backup.NewEngine(mock, "", backup.WithRunner(bRunner)), Restore: restore.NewEngine(mock, ""),
		Runs: manager, Version: "v-test", HistoryCacheTTL: ttl,
	})
	return svc, st, cs
}

func TestHistoryCacheSharesAndInvalidates(t *testing.T) {
	svc, st, cs := newCachedService(t, time.Hour)
	ctx := context.Background()
	if err := st.SaveJob(ctx, &models.Job{ID: "job_a", Name: "a", Database: "shop", CronExpression: "0 3 * * *", Enabled: true, ConnectionID: "c1"}); err != nil {
		t.Fatal(err)
	}
	first, err := svc.History(ctx, operations.HistoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := svc.History(ctx, operations.HistoryRequest{})
	if again != first || cs.calls.Load() != 1 {
		t.Fatalf("second call: same %v, %d aggregations; want the cached one", again == first, cs.calls.Load())
	}
	// Another window, zone or connection access is another entry.
	for _, c := range []struct {
		ctx context.Context
		req operations.HistoryRequest
	}{
		{ctx, operations.HistoryRequest{Days: 7}},
		{ctx, operations.HistoryRequest{TimeZone: "Europe/Istanbul"}},
		{ctx, operations.HistoryRequest{OffsetMinutes: 60}},
		{limitedTo(auth.ScopeRead, "c2"), operations.HistoryRequest{}},
	} {
		if h, herr := svc.History(c.ctx, c.req); herr != nil || h == first {
			t.Errorf("%+v: shared the default entry (%v)", c.req, herr)
		}
	}
	if h, _ := svc.History(limitedTo(auth.ScopeRead, "c2"), operations.HistoryRequest{}); len(h.Jobs) != 0 {
		t.Errorf("a caller limited to c2 sees jobs %v", h.Jobs)
	}

	// A backup stored without an event keeps the cached history; its event drops it.
	now := time.Now().UTC()
	if err = st.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bkp_1", JobID: "job_a", Database: "shop", Status: models.StatusCompleted, StartedAt: now, SizeBytes: 5}); err != nil {
		t.Fatal(err)
	}
	if h, _ := svc.History(ctx, operations.HistoryRequest{}); h != first {
		t.Fatal("the cache was dropped without an event")
	}
	svc.HandleEvent(ctx, events.Event{Type: events.NotificationTest})
	if h, _ := svc.History(ctx, operations.HistoryRequest{}); h != first {
		t.Fatal("an unrelated event dropped the cache")
	}
	svc.HandleEvent(ctx, events.Event{Type: events.BackupSucceeded})
	fresh, err := svc.History(ctx, operations.HistoryRequest{})
	if err != nil || fresh == first || len(fresh.Jobs["job_a"].Runs) != 1 {
		t.Fatalf("after backup.succeeded: %+v, %v; want the new backup", fresh.Jobs["job_a"], err)
	}
	svc.InvalidateHistory()
	if h, _ := svc.History(ctx, operations.HistoryRequest{}); h == fresh {
		t.Fatal("InvalidateHistory kept the cache")
	}
	// Editing a job through the service drops it too.
	cached, _ := svc.History(ctx, operations.HistoryRequest{})
	if err := svc.DeleteJob(ctx, "job_a"); err != nil {
		t.Fatal(err)
	}
	if h, _ := svc.History(ctx, operations.HistoryRequest{}); h == cached || len(h.Jobs) != 0 {
		t.Fatalf("after deleting the job: %v", h.Jobs)
	}
}

func TestHistoryCacheComputesOnceForConcurrentCallers(t *testing.T) {
	svc, _, cs := newCachedService(t, time.Hour)
	cs.gate = make(chan struct{})
	var wg sync.WaitGroup
	results := make([]*operations.History, 16)
	for i := range results {
		wg.Go(func() {
			h, err := svc.History(context.Background(), operations.HistoryRequest{})
			if err != nil {
				t.Error(err)
			}
			results[i] = h
		})
	}
	for cs.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(cs.gate)
	wg.Wait()
	if n := cs.calls.Load(); n != 1 {
		t.Fatalf("%d aggregations for 16 concurrent callers; want 1", n)
	}
	for _, h := range results {
		if h == nil || h != results[0] {
			t.Fatal("callers got different results")
		}
	}
}

func TestHistoryCacheDoesNotKeepFailuresAndExpires(t *testing.T) {
	svc, _, cs := newCachedService(t, 20*time.Millisecond)
	cs.fail = errors.New("disk on fire")
	if _, err := svc.History(context.Background(), operations.HistoryRequest{}); err == nil {
		t.Fatal("the failure was not returned")
	}
	first, err := svc.History(context.Background(), operations.HistoryRequest{})
	if err != nil || cs.calls.Load() != 2 {
		t.Fatalf("after a failure: %v, %d aggregations; want a new one", err, cs.calls.Load())
	}
	time.Sleep(40 * time.Millisecond)
	if h, _ := svc.History(context.Background(), operations.HistoryRequest{}); h == first || cs.calls.Load() != 3 {
		t.Fatalf("an expired entry was served (%d aggregations)", cs.calls.Load())
	}
}

func TestHistoryCacheWaiterOutlivesACancelledLeader(t *testing.T) {
	svc, _, cs := newCachedService(t, time.Hour)
	cs.gate = make(chan struct{})
	leaderCtx, cancel := context.WithCancel(context.Background())
	cs.fail = context.Canceled
	done := make(chan error, 1)
	go func() {
		_, err := svc.History(leaderCtx, operations.HistoryRequest{})
		done <- err
	}()
	for cs.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	waiter := make(chan *operations.History, 1)
	go func() {
		h, err := svc.History(context.Background(), operations.HistoryRequest{})
		if err != nil {
			t.Error(err)
		}
		waiter <- h
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	close(cs.gate)
	if err := <-done; err == nil {
		t.Fatal("the cancelled leader succeeded")
	}
	if h := <-waiter; h == nil {
		t.Fatal("the waiter got no history")
	}
}

func TestHistoryWithoutCacheComputesEveryTime(t *testing.T) {
	svc, _, cs := newCachedService(t, 0)
	a, _ := svc.History(context.Background(), operations.HistoryRequest{})
	b, _ := svc.History(context.Background(), operations.HistoryRequest{})
	if a == b || cs.calls.Load() != 2 {
		t.Fatalf("without a TTL: shared %v, %d aggregations", a == b, cs.calls.Load())
	}
}

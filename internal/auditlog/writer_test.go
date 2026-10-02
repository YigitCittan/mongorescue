package auditlog

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatedRepo blocks the first append until release is closed.
type gatedRepo struct {
	*memRepo
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *gatedRepo) AppendAuditEvent(ctx context.Context, e *Event) error {
	first := false
	g.once.Do(func() { first = true })
	if first {
		close(g.entered)
		<-g.release
	}
	return g.memRepo.AppendAuditEvent(ctx, e)
}

// failingRepo refuses every append.
type failingRepo struct{ *memRepo }

func (failingRepo) AppendAuditEvent(context.Context, *Event) error { return errors.New("disk full") }

// TestRunWritesOffTheRequestPath proves entries recorded while Run runs are written
// by its writer, that a full queue falls back to a synchronous write instead of
// dropping, and that stopping drains the queue and flushes open windows.
func TestRunWritesOffTheRequestPath(t *testing.T) {
	g := &gatedRepo{memRepo: newMemRepo(), entered: make(chan struct{}), release: make(chan struct{})}
	var syncWrites atomic.Int64
	s := New(Config{Repo: g, QueueSize: 1, OnSyncWrite: func() { syncWrites.Add(1) }, PruneInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { s.Run(ctx) })
	waitFor(t, func() bool {
		s.gate.RLock()
		defer s.gate.RUnlock()
		return s.running
	})

	rec := func(action string) {
		s.Record(context.Background(), Event{ActorKind: ActorUser, Action: action, Status: 200})
	}
	rec("POST /1") // the writer takes it and blocks in the store
	<-g.entered
	rec("POST /2") // queued
	if s.QueueDepth() != 1 {
		t.Fatalf("queue depth = %d; want 1", s.QueueDepth())
	}
	done := make(chan struct{})
	go func() {
		rec("POST /3") // the queue is full: written synchronously, not dropped
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a full queue blocked Record")
	}
	if syncWrites.Load() != 1 {
		t.Fatalf("sync writes = %d; want 1", syncWrites.Load())
	}
	// A refusal leaves an open window; stopping must write its summary.
	for range 3 {
		s.Record(context.Background(), Event{ActorKind: ActorAnonymous, Action: "POST /login", Status: 401, ClientIP: "192.0.2.1"})
	}
	close(g.release)
	cancel()
	wg.Wait()

	actions := map[string]int{}
	for _, e := range g.rows {
		actions[e.Action] += e.Count
	}
	if actions["POST /1"] != 1 || actions["POST /2"] != 1 || actions["POST /3"] != 1 || actions["POST /login"] != 3 {
		t.Fatalf("stored = %v; want every entry, the refusals as 1 + a summary of 2", actions)
	}
	if v, _ := s.Verify(context.Background()); !v.OK {
		t.Fatalf("Verify = %+v", v)
	}
	// After Run returned, Record writes synchronously.
	rec("POST /after")
	if g.rows[len(g.rows)-1].Action != "POST /after" {
		t.Fatal("an entry recorded after Run stopped was not written")
	}
}

// TestRunFlushesClosedWindows proves the ticker writes the summary of a window that
// closed, without a further refusal.
func TestRunFlushesClosedWindows(t *testing.T) {
	repo := newMemRepo()
	var mu sync.Mutex
	now := vectorTime
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	s := New(Config{Repo: repo, Now: clock})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { s.Run(ctx) })
	for range 4 {
		s.Record(context.Background(), Event{ActorKind: ActorAnonymous, Action: "POST /login", Status: 401, ClientIP: "192.0.2.1"})
	}
	mu.Lock()
	now = now.Add(CoalesceWindow)
	mu.Unlock()
	waitFor(t, func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return len(repo.rows) == 2 && repo.rows[1].Count == 3
	})
	cancel()
	wg.Wait()
}

func TestWriteFailuresAreCounted(t *testing.T) {
	var failures atomic.Int64
	s := New(Config{Repo: failingRepo{newMemRepo()}, OnWriteFailure: func() { failures.Add(1) }})
	s.Record(context.Background(), Event{ActorKind: ActorUser, Action: "POST /x", Status: 200})
	if failures.Load() != 1 {
		t.Fatalf("failures = %d", failures.Load())
	}
}

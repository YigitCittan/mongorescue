package auditlog

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/notify"
)

// TestForwarderQueueOverflow proves Enqueue never blocks: with no worker running,
// entries beyond the queue size are dropped and counted.
func TestForwarderQueueOverflow(t *testing.T) {
	var observed sync.Map
	f := NewForwarder(ForwarderConfig{
		Endpoint:  func() Endpoint { return Endpoint{URL: "http://127.0.0.1:1/audit"} },
		QueueSize: 3,
		Observe: func(o string) {
			n, _ := observed.LoadOrStore(o, new(atomic.Int64))
			n.(*atomic.Int64).Add(1)
		},
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 10 {
			f.Enqueue(Event{ID: int64(i + 1)})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue blocked on a full queue")
	}
	st := f.Status()
	if !st.Enabled || st.Queued != 3 || st.Dropped != 7 || st.Sent != 0 {
		t.Fatalf("status = %+v; want 3 queued, 7 dropped", st)
	}
	if n, _ := observed.Load(ForwardDropped); n == nil || n.(*atomic.Int64).Load() != 7 {
		t.Fatalf("dropped observations = %v", n)
	}
}

func TestForwarderOffQueuesNothing(t *testing.T) {
	f := NewForwarder(ForwarderConfig{QueueSize: 1})
	f.Enqueue(Event{ID: 1})
	f.Enqueue(Event{ID: 2})
	if st := f.Status(); st.Enabled || st.Queued != 0 || st.Dropped != 0 {
		t.Fatalf("status without an endpoint = %+v", st)
	}
	var nilF *Forwarder
	nilF.Enqueue(Event{})
	nilF.Run(context.Background())
	if st := nilF.Status(); st.Enabled {
		t.Fatal("a nil forwarder reports enabled")
	}
}

// TestForwarderDeliversSignedEntries proves entries are POSTed as JSON with the
// HMAC signature, failures are counted with a scrubbed error, and stopping drains
// the queue.
func TestForwarderDeliversSignedEntries(t *testing.T) {
	const secret = "audit-signing-secret"
	var mu sync.Mutex
	var got []Event
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get(SignatureHeader) != notify.Sign(secret, body) || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		if fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		var e Event
		_ = json.Unmarshal(body, &e)
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	}))
	defer srv.Close()

	f := NewForwarder(ForwarderConfig{
		Endpoint: func() Endpoint { return Endpoint{URL: srv.URL + "/hook/token-in-path", Secret: secret} },
		Client:   srv.Client(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { f.Run(ctx) })

	f.Enqueue(Event{ID: 1, Action: "POST /api/v1/jobs", Hash: "h1"})
	waitFor(t, func() bool { return f.Status().Sent == 1 })
	fail.Store(true)
	f.Enqueue(Event{ID: 2})
	waitFor(t, func() bool { return f.Status().Failed == 1 })
	if st := f.Status(); !strings.Contains(st.LastError, "502") || strings.Contains(st.LastError, "token-in-path") || st.LastErrorAt.IsZero() {
		t.Fatalf("status = %+v", st)
	}
	fail.Store(false)
	for i := range 5 {
		f.Enqueue(Event{ID: int64(3 + i)})
	}
	cancel()
	wg.Wait()
	st := f.Status()
	if st.Sent+st.Dropped != 6 || st.Failed != 1 || st.Queued != 0 {
		t.Fatalf("after stop = %+v; want every queued entry delivered or dropped", st)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || got[0].ID != 1 || got[0].Hash != "h1" || got[0].Action != "POST /api/v1/jobs" {
		t.Fatalf("received = %+v", got)
	}
	// After stopping, entries are dropped at once.
	f.Enqueue(Event{ID: 99})
	if f.Status().Dropped != st.Dropped+1 {
		t.Fatal("an entry enqueued after stop was not dropped")
	}
}

// TestForwarderErrorHidesTheURL proves transport errors keep only their cause.
func TestForwarderErrorHidesTheURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/services/T000/B000/s3cr3tt0ken"
	srv.Close() // connections are refused now
	f := NewForwarder(ForwarderConfig{Endpoint: func() Endpoint { return Endpoint{URL: url} }})
	f.deliver(context.Background(), Event{ID: 1})
	st := f.Status()
	if st.Failed != 1 || st.LastError == "" || strings.Contains(st.LastError, "s3cr3tt0ken") {
		t.Fatalf("status = %+v", st)
	}
}

// TestServiceForwardsStoredEntries proves Record hands stored entries, with their
// hash, to the forwarder, and never coalesced ones.
func TestServiceForwardsStoredEntries(t *testing.T) {
	f := NewForwarder(ForwarderConfig{Endpoint: func() Endpoint { return Endpoint{URL: "http://127.0.0.1:1/"} }, QueueSize: 10})
	s := New(Config{Repo: newMemRepo(), Forwarder: f, Now: fixedClock(vectorTime, time.Second)})
	s.Record(context.Background(), Event{ActorKind: ActorUser, Action: "POST /a", Status: 200})
	s.Record(context.Background(), Event{ActorKind: ActorAnonymous, Action: "POST /login", Status: 401})
	s.Record(context.Background(), Event{ActorKind: ActorAnonymous, Action: "POST /login", Status: 401})
	if st := s.Forwarding(); st.Queued != 2 {
		t.Fatalf("queued = %d; want 2", st.Queued)
	}
	e := <-f.queue
	if e.ID != 1 || len(e.Hash) != 64 {
		t.Fatalf("forwarded = %+v", e)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

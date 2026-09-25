package events

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestBackupEvent(t *testing.T) {
	done := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		rec       *models.BackupRecord
		err       error
		wantType  EventType
		wantDB    string
		wantError string
	}{
		{
			name:     "completed",
			rec:      &models.BackupRecord{ID: "bkp_1", Database: "shop", Status: models.StatusCompleted, SizeBytes: 42, DurationSeconds: 1.5, CompletedAt: &done},
			wantType: BackupSucceeded,
			wantDB:   "shop",
		},
		{
			name:      "failed record redacts credentials",
			rec:       &models.BackupRecord{ID: "bkp_2", Database: "shop", Status: models.StatusFailed, ErrorMessage: "dial mongodb://u:hunter2@h/db failed"},
			err:       errors.New("boom"),
			wantType:  BackupFailed,
			wantDB:    "shop",
			wantError: "mongodb://u:******@h/db",
		},
		{
			name:      "nil record",
			err:       errors.New("backup: target database name is required"),
			wantType:  BackupFailed,
			wantDB:    "fallback",
			wantError: "target database name is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := BackupEvent(tt.rec, tt.err, "job_1", "fallback")
			if e.Type != tt.wantType {
				t.Fatalf("type = %s; want %s", e.Type, tt.wantType)
			}
			if e.Database != tt.wantDB || e.JobID != "job_1" {
				t.Errorf("unexpected identity fields: %+v", e)
			}
			if tt.wantError == "" && e.Error != "" {
				t.Errorf("unexpected error %q", e.Error)
			}
			if !strings.Contains(e.Error, tt.wantError) {
				t.Errorf("error %q does not contain %q", e.Error, tt.wantError)
			}
			if strings.Contains(e.Error, "hunter2") {
				t.Errorf("error leaks password: %q", e.Error)
			}
		})
	}

	ok := BackupEvent(tests[0].rec, nil, "", "")
	if ok.Duration != 1500*time.Millisecond || ok.SizeBytes != 42 || !ok.Time.Equal(done) {
		t.Errorf("unexpected success event fields: %+v", ok)
	}
}

func TestRestoreEvent(t *testing.T) {
	rec := &models.RestoreRecord{ID: "rst_1", BackupID: "bkp_1", TargetDatabase: "shop_rescue", Status: models.RestoreStatusCompleted}
	if e := RestoreEvent(rec, nil, ""); e.Type != RestoreSucceeded || e.RestoreID != "rst_1" || e.Database != "shop_rescue" || e.BackupID != "bkp_1" {
		t.Errorf("unexpected success event: %+v", e)
	}
	failed := &models.RestoreRecord{ID: "rst_2", Status: models.RestoreStatusFailed, ErrorMessage: "x"}
	if e := RestoreEvent(failed, errors.New("x"), "bkp_9"); e.Type != RestoreFailed || e.BackupID != "bkp_9" || e.Error != "x" {
		t.Errorf("unexpected failure event: %+v", e)
	}
	if e := RestoreEvent(nil, context.Canceled, "bkp_9"); e.Type != RestoreFailed || e.Error != "operation cancelled" {
		t.Errorf("unexpected cancelled event: %+v", e)
	}
}

func TestEventTypeHelpers(t *testing.T) {
	for _, et := range RuleTypes() {
		if !et.Subscribable() {
			t.Errorf("%s should be subscribable", et)
		}
	}
	if NotificationTest.Subscribable() || EventType("bogus").Subscribable() {
		t.Error("test/bogus types must not be subscribable")
	}
	if !BackupFailed.Failed() || BackupSucceeded.Failed() {
		t.Error("Failed() mismatch")
	}
}

func TestBusDropsWhenFull(t *testing.T) {
	var hookCalls atomic.Int32
	b := NewBus(WithBufferSize(1), WithDropHook(func(Event) { hookCalls.Add(1) }))

	if !b.Publish(context.Background(), Event{Type: BackupSucceeded}) {
		t.Fatal("first publish should be accepted")
	}
	if b.Publish(context.Background(), Event{Type: BackupFailed}) {
		t.Fatal("second publish should be dropped when the queue is full")
	}
	if b.Dropped() != 1 || hookCalls.Load() != 1 {
		t.Fatalf("dropped = %d, hook calls = %d; want 1, 1", b.Dropped(), hookCalls.Load())
	}

	var nilBus *Bus
	if nilBus.Publish(context.Background(), Event{}) {
		t.Error("nil bus must not accept events")
	}
}

func TestBusDispatchAndCleanShutdown(t *testing.T) {
	b := NewBus(WithBufferSize(16), WithDrainTimeout(time.Second))

	var mu sync.Mutex
	var got []EventType
	b.Subscribe(func(_ context.Context, e Event) {
		mu.Lock()
		got = append(got, e.Type)
		mu.Unlock()
	})
	b.Subscribe(func(context.Context, Event) { panic("faulty subscriber") })
	b.Subscribe(nil)

	// Queue events before the dispatcher starts: they must be drained on shutdown.
	for i := 0; i < 5; i++ {
		b.Publish(context.Background(), Event{Type: BackupSucceeded})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n != 5 {
		t.Fatalf("delivered %d events; want 5 drained on shutdown", n)
	}

	if b.Publish(context.Background(), Event{Type: BackupFailed}) {
		t.Error("publish after stop must be rejected")
	}
	if err := b.Run(context.Background()); !errors.Is(err, ErrBusRunning) {
		t.Errorf("second Run = %v; want ErrBusRunning", err)
	}
}

func TestBusDeliversWhileRunning(t *testing.T) {
	b := NewBus()
	received := make(chan Event, 1)
	b.Subscribe(func(_ context.Context, e Event) { received <- e })

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = b.Run(ctx)
	}()
	defer func() {
		cancel()
		wg.Wait()
	}()

	b.Publish(context.Background(), Event{Type: RestoreFailed, RestoreID: "rst_1"})
	select {
	case e := <-received:
		if e.RestoreID != "rst_1" {
			t.Errorf("unexpected event %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not delivered")
	}
}

func TestBusPublishRacingShutdown(t *testing.T) {
	var hookCalls atomic.Uint64
	b := NewBus(WithBufferSize(1024), WithDrainTimeout(5*time.Second), WithDropHook(func(Event) { hookCalls.Add(1) }))
	var delivered atomic.Uint64
	b.Subscribe(func(context.Context, Event) { delivered.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = b.Run(ctx)
	}()

	var accepted atomic.Uint64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if b.Publish(context.Background(), Event{Type: BackupSucceeded}) {
					accepted.Add(1)
				}
			}
		}()
	}
	time.Sleep(time.Millisecond)
	cancel()
	wg.Wait()
	<-runDone

	// Every accepted event is delivered; every rejected one is counted as dropped.
	if delivered.Load() != accepted.Load() {
		t.Fatalf("delivered %d of %d accepted events", delivered.Load(), accepted.Load())
	}
	if got := b.Dropped(); got != 1600-accepted.Load() || hookCalls.Load() != got {
		t.Fatalf("dropped = %d, hook = %d; want %d", got, hookCalls.Load(), 1600-accepted.Load())
	}

	before := b.Dropped()
	if b.Publish(context.Background(), Event{Type: BackupFailed}) || b.Dropped() != before+1 {
		t.Fatal("publish after drain must be rejected and counted as dropped")
	}
}

func TestBusDrainTimeout(t *testing.T) {
	b := NewBus(WithBufferSize(8), WithDrainTimeout(50*time.Millisecond))
	var calls atomic.Int32
	b.Subscribe(func(ctx context.Context, _ Event) {
		calls.Add(1)
		<-ctx.Done() // a handler that honours the drain deadline
	})
	for i := 0; i < 8; i++ {
		b.Publish(context.Background(), Event{Type: BackupFailed})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := b.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("drain took %v; want bounded by the drain timeout", elapsed)
	}
	if calls.Load() != 1 {
		t.Errorf("handler calls = %d; want 1 before the deadline expired", calls.Load())
	}
}

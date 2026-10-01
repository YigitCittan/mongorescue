package runs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestKeyLocksExcludeAndCancel(t *testing.T) {
	k := NewKeyLocks()
	unlock, err := k.Lock(context.Background(), "a", "b", "a")
	if err != nil {
		t.Fatal(err)
	}
	// Another key is free; a held one blocks until the context ends.
	other, err := k.Lock(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	other()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := k.Lock(ctx, "c", "b"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock of a held key: %v; want the context's error", err)
	}
	// The failed attempt released "c" again.
	if again, err := k.Lock(context.Background(), "c"); err != nil {
		t.Fatal(err)
	} else {
		again()
	}

	var mu sync.Mutex
	inside := 0
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := k.Lock(context.Background(), "a")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			inside++
			if inside != 1 {
				t.Error("two holders of one key")
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
			u()
		}()
	}
	unlock()
	unlock() // idempotent
	wg.Wait()
	if len(k.held) != 0 {
		t.Fatalf("locks left behind: %v", k.held)
	}
}

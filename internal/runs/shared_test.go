package runs

import (
	"context"
	"errors"
	"testing"
)

func TestSharedAndExclusiveKeys(t *testing.T) {
	m := NewManager(nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	key := InstanceKey("conn_a")
	r1, err := m.AcquireShared(key)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := m.AcquireShared(key)
	if err != nil {
		t.Fatalf("a second shared holder: %v", err)
	}
	if _, err = m.Acquire(key); !errors.Is(err, ErrBusy) {
		t.Fatalf("exclusive while shared: %v", err)
	}
	r1()
	r1() // idempotent
	if _, err = m.Acquire(key); !errors.Is(err, ErrBusy) {
		t.Fatalf("exclusive while one shared holder remains: %v", err)
	}
	r2()
	excl, err := m.Acquire(key)
	if err != nil {
		t.Fatalf("exclusive once free: %v", err)
	}
	if _, err = m.AcquireShared(key); !errors.Is(err, ErrBusy) {
		t.Fatalf("shared while exclusive: %v", err)
	}
	excl()
	if r, err := m.AcquireShared(key); err != nil {
		t.Fatalf("shared once free: %v", err)
	} else {
		r()
	}
}

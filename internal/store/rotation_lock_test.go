package store_test

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestWithKeyLockedSeesOnCommitWithTheNewKey starts a WithKeyLocked reader while the
// rotation transaction runs: the reader must wait until the store uses the new key
// and OnCommit (which switches the metadata backup install ID) has run, so it never
// pairs the re-sealed database with the old install ID.
func TestWithKeyLockedSeesOnCommitWithTheNewKey(t *testing.T) {
	ctx := context.Background()
	st := storetest.OpenWithBox(t, filepath.Join(t.TempDir(), dbFile), storetest.NewBox(t))
	next := storetest.NewBox(t)
	var switched atomic.Bool
	var sawSwitched atomic.Bool
	var wg sync.WaitGroup
	_, err := st.RotateSecretBox(ctx, store.SecretKeyRotation{
		Next: next,
		BeforeCommit: func() error {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = st.WithKeyLocked(func() error {
					sawSwitched.Store(switched.Load())
					return nil
				})
			}()
			time.Sleep(50 * time.Millisecond) // give the reader time to block
			return nil
		},
		OnCommit: func() { switched.Store(true) },
	})
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if !sawSwitched.Load() {
		t.Fatal("a WithKeyLocked reader ran before OnCommit switched the key-derived state")
	}
	if ok, err := st.SealedWith(ctx, next); err != nil || !ok {
		t.Fatalf("SealedWith(next) = %v, %v", ok, err)
	}
}

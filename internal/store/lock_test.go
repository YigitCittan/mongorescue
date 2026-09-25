package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/store"
)

func TestLockDataDirIsExclusive(t *testing.T) {
	dir := t.TempDir()
	first, err := store.LockDataDir(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	// A second open of the lock file (a separate descriptor, as another process would
	// have) must fail fast.
	second, err := store.LockDataDir(dir)
	if !errors.Is(err, store.ErrDataDirLocked) || second != nil {
		t.Fatalf("second lock = %v, %v; want ErrDataDirLocked", second, err)
	}
	if !strings.Contains(err.Error(), "another MongoRescue instance is using "+dir) {
		t.Fatalf("error must name the directory: %v", err)
	}

	if err = first.Release(); err != nil {
		t.Fatal(err)
	}
	if err = first.Release(); err != nil {
		t.Fatalf("second Release = %v; want nil", err)
	}
	again, err := store.LockDataDir(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
}

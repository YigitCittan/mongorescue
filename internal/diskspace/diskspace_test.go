package diskspace_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/diskspace"
)

func TestFree(t *testing.T) {
	n, err := diskspace.Free(t.TempDir())
	if errors.Is(err, diskspace.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no free space reported for the temporary directory")
	}
}

func TestFreeOfAMissingPath(t *testing.T) {
	_, err := diskspace.Free(filepath.Join(t.TempDir(), "missing", "dir"))
	if err == nil {
		t.Fatal("a missing path reported free space")
	}
}

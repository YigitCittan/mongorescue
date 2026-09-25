package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestMockStorage(t *testing.T) {
	mock := NewMockStorage()
	ctx := context.Background()

	data := []byte("mongodb-mock-archive-data")
	key := "backups/prod/mydb.gz"

	// 1. Test Save
	obj, err := mock.Save(ctx, key, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if obj.SizeBytes != int64(len(data)) {
		t.Errorf("expected size %d, got %d", len(data), obj.SizeBytes)
	}

	// 2. Test Retrieve
	rc, err := mock.Retrieve(ctx, key)
	if err != nil {
		t.Fatalf("Retrieve failed: %v", err)
	}
	defer rc.Close()

	readBytes, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if !bytes.Equal(readBytes, data) {
		t.Errorf("expected %q, got %q", data, readBytes)
	}

	// 3. Test Stat
	statObj, err := mock.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if statObj.SizeBytes != int64(len(data)) {
		t.Errorf("expected stat size %d, got %d", len(data), statObj.SizeBytes)
	}

	// 4. Test List
	_, _ = mock.Save(ctx, "backups/prod/second.gz", bytes.NewReader([]byte("2")))
	_, _ = mock.Save(ctx, "other/file.txt", bytes.NewReader([]byte("3")))

	items, err := mock.List(ctx, "backups/prod")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}

	// 5. Test Delete
	if err = mock.Delete(ctx, key); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	_, err = mock.Retrieve(ctx, key)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after deletion, got %v", err)
	}
}

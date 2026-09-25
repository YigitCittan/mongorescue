package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestLocalStorage(t *testing.T) {
	tempDir := t.TempDir()
	store, err := NewLocalStorage(tempDir)
	if err != nil {
		t.Fatalf("failed to create local storage: %v", err)
	}

	ctx := context.Background()

	// 1. Test Save
	sampleData := []byte("mongodb-rescue-test-stream-data-12345")
	key := "backups/2026/09/mydb.gz"

	obj, err := store.Save(ctx, key, bytes.NewReader(sampleData))
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if obj.SizeBytes != int64(len(sampleData)) {
		t.Errorf("expected size %d, got %d", len(sampleData), obj.SizeBytes)
	}
	if obj.StorageType != models.StorageLocal {
		t.Errorf("expected storage local, got %s", obj.StorageType)
	}

	// 2. Test Stat
	statObj, err := store.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if statObj.SizeBytes != int64(len(sampleData)) {
		t.Errorf("expected stat size %d, got %d", len(sampleData), statObj.SizeBytes)
	}

	// 3. Test Retrieve
	rc, err := store.Retrieve(ctx, key)
	if err != nil {
		t.Fatalf("Retrieve failed: %v", err)
	}
	readData, err := io.ReadAll(rc)
	// Close before Delete: Windows cannot remove a file that is still open.
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read stream failed: %v", err)
	}
	if !bytes.Equal(readData, sampleData) {
		t.Errorf("expected content %q, got %q", sampleData, readData)
	}

	// 4. Test List with prefix
	_, _ = store.Save(ctx, "backups/2026/09/anotherdb.gz", strings.NewReader("another"))
	_, _ = store.Save(ctx, "other/unrelated.txt", strings.NewReader("other"))

	items, err := store.List(ctx, "backups/2026/09")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items with prefix, got %d", len(items))
	}

	// 5. Test Delete
	if err = store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Retrieve after delete should return ErrNotFound
	_, err = store.Retrieve(ctx, key)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}

	// Delete again should return ErrNotFound
	if err := store.Delete(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound on second delete, got %v", err)
	}
}

func TestLocalStoragePathTraversal(t *testing.T) {
	tempDir := t.TempDir()
	store, err := NewLocalStorage(tempDir)
	if err != nil {
		t.Fatalf("failed to create local storage: %v", err)
	}

	ctx := context.Background()

	traversalKeys := []string{
		"../../etc/passwd",
		"/etc/passwd",
		"backups/../../../var/log",
		"../escape",
	}

	for _, badKey := range traversalKeys {
		t.Run("Traversal_"+badKey, func(t *testing.T) {
			_, err := store.Save(ctx, badKey, strings.NewReader("malicious"))
			if !errors.Is(err, ErrPathTraversal) {
				t.Errorf("expected ErrPathTraversal for key %q, got %v", badKey, err)
			}

			_, err = store.Retrieve(ctx, badKey)
			if !errors.Is(err, ErrPathTraversal) {
				t.Errorf("expected ErrPathTraversal on Retrieve for key %q, got %v", badKey, err)
			}
		})
	}
}

func TestLocalStorageContextCancellation(t *testing.T) {
	tempDir := t.TempDir()
	store, err := NewLocalStorage(tempDir)
	if err != nil {
		t.Fatalf("failed to create local storage: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err = store.Save(ctx, "test.gz", strings.NewReader("data"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled error, got %v", err)
	}

	// Ensure no orphan file was left behind
	items, _ := store.List(context.Background(), "")
	if len(items) != 0 {
		t.Errorf("expected 0 files after cancelled write, found %d", len(items))
	}
}

func TestLocalStorageInvalidKey(t *testing.T) {
	tempDir := t.TempDir()
	store, _ := NewLocalStorage(tempDir)
	ctx := context.Background()

	_, err := store.Save(ctx, "   ", strings.NewReader("data"))
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("expected ErrInvalidKey, got %v", err)
	}
}

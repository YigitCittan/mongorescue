package storage

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// MockStorage provides an in-memory, thread-safe implementation of the Storage interface.
// It is intended for testing backup, restore, and retention workflows without external I/O.
type MockStorage struct {
	mu      sync.RWMutex
	objects map[string][]byte
	modTime map[string]time.Time
}

// NewMockStorage initializes a fresh in-memory storage instance.
func NewMockStorage() *MockStorage {
	return &MockStorage{
		objects: make(map[string][]byte),
		modTime: make(map[string]time.Time),
	}
}

// Save reads all data from r and stores it in memory.
func (m *MockStorage) Save(ctx context.Context, key string, r io.Reader) (*models.StorageObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cleanKey := strings.TrimSpace(key)
	if cleanKey == "" {
		return nil, ErrInvalidKey
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	now := time.Now()

	m.mu.Lock()
	m.objects[cleanKey] = data
	m.modTime[cleanKey] = now
	m.mu.Unlock()

	return &models.StorageObject{
		Key:         cleanKey,
		SizeBytes:   int64(len(data)),
		ModTime:     now,
		StorageType: models.StorageLocal,
	}, nil
}

// Retrieve returns a ReadCloser over the stored byte slice.
func (m *MockStorage) Retrieve(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	data, ok := m.objects[key]
	if !ok {
		return nil, ErrNotFound
	}

	return io.NopCloser(bytes.NewReader(data)), nil
}

// Delete removes an object from in-memory storage.
func (m *MockStorage) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.objects[key]; !ok {
		return ErrNotFound
	}

	delete(m.objects, key)
	delete(m.modTime, key)
	return nil
}

// List returns all stored objects matching an optional prefix.
func (m *MockStorage) List(ctx context.Context, prefix string) ([]*models.StorageObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*models.StorageObject
	for k, v := range m.objects {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			result = append(result, &models.StorageObject{
				Key:         k,
				SizeBytes:   int64(len(v)),
				ModTime:     m.modTime[k],
				StorageType: models.StorageLocal,
			})
		}
	}

	return result, nil
}

// Stat returns metadata for a stored key.
func (m *MockStorage) Stat(ctx context.Context, key string) (*models.StorageObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	data, ok := m.objects[key]
	if !ok {
		return nil, ErrNotFound
	}

	return &models.StorageObject{
		Key:         key,
		SizeBytes:   int64(len(data)),
		ModTime:     m.modTime[key],
		StorageType: models.StorageLocal,
	}, nil
}

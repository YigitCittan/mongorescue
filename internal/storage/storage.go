// Package storage defines the persistence abstraction and concrete drivers
// for MongoRescue backup artifacts.
package storage

import (
	"context"
	"errors"
	"io"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Standard sentinel errors returned by storage providers.
var (
	// ErrNotFound indicates that the requested object key does not exist.
	ErrNotFound = errors.New("storage: object not found")

	// ErrInvalidKey indicates that the storage key is empty or malformed.
	ErrInvalidKey = errors.New("storage: invalid object key")

	// ErrPathTraversal indicates an attempt to escape the designated storage root.
	ErrPathTraversal = errors.New("storage: path traversal detected")
)

// Storage defines the pluggable persistence contract for backup archives.
// All storage implementations (Local filesystem, S3, MinIO) must be thread-safe.
type Storage interface {
	// Save streams data from reader to target key and returns object metadata.
	// It must stream the reader directly without buffering the entire content in RAM.
	Save(ctx context.Context, key string, r io.Reader) (*models.StorageObject, error)

	// Retrieve returns a readable stream for a specific backup key.
	// The caller is strictly responsible for closing the returned ReadCloser.
	Retrieve(ctx context.Context, key string) (io.ReadCloser, error)

	// Delete removes the backup artifact from storage.
	Delete(ctx context.Context, key string) error

	// List returns all backup objects matching an optional prefix/filter.
	List(ctx context.Context, prefix string) ([]*models.StorageObject, error)

	// Stat returns metadata for a single stored object.
	Stat(ctx context.Context, key string) (*models.StorageObject, error)
}

package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// LocalStorage implements the Storage interface for local disk and mounted network paths.
type LocalStorage struct {
	baseDir string
}

// NewLocalStorage initializes a LocalStorage driver bound to the specified directory.
// It creates the base directory if it does not already exist.
func NewLocalStorage(baseDir string) (*LocalStorage, error) {
	if strings.TrimSpace(baseDir) == "" {
		return nil, fmt.Errorf("%w: base directory cannot be empty", ErrInvalidKey)
	}

	absDir, err := filepath.Abs(filepath.Clean(baseDir))
	if err != nil {
		return nil, fmt.Errorf("resolve base directory: %w", err)
	}

	if err := os.MkdirAll(absDir, 0o750); err != nil {
		return nil, fmt.Errorf("create base directory: %w", err)
	}

	return &LocalStorage{baseDir: absDir}, nil
}

// Save streams data from the reader to the destination key on disk atomically.
func (s *LocalStorage) Save(ctx context.Context, key string, r io.Reader) (*models.StorageObject, error) {
	fullPath, err := s.resolvePath(key)
	if err != nil {
		return nil, err
	}

	// Ensure parent directories exist
	if err = os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
		return nil, fmt.Errorf("create parent directory: %w", err)
	}

	// Write to a temporary file first to guarantee atomic persistence
	tmpPath := fmt.Sprintf("%s.tmp.%d", fullPath, time.Now().UnixNano())
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, fmt.Errorf("create temporary file: %w", err)
	}

	cleanup := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}

	// Stream copying with 64KB buffer
	buf := make([]byte, 64*1024)
	var written int64

	for {
		select {
		case <-ctx.Done():
			cleanup()
			return nil, ctx.Err()
		default:
		}

		nr, readErr := r.Read(buf)
		if nr > 0 {
			nw, writeErr := tmpFile.Write(buf[0:nr])
			if writeErr != nil {
				cleanup()
				return nil, fmt.Errorf("write stream chunk: %w", writeErr)
			}
			written += int64(nw)
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			cleanup()
			return nil, fmt.Errorf("read stream chunk: %w", readErr)
		}
	}

	if err = tmpFile.Sync(); err != nil {
		cleanup()
		return nil, fmt.Errorf("sync temporary file: %w", err)
	}

	if err = tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("close temporary file: %w", err)
	}

	// Atomically move the file into place
	if err = os.Rename(tmpPath, fullPath); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("rename temporary file: %w", err)
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		return nil, fmt.Errorf("stat saved file: %w", err)
	}

	return &models.StorageObject{
		Key:         s.toStandardKey(key),
		SizeBytes:   info.Size(),
		ModTime:     info.ModTime(),
		StorageType: models.StorageLocal,
	}, nil
}

// Retrieve returns a readable stream for a specific backup key.
func (s *LocalStorage) Retrieve(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fullPath, err := s.resolvePath(key)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open file: %w", err)
	}

	return f, nil
}

// Delete removes a stored backup artifact.
func (s *LocalStorage) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	fullPath, err := s.resolvePath(key)
	if err != nil {
		return err
	}

	if err := os.Remove(fullPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("delete file: %w", err)
	}

	return nil
}

// List returns all backup artifacts matching an optional key prefix.
func (s *LocalStorage) List(ctx context.Context, prefix string) ([]*models.StorageObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stdPrefix := s.toStandardKey(prefix)
	var objects []*models.StorageObject

	err := filepath.WalkDir(s.baseDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if d.IsDir() {
			return nil
		}

		// Ignore temporary in-flight files
		if strings.Contains(d.Name(), ".tmp.") {
			return nil
		}

		relPath, err := filepath.Rel(s.baseDir, path)
		if err != nil {
			return err
		}

		key := s.toStandardKey(relPath)
		if stdPrefix != "" && !strings.HasPrefix(key, stdPrefix) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		objects = append(objects, &models.StorageObject{
			Key:         key,
			SizeBytes:   info.Size(),
			ModTime:     info.ModTime(),
			StorageType: models.StorageLocal,
		})

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("list directory: %w", err)
	}

	return objects, nil
}

// Stat returns metadata for a single stored object.
func (s *LocalStorage) Stat(ctx context.Context, key string) (*models.StorageObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fullPath, err := s.resolvePath(key)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("stat file: %w", err)
	}

	return &models.StorageObject{
		Key:         s.toStandardKey(key),
		SizeBytes:   info.Size(),
		ModTime:     info.ModTime(),
		StorageType: models.StorageLocal,
	}, nil
}

// resolvePath validates and calculates the absolute path, preventing directory traversal.
func (s *LocalStorage) resolvePath(key string) (string, error) {
	cleanKey := strings.TrimSpace(key)
	if cleanKey == "" {
		return "", ErrInvalidKey
	}

	// Normalize slashes for filesystem
	cleanKey = filepath.Clean(cleanKey)
	if cleanKey == "." {
		// The storage root itself is not an object.
		return "", ErrInvalidKey
	}

	// Disallow absolute and rooted keys directly. On Windows "\etc\passwd" is rooted
	// but not absolute (no volume), so it is checked explicitly for consistent
	// behaviour across platforms.
	if filepath.IsAbs(cleanKey) || strings.HasPrefix(cleanKey, "/") || strings.HasPrefix(cleanKey, `\`) {
		return "", fmt.Errorf("%w: absolute paths are forbidden", ErrPathTraversal)
	}

	// Reject anything else that is not a plain relative path: ".." components,
	// volume names ("C:x") and, on Windows, reserved device names ("NUL", "COM1").
	if !filepath.IsLocal(cleanKey) {
		return "", ErrPathTraversal
	}

	fullPath := filepath.Join(s.baseDir, cleanKey)

	// Ensure fullPath is strictly located within baseDir
	rel, err := filepath.Rel(s.baseDir, fullPath)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return "", ErrPathTraversal
	}

	return fullPath, nil
}

// toStandardKey normalizes OS-specific path separators to standard forward slashes.
func (s *LocalStorage) toStandardKey(path string) string {
	return filepath.ToSlash(strings.TrimPrefix(path, "/"))
}

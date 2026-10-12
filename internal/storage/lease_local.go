package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Compile-time check that LocalStorage supports conditional writes.
var _ LeaseStore = (*LocalStorage)(nil)

// leaseLockSuffix names the sidecar lock file of a conditional object.
const leaseLockSuffix = ".lock"

// leaseLockPoll is how often a conditional operation retries a held lock.
const leaseLockPoll = 10 * time.Millisecond

// ReadVersioned returns the content of key and its SHA-256 (hex) as the version.
func (s *LocalStorage) ReadVersioned(ctx context.Context, key string) ([]byte, string, error) {
	var data []byte
	err := s.withLeaseLock(ctx, key, func(path string) error {
		var readErr error
		data, readErr = readLeaseFile(path)
		return readErr
	})
	if err != nil {
		return nil, "", err
	}
	return data, contentVersion(data), nil
}

// CreateIfAbsent creates key with O_EXCL under the sidecar lock.
func (s *LocalStorage) CreateIfAbsent(ctx context.Context, key string, data []byte) (string, error) {
	if len(data) > MaxLeaseObjectSize {
		return "", ErrObjectTooLarge
	}
	err := s.withLeaseLock(ctx, key, func(path string) error {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return ErrPreconditionFailed
			}
			return fmt.Errorf("create %s: %w", filepath.Base(path), err)
		}
		if _, err = f.Write(data); err == nil {
			err = f.Sync()
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
			return fmt.Errorf("write %s: %w", filepath.Base(path), err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return contentVersion(data), nil
}

// ReplaceIfMatch replaces key under the sidecar lock when the hash of its content
// is version; the new content is written to a temporary file and renamed into place.
func (s *LocalStorage) ReplaceIfMatch(ctx context.Context, key string, data []byte, version string) (string, error) {
	if len(data) > MaxLeaseObjectSize {
		return "", ErrObjectTooLarge
	}
	err := s.withLeaseLock(ctx, key, func(path string) error {
		current, err := readLeaseFile(path)
		if errors.Is(err, ErrNotFound) {
			return ErrPreconditionFailed
		}
		if err != nil {
			return err
		}
		if contentVersion(current) != version {
			return ErrPreconditionFailed
		}
		return writeFileAtomic(path, data)
	})
	if err != nil {
		return "", err
	}
	return contentVersion(data), nil
}

// withLeaseLock runs fn with the path of key while holding an exclusive lock on its
// sidecar file, waiting for the lock until ctx ends.
func (s *LocalStorage) withLeaseLock(ctx context.Context, key string, fn func(path string) error) error {
	path, err := s.resolvePath(key)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	lock, err := os.OpenFile(path+leaseLockSuffix, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer func() { _ = lock.Close() }()
	for {
		locked, lockErr := tryFileLock(lock)
		if lockErr != nil {
			return fmt.Errorf("lock %s: %w", filepath.Base(lock.Name()), lockErr)
		}
		if locked {
			break
		}
		timer := time.NewTimer(leaseLockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer func() { _ = fileUnlock(lock) }()
	if err = ctx.Err(); err != nil {
		return err
	}
	return fn(path)
}

// readLeaseFile reads a conditional object of at most MaxLeaseObjectSize bytes.
func readLeaseFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxLeaseObjectSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if len(data) > MaxLeaseObjectSize {
		return nil, ErrObjectTooLarge
	}
	return data, nil
}

// writeFileAtomic writes data to a temporary file next to path, syncs it and renames
// it over path. The temporary name contains ".tmp." so List skips it.
func writeFileAtomic(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.tmp.%d", path, time.Now().UnixNano())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	return nil
}

// contentVersion is the version of a local conditional object: the SHA-256 of its
// content.
func contentVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

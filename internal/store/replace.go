package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// BackupSuffix names the copy of the metadata database that ReplaceDatabase keeps.
const BackupSuffix = ".bak"

// ErrInvalidReplacement is returned by ReplaceDatabase when the new database cannot
// be used: it does not open with the store's options (another secret key, a newer
// schema) or fails its integrity check.
var ErrInvalidReplacement = errors.New("store: the replacement database cannot be used")

// ReplaceDatabase replaces the metadata database at path with the database file
// newPath, such as a metadata snapshot of another instance. newPath is checked
// first: it must open with opts (WithSecretBox checks the secret key), migrate to
// this version's schema and pass PRAGMA quick_check. The previous database is kept
// as path + BackupSuffix (with its write-ahead log), and newPath is renamed over
// path, so a crash leaves either database in place, never none. No store may have
// path open.
func ReplaceDatabase(ctx context.Context, path, newPath string, logger *slog.Logger, opts ...Option) error {
	if logger == nil {
		logger = slog.Default()
	}
	candidate, err := OpenSQLite(ctx, newPath, logger, opts...)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidReplacement, err)
	}
	var check string
	err = candidate.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check)
	closeErr := candidate.Close()
	switch {
	case err != nil:
		return fmt.Errorf("%w: integrity check: %w", ErrInvalidReplacement, err)
	case check != "ok":
		return fmt.Errorf("%w: integrity check: %s", ErrInvalidReplacement, check)
	case closeErr != nil:
		return closeErr
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err = removeIfExists(newPath + suffix); err != nil {
			return err
		}
	}

	bak := path + BackupSuffix
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err = removeIfExists(bak + suffix); err != nil {
			return err
		}
	}
	if _, err = os.Stat(path); err == nil {
		// A hard link keeps the old database under the backup name at no cost; the
		// rename below then replaces only the name path.
		if linkErr := os.Link(path, bak); linkErr != nil {
			if err = copyFile(path, bak); err != nil {
				return fmt.Errorf("store: keep the previous database: %w", err)
			}
		}
		// SQLite pairs the log with the database by name: the old log follows the
		// old database.
		if err = os.Rename(path+"-wal", bak+"-wal"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("store: keep the previous database log: %w", err)
		}
		if err = removeIfExists(path + "-shm"); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: stat the database: %w", err)
	}
	if err = os.Rename(newPath, path); err != nil {
		return fmt.Errorf("store: replace the database: %w", err)
	}
	if err = restrictPermissions(path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	logger.Info("replaced the metadata database", slog.String("path", path), slog.String("previous", bak))
	return nil
}

// removeIfExists removes path, ignoring a missing file.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: remove %s: %w", filepath.Base(path), err)
	}
	return nil
}

// copyFile copies src to the new file dst (mode 0600).
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec // G304: the database path of the data directory.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, dbFileMode)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// syncDir flushes a directory entry change to disk where the platform allows it.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // G304: the data directory.
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

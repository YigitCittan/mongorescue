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
	"sort"
	"strings"
	"time"
)

// BackupInfix separates the database file name from the time and epoch of a copy
// that ReplaceDatabase keeps: mongorescue.db.bak-20261012T100000Z-epoch7.
const BackupInfix = ".bak-"

// DefaultBackupsKept is how many copies of replaced databases ReplaceDatabase keeps
// by default.
const DefaultBackupsKept = 3

// ErrInvalidReplacement is returned by ReplaceDatabase when the new database cannot
// be used: it does not open with the store's options (another secret key, a newer
// schema) or fails its integrity check.
var ErrInvalidReplacement = errors.New("store: the replacement database cannot be used")

// Replacement describes a ReplaceDatabase call.
type Replacement struct {
	// Path is the metadata database; NewPath the database file replacing it.
	Path    string
	NewPath string
	// Epoch is the HA epoch of the replacement; it names the copy of the previous
	// database.
	Epoch uint64
	// Keep is how many copies of replaced databases are kept (DefaultBackupsKept
	// when 0); the oldest are removed.
	Keep int
	// Logger receives operational logs; nil means slog.Default().
	Logger *slog.Logger
	// Now is the clock of the copy's name; nil means time.Now.
	Now func() time.Time
}

// ReplaceDatabase replaces the metadata database at r.Path with the database file
// r.NewPath, such as a metadata snapshot of another instance, and returns the path
// of the copy of the previous database. r.NewPath is checked first: it must open
// with opts (WithSecretBox checks the secret key), migrate to this version's
// schema and pass PRAGMA quick_check. The previous database is never lost: it is
// kept as r.Path + BackupInfix + "<UTC time>-epoch<N>" (with its write-ahead log),
// and only the oldest copies beyond r.Keep are removed. No store may have r.Path
// open.
func ReplaceDatabase(ctx context.Context, r Replacement, opts ...Option) (string, error) {
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Keep <= 0 {
		r.Keep = DefaultBackupsKept
	}
	candidate, err := OpenSQLite(ctx, r.NewPath, r.Logger, opts...)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidReplacement, err)
	}
	var check string
	err = candidate.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check)
	closeErr := candidate.Close()
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: integrity check: %w", ErrInvalidReplacement, err)
	case check != "ok":
		return "", fmt.Errorf("%w: integrity check: %s", ErrInvalidReplacement, check)
	case closeErr != nil:
		return "", closeErr
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err = removeIfExists(r.NewPath + suffix); err != nil {
			return "", err
		}
	}

	bak := ""
	if _, err = os.Stat(r.Path); err == nil {
		bak = fmt.Sprintf("%s%s%s-epoch%d", r.Path, BackupInfix, r.Now().UTC().Format("20060102T150405Z"), r.Epoch)
		if _, statErr := os.Stat(bak); statErr == nil {
			bak += "-" + strconvNano(r.Now())
		}
		// A hard link keeps the old database under the backup name at no cost; the
		// rename below then replaces only the name r.Path.
		if linkErr := os.Link(r.Path, bak); linkErr != nil {
			if err = copyFile(r.Path, bak); err != nil {
				return "", fmt.Errorf("store: keep the previous database: %w", err)
			}
		}
		// SQLite pairs the log with the database by name: the old log follows the
		// old database.
		if err = os.Rename(r.Path+"-wal", bak+"-wal"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("store: keep the previous database log: %w", err)
		}
		if err = removeIfExists(r.Path + "-shm"); err != nil {
			return "", err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("store: stat the database: %w", err)
	}
	if err = os.Rename(r.NewPath, r.Path); err != nil {
		return "", fmt.Errorf("store: replace the database: %w", err)
	}
	if err = restrictPermissions(r.Path); err != nil {
		return "", err
	}
	syncDir(filepath.Dir(r.Path))
	pruneBackups(r.Path, r.Keep, r.Logger)
	r.Logger.Info("replaced the metadata database", slog.String("path", r.Path), slog.String("previous", bak))
	return bak, nil
}

// strconvNano returns t as Unix nanoseconds, to make a backup name unique.
func strconvNano(t time.Time) string { return fmt.Sprintf("%d", t.UnixNano()) }

// DatabaseBackups returns the copies of replaced databases kept next to path,
// oldest first.
func DatabaseBackups(path string) ([]string, error) {
	matches, err := filepath.Glob(path + BackupInfix + "*")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range matches {
		if strings.HasSuffix(m, "-wal") || strings.HasSuffix(m, "-shm") {
			continue
		}
		out = append(out, m)
	}
	sort.Strings(out) // the UTC time in the name sorts them
	return out, nil
}

// pruneBackups removes the oldest copies beyond keep, with their logs.
func pruneBackups(path string, keep int, logger *slog.Logger) {
	backups, err := DatabaseBackups(path)
	if err != nil || len(backups) <= keep {
		return
	}
	for _, old := range backups[:len(backups)-keep] {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if rmErr := removeIfExists(old + suffix); rmErr != nil {
				logger.Warn("cannot remove an old copy of the metadata database", slog.String("path", old+suffix), slog.Any("error", rmErr))
			}
		}
	}
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

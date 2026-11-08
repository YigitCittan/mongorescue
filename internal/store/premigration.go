package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// ErrPreMigrationCopy is returned by OpenSQLite when the copy of the database taken
// before pending schema migrations cannot be written. The database is left
// unmigrated: free disk space, fix the directory permissions, or start with the
// copies turned off knowingly.
var ErrPreMigrationCopy = errors.New("store: cannot copy the database before migrating it")

// preMigrationInfix separates the database file name from the schema versions in the
// name of a pre-migration copy: mongorescue.db.pre-v<from>-to-v<to>.
const preMigrationInfix = ".pre-v"

// preMigrationTmpSuffix marks a copy that is still being written; rotation ignores it
// and the next attempt removes it.
const preMigrationTmpSuffix = ".partial"

// WithPreMigrationCopies makes OpenSQLite copy an existing database to
// <database>.pre-v<from>-to-v<to> (VACUUM INTO, so a consistent and compact copy,
// written to disk before the first migration runs) whenever schema migrations are
// pending, and keep only the newest keep copies. A database without any applied
// migration (a fresh one) is never copied. keep <= 0 turns the copies off. A copy
// that cannot be written fails the open with ErrPreMigrationCopy instead of
// migrating without one. A database that predates the sealing of stored credentials
// (no secrets format marker) is not copied, with a warning, so no copy ever holds
// plaintext secrets. secret.key is not part of the copy.
func WithPreMigrationCopies(keep int) Option {
	return func(s *SQLiteStore) { s.keepCopies = keep }
}

// PreMigrationCopyPath returns the path of the copy taken before migrating the
// database at dbPath from schema version from to version to.
func PreMigrationCopyPath(dbPath string, from, to int) string {
	return fmt.Sprintf("%s%s%d-to-v%d", dbPath, preMigrationInfix, from, to)
}

// preMigrationCopy copies the database before migrations from..to run, then drops the
// oldest copies beyond the configured count.
func (s *SQLiteStore) preMigrationCopy(ctx context.Context, from, to int) error {
	// A database that has not been through the one-time sealing of secrets (see
	// upgradeSecrets) may hold connection URIs, webhook secrets and S3 keys in
	// plaintext. VACUUM INTO would write them, unprotected by secret.key, to a second
	// file next to the live database, while opening the upgraded database seals only
	// the live one. Sealing the copy instead would make it unreadable by the release
	// that expects plaintext, so no copy is taken.
	sealed, err := s.secretsSealedOnDisk(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPreMigrationCopy, err)
	}
	if !sealed {
		s.logger.Warn("not copying the metadata database before migrating it: it predates the encryption of stored credentials and a copy would hold them in plaintext; to be able to roll back, restore from your own backup of the data directory",
			slog.Int("from_version", from), slog.Int("to_version", to))
		return nil
	}
	dest := PreMigrationCopyPath(s.path, from, to)
	tmp := dest + preMigrationTmpSuffix
	// VACUUM INTO refuses an existing target: clear leftovers of an earlier attempt.
	for _, p := range []string{tmp, dest} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: remove %s: %w", ErrPreMigrationCopy, p, err)
		}
	}
	// VACUUM INTO takes its target file name as an expression, so it is bound.
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: %w", ErrPreMigrationCopy, err)
	}
	if err := finishCopy(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: %w", ErrPreMigrationCopy, err)
	}
	s.logger.Info("copied the metadata database before migrating it",
		slog.String("copy", dest), slog.Int("from_version", from), slog.Int("to_version", to))
	if err := s.rotatePreMigrationCopies(); err != nil {
		s.logger.Warn("could not remove old pre-migration copies", slog.Any("error", err))
	}
	return nil
}

// secretsSealedOnDisk reports whether the database carries the secrets format marker,
// recorded in the same transaction that sealed every stored secret. A database
// without it (or from before the settings table existed) may hold plaintext secrets.
func (s *SQLiteStore) secretsSealedOnDisk(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'settings'").Scan(&n); err != nil {
		return false, fmt.Errorf("inspect schema: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", secretsFormatSetting).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read secrets format: %w", err)
	}
	return v == secretsFormatCurrent, nil
}

// finishCopy restricts tmp to dbFileMode, flushes it to disk and renames it to dest,
// so dest only ever exists complete and durable.
func finishCopy(tmp, dest string) error {
	if err := restrictPermissions(tmp); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_RDWR, 0) //nolint:gosec // G304: a file next to the database.
	if err != nil {
		return fmt.Errorf("open copy: %w", err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("flush copy: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close copy: %w", err)
	}
	if err = os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("rename copy: %w", err)
	}
	syncDir(filepath.Dir(dest))
	return nil
}

type preMigrationFile struct {
	path     string
	from, to int
	modTime  int64
}

// listPreMigrationCopies returns the copies of the database, oldest first: by target
// schema version, then source version, then modification time.
func (s *SQLiteStore) listPreMigrationCopies() ([]preMigrationFile, error) {
	dir, base := filepath.Split(s.path)
	if dir == "" {
		dir = "."
	}
	pattern := regexp.MustCompile("^" + regexp.QuoteMeta(base+preMigrationInfix) + `(\d+)-to-v(\d+)$`)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	var out []preMigrationFile
	for _, e := range entries {
		m := pattern.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		from, _ := strconv.Atoi(m[1])
		to, _ := strconv.Atoi(m[2])
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, preMigrationFile{path: filepath.Join(dir, e.Name()), from: from, to: to, modTime: info.ModTime().UnixNano()})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.to != b.to {
			return a.to < b.to
		}
		if a.from != b.from {
			return a.from < b.from
		}
		return a.modTime < b.modTime
	})
	return out, nil
}

// rotatePreMigrationCopies removes the oldest copies beyond s.keepCopies.
func (s *SQLiteStore) rotatePreMigrationCopies() error {
	copies, err := s.listPreMigrationCopies()
	if err != nil {
		return err
	}
	var errs []error
	for len(copies) > s.keepCopies {
		if err := os.Remove(copies[0].path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
		copies = copies[1:]
	}
	return errors.Join(errs...)
}

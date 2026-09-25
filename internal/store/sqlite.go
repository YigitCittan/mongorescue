package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	// Registers the pure-Go (CGO-free) "sqlite" database/sql driver.
	_ "modernc.org/sqlite"

	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

const (
	// dbFileMode is the permission of the database and its -wal/-shm files, which hold
	// notification secrets and MongoDB URIs.
	dbFileMode fs.FileMode = 0o600
	// dbDirMode is the permission of a data directory created by OpenSQLite.
	dbDirMode fs.FileMode = 0o750
	// busyTimeoutMillis bounds how long a connection waits for a lock held by another
	// connection (for example a second process) before failing with SQLITE_BUSY.
	busyTimeoutMillis = 5000
)

// Compile-time checks that SQLiteStore serves both the metadata and the notification ports.
var (
	_ Store             = (*SQLiteStore)(nil)
	_ notify.Repository = (*SQLiteStore)(nil)
)

// SQLiteStore implements Store and notify.Repository on an embedded SQLite database in
// WAL mode. It is safe for concurrent use.
//
// The pool holds a single connection: SQLite allows one writer at a time, and metadata
// traffic is small, so serialising every statement through one connection removes
// SQLITE_BUSY contention inside the process without measurable cost. busy_timeout
// still covers locks held by other processes opening the same file.
type SQLiteStore struct {
	db     *sql.DB
	path   string
	logger *slog.Logger
	// box encrypts connection URIs and notification channel secrets at rest.
	box *secretbox.Box

	closeOnce sync.Once
	closeErr  error
}

// Option customises OpenSQLite.
type Option func(*SQLiteStore)

// WithSecretBox enables encryption of credentials at rest with box. On open, the key
// is checked against the key check value stored in the database (returning an error
// wrapping secretbox.ErrSecretKeyMismatch on mismatch) and legacy plaintext secrets are
// encrypted. Without a box, operations touching secrets fail with ErrNoSecretBox.
func WithSecretBox(box *secretbox.Box) Option {
	return func(s *SQLiteStore) { s.box = box }
}

// OpenSQLite opens (creating when needed) the metadata database at path, restricts
// its files to mode 0600 and applies all pending schema migrations. The caller must
// Close the returned store.
func OpenSQLite(ctx context.Context, path string, logger *slog.Logger, opts ...Option) (*SQLiteStore, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if path == "" || strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("%w: %q", ErrInvalidPath, path)
	}
	cleanPath := filepath.Clean(path)

	if err := os.MkdirAll(filepath.Dir(cleanPath), dbDirMode); err != nil {
		return nil, fmt.Errorf("store: create database directory: %w", err)
	}
	// Create the file ourselves so its mode never depends on the process umask.
	if err := ensureFile(cleanPath); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dataSourceName(cleanPath))
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	s := &SQLiteStore{db: db, path: cleanPath, logger: logger}
	for _, opt := range opts {
		opt(s)
	}
	if err := s.init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// init verifies the connection, applies migrations and tightens file permissions.
func (s *SQLiteStore) init(ctx context.Context) error {
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("store: open database %s: %w", s.path, err)
	}
	if !strings.EqualFold(mode, "wal") {
		s.logger.Warn("metadata database is not in WAL mode; the file system may not support it",
			slog.String("path", s.path), slog.String("journal_mode", mode))
	}
	if err := s.migrate(ctx); err != nil {
		return err
	}
	if err := restrictPermissions(s.path); err != nil {
		return err
	}
	if s.box != nil {
		upgraded, err := s.secretsUpgraded(ctx)
		if err != nil {
			return err
		}
		if err := s.checkSecretKey(ctx, upgraded); err != nil {
			return err
		}
		if !upgraded {
			// One-time upgrade; it records the format marker in the same transaction.
			return s.upgradeSecrets(ctx)
		}
		// Once upgraded, a secret field that is not sealed in the current format was
		// planted: refuse to start instead of accepting (and re-sealing) it.
		if err := s.verifySecretsSealed(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Path returns the cleaned file path of the database.
func (s *SQLiteStore) Path() string { return s.path }

// Close optimises query planner statistics and closes the database, checkpointing
// the write-ahead log. It is safe to call more than once.
func (s *SQLiteStore) Close() error {
	s.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.db.ExecContext(ctx, "PRAGMA optimize"); err != nil {
			s.logger.Debug("metadata database optimize failed", slog.Any("error", err))
		}
		if err := s.db.Close(); err != nil {
			s.closeErr = fmt.Errorf("store: close database: %w", err)
		}
	})
	return s.closeErr
}

// dataSourceName builds the driver DSN for path with the pragmas every connection needs.
// _txlock=immediate makes write transactions take the write lock at BEGIN, so they
// wait on busy_timeout instead of failing when upgrading from a read lock.
func dataSourceName(path string) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	// Overwrite deleted content so revoked sessions and replaced secrets do not linger
	// in free pages of the file.
	q.Add("_pragma", "secure_delete(ON)")
	q.Set("_txlock", "immediate")
	return path + "?" + q.Encode()
}

// ensureFile creates path with dbFileMode when it does not exist yet.
func ensureFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, dbFileMode)
	if err != nil {
		return fmt.Errorf("store: create database file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: create database file: %w", err)
	}
	return nil
}

// restrictPermissions forces dbFileMode on the database and its WAL and shared-memory
// files. Windows has no POSIX modes, so it is a no-op there.
func restrictPermissions(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, dbFileMode); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("store: restrict permissions of %s: %w", p, err)
		}
	}
	return nil
}

// execer runs statements on the database or inside a transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// queryer runs queries on the database or inside a transaction.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// withTx runs fn in a write transaction, committing when it returns nil and rolling
// back otherwise. A panic in fn rolls the transaction back before it propagates, so
// the single pooled connection is never left holding an open transaction.
func (s *SQLiteStore) withTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback() // the original error matters; rollback of a failed tx adds nothing
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}
	return nil
}

// getRecord loads the JSON data column of the single row selected by query and
// decodes it into a new T, returning notFound when there is no such row.
func getRecord[T any](ctx context.Context, q queryer, notFound error, query string, args ...any) (*T, error) {
	var data string
	if err := q.QueryRowContext(ctx, query, args...).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFound
		}
		return nil, fmt.Errorf("store: query: %w", err)
	}
	return decode[T](data)
}

// listRecords decodes the JSON data column of every row selected by query. It never
// returns a nil slice, so empty results serialise as [].
func listRecords[T any](ctx context.Context, q queryer, query string, args ...any) ([]*T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	list := make([]*T, 0)
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("store: scan row: %w", err)
		}
		v, err := decode[T](data)
		if err != nil {
			return nil, err
		}
		list = append(list, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate rows: %w", err)
	}
	return list, nil
}

// decode unmarshals a stored JSON record.
func decode[T any](data string) (*T, error) {
	v := new(T)
	if err := json.Unmarshal([]byte(data), v); err != nil {
		return nil, fmt.Errorf("store: decode record: %w", err)
	}
	return v, nil
}

// encode marshals a record for the data column.
func encode(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("store: encode record: %w", err)
	}
	return string(b), nil
}

// execOne runs a statement that must affect exactly one row, returning notFound when
// it affected none.
func execOne(ctx context.Context, e execer, notFound error, query string, args ...any) error {
	res, err := e.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rows affected: %w", err)
	}
	if n == 0 {
		return notFound
	}
	return nil
}

// Bounds of time.Time values representable as int64 Unix nanoseconds.
var (
	minNanoTime = time.Unix(0, math.MinInt64)
	maxNanoTime = time.Unix(0, math.MaxInt64)
)

// timeKey converts t into the sortable integer stored in timestamp columns, clamping
// values outside the int64 nanosecond range (such as the zero time).
func timeKey(t time.Time) int64 {
	switch {
	case t.Before(minNanoTime):
		return math.MinInt64
	case t.After(maxNanoTime):
		return math.MaxInt64
	default:
		return t.UnixNano()
	}
}

// boolInt converts b into SQLite's integer boolean.
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

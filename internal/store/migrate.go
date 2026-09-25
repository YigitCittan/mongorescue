package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// migrationFiles holds the versioned schema migrations, named NNNN_description.sql.
// Released migrations must never be edited; add a new file instead.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migration is one embedded schema migration.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations parses the embedded migrations, sorted by version.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read embedded migrations: %w", err)
	}
	list := make([]migration, 0, len(entries))
	seen := make(map[int]string, len(entries))
	for _, e := range entries {
		name := e.Name()
		prefix, _, ok := strings.Cut(name, "_")
		version, err := strconv.Atoi(prefix)
		if !ok || err != nil || version <= 0 {
			return nil, fmt.Errorf("store: migration %q must be named NNNN_description.sql", name)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: migrations %q and %q share version %d", prev, name, version)
		}
		seen[version] = name
		body, err := fs.ReadFile(migrationFiles, path.Join("migrations", name))
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", name, err)
		}
		list = append(list, migration{version: version, name: strings.TrimSuffix(name, ".sql"), sql: string(body)})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].version < list[j].version })
	return list, nil
}

// migrate brings the schema up to date. Each migration runs in its own transaction
// together with its schema_migrations row, so a failed migration leaves no trace and
// re-opening an up-to-date database changes nothing.
func (s *SQLiteStore) migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY NOT NULL,
		name       TEXT    NOT NULL,
		applied_at TEXT    NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	var current int
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if latest := migrations[len(migrations)-1].version; current > latest {
		return fmt.Errorf("%w: database is at version %d, this binary knows up to %d", ErrSchemaTooNew, current, latest)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		applied := false
		err := s.withTx(ctx, func(tx *sql.Tx) error {
			// Re-check inside the write transaction: another process may have
			// applied the migration since the version was read.
			var n int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = ?", m.version).Scan(&n); err != nil {
				return fmt.Errorf("store: check migration %s: %w", m.name, err)
			}
			if n > 0 {
				return nil
			}
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("store: apply migration %s: %w", m.name, err)
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
				m.version, m.name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return fmt.Errorf("store: record migration %s: %w", m.name, err)
			}
			applied = true
			return nil
		})
		if err != nil {
			return err
		}
		if applied {
			s.logger.Info("applied metadata schema migration",
				slog.Int("version", m.version), slog.String("name", m.name), slog.String("path", s.path))
		}
	}
	return nil
}

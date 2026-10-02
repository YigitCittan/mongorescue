package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
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
	version  int
	name     string
	sql      string
	checksum string // hex SHA-256 of sql
}

// migrationChecksum returns the checksum recorded for a migration's SQL.
func migrationChecksum(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
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
		list = append(list, migration{version: version, name: strings.TrimSuffix(name, ".sql"), sql: string(body), checksum: migrationChecksum(body)})
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
	// Refuse edited migrations before anything is written.
	if err := s.verifyMigrationChecksums(ctx, migrations); err != nil {
		return err
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
			hasChecksum, err := checksumColumnExists(ctx, tx)
			if err != nil {
				return err
			}
			appliedAt := time.Now().UTC().Format(time.RFC3339Nano)
			if hasChecksum {
				_, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at, checksum) VALUES (?, ?, ?, ?)",
					m.version, m.name, appliedAt, m.checksum)
			} else {
				_, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
					m.version, m.name, appliedAt)
			}
			if err != nil {
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
	return s.backfillMigrationChecksums(ctx, migrations)
}

// checksumColumnExists reports whether schema_migrations has its checksum column,
// added by migration 0014.
func checksumColumnExists(ctx context.Context, q queryer) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pragma_table_info('schema_migrations') WHERE name = 'checksum'").Scan(&n); err != nil {
		return false, fmt.Errorf("store: inspect schema_migrations: %w", err)
	}
	return n > 0, nil
}

// verifyMigrationChecksums compares the checksum recorded for every applied
// migration with the SQL embedded in this binary. Rows without a checksum (applied
// before migration 0014 and not yet backfilled) are skipped. A mismatch, or an
// applied version this binary does not embed, returns ErrMigrationChanged.
func (s *SQLiteStore) verifyMigrationChecksums(ctx context.Context, migrations []migration) error {
	hasChecksum, err := checksumColumnExists(ctx, s.db)
	if err != nil || !hasChecksum {
		return err
	}
	embedded := make(map[int]migration, len(migrations))
	for _, m := range migrations {
		embedded[m.version] = m
	}
	rows, err := s.db.QueryContext(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return fmt.Errorf("store: read applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			version        int
			name, recorded string
		)
		if err = rows.Scan(&version, &name, &recorded); err != nil {
			return fmt.Errorf("store: read applied migrations: %w", err)
		}
		m, ok := embedded[version]
		if !ok {
			return fmt.Errorf("%w: applied migration %s (version %d) is not part of this binary", ErrMigrationChanged, name, version)
		}
		if recorded != "" && recorded != m.checksum {
			return fmt.Errorf("%w: migration %s was applied with checksum %s but this binary embeds %s; "+
				"released migrations must never be edited, run the MongoRescue release that applied it or restore the original file",
				ErrMigrationChanged, m.name, recorded, m.checksum)
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("store: read applied migrations: %w", err)
	}
	return nil
}

// backfillMigrationChecksums records the embedded checksum for applied migrations
// that have none: rows written before migration 0014 added the column. It trusts
// the SQL embedded in the running binary, which applied or inherited them.
func (s *SQLiteStore) backfillMigrationChecksums(ctx context.Context, migrations []migration) error {
	hasChecksum, err := checksumColumnExists(ctx, s.db)
	if err != nil || !hasChecksum {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, m := range migrations {
			if _, err := tx.ExecContext(ctx, "UPDATE schema_migrations SET checksum = ? WHERE version = ? AND checksum = ''",
				m.checksum, m.version); err != nil {
				return fmt.Errorf("store: record checksum of migration %s: %w", m.name, err)
			}
		}
		return nil
	})
}

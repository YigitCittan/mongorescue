package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// LegacyStateFileName is the JSON metadata file used by releases before the SQLite store.
const LegacyStateFileName = "state.json"

// legacyArchiveSuffix is appended, with a UTC timestamp, to an imported state file.
const legacyArchiveSuffix = ".migrated-"

// legacyState is the schema of state.json. Notification fields were added after the
// first release, so files written by older versions omit them.
type legacyState struct {
	Jobs     map[string]*legacyJob            `json:"jobs"`
	Backups  map[string]*models.BackupRecord  `json:"backups"`
	Restores map[string]*models.RestoreRecord `json:"restores"`
	Channels map[string]*notify.Channel       `json:"notification_channels,omitempty"`
	Rules    map[string]*notify.Rule          `json:"notification_rules,omitempty"`
}

// legacyJob is a job as written by releases before managed connections: it may carry
// its own connection string, which MigrateLegacyJobURIs turns into a Connection.
type legacyJob struct {
	models.Job
	MongoURI string `json:"mongo_uri,omitempty"`
}

// LegacyImport reports the outcome of a successful MigrateLegacyState.
type LegacyImport struct {
	// Source is the imported state file.
	Source string
	// ArchivedAs is where Source was renamed to; empty if the rename failed.
	ArchivedAs string
	// Jobs, Backups, Restores, Channels and Rules count the imported records.
	Jobs, Backups, Restores, Channels, Rules int
}

// MigrateLegacyState imports a state.json file written by an older release.
//
// It does nothing (and returns nil, nil) when statePath does not exist, or when the
// database already holds data, in which case the file is ignored with a warning. Else
// every record is inserted in one transaction, the row counts are verified, and the
// file is renamed to <statePath>.migrated-<UTC timestamp>. On any import error the
// transaction is rolled back, the file is left untouched and the error wraps
// ErrLegacyImport.
func (s *SQLiteStore) MigrateLegacyState(ctx context.Context, statePath string) (*LegacyImport, error) {
	statePath = filepath.Clean(statePath)
	if _, err := os.Stat(statePath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: stat %s: %w", ErrLegacyImport, statePath, err)
	}

	hasData, err := s.hasData(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLegacyImport, err)
	}
	if hasData {
		s.logger.Warn("ignoring legacy state file: the metadata database already has data; remove or archive the file",
			slog.String("state_file", statePath), slog.String("database", s.path))
		return nil, nil
	}

	state, err := readLegacyState(statePath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLegacyImport, err)
	}

	result := &LegacyImport{Source: statePath}
	skipped := false
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		// Re-check under the write lock in case another process imported meanwhile.
		has, checkErr := s.hasData(ctx, tx)
		if checkErr != nil || has {
			skipped = has
			return checkErr
		}
		return s.importLegacyState(ctx, tx, state, result)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrLegacyImport, statePath, err)
	}
	if skipped {
		s.logger.Warn("ignoring legacy state file: the metadata database already has data; remove or archive the file",
			slog.String("state_file", statePath), slog.String("database", s.path))
		return nil, nil
	}

	archived := statePath + legacyArchiveSuffix + time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(statePath, archived); err != nil {
		// The data is safely committed; on the next start the file is ignored with a warning.
		s.logger.Warn("imported legacy state file but could not archive it; remove it manually",
			slog.String("state_file", statePath), slog.Any("error", err))
	} else {
		result.ArchivedAs = archived
	}

	s.logger.Info("migrated legacy state.json into the metadata database",
		slog.String("state_file", statePath),
		slog.String("archived_as", result.ArchivedAs),
		slog.String("database", s.path),
		slog.Int("jobs", result.Jobs),
		slog.Int("backups", result.Backups),
		slog.Int("restores", result.Restores),
		slog.Int("notification_channels", result.Channels),
		slog.Int("notification_rules", result.Rules),
	)
	return result, nil
}

// readLegacyState parses a state file. An empty file is an empty state, as it was for
// the JSON store.
func readLegacyState(path string) (*legacyState, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var state legacyState
	if len(content) > 0 {
		if err := json.Unmarshal(content, &state); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	return &state, nil
}

// importLegacyState inserts every record of state through tx and verifies that each
// table holds exactly the imported number of rows. Records without an ID take their
// map key, which is how the JSON store keyed them.
func (s *SQLiteStore) importLegacyState(ctx context.Context, tx *sql.Tx, state *legacyState, result *LegacyImport) error {
	var err error
	if result.Jobs, err = importMap(ctx, tx, "jobs", state.Jobs,
		func(j *legacyJob, key string) string { j.ID = cmp.Or(j.ID, key); return j.ID }, s.putLegacyJob); err != nil {
		return err
	}
	if result.Backups, err = importMap(ctx, tx, "backups", state.Backups,
		func(b *models.BackupRecord, key string) string { b.ID = cmp.Or(b.ID, key); return b.ID }, putBackup); err != nil {
		return err
	}
	if result.Restores, err = importMap(ctx, tx, "restores", state.Restores,
		func(r *models.RestoreRecord, key string) string { r.ID = cmp.Or(r.ID, key); return r.ID }, putRestore); err != nil {
		return err
	}
	if result.Channels, err = importMap(ctx, tx, "notification_channels", state.Channels,
		func(c *notify.Channel, key string) string { c.ID = cmp.Or(c.ID, key); return c.ID }, s.putChannel); err != nil {
		return err
	}
	if result.Rules, err = importMap(ctx, tx, "notification_rules", state.Rules,
		func(r *notify.Rule, key string) string { r.ID = cmp.Or(r.ID, key); return r.ID }, putRule); err != nil {
		return err
	}
	return nil
}

// putLegacyJob stores a legacy job with its connection string encrypted in the
// mongo_uri field, for MigrateLegacyJobURIs to pick up.
func (s *SQLiteStore) putLegacyJob(ctx context.Context, e execer, j *legacyJob) error {
	if j.MongoURI == "" {
		return putJob(ctx, e, &j.Job)
	}
	if s.box == nil {
		return ErrNoSecretBox
	}
	sealed, err := s.seal(secretbox.At(tableJobs, j.ID, fieldLegacyJobURI), j.MongoURI)
	if err != nil {
		return err
	}
	stored := *j
	stored.MongoURI = sealed
	data, err := encode(stored)
	if err != nil {
		return err
	}
	if _, err := e.ExecContext(ctx, upsertJobSQL,
		j.ID, j.Name, j.Database, boolInt(j.Enabled), timeKey(j.CreatedAt), j.ConnectionID, j.StorageTargetID, data); err != nil {
		return fmt.Errorf("store: save job %s: %w", j.ID, err)
	}
	return nil
}

// importMap writes the non-nil values of records with put and checks the row count of
// table afterwards. fixID fills a missing ID from the map key and returns the effective
// ID. Two entries resolving to the same ID are rejected with an error naming the table,
// the ID and both map keys, since one would silently overwrite the other.
func importMap[T any](ctx context.Context, tx *sql.Tx, table string, records map[string]*T,
	fixID func(*T, string) string, put func(context.Context, execer, *T) error,
) (int, error) {
	keys := slices.Sorted(maps.Keys(records))
	seen := make(map[string]string, len(records)) // effective ID -> map key
	for _, key := range keys {
		rec := records[key]
		if rec == nil {
			continue
		}
		id := fixID(rec, key)
		if prev, dup := seen[id]; dup {
			return 0, fmt.Errorf("%s: duplicate id %q (entries %q and %q)", table, id, prev, key)
		}
		seen[id] = key
		if err := put(ctx, tx, rec); err != nil {
			return 0, err
		}
	}
	var got int
	// table is one of the fixed names above, never user input.
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&got); err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	if got != len(seen) {
		return 0, fmt.Errorf("verify %s: imported %d records but the table holds %d", table, len(seen), got)
	}
	return got, nil
}

// hasData reports whether any metadata table holds a row.
func (s *SQLiteStore) hasData(ctx context.Context, q queryer) (bool, error) {
	var has bool
	err := q.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM jobs) OR EXISTS (SELECT 1 FROM backups) OR EXISTS (SELECT 1 FROM restores)
		OR EXISTS (SELECT 1 FROM notification_channels) OR EXISTS (SELECT 1 FROM notification_rules)
		OR EXISTS (SELECT 1 FROM connections)`).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("check for existing data: %w", err)
	}
	return has, nil
}

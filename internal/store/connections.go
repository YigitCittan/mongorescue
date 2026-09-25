package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// Compile-time check that SQLiteStore serves the connections port.
var _ connections.Repository = (*SQLiteStore)(nil)

const upsertConnectionSQL = `INSERT INTO connections (id, name, data) VALUES (?, ?, ?)
	ON CONFLICT (id) DO UPDATE SET name = excluded.name, data = excluded.data`

// ListConnections returns all connections sorted by name, with decrypted URIs.
func (s *SQLiteStore) ListConnections(ctx context.Context) ([]*models.Connection, error) {
	list, err := listRecords[models.Connection](ctx, s.db, "SELECT data FROM connections ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	for _, c := range list {
		if err := s.openConnection(c); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// GetConnection returns a connection with its decrypted URI or connections.ErrNotFound.
func (s *SQLiteStore) GetConnection(ctx context.Context, id string) (*models.Connection, error) {
	c, err := getRecord[models.Connection](ctx, s.db, connections.ErrNotFound, "SELECT data FROM connections WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if err := s.openConnection(c); err != nil {
		return nil, err
	}
	return c, nil
}

// SaveConnection creates or replaces a connection, encrypting its URI.
func (s *SQLiteStore) SaveConnection(ctx context.Context, c *models.Connection) error {
	if c == nil || c.ID == "" {
		return fmt.Errorf("%w: connection with ID is required", ErrInvalidRecord)
	}
	return s.putConnection(ctx, s.db, c)
}

// DeleteConnection removes a connection. In the same transaction it refuses with
// connections.ErrInUse while any job references it.
func (s *SQLiteStore) DeleteConnection(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var jobs int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE connection_id = ?", id).Scan(&jobs); err != nil {
			return fmt.Errorf("store: count jobs of connection: %w", err)
		}
		if jobs > 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM connections WHERE id = ?", id).Scan(&exists); err != nil {
				return fmt.Errorf("store: check connection: %w", err)
			}
			if exists == 0 {
				return connections.ErrNotFound
			}
			return fmt.Errorf("%w (%d jobs)", connections.ErrInUse, jobs)
		}
		return execOne(ctx, tx, connections.ErrNotFound, "DELETE FROM connections WHERE id = ?", id)
	})
}

// putConnection encrypts the URI of c and upserts it.
func (s *SQLiteStore) putConnection(ctx context.Context, e execer, c *models.Connection) error {
	if s.box == nil {
		return ErrNoSecretBox
	}
	sealed := *c
	var err error
	if sealed.URI, err = s.seal(secretbox.At(tableConnections, c.ID, fieldConnectionURI), c.URI); err != nil {
		return err
	}
	data, err := encode(&sealed)
	if err != nil {
		return err
	}
	if _, err := e.ExecContext(ctx, upsertConnectionSQL, c.ID, c.Name, data); err != nil {
		return fmt.Errorf("store: save connection %s: %w", c.ID, err)
	}
	return nil
}

// openConnection decrypts c.URI in place.
func (s *SQLiteStore) openConnection(c *models.Connection) error {
	if s.box == nil {
		return ErrNoSecretBox
	}
	uri, err := s.open(secretbox.At(tableConnections, c.ID, fieldConnectionURI), c.URI)
	if err != nil {
		return err
	}
	c.URI = uri
	return nil
}

// LegacyURIMigration reports what MigrateLegacyJobURIs changed.
type LegacyURIMigration struct {
	// ConnectionsCreated counts connections created for distinct legacy URIs.
	ConnectionsCreated int
	// JobsAssigned counts jobs that received a connection_id.
	JobsAssigned int
	// JobsUnassigned counts jobs left without a connection (no URI and no default).
	JobsUnassigned int
}

// MigrateLegacyJobURIs assigns a connection to every job that has none, in one
// transaction. A job's own legacy mongo_uri (or, without one, defaultURI, the former
// server-wide default) is matched against existing connections by exact URI; one new
// connection is created per distinct URI, named after its host list ("default" for
// defaultURI). The legacy URI is removed from the job, and backup records of migrated
// jobs get the connection ID and name. Jobs with neither are left unassigned.
func (s *SQLiteStore) MigrateLegacyJobURIs(ctx context.Context, defaultURI string) (LegacyURIMigration, error) {
	var res LegacyURIMigration
	if s.box == nil {
		return res, ErrNoSecretBox
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := listRawRows(ctx, tx, "SELECT id, data FROM jobs WHERE connection_id = '' ORDER BY id")
		if err != nil || len(rows) == 0 {
			return err
		}
		existing, err := listRecords[models.Connection](ctx, tx, "SELECT data FROM connections ORDER BY json_extract(data, '$.created_at'), id")
		if err != nil {
			return err
		}
		byURI := make(map[string]*models.Connection, len(existing))
		for _, c := range existing {
			if err = s.openConnection(c); err != nil {
				return err
			}
			if _, dup := byURI[c.URI]; !dup {
				byURI[c.URI] = c
			}
		}

		for _, row := range rows {
			var legacy legacyJob
			if err = json.Unmarshal([]byte(row.data), &legacy); err != nil {
				return fmt.Errorf("store: decode job %s: %w", row.id, err)
			}
			uri, name := legacy.MongoURI, ""
			if uri != "" {
				// Legacy job URIs are sealed by the state.json import and the one-time
				// format upgrade; anything else was planted and is refused.
				if uri, err = s.open(secretbox.At(tableJobs, row.id, fieldLegacyJobURI), uri); err != nil {
					return err
				}
				name = connections.HostList(uri)
			} else if defaultURI != "" {
				uri, name = defaultURI, "default"
			} else {
				res.JobsUnassigned++
				continue
			}

			conn, ok := byURI[uri]
			if !ok {
				id, err := connections.NewID()
				if err != nil {
					return err
				}
				now := time.Now().UTC()
				conn = &models.Connection{ID: id, Name: name, URI: uri, CreatedAt: now, UpdatedAt: now,
					Description: "Migrated from a job connection string"}
				if uri == defaultURI && legacy.MongoURI == "" {
					conn.Description = "Migrated from MONGORESCUE_MONGO_URI"
				}
				if err := s.putConnection(ctx, tx, conn); err != nil {
					return err
				}
				byURI[uri] = conn
				res.ConnectionsCreated++
			}

			job := legacy.Job
			job.ConnectionID = conn.ID
			if err := putJob(ctx, tx, &job); err != nil { // re-encoding drops mongo_uri
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE backups SET connection_id = ?,
				data = json_set(data, '$.connection_id', ?, '$.connection_name', ?)
				WHERE job_id = ? AND connection_id = ''`, conn.ID, conn.ID, conn.Name, job.ID); err != nil {
				return fmt.Errorf("store: assign connection to backups of job %s: %w", job.ID, err)
			}
			res.JobsAssigned++
		}
		return nil
	})
	if err != nil {
		return LegacyURIMigration{}, err
	}
	if res.JobsAssigned > 0 || res.JobsUnassigned > 0 {
		s.logger.Info("migrated legacy job connection strings to managed connections",
			slog.Int("connections_created", res.ConnectionsCreated),
			slog.Int("jobs_assigned", res.JobsAssigned),
			slog.Int("jobs_without_connection", res.JobsUnassigned))
	}
	return res, nil
}

// rawRow is an (id, data) pair read without decoding.
type rawRow struct {
	id, data string
}

// listRawRows reads (id, data) pairs.
func listRawRows(ctx context.Context, q queryer, query string, args ...any) ([]rawRow, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []rawRow
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(&r.id, &r.data); err != nil {
			return nil, fmt.Errorf("store: scan row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate rows: %w", err)
	}
	return out, nil
}

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
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// Compile-time check that SQLiteStore serves the connections port.
var _ connections.Repository = (*SQLiteStore)(nil)

const upsertConnectionSQL = `INSERT INTO connections (id, name, data) VALUES (?, ?, ?)
	ON CONFLICT (id) DO UPDATE SET name = excluded.name, data = excluded.data`

// storedConnection is the stored form of a connection. Its post-restore commands
// are an erasure log (identifiers of people whose data was erased), so they are
// sealed like a credential, as one JSON value bound to the connection's ID, and
// never stored in plain form.
type storedConnection struct {
	models.Connection
	// PostRestoreSealed holds Connection.PostRestoreCommands, sealed.
	PostRestoreSealed string `json:"post_restore_sealed,omitempty"`
}

// ListConnections returns all connections sorted by name, with decrypted URIs and
// post-restore commands.
func (s *SQLiteStore) ListConnections(ctx context.Context) ([]*models.Connection, error) {
	defer s.lockKey()()
	stored, err := listRecords(ctx, s, tableConnections, s.openStoredConnection, "SELECT id, data FROM connections ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	out := make([]*models.Connection, 0, len(stored))
	for _, sc := range stored {
		out = append(out, &sc.Connection)
	}
	return out, nil
}

// GetConnection returns a connection with its decrypted URI and post-restore
// commands, or connections.ErrNotFound.
func (s *SQLiteStore) GetConnection(ctx context.Context, id string) (*models.Connection, error) {
	defer s.lockKey()()
	sc, err := getRecord[storedConnection](ctx, s.db, connections.ErrNotFound, "SELECT data FROM connections WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if err := s.openStoredConnection(sc); err != nil {
		return nil, err
	}
	return &sc.Connection, nil
}

// SaveConnection creates or replaces a connection, encrypting its URI.
func (s *SQLiteStore) SaveConnection(ctx context.Context, c *models.Connection) error {
	defer s.lockKey()()
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

// putConnection seals the URI, the post-restore commands and the TLS client key
// and password of c and upserts it.
func (s *SQLiteStore) putConnection(ctx context.Context, e execer, c *models.Connection) error {
	if s.box == nil {
		return ErrNoSecretBox
	}
	sc := storedConnection{Connection: *c}
	var err error
	if len(c.PostRestoreCommands) > 0 {
		var raw []byte
		raw, err = json.Marshal(c.PostRestoreCommands)
		if err != nil {
			return fmt.Errorf("store: encode post-restore commands of %s: %w", c.ID, err)
		}
		if sc.PostRestoreSealed, err = s.seal(secretbox.At(tableConnections, c.ID, fieldConnectionPostRestore), string(raw)); err != nil {
			return err
		}
	}
	sc.PostRestoreCommands = nil
	for _, f := range connectionTLSSecrets(&sc.Connection) {
		if *f.value, err = s.seal(secretbox.At(tableConnections, c.ID, f.field), *f.value); err != nil {
			return err
		}
	}
	return s.putStoredConnection(ctx, e, &sc)
}

// tlsSecret is a sealed TLS field of a connection: its binding field name and a
// pointer to its value.
type tlsSecret struct {
	field string
	value *string
}

// connectionTLSSecrets returns the TLS fields of c that are sealed at rest: the
// client key and its password.
func connectionTLSSecrets(c *models.Connection) []tlsSecret {
	return []tlsSecret{
		{fieldConnectionTLSKey, &c.ClientKeyPEM},
		{fieldConnectionTLSKeyPassword, &c.ClientKeyPassword},
	}
}

// putStoredConnection seals the plain URI of sc and upserts it; its post-restore
// commands (PostRestoreSealed) and TLS client key and password must be sealed
// already.
func (s *SQLiteStore) putStoredConnection(ctx context.Context, e execer, sc *storedConnection) error {
	if s.box == nil {
		return ErrNoSecretBox
	}
	if len(sc.PostRestoreCommands) > 0 {
		return fmt.Errorf("%w: post-restore commands of %s are not sealed", ErrInvalidRecord, sc.ID)
	}
	for _, f := range connectionTLSSecrets(&sc.Connection) {
		if *f.value != "" && !secretbox.IsSealed(*f.value) {
			return fmt.Errorf("%w: %s of %s is not sealed", ErrInvalidRecord, f.field, sc.ID)
		}
	}
	sealed := *sc
	var err error
	if sealed.URI, err = s.seal(secretbox.At(tableConnections, sc.ID, fieldConnectionURI), sc.URI); err != nil {
		return err
	}
	data, err := encode(&sealed)
	if err != nil {
		return err
	}
	if _, err := e.ExecContext(ctx, upsertConnectionSQL, sc.ID, sc.Name, data); err != nil {
		return fmt.Errorf("store: save connection %s: %w", sc.ID, err)
	}
	return nil
}

// openStoredConnection decrypts the URI and the post-restore commands of sc in
// place. Post-restore commands stored in plain form are refused
// (ErrUnsealedSecret): they may have been planted.
func (s *SQLiteStore) openStoredConnection(sc *storedConnection) error {
	if err := s.openConnection(&sc.Connection); err != nil {
		return err
	}
	at := secretbox.At(tableConnections, sc.ID, fieldConnectionPostRestore)
	if len(sc.PostRestoreCommands) > 0 {
		return fmt.Errorf("%w: %s", ErrUnsealedSecret, at)
	}
	plain, err := s.open(at, sc.PostRestoreSealed)
	if err != nil || plain == "" {
		return err
	}
	if err = json.Unmarshal([]byte(plain), &sc.PostRestoreCommands); err != nil {
		return unreadable(fmt.Errorf("store: decode post-restore commands of %s: %w", sc.ID, err))
	}
	sc.PostRestoreSealed = ""
	return nil
}

// openConnection decrypts c.URI and the TLS client key and password of c in place.
// Plain (unsealed) values are refused (ErrUnsealedSecret).
func (s *SQLiteStore) openConnection(c *models.Connection) error {
	if s.box == nil {
		return ErrNoSecretBox
	}
	uri, err := s.open(secretbox.At(tableConnections, c.ID, fieldConnectionURI), c.URI)
	if err != nil {
		return err
	}
	for _, f := range connectionTLSSecrets(c) {
		if *f.value, err = s.open(secretbox.At(tableConnections, c.ID, f.field), *f.value); err != nil {
			return err
		}
	}
	// A connection with TLS material always asks for TLS in its URI (see
	// mongotools.EnsureTLS), also when it was saved before that was enforced.
	if !c.IsZero() {
		uri = mongotools.EnsureTLS(uri)
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
		existing, err := listRecordsStrict[models.Connection](ctx, tx, "SELECT data FROM connections ORDER BY json_extract(data, '$.created_at'), id")
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

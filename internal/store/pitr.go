package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// Compile-time check that SQLiteStore serves the PITR port.
var _ pitr.Repository = (*SQLiteStore)(nil)

const (
	streamColumns = `id, connection_id, replica_set, target_id, enabled, base_cron, base_keep_count,
		base_keep_days, oplog_max_days, chunk_seconds, data, created_at, updated_at`

	chunkColumns = `id, stream_id, chain_id, target_id, storage_key, from_t, from_i, to_t, to_i,
		first_term, last_term, entries, size_bytes, sha256, encrypted, encryption_mode, status,
		created_at, verified_at, verify_error, deleted_at, purge_after, version_id, retain_until`
)

// streamData is the JSON data column of pitr_streams: the options without a column.
type streamData struct {
	BaseOnGap      bool   `json:"base_on_gap"`
	ReadPreference string `json:"read_preference,omitempty"`
	ChainTestCron  string `json:"chain_test_cron,omitempty"`
	ChainTestConn  string `json:"chain_test_connection_id,omitempty"`
}

// oplogTS converts a stored integer pair to a BSON timestamp.
func oplogTS(t, i int64) pitr.Timestamp { return pitr.Timestamp{T: uint32(t), I: uint32(i)} } //nolint:gosec // written from uint32 values

// CreateStream inserts a PITR stream, setting CreatedAt (when zero) and UpdatedAt.
// It returns pitr.ErrAlreadyExists for a taken ID and pitr.ErrConnectionTaken when
// the connection already has a stream.
func (s *SQLiteStore) CreateStream(ctx context.Context, st *pitr.Stream) error {
	if st == nil || st.ID == "" || st.ConnectionID == "" {
		return fmt.Errorf("%w: PITR stream with ID and connection is required", ErrInvalidRecord)
	}
	data, err := json.Marshal(streamData{BaseOnGap: st.BaseOnGap, ReadPreference: st.ReadPreference, ChainTestCron: st.ChainTestCron, ChainTestConn: st.ChainTestConnectionID})
	if err != nil {
		return fmt.Errorf("store: encode PITR stream %s: %w", st.ID, err)
	}
	now := time.Now().UTC()
	created := st.CreatedAt
	if created.IsZero() {
		created = now
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO pitr_streams ("+streamColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		st.ID, st.ConnectionID, st.ReplicaSet, st.TargetID, st.Enabled, st.BaseCron, st.BaseKeepCount,
		st.BaseKeepDays, st.OplogMaxDays, st.ChunkSeconds, string(data), timeKey(created), timeKey(now))
	if err != nil {
		return streamWriteError(err, st.ID, "create")
	}
	st.CreatedAt, st.UpdatedAt = created, now
	return nil
}

// UpdateStream replaces a PITR stream, setting UpdatedAt and reading back the stored
// CreatedAt. It returns pitr.ErrNotFound for a deleted stream and
// pitr.ErrConnectionTaken when moving it to a connection that has a stream.
func (s *SQLiteStore) UpdateStream(ctx context.Context, st *pitr.Stream) error {
	if st == nil || st.ID == "" || st.ConnectionID == "" {
		return fmt.Errorf("%w: PITR stream with ID and connection is required", ErrInvalidRecord)
	}
	data, err := json.Marshal(streamData{BaseOnGap: st.BaseOnGap, ReadPreference: st.ReadPreference, ChainTestCron: st.ChainTestCron, ChainTestConn: st.ChainTestConnectionID})
	if err != nil {
		return fmt.Errorf("store: encode PITR stream %s: %w", st.ID, err)
	}
	now := time.Now().UTC()
	var created int64
	err = s.db.QueryRowContext(ctx, `UPDATE pitr_streams SET connection_id = ?, replica_set = ?, target_id = ?,
			enabled = ?, base_cron = ?, base_keep_count = ?, base_keep_days = ?, oplog_max_days = ?,
			chunk_seconds = ?, data = ?, updated_at = ?
		WHERE id = ? RETURNING created_at`,
		st.ConnectionID, st.ReplicaSet, st.TargetID, st.Enabled, st.BaseCron, st.BaseKeepCount,
		st.BaseKeepDays, st.OplogMaxDays, st.ChunkSeconds, string(data), timeKey(now), st.ID).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return pitr.ErrNotFound
	}
	if err != nil {
		return streamWriteError(err, st.ID, "update")
	}
	st.CreatedAt, st.UpdatedAt = fromKey(created), now
	return nil
}

// streamWriteError maps a failed insert or update of a stream to the port's errors.
func streamWriteError(err error, id, op string) error {
	if isUniqueViolation(err) {
		if strings.Contains(err.Error(), "connection_id") {
			return pitr.ErrConnectionTaken
		}
		return pitr.ErrAlreadyExists
	}
	return fmt.Errorf("store: %s PITR stream %s: %w", op, id, err)
}

// GetStream returns a PITR stream or pitr.ErrNotFound.
func (s *SQLiteStore) GetStream(ctx context.Context, id string) (*pitr.Stream, error) {
	return scanStream(s.db.QueryRowContext(ctx, "SELECT "+streamColumns+" FROM pitr_streams WHERE id = ?", id))
}

// GetStreamByConnection returns the PITR stream of a connection or pitr.ErrNotFound.
func (s *SQLiteStore) GetStreamByConnection(ctx context.Context, connectionID string) (*pitr.Stream, error) {
	return scanStream(s.db.QueryRowContext(ctx, "SELECT "+streamColumns+" FROM pitr_streams WHERE connection_id = ?", connectionID))
}

// ListStreams returns every PITR stream ordered by ID.
func (s *SQLiteStore) ListStreams(ctx context.Context) ([]*pitr.Stream, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+streamColumns+" FROM pitr_streams ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("store: list PITR streams: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*pitr.Stream{}
	for rows.Next() {
		st, scanErr := scanStream(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, st)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list PITR streams: %w", err)
	}
	return out, nil
}

// scanStream reads one pitr_streams row selected with streamColumns.
func scanStream(r rowScanner) (*pitr.Stream, error) {
	var (
		st               pitr.Stream
		data             string
		created, updated int64
	)
	err := r.Scan(&st.ID, &st.ConnectionID, &st.ReplicaSet, &st.TargetID, &st.Enabled, &st.BaseCron,
		&st.BaseKeepCount, &st.BaseKeepDays, &st.OplogMaxDays, &st.ChunkSeconds, &data, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, pitr.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read PITR stream: %w", err)
	}
	var d streamData
	if err = json.Unmarshal([]byte(data), &d); err != nil {
		return nil, fmt.Errorf("%w: PITR stream %s: %w", ErrCorruptRecord, st.ID, err)
	}
	st.BaseOnGap, st.ReadPreference, st.ChainTestCron = d.BaseOnGap, d.ReadPreference, d.ChainTestCron
	st.ChainTestConnectionID = d.ChainTestConn
	st.CreatedAt, st.UpdatedAt = fromKey(created), fromKey(updated)
	return &st, nil
}

// DeleteStream removes a PITR stream with its chains, collector state and pruned
// chunk rows, in one transaction. It returns pitr.ErrNotFound for an unknown stream
// and pitr.ErrInUse, deleting nothing, while committed or superseded chunks remain.
func (s *SQLiteStore) DeleteStream(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := streamExists(ctx, tx, id); err != nil {
			return err
		}
		var stored int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM oplog_chunks WHERE stream_id = ? AND status <> ?",
			id, string(pitr.ChunkPruned)).Scan(&stored); err != nil {
			return fmt.Errorf("store: count chunks of PITR stream %s: %w", id, err)
		}
		if stored > 0 {
			return fmt.Errorf("%w: %d chunks", pitr.ErrInUse, stored)
		}
		for _, q := range []string{
			"DELETE FROM oplog_chunks WHERE stream_id = ?",
			"DELETE FROM pitr_chains WHERE stream_id = ?",
			"DELETE FROM pitr_state WHERE stream_id = ?",
			"DELETE FROM pitr_streams WHERE id = ?",
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return fmt.Errorf("store: delete PITR stream %s: %w", id, err)
			}
		}
		return nil
	})
}

// streamExists returns pitr.ErrNotFound unless stream id exists.
func streamExists(ctx context.Context, q queryer, id string) error {
	var n int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM pitr_streams WHERE id = ?", id).Scan(&n); err != nil {
		return fmt.Errorf("store: check PITR stream %s: %w", id, err)
	}
	if n == 0 {
		return pitr.ErrNotFound
	}
	return nil
}

// StartChain inserts an open chain starting at start.TS and points the stream's
// collector state at it (Last = start; a new state is running), in one transaction.
// It returns pitr.ErrNotFound for an unknown stream, pitr.ErrChainOpen while the
// stream has an open chain and pitr.ErrAlreadyExists for a taken chain ID.
func (s *SQLiteStore) StartChain(ctx context.Context, streamID, chainID string, start pitr.OpTime, at time.Time) error {
	if streamID == "" || chainID == "" {
		return fmt.Errorf("%w: chain with stream and chain ID is required", ErrInvalidRecord)
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := streamExists(ctx, tx, streamID); err != nil {
			return err
		}
		var open int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM pitr_chains WHERE stream_id = ? AND ended_at IS NULL",
			streamID).Scan(&open); err != nil {
			return fmt.Errorf("store: check open chains of PITR stream %s: %w", streamID, err)
		}
		if open > 0 {
			return pitr.ErrChainOpen
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO pitr_chains (stream_id, chain_id, start_t, start_i) VALUES (?, ?, ?, ?)",
			streamID, chainID, start.TS.T, start.TS.I); err != nil {
			if isUniqueViolation(err) {
				return pitr.ErrAlreadyExists
			}
			return fmt.Errorf("store: start chain %s of PITR stream %s: %w", chainID, streamID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pitr_state
				(stream_id, chain_id, last_t, last_i, last_term, status, last_error, lag_since, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, '', NULL, ?)
			ON CONFLICT (stream_id) DO UPDATE SET chain_id = excluded.chain_id, last_t = excluded.last_t,
				last_i = excluded.last_i, last_term = excluded.last_term, updated_at = excluded.updated_at`,
			streamID, chainID, start.TS.T, start.TS.I, start.Term, string(pitr.CollectorRunning), timeKey(at)); err != nil {
			return fmt.Errorf("store: move PITR state of stream %s to chain %s: %w", streamID, chainID, err)
		}
		return nil
	})
}

// EndChain records that an open chain ended at end for reason. It returns
// pitr.ErrNotFound for an unknown chain and pitr.ErrChainEnded for an ended one.
func (s *SQLiteStore) EndChain(ctx context.Context, streamID, chainID string, end pitr.Timestamp, reason pitr.EndReason, at time.Time) error {
	if reason == "" {
		return fmt.Errorf("%w: a chain needs an end reason", ErrInvalidRecord)
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := openChain(ctx, tx, streamID, chainID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE pitr_chains SET end_t = ?, end_i = ?, end_reason = ?, ended_at = ?
			WHERE stream_id = ? AND chain_id = ?`, end.T, end.I, string(reason), timeKey(at), streamID, chainID); err != nil {
			return fmt.Errorf("store: end chain %s of PITR stream %s: %w", chainID, streamID, err)
		}
		return nil
	})
}

// openChain returns pitr.ErrNotFound for an unknown chain and pitr.ErrChainEnded for
// an ended one.
func openChain(ctx context.Context, q queryer, streamID, chainID string) error {
	var ended sql.NullInt64
	err := q.QueryRowContext(ctx, "SELECT ended_at FROM pitr_chains WHERE stream_id = ? AND chain_id = ?",
		streamID, chainID).Scan(&ended)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return pitr.ErrNotFound
	case err != nil:
		return fmt.Errorf("store: read chain %s of PITR stream %s: %w", chainID, streamID, err)
	case ended.Valid:
		return pitr.ErrChainEnded
	}
	return nil
}

// ListChains returns the chains of a PITR stream ordered by start.
func (s *SQLiteStore) ListChains(ctx context.Context, streamID string) ([]*pitr.Chain, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT stream_id, chain_id, start_t, start_i, end_t, end_i, end_reason, ended_at
		FROM pitr_chains WHERE stream_id = ? ORDER BY start_t, start_i, chain_id`, streamID)
	if err != nil {
		return nil, fmt.Errorf("store: list chains of PITR stream %s: %w", streamID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []*pitr.Chain{}
	for rows.Next() {
		var (
			c                   pitr.Chain
			startT, startI      int64
			endT, endI, endedAt sql.NullInt64
			reason              sql.NullString
		)
		if err = rows.Scan(&c.StreamID, &c.ChainID, &startT, &startI, &endT, &endI, &reason, &endedAt); err != nil {
			return nil, fmt.Errorf("store: read chain of PITR stream %s: %w", streamID, err)
		}
		c.Start = oplogTS(startT, startI)
		if endedAt.Valid {
			c.End = oplogTS(endT.Int64, endI.Int64)
			c.EndReason = pitr.EndReason(reason.String)
			c.EndedAt = nullableKey(endedAt)
		}
		out = append(out, &c)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list chains of PITR stream %s: %w", streamID, err)
	}
	return out, nil
}

// CommitChunk inserts c as committed and moves the stream's collector state to
// (c.To, c.LastTerm), running and without error, in one transaction. The chunk must
// continue the state: the state's chain, still open, and c.From equal to the stored
// position. It sets CreatedAt (when zero) and Status. It returns pitr.ErrNotFound
// without a state, pitr.ErrChainEnded, pitr.ErrDiscontinuous, or
// pitr.ErrAlreadyExists for a recorded ID or target and storage key.
func (s *SQLiteStore) CommitChunk(ctx context.Context, c *pitr.Chunk) error {
	if c == nil || c.ID == "" || c.StreamID == "" || c.ChainID == "" || c.TargetID == "" || c.StorageKey == "" {
		return fmt.Errorf("%w: chunk with ID, stream, chain, target and storage key is required", ErrInvalidRecord)
	}
	if c.To.Compare(c.From) < 0 {
		return fmt.Errorf("%w: chunk %s ends at %s before its start %s", ErrInvalidRecord, c.ID, c.To, c.From)
	}
	created := c.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			chainID      string
			lastT, lastI int64
		)
		err := tx.QueryRowContext(ctx, "SELECT chain_id, last_t, last_i FROM pitr_state WHERE stream_id = ?",
			c.StreamID).Scan(&chainID, &lastT, &lastI)
		if errors.Is(err, sql.ErrNoRows) {
			return pitr.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read PITR state of stream %s: %w", c.StreamID, err)
		}
		if chainID != c.ChainID {
			return fmt.Errorf("%w: chain %s, current chain %s", pitr.ErrChainEnded, c.ChainID, chainID)
		}
		if err = openChain(ctx, tx, c.StreamID, c.ChainID); err != nil {
			return err
		}
		if last := oplogTS(lastT, lastI); last != c.From {
			return fmt.Errorf("%w: chunk %s starts at %s, the chain is at %s", pitr.ErrDiscontinuous, c.ID, c.From, last)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO oplog_chunks ("+chunkColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?)`,
			c.ID, c.StreamID, c.ChainID, c.TargetID, c.StorageKey, c.From.T, c.From.I, c.To.T, c.To.I,
			c.FirstTerm, c.LastTerm, c.Entries, c.SizeBytes, c.SHA256, c.Encrypted, c.EncryptionMode,
			string(pitr.ChunkCommitted), timeKey(created), nullTime(c.VerifiedAt), c.VerifyError,
			c.VersionID, nullTime(c.RetainUntil)); err != nil {
			if isUniqueViolation(err) {
				return pitr.ErrAlreadyExists
			}
			return fmt.Errorf("store: insert chunk %s: %w", c.ID, err)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE pitr_state SET last_t = ?, last_i = ?, last_term = ?, status = ?,
				last_error = '', updated_at = ?
			WHERE stream_id = ?`, c.To.T, c.To.I, c.LastTerm, string(pitr.CollectorRunning), timeKey(created), c.StreamID); err != nil {
			return fmt.Errorf("store: update PITR state of stream %s: %w", c.StreamID, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	c.CreatedAt, c.Status = created, pitr.ChunkCommitted
	return nil
}

// ListChunks returns the chunks selected by q ordered by From.
func (s *SQLiteStore) ListChunks(ctx context.Context, q pitr.ChunkQuery) ([]*pitr.Chunk, error) {
	if q.StreamID == "" || q.ChainID == "" {
		return nil, fmt.Errorf("%w: chunk query needs a stream and a chain", ErrInvalidRecord)
	}
	query := "SELECT " + chunkColumns + " FROM oplog_chunks WHERE stream_id = ? AND chain_id = ?"
	args := []any{q.StreamID, q.ChainID}
	if !q.After.IsZero() {
		query += " AND (to_t, to_i) > (?, ?)"
		args = append(args, q.After.T, q.After.I)
	}
	if !q.Until.IsZero() {
		query += " AND (from_t, from_i) < (?, ?)"
		args = append(args, q.Until.T, q.Until.I)
	}
	if q.Status != "" {
		query += " AND status = ?"
		args = append(args, string(q.Status))
	}
	if q.Live {
		query += " AND deleted_at IS NULL AND status != 'pruned'"
	}
	query += " ORDER BY from_t, from_i, to_t, to_i, id"
	return s.queryChunks(ctx, query, args...)
}

// queryChunks runs query, which selects chunkColumns, and scans the chunks.
func (s *SQLiteStore) queryChunks(ctx context.Context, query string, args ...any) ([]*pitr.Chunk, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list oplog chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*pitr.Chunk{}
	for rows.Next() {
		var (
			c                      pitr.Chunk
			fromT, fromI, toT, toI int64
			status                 string
			created                int64
			verified, deleted, pa  sql.NullInt64
			retain                 sql.NullInt64
		)
		if err = rows.Scan(&c.ID, &c.StreamID, &c.ChainID, &c.TargetID, &c.StorageKey, &fromT, &fromI, &toT, &toI,
			&c.FirstTerm, &c.LastTerm, &c.Entries, &c.SizeBytes, &c.SHA256, &c.Encrypted, &c.EncryptionMode,
			&status, &created, &verified, &c.VerifyError, &deleted, &pa, &c.VersionID, &retain); err != nil {
			return nil, fmt.Errorf("store: read oplog chunk: %w", err)
		}
		c.From, c.To = oplogTS(fromT, fromI), oplogTS(toT, toI)
		c.Status = pitr.ChunkStatus(status)
		c.CreatedAt, c.VerifiedAt = fromKey(created), nullableKey(verified)
		c.DeletedAt, c.PurgeAfter = nullableKey(deleted), nullableKey(pa)
		c.RetainUntil = nullableKey(retain)
		out = append(out, &c)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list oplog chunks: %w", err)
	}
	return out, nil
}

// ListStreamChunks returns a page of the chunks of a stream, newest first, and
// how many there are.
func (s *SQLiteStore) ListStreamChunks(ctx context.Context, streamID string, limit, offset int) ([]*pitr.Chunk, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM oplog_chunks WHERE stream_id = ?", streamID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count chunks of PITR stream %s: %w", streamID, err)
	}
	chunks, err := s.queryChunks(ctx, "SELECT "+chunkColumns+` FROM oplog_chunks WHERE stream_id = ?
		ORDER BY to_t DESC, to_i DESC, id DESC LIMIT ? OFFSET ?`, streamID, limit, offset)
	return chunks, total, err
}

// liveChunk selects the live committed chunks of the chain of row c.
const liveChunk = `stream_id = c.stream_id AND chain_id = c.chain_id AND status = 'committed' AND deleted_at IS NULL`

// ChainSpans returns, per chain of a stream, the span of its live committed chunks
// (not deleted, superseded or pruned).
func (s *SQLiteStore) ChainSpans(ctx context.Context, streamID string) ([]pitr.ChainSpan, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.chain_id,
			(SELECT from_t FROM oplog_chunks WHERE `+liveChunk+` ORDER BY from_t, from_i LIMIT 1),
			(SELECT from_i FROM oplog_chunks WHERE `+liveChunk+` ORDER BY from_t, from_i LIMIT 1),
			(SELECT to_t FROM oplog_chunks WHERE `+liveChunk+` ORDER BY to_t DESC, to_i DESC LIMIT 1),
			(SELECT to_i FROM oplog_chunks WHERE `+liveChunk+` ORDER BY to_t DESC, to_i DESC LIMIT 1),
			(SELECT COUNT(*) FROM oplog_chunks WHERE `+liveChunk+`),
			(SELECT COALESCE(SUM(size_bytes), 0) FROM oplog_chunks WHERE `+liveChunk+`)
		FROM pitr_chains c WHERE c.stream_id = ? ORDER BY c.start_t, c.start_i, c.chain_id`, streamID)
	if err != nil {
		return nil, fmt.Errorf("store: chain spans of PITR stream %s: %w", streamID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []pitr.ChainSpan{}
	for rows.Next() {
		var (
			sp                     pitr.ChainSpan
			fromT, fromI, toT, toI sql.NullInt64
		)
		if err = rows.Scan(&sp.ChainID, &fromT, &fromI, &toT, &toI, &sp.Chunks, &sp.SizeBytes); err != nil {
			return nil, fmt.Errorf("store: read a chain span: %w", err)
		}
		if sp.Chunks > 0 {
			sp.From, sp.To = oplogTS(fromT.Int64, fromI.Int64), oplogTS(toT.Int64, toI.Int64)
		}
		out = append(out, sp)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("store: chain spans of PITR stream %s: %w", streamID, err)
	}
	return out, nil
}

// MarkChunkVerified records the outcome of a chunk's verification; verifyError is
// empty when it passed.
func (s *SQLiteStore) MarkChunkVerified(ctx context.Context, id string, at time.Time, verifyError string) error {
	return execOne(ctx, s.db, pitr.ErrNotFound, "UPDATE oplog_chunks SET verified_at = ?, verify_error = ? WHERE id = ?",
		timeKey(at), verifyError, id)
}

// DeleteChunks soft-deletes the chunks ids that are neither deleted nor pruned:
// they leave every window, and their objects stay until purgeAfter. It returns
// how many it deleted.
func (s *SQLiteStore) DeleteChunks(ctx context.Context, ids []string, at, purgeAfter time.Time) (int64, error) {
	var n int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, `UPDATE oplog_chunks SET deleted_at = ?, purge_after = ?
				WHERE id = ? AND deleted_at IS NULL AND status != 'pruned'`, timeKey(at), timeKey(purgeAfter), id)
			if err != nil {
				return fmt.Errorf("store: delete oplog chunk %s: %w", id, err)
			}
			k, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: delete oplog chunk %s: %w", id, err)
			}
			n += k
		}
		return nil
	})
	return n, err
}

// ListPurgeableChunks returns up to limit deleted chunks of a stream whose grace
// period ended at now and whose object is still there, oldest first. A chunk whose
// object is still under its S3 Object Lock retention at now waits.
func (s *SQLiteStore) ListPurgeableChunks(ctx context.Context, streamID string, now time.Time, limit int) ([]*pitr.Chunk, error) {
	return s.queryChunks(ctx, "SELECT "+chunkColumns+` FROM oplog_chunks
		WHERE stream_id = ? AND purge_after IS NOT NULL AND purge_after <= ? AND status != 'pruned'
			AND (retain_until IS NULL OR retain_until <= ?)
		ORDER BY purge_after, id LIMIT ?`, streamID, timeKey(now), timeKey(now), limit)
}

// MarkChunkPruned records that the object of deleted chunk id was removed. It
// returns pitr.ErrNotFound unless the chunk is deleted and not pruned yet.
func (s *SQLiteStore) MarkChunkPruned(ctx context.Context, id string) error {
	return execOne(ctx, s.db, pitr.ErrNotFound, `UPDATE oplog_chunks SET status = 'pruned'
		WHERE id = ? AND deleted_at IS NOT NULL AND status != 'pruned'`, id)
}

// ListChunksToVerify returns up to limit live committed chunks last verified
// before before, those never verified first.
func (s *SQLiteStore) ListChunksToVerify(ctx context.Context, before time.Time, limit int) ([]*pitr.Chunk, error) {
	return s.queryChunks(ctx, "SELECT "+chunkColumns+` FROM oplog_chunks
		WHERE status = 'committed' AND deleted_at IS NULL AND (verified_at IS NULL OR verified_at < ?)
		ORDER BY verified_at IS NOT NULL, verified_at, created_at, id LIMIT ?`, timeKey(before), limit)
}

// SupersedeChunks marks the committed chunks of a chain whose To is after after as
// superseded and returns how many it marked.
func (s *SQLiteStore) SupersedeChunks(ctx context.Context, streamID, chainID string, after pitr.Timestamp) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE oplog_chunks SET status = ?
		WHERE stream_id = ? AND chain_id = ? AND status = ? AND (to_t, to_i) > (?, ?)`,
		string(pitr.ChunkSuperseded), streamID, chainID, string(pitr.ChunkCommitted), after.T, after.I)
	if err != nil {
		return 0, fmt.Errorf("store: supersede chunks of chain %s: %w", chainID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: supersede chunks of chain %s: %w", chainID, err)
	}
	return n, nil
}

// LoadState returns a stream's collector state or pitr.ErrNotFound.
func (s *SQLiteStore) LoadState(ctx context.Context, streamID string) (*pitr.State, error) {
	var (
		st           pitr.State
		lastT, lastI int64
		status       string
		lagSince     sql.NullInt64
		windowLow    sql.NullInt64
		updated      int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT stream_id, chain_id, last_t, last_i, last_term, status, last_error,
			lag_since, updated_at, window_low_since, replica_set_id
		FROM pitr_state WHERE stream_id = ?`, streamID).Scan(&st.StreamID, &st.ChainID, &lastT, &lastI,
		&st.Last.Term, &status, &st.LastError, &lagSince, &updated, &windowLow, &st.ReplicaSetID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, pitr.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read PITR state of stream %s: %w", streamID, err)
	}
	st.Last.TS = oplogTS(lastT, lastI)
	st.Status = pitr.CollectorStatus(status)
	st.LagSince, st.UpdatedAt = nullableKey(lagSince), fromKey(updated)
	st.WindowLowSince = nullableKey(windowLow)
	return &st, nil
}

// SetCollectorStatus records the collector's status, last error and lag start
// without moving its position. It returns pitr.ErrNotFound without a state.
func (s *SQLiteStore) SetCollectorStatus(ctx context.Context, streamID string, status pitr.CollectorStatus, lastError string, lagSince *time.Time, at time.Time) error {
	if status == "" {
		return fmt.Errorf("%w: collector status is required", ErrInvalidRecord)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE pitr_state SET status = ?, last_error = ?, lag_since = ?, updated_at = ?
		WHERE stream_id = ?`, string(status), lastError, nullTime(lagSince), timeKey(at), streamID)
	if err != nil {
		return fmt.Errorf("store: update PITR state of stream %s: %w", streamID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update PITR state of stream %s: %w", streamID, err)
	}
	if n == 0 {
		return pitr.ErrNotFound
	}
	return nil
}

// ListBaseBackups returns the PITR base backups (instance scope) of stream
// streamID, newest first.
func (s *SQLiteStore) ListBaseBackups(ctx context.Context, streamID string) ([]*models.BackupRecord, error) {
	return listRecords[models.BackupRecord](ctx, s, tableBackups, nil,
		`SELECT id, data FROM backups WHERE database_name = '' AND json_extract(data, '$.scope') = 'instance'
			AND json_extract(data, '$.pitr_stream_id') = ? ORDER BY started_at DESC, id DESC`, streamID)
}

// SetWindowLow records when the oplog headroom of a stream dropped below its
// threshold (nil: it is back above). It returns pitr.ErrNotFound without a state.
func (s *SQLiteStore) SetWindowLow(ctx context.Context, streamID string, since *time.Time) error {
	return execOne(ctx, s.db, pitr.ErrNotFound, "UPDATE pitr_state SET window_low_since = ? WHERE stream_id = ?",
		nullTime(since), streamID)
}

// SetReplicaSetID records the replica set ID the collector of a stream reads from.
// It returns pitr.ErrNotFound without a state.
func (s *SQLiteStore) SetReplicaSetID(ctx context.Context, streamID, replicaSetID string) error {
	return execOne(ctx, s.db, pitr.ErrNotFound, "UPDATE pitr_state SET replica_set_id = ? WHERE stream_id = ?",
		replicaSetID, streamID)
}

// ChunkKeys returns the storage keys of the oplog chunks on target targetID whose
// object is expected to exist: every chunk that is not pruned.
func (s *SQLiteStore) ChunkKeys(ctx context.Context, targetID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT storage_key FROM oplog_chunks WHERE target_id = ? AND status != 'pruned'", targetID)
	if err != nil {
		return nil, fmt.Errorf("store: list the chunk keys of target %s: %w", targetID, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("store: read a chunk key: %w", err)
		}
		out[key] = true
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the chunk keys of target %s: %w", targetID, err)
	}
	return out, nil
}

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

// chunkCopyWhereLive selects the copy rows (alias cc) whose chunk is live:
// committed and not deleted.
const chunkCopyWhereLive = `EXISTS (SELECT 1 FROM oplog_chunks c WHERE c.id = cc.chunk_id
	AND c.status = 'committed' AND c.deleted_at IS NULL)`

// PlanChunkCopies queues a copy of every live committed chunk of stream streamID
// on each of targets that has none, and drops the queued copies of the stream on
// other targets that never wrote an object (a copy target removed from the
// stream). It returns how many copies it queued.
func (s *SQLiteStore) PlanChunkCopies(ctx context.Context, streamID string, targets []models.CopyTarget) (int64, error) {
	var planned int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		keep := make([]string, 0, len(targets))
		for _, t := range targets {
			keep = append(keep, t.ID)
			res, err := tx.ExecContext(ctx, `INSERT INTO oplog_chunk_copies (chunk_id, stream_id, target_id, status, next_attempt_at, data)
				SELECT c.id, c.stream_id, ?, 'pending', NULL,
					json_object('target_id', ?, 'target_name', ?, 'storage_key', c.storage_key, 'status', 'pending', 'sha256_ok', json('false'))
				FROM oplog_chunks c
				WHERE c.stream_id = ? AND c.status = 'committed' AND c.deleted_at IS NULL AND c.target_id <> ?
					AND NOT EXISTS (SELECT 1 FROM oplog_chunk_copies cc WHERE cc.chunk_id = c.id AND cc.target_id = ?)`,
				t.ID, t.ID, t.Name, streamID, t.ID, t.ID)
			if err != nil {
				return fmt.Errorf("store: plan the chunk copies of PITR stream %s on %s: %w", streamID, t.ID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: rows affected: %w", err)
			}
			planned += n
		}
		query := `DELETE FROM oplog_chunk_copies WHERE stream_id = ? AND status IN ('pending', 'failed')
			AND coalesce(json_extract(data, '$.written'), 0) = 0 AND json_extract(data, '$.retain_until') IS NULL`
		args := []any{streamID}
		if len(keep) > 0 {
			query += " AND target_id NOT IN (" + placeholders(len(keep)) + ")" //nolint:gosec // placeholders only, the IDs are arguments
			for _, id := range keep {
				args = append(args, id)
			}
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("store: drop the chunk copies of PITR stream %s: %w", streamID, err)
		}
		return nil
	})
	return planned, err
}

// placeholders returns n comma-separated SQL placeholders.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// DueChunkCopies returns up to limit chunks with a copy that is due at now
// (models.CopyDue) and still live, oldest first.
func (s *SQLiteStore) DueChunkCopies(ctx context.Context, now time.Time, limit int) ([]*pitr.Chunk, error) {
	return s.queryChunks(ctx, "SELECT "+chunkColumns+` FROM oplog_chunks WHERE id IN (
			SELECT cc.chunk_id FROM oplog_chunk_copies cc
			WHERE ((cc.status = 'pending' AND (cc.next_attempt_at IS NULL OR cc.next_attempt_at <= ?))
				OR (cc.status = 'failed' AND cc.next_attempt_at IS NOT NULL AND cc.next_attempt_at <= ?))
				AND `+chunkCopyWhereLive+`)
		ORDER BY created_at, id LIMIT ?`, timeKey(now), timeKey(now), limit)
}

// CountWaitingChunkCopies returns how many chunk copies wait in the copy queue
// (pending, or failed with a next attempt).
func (s *SQLiteStore) CountWaitingChunkCopies(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oplog_chunk_copies cc
		WHERE (cc.status = 'pending' OR (cc.status = 'failed' AND cc.next_attempt_at IS NOT NULL)) AND `+chunkCopyWhereLive).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count the chunk copy queue: %w", err)
	}
	return n, nil
}

// ChunkCopiesOf returns the copies of the chunks ids by chunk ID, ordered by
// target.
func (s *SQLiteStore) ChunkCopiesOf(ctx context.Context, ids []string) (map[string][]*models.ChunkCopy, error) {
	out := map[string][]*models.ChunkCopy{}
	const batch = 500
	for start := 0; start < len(ids); start += batch {
		part := ids[start:min(start+batch, len(ids))]
		args := make([]any, len(part))
		for i, id := range part {
			args[i] = id
		}
		list, err := s.queryChunkCopies(ctx, `SELECT chunk_id, stream_id, data FROM oplog_chunk_copies
			WHERE chunk_id IN (`+placeholders(len(part))+`) ORDER BY chunk_id, target_id`, args...)
		if err != nil {
			return nil, err
		}
		for _, c := range list {
			out[c.ChunkID] = append(out[c.ChunkID], c)
		}
	}
	return out, nil
}

// PurgeableChunkCopies returns up to limit copies whose object may exist (not
// purged, see models.BackupCopy.MayExist) and whose chunk is gone: its row was
// removed, it was pruned, or it was deleted and its grace period ended at now.
// Copies whose Object Lock is known to last beyond now are left out.
func (s *SQLiteStore) PurgeableChunkCopies(ctx context.Context, now time.Time, limit int) ([]*models.ChunkCopy, error) {
	list, err := s.queryChunkCopies(ctx, `SELECT cc.chunk_id, cc.stream_id, cc.data FROM oplog_chunk_copies cc
		LEFT JOIN oplog_chunks c ON c.id = cc.chunk_id
		WHERE cc.status <> 'purged'
			AND (c.id IS NULL OR c.status = 'pruned' OR (c.purge_after IS NOT NULL AND c.purge_after <= ?))
		ORDER BY cc.chunk_id, cc.target_id`, timeKey(now))
	if err != nil {
		return nil, err
	}
	out := make([]*models.ChunkCopy, 0, min(len(list), limit))
	for _, c := range list {
		if len(out) == limit {
			break
		}
		if !c.LockedAt(now) {
			out = append(out, c)
		}
	}
	return out, nil
}

// UpdateChunkCopy applies fn to the copy of chunk chunkID on target targetID in one
// transaction and returns the stored result. It returns an error wrapping
// ErrNotFound when there is no such copy, and fn's error unchanged.
func (s *SQLiteStore) UpdateChunkCopy(ctx context.Context, chunkID, targetID string, fn func(*models.ChunkCopy) error) (*models.ChunkCopy, error) {
	var out *models.ChunkCopy
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var streamID, data string
		err := tx.QueryRowContext(ctx, "SELECT stream_id, data FROM oplog_chunk_copies WHERE chunk_id = ? AND target_id = ?",
			chunkID, targetID).Scan(&streamID, &data)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: copy of chunk %s on %s", ErrNotFound, chunkID, targetID)
		case err != nil:
			return fmt.Errorf("store: read the copy of chunk %s: %w", chunkID, err)
		}
		c := &models.ChunkCopy{ChunkID: chunkID, StreamID: streamID}
		if err = json.Unmarshal([]byte(data), &c.BackupCopy); err != nil {
			return fmt.Errorf("%w: copy of chunk %s: %w", ErrCorruptRecord, chunkID, err)
		}
		if err = fn(c); err != nil {
			return err
		}
		c.ChunkID, c.StreamID, c.TargetID = chunkID, streamID, targetID
		encoded, err := json.Marshal(c.BackupCopy)
		if err != nil {
			return fmt.Errorf("store: encode the copy of chunk %s: %w", chunkID, err)
		}
		var next any
		if c.NextAttemptAt != nil {
			next = timeKey(*c.NextAttemptAt)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE oplog_chunk_copies SET status = ?, next_attempt_at = ?, data = ?
			WHERE chunk_id = ? AND target_id = ?`, string(c.Status), next, string(encoded), chunkID, targetID); err != nil {
			return fmt.Errorf("store: update the copy of chunk %s: %w", chunkID, err)
		}
		out = c
		return nil
	})
	return out, err
}

// ChunkCopyKeys returns the storage keys of the chunk copies on a target whose
// object may exist (models.BackupCopy.MayExist).
func (s *SQLiteStore) ChunkCopyKeys(ctx context.Context, targetID string) (map[string]bool, error) {
	list, err := s.queryChunkCopies(ctx, `SELECT chunk_id, stream_id, data FROM oplog_chunk_copies
		WHERE target_id = ? AND status <> 'purged'`, targetID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, c := range list {
		if c.MayExist() {
			out[c.StorageKey] = true
		}
	}
	return out, nil
}

// queryChunkCopies runs a query selecting chunk_id, stream_id and data.
func (s *SQLiteStore) queryChunkCopies(ctx context.Context, query string, args ...any) ([]*models.ChunkCopy, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list chunk copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*models.ChunkCopy
	for rows.Next() {
		var data string
		c := &models.ChunkCopy{}
		if err = rows.Scan(&c.ChunkID, &c.StreamID, &data); err != nil {
			return nil, fmt.Errorf("store: read a chunk copy: %w", err)
		}
		if err = json.Unmarshal([]byte(data), &c.BackupCopy); err != nil {
			return nil, fmt.Errorf("%w: copy of chunk %s: %w", ErrCorruptRecord, c.ChunkID, err)
		}
		out = append(out, c)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list chunk copies: %w", err)
	}
	return out, nil
}

package store

import (
	"context"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// liveCopySQL matches a copy (c, a json_each row of data.copies) on storage target
// ? whose object exists or is about to be written: a done or pending copy, or any
// copy but a purged one that is still under its Object Lock (a failed copy wrote no
// object, see copies.Copy).
const liveCopySQL = `json_extract(c.value, '$.target_id') = ? AND (
		json_extract(c.value, '$.status') IN ('done', 'pending')
		OR (json_extract(c.value, '$.status') != 'purged' AND json_extract(c.value, '$.retain_until') IS NOT NULL))`

// PendingCopyRecords returns the completed backups with a copy that is pending or
// failed (the copy queue), oldest first. The queue decides from each copy's
// NextAttemptAt whether it is due.
func (s *SQLiteStore) PendingCopyRecords(ctx context.Context) ([]*models.BackupRecord, error) {
	return listRecords[models.BackupRecord](ctx, s, tableBackups, nil,
		`SELECT id, data FROM backups WHERE status = 'completed' AND EXISTS (
			SELECT 1 FROM json_each(backups.data, '$.copies') c
			WHERE json_extract(c.value, '$.status') IN ('pending', 'failed'))
		ORDER BY started_at, id`)
}

// CopyKeys returns the storage keys of the copies recorded on storage target
// targetID that are not purged: a storage scan of the target never reports them as
// orphans.
func (s *SQLiteStore) CopyKeys(ctx context.Context, targetID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT json_extract(c.value, '$.storage_key') FROM backups, json_each(backups.data, '$.copies') c
		WHERE json_extract(c.value, '$.target_id') = ? AND json_extract(c.value, '$.status') != 'purged'
		AND coalesce(json_extract(c.value, '$.storage_key'), '') != ''`, targetID)
	if err != nil {
		return nil, fmt.Errorf("store: list copy keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("store: scan copy key: %w", err)
		}
		out[key] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list copy keys: %w", err)
	}
	return out, nil
}

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/audit"
)

// Compile-time check that SQLiteStore serves the audit port.
var _ audit.Repository = (*SQLiteStore)(nil)

// AppendAudit stores e, assigns its ID and deletes all but the newest keep entries,
// in one transaction.
func (s *SQLiteStore) AppendAudit(ctx context.Context, e *audit.Entry, keep int) error {
	if e == nil {
		return fmt.Errorf("%w: audit entry is nil", ErrInvalidRecord)
	}
	args := string(e.Arguments)
	if args == "" || !json.Valid(e.Arguments) {
		args = "{}"
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO audit_log (at, api_key_id, api_key_name, transport, tool, arguments, result, error, duration_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			timeKey(e.Time), e.APIKeyID, e.APIKeyName, e.Transport, e.Tool, args, e.Result, e.Error, e.DurationMS)
		if err != nil {
			return fmt.Errorf("store: append audit entry: %w", err)
		}
		if e.ID, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("store: append audit entry: %w", err)
		}
		if keep > 0 {
			if _, err := tx.ExecContext(ctx, "DELETE FROM audit_log WHERE id <= ?", e.ID-int64(keep)); err != nil {
				return fmt.Errorf("store: prune audit log: %w", err)
			}
		}
		return nil
	})
}

// ListAudit returns up to limit audit entries, newest first.
func (s *SQLiteStore) ListAudit(ctx context.Context, limit int) ([]*audit.Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, api_key_id, api_key_name, transport, tool, arguments, result, error, duration_ms
		FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit log: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := make([]*audit.Entry, 0)
	for rows.Next() {
		var e audit.Entry
		var at int64
		var args string
		if err := rows.Scan(&e.ID, &at, &e.APIKeyID, &e.APIKeyName, &e.Transport, &e.Tool, &args, &e.Result, &e.Error, &e.DurationMS); err != nil {
			return nil, fmt.Errorf("store: scan audit entry: %w", err)
		}
		e.Time, e.Arguments = fromKey(at), json.RawMessage(args)
		list = append(list, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit log: %w", err)
	}
	return list, nil
}

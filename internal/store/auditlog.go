package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
)

// Compile-time check that SQLiteStore serves the audit log port.
var _ auditlog.Repository = (*SQLiteStore)(nil)

// auditEventColumns are the columns of audit_events in scan order.
const auditEventColumns = `id, at, actor_kind, actor_user_id, actor_name, actor_key_id, actor_key_name,
	action, targets, status, outcome, client_ip, user_agent, count, hash`

// AppendAuditEvent assigns e the ID after the newest entry (or the anchor), chains
// it to that entry's hash and stores it, in one write transaction, so concurrent
// appends cannot fork the chain.
func (s *SQLiteStore) AppendAuditEvent(ctx context.Context, e *auditlog.Event) error {
	if e == nil {
		return fmt.Errorf("%w: audit log entry is nil", ErrInvalidRecord)
	}
	targets := e.Targets
	if targets == nil {
		targets = map[string]string{}
	}
	rawTargets, err := json.Marshal(targets)
	if err != nil {
		return fmt.Errorf("store: encode audit log targets: %w", err)
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var prevID int64
		var prevHash string
		err := tx.QueryRowContext(ctx, "SELECT id, hash FROM audit_events ORDER BY id DESC LIMIT 1").Scan(&prevID, &prevHash)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, "SELECT last_id, last_hash FROM audit_chain_anchor WHERE id = 1").Scan(&prevID, &prevHash)
		}
		if err != nil {
			return fmt.Errorf("store: read audit chain head: %w", err)
		}
		next := *e
		next.ID, next.Targets = prevID+1, targets
		if next.Hash, err = auditlog.ChainHash(prevHash, &next); err != nil {
			return fmt.Errorf("store: hash audit log entry: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events (`+auditEventColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			next.ID, timeKey(next.Time), next.ActorKind, next.ActorUserID, next.ActorName, next.ActorKeyID, next.ActorKeyName,
			next.Action, string(rawTargets), next.Status, next.Outcome, next.ClientIP, next.UserAgent, next.Count, next.Hash); err != nil {
			return fmt.Errorf("store: append audit log entry: %w", err)
		}
		*e = next
		return nil
	})
}

// ListAuditEvents returns the entries matching f, newest first unless f.Ascending.
func (s *SQLiteStore) ListAuditEvents(ctx context.Context, f auditlog.Filter) ([]*auditlog.Event, error) {
	var where []string
	var args []any
	add := func(cond string, vals ...any) {
		where = append(where, cond)
		args = append(args, vals...)
	}
	if f.Actor != "" {
		add("(actor_user_id = ? OR actor_name = ? OR actor_key_id = ? OR actor_key_name = ?)", f.Actor, f.Actor, f.Actor, f.Actor)
	}
	if f.ActorKind != "" {
		add("actor_kind = ?", f.ActorKind)
	}
	if f.Action != "" {
		add(`action LIKE ? ESCAPE '\'`, "%"+escapeLike(f.Action)+"%")
	}
	if f.Outcome != "" {
		add("outcome = ?", f.Outcome)
	}
	if !f.Since.IsZero() {
		add("at >= ?", timeKey(f.Since))
	}
	if !f.Until.IsZero() {
		add("at < ?", timeKey(f.Until))
	}
	if f.BeforeID > 0 {
		add("id < ?", f.BeforeID)
	}
	if f.AfterID > 0 {
		add("id > ?", f.AfterID)
	}
	query := "SELECT " + auditEventColumns + " FROM audit_events"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // G202: only constant clauses are joined; values are ? arguments.
	}
	if f.Ascending {
		query += " ORDER BY id ASC"
	} else {
		query += " ORDER BY id DESC"
	}
	limit := f.Limit
	if limit <= 0 {
		limit = auditlog.DefaultListLimit
	}
	query += " LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list audit log: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := make([]*auditlog.Event, 0)
	for rows.Next() {
		var e auditlog.Event
		var at int64
		var targets string
		if err := rows.Scan(&e.ID, &at, &e.ActorKind, &e.ActorUserID, &e.ActorName, &e.ActorKeyID, &e.ActorKeyName,
			&e.Action, &targets, &e.Status, &e.Outcome, &e.ClientIP, &e.UserAgent, &e.Count, &e.Hash); err != nil {
			return nil, fmt.Errorf("store: scan audit log entry: %w", err)
		}
		e.Time = fromKey(at)
		if err := json.Unmarshal([]byte(targets), &e.Targets); err != nil {
			// Stored targets are always an object of strings; anything else is
			// tampering, which verification reports through the hash.
			e.Targets = map[string]string{"_invalid": targets}
		}
		if e.Targets == nil {
			e.Targets = map[string]string{}
		}
		list = append(list, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit log: %w", err)
	}
	return list, nil
}

// escapeLike escapes the LIKE wildcards of s for ESCAPE '\'.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// AuditChainAnchor returns the chain anchor.
func (s *SQLiteStore) AuditChainAnchor(ctx context.Context) (auditlog.Anchor, error) {
	var a auditlog.Anchor
	var lastAt, pruned int64
	err := s.db.QueryRowContext(ctx, "SELECT last_id, last_hash, last_at, pruned_at FROM audit_chain_anchor WHERE id = 1").Scan(&a.LastID, &a.LastHash, &lastAt, &pruned)
	if err != nil {
		return auditlog.Anchor{}, fmt.Errorf("store: read audit chain anchor: %w", err)
	}
	if a.LastID != 0 {
		a.LastTime = fromKey(lastAt)
	}
	if pruned != 0 {
		a.PrunedAt = fromKey(pruned)
	}
	return a, nil
}

// PruneAuditEvents moves the anchor to the newest entry recorded before cutoff and
// deletes it and every older entry, in one transaction.
func (s *SQLiteStore) PruneAuditEvents(ctx context.Context, cutoff time.Time) (int64, error) {
	var removed int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var lastID, lastAt int64
		var lastHash string
		err := tx.QueryRowContext(ctx, `SELECT id, hash, at FROM audit_events
			WHERE id = (SELECT MAX(id) FROM audit_events WHERE at < ?)`, timeKey(cutoff)).Scan(&lastID, &lastHash, &lastAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: find expired audit log entries: %w", err)
		}
		if _, err = tx.ExecContext(ctx, "UPDATE audit_chain_anchor SET last_id = ?, last_hash = ?, last_at = ?, pruned_at = ? WHERE id = 1",
			lastID, lastHash, lastAt, timeKey(time.Now())); err != nil {
			return fmt.Errorf("store: move audit chain anchor: %w", err)
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM audit_events WHERE id <= ?", lastID)
		if err != nil {
			return fmt.Errorf("store: prune audit log: %w", err)
		}
		removed, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: prune audit log: %w", err)
		}
		return nil
	})
	return removed, err
}

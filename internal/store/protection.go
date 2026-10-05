package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Tables of the delete protection (see migration 0022).
const (
	tablePendingChanges = "pending_changes"
	tableApprovals      = "approvals"
)

// MaxApprovalList caps ListApprovals.
const MaxApprovalList = 500

// ReplacePendingChange stores c, replacing in the same transaction any pending change
// of the same kind and subject (models.PendingChange.Subject), and returns the
// change it replaced, if any.
func (s *SQLiteStore) ReplacePendingChange(ctx context.Context, c *models.PendingChange) (*models.PendingChange, error) {
	if c == nil || c.ID == "" {
		return nil, fmt.Errorf("%w: pending change with ID is required", ErrInvalidRecord)
	}
	data, err := encode(c)
	if err != nil {
		return nil, err
	}
	var replaced *models.PendingChange
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		prev, getErr := getRecord[models.PendingChange](ctx, tx, ErrNotFound,
			"SELECT data FROM pending_changes WHERE kind = ? AND subject = ?", string(c.Kind), c.Subject())
		switch {
		case getErr == nil:
			replaced = prev
		case !errors.Is(getErr, ErrNotFound):
			return getErr
		}
		if _, execErr := tx.ExecContext(ctx, "DELETE FROM pending_changes WHERE kind = ? AND subject = ?", string(c.Kind), c.Subject()); execErr != nil {
			return fmt.Errorf("store: replace pending change: %w", execErr)
		}
		if _, execErr := tx.ExecContext(ctx, "INSERT INTO pending_changes (id, kind, subject, effective_at, data) VALUES (?, ?, ?, ?, ?)",
			c.ID, string(c.Kind), c.Subject(), timeKey(c.EffectiveAt), data); execErr != nil {
			return fmt.Errorf("store: save pending change %s: %w", c.ID, execErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return replaced, nil
}

// ListPendingChanges returns every pending change, the earliest effective first.
func (s *SQLiteStore) ListPendingChanges(ctx context.Context) ([]*models.PendingChange, error) {
	return listRecords[models.PendingChange](ctx, s, tablePendingChanges, nil,
		"SELECT id, data FROM pending_changes ORDER BY effective_at, id")
}

// GetPendingChange returns pending change id or ErrNotFound.
func (s *SQLiteStore) GetPendingChange(ctx context.Context, id string) (*models.PendingChange, error) {
	return getRecord[models.PendingChange](ctx, s.db, ErrNotFound, "SELECT data FROM pending_changes WHERE id = ?", id)
}

// DeletePendingChange removes pending change id; ErrNotFound when it is gone (applied
// or replaced meanwhile), so two callers never both act on one change.
func (s *SQLiteStore) DeletePendingChange(ctx context.Context, id string) error {
	return execOne(ctx, s.db, ErrNotFound, "DELETE FROM pending_changes WHERE id = ?", id)
}

// DeletePendingChangesOf removes the pending change of kind and subject, if any, and
// reports whether there was one.
func (s *SQLiteStore) DeletePendingChangesOf(ctx context.Context, kind models.PendingChangeKind, subject string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM pending_changes WHERE kind = ? AND subject = ?", string(kind), subject)
	if err != nil {
		return false, fmt.Errorf("store: delete pending changes: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: rows affected: %w", err)
	}
	return n > 0, nil
}

// CreateApproval inserts a new approval request; ErrAlreadyExists for a taken ID.
func (s *SQLiteStore) CreateApproval(ctx context.Context, a *models.Approval) error {
	if a == nil || a.ID == "" {
		return fmt.Errorf("%w: approval with ID is required", ErrInvalidRecord)
	}
	data, err := encode(a)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM approvals WHERE id = ?", a.ID).Scan(&n); err != nil {
			return fmt.Errorf("store: check approval: %w", err)
		}
		if n > 0 {
			return ErrAlreadyExists
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO approvals (id, status, created_at, expires_at, data, secret) VALUES (?, ?, ?, ?, ?, ?)",
			a.ID, string(a.Status), timeKey(a.CreatedAt), timeKey(a.ExpiresAt), data, a.Secret); err != nil {
			return fmt.Errorf("store: create approval %s: %w", a.ID, err)
		}
		return nil
	})
}

// GetApproval returns approval id or ErrNotFound.
func (s *SQLiteStore) GetApproval(ctx context.Context, id string) (*models.Approval, error) {
	return getRecord[models.Approval](ctx, s.db, ErrNotFound, "SELECT data FROM approvals WHERE id = ?", id)
}

// ListApprovals returns up to limit (at most MaxApprovalList) approvals in status
// ("" for any), newest first.
func (s *SQLiteStore) ListApprovals(ctx context.Context, status models.ApprovalStatus, limit int) ([]*models.Approval, error) {
	if limit <= 0 || limit > MaxApprovalList {
		limit = MaxApprovalList
	}
	if status == "" {
		return listRecords[models.Approval](ctx, s, tableApprovals, nil,
			"SELECT id, data FROM approvals ORDER BY created_at DESC, id DESC LIMIT ?", limit)
	}
	return listRecords[models.Approval](ctx, s, tableApprovals, nil,
		"SELECT id, data FROM approvals WHERE status = ? ORDER BY created_at DESC, id DESC LIMIT ?", string(status), limit)
}

// ApprovalSecret returns the secret kept apart from pending approval id (empty once
// it was decided), or ErrNotFound.
func (s *SQLiteStore) ApprovalSecret(ctx context.Context, id string) (string, error) {
	var secret string
	err := s.db.QueryRowContext(ctx, "SELECT secret FROM approvals WHERE id = ?", id).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: read approval secret: %w", err)
	}
	return secret, nil
}

// UpdateApproval applies fn to approval id and saves the result in one transaction,
// so two administrators deciding at once never both succeed. When fn returns an
// error nothing is written and the error is returned unchanged. The secret of a
// request is cleared once it is no longer pending.
func (s *SQLiteStore) UpdateApproval(ctx context.Context, id string, fn func(*models.Approval) error) (*models.Approval, error) {
	var out *models.Approval
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		a, err := getRecord[models.Approval](ctx, tx, ErrNotFound, "SELECT data FROM approvals WHERE id = ?", id)
		if err != nil {
			return err
		}
		if fnErr := fn(a); fnErr != nil {
			return fnErr
		}
		if a.ID != id {
			return fmt.Errorf("%w: approval id must not change", ErrInvalidRecord)
		}
		data, err := encode(a)
		if err != nil {
			return err
		}
		out = a
		return execOne(ctx, tx, ErrNotFound, `UPDATE approvals SET status = ?, expires_at = ?, data = ?,
			secret = CASE WHEN ? = 'pending' THEN secret ELSE '' END WHERE id = ?`,
			string(a.Status), timeKey(a.ExpiresAt), data, string(a.Status), id)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

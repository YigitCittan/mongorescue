package operations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// Delete protection (see docs/security.md): deletions are soft and wait for the
// delete grace period, lowered protections wait for it too, and with the two-person
// rule destructive actions wait for a second administrator.

// Errors of the delete protection.
var (
	// ErrApprovalRequired is matched by *ApprovalPendingError: the two-person rule is
	// on, so the action waits for a second administrator instead of running.
	ErrApprovalRequired = errors.New("operations: a second administrator must approve this action")
	// ErrAlreadyDeleted is returned when deleting a backup that is already deleted
	// (or purged).
	ErrAlreadyDeleted = errors.New("operations: the backup is already deleted")
	// ErrNotDeleted is returned when undeleting a backup that is not deleted (a purged
	// one cannot be undeleted: its archive is gone).
	ErrNotDeleted = errors.New("operations: the backup is not deleted")
	// ErrBackupDeleted is returned when restoring, pinning or reading a deleted
	// backup: undelete it first.
	ErrBackupDeleted = errors.New("operations: the backup is deleted; undelete it first")
	// ErrBackupRunning is returned when deleting a backup that is still running.
	ErrBackupRunning = errors.New("operations: the backup is still running; cancel it first")
	// ErrApprovalClosed is returned when deciding an approval request that was
	// already decided or has expired.
	ErrApprovalClosed = errors.New("operations: the approval request is no longer open")
	// ErrApprovalNeedsSession aliases auth.ErrApprovalNeedsSession.
	ErrApprovalNeedsSession = auth.ErrApprovalNeedsSession
	// ErrSelfApproval aliases auth.ErrSelfApproval.
	ErrSelfApproval = auth.ErrSelfApproval
	// ErrTooFewAdmins aliases auth.ErrTooFewAdmins.
	ErrTooFewAdmins = auth.ErrTooFewAdmins
)

// MaxDeleteReasonLength bounds the reason of a deletion, in characters.
const MaxDeleteReasonLength = 500

// ApprovalPendingError is returned instead of running a destructive action while the
// two-person rule (security.require_second_approver) is on: Approval is the request
// that now waits for another administrator. It matches ErrApprovalRequired.
type ApprovalPendingError struct {
	// Approval is the stored request.
	Approval *models.Approval
}

// Error implements error.
func (e *ApprovalPendingError) Error() string {
	return fmt.Sprintf("a second administrator must approve this action: request %s (%s) waits for approval until %s",
		e.Approval.ID, e.Approval.Summary, e.Approval.ExpiresAt.UTC().Format(time.RFC3339))
}

// Unwrap returns ErrApprovalRequired.
func (e *ApprovalPendingError) Unwrap() error { return ErrApprovalRequired }

// SettingsUpdater applies settings changes (implemented by *settings.Service).
type SettingsUpdater interface {
	// UpdateChanged validates and applies p and returns the masked settings and the
	// changed keys.
	UpdateChanged(ctx context.Context, p settings.Patch) (settings.Settings, []string, error)
}

// approvalStore persists approval requests (implemented by *store.SQLiteStore).
type approvalStore interface {
	CreateApproval(ctx context.Context, a *models.Approval) error
	GetApproval(ctx context.Context, id string) (*models.Approval, error)
	ListApprovals(ctx context.Context, status models.ApprovalStatus, limit int) ([]*models.Approval, error)
	UpdateApproval(ctx context.Context, id string, fn func(*models.Approval) error) (*models.Approval, error)
}

// pendingStore persists pending changes (implemented by *store.SQLiteStore).
type pendingStore interface {
	ReplacePendingChange(ctx context.Context, c *models.PendingChange) (*models.PendingChange, error)
	ListPendingChanges(ctx context.Context) ([]*models.PendingChange, error)
	GetPendingChange(ctx context.Context, id string) (*models.PendingChange, error)
	DeletePendingChange(ctx context.Context, id string) error
	DeletePendingChangesOf(ctx context.Context, kind models.PendingChangeKind, subject string) (bool, error)
}

// targetDeleter deletes storage targets (implemented by *targets.Service).
type targetDeleter interface {
	Delete(ctx context.Context, id string) error
}

// approvedKey carries the approval an action runs for (see withApproval).
type approvedKey struct{}

// approved is an approval being executed and the administrator who approved it.
type approved struct {
	approval *models.Approval
	approver string
}

// withApproval marks ctx as running approved action a, approved by approver: the
// two-person rule does not ask again.
func withApproval(ctx context.Context, a *models.Approval, approver string) context.Context {
	return context.WithValue(ctx, approvedKey{}, &approved{approval: a, approver: approver})
}

// approvalOf returns the approval ctx runs for, or nil.
func approvalOf(ctx context.Context) *approved {
	a, _ := ctx.Value(approvedKey{}).(*approved)
	return a
}

// deleteGrace returns the delete grace period in force.
func (s *Service) deleteGrace() time.Duration {
	return s.settings().Security.DeleteGrace()
}

// needsApproval reports whether a destructive action in ctx must wait for a second
// administrator: the two-person rule is on and ctx is not an approved action.
func (s *Service) needsApproval(ctx context.Context) bool {
	return s.settings().Security.RequireSecondApprover && approvalOf(ctx) == nil
}

// actorNames names who acts in ctx: the requester of an approved action and its
// approver, or the caller (see principalName) and "".
func actorNames(ctx context.Context) (by, approvedBy string) {
	if a := approvalOf(ctx); a != nil {
		return a.approval.RequestedBy, a.approver
	}
	return principalName(ctx), ""
}

// newProtectionID returns prefix and 16 random hexadecimal characters.
func newProtectionID(prefix string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

// checkReason validates the optional reason of a deletion or an approval request.
func checkReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > MaxDeleteReasonLength {
		return "", public(fmt.Sprintf("reason must be at most %d characters", MaxDeleteReasonLength), ErrInvalid)
	}
	if strings.ContainsFunc(reason, func(r rune) bool { return r < 0x20 && r != '\n' || r == 0x7f }) {
		return "", public("reason must not contain control characters", ErrInvalid)
	}
	return reason, nil
}

// destructive publishes a security.destructive_action event for action, described by
// detail, with fill (optional) adding the event's subject.
func (s *Service) destructive(ctx context.Context, action, detail string, fill func(*events.Event)) {
	by, approvedBy := actorNames(ctx)
	if approvedBy != "" {
		by += " (approved by " + approvedBy + ")"
	}
	e := events.SecurityEvent(events.SecurityDestructiveAction, s.now(), action, by, "", detail)
	if a := approvalOf(ctx); a != nil {
		e.ApprovalID = a.approval.ID
	}
	if fill != nil {
		fill(&e)
	}
	s.publish(context.WithoutCancel(ctx), e)
	s.logger.With(actorAttrs(ctx)...).Info("destructive action", slog.String("action", action), logsafe.Attr("detail", detail))
}

// requestApproval stores a as a new approval request of the caller in ctx (see
// storeApproval) and returns the *ApprovalPendingError that callers return instead
// of acting, or why the request could not be stored.
func (s *Service) requestApproval(ctx context.Context, a *models.Approval) error {
	stored, err := s.storeApproval(ctx, a)
	if err != nil {
		return err
	}
	return &ApprovalPendingError{Approval: stored}
}

// storeApproval stores a as a new approval request of the caller in ctx, announces
// it (security.approval_requested) and returns it.
func (s *Service) storeApproval(ctx context.Context, a *models.Approval) (*models.Approval, error) {
	st, ok := s.cfg.Store.(approvalStore)
	if !ok {
		return nil, public("the two-person rule is on but approval requests cannot be stored", ErrUnavailable)
	}
	id, err := newProtectionID("apr_")
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	p := auth.PrincipalFrom(ctx)
	a.ID, a.Status, a.CreatedAt, a.ExpiresAt = id, models.ApprovalPending, now, now.Add(models.ApprovalTTL)
	a.RequestedBy, a.RequestedByUserID, a.RequestedVia = principalName(ctx), p.UserID(), string(auth.MethodSystem)
	if p != nil && p.Method != "" {
		a.RequestedVia = string(p.Method)
	}
	if err := st.CreateApproval(ctx, a); err != nil {
		return nil, fmt.Errorf("store approval request: %w", err)
	}
	auditlog.Annotate(ctx, "approval_id", a.ID)
	auditlog.Annotate(ctx, "protection", "approval_requested")
	e := events.SecurityEvent(events.SecurityApprovalRequested, now, string(a.Action), a.RequestedBy, a.ID, a.Summary)
	s.publish(context.WithoutCancel(ctx), e)
	s.logger.With(actorAttrs(ctx)...).Info("destructive action waits for a second administrator",
		logsafe.Attr("approval_id", a.ID), slog.String("action", string(a.Action)), logsafe.Attr("summary", a.Summary))
	return a, nil
}

// approvals returns the approval store, or ErrUnavailable.
func (s *Service) approvals() (approvalStore, error) {
	st, ok := s.cfg.Store.(approvalStore)
	if !ok {
		return nil, public("approval requests are not available", ErrUnavailable)
	}
	return st, nil
}

// ListApprovals returns up to store.MaxApprovalList approval requests in status (""
// for all), newest first. Requests past their expiry are reported expired. It needs
// the admin scope.
func (s *Service) ListApprovals(ctx context.Context, status models.ApprovalStatus) ([]*models.Approval, error) {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return nil, err
	}
	switch status {
	case "", models.ApprovalPending, models.ApprovalApproved, models.ApprovalFailed, models.ApprovalRejected, models.ApprovalExpired:
	default:
		return nil, public("status must be pending, approved, failed, rejected or expired", ErrInvalid)
	}
	st, err := s.approvals()
	if err != nil {
		return nil, err
	}
	list, err := st.ListApprovals(ctx, "", store.MaxApprovalList)
	if err != nil {
		return nil, fmt.Errorf("list approvals: %w", err)
	}
	now := s.now()
	out := make([]*models.Approval, 0, len(list))
	for _, a := range list {
		if a.Status == models.ApprovalPending && !a.Open(now) {
			a.Status = models.ApprovalExpired
		}
		if status == "" || a.Status == status {
			out = append(out, a)
		}
	}
	return out, nil
}

// GetApproval returns approval request id (admin scope). Expected failures:
// ErrNotFound.
func (s *Service) GetApproval(ctx context.Context, id string) (*models.Approval, error) {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return nil, err
	}
	st, err := s.approvals()
	if err != nil {
		return nil, err
	}
	a, err := st.GetApproval(ctx, id)
	if err != nil {
		return nil, notFound(err, "approval request not found")
	}
	if a.Status == models.ApprovalPending && !a.Open(s.now()) {
		a.Status = models.ApprovalExpired
	}
	return a, nil
}

// decide moves open approval id to status (approved or rejected) by the caller in
// ctx. An expired request is stored as expired and refused (ErrApprovalClosed).
func (s *Service) decide(ctx context.Context, id string, status models.ApprovalStatus, check func(*models.Approval) error, reason string) (*models.Approval, error) {
	st, err := s.approvals()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	p := auth.PrincipalFrom(ctx)
	expired := false
	a, err := st.UpdateApproval(ctx, id, func(a *models.Approval) error {
		if a.Status != models.ApprovalPending {
			return public(fmt.Sprintf("approval request %s is %s", a.ID, a.Status), ErrApprovalClosed)
		}
		if !a.Open(now) {
			a.Status, expired = models.ApprovalExpired, true
			return nil
		}
		if checkErr := check(a); checkErr != nil {
			return checkErr
		}
		a.Status, a.DecidedBy, a.DecidedByUserID, a.DecidedAt = status, principalName(ctx), p.UserID(), &now
		if reason != "" {
			a.Result = "rejected: " + reason
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, public("approval request not found", ErrNotFound, err)
		}
		return nil, err
	}
	if expired {
		return a, public(fmt.Sprintf("approval request %s expired at %s", a.ID, a.ExpiresAt.Format(time.RFC3339)), ErrApprovalClosed)
	}
	return a, nil
}

// Approve approves request id and runs its action at once, with the approver's
// rights and every protection re-checked. Only a signed-in administrator other than
// the requester may approve (auth.CheckApprover): API keys never can. The returned
// request is approved, or failed with Error when the action failed. Expected
// failures: ErrNotFound, ErrApprovalClosed, ErrApprovalNeedsSession, ErrSelfApproval
// and auth.ErrForbidden.
func (s *Service) Approve(ctx context.Context, id string) (*models.Approval, error) {
	if err := auth.CheckApprover(auth.PrincipalFrom(ctx), ""); err != nil {
		return nil, err
	}
	a, err := s.decide(ctx, id, models.ApprovalApproved, func(a *models.Approval) error {
		return auth.CheckApprover(auth.PrincipalFrom(ctx), a.RequestedByUserID)
	}, "")
	if err != nil {
		return a, err
	}
	auditlog.Annotate(ctx, "protection", "approved")
	auditlog.Annotate(ctx, "approval_action", string(a.Action))
	result, execErr := s.executeApproval(withApproval(ctx, a, principalName(ctx)), a)
	st, _ := s.approvals()
	final, err := st.UpdateApproval(context.WithoutCancel(ctx), a.ID, func(r *models.Approval) error {
		if execErr != nil {
			r.Status, r.Error = models.ApprovalFailed, redact.Text(execErr.Error())
		} else {
			r.Result = result
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("record approval outcome: %w", err)
	}
	if execErr != nil {
		s.logger.Warn("approved action failed", logsafe.Attr("approval_id", a.ID), slog.String("action", string(a.Action)), logsafe.Error(execErr))
	}
	return final, nil
}

// Reject rejects request id (admin scope; any administrator, the requester included,
// and admin API keys may reject). Expected failures: ErrNotFound and
// ErrApprovalClosed.
func (s *Service) Reject(ctx context.Context, id, reason string) (*models.Approval, error) {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return nil, err
	}
	reason, err := checkReason(reason)
	if err != nil {
		return nil, err
	}
	a, err := s.decide(ctx, id, models.ApprovalRejected, func(*models.Approval) error { return nil }, reason)
	if err != nil {
		return a, err
	}
	auditlog.Annotate(ctx, "protection", "rejected")
	s.logger.With(actorAttrs(ctx)...).Info("approval request rejected", logsafe.Attr("approval_id", a.ID))
	return a, nil
}

// executeApproval runs the action of approved request a in ctx (withApproval) and
// describes what it did.
func (s *Service) executeApproval(ctx context.Context, a *models.Approval) (string, error) {
	switch a.Action {
	case models.ApprovalDeleteBackup:
		res, err := s.DeleteBackup(ctx, a.Subject, a.Reason)
		if err != nil {
			return "", err
		}
		return "backup " + res.DeletedID + " deleted; recoverable until " + res.PurgeAfter.Format(time.RFC3339), nil
	case models.ApprovalBulkDeleteBackups, models.ApprovalBulkUnpinBackups:
		action := BulkDelete
		if a.Action == models.ApprovalBulkUnpinBackups {
			action = BulkUnpin
		}
		res, err := s.Bulk(ctx, BulkBackups, BulkRequest{Action: action, IDs: a.IDs, Reason: a.Reason})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d succeeded, %d skipped, %d failed", res.Succeeded, len(res.Skipped), res.Failed), nil
	case models.ApprovalUnpinBackup:
		if _, err := s.UnpinBackup(ctx, a.Subject); err != nil {
			return "", err
		}
		return "backup " + a.Subject + " unpinned", nil
	case models.ApprovalDeleteTarget:
		if err := s.DeleteTarget(ctx, a.Subject); err != nil {
			return "", err
		}
		return "storage target " + a.Subject + " deleted", nil
	case models.ApprovalShortenRetention:
		c, err := s.scheduleRetention(ctx, a.Subject, a.RetentionDays, a.RetentionCount)
		if err != nil {
			return "", err
		}
		return "retention change scheduled for " + c.EffectiveAt.Format(time.RFC3339), nil
	case models.ApprovalLowerGrace:
		if a.DeleteGraceDays == nil {
			return "", public("the request names no grace period", ErrInvalid)
		}
		c, err := s.scheduleGrace(ctx, *a.DeleteGraceDays)
		if err != nil {
			return "", err
		}
		return "grace period change scheduled for " + c.EffectiveAt.Format(time.RFC3339), nil
	case models.ApprovalDisableSecondApprover:
		if err := s.disableSecondApprover(ctx); err != nil {
			return "", err
		}
		return "the two-person rule is off", nil
	default:
		return "", public("unknown approval action "+string(a.Action), ErrInvalid)
	}
}

// ExpireApprovals stores every pending request past its expiry as expired.
func (s *Service) ExpireApprovals(ctx context.Context) {
	st, ok := s.cfg.Store.(approvalStore)
	if !ok {
		return
	}
	list, err := st.ListApprovals(ctx, models.ApprovalPending, store.MaxApprovalList)
	if err != nil {
		s.logger.Warn("cannot list approval requests", logsafe.Error(err))
		return
	}
	now := s.now()
	for _, a := range list {
		if a.Open(now) {
			continue
		}
		if _, err := st.UpdateApproval(ctx, a.ID, func(r *models.Approval) error {
			if r.Status != models.ApprovalPending || r.Open(now) {
				return errNotDueYet
			}
			r.Status = models.ApprovalExpired
			return nil
		}); err != nil && !errors.Is(err, errNotDueYet) {
			s.logger.Warn("cannot expire an approval request", logsafe.Attr("approval_id", a.ID), logsafe.Error(err))
		}
	}
}

// errNotDueYet skips a pending change or approval that changed since it was listed.
var errNotDueYet = errors.New("operations: not due")

// pending returns the pending change store, or ErrUnavailable.
func (s *Service) pending() (pendingStore, error) {
	st, ok := s.cfg.Store.(pendingStore)
	if !ok {
		return nil, public("delayed protection changes are not available", ErrUnavailable)
	}
	return st, nil
}

// PendingChanges returns every lowered protection waiting to take effect, the
// earliest first.
func (s *Service) PendingChanges(ctx context.Context) ([]*models.PendingChange, error) {
	st, err := s.pending()
	if err != nil {
		return []*models.PendingChange{}, nil //nolint:nilerr // Without the store nothing is pending.
	}
	list, err := st.ListPendingChanges(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pending changes: %w", err)
	}
	return list, nil
}

// pendingRetention returns the pending retention change of job jobID, or nil.
func (s *Service) pendingRetention(ctx context.Context, jobID string) *models.PendingChange {
	list, err := s.PendingChanges(ctx)
	if err != nil {
		return nil
	}
	for _, c := range list {
		if c.Kind == models.PendingRetention && c.JobID == jobID {
			return c
		}
	}
	return nil
}

// CancelPendingChange drops pending change id: the protection it would have lowered
// stays as it is. Cancelling only keeps a protection, so it needs no approval (admin
// scope). Expected failures: ErrNotFound.
func (s *Service) CancelPendingChange(ctx context.Context, id string) error {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return err
	}
	st, err := s.pending()
	if err != nil {
		return err
	}
	if err := st.DeletePendingChange(ctx, id); err != nil {
		return notFound(err, "pending change not found")
	}
	auditlog.Annotate(ctx, "protection", "pending_change_cancelled")
	s.logger.With(actorAttrs(ctx)...).Info("pending protection change cancelled", logsafe.Attr("change_id", id))
	return nil
}

// schedulePending stores c to take effect after the grace period in force, replacing
// an earlier change of the same kind and subject.
func (s *Service) schedulePending(ctx context.Context, c *models.PendingChange) (*models.PendingChange, error) {
	st, err := s.pending()
	if err != nil {
		return nil, err
	}
	if c.ID, err = newProtectionID("chg_"); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	c.CreatedAt, c.EffectiveAt = now, now.Add(s.deleteGrace())
	c.RequestedBy, c.ApprovedBy = actorNames(ctx)
	if _, err := st.ReplacePendingChange(ctx, c); err != nil {
		return nil, fmt.Errorf("store pending change: %w", err)
	}
	auditlog.Annotate(ctx, "pending_change_id", c.ID)
	auditlog.Annotate(ctx, "effective_at", c.EffectiveAt.Format(time.RFC3339))
	return c, nil
}

// scheduleRetention schedules the shortening of job jobID's retention to days and
// count (nil keeps a value) after the grace period.
func (s *Service) scheduleRetention(ctx context.Context, jobID string, days, count *int) (*models.PendingChange, error) {
	if _, err := s.cfg.Store.GetJob(ctx, jobID); err != nil {
		return nil, notFound(err, "job not found")
	}
	c, err := s.schedulePending(ctx, &models.PendingChange{Kind: models.PendingRetention, JobID: jobID, RetentionDays: days, RetentionCount: count})
	if err != nil {
		return nil, err
	}
	s.destructive(ctx, "shorten_retention", fmt.Sprintf("retention of job %s shortened to %s, effective %s", jobID, retentionText(days, count), c.EffectiveAt.Format(time.RFC3339)),
		func(e *events.Event) { e.JobID = jobID })
	return c, nil
}

// scheduleGrace schedules lowering security.delete_grace_days to days after the
// grace period in force.
func (s *Service) scheduleGrace(ctx context.Context, days int) (*models.PendingChange, error) {
	c, err := s.schedulePending(ctx, &models.PendingChange{Kind: models.PendingDeleteGrace, DeleteGraceDays: &days})
	if err != nil {
		return nil, err
	}
	s.destructive(ctx, "lower_delete_grace", fmt.Sprintf("delete grace period lowered to %d days, effective %s", days, c.EffectiveAt.Format(time.RFC3339)), nil)
	return c, nil
}

// retentionText describes a retention change.
func retentionText(days, count *int) string {
	var parts []string
	if days != nil {
		parts = append(parts, fmt.Sprintf("%d days", *days))
	}
	if count != nil {
		parts = append(parts, fmt.Sprintf("%d backups", *count))
	}
	return strings.Join(parts, " and ")
}

// ApplyDueChanges applies every pending change whose time has come and expires
// approval requests past their expiry. The scheduler calls it every ten minutes
// (scheduler.WithMaintenance).
func (s *Service) ApplyDueChanges(ctx context.Context) {
	s.ExpireApprovals(ctx)
	st, ok := s.cfg.Store.(pendingStore)
	if !ok {
		return
	}
	list, err := st.ListPendingChanges(ctx)
	if err != nil {
		s.logger.Warn("cannot list pending protection changes", logsafe.Error(err))
		return
	}
	now := s.now()
	for _, c := range list {
		if now.Before(c.EffectiveAt) {
			continue
		}
		// Taking the row first means two callers never both apply one change; a
		// failure afterwards keeps the stronger protection.
		if err := st.DeletePendingChange(ctx, c.ID); err != nil {
			continue
		}
		var applyErr error
		switch c.Kind {
		case models.PendingRetention:
			applyErr = s.applyRetentionChange(ctx, c)
		case models.PendingDeleteGrace:
			applyErr = s.applyGraceChange(ctx, c)
		}
		if applyErr != nil {
			s.logger.Error("could not apply a pending protection change; the current protection stays",
				logsafe.Attr("change_id", c.ID), slog.String("kind", string(c.Kind)), logsafe.Error(applyErr))
			continue
		}
		s.logger.Info("pending protection change applied", logsafe.Attr("change_id", c.ID), slog.String("kind", string(c.Kind)))
		if s.cfg.Audit != nil {
			s.cfg.Audit.Record(ctx, systemAudit(now, "protection.apply_pending", map[string]any{
				"change_id": c.ID, "kind": c.Kind, "job_id": c.JobID, "requested_by": c.RequestedBy,
			}))
		}
	}
}

// applyRetentionChange sets the retention of the job of c to its values.
func (s *Service) applyRetentionChange(ctx context.Context, c *models.PendingChange) error {
	existing, err := s.cfg.Store.GetJob(ctx, c.JobID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	job := existing.Clone()
	persist := func() error {
		current, getErr := s.cfg.Store.GetJob(ctx, c.JobID)
		if getErr != nil {
			return getErr
		}
		*job = *current.Clone()
		if c.RetentionDays != nil {
			job.RetentionDays = *c.RetentionDays
		}
		if c.RetentionCount != nil {
			job.RetentionCount = *c.RetentionCount
		}
		return s.cfg.Store.UpdateJob(ctx, job)
	}
	if s.cfg.Scheduler != nil {
		err = s.cfg.Scheduler.ApplyJobUpdate(job, persist)
	} else {
		err = persist()
	}
	if err != nil {
		return err
	}
	ctx = withApproval(ctx, &models.Approval{RequestedBy: c.RequestedBy}, c.ApprovedBy)
	s.destructive(ctx, "apply_retention", fmt.Sprintf("retention of job %s is now %d days and %d backups", job.ID, job.RetentionDays, job.RetentionCount),
		func(e *events.Event) { e.JobID = job.ID })
	return nil
}

// applyGraceChange lowers the grace period to the value of c, unless it is already
// at most that.
func (s *Service) applyGraceChange(ctx context.Context, c *models.PendingChange) error {
	if c.DeleteGraceDays == nil || s.cfg.SettingsUpdater == nil {
		return nil
	}
	days := *c.DeleteGraceDays
	if days >= s.settings().Security.DeleteGraceDays {
		return nil
	}
	if _, _, err := s.cfg.SettingsUpdater.UpdateChanged(settings.WithLoweredProtection(ctx), settings.Patch{Security: &settings.SecurityPatch{DeleteGraceDays: &days}}); err != nil {
		return err
	}
	ctx = withApproval(ctx, &models.Approval{RequestedBy: c.RequestedBy}, c.ApprovedBy)
	s.destructive(ctx, "apply_delete_grace", fmt.Sprintf("delete grace period is now %d days", days), nil)
	return nil
}

// disableSecondApprover turns the two-person rule off (an approved action).
func (s *Service) disableSecondApprover(ctx context.Context) error {
	if s.cfg.SettingsUpdater == nil {
		return public("settings are not available", ErrUnavailable)
	}
	off := false
	if _, _, err := s.cfg.SettingsUpdater.UpdateChanged(settings.WithLoweredProtection(ctx), settings.Patch{Security: &settings.SecurityPatch{RequireSecondApprover: &off}}); err != nil {
		return err
	}
	s.destructive(ctx, "disable_second_approver", "the two-person rule was turned off", nil)
	return nil
}

// ManagesSettings reports whether UpdateSettings is available (Config.SettingsUpdater
// is set).
func (s *Service) ManagesSettings() bool { return s.cfg.SettingsUpdater != nil }

// SettingsUpdate is the outcome of UpdateSettings.
type SettingsUpdate struct {
	// Settings are the settings now in force (masked).
	Settings settings.Settings
	// Changed lists the keys that changed now.
	Changed []string
	// Pending lists the lowered protections that take effect later.
	Pending []*models.PendingChange
	// Approvals lists the requests that wait for a second administrator.
	Approvals []*models.Approval
}

// UpdateSettings applies settings patch p like settings.Service.UpdateChanged, except
// for the delete protections: a lower security.delete_grace_days is scheduled to
// take effect after the current grace period (or, with the two-person rule, waits for
// a second administrator first), turning security.require_second_approver off
// always waits for a second administrator, and turning it on needs at least two
// administrators (ErrTooFewAdmins). A higher grace period applies at once and
// cancels a pending lowering. Expected failures: ErrUnavailable, ErrTooFewAdmins and
// the settings errors.
func (s *Service) UpdateSettings(ctx context.Context, p settings.Patch) (*SettingsUpdate, error) {
	if s.cfg.SettingsUpdater == nil {
		return nil, public("settings are not available", ErrUnavailable)
	}
	cur := s.settings().Security
	var lowerGrace *int
	disable := false
	if p.Security != nil {
		sec := *p.Security
		if v := sec.DeleteGraceDays; v != nil && *v < cur.DeleteGraceDays {
			if *v < models.MinDeleteGraceDays || *v > models.MaxDeleteGraceDays {
				return nil, fmt.Errorf("%w: security.delete_grace_days must be between %d and %d", settings.ErrInvalid, models.MinDeleteGraceDays, models.MaxDeleteGraceDays)
			}
			days := *v
			lowerGrace, sec.DeleteGraceDays = &days, nil
		}
		if v := sec.RequireSecondApprover; v != nil {
			switch {
			case *v && !cur.RequireSecondApprover:
				if s.cfg.SecondApproverCheck == nil {
					return nil, public("the two-person rule needs user accounts", ErrUnavailable)
				}
				if err := s.cfg.SecondApproverCheck(ctx); err != nil {
					return nil, err
				}
			case !*v && cur.RequireSecondApprover:
				disable, sec.RequireSecondApprover = true, nil
			}
		}
		p.Security = &sec
	}
	next, changed, err := s.cfg.SettingsUpdater.UpdateChanged(ctx, p)
	if err != nil {
		return nil, err
	}
	out := &SettingsUpdate{Settings: next, Changed: changed, Pending: []*models.PendingChange{}, Approvals: []*models.Approval{}}
	if next.Security.DeleteGraceDays > cur.DeleteGraceDays {
		if st, ok := s.cfg.Store.(pendingStore); ok {
			if _, err := st.DeletePendingChangesOf(ctx, models.PendingDeleteGrace, ""); err != nil {
				return nil, fmt.Errorf("cancel the pending grace period change: %w", err)
			}
		}
	}
	if lowerGrace != nil {
		if s.needsApproval(ctx) {
			a, err := s.storeApproval(ctx, &models.Approval{Action: models.ApprovalLowerGrace, DeleteGraceDays: lowerGrace,
				Summary: fmt.Sprintf("lower the delete grace period from %d to %d days", cur.DeleteGraceDays, *lowerGrace)})
			if err != nil {
				return nil, err
			}
			out.Approvals = append(out.Approvals, a)
		} else {
			c, err := s.scheduleGrace(ctx, *lowerGrace)
			if err != nil {
				return nil, err
			}
			out.Pending = append(out.Pending, c)
		}
	}
	if disable {
		a, err := s.storeApproval(ctx, &models.Approval{Action: models.ApprovalDisableSecondApprover,
			Summary: "turn the two-person rule (security.require_second_approver) off"})
		if err != nil {
			return nil, err
		}
		out.Approvals = append(out.Approvals, a)
	}
	return out, nil
}

// DeleteTarget deletes storage target id. A target still used by a job or by any
// backup record but purged ones is refused (targets.ErrInUse), so deleted backups
// keep their target until their grace period ends; an unused target goes at once.
// With the two-person rule the deletion waits for a second administrator
// (*ApprovalPendingError). It needs the admin scope. Expected failures: those of
// targets.Service.Delete and ErrApprovalRequired.
func (s *Service) DeleteTarget(ctx context.Context, id string) error {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return err
	}
	d, ok := s.cfg.Targets.(targetDeleter)
	if !ok {
		return public("storage targets are not configured", ErrUnavailable)
	}
	if s.needsApproval(ctx) {
		if id == "" {
			return targets.ErrNotFound
		}
		t, err := s.cfg.Targets.Resolve(ctx, id)
		if err != nil {
			return err
		}
		return s.requestApproval(ctx, &models.Approval{Action: models.ApprovalDeleteTarget, Subject: id,
			Summary: "delete storage target " + t.Name + " (" + id + ")"})
	}
	if err := d.Delete(ctx, id); err != nil {
		return err
	}
	s.destructive(ctx, "delete_storage_target", "storage target "+id+" deleted", func(e *events.Event) { e.TargetID = id })
	return nil
}

// RetentionHold is a shortening of a job's retention that a job edit held back: the
// job is stored with its current retention, and the shorter values take effect after
// the grace period (ApplyRetentionHold).
type RetentionHold struct {
	// JobID is the job.
	JobID string
	// Days and Count are the requested shorter values; nil when that value was not
	// shortened.
	Days, Count *int
	// From describes the retention in force.
	From string
}

// JobProtection reports what a job edit deferred.
type JobProtection struct {
	// PendingRetention is the scheduled retention shortening, if any.
	PendingRetention *models.PendingChange `json:"pending_retention,omitempty"`
	// Approval is the request for a second administrator, if the shortening waits
	// for one.
	Approval *models.Approval `json:"approval,omitempty"`
}

// shorter reports whether retention value v (0 keeps forever) keeps less than old.
func shorter(v, old int) bool {
	return v > 0 && (old == 0 || v < old)
}

// HoldRetention compares the retention of job, about to be stored, with that of
// existing (nil for a new job). A shortened value is reset to the current one in job
// and returned in the hold, to be applied with ApplyRetentionHold once job is stored.
// A new job whose ID already names backups counts as keeping them forever, so it
// cannot shorten their retention either. It returns nil when nothing was shortened.
func (s *Service) HoldRetention(ctx context.Context, existing, job *models.Job) (*RetentionHold, error) {
	baseline := existing
	if baseline == nil {
		page, err := s.cfg.Store.QueryBackupRecords(ctx, store.BackupFilter{JobID: job.ID, Limit: 1})
		if err != nil {
			return nil, fmt.Errorf("check the backups of job %s: %w", job.ID, err)
		}
		if page.Total == 0 {
			return nil, nil
		}
		baseline = &models.Job{}
	}
	h := &RetentionHold{JobID: job.ID, From: fmt.Sprintf("%d days and %d backups", baseline.RetentionDays, baseline.RetentionCount)}
	if shorter(job.RetentionDays, baseline.RetentionDays) {
		v := job.RetentionDays
		h.Days, job.RetentionDays = &v, baseline.RetentionDays
	}
	if shorter(job.RetentionCount, baseline.RetentionCount) {
		v := job.RetentionCount
		h.Count, job.RetentionCount = &v, baseline.RetentionCount
	}
	if h.Days == nil && h.Count == nil {
		return nil, nil
	}
	return h, nil
}

// ApplyRetentionHold schedules the shortening h after the grace period or, with the
// two-person rule, asks a second administrator first. retentionChanged reports that
// the stored job's retention changed otherwise (lengthened): that drops a pending
// shortening of the job when there is no new one.
func (s *Service) ApplyRetentionHold(ctx context.Context, jobID string, h *RetentionHold, retentionChanged bool) (*JobProtection, error) {
	out := &JobProtection{}
	if h == nil {
		if retentionChanged {
			if st, ok := s.cfg.Store.(pendingStore); ok {
				if _, err := st.DeletePendingChangesOf(ctx, models.PendingRetention, jobID); err != nil {
					return nil, fmt.Errorf("cancel the pending retention change: %w", err)
				}
			}
		}
		out.PendingRetention = s.pendingRetention(ctx, jobID)
		return out, nil
	}
	if s.needsApproval(ctx) {
		a, err := s.storeApproval(ctx, &models.Approval{Action: models.ApprovalShortenRetention, Subject: jobID,
			RetentionDays: h.Days, RetentionCount: h.Count,
			Summary: fmt.Sprintf("shorten the retention of job %s from %s to %s", jobID, h.From, retentionText(h.Days, h.Count))})
		if err != nil {
			return nil, err
		}
		out.Approval = a
		out.PendingRetention = s.pendingRetention(ctx, jobID)
		return out, nil
	}
	c, err := s.scheduleRetention(ctx, jobID, h.Days, h.Count)
	if err != nil {
		return nil, err
	}
	out.PendingRetention = c
	return out, nil
}

// systemAudit is the audit entry of a system action (mirrored into the audit log as
// "SYSTEM <tool>").
func systemAudit(at time.Time, tool string, args map[string]any) audit.Entry {
	raw, _ := json.Marshal(args)
	return audit.Entry{Time: at.UTC(), APIKeyName: "protection", Transport: audit.TransportSystem, Tool: tool, Arguments: raw, Result: audit.ResultOK}
}

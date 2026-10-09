package operations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
	// ErrJobRecreated is returned when an approved retention change applies to a job
	// that was deleted and created again under the same ID since the request.
	ErrJobRecreated = errors.New("operations: the job was recreated since the request")
	// ErrRequesterUnknown is returned when an API key without a creator requests a
	// destructive action while the two-person rule is on (adapters answer 403).
	ErrRequesterUnknown = errors.New("operations: the requester cannot be identified")
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

// Unwrap returns ErrApprovalRequired and auth.ErrAwaitingApproval.
func (e *ApprovalPendingError) Unwrap() []error {
	return []error{ErrApprovalRequired, auth.ErrAwaitingApproval}
}

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

// approvalSecrets reads the secret kept apart from a request (implemented by
// *store.SQLiteStore).
type approvalSecrets interface {
	ApprovalSecret(ctx context.Context, id string) (string, error)
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
// it (security.approval_requested) and returns it. An API key without a creator
// cannot request anything (ErrRequesterUnknown): nobody could tell whether its
// approver is a different person.
func (s *Service) storeApproval(ctx context.Context, a *models.Approval) (*models.Approval, error) {
	if p := auth.PrincipalFrom(ctx); p != nil && p.Method == auth.MethodAPIKey && p.User == nil {
		return nil, public("the two-person rule is on and this API key has no creator (it was imported or created before users existed), "+
			"so its requests cannot be told apart from an approver's: use a key created by a user, or the dashboard", ErrRequesterUnknown)
	}
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
	// The secret stays in the store only.
	a.Secret = ""
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
	if err := auth.CheckApprover(auth.PrincipalFrom(ctx), "", time.Time{}); err != nil {
		return nil, err
	}
	// The secret (a new password hash) is read first: deciding clears it.
	secret := ""
	if sr, ok := s.cfg.Store.(approvalSecrets); ok {
		if v, err := sr.ApprovalSecret(ctx, id); err == nil {
			secret = v
		}
	}
	a, err := s.decide(ctx, id, models.ApprovalApproved, func(a *models.Approval) error {
		return auth.CheckApprover(auth.PrincipalFrom(ctx), a.RequestedByUserID, a.CreatedAt)
	}, "")
	if err != nil {
		return a, err
	}
	a.Secret = secret
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
		if a.SubjectCreatedAt == nil {
			return "", public("the request is not bound to a job; request it again", ErrInvalid)
		}
		c, err := s.scheduleRetention(ctx, a.Subject, a.RetentionDays, a.RetentionCount, a.SubjectCreatedAt)
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
	case models.ApprovalGrantAdminRole:
		if s.cfg.Users == nil {
			return "", public("users are not available", ErrUnavailable)
		}
		if _, err := s.cfg.Users.SetUserRole(ctx, auth.PrincipalFrom(ctx), a.Subject, auth.RoleAdmin); err != nil {
			return "", err
		}
		s.destructive(ctx, "grant_admin_role", "user "+a.Subject+" is now an administrator", nil)
		return "user " + a.Subject + " is now an administrator", nil
	case models.ApprovalGrantAdminKey:
		if s.cfg.Users == nil {
			return "", public("API keys are not available", ErrUnavailable)
		}
		if _, err := s.cfg.Users.GrantAPIKeyAdmin(ctx, auth.PrincipalFrom(ctx), a.Subject); err != nil {
			return "", err
		}
		s.destructive(ctx, "grant_admin_api_key", "API key "+a.Subject+" has the admin scope", nil)
		return "API key " + a.Subject + " has the admin scope", nil
	case models.ApprovalOIDCAdminMapping:
		if err := s.applyOIDCGrant(ctx, a); err != nil {
			return "", err
		}
		return "the single sign-on settings that can grant admin are applied", nil
	case models.ApprovalResetPassword:
		if s.cfg.Users == nil {
			return "", public("users are not available", ErrUnavailable)
		}
		if a.Secret == "" {
			return "", public("the request carries no password; request it again", ErrInvalid)
		}
		if err := s.cfg.Users.ApplyPasswordReset(ctx, auth.PrincipalFrom(ctx), a.Subject, a.Secret); err != nil {
			return "", err
		}
		s.destructive(ctx, "reset_password", "the password of user "+a.Subject+" was reset", nil)
		return "the password of user " + a.Subject + " is reset", nil
	case models.ApprovalChangeAdminRole:
		if s.cfg.Users == nil {
			return "", public("users are not available", ErrUnavailable)
		}
		if _, err := s.cfg.Users.SetUserRole(ctx, auth.PrincipalFrom(ctx), a.Subject, auth.Role(a.Role)); err != nil {
			return "", err
		}
		s.destructive(ctx, "demote_admin", "administrator "+a.Subject+" is now "+a.Role, nil)
		s.forgetAdmin(ctx, a.Subject)
		return "user " + a.Subject + " is now " + a.Role, nil
	case models.ApprovalSSODemoteAdmin:
		if s.cfg.Users == nil {
			return "", public("users are not available", ErrUnavailable)
		}
		if _, err := s.cfg.Users.ApplyProviderRole(ctx, auth.PrincipalFrom(ctx), a.Subject, auth.Role(a.Role)); err != nil {
			return "", err
		}
		s.destructive(ctx, "demote_admin", "administrator "+a.Subject+" is now "+a.Role+" (single sign-on)", nil)
		s.forgetAdmin(ctx, a.Subject)
		return "user " + a.Subject + " is now " + a.Role, nil
	case models.ApprovalDeleteAdmin:
		if s.cfg.Users == nil {
			return "", public("users are not available", ErrUnavailable)
		}
		if err := s.cfg.Users.DeleteUser(ctx, auth.PrincipalFrom(ctx), a.Subject); err != nil {
			return "", err
		}
		s.destructive(ctx, "delete_admin", "administrator "+a.Subject+" was deleted", nil)
		s.forgetAdmin(ctx, a.Subject)
		return "user " + a.Subject + " is deleted", nil
	case models.ApprovalShortenMetadataRetention:
		if a.MetadataRetentionCount == nil {
			return "", public("the request names no count", ErrInvalid)
		}
		c, err := s.scheduleMetadataRetention(ctx, *a.MetadataRetentionCount)
		if err != nil {
			return "", err
		}
		return "metadata snapshot retention change scheduled for " + c.EffectiveAt.Format(time.RFC3339), nil
	case models.ApprovalDisableLockedCopies:
		c, err := s.scheduleDisableLockedCopies(ctx)
		if err != nil {
			return "", err
		}
		return "turning security.require_locked_copies off scheduled for " + c.EffectiveAt.Format(time.RFC3339), nil
	case models.ApprovalLowerObjectLock:
		if a.ObjectLock == nil {
			return "", public("the request names no object lock", ErrInvalid)
		}
		c, err := s.scheduleObjectLock(ctx, a.Subject, *a.ObjectLock, a.SubjectCreatedAt)
		if err != nil {
			return "", err
		}
		return "object lock change scheduled for " + c.EffectiveAt.Format(time.RFC3339), nil
	case models.ApprovalRestoreDropTarget:
		if a.Restore == nil {
			return "", public("the request names no restore", ErrInvalid)
		}
		rec, err := s.StartRestore(ctx, *a.Restore)
		if err != nil {
			return "", err
		}
		return "restore " + rec.ID + " started", nil
	case models.ApprovalRotateSecretKey:
		res, err := s.RotateSecretKey(ctx)
		if err != nil {
			return "", err
		}
		return "secret.key rotated (new fingerprint " + res.NewFingerprint + "); every user has to sign in again", nil
	case models.ApprovalPostRestoreCommands:
		return s.applyPostRestoreChange(ctx, a)
	default:
		return "", public("unknown approval action "+string(a.Action), ErrInvalid)
	}
}

// UserAdmin applies the account changes that the two-person rule held back once they
// are approved (implemented by *auth.Service).
type UserAdmin interface {
	// SetUserRole changes the role of user id.
	SetUserRole(ctx context.Context, actor *auth.Principal, id string, role auth.Role) (*auth.RoleChange, error)
	// GrantAPIKeyAdmin raises key id to the admin scope.
	GrantAPIKeyAdmin(ctx context.Context, actor *auth.Principal, id string) (*auth.APIKey, error)
	// ApplyPasswordReset sets the password hash of user id.
	ApplyPasswordReset(ctx context.Context, actor *auth.Principal, id, hash string) error
	// DeleteUser deletes user id.
	DeleteUser(ctx context.Context, actor *auth.Principal, id string) error
	// ApplyProviderRole sets the role a single sign-on gave user id.
	ApplyProviderRole(ctx context.Context, actor *auth.Principal, id string, role auth.Role) (*auth.RoleChange, error)
}

// RequestSignInDemotion asks a second administrator to apply g.Role, the role a
// single sign-on gave administrator g.UserID, who kept the admin role because the
// two-person rule is on (auth.AdminGrantGate). An open request for the same user and
// role is not repeated at every sign-in.
func (s *Service) RequestSignInDemotion(ctx context.Context, g auth.AdminGrant) error {
	st, err := s.approvals()
	if err != nil {
		return err
	}
	open, err := st.ListApprovals(ctx, models.ApprovalPending, store.MaxApprovalList)
	if err != nil {
		return fmt.Errorf("list approval requests: %w", err)
	}
	now := s.now()
	for _, a := range open {
		if a.Action == models.ApprovalSSODemoteAdmin && a.Subject == g.UserID && a.Role == string(g.Role) && a.Open(now) {
			return nil
		}
	}
	_, err = s.storeApproval(ctx, &models.Approval{Action: models.ApprovalSSODemoteAdmin, Subject: g.UserID, Role: string(g.Role),
		Summary: fmt.Sprintf("apply the role %s that single sign-on gave administrator %s (%s); the admin role stays until then", g.Role, g.Username, g.UserID)})
	return err
}

// HoldsAdminGrants reports whether a grant of admin rights in ctx must wait for a
// second administrator (auth.AdminGrantGate): the two-person rule is on and ctx is
// not an approved action.
func (s *Service) HoldsAdminGrants(ctx context.Context) bool {
	return s.needsApproval(ctx)
}

// RequestAdminGrant asks a second administrator to approve g (auth.AdminGrantGate)
// and returns the *ApprovalPendingError the auth service returns to its caller.
func (s *Service) RequestAdminGrant(ctx context.Context, g auth.AdminGrant) error {
	switch g.Kind {
	case auth.GrantAdminRole:
		summary := fmt.Sprintf("give user %s (%s) the admin role", g.Username, g.UserID)
		if g.Created {
			summary = fmt.Sprintf("give the new user %s (%s, created as a viewer) the admin role", g.Username, g.UserID)
		}
		return s.requestApproval(ctx, &models.Approval{Action: models.ApprovalGrantAdminRole, Subject: g.UserID, Summary: summary})
	case auth.GrantAdminKey:
		summary := fmt.Sprintf("give API key %s (%s) the admin scope", g.KeyName, g.KeyID)
		if g.Created {
			summary = fmt.Sprintf("give the new API key %s (%s, created with the operator scope) the admin scope", g.KeyName, g.KeyID)
		}
		return s.requestApproval(ctx, &models.Approval{Action: models.ApprovalGrantAdminKey, Subject: g.KeyID, Summary: summary})
	case auth.ResetUserPassword:
		return s.requestApproval(ctx, &models.Approval{Action: models.ApprovalResetPassword, Subject: g.UserID, Secret: g.PasswordHash,
			Summary: fmt.Sprintf("reset the password of user %s (%s); they will be forced to choose a new password at their next sign-in", g.Username, g.UserID)})
	case auth.DemoteAdmin:
		return s.requestApproval(ctx, &models.Approval{Action: models.ApprovalChangeAdminRole, Subject: g.UserID, Role: string(g.Role),
			Summary: fmt.Sprintf("change administrator %s (%s) to %s", g.Username, g.UserID, g.Role)})
	case auth.DeleteAdmin:
		return s.requestApproval(ctx, &models.Approval{Action: models.ApprovalDeleteAdmin, Subject: g.UserID,
			Summary: fmt.Sprintf("delete administrator %s (%s)", g.Username, g.UserID)})
	default:
		return public("unknown admin grant "+string(g.Kind), ErrInvalid)
	}
}

// holdOIDCGrant takes an oidc settings change that can grant admin through single
// sign-on out of p while the two-person rule is on, and asks a second administrator
// to approve it; it returns the request (nil when nothing was held). A change that
// carries a new client secret is refused: secrets are never kept in requests.
func (s *Service) holdOIDCGrant(ctx context.Context, p *settings.Patch) (*models.Approval, error) {
	if p.OIDC == nil || !s.needsApproval(ctx) {
		return nil, nil
	}
	cur := s.settings()
	next, err := settings.Preview(cur, settings.Patch{OIDC: p.OIDC})
	if err != nil {
		return nil, err
	}
	grants := settings.OIDCGrantsAdmin(cur.OIDC, next.OIDC)
	if !grants && !settings.OIDCRevokesAdmin(cur.OIDC, next.OIDC, s.hasOIDCAdmins(ctx)) {
		return nil, nil
	}
	summary := "change the single sign-on settings so that identity provider groups can get the admin role"
	if !grants {
		summary = "change the single sign-on settings so that administrators can lose the admin role at their next sign-in"
	}
	if sec := p.OIDC.ClientSecret; sec != nil && *sec != settings.SecretMask {
		return nil, public("this single sign-on change can grant or take away the admin role and needs a second administrator, "+
			"but requests never keep secrets: save oidc.client_secret on its own first, then the rest", ErrInvalid)
	}
	held := *p.OIDC
	held.ClientSecret = nil
	raw, err := json.Marshal(held) //nolint:gosec // G117: ClientSecret was cleared above; requests never keep secrets.
	if err != nil {
		return nil, fmt.Errorf("encode the oidc change: %w", err)
	}
	a, err := s.storeApproval(ctx, &models.Approval{Action: models.ApprovalOIDCAdminMapping, Settings: raw, Summary: summary})
	if err != nil {
		return nil, err
	}
	p.OIDC = nil
	return a, nil
}

// userLister lists the users (implemented by *store.SQLiteStore).
type userLister interface {
	ListUsers(ctx context.Context) ([]*auth.User, error)
}

// adminIDs returns the IDs of the users with the admin role, sorted by username.
func (s *Service) adminIDs(ctx context.Context) ([]string, error) {
	st, ok := s.cfg.Store.(userLister)
	if !ok {
		return nil, nil
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	var ids []string
	for _, u := range users {
		if u != nil && u.Role == auth.RoleAdmin {
			ids = append(ids, u.ID)
		}
	}
	return ids, nil
}

// adminLost returns the ID of an administrator recorded in pending change c
// (PendingDisableSecondApprover) who is no longer an administrator, or "".
func (s *Service) adminLost(ctx context.Context, c *models.PendingChange) (string, error) {
	if len(c.Admins) == 0 {
		return "", nil
	}
	now, err := s.adminIDs(ctx)
	if err != nil {
		return "", err
	}
	for _, id := range c.Admins {
		if !slices.Contains(now, id) {
			return id, nil
		}
	}
	return "", nil
}

// forgetAdmin removes administrator id, demoted or deleted by an approved request,
// from the pending change that turns the two-person rule off: an approved loss of
// the admin role does not cancel it (see adminLost).
func (s *Service) forgetAdmin(ctx context.Context, id string) {
	st, ok := s.cfg.Store.(pendingStore)
	if !ok {
		return
	}
	list, err := st.ListPendingChanges(ctx)
	if err != nil {
		s.logger.Warn("cannot list pending protection changes", logsafe.Error(err))
		return
	}
	for _, c := range list {
		if c.Kind != models.PendingDisableSecondApprover || !slices.Contains(c.Admins, id) {
			continue
		}
		c.Admins = slices.DeleteFunc(c.Admins, func(v string) bool { return v == id })
		if _, err = st.ReplacePendingChange(ctx, c); err != nil {
			s.logger.Warn("cannot update the pending change that turns the two-person rule off", logsafe.Attr("change_id", c.ID), logsafe.Error(err))
		}
	}
}

// hasOIDCAdmins reports whether a user who signs in with single sign-on holds the
// admin role; true when the users cannot be read.
func (s *Service) hasOIDCAdmins(ctx context.Context) bool {
	st, ok := s.cfg.Store.(userLister)
	if !ok {
		return false
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		return true
	}
	for _, u := range users {
		if u != nil && u.Role == auth.RoleAdmin && u.AuthProvider == auth.ProviderOIDC {
			return true
		}
	}
	return false
}

// applyOIDCGrant applies the oidc change of approved request a.
func (s *Service) applyOIDCGrant(ctx context.Context, a *models.Approval) error {
	if s.cfg.SettingsUpdater == nil {
		return public("settings are not available", ErrUnavailable)
	}
	var p settings.OIDCPatch
	if err := json.Unmarshal(a.Settings, &p); err != nil {
		return public("the request carries no valid oidc change", ErrInvalid)
	}
	p.ClientSecret = nil
	if _, _, err := s.cfg.SettingsUpdater.UpdateChanged(settings.WithLoweredProtection(ctx), settings.Patch{OIDC: &p}); err != nil {
		return err
	}
	s.destructive(ctx, "oidc_admin_mapping", "single sign-on settings that can grant the admin role were applied", nil)
	return nil
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
// earliest first. A caller limited to some connections sees the settings changes
// and the retention changes of its own jobs.
func (s *Service) PendingChanges(ctx context.Context) ([]*models.PendingChange, error) {
	st, err := s.pending()
	if err != nil {
		return []*models.PendingChange{}, nil //nolint:nilerr // Without the store nothing is pending.
	}
	list, err := st.ListPendingChanges(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pending changes: %w", err)
	}
	if auth.ConnectionFilter(ctx).Limited() {
		list = slices.DeleteFunc(list, func(c *models.PendingChange) bool {
			if c.JobID == "" {
				return false
			}
			_, getErr := s.store.GetJob(ctx, c.JobID)
			return getErr != nil
		})
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
//
// bound, when set, is the creation time of the job the request was made for: a job
// deleted and recreated under the same ID since is refused (ErrJobRecreated).
func (s *Service) scheduleRetention(ctx context.Context, jobID string, days, count *int, bound *time.Time) (*models.PendingChange, error) {
	job, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, notFound(err, "job not found")
	}
	if bound != nil && !job.CreatedAt.Equal(*bound) {
		return nil, public(fmt.Sprintf("job %s was deleted and created again since the request; the request does not apply to the new job", jobID), ErrJobRecreated)
	}
	created := job.CreatedAt
	c, err := s.schedulePending(ctx, &models.PendingChange{Kind: models.PendingRetention, JobID: jobID, RetentionDays: days, RetentionCount: count, JobCreatedAt: &created})
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
		case models.PendingMetadataRetention:
			applyErr = s.applyMetadataRetentionChange(ctx, c)
		case models.PendingObjectLock:
			applyErr = s.applyObjectLockChange(ctx, c)
		case models.PendingDisableLockedCopies:
			applyErr = s.applyDisableLockedCopies(ctx, c)
		case models.PendingDisableSecondApprover:
			// Only while the lockout lasts: with two administrators again, turning the
			// rule off needs an approval like before.
			if s.cfg.SecondApproverCheck != nil && s.cfg.SecondApproverCheck(ctx) == nil {
				s.logger.Warn("dropping the pending change that turns the two-person rule off: two administrators can approve again",
					logsafe.Attr("change_id", c.ID))
				continue
			}
			// An administrator demoted or deleted without an approval while the change
			// waited is how one credential would make itself the last administrator.
			if lost, lostErr := s.adminLost(ctx, c); lostErr != nil || lost != "" {
				s.logger.Warn("dropping the pending change that turns the two-person rule off: an administrator lost the admin role without an approval while it waited",
					logsafe.Attr("change_id", c.ID), logsafe.Attr("user_id", lost), logsafe.Error(lostErr))
				s.destructive(ctx, "disable_second_approver_dropped", "the pending change that turns the two-person rule off was dropped: "+
					"an administrator lost the admin role without an approval while it waited", nil)
				continue
			}
			applyErr = s.disableSecondApprover(withApproval(ctx, &models.Approval{RequestedBy: c.RequestedBy}, "the grace period (no second administrator could approve)"))
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
	existing, err := s.store.GetJob(ctx, c.JobID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if c.JobCreatedAt != nil && !existing.CreatedAt.Equal(*c.JobCreatedAt) {
		// The job was deleted and created again: the change was for the old one.
		return nil
	}
	job := existing.Clone()
	persist := func() error {
		current, getErr := s.store.GetJob(ctx, c.JobID)
		if getErr != nil {
			return getErr
		}
		if c.JobCreatedAt != nil && !current.CreatedAt.Equal(*c.JobCreatedAt) {
			return errNotDueYet
		}
		*job = *current.Clone()
		if c.RetentionDays != nil {
			job.RetentionDays = *c.RetentionDays
		}
		if c.RetentionCount != nil {
			job.RetentionCount = *c.RetentionCount
		}
		return s.store.UpdateJob(ctx, job)
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
// administrators (ErrTooFewAdmins), and turning security.require_locked_copies off
// is delayed like a lower grace period. A higher grace period applies at once and
// cancels a pending lowering. Expected failures: ErrUnavailable, ErrTooFewAdmins and
// the settings errors.
func (s *Service) UpdateSettings(ctx context.Context, p settings.Patch) (*SettingsUpdate, error) {
	if s.cfg.SettingsUpdater == nil {
		return nil, public("settings are not available", ErrUnavailable)
	}
	cur := s.settings().Security
	var lowerGrace *int
	disable, lowerLocked := false, false
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
		if v := sec.RequireLockedCopies; v != nil && !*v && cur.RequireLockedCopies {
			lowerLocked, sec.RequireLockedCopies = true, nil
		}
		p.Security = &sec
	}
	var lowerMeta *int
	curMeta := s.settings().MetadataBackup.RetentionCount
	if p.MetadataBackup != nil && p.MetadataBackup.RetentionCount != nil && *p.MetadataBackup.RetentionCount < curMeta {
		v := *p.MetadataBackup.RetentionCount
		if v < 1 || v > settings.MaxMetadataBackupRetention {
			return nil, fmt.Errorf("%w: metadata_backup.retention_count must be between 1 and %d", settings.ErrInvalid, settings.MaxMetadataBackupRetention)
		}
		mb := *p.MetadataBackup
		mb.RetentionCount, lowerMeta = nil, &v
		p.MetadataBackup = &mb
	}
	oidcHeld, err := s.holdOIDCGrant(ctx, &p)
	if err != nil {
		return nil, err
	}
	next, changed, err := s.cfg.SettingsUpdater.UpdateChanged(ctx, p)
	if err != nil {
		return nil, err
	}
	out := &SettingsUpdate{Settings: next, Changed: changed, Pending: []*models.PendingChange{}, Approvals: []*models.Approval{}}
	if oidcHeld != nil {
		out.Approvals = append(out.Approvals, oidcHeld)
	}
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
	if next.MetadataBackup.RetentionCount > curMeta {
		if st, ok := s.cfg.Store.(pendingStore); ok {
			if _, err := st.DeletePendingChangesOf(ctx, models.PendingMetadataRetention, ""); err != nil {
				return nil, fmt.Errorf("cancel the pending metadata retention change: %w", err)
			}
		}
	}
	if lowerMeta != nil {
		if s.needsApproval(ctx) {
			a, err := s.storeApproval(ctx, &models.Approval{Action: models.ApprovalShortenMetadataRetention, MetadataRetentionCount: lowerMeta,
				Summary: fmt.Sprintf("keep %d metadata snapshots instead of %d", *lowerMeta, curMeta)})
			if err != nil {
				return nil, err
			}
			out.Approvals = append(out.Approvals, a)
		} else {
			c, err := s.scheduleMetadataRetention(ctx, *lowerMeta)
			if err != nil {
				return nil, err
			}
			out.Pending = append(out.Pending, c)
		}
	}
	if lockErr := s.holdLockedCopies(ctx, cur, next.Security, lowerLocked, out); lockErr != nil {
		return nil, lockErr
	}
	if disable {
		// Without two administrators nobody could approve: turning the rule off then
		// waits for the grace period instead, so a lockout always ends.
		if s.cfg.SecondApproverCheck != nil && errors.Is(s.cfg.SecondApproverCheck(ctx), ErrTooFewAdmins) {
			admins, err := s.adminIDs(ctx)
			if err != nil {
				return nil, err
			}
			c, err := s.schedulePending(ctx, &models.PendingChange{Kind: models.PendingDisableSecondApprover, Admins: admins})
			if err != nil {
				return nil, err
			}
			s.destructive(ctx, "disable_second_approver", "the two-person rule is turned off at "+c.EffectiveAt.Format(time.RFC3339)+
				" (fewer than two administrators could approve it)", nil)
			out.Pending = append(out.Pending, c)
			return out, nil
		}
		a, err := s.storeApproval(ctx, &models.Approval{Action: models.ApprovalDisableSecondApprover,
			Summary: "turn the two-person rule (security.require_second_approver) off"})
		if err != nil {
			return nil, err
		}
		out.Approvals = append(out.Approvals, a)
	}
	return out, nil
}

// holdLockedCopies cancels a pending turning off of security.require_locked_copies
// when it was turned on again (cur is the security settings before the update, next
// after it) and, for lower (the update turns it off), schedules turning it off after
// the grace period or, with the two-person rule, asks a second administrator first.
func (s *Service) holdLockedCopies(ctx context.Context, cur, next settings.Security, lower bool, out *SettingsUpdate) error {
	if next.RequireLockedCopies && !cur.RequireLockedCopies {
		if st, ok := s.cfg.Store.(pendingStore); ok {
			if _, err := st.DeletePendingChangesOf(ctx, models.PendingDisableLockedCopies, ""); err != nil {
				return fmt.Errorf("cancel the pending locked copies change: %w", err)
			}
		}
	}
	if !lower {
		return nil
	}
	if s.needsApproval(ctx) {
		a, err := s.storeApproval(ctx, &models.Approval{Action: models.ApprovalDisableLockedCopies,
			Summary: "stop requiring Object Lock on the copy targets of new jobs (security.require_locked_copies)"})
		if err != nil {
			return err
		}
		out.Approvals = append(out.Approvals, a)
		return nil
	}
	c, err := s.scheduleDisableLockedCopies(ctx)
	if err != nil {
		return err
	}
	out.Pending = append(out.Pending, c)
	return nil
}

// scheduleDisableLockedCopies schedules turning security.require_locked_copies off
// after the grace period in force.
func (s *Service) scheduleDisableLockedCopies(ctx context.Context) (*models.PendingChange, error) {
	c, err := s.schedulePending(ctx, &models.PendingChange{Kind: models.PendingDisableLockedCopies})
	if err != nil {
		return nil, err
	}
	s.destructive(ctx, "disable_locked_copies", "security.require_locked_copies is turned off at "+c.EffectiveAt.Format(time.RFC3339), nil)
	return c, nil
}

// applyDisableLockedCopies turns security.require_locked_copies off, unless it
// already is.
func (s *Service) applyDisableLockedCopies(ctx context.Context, c *models.PendingChange) error {
	if s.cfg.SettingsUpdater == nil || !s.settings().Security.RequireLockedCopies {
		return nil
	}
	off := false
	if _, _, err := s.cfg.SettingsUpdater.UpdateChanged(settings.WithLoweredProtection(ctx), settings.Patch{Security: &settings.SecurityPatch{RequireLockedCopies: &off}}); err != nil {
		return err
	}
	ctx = withApproval(ctx, &models.Approval{RequestedBy: c.RequestedBy}, c.ApprovedBy)
	s.destructive(ctx, "apply_disable_locked_copies", "security.require_locked_copies is off", nil)
	return nil
}

// scheduleMetadataRetention schedules lowering metadata_backup.retention_count to n
// after the grace period in force.
func (s *Service) scheduleMetadataRetention(ctx context.Context, n int) (*models.PendingChange, error) {
	c, err := s.schedulePending(ctx, &models.PendingChange{Kind: models.PendingMetadataRetention, MetadataRetentionCount: &n})
	if err != nil {
		return nil, err
	}
	s.destructive(ctx, "shorten_metadata_retention", fmt.Sprintf("metadata snapshots kept lowered to %d, effective %s", n, c.EffectiveAt.Format(time.RFC3339)), nil)
	return c, nil
}

// applyMetadataRetentionChange lowers metadata_backup.retention_count to the value
// of c, unless it is already at most that.
func (s *Service) applyMetadataRetentionChange(ctx context.Context, c *models.PendingChange) error {
	if c.MetadataRetentionCount == nil || s.cfg.SettingsUpdater == nil {
		return nil
	}
	n := *c.MetadataRetentionCount
	if n >= s.settings().MetadataBackup.RetentionCount {
		return nil
	}
	if _, _, err := s.cfg.SettingsUpdater.UpdateChanged(settings.WithLoweredProtection(ctx), settings.Patch{MetadataBackup: &settings.MetadataBackupPatch{RetentionCount: &n}}); err != nil {
		return err
	}
	ctx = withApproval(ctx, &models.Approval{RequestedBy: c.RequestedBy}, c.ApprovedBy)
	s.destructive(ctx, "apply_metadata_retention", fmt.Sprintf("metadata snapshots kept: %d", n), nil)
	return nil
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
		page, err := s.store.QueryBackupRecords(ctx, store.BackupFilter{JobID: job.ID, Limit: 1})
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
		var bound *time.Time
		if job, err := s.store.GetJob(ctx, jobID); err == nil {
			created := job.CreatedAt
			bound = &created
		}
		a, err := s.storeApproval(ctx, &models.Approval{Action: models.ApprovalShortenRetention, Subject: jobID,
			RetentionDays: h.Days, RetentionCount: h.Count, SubjectCreatedAt: bound,
			Summary: fmt.Sprintf("shorten the retention of job %s from %s to %s", jobID, h.From, retentionText(h.Days, h.Count))})
		if err != nil {
			return nil, err
		}
		out.Approval = a
		out.PendingRetention = s.pendingRetention(ctx, jobID)
		return out, nil
	}
	c, err := s.scheduleRetention(ctx, jobID, h.Days, h.Count, nil)
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

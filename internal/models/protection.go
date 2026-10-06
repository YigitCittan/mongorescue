package models

import (
	"encoding/json"
	"time"
)

// Delete protection: soft deletes with a grace period, delayed lowering of
// protections and the optional two-person rule (see docs/security.md).

// Delete grace period bounds, in days (setting security.delete_grace_days).
const (
	// DefaultDeleteGraceDays is the grace period of a fresh installation.
	DefaultDeleteGraceDays = 7
	// MinDeleteGraceDays is the shortest grace period.
	MinDeleteGraceDays = 1
	// MaxDeleteGraceDays is the longest grace period.
	MaxDeleteGraceDays = 90
)

// ApprovalTTL is how long a request for a second approval stays open.
const ApprovalTTL = 72 * time.Hour

// GraceDuration returns days as a duration, clamped to the allowed range.
func GraceDuration(days int) time.Duration {
	days = max(MinDeleteGraceDays, min(days, MaxDeleteGraceDays))
	return time.Duration(days) * 24 * time.Hour
}

// PendingChangeKind names what a pending change lowers.
type PendingChangeKind string

// Pending change kinds.
const (
	// PendingRetention shortens the retention of job JobID.
	PendingRetention PendingChangeKind = "retention"
	// PendingDeleteGrace lowers security.delete_grace_days.
	PendingDeleteGrace PendingChangeKind = "delete_grace_days"
	// PendingMetadataRetention lowers metadata_backup.retention_count.
	PendingMetadataRetention PendingChangeKind = "metadata_backup_retention"
	// PendingDisableSecondApprover turns security.require_second_approver off when
	// fewer than two administrators could approve it (a lockout).
	PendingDisableSecondApprover PendingChangeKind = "disable_second_approver"
)

// PendingChange is a lowered protection that takes effect only at EffectiveAt (the
// end of the grace period in force when it was requested), so a stolen credential
// cannot shorten retention or the grace period to destroy backups at once.
type PendingChange struct {
	// ID identifies the change ("chg_" + random hex).
	ID string `json:"id"`
	// Kind is what the change lowers.
	Kind PendingChangeKind `json:"kind"`
	// JobID is the job whose retention is shortened (PendingRetention).
	JobID string `json:"job_id,omitempty"`
	// JobCreatedAt binds the change to that job: a job deleted and recreated under
	// the same ID is a different job, and the change is dropped.
	JobCreatedAt *time.Time `json:"job_created_at,omitempty"`
	// RetentionDays and RetentionCount are the new retention values; nil keeps the
	// job's current one (PendingRetention).
	RetentionDays  *int `json:"retention_days,omitempty"`
	RetentionCount *int `json:"retention_count,omitempty"`
	// DeleteGraceDays is the new grace period (PendingDeleteGrace).
	DeleteGraceDays *int `json:"delete_grace_days,omitempty"`
	// MetadataRetentionCount is the new metadata snapshot count
	// (PendingMetadataRetention).
	MetadataRetentionCount *int `json:"metadata_retention_count,omitempty"`
	// Admins are the IDs of the administrators when the two-person rule was asked
	// to turn off (PendingDisableSecondApprover): the change is dropped when one of
	// them lost the admin role or was deleted by then without an approval.
	Admins []string `json:"admins,omitempty"`
	// RequestedBy names who asked for the change.
	RequestedBy string `json:"requested_by"`
	// ApprovedBy names the second administrator who approved it, if any.
	ApprovedBy string `json:"approved_by,omitempty"`
	// CreatedAt is when the change was requested.
	CreatedAt time.Time `json:"created_at"`
	// EffectiveAt is when the scheduler applies it.
	EffectiveAt time.Time `json:"effective_at"`
}

// Subject is the key a pending change replaces older ones of: the job of a retention
// change, "" for the grace period.
func (c *PendingChange) Subject() string {
	if c.Kind == PendingRetention {
		return c.JobID
	}
	return ""
}

// ApprovalAction names a destructive action that waits for a second administrator.
type ApprovalAction string

// Actions that need a second approval when security.require_second_approver is on.
const (
	// ApprovalDeleteBackup deletes backup Subject.
	ApprovalDeleteBackup ApprovalAction = "delete_backup"
	// ApprovalBulkDeleteBackups deletes the backups IDs.
	ApprovalBulkDeleteBackups ApprovalAction = "bulk_delete_backups"
	// ApprovalUnpinBackup unpins backup Subject.
	ApprovalUnpinBackup ApprovalAction = "unpin_backup"
	// ApprovalBulkUnpinBackups unpins the backups IDs.
	ApprovalBulkUnpinBackups ApprovalAction = "bulk_unpin_backups"
	// ApprovalDeleteTarget deletes storage target Subject.
	ApprovalDeleteTarget ApprovalAction = "delete_storage_target"
	// ApprovalShortenRetention shortens the retention of job Subject (the change then
	// waits for the grace period like any shortening).
	ApprovalShortenRetention ApprovalAction = "shorten_retention"
	// ApprovalLowerGrace lowers security.delete_grace_days (then delayed too).
	ApprovalLowerGrace ApprovalAction = "lower_delete_grace"
	// ApprovalDisableSecondApprover turns security.require_second_approver off.
	ApprovalDisableSecondApprover ApprovalAction = "disable_second_approver"
	// ApprovalGrantAdminRole gives user Subject the admin role (an administrator
	// created as a viewer, or a promotion).
	ApprovalGrantAdminRole ApprovalAction = "grant_admin_role"
	// ApprovalGrantAdminKey gives API key Subject the admin scope (created with the
	// operator scope).
	ApprovalGrantAdminKey ApprovalAction = "grant_admin_api_key"
	// ApprovalOIDCAdminMapping applies an oidc settings change (Settings) that can
	// grant or take away admin through single sign-on.
	ApprovalOIDCAdminMapping ApprovalAction = "oidc_admin_mapping"
	// ApprovalResetPassword sets the password of user Subject (another user's; the
	// new bcrypt hash is kept apart from the request, never shown).
	ApprovalResetPassword ApprovalAction = "reset_password"
	// ApprovalChangeAdminRole demotes administrator Subject to Role.
	ApprovalChangeAdminRole ApprovalAction = "demote_admin"
	// ApprovalDeleteAdmin deletes administrator Subject.
	ApprovalDeleteAdmin ApprovalAction = "delete_admin"
	// ApprovalSSODemoteAdmin applies the role Role that the identity provider's
	// groups gave administrator Subject at a single sign-on: the sign-in kept the
	// admin role while the two-person rule is on.
	ApprovalSSODemoteAdmin ApprovalAction = "sso_demote_admin"
	// ApprovalShortenMetadataRetention lowers metadata_backup.retention_count (then
	// delayed by the grace period).
	ApprovalShortenMetadataRetention ApprovalAction = "shorten_metadata_retention"
	// ApprovalRestoreDropTarget runs the in-place restore Restore that drops the
	// target database first.
	ApprovalRestoreDropTarget ApprovalAction = "restore_drop_target"
	// ApprovalRotateSecretKey rotates secret.key (every session ends).
	ApprovalRotateSecretKey ApprovalAction = "rotate_secret_key"
	// ApprovalPostRestoreCommands replaces the post-restore commands of connection
	// Subject with a list that removes or changes some of them (which could bring
	// erased data back after a restore). The new list is the request's Secret,
	// sealed in the store.
	ApprovalPostRestoreCommands ApprovalAction = "reduce_post_restore_commands"
)

// ApprovalStatus is the state of an approval request.
type ApprovalStatus string

// Approval states.
const (
	// ApprovalPending waits for a decision.
	ApprovalPending ApprovalStatus = "pending"
	// ApprovalApproved was approved and its action ran.
	ApprovalApproved ApprovalStatus = "approved"
	// ApprovalFailed was approved but its action failed (Error says why).
	ApprovalFailed ApprovalStatus = "failed"
	// ApprovalRejected was rejected (or withdrawn by its requester).
	ApprovalRejected ApprovalStatus = "rejected"
	// ApprovalExpired was not decided within ApprovalTTL.
	ApprovalExpired ApprovalStatus = "expired"
)

// Approval is a destructive action that waits for a second administrator
// (security.require_second_approver). Only a different administrator, signed in with
// a session, can approve it; API keys can request but never approve.
type Approval struct {
	// ID identifies the request ("apr_" + random hex).
	ID string `json:"id"`
	// Action is what runs when it is approved.
	Action ApprovalAction `json:"action"`
	// Status is the request's state.
	Status ApprovalStatus `json:"status"`
	// Summary describes the action in English.
	Summary string `json:"summary"`
	// Subject is the backup, storage target or job the action applies to.
	Subject string `json:"subject,omitempty"`
	// IDs are the backups of a bulk action.
	IDs []string `json:"ids,omitempty"`
	// Reason is the optional reason the requester gave.
	Reason string `json:"reason,omitempty"`
	// RetentionDays and RetentionCount are the new retention (ApprovalShortenRetention).
	RetentionDays  *int `json:"retention_days,omitempty"`
	RetentionCount *int `json:"retention_count,omitempty"`
	// DeleteGraceDays is the new grace period (ApprovalLowerGrace).
	DeleteGraceDays *int `json:"delete_grace_days,omitempty"`
	// Settings is the settings change of ApprovalOIDCAdminMapping (the oidc group,
	// never a secret).
	Settings json.RawMessage `json:"settings,omitempty"`
	// SubjectCreatedAt binds the request to the creation time of its job (or user):
	// a job deleted and recreated under the same ID is refused.
	SubjectCreatedAt *time.Time `json:"subject_created_at,omitempty"`
	// Role is the new role of ApprovalChangeAdminRole.
	Role string `json:"role,omitempty"`
	// MetadataRetentionCount is the new count of ApprovalShortenMetadataRetention.
	MetadataRetentionCount *int `json:"metadata_retention_count,omitempty"`
	// Restore is the in-place restore of ApprovalRestoreDropTarget.
	Restore *RestoreRequest `json:"restore,omitempty"`
	// Secret is kept apart from the request in the store (the new password hash of
	// ApprovalResetPassword) and never serialized; it is cleared once the request is
	// decided.
	Secret string `json:"-"`
	// RequestedBy names the requester; RequestedByUserID is their user ID ("" for an
	// API key without a user); RequestedVia is "session", "api_key" or "system".
	RequestedBy       string `json:"requested_by"`
	RequestedByUserID string `json:"requested_by_user_id,omitempty"`
	RequestedVia      string `json:"requested_via"`
	// CreatedAt is when it was requested; ExpiresAt when it expires undecided.
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// DecidedBy and DecidedByUserID name who approved or rejected it, DecidedAt when.
	DecidedBy       string     `json:"decided_by,omitempty"`
	DecidedByUserID string     `json:"decided_by_user_id,omitempty"`
	DecidedAt       *time.Time `json:"decided_at,omitempty"`
	// Result describes what the approved action did; Error why it failed.
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Open reports whether the request still waits for a decision at now.
func (a *Approval) Open(now time.Time) bool {
	return a.Status == ApprovalPending && now.Before(a.ExpiresAt)
}

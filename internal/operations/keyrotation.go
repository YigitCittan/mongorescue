package operations

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Kinds of key rotations (events.SecurityKeyRotated, Event.Action).
const (
	// KeyKindSecretKey is a rotation of secret.key.
	KeyKindSecretKey = "secret_key"
	// KeyKindEncryption is a rotation of the backup encryption key.
	KeyKindEncryption = "encryption"
	// KeyKindStorageCredentials is a rotation of storage target credentials.
	KeyKindStorageCredentials = "storage_credentials"
)

// SecretKeyRotator rotates secret.key (implemented by *keyrotation.Rotator).
type SecretKeyRotator interface {
	// FromEnv reports a key from MONGORESCUE_SECRET_KEY, which cannot be rotated.
	FromEnv() bool
	// RotateAs rotates the key on behalf of actor, for approval approvalID.
	RotateAs(ctx context.Context, actor, approvalID string) (*keyrotation.Result, error)
}

// RotateSecretKey rotates secret.key: every sealed credential is re-sealed under a
// new key and every dashboard session ends. It needs an administrator signed in to
// the dashboard (the caller confirms the password, see internal/server); with the
// two-person rule it waits for a second administrator (*ApprovalPendingError). A key
// from MONGORESCUE_SECRET_KEY is refused with keyrotation.ErrEnvKey.
func (s *Service) RotateSecretKey(ctx context.Context) (*keyrotation.Result, error) {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return nil, err
	}
	r := s.cfg.KeyRotator
	if r == nil {
		return nil, public("secret key rotation is not available", ErrUnavailable)
	}
	if r.FromEnv() {
		return nil, keyrotation.ErrEnvKey
	}
	if approvalOf(ctx) == nil {
		if p := auth.PrincipalFrom(ctx); p == nil || p.Method != auth.MethodSession {
			return nil, auth.ErrSessionRequired
		}
	}
	if s.needsApproval(ctx) {
		return nil, s.requestApproval(ctx, &models.Approval{Action: models.ApprovalRotateSecretKey, Subject: KeyKindSecretKey,
			Summary: "rotate secret.key (every user has to sign in again)"})
	}
	by, approvedBy := actorNames(ctx)
	if approvedBy != "" {
		by += " (approved by " + approvedBy + ")"
	}
	approvalID := ""
	if a := approvalOf(ctx); a != nil {
		approvalID = a.approval.ID
	}
	res, err := r.RotateAs(ctx, by, approvalID)
	if errors.Is(err, keyrotation.ErrIncomplete) && res != nil {
		// The database uses the new key; the next start installs the key file and
		// sends security.key_rotated then. The audit entry records it now.
		auditlog.Annotate(ctx, "key_rotated", KeyKindSecretKey+" (completes at the next start)")
		auditlog.Annotate(ctx, "old_fingerprint", res.OldFingerprint)
		auditlog.Annotate(ctx, "new_fingerprint", res.NewFingerprint)
	}
	if err != nil {
		return nil, err
	}
	s.keyRotated(ctx, KeyKindSecretKey, res.OldFingerprint, res.NewFingerprint, res.Skipped...)
	return res, nil
}

// KeyRotationCompleted publishes security.key_rotated for a secret key rotation
// that the startup recovery completed (keyrotation.Opened.Completed): one a crash
// or a failed key file install had interrupted after its commit.
func (s *Service) KeyRotationCompleted(ctx context.Context, m *store.KeyRotation) {
	if m == nil {
		return
	}
	by := m.Actor
	if by == "" {
		by = "unknown"
	}
	detail := "old fingerprint " + m.OldFingerprint + ", new fingerprint " + m.NewFingerprint + " (completed at startup)"
	s.publish(context.WithoutCancel(ctx), events.SecurityEvent(events.SecurityKeyRotated, s.now(), KeyKindSecretKey, by, m.ApprovalID, detail))
	if s.cfg.Audit != nil {
		s.cfg.Audit.Record(context.WithoutCancel(ctx), systemAudit(s.now(), "keyrotation.completed", map[string]any{
			"kind": KeyKindSecretKey, "old_fingerprint": m.OldFingerprint, "new_fingerprint": m.NewFingerprint,
			"actor": m.Actor, "approval_id": m.ApprovalID,
		}))
	}
	s.logger.Info("key rotated", slog.String("kind", KeyKindSecretKey), slog.String("old_fingerprint", m.OldFingerprint),
		slog.String("new_fingerprint", m.NewFingerprint), slog.String("completed", "at startup"))
}

// keyRotated records a key rotation: a security.key_rotated event naming the kind,
// the old and new fingerprints (never key material), the actor and the approval,
// and the same facts on the request's audit entry. skipped names the stored secrets
// that could not be re-sealed (locations only).
func (s *Service) keyRotated(ctx context.Context, kind, oldFP, newFP string, skipped ...string) {
	by, approvedBy := actorNames(ctx)
	if approvedBy != "" {
		by += " (approved by " + approvedBy + ")"
	}
	detail := "old fingerprint " + oldFP + ", new fingerprint " + newFP
	if len(skipped) > 0 {
		list := strings.Join(skipped, ", ")
		detail += "; " + strconv.Itoa(len(skipped)) + " unreadable secrets left as they were: " + list
		auditlog.Annotate(ctx, "skipped", list)
	}
	e := events.SecurityEvent(events.SecurityKeyRotated, s.now(), kind, by, "", detail)
	if a := approvalOf(ctx); a != nil {
		e.ApprovalID = a.approval.ID
	}
	s.publish(context.WithoutCancel(ctx), e)
	auditlog.Annotate(ctx, "key_rotated", kind)
	auditlog.Annotate(ctx, "old_fingerprint", oldFP)
	auditlog.Annotate(ctx, "new_fingerprint", newFP)
	s.logger.With(actorAttrs(ctx)...).Info("key rotated", slog.String("kind", kind),
		slog.String("old_fingerprint", oldFP), slog.String("new_fingerprint", newFP))
}

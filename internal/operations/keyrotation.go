package operations

import (
	"context"
	"log/slog"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/models"
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
	// Rotate rotates the key.
	Rotate(ctx context.Context) (*keyrotation.Result, error)
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
	res, err := r.Rotate(ctx)
	if err != nil {
		return nil, err
	}
	s.keyRotated(ctx, KeyKindSecretKey, res.OldFingerprint, res.NewFingerprint)
	return res, nil
}

// keyRotated records a key rotation: a security.key_rotated event naming the kind,
// the old and new fingerprints (never key material), the actor and the approval,
// and the same facts on the request's audit entry.
func (s *Service) keyRotated(ctx context.Context, kind, oldFP, newFP string) {
	by, approvedBy := actorNames(ctx)
	if approvedBy != "" {
		by += " (approved by " + approvedBy + ")"
	}
	detail := "old fingerprint " + oldFP + ", new fingerprint " + newFP
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

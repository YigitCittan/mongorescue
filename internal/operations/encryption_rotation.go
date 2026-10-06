package operations

import (
	"context"
	"slices"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/reencrypt"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// Reencrypter runs the background re-encryption of existing backups (implemented by
// *reencrypt.Service).
type Reencrypter interface {
	// Trigger starts a job; requestedBy names the caller.
	Trigger(ctx context.Context, requestedBy string) (*reencrypt.State, error)
	// Status returns the latest job (nil when none ran).
	Status(ctx context.Context) (*reencrypt.State, error)
}

// EncryptionRotationRequest is the body of POST /api/v1/encryption/rotate.
type EncryptionRotationRequest struct {
	// Passphrase is the new passphrase (passphrase mode only; required there).
	Passphrase string `json:"passphrase,omitempty"`
	// Reencrypt starts a background job re-encrypting existing backups under the
	// new key; their old archives go through the delete grace period.
	Reencrypt bool `json:"reencrypt,omitempty"`
}

// EncryptionRotation is the outcome of RotateEncryptionKey. It never carries
// secrets: X25519 keys are named by their public keys.
type EncryptionRotation struct {
	// Mode is the encryption mode (x25519 or passphrase).
	Mode settings.EncryptionMode `json:"mode"`
	// OldFingerprint and NewFingerprint name the keys: the public keys of the
	// X25519 identities, or "passphrase" (passphrases have no public part).
	OldFingerprint string `json:"old_fingerprint"`
	NewFingerprint string `json:"new_fingerprint"`
	// Recipients are the public keys new backups are encrypted to.
	Recipients []string `json:"recipients,omitempty"`
	// Job is the re-encryption job, when one was started.
	Job *reencrypt.State `json:"reencryption,omitempty"`
}

// RotateEncryptionKey replaces the backup encryption key: in X25519 mode a new
// identity is generated and its public key replaces the old identity's among the
// recipients; in passphrase mode req.Passphrase replaces the passphrase. The old
// key is retired, so backups taken before stay restorable. With req.Reencrypt a
// background job re-encrypts them under the new key. It needs the admin scope.
func (s *Service) RotateEncryptionKey(ctx context.Context, req EncryptionRotationRequest) (*EncryptionRotation, error) {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return nil, err
	}
	if s.cfg.SettingsUpdater == nil {
		return nil, public("settings are not configured", ErrUnavailable)
	}
	if req.Reencrypt && s.cfg.Reencrypter == nil {
		return nil, public("re-encryption of existing backups is not available", ErrUnavailable)
	}
	cur := s.settings().Encryption
	if !cur.Enabled {
		return nil, public("backup encryption is disabled; enable it before rotating its key", ErrInvalid)
	}
	out := &EncryptionRotation{Mode: cur.Mode}
	var patch settings.EncryptionPatch
	switch cur.Mode {
	case settings.ModePassphrase:
		if strings.TrimSpace(req.Passphrase) == "" {
			return nil, public("passphrase mode: the new passphrase is required", ErrInvalid)
		}
		if req.Passphrase == cur.Passphrase {
			return nil, public("the new passphrase must differ from the current one", ErrInvalid)
		}
		patch.Passphrase = &req.Passphrase
		out.OldFingerprint, out.NewFingerprint = "passphrase", "passphrase"
	default:
		if req.Passphrase != "" {
			return nil, public("passphrase is only used in passphrase mode", ErrInvalid)
		}
		if cur.Identity == "" {
			return nil, public("recipient-only mode: MongoRescue holds no private key to rotate; generate a new key pair "+
				"yourself and replace the recipient", ErrInvalid)
		}
		oldRecipients, err := encryption.IdentityRecipients(cur.Identity)
		if err != nil {
			return nil, err
		}
		identity, recipient, err := encryption.GenerateX25519()
		if err != nil {
			return nil, err
		}
		recipients := []string{recipient}
		for _, r := range cur.Recipients {
			if !slices.Contains(oldRecipients, r) {
				recipients = append(recipients, r)
			}
		}
		patch.Identity, patch.Recipients = &identity, &recipients
		out.OldFingerprint, out.NewFingerprint, out.Recipients = strings.Join(oldRecipients, ","), recipient, recipients
	}
	if _, _, err := s.cfg.SettingsUpdater.UpdateChanged(ctx, settings.Patch{Encryption: &patch}); err != nil {
		return nil, err // settings.ErrInvalid answers 400
	}
	s.keyRotated(ctx, KeyKindEncryption, out.OldFingerprint, out.NewFingerprint)
	if req.Reencrypt {
		by, _ := actorNames(ctx)
		job, err := s.cfg.Reencrypter.Trigger(ctx, by)
		if err != nil {
			return out, public("the key was rotated, but re-encryption could not start: "+err.Error(), ErrBusy)
		}
		out.Job = job
	}
	return out, nil
}

// ReencryptionStatus returns the latest re-encryption job (nil when none ran).
func (s *Service) ReencryptionStatus(ctx context.Context) (*reencrypt.State, error) {
	if err := auth.RequireScope(ctx, auth.ScopeRead); err != nil {
		return nil, err
	}
	if s.cfg.Reencrypter == nil {
		return nil, nil
	}
	return s.cfg.Reencrypter.Status(ctx)
}

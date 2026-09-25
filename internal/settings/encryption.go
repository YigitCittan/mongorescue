package settings

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/encryption"
)

// validateEncryption normalises and checks the encryption settings. Key material is
// parsed but never echoed in errors.
func validateEncryption(e *Encryption, strictPassphrase bool) error {
	switch e.Mode {
	case "":
		e.Mode = ModeX25519
	case ModeX25519, ModePassphrase:
	default:
		return fmt.Errorf("%w: encryption.mode must be x25519 or passphrase", ErrInvalid)
	}
	recipients := make([]string, 0, len(e.Recipients))
	for _, r := range e.Recipients {
		if r = strings.TrimSpace(r); r != "" && !slices.Contains(recipients, r) {
			recipients = append(recipients, r)
		}
	}
	e.Recipients = recipients
	if len(recipients) > maxRecipients {
		return fmt.Errorf("%w: encryption.recipients allows at most %d entries", ErrInvalid, maxRecipients)
	}
	if len(recipients) > 0 {
		if _, err := encryption.NewX25519Encryptor(recipients); err != nil {
			return fmt.Errorf("%w: encryption.recipients: %w", ErrInvalid, err)
		}
	}
	e.Identity = strings.TrimSpace(e.Identity)
	if len(e.Identity) > maxSecretLength || len(e.Passphrase) > maxSecretLength {
		return fmt.Errorf("%w: encryption keys must be at most %d bytes", ErrInvalid, maxSecretLength)
	}
	if e.Identity != "" {
		if _, err := encryption.IdentityRecipients(e.Identity); err != nil {
			return fmt.Errorf("%w: encryption.identity: %w", ErrInvalid, err)
		}
	}
	if strictPassphrase && e.Passphrase != "" && len(e.Passphrase) < minPassphraseLength {
		return fmt.Errorf("%w: encryption.passphrase must be at least %d characters", ErrInvalid, minPassphraseLength)
	}
	if !e.Enabled {
		return nil
	}
	switch {
	case e.Mode == ModeX25519 && len(recipients) == 0:
		return fmt.Errorf("%w: encryption.recipients: add at least one public key (age1...) to enable encryption", ErrInvalid)
	case e.Mode == ModePassphrase && e.Passphrase == "":
		return fmt.Errorf("%w: encryption.passphrase is required in passphrase mode", ErrInvalid)
	}
	return nil
}

// recipientsOf returns the comma-separated public keys of identity, or "".
func recipientsOf(identity string) string {
	r, err := encryption.IdentityRecipients(identity)
	if err != nil {
		return ""
	}
	return strings.Join(r, ",")
}

// newEncryptor builds the encryptor of e, or nil when encryption is disabled.
func newEncryptor(e Encryption) (*encryption.Encryptor, error) {
	if !e.Enabled {
		return nil, nil
	}
	if e.Mode == ModePassphrase {
		return encryption.NewScryptEncryptor(e.Passphrase, 0)
	}
	return encryption.NewX25519Encryptor(e.Recipients)
}

// newDecryptor builds a decryptor from the current and every retired identity and
// passphrase, or nil when there is none. It works whether or not encryption is
// enabled, so older encrypted backups stay restorable.
func newDecryptor(e Encryption, logger *slog.Logger) (*encryption.Decryptor, error) {
	identities := []string{}
	if e.Identity != "" {
		identities = append(identities, e.Identity)
	}
	var passphrases []string
	for _, k := range e.RetiredKeys {
		switch k.Kind {
		case KindX25519:
			identities = append(identities, k.Secret)
		case KindPassphrase:
			passphrases = append(passphrases, k.Secret)
		}
	}
	if len(identities) == 0 && e.Passphrase == "" && len(passphrases) == 0 {
		return nil, nil
	}
	return encryption.NewDecryptor(encryption.DecryptorConfig{
		Identity:    strings.Join(identities, "\n"),
		Passphrase:  e.Passphrase,
		Passphrases: passphrases,
		Logger:      logger,
	})
}

// GeneratedKey is a new X25519 key pair.
type GeneratedKey struct {
	// Identity is the private key ("AGE-SECRET-KEY-1..."); it is not stored.
	Identity string `json:"identity"`
	// Recipient is the public key ("age1...").
	Recipient string `json:"recipient"`
}

// GenerateKey returns a new X25519 key pair without storing it. The dashboard shows
// the identity once and stores it only when the operator takes it into use.
func GenerateKey() (GeneratedKey, error) {
	identity, recipient, err := encryption.GenerateX25519()
	if err != nil {
		return GeneratedKey{}, err
	}
	return GeneratedKey{Identity: identity, Recipient: recipient}, nil
}

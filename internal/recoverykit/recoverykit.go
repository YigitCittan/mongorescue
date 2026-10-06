// Package recoverykit builds the recovery kit: a tar archive, sealed with age
// scrypt encryption under a passphrase the administrator chooses, holding what is
// needed to bring MongoRescue back on a new host:
//
//   - secret.key, without which the credentials in mongorescue.db (and in its
//     metadata snapshots) cannot be opened;
//   - identities.txt, the age X25519 private keys (current and retired) that decrypt
//     backups and metadata snapshots, when any is configured;
//   - recovery.json, the encryption settings (passphrases are never included, only
//     whether one is configured), every storage target with its credentials and the
//     location of the latest metadata snapshot;
//   - README.txt, the recovery steps.
//
// The kit is the one place where MongoRescue hands out its secrets in plain form; it
// is only produced for a signed-in administrator who confirmed their password (see
// internal/server), and its contents are never logged.
//
// The package also computes the fingerprint of the material a kit carries, which
// drives the reminder to download a new kit (settings.WarningRecoveryKit).
package recoverykit

import (
	"archive/tar"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// Passphrase limits.
const (
	// MinPassphraseLength is the minimum length (in characters) of the passphrase
	// that seals a kit.
	MinPassphraseLength = 12
	// MaxPassphraseLength bounds the passphrase (in bytes).
	MaxPassphraseLength = 1024
)

// Names of the files inside the kit.
const (
	// ReadmeName is the file with the recovery steps.
	ReadmeName = "README.txt"
	// SecretKeyName is the copy of secret.key.
	SecretKeyName = secretbox.KeyFileName
	// PreviousKeyName is the copy of secret.key.previous, the key a rotation
	// replaced, when the kit was asked to carry it.
	PreviousKeyName = secretbox.PreviousKeyFileName
	// IdentitiesName holds the age X25519 private keys, one per line.
	IdentitiesName = "identities.txt"
	// ManifestName is the JSON document with settings, targets and the snapshot.
	ManifestName = "recovery.json"
)

// Format identifies the manifest format.
const Format = "mongorescue-recovery-kit"

// fingerprintPurpose derives the key of the material fingerprint from secret.key.
const fingerprintPurpose = "recovery-kit-fingerprint"

// Sentinel errors. Their messages are safe to show to clients.
var (
	// ErrPassphrase is returned for a passphrase that is too short or too long.
	ErrPassphrase = fmt.Errorf("recoverykit: the passphrase must be between %d characters and %d bytes", MinPassphraseLength, MaxPassphraseLength)
)

// Settings is the settings port (implemented by *settings.Service).
type Settings interface {
	// Current returns the live settings, secrets included.
	Current() settings.Settings
	// NoteRecoveryKitFingerprint records the fingerprint of the current material.
	NoteRecoveryKitFingerprint(fingerprint string)
	// MarkRecoveryKitDownloaded records a downloaded kit.
	MarkRecoveryKitDownloaded(ctx context.Context, fingerprint string, at time.Time) error
}

// Targets lists storage targets (implemented by *targets.Service).
type Targets interface {
	// List returns every storage target with masked secrets.
	List(ctx context.Context) ([]*models.StorageTarget, error)
	// Resolve returns target id with its secret.
	Resolve(ctx context.Context, id string) (*models.StorageTarget, error)
}

// Config holds the dependencies of a Service. SecretKey, Settings and Targets are
// required.
type Config struct {
	// SecretKey is the raw key of secret.key.
	SecretKey []byte
	// SecretKeyFromEnv reports that the key came from MONGORESCUE_SECRET_KEY.
	SecretKeyFromEnv bool
	// Settings provides the encryption settings and stores the kit state.
	Settings Settings
	// Targets provides the storage targets.
	Targets Targets
	// LatestSnapshot returns the last stored metadata snapshot (nil when none).
	LatestSnapshot func(ctx context.Context) *metabackup.Snapshot
	// MetadataPrefix is the storage key prefix of this installation's metadata
	// snapshots (metabackup.Service.Prefix).
	MetadataPrefix string
	// PreviousKeyFile is the path of secret.key.previous ("" when unknown); see
	// PrepareOptions.IncludePreviousKey.
	PreviousKeyFile string
	// Version is the MongoRescue version written to the kit.
	Version string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// WorkFactor is the scrypt work factor of the seal (0 = the age default).
	WorkFactor int
}

// Service builds recovery kits. It is safe for concurrent use.
type Service struct {
	cfg Config

	mu     sync.RWMutex
	fpKey  []byte
	keyB64 string
	prefix string
}

// New returns a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Settings == nil || cfg.Targets == nil {
		return nil, errors.New("recoverykit: Settings and Targets are required")
	}
	fpKey, err := secretbox.DeriveSubkey(cfg.SecretKey, fingerprintPurpose)
	if err != nil {
		return nil, fmt.Errorf("recoverykit: %w", err)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg, fpKey: fpKey, keyB64: secretbox.EncodeKey(cfg.SecretKey), prefix: cfg.MetadataPrefix}, nil
}

// SetSecretKey switches the kit to a rotated secret.key and the metadata prefix
// that goes with it. The fingerprint changes, so the reminder to download a new kit
// fires again.
func (s *Service) SetSecretKey(key []byte, metadataPrefix string) error {
	fpKey, err := secretbox.DeriveSubkey(key, fingerprintPurpose)
	if err != nil {
		return fmt.Errorf("recoverykit: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fpKey, s.keyB64, s.prefix = fpKey, secretbox.EncodeKey(key), metadataPrefix
	return nil
}

// keys returns the fingerprint key, the encoded secret key and the metadata prefix.
func (s *Service) keys() (fpKey []byte, keyB64, prefix string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fpKey, s.keyB64, s.prefix
}

// ValidatePassphrase checks the passphrase that seals a kit.
func ValidatePassphrase(p string) error {
	if utf8.RuneCountInString(p) < MinPassphraseLength || len(p) > MaxPassphraseLength || strings.TrimSpace(p) == "" {
		return ErrPassphrase
	}
	return nil
}

// FileName returns the download name of a kit created at.
func FileName(at time.Time) string {
	return "mongorescue-recovery-kit-" + at.UTC().Format("2006-01-02") + ".tar" + encryption.FileExtension
}

// fingerprintInput is what the fingerprint covers besides secret.key (which keys
// it): only public data, so no secret is ever hashed.
type fingerprintInput struct {
	Mode       settings.EncryptionMode `json:"mode"`
	Recipients []string                `json:"recipients"`
	Identity   []string                `json:"identity_recipients"`
	Retired    []fingerprintRetired    `json:"retired"`
	Targets    []fingerprintTarget     `json:"targets"`
}

type fingerprintRetired struct {
	Kind      string    `json:"kind"`
	Recipient string    `json:"recipient"`
	RetiredAt time.Time `json:"retired_at"`
}

type fingerprintTarget struct {
	ID        string             `json:"id"`
	Type      models.StorageType `json:"type"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// Fingerprint returns the fingerprint of the material a kit would carry now:
// secret.key, the encryption keys (by their public keys) and the storage targets (by
// ID and last change). Changing any of them changes it.
func (s *Service) Fingerprint(ctx context.Context) (string, error) {
	fpKey, _, _ := s.keys()
	return s.fingerprint(ctx, fpKey)
}

// fingerprint computes the fingerprint keyed with fpKey.
func (s *Service) fingerprint(ctx context.Context, fpKey []byte) (string, error) {
	enc := s.cfg.Settings.Current().Encryption
	in := fingerprintInput{Mode: enc.Mode, Recipients: slices.Clone(enc.Recipients), Retired: []fingerprintRetired{}, Targets: []fingerprintTarget{}}
	slices.Sort(in.Recipients)
	if enc.Identity != "" {
		ids, err := encryption.IdentityRecipients(enc.Identity)
		if err != nil {
			return "", fmt.Errorf("recoverykit: %w", err)
		}
		in.Identity = ids
	}
	for _, k := range enc.RetiredKeys {
		in.Retired = append(in.Retired, fingerprintRetired{Kind: k.Kind, Recipient: k.Recipient, RetiredAt: k.RetiredAt.UTC()})
	}
	list, err := s.cfg.Targets.List(ctx)
	if err != nil {
		return "", fmt.Errorf("recoverykit: list storage targets: %w", err)
	}
	for _, t := range list {
		in.Targets = append(in.Targets, fingerprintTarget{ID: t.ID, Type: t.Type, UpdatedAt: t.UpdatedAt.UTC()})
	}
	slices.SortFunc(in.Targets, func(a, b fingerprintTarget) int { return strings.Compare(a.ID, b.ID) })
	doc, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, fpKey)
	_, _ = mac.Write(doc)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Refresh computes the fingerprint and hands it to the settings service, which
// raises the reminder when no kit was downloaded for it.
func (s *Service) Refresh(ctx context.Context) error {
	fp, err := s.Fingerprint(ctx)
	if err != nil {
		return err
	}
	s.cfg.Settings.NoteRecoveryKitFingerprint(fp)
	return nil
}

// Manifest is the content of recovery.json.
type Manifest struct {
	// Format is "mongorescue-recovery-kit"; FormatVersion is 1.
	Format        string `json:"format"`
	FormatVersion int    `json:"format_version"`
	// CreatedAt is when the kit was built; Version the MongoRescue version.
	CreatedAt time.Time `json:"created_at"`
	Version   string    `json:"mongorescue_version,omitempty"`
	// SecretKey describes the secret.key file of the kit.
	SecretKey SecretKeyInfo `json:"secret_key"`
	// Encryption describes the backup encryption keys.
	Encryption EncryptionInfo `json:"encryption"`
	// StorageTargets lists every storage target with its credentials.
	StorageTargets []*models.StorageTarget `json:"storage_targets"`
	// MetadataPrefix is where this installation stores its metadata snapshots.
	MetadataPrefix string `json:"metadata_prefix,omitempty"`
	// MetadataSnapshot is the latest metadata snapshot (nil when none was stored).
	MetadataSnapshot *metabackup.Snapshot `json:"metadata_snapshot"`
}

// SecretKeyInfo describes the secret.key file of the kit.
type SecretKeyInfo struct {
	// File is the name of the file in the kit.
	File string `json:"file"`
	// FromEnv reports that the installation reads the key from
	// MONGORESCUE_SECRET_KEY (the file content is that value).
	FromEnv bool `json:"from_env"`
	// PreviousFile names the copy of the key the last rotation replaced ("" when
	// the kit does not carry it). Metadata snapshots taken before that rotation are
	// sealed with it.
	PreviousFile string `json:"previous_file,omitempty"`
}

// EncryptionInfo describes the backup encryption keys. Passphrases are never
// included: only whether one is configured.
type EncryptionInfo struct {
	// Enabled and Mode are the encryption settings.
	Enabled bool                    `json:"enabled"`
	Mode    settings.EncryptionMode `json:"mode"`
	// Recipients are the X25519 public keys new backups are encrypted to.
	Recipients []string `json:"recipients"`
	// IdentitiesFile names the file with the private keys ("" when none).
	IdentitiesFile string `json:"identities_file,omitempty"`
	// Identities describes the private keys of IdentitiesFile.
	Identities []IdentityInfo `json:"identities"`
	// PassphraseConfigured reports that a passphrase is stored; it is not in the kit.
	PassphraseConfigured bool `json:"passphrase_configured"`
	// RetiredPassphrases counts retired passphrases (not in the kit either).
	RetiredPassphrases int `json:"retired_passphrases"`
	// PassphraseHint explains where the passphrase can be found.
	PassphraseHint string `json:"passphrase_hint,omitempty"`
}

// IdentityInfo describes one private key of the identities file.
type IdentityInfo struct {
	// Recipients are its public keys.
	Recipients string `json:"recipients"`
	// Current reports the active identity; retired ones carry RetiredAt.
	Current   bool       `json:"current"`
	RetiredAt *time.Time `json:"retired_at,omitempty"`
}

// Kit is a prepared recovery kit. It holds secrets: never log or serialize it except
// through Seal.
type Kit struct {
	manifest    Manifest
	identities  string
	fingerprint string
	secretKey   string
	previousKey string
}

// PrepareOptions customises PrepareWith.
type PrepareOptions struct {
	// IncludePreviousKey adds secret.key.previous (the key the last secret key
	// rotation replaced), which opens metadata snapshots taken before the rotation.
	IncludePreviousKey bool
}

// ErrNoPreviousKey is returned by PrepareWith when the previous key was asked for
// but there is none (no rotation happened, or the file was removed).
var ErrNoPreviousKey = errors.New("recoverykit: there is no previous secret key (secret.key.previous) to include")

// CreatedAt returns when the kit was prepared.
func (k *Kit) CreatedAt() time.Time { return k.manifest.CreatedAt }

// Prepare gathers the kit's content.
func (s *Service) Prepare(ctx context.Context) (*Kit, error) {
	return s.PrepareWith(ctx, PrepareOptions{})
}

// PrepareWith gathers the kit's content with opts.
func (s *Service) PrepareWith(ctx context.Context, opts PrepareOptions) (*Kit, error) {
	fpKey, keyB64, prefix := s.keys()
	fp, err := s.fingerprint(ctx, fpKey)
	if err != nil {
		return nil, err
	}
	var previous string
	if opts.IncludePreviousKey {
		if s.cfg.PreviousKeyFile == "" {
			return nil, ErrNoPreviousKey
		}
		key, readErr := secretbox.ReadKeyFile(s.cfg.PreviousKeyFile)
		switch {
		case errors.Is(readErr, fs.ErrNotExist):
			return nil, ErrNoPreviousKey
		case readErr != nil:
			return nil, fmt.Errorf("recoverykit: %w", readErr)
		}
		previous = secretbox.EncodeKey(key)
	}
	enc := s.cfg.Settings.Current().Encryption
	m := Manifest{
		Format: Format, FormatVersion: 1, CreatedAt: s.cfg.Now().UTC(), Version: s.cfg.Version,
		SecretKey: SecretKeyInfo{File: SecretKeyName, FromEnv: s.cfg.SecretKeyFromEnv},
		Encryption: EncryptionInfo{
			Enabled: enc.Enabled, Mode: enc.Mode, Recipients: slices.Clone(enc.Recipients), Identities: []IdentityInfo{},
			PassphraseConfigured: enc.Passphrase != "",
		},
		StorageTargets: []*models.StorageTarget{},
		MetadataPrefix: prefix,
	}
	if m.Encryption.Recipients == nil {
		m.Encryption.Recipients = []string{}
	}
	var ids strings.Builder
	if enc.Identity != "" {
		r, _ := encryption.IdentityRecipients(enc.Identity)
		m.Encryption.Identities = append(m.Encryption.Identities, IdentityInfo{Recipients: strings.Join(r, ","), Current: true})
		fmt.Fprintf(&ids, "# current identity (public keys: %s)\n%s\n", strings.Join(r, ", "), strings.TrimSpace(enc.Identity))
	}
	for _, k := range enc.RetiredKeys {
		switch k.Kind {
		case settings.KindX25519:
			at := k.RetiredAt.UTC()
			m.Encryption.Identities = append(m.Encryption.Identities, IdentityInfo{Recipients: k.Recipient, RetiredAt: &at})
			fmt.Fprintf(&ids, "# retired %s (public keys: %s)\n%s\n", at.Format(time.RFC3339), k.Recipient, strings.TrimSpace(k.Secret))
		case settings.KindPassphrase:
			m.Encryption.RetiredPassphrases++
		}
	}
	if ids.Len() > 0 {
		m.Encryption.IdentitiesFile = IdentitiesName
	}
	if m.Encryption.PassphraseConfigured || m.Encryption.RetiredPassphrases > 0 {
		m.Encryption.PassphraseHint = "Passphrases are not part of the kit. They are stored, sealed with secret.key, in mongorescue.db " +
			"and its metadata snapshots; keep a copy in your password manager."
	}
	list, err := s.cfg.Targets.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("recoverykit: list storage targets: %w", err)
	}
	for _, t := range list {
		full, resolveErr := s.cfg.Targets.Resolve(ctx, t.ID)
		if resolveErr != nil {
			return nil, fmt.Errorf("recoverykit: read storage target %s: %w", t.ID, resolveErr)
		}
		full = full.Clone()
		full.LastTestAt, full.LastTestOK, full.LastTestError = nil, false, ""
		m.StorageTargets = append(m.StorageTargets, full)
	}
	if s.cfg.LatestSnapshot != nil {
		m.MetadataSnapshot = s.cfg.LatestSnapshot(ctx)
	}
	if previous != "" {
		m.SecretKey.PreviousFile = PreviousKeyName
	}
	return &Kit{manifest: m, identities: ids.String(), fingerprint: fp, secretKey: keyB64, previousKey: previous}, nil
}

// kitFile is one file of the kit archive.
type kitFile struct {
	name string
	mode int64
	body string
}

// Seal writes kit to w as a tar archive encrypted with age scrypt under passphrase.
// Nothing is buffered beyond one file of the kit. On error, w holds a truncated
// stream that must be discarded.
func (s *Service) Seal(w io.Writer, kit *Kit, passphrase string) error {
	if err := ValidatePassphrase(passphrase); err != nil {
		return err
	}
	enc, err := encryption.NewScryptEncryptor(passphrase, s.cfg.WorkFactor)
	if err != nil {
		return fmt.Errorf("recoverykit: %w", err)
	}
	aw, err := enc.Encrypt(w)
	if err != nil {
		return fmt.Errorf("recoverykit: %w", err)
	}
	tw := tar.NewWriter(aw)
	manifest, err := json.MarshalIndent(kit.manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("recoverykit: encode manifest: %w", err)
	}
	files := []kitFile{
		{ReadmeName, 0o644, readme(kit)},
		{SecretKeyName, 0o600, kit.secretKey + "\n"},
		{ManifestName, 0o600, string(manifest) + "\n"},
	}
	if kit.previousKey != "" {
		files = append(files, kitFile{PreviousKeyName, 0o600, kit.previousKey + "\n"})
	}
	if kit.identities != "" {
		files = append(files, kitFile{IdentitiesName, 0o600, kit.identities})
	}
	for _, f := range files {
		hdr := &tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), ModTime: kit.manifest.CreatedAt, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err = tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("recoverykit: write %s: %w", f.name, err)
		}
		if _, err = io.WriteString(tw, f.body); err != nil {
			return fmt.Errorf("recoverykit: write %s: %w", f.name, err)
		}
	}
	if err = tw.Close(); err != nil {
		return fmt.Errorf("recoverykit: close archive: %w", err)
	}
	if err = aw.Close(); err != nil {
		return fmt.Errorf("recoverykit: seal archive: %w", err)
	}
	return nil
}

// MarkDownloaded records that kit was handed out, which resolves the reminder until
// the material changes.
func (s *Service) MarkDownloaded(ctx context.Context, kit *Kit) error {
	return s.cfg.Settings.MarkRecoveryKitDownloaded(ctx, kit.fingerprint, kit.manifest.CreatedAt)
}

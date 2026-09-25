// Package encryption provides streaming, client-side encryption of backup archives at
// rest using the age file format (https://age-encryption.org/v1).
//
// Two key modes are supported:
//
//   - X25519: backups are encrypted to one or more "age1..." public keys (recipients)
//     and decrypted with the matching "AGE-SECRET-KEY-1..." private keys (identities).
//     The server that takes backups never needs to hold a private key.
//   - scrypt: backups are encrypted and decrypted with a shared passphrase.
//
// X25519 is the recommended mode. Passphrase (scrypt) mode deliberately makes key
// derivation expensive: at DefaultScryptWorkFactor each backup and each restore
// transiently allocates about 256 MiB and spends roughly a second of CPU, which is
// well above the tool's otherwise constant ~15-25 MB footprint.
//
// Both Encryptor and Decryptor operate on io.Writer / io.Reader streams, processing
// data in fixed 64 KiB age chunks so memory usage is independent of archive size.
//
// Error messages produced by this package never contain key material: recipients and
// identities are referenced by position (index or line number) only.
package encryption

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"filippo.io/age"
)

// Sentinel errors returned by the encryption package.
var (
	// ErrEncryptionKeyRequired indicates that no key material (recipient, identity or
	// passphrase) was supplied for an operation that requires it.
	ErrEncryptionKeyRequired = errors.New("encryption: key material required")

	// ErrInvalidRecipient indicates that an encryption recipient is not a valid
	// X25519 age public key ("age1...").
	ErrInvalidRecipient = errors.New("encryption: invalid age recipient")

	// ErrInvalidIdentity indicates that a decryption identity is not a valid X25519
	// age private key ("AGE-SECRET-KEY-1...") or the identity file cannot be read.
	ErrInvalidIdentity = errors.New("encryption: invalid age identity")

	// ErrInvalidWorkFactor indicates an scrypt work factor outside the supported range.
	ErrInvalidWorkFactor = errors.New("encryption: invalid scrypt work factor")

	// ErrDecryptionFailed indicates that a ciphertext could not be decrypted: no
	// configured identity matched, the passphrase was wrong, or the stream was
	// truncated or tampered with. The underlying age error is wrapped as well.
	ErrDecryptionFailed = errors.New("encryption: decryption failed")
)

// Mode identifies the age recipient type used to encrypt a backup.
type Mode string

const (
	// ModeX25519 denotes encryption to one or more X25519 public keys.
	ModeX25519 Mode = "x25519"

	// ModeScrypt denotes passphrase-based encryption (scrypt key derivation).
	ModeScrypt Mode = "scrypt"
)

// FileExtension is appended to the storage key of every encrypted backup artifact.
const FileExtension = ".age"

const (
	// DefaultScryptWorkFactor is the default scrypt work factor (log2 N), matching
	// age's own default. Deriving the key at this factor takes roughly one second and
	// transiently allocates about 256 MiB, once per backup or restore.
	DefaultScryptWorkFactor = 18

	// MaxScryptWorkFactor is the highest scrypt work factor accepted for encryption and
	// decryption. It equals DefaultScryptWorkFactor so that a crafted or foreign header
	// can never make a restore allocate more than ~256 MiB for key derivation.
	MaxScryptWorkFactor = 18

	// identitySizeLimit caps the size of an identity file or inline identity list.
	identitySizeLimit = 1 << 20
)

// Encryptor produces age ciphertext streams for a fixed set of recipients.
// It is safe for concurrent use: each Encrypt call is independent.
type Encryptor struct {
	mode       Mode
	recipients []age.Recipient
}

// NewX25519Encryptor returns an Encryptor that encrypts to every given X25519 public
// key. Blank entries are ignored. It returns ErrEncryptionKeyRequired if no recipient
// remains and ErrInvalidRecipient (with the offending index, never the value) if any
// entry fails to parse.
func NewX25519Encryptor(recipients []string) (*Encryptor, error) {
	parsed := make([]age.Recipient, 0, len(recipients))
	for i, raw := range recipients {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		r, err := age.ParseX25519Recipient(s)
		if err != nil {
			// The age error echoes the input; drop it in case a secret was pasted by mistake.
			return nil, fmt.Errorf("%w: recipient #%d is not an X25519 public key (age1...)", ErrInvalidRecipient, i)
		}
		parsed = append(parsed, r)
	}
	if len(parsed) == 0 {
		return nil, fmt.Errorf("%w: at least one recipient is required", ErrEncryptionKeyRequired)
	}
	return &Encryptor{mode: ModeX25519, recipients: parsed}, nil
}

// NewScryptEncryptor returns an Encryptor that derives the file key from passphrase.
// A workFactor of zero selects DefaultScryptWorkFactor; otherwise it must lie in
// [1, MaxScryptWorkFactor] or ErrInvalidWorkFactor is returned. Prefer
// NewX25519Encryptor: at the default factor every encryption transiently allocates
// about 256 MiB for scrypt key derivation.
func NewScryptEncryptor(passphrase string, workFactor int) (*Encryptor, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("%w: passphrase is empty", ErrEncryptionKeyRequired)
	}
	if workFactor == 0 {
		workFactor = DefaultScryptWorkFactor
	}
	if workFactor < 1 || workFactor > MaxScryptWorkFactor {
		return nil, fmt.Errorf("%w: %d (must be 1-%d)", ErrInvalidWorkFactor, workFactor, MaxScryptWorkFactor)
	}
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEncryptionKeyRequired, err)
	}
	r.SetWorkFactor(workFactor)
	return &Encryptor{mode: ModeScrypt, recipients: []age.Recipient{r}}, nil
}

// Mode reports the recipient type this Encryptor encrypts to.
func (e *Encryptor) Mode() Mode {
	return e.mode
}

// Encrypt writes the age header to dst and returns a WriteCloser that encrypts
// plaintext written to it into dst. Close MUST be called to flush and authenticate
// the final chunk; it does not close dst. On a failed write, callers should discard
// dst instead of calling Close, since Close would seal a truncated but valid stream.
func (e *Encryptor) Encrypt(dst io.Writer) (io.WriteCloser, error) {
	w, err := age.Encrypt(dst, e.recipients...)
	if err != nil {
		return nil, fmt.Errorf("encryption: start age stream: %w", err)
	}
	return w, nil
}

// DecryptorConfig lists the key material a Decryptor may use. Any combination may be
// set; every configured identity is tried against each ciphertext.
type DecryptorConfig struct {
	// Identity holds one or more inline X25519 private keys, one per line.
	Identity string

	// IdentityFile is the path of an age identity file (one key per line, "#" comments).
	IdentityFile string

	// Passphrase enables decryption of scrypt (passphrase) encrypted backups. Each
	// such restore transiently allocates up to ~256 MiB for key derivation.
	Passphrase string

	// Passphrases are further passphrases tried after Passphrase (e.g. retired ones).
	// Every passphrase that does not match costs one scrypt derivation.
	Passphrases []string

	// Logger receives non-fatal warnings (e.g. an identity file readable by group or
	// others). Nil uses slog.Default().
	Logger *slog.Logger
}

// Decryptor decrypts age ciphertext streams using a fixed set of identities.
// It is safe for concurrent use.
type Decryptor struct {
	identities []age.Identity
}

// NewDecryptor parses all key material in cfg. It returns ErrEncryptionKeyRequired
// if cfg is empty and ErrInvalidIdentity if an identity cannot be read or parsed.
// An identity file with group/other permission bits set is accepted but logged as a
// warning (except on Windows, where POSIX modes are not meaningful).
func NewDecryptor(cfg DecryptorConfig) (*Decryptor, error) {
	var ids []age.Identity

	if strings.TrimSpace(cfg.Identity) != "" {
		parsed, err := parseIdentities(strings.NewReader(cfg.Identity), "inline identity")
		if err != nil {
			return nil, err
		}
		ids = append(ids, parsed...)
	}

	if cfg.IdentityFile != "" {
		f, err := os.Open(cfg.IdentityFile)
		if err != nil {
			return nil, fmt.Errorf("%w: open identity file: %w", ErrInvalidIdentity, err)
		}
		warnIfExposed(f, cfg.IdentityFile, cfg.Logger)
		parsed, err := parseIdentities(f, "identity file")
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		ids = append(ids, parsed...)
	}

	for _, passphrase := range append([]string{cfg.Passphrase}, cfg.Passphrases...) {
		if passphrase == "" {
			continue
		}
		sid, err := age.NewScryptIdentity(passphrase)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrEncryptionKeyRequired, err)
		}
		sid.SetMaxWorkFactor(MaxScryptWorkFactor)
		ids = append(ids, sid)
	}

	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: no identity or passphrase configured", ErrEncryptionKeyRequired)
	}
	return &Decryptor{identities: ids}, nil
}

// Decrypt parses the age header from src and returns a Reader yielding plaintext.
// Header failures (no matching identity, wrong passphrase, corrupt header) are
// returned immediately; authentication failures later in the stream are returned by
// the Reader. Both wrap ErrDecryptionFailed and the original age error. Read errors
// of src itself (e.g. storage I/O) are passed through untagged.
func (d *Decryptor) Decrypt(src io.Reader) (io.Reader, error) {
	sr := &sourceReader{r: src}
	r, err := age.Decrypt(sr, d.identities...)
	if err != nil {
		return nil, classify(err, sr.err)
	}
	return &decryptingReader{r: r, src: sr}, nil
}

// sourceReader records the first non-EOF error of the ciphertext source so that
// transport failures can be told apart from cryptographic ones.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && s.err == nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return n, err
}

// decryptingReader tags mid-stream age failures (truncation, tampering) with
// ErrDecryptionFailed so callers can classify them.
type decryptingReader struct {
	r   io.Reader
	src *sourceReader
}

func (dr *decryptingReader) Read(p []byte) (int, error) {
	n, err := dr.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = classify(err, dr.src.err)
	}
	return n, err
}

// classify wraps an age error with ErrDecryptionFailed unless it originates from
// the ciphertext source, in which case it is reported as an I/O failure.
func classify(err, srcErr error) error {
	if srcErr != nil && errors.Is(err, srcErr) {
		return fmt.Errorf("encryption: read ciphertext: %w", err)
	}
	return fmt.Errorf("%w: %w", ErrDecryptionFailed, err)
}

// GenerateX25519 creates a new X25519 key pair and returns the private identity
// ("AGE-SECRET-KEY-1...") and its public recipient ("age1...").
func GenerateX25519() (identity, recipient string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", fmt.Errorf("encryption: generate identity: %w", err)
	}
	return id.String(), id.Recipient().String(), nil
}

// IdentityRecipients returns the public recipients ("age1...") of the X25519 identities
// in identity (one per line, blanks and "#" comments skipped). Errors never contain
// key material.
func IdentityRecipients(identity string) ([]string, error) {
	ids, err := parseIdentities(strings.NewReader(identity), "identity")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if x, ok := id.(*age.X25519Identity); ok {
			out = append(out, x.Recipient().String())
		}
	}
	return out, nil
}

// warnIfExposed logs a warning when an identity file is accessible to group or others.
func warnIfExposed(f *os.File, path string, logger *slog.Logger) {
	if runtime.GOOS == "windows" {
		return
	}
	info, err := f.Stat()
	if err != nil || info.Mode().Perm()&0o077 == 0 {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("age identity file is readable by group or others; restrict it with chmod 600",
		slog.String("path", path),
		slog.String("mode", info.Mode().Perm().String()),
	)
}

// parseIdentities reads X25519 identities one per line, skipping blanks and "#"
// comments. Parse errors reference the line number only, never the line content.
func parseIdentities(r io.Reader, source string) ([]age.Identity, error) {
	var ids []age.Identity
	scanner := bufio.NewScanner(io.LimitReader(r, identitySizeLimit))
	line := 0
	for scanner.Scan() {
		line++
		s := strings.TrimSpace(scanner.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		id, err := age.ParseX25519Identity(s)
		if err != nil {
			return nil, fmt.Errorf("%w: %s line %d is not an X25519 secret key (AGE-SECRET-KEY-1...)", ErrInvalidIdentity, source, line)
		}
		ids = append(ids, id)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrInvalidIdentity, source, err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: %s contains no keys", ErrInvalidIdentity, source)
	}
	return ids, nil
}

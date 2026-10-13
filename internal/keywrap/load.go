package keywrap

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// Retry bounds the attempts to reach the KMS.
type Retry struct {
	// Attempts is the number of tries (at least 1).
	Attempts int
	// Initial is the wait after the first failure; it doubles up to Max.
	Initial, Max time.Duration
	// Timeout bounds one attempt (0 = no bound beyond the context).
	Timeout time.Duration
}

// DefaultRetry tries 5 times over about 15 seconds, 20 seconds at most per try:
// enough for a KMS endpoint or a metadata service that is still coming up, short
// enough for a failed start to show.
var DefaultRetry = Retry{Attempts: 5, Initial: time.Second, Max: 8 * time.Second, Timeout: 20 * time.Second}

// normalized returns r with defaults for zero fields.
func (r Retry) normalized() Retry {
	if r.Attempts < 1 {
		r.Attempts = 1
	}
	if r.Initial <= 0 {
		r.Initial = time.Second
	}
	if r.Max < r.Initial {
		r.Max = r.Initial
	}
	return r
}

// sleepCtx waits d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// do runs op until it succeeds, fails permanently, the attempts are used up or ctx
// is done. It returns the number of attempts made.
func (r Retry) do(ctx context.Context, sleep func(context.Context, time.Duration) error, op func(context.Context) error) (int, error) {
	r = r.normalized()
	if sleep == nil {
		sleep = sleepCtx
	}
	wait := r.Initial
	var err error
	for attempt := 1; ; attempt++ {
		actx, cancel := ctx, context.CancelFunc(func() {})
		if r.Timeout > 0 {
			actx, cancel = context.WithTimeout(ctx, r.Timeout)
		}
		err = op(actx)
		cancel()
		if err == nil || IsPermanent(err) || errors.Is(err, ErrWrongKey) || attempt >= r.Attempts || ctx.Err() != nil {
			return attempt, err
		}
		if serr := sleep(ctx, wait); serr != nil {
			return attempt, err
		}
		wait = min(2*wait, r.Max)
	}
}

// Hint returns what an operator checks when the KMS of provider refuses or cannot
// be reached: the permissions and where the credentials come from.
func Hint(provider string) string {
	switch provider {
	case ProviderAWSKMS:
		return "check network access to AWS KMS and that the instance's identity (the AWS default credential chain: IRSA, an instance profile, AWS_* variables) " +
			"has kms:Decrypt and kms:Encrypt on the key"
	case ProviderGCPKMS:
		return "check network access to cloudkms.googleapis.com and that the Application Default Credentials (Workload Identity, an attached service account, " +
			"GOOGLE_APPLICATION_CREDENTIALS) have roles/cloudkms.cryptoKeyEncrypterDecrypter on the key"
	case ProviderAzureKeyVault:
		return "check network access to the key vault and that the DefaultAzureCredential identity (Workload Identity, a managed identity, AZURE_* variables) " +
			"has the wrapKey and unwrapKey permissions (Key Vault Crypto User) on the key"
	case ProviderVaultTransit:
		return "check network access to Vault and that the token (VAULT_TOKEN, AppRole VAULT_ROLE_ID/VAULT_SECRET_ID or Kubernetes VAULT_K8S_ROLE) " +
			"may update <mount>/encrypt/<key> and <mount>/decrypt/<key>"
	}
	return "check the key wrapping configuration"
}

// keyFileNames are the key files of a data directory, the current key first (see
// internal/keyrotation).
var keyFileNames = []string{secretbox.KeyFileName, secretbox.NextKeyFileName, secretbox.PreviousKeyFileName}

// LoadConfig holds the parameters of Load.
type LoadConfig struct {
	// DataDir is the data directory.
	DataDir string
	// Spec is the configured key wrapping (required).
	Spec Spec
	// Wrapper wraps with the KMS key of Spec (required).
	Wrapper Wrapper
	// Retry bounds the attempts to reach the KMS; the zero value means
	// DefaultRetry.
	Retry Retry
	// Logger logs what Load does (paths, provider, fingerprints; never keys).
	Logger *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Sleep waits between attempts; nil waits for real (tests replace it).
	Sleep func(context.Context, time.Duration) error
}

// Loaded is the result of Load.
type Loaded struct {
	// Key is the plaintext secret.key, in memory only.
	Key []byte
	// File is the content of secret.key.wrapped.
	File *File
	// Created reports that there was no key and a new one was generated and
	// wrapped. A caller that cannot open the database with it removes
	// secret.key.wrapped again.
	Created bool
	// Migrated names the plaintext key files that were wrapped and removed.
	Migrated []string
	// Attempts is the number of unwrap attempts of secret.key.wrapped.
	Attempts int
	// UnwrapDuration is how long unwrapping secret.key took, retries included.
	UnwrapDuration time.Duration
}

// Load returns the plaintext secret.key of the data directory, unwrapping
// secret.key.wrapped with the KMS. On the first start with wrapping on it wraps
// the plaintext key files (secret.key and those of a key rotation) and removes
// them once their wrapped counterparts were written and checked; without any key
// it generates one and stores it wrapped. It never falls back to a plaintext key:
// when the KMS refuses or stays unreachable, it fails with an error naming the
// provider and what to check.
func Load(ctx context.Context, cfg LoadConfig) (*Loaded, error) {
	if !cfg.Spec.Enabled() || cfg.Wrapper == nil || cfg.DataDir == "" {
		return nil, errors.New("keywrap: Load needs a data directory, a spec and a wrapper")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Retry == (Retry{}) {
		cfg.Retry = DefaultRetry
	}
	l := &loader{cfg: cfg, log: cfg.Logger.With(slog.String("key_wrap", cfg.Spec.String()))}
	out, err := l.current(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range keyFileNames[1:] {
		migrated, err := l.migrateAside(ctx, name)
		if err != nil {
			return nil, err
		}
		if migrated {
			out.Migrated = append(out.Migrated, name)
		}
	}
	if len(out.Migrated) > 0 {
		change := Change{Action: ActionWrapped, At: cfg.Now().UTC(), Actor: ActorSystem, Provider: cfg.Spec.Provider,
			KeyURI: cfg.Spec.KeyURI, KeyID: out.File.KeyID, Fingerprint: out.File.Fingerprint, Files: out.Migrated}
		if err := AppendJournal(cfg.DataDir, change); err != nil {
			l.log.Warn("could not journal the key wrapping for its security event", logsafe.Error(err))
		}
	}
	return out, nil
}

// loader carries the state of one Load.
type loader struct {
	cfg LoadConfig
	log *slog.Logger
}

// paths returns the plaintext and wrapped paths of a key file name.
func (l *loader) paths(name string) (plain, wrapped string) {
	plain = filepath.Join(l.cfg.DataDir, name)
	return plain, plain + Suffix
}

// open unwraps f, retrying a KMS that is unreachable.
func (l *loader) open(ctx context.Context, f *File, name string) ([]byte, int, error) {
	if f.Provider != l.cfg.Spec.Provider || f.KeyURI != l.cfg.Spec.KeyURI {
		return nil, 0, fmt.Errorf("%w: %s holds %s, the configuration names %s; run \"mongorescue key rewrap\" to move it to the new key, "+
			"or set %s back", ErrKeyMismatch, name+Suffix, f.Spec(), l.cfg.Spec, EnvKeyWrap)
	}
	return openRetry(ctx, f, l.cfg.Wrapper, l.cfg.Retry, l.cfg.Sleep, name+Suffix)
}

// openRetry unwraps f with w under retry and explains a failure; file names f in
// the error.
func openRetry(ctx context.Context, f *File, w Wrapper, r Retry, sleep func(context.Context, time.Duration) error, file string) ([]byte, int, error) {
	var key []byte
	attempts, err := r.do(ctx, sleep, func(ctx context.Context) error {
		var err error
		key, err = f.Open(ctx, w)
		return err
	})
	if err != nil {
		return nil, attempts, fmt.Errorf("could not unwrap %s with %s after %d attempt(s): %w; %s. MongoRescue never falls back to a plaintext key",
			file, f.Spec(), attempts, err, Hint(f.Provider))
	}
	return key, attempts, nil
}

// sealChecked wraps key under retry and checks that the result unwraps to key again.
func sealChecked(ctx context.Context, w Wrapper, spec Spec, key []byte, now time.Time, r Retry, sleep func(context.Context, time.Duration) error) (*File, error) {
	var f *File
	if _, err := r.do(ctx, sleep, func(ctx context.Context) error {
		var err error
		f, err = Seal(ctx, w, spec, key, now)
		return err
	}); err != nil {
		return nil, fmt.Errorf("could not wrap the key with %s: %w; %s", spec, err, Hint(spec.Provider))
	}
	back, _, err := openRetry(ctx, f, w, r, sleep, "the new wrapped key")
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(back, key) != 1 {
		return nil, ErrWrongKey
	}
	return f, nil
}

// writeChecked writes f to path and reads it back.
func writeChecked(path string, f *File) error {
	if err := WriteFile(path, f); err != nil {
		return err
	}
	back, err := ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(back.Ciphertext, f.Ciphertext) || back.Fingerprint != f.Fingerprint {
		return fmt.Errorf("keywrap: %s does not read back as written", filepath.Base(path))
	}
	return nil
}

// current loads secret.key.
func (l *loader) current(ctx context.Context) (*Loaded, error) {
	name := secretbox.KeyFileName
	plain, wrapped := l.paths(name)
	hasPlain, err := secretbox.KeyFileExists(plain)
	if err != nil {
		return nil, err
	}
	f, err := ReadFile(wrapped)
	switch {
	case err == nil:
		start := time.Now()
		key, attempts, err := l.open(ctx, f, name)
		if err != nil {
			return nil, err
		}
		out := &Loaded{Key: key, File: f, Attempts: attempts, UnwrapDuration: time.Since(start)}
		if hasPlain {
			if err := l.dropPlain(plain, key); err != nil {
				return nil, err
			}
			l.log.Warn("removed a plaintext secret.key next to secret.key.wrapped (left by an interrupted migration or copied back)")
		}
		l.log.Info("unwrapped secret.key with the key management service", slog.String("key_id", f.KeyID),
			slog.String("fingerprint", f.Fingerprint), slog.Int("attempts", attempts), slog.Duration("duration", out.UnwrapDuration))
		return out, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}

	created := false
	var key []byte
	if hasPlain {
		if key, err = secretbox.ReadKeyFile(plain); err != nil {
			return nil, err
		}
	} else {
		if key, err = secretbox.GenerateKey(); err != nil {
			return nil, err
		}
		created = true
	}
	start := time.Now()
	f, err = sealChecked(ctx, l.cfg.Wrapper, l.cfg.Spec, key, l.cfg.Now(), l.cfg.Retry, l.cfg.Sleep)
	if err != nil {
		return nil, err
	}
	if err = writeChecked(wrapped, f); err != nil {
		return nil, err
	}
	out := &Loaded{Key: key, File: f, Created: created, Attempts: 1, UnwrapDuration: time.Since(start)}
	if created {
		l.log.Info("generated a new secret key for credentials at rest and stored it wrapped by the key management service",
			slog.String("path", wrapped), slog.String("key_id", f.KeyID), slog.String("fingerprint", f.Fingerprint))
		return out, nil
	}
	if err = secureRemove(plain); err != nil {
		return nil, fmt.Errorf("keywrap: secret.key was wrapped but the plaintext file could not be removed: %w", err)
	}
	out.Migrated = []string{name}
	l.log.Warn("wrapped secret.key with the key management service and removed the plaintext file; recovery now needs access to the KMS key: "+
		"download a new recovery kit", slog.String("path", wrapped), slog.String("key_id", f.KeyID), slog.String("fingerprint", f.Fingerprint))
	return out, nil
}

// migrateAside wraps a plaintext secret.key.next or secret.key.previous. A wrapped
// counterpart that already exists is only checked when a plaintext file sits next
// to it (an interrupted migration).
func (l *loader) migrateAside(ctx context.Context, name string) (bool, error) {
	plain, wrapped := l.paths(name)
	hasPlain, err := secretbox.KeyFileExists(plain)
	if err != nil || !hasPlain {
		return false, err
	}
	key, err := secretbox.ReadKeyFile(plain)
	if err != nil {
		return false, err
	}
	f, err := ReadFile(wrapped)
	switch {
	case err == nil:
		back, _, err := l.open(ctx, f, name)
		if err != nil {
			return false, err
		}
		if err = l.dropPlain(plain, back); err != nil {
			return false, err
		}
		return false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	if f, err = sealChecked(ctx, l.cfg.Wrapper, l.cfg.Spec, key, l.cfg.Now(), l.cfg.Retry, l.cfg.Sleep); err != nil {
		return false, err
	}
	if err = writeChecked(wrapped, f); err != nil {
		return false, err
	}
	if err = secureRemove(plain); err != nil {
		return false, fmt.Errorf("keywrap: %s was wrapped but the plaintext file could not be removed: %w", name, err)
	}
	l.log.Warn("wrapped a key file with the key management service and removed the plaintext file", slog.String("file", name),
		slog.String("fingerprint", f.Fingerprint))
	return true, nil
}

// dropPlain removes the plaintext key file at plain when it holds key, and
// refuses when it holds another one.
func (l *loader) dropPlain(plain string, key []byte) error {
	pk, err := secretbox.ReadKeyFile(plain)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(pk, key) != 1 {
		return fmt.Errorf("%w: %s and %s; keep the one the database uses and remove the other", ErrConflict,
			filepath.Base(plain), filepath.Base(plain)+Suffix)
	}
	return secureRemove(plain)
}

// secureRemove overwrites the file at path with zeros, syncs it and removes it.
// On copy-on-write file systems and SSDs the old blocks may survive the
// overwrite; the file is gone from the directory either way.
func secureRemove(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // G304: a key file in the configured data directory.
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("keywrap: open %s: %w", filepath.Base(path), err)
	}
	info, statErr := f.Stat()
	if statErr == nil && info.Size() > 0 {
		_, _ = f.WriteAt(make([]byte, info.Size()), 0)
		_ = f.Sync()
	}
	_ = f.Close()
	return secretbox.RemoveKeyFile(path)
}

// Codec reads and writes the wrapped key files of a key rotation
// (keyrotation.Files.Codec): a key written is wrapped, checked by unwrapping it
// and stored atomically; a key read is unwrapped under retry.
type Codec struct {
	spec  Spec
	w     Wrapper
	retry Retry
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewCodec returns the Codec of spec and w with DefaultRetry.
func NewCodec(spec Spec, w Wrapper) *Codec {
	return &Codec{spec: spec, w: w, retry: DefaultRetry, now: time.Now}
}

// WithRetry returns a copy of c with r (and sleep, which may be nil) for the KMS
// calls.
func (c *Codec) WithRetry(r Retry, sleep func(context.Context, time.Duration) error) *Codec {
	cp := *c
	cp.retry, cp.sleep = r, sleep
	return &cp
}

// Spec returns the configured key wrapping.
func (c *Codec) Spec() Spec { return c.spec }

// ReadKey unwraps the wrapped key file at path.
func (c *Codec) ReadKey(ctx context.Context, path string) ([]byte, error) {
	f, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	if f.Provider != c.spec.Provider || f.KeyURI != c.spec.KeyURI {
		return nil, fmt.Errorf("%w: %s holds %s; run \"mongorescue key rewrap\" again", ErrKeyMismatch, name, f.Spec())
	}
	key, _, err := openRetry(ctx, f, c.w, c.retry, c.sleep, name)
	return key, err
}

// WriteKey wraps key, checks the result and writes it to path atomically.
func (c *Codec) WriteKey(ctx context.Context, path string, key []byte) error {
	f, err := sealChecked(ctx, c.w, c.spec, key, c.now(), c.retry, c.sleep)
	if err != nil {
		return err
	}
	return WriteFile(path, f)
}

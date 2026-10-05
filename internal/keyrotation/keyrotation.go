// Package keyrotation rotates secret.key, the key that seals every credential in the
// metadata database (internal/secretbox), and recovers an interrupted rotation when
// the application starts.
//
// A rotation moves through these steps; the files live in the data directory:
//
//  1. a marker row (old and new key fingerprints) is stored in the database;
//  2. the new key is written to secret.key.next (temporary file, fsync, rename);
//  3. every sealed value is re-sealed under the new key in one transaction, which
//     also removes every dashboard session (synchronous=FULL, so a committed
//     rotation survives a power loss);
//  4. the old key is written to secret.key.previous;
//  5. secret.key.next is renamed to secret.key;
//  6. the marker row is removed.
//
// A crash may stop it after any step. Open, at the next start, settles it from what
// it finds: when secret.key opens the database, the rotation either completed (its
// fingerprint is the marker's new one) or never committed (it is the old one, and
// secret.key.next is discarded). When it does not, and the marker names it as the old
// key, the transaction committed before the rename: secret.key.next is installed.
// Should the commit itself have been lost while the new key was already installed,
// secret.key.previous is put back. Every other mismatch is refused, as before.
//
// Keys from MONGORESCUE_SECRET_KEY are never rotated by the application.
package keyrotation

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Sentinel errors. Their messages are safe to show to clients.
var (
	// ErrEnvKey is returned when the key comes from MONGORESCUE_SECRET_KEY.
	ErrEnvKey = errors.New("keyrotation: the secret key is set by MONGORESCUE_SECRET_KEY and cannot be rotated by MongoRescue; " +
		"follow the manual procedure in docs/encryption.md")
	// ErrBusy is returned while another rotation runs.
	ErrBusy = errors.New("keyrotation: a secret key rotation is already running")
	// ErrPending is returned when an earlier rotation did not finish; restart
	// MongoRescue to complete or roll it back.
	ErrPending = errors.New("keyrotation: an earlier secret key rotation did not finish; restart MongoRescue to settle it")
	// ErrIncomplete is returned when the database was re-sealed but the new key file
	// could not be installed; the next start installs it from secret.key.next.
	ErrIncomplete = errors.New("keyrotation: the database uses the new key but secret.key could not be replaced; " +
		"keep secret.key.next and restart MongoRescue to finish the rotation")
)

// Outcome tells what Open did about an interrupted rotation.
type Outcome string

// Outcomes of Open.
const (
	// OutcomeNone means no rotation was pending.
	OutcomeNone Outcome = ""
	// OutcomeCompleted means an interrupted rotation was completed.
	OutcomeCompleted Outcome = "completed"
	// OutcomeRolledBack means an interrupted rotation was rolled back.
	OutcomeRolledBack Outcome = "rolled_back"
)

// Files are the key files of a data directory.
type Files struct {
	// Current is secret.key, Next secret.key.next and Previous secret.key.previous.
	Current, Next, Previous string
}

// FilesIn returns the key files of dataDir.
func FilesIn(dataDir string) Files {
	return Files{
		Current:  filepath.Join(dataDir, secretbox.KeyFileName),
		Next:     filepath.Join(dataDir, secretbox.NextKeyFileName),
		Previous: filepath.Join(dataDir, secretbox.PreviousKeyFileName),
	}
}

// OpenFunc opens the metadata store with box; a key that does not match the database
// returns an error wrapping secretbox.ErrSecretKeyMismatch.
type OpenFunc func(ctx context.Context, box *secretbox.Box) (*store.SQLiteStore, error)

// Opened is the result of Open.
type Opened struct {
	// Store is the open metadata store.
	Store *store.SQLiteStore
	// Key is the key the store is sealed with (secret.key once Open returns).
	Key []byte
	// Outcome tells what happened to an interrupted rotation.
	Outcome Outcome
}

// Open opens the metadata store with key (loaded from files.Current, or from
// MONGORESCUE_SECRET_KEY when fromEnv), settling a rotation that a crash interrupted
// (see the package comment). The error of the plain open is returned when nothing
// can be settled.
func Open(ctx context.Context, files Files, key []byte, fromEnv bool, open OpenFunc, logger *slog.Logger) (*Opened, error) {
	if logger == nil {
		logger = slog.Default()
	}
	box, err := secretbox.New(key)
	if err != nil {
		return nil, err
	}
	st, openErr := open(ctx, box)
	if fromEnv {
		if openErr != nil {
			return nil, openErr
		}
		return &Opened{Store: st, Key: key}, nil
	}
	if openErr == nil {
		outcome, err := settleOpened(ctx, st, files, key, logger)
		if err != nil {
			_ = st.Close()
			return nil, err
		}
		return &Opened{Store: st, Key: key, Outcome: outcome}, nil
	}
	if !errors.Is(openErr, secretbox.ErrSecretKeyMismatch) {
		return nil, openErr
	}
	curFP, err := secretbox.Fingerprint(key)
	if err != nil {
		return nil, err
	}
	// The transaction committed before secret.key.next was installed.
	if opened, err := tryCandidate(ctx, files.Next, open, func(m *store.KeyRotation, fp string) bool {
		return m.OldFingerprint == curFP && m.NewFingerprint == fp
	}); err != nil {
		return nil, err
	} else if opened != nil {
		if err = completeInstall(ctx, opened.Store, files, key); err != nil {
			_ = opened.Store.Close()
			return nil, err
		}
		logger.Warn("completed an interrupted secret key rotation: installed secret.key.next as secret.key")
		opened.Outcome = OutcomeCompleted
		return opened, nil
	}
	// The new key was installed but the commit was lost (power loss).
	if opened, err := tryCandidate(ctx, files.Previous, open, func(m *store.KeyRotation, fp string) bool {
		return m.OldFingerprint == fp && m.NewFingerprint == curFP
	}); err != nil {
		return nil, err
	} else if opened != nil {
		if err = rollBackInstall(ctx, opened.Store, files, opened.Key); err != nil {
			_ = opened.Store.Close()
			return nil, err
		}
		logger.Warn("rolled back an interrupted secret key rotation: the database still uses the previous key, which is secret.key again")
		opened.Outcome = OutcomeRolledBack
		return opened, nil
	}
	return nil, openErr
}

// settleOpened cleans up after a rotation when secret.key opened the database.
func settleOpened(ctx context.Context, st *store.SQLiteStore, files Files, key []byte, logger *slog.Logger) (Outcome, error) {
	m, err := st.PendingKeyRotation(ctx)
	if err != nil {
		return OutcomeNone, err
	}
	if m == nil {
		// A next key without a marker belongs to no rotation (the marker is written
		// first and removed last).
		return OutcomeNone, secretbox.RemoveKeyFile(files.Next)
	}
	fp, err := secretbox.Fingerprint(key)
	if err != nil {
		return OutcomeNone, err
	}
	if err = secretbox.RemoveKeyFile(files.Next); err != nil {
		return OutcomeNone, err
	}
	if err = st.EndKeyRotation(ctx); err != nil {
		return OutcomeNone, err
	}
	if fp == m.NewFingerprint {
		logger.Info("completed an interrupted secret key rotation")
		return OutcomeCompleted, nil
	}
	logger.Warn("rolled back an interrupted secret key rotation: the database still uses secret.key")
	return OutcomeRolledBack, nil
}

// tryCandidate opens the store with the key file at path, when it exists, and keeps
// it only when the rotation marker matches (match gets the marker and the key's
// fingerprint). It returns nil when the candidate does not apply.
func tryCandidate(ctx context.Context, path string, open OpenFunc, match func(*store.KeyRotation, string) bool) (*Opened, error) {
	ok, err := secretbox.KeyFileExists(path)
	if err != nil || !ok {
		return nil, err
	}
	key, err := secretbox.ReadKeyFile(path)
	if err != nil {
		return nil, err
	}
	box, err := secretbox.New(key)
	if err != nil {
		return nil, err
	}
	st, err := open(ctx, box)
	if errors.Is(err, secretbox.ErrSecretKeyMismatch) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m, err := st.PendingKeyRotation(ctx)
	fp, fpErr := secretbox.Fingerprint(key)
	if err != nil || fpErr != nil || m == nil || !match(m, fp) {
		_ = st.Close()
		return nil, errors.Join(err, fpErr)
	}
	return &Opened{Store: st, Key: key}, nil
}

// completeInstall finishes a rotation whose transaction committed: the old key goes
// to secret.key.previous, secret.key.next becomes secret.key and the marker goes.
func completeInstall(ctx context.Context, st *store.SQLiteStore, files Files, old []byte) error {
	if err := secretbox.WriteKeyFile(files.Previous, old); err != nil {
		return err
	}
	if err := secretbox.InstallKeyFile(files.Next, files.Current); err != nil {
		return err
	}
	return st.EndKeyRotation(ctx)
}

// rollBackInstall undoes a rotation whose commit was lost: the previous key becomes
// secret.key again and the marker goes.
func rollBackInstall(ctx context.Context, st *store.SQLiteStore, files Files, previous []byte) error {
	if err := secretbox.WriteKeyFile(files.Current, previous); err != nil {
		return err
	}
	if err := secretbox.RemoveKeyFile(files.Next); err != nil {
		return err
	}
	if err := secretbox.RemoveKeyFile(files.Previous); err != nil {
		return err
	}
	return st.EndKeyRotation(ctx)
}

// Step names a point of a rotation, for fault injection in tests.
type Step string

// Steps of a rotation, in order (see the package comment).
const (
	StepMarked       Step = "marked"
	StepNextWritten  Step = "next_written"
	StepBeforeCommit Step = "before_commit"
	StepCommitted    Step = "committed"
	StepPreviousKept Step = "previous_kept"
	StepInstalled    Step = "installed"
)

// Result describes a finished rotation. It never carries key material.
type Result struct {
	// OldFingerprint and NewFingerprint identify the keys (secretbox.Fingerprint).
	OldFingerprint string `json:"old_fingerprint"`
	NewFingerprint string `json:"new_fingerprint"`
	// Resealed counts the values sealed under the new key.
	Resealed int `json:"resealed"`
	// SessionsRevoked counts the dashboard sessions that were signed out.
	SessionsRevoked int `json:"sessions_revoked"`
	// RotatedAt is when the rotation finished.
	RotatedAt time.Time `json:"rotated_at"`
}

// ApplyFunc hands the new key (and the one it replaced) to the components that hold
// secret.key or one of its subkeys in memory. It runs once the database committed.
type ApplyFunc func(next, previous []byte)

// Config holds the dependencies of a Rotator.
type Config struct {
	// Files are the key files.
	Files Files
	// Store is the metadata store.
	Store *store.SQLiteStore
	// Key is the key the store is sealed with now.
	Key []byte
	// FromEnv reports a key from MONGORESCUE_SECRET_KEY (rotation refused).
	FromEnv bool
	// RetiredMAC derives, from a key that is being replaced, the key that keeps
	// API keys imported under it verifying (see store.SecretKeyRotation).
	RetiredMAC func(old []byte) ([]byte, error)
	// Apply propagates a new key (see ApplyFunc); may be nil.
	Apply ApplyFunc
	// Logger logs the rotation (fingerprints only).
	Logger *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Fault, when set, is called after every Step; a non-nil error stops the
	// rotation at once, as a crash would (fault injection in tests).
	Fault func(Step) error
}

// Rotator rotates secret.key. It is safe for concurrent use; one rotation runs at a
// time.
type Rotator struct {
	cfg Config
	run sync.Mutex
	mu  sync.Mutex
	key []byte
}

// New returns a Rotator.
func New(cfg Config) *Rotator {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Rotator{cfg: cfg, key: cfg.Key}
}

// FromEnv reports whether the key comes from MONGORESCUE_SECRET_KEY.
func (r *Rotator) FromEnv() bool { return r.cfg.FromEnv }

// Fingerprint returns the fingerprint of the current key.
func (r *Rotator) Fingerprint() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	fp, _ := secretbox.Fingerprint(r.key)
	return fp
}

// crashError is an injected crash (see Config.Fault).
type crashError struct{ err error }

func (e *crashError) Error() string { return e.err.Error() }
func (e *crashError) Unwrap() error { return e.err }

// fault reports the injected crash at step, if any.
func (r *Rotator) fault(step Step) error {
	if r.cfg.Fault == nil {
		return nil
	}
	if err := r.cfg.Fault(step); err != nil {
		return &crashError{err: err}
	}
	return nil
}

// Rotate generates a new key, re-seals the database under it and installs it as
// secret.key (see the package comment). Everyone has to sign in again afterwards.
func (r *Rotator) Rotate(ctx context.Context) (*Result, error) {
	if r.cfg.FromEnv {
		return nil, ErrEnvKey
	}
	if !r.run.TryLock() {
		return nil, ErrBusy
	}
	defer r.run.Unlock()
	r.mu.Lock()
	old := r.key
	r.mu.Unlock()

	next, err := secretbox.GenerateKey()
	if err != nil {
		return nil, err
	}
	nextBox, err := secretbox.New(next)
	if err != nil {
		return nil, err
	}
	oldFP, err := secretbox.Fingerprint(old)
	if err != nil {
		return nil, err
	}
	newFP, err := secretbox.Fingerprint(next)
	if err != nil {
		return nil, err
	}
	var retiredMAC []byte
	if r.cfg.RetiredMAC != nil {
		if retiredMAC, err = r.cfg.RetiredMAC(old); err != nil {
			return nil, err
		}
	}
	log := r.cfg.Logger.With(slog.String("old_fingerprint", oldFP), slog.String("new_fingerprint", newFP))

	if err = r.cfg.Store.BeginKeyRotation(ctx, store.KeyRotation{OldFingerprint: oldFP, NewFingerprint: newFP, StartedAt: r.cfg.Now().UTC()}); err != nil {
		if errors.Is(err, store.ErrKeyRotationPending) {
			return nil, ErrPending
		}
		return nil, err
	}
	if err = r.fault(StepMarked); err != nil {
		return nil, err
	}
	if err = secretbox.WriteKeyFile(r.cfg.Files.Next, next); err != nil {
		r.abort(ctx, log)
		return nil, err
	}
	if err = r.fault(StepNextWritten); err != nil {
		return nil, err
	}
	res, err := r.cfg.Store.RotateSecretBox(ctx, store.SecretKeyRotation{
		Next: nextBox, RetiredImportedKeyMAC: retiredMAC,
		BeforeCommit: func() error { return r.fault(StepBeforeCommit) },
	})
	if err != nil {
		if crash := (*crashError)(nil); !errors.As(err, &crash) {
			r.abort(ctx, log)
		}
		return nil, err
	}
	// The database is sealed with the new key from here on: the components that hold
	// the key follow it whatever happens to the files.
	r.mu.Lock()
	r.key = next
	r.mu.Unlock()
	if r.cfg.Apply != nil {
		r.cfg.Apply(next, old)
	}
	if err = r.fault(StepCommitted); err != nil {
		return nil, err
	}
	if err = secretbox.WriteKeyFile(r.cfg.Files.Previous, old); err != nil {
		log.Error("secret key rotation: could not keep the previous key", logsafe.Error(err))
		return nil, ErrIncomplete
	}
	if err = r.fault(StepPreviousKept); err != nil {
		return nil, err
	}
	if err = secretbox.InstallKeyFile(r.cfg.Files.Next, r.cfg.Files.Current); err != nil {
		log.Error("secret key rotation: could not install the new key file", logsafe.Error(err))
		return nil, ErrIncomplete
	}
	if err = r.fault(StepInstalled); err != nil {
		return nil, err
	}
	if err = r.cfg.Store.EndKeyRotation(ctx); err != nil {
		// The next start clears the marker (secret.key matches its new fingerprint).
		log.Warn("secret key rotation: could not clear the rotation marker", logsafe.Error(err))
	}
	log.Info("secret key rotated", slog.Int("resealed", res.Resealed), slog.Int("sessions_revoked", res.SessionsRevoked))
	return &Result{OldFingerprint: oldFP, NewFingerprint: newFP, Resealed: res.Resealed,
		SessionsRevoked: res.SessionsRevoked, RotatedAt: r.cfg.Now().UTC()}, nil
}

// abort undoes a rotation that never committed: the next key file and the marker go.
func (r *Rotator) abort(ctx context.Context, log *slog.Logger) {
	ctx = context.WithoutCancel(ctx)
	if err := secretbox.RemoveKeyFile(r.cfg.Files.Next); err != nil {
		log.Warn("secret key rotation: could not remove secret.key.next", logsafe.Error(err))
	}
	if err := r.cfg.Store.EndKeyRotation(ctx); err != nil {
		log.Warn("secret key rotation: could not clear the rotation marker", logsafe.Error(err))
	}
}

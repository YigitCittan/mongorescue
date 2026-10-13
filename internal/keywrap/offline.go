package keywrap

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// OpenFunc returns the Wrapper of a spec (internal/keywrap/provider.Open).
type OpenFunc func(ctx context.Context, spec Spec) (Wrapper, error)

// OfflineConfig holds the parameters of UnwrapToFiles and Rewrap. Both run while
// MongoRescue is stopped (the caller holds the data directory lock).
type OfflineConfig struct {
	// DataDir is the data directory.
	DataDir string
	// Open returns the wrapper of a spec (required).
	Open OpenFunc
	// Actor is who runs the command, recorded in the journal.
	Actor string
	// Retry bounds the KMS attempts; the zero value means DefaultRetry.
	Retry Retry
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// normalized fills the defaults of c.
func (c OfflineConfig) normalized() (OfflineConfig, error) {
	if c.DataDir == "" || c.Open == nil {
		return c, errors.New("keywrap: a data directory and an opener are required")
	}
	if c.Retry == (Retry{}) {
		c.Retry = DefaultRetry
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Actor == "" {
		c.Actor = ActorSystem
	}
	return c, nil
}

// offlineFile is one wrapped key file of a data directory.
type offlineFile struct {
	name, plain, wrapped string
	file                 *File
}

// wrappedFiles returns the wrapped key files of dataDir with secret.key last, so
// that an interrupted command leaves secret.key.wrapped (what the next start reads
// first) in its old state until everything else is done.
func wrappedFiles(dataDir string) ([]offlineFile, error) {
	var out []offlineFile
	for _, name := range []string{secretbox.PreviousKeyFileName, secretbox.NextKeyFileName, secretbox.KeyFileName} {
		plain := filepath.Join(dataDir, name)
		f, err := ReadFile(plain + Suffix)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, offlineFile{name: name, plain: plain, wrapped: plain + Suffix, file: f})
	}
	if len(out) == 0 || out[len(out)-1].name != secretbox.KeyFileName {
		return nil, ErrNotWrapped
	}
	return out, nil
}

// wrappers caches the wrapper of each spec.
type wrappers struct {
	open OpenFunc
	m    map[Spec]Wrapper
}

func (w *wrappers) get(ctx context.Context, spec Spec) (Wrapper, error) {
	if wr, ok := w.m[spec]; ok {
		return wr, nil
	}
	wr, err := w.open(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("keywrap: open %s: %w", spec, err)
	}
	if w.m == nil {
		w.m = map[Spec]Wrapper{}
	}
	w.m[spec] = wr
	return wr, nil
}

// UnwrapToFiles leaves the KMS: every wrapped key file of the data directory is
// unwrapped, written back as a plaintext key file (atomically, mode 0600), read
// back and only then removed. It journals ActionUnwrappedToFile and returns the
// change. A plaintext file that already holds the same key is kept (a rerun after
// an interruption); one that holds another key stops the command (ErrConflict).
func UnwrapToFiles(ctx context.Context, cfg OfflineConfig) (*Change, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	files, err := wrappedFiles(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	ws := &wrappers{open: cfg.Open}
	change := &Change{Action: ActionUnwrappedToFile, At: cfg.Now().UTC(), Actor: cfg.Actor}
	for _, of := range files {
		w, err := ws.get(ctx, of.file.Spec())
		if err != nil {
			return nil, err
		}
		key, _, err := openRetry(ctx, of.file, w, cfg.Retry, nil, of.name+Suffix)
		if err != nil {
			return nil, err
		}
		existing, err := secretbox.ReadKeyFile(of.plain)
		switch {
		case err == nil:
			if subtle.ConstantTimeCompare(existing, key) != 1 {
				return nil, fmt.Errorf("%w: %s and %s; remove the one the database does not use", ErrConflict, of.name, of.name+Suffix)
			}
		case errors.Is(err, fs.ErrNotExist):
			if err = secretbox.WriteKeyFile(of.plain, key); err != nil {
				return nil, err
			}
			back, readErr := secretbox.ReadKeyFile(of.plain)
			if readErr != nil {
				return nil, readErr
			}
			if subtle.ConstantTimeCompare(back, key) != 1 {
				return nil, fmt.Errorf("keywrap: %s does not read back as written", of.name)
			}
		default:
			return nil, err
		}
		if err = secretbox.RemoveKeyFile(of.wrapped); err != nil {
			return nil, err
		}
		change.Files = append(change.Files, of.name)
		if of.name == secretbox.KeyFileName {
			change.PreviousProvider, change.PreviousKeyURI, change.Fingerprint = of.file.Provider, of.file.KeyURI, of.file.Fingerprint
		}
	}
	if err := AppendJournal(cfg.DataDir, *change); err != nil {
		return change, err
	}
	return change, nil
}

// Rewrap moves every wrapped key file of the data directory to the KMS key of to:
// each key is unwrapped with the KMS key it was wrapped with, wrapped with the new
// one, checked by unwrapping it again and written atomically over the old file.
// Files already wrapped with to are left alone, so a rerun after an interruption
// finishes the job. It journals ActionRewrapped and returns the change. The old
// KMS key must stay available until no backup of the data directory (and no
// recovery kit) still holds a key wrapped with it.
func Rewrap(ctx context.Context, cfg OfflineConfig, to Spec) (*Change, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	if !to.Enabled() {
		return nil, ErrInvalidSpec
	}
	files, err := wrappedFiles(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	ws := &wrappers{open: cfg.Open}
	next, err := ws.get(ctx, to)
	if err != nil {
		return nil, err
	}
	change := &Change{Action: ActionRewrapped, At: cfg.Now().UTC(), Actor: cfg.Actor, Provider: to.Provider, KeyURI: to.KeyURI, KeyID: next.KeyID()}
	for _, of := range files {
		if of.name == secretbox.KeyFileName {
			change.PreviousProvider, change.PreviousKeyURI, change.Fingerprint = of.file.Provider, of.file.KeyURI, of.file.Fingerprint
		}
		if of.file.Spec() == to {
			continue
		}
		old, err := ws.get(ctx, of.file.Spec())
		if err != nil {
			return nil, err
		}
		key, _, err := openRetry(ctx, of.file, old, cfg.Retry, nil, of.name+Suffix)
		if err != nil {
			return nil, err
		}
		f, err := sealChecked(ctx, next, to, key, cfg.Now(), cfg.Retry, nil)
		if err != nil {
			return nil, err
		}
		if err = writeChecked(of.wrapped, f); err != nil {
			return nil, err
		}
		change.Files = append(change.Files, of.name)
	}
	if len(change.Files) == 0 {
		return change, nil
	}
	if err := AppendJournal(cfg.DataDir, *change); err != nil {
		return change, err
	}
	return change, nil
}

// Status describes the key wrapping of a running instance (readiness, metrics).
type Status struct {
	// Active reports that secret.key is wrapped by a KMS.
	Active bool `json:"active"`
	// Provider names the KMS ("" when inactive).
	Provider string `json:"provider,omitempty"`
	// KeyID identifies the KMS key ("" when inactive).
	KeyID string `json:"key_id,omitempty"`
}

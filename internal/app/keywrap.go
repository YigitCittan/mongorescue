package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/keywrap/provider"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/recoverykit"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// WithKeyWrapper replaces how the KMS wrapper of MONGORESCUE_KEY_WRAP is opened
// (tests: a fake KMS). The default is provider.Open.
func WithKeyWrapper(open keywrap.OpenFunc) Option {
	return func(o *options) { o.openWrapper = open }
}

// withKeyWrapRetry replaces keywrap.DefaultRetry and its sleep (tests).
func withKeyWrapRetry(r keywrap.Retry, sleep func(context.Context, time.Duration) error) Option {
	return func(o *options) { o.wrapRetry, o.wrapSleep = r, sleep }
}

// keyWrapping is the KMS wrapping of secret.key of an App (nil when off).
type keyWrapping struct {
	spec    keywrap.Spec
	wrapper *keywrap.Instrumented
	codec   *keywrap.Codec
	keyID   string
}

// status returns the wrapping for the readiness report.
func (w *keyWrapping) status() readiness.KeyWrapping {
	if w == nil {
		return readiness.KeyWrapping{}
	}
	return readiness.KeyWrapping{Active: true, Provider: w.spec.Provider}
}

// kit returns the wrapping for the recovery kit.
func (w *keyWrapping) kit(dataDir string) *recoverykit.KeyWrap {
	if w == nil {
		return nil
	}
	return &recoverykit.KeyWrap{Spec: w.spec, KeyID: w.keyID, CurrentFile: filepath.Join(dataDir, keywrap.WrappedKeyFileName),
		ReadKey: w.codec.ReadKey}
}

// secretKey is the secret key an App opens its database with.
type secretKey struct {
	*secretbox.LoadedKey
	// files are the key files of a rotation (wrapped with key wrapping on).
	files keyrotation.Files
	// path is the key file a key Created by this start lives in.
	path string
	// wrap is the KMS wrapping (nil when off).
	wrap *keyWrapping
}

// wrappedKeyCache keeps the unwrapped key of an HA member across the Apps of its
// roles, so that a takeover does not depend on the KMS being reachable (secret
// key rotation is refused in HA mode, so the key does not change while it runs).
type wrappedKeyCache struct {
	mu  sync.Mutex
	key *secretKey
}

// loadSecretKey returns the secret key of cfg: MONGORESCUE_SECRET_KEY (envKey),
// the plaintext secret.key, or with MONGORESCUE_KEY_WRAP the key unwrapped from
// secret.key.wrapped by the KMS (keywrap.Load, which wraps a plaintext key first).
func loadSecretKey(ctx context.Context, cfg *config.Config, o *options, envKey string, logger *slog.Logger) (*secretKey, error) {
	keyFile := filepath.Join(cfg.DataDir, secretbox.KeyFileName)
	if !cfg.KeyWrap.Enabled() {
		if envKey == "" {
			if ok, err := secretbox.KeyFileExists(keyFile + keywrap.Suffix); err != nil {
				return nil, err
			} else if ok {
				return nil, keywrap.ErrWrappedKeyNeedsConfig
			}
		}
		key, err := secretbox.LoadKey(secretbox.KeySource{Env: envKey, File: keyFile}, logger)
		if err != nil {
			return nil, err
		}
		return &secretKey{LoadedKey: key, files: keyrotation.FilesIn(cfg.DataDir), path: keyFile}, nil
	}
	if envKey != "" {
		return nil, config.ErrKeyWrapWithSecretKey
	}
	var cache *wrappedKeyCache
	if o.ha != nil {
		cache = &o.ha.rt.wrapped
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if k := cache.key; k != nil && k.wrap.spec == cfg.KeyWrap {
			out := *k
			out.LoadedKey = &secretbox.LoadedKey{Key: slices.Clone(k.Key)}
			return &out, nil
		}
	}
	open := o.openWrapper
	if open == nil {
		open = provider.Open
	}
	w, err := open(ctx, cfg.KeyWrap)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.EnvKeyWrap, err)
	}
	inst := keywrap.Instrument(w)
	retry := o.wrapRetry
	if retry == (keywrap.Retry{}) {
		retry = keywrap.DefaultRetry
	}
	loaded, err := keywrap.Load(ctx, keywrap.LoadConfig{DataDir: cfg.DataDir, Spec: cfg.KeyWrap, Wrapper: inst, Retry: retry,
		Sleep: o.wrapSleep, Logger: logger})
	if err != nil {
		return nil, err
	}
	wrap := &keyWrapping{spec: cfg.KeyWrap, wrapper: inst, codec: keywrap.NewCodec(cfg.KeyWrap, inst).WithRetry(retry, o.wrapSleep),
		keyID: loaded.File.KeyID}
	out := &secretKey{LoadedKey: &secretbox.LoadedKey{Key: loaded.Key, Created: loaded.Created},
		files: keyrotation.WrappedFilesIn(cfg.DataDir, wrap.codec), path: keyFile + keywrap.Suffix, wrap: wrap}
	if cache != nil {
		cached := *out
		cached.LoadedKey = &secretbox.LoadedKey{Key: slices.Clone(loaded.Key)}
		cache.key = &cached
	}
	return out, nil
}

// removeCreated removes the key file a start created when the database turned
// out to need another key.
func (k *secretKey) removeCreated() {
	if k != nil && k.Created && k.path != "" {
		_ = os.Remove(k.path)
	}
}

// publishKeyWrapJournal publishes the journaled changes of the key wrapping
// (security.key_wrapping_changed, an audit entry each) and clears the journal.
func publishKeyWrapJournal(ctx context.Context, dataDir string, ops *operations.Service, logger *slog.Logger) {
	changes, err := keywrap.ReadJournal(dataDir)
	if err != nil {
		logger.Warn("could not read the key wrapping journal", logsafe.Error(err))
	}
	if len(changes) == 0 {
		return
	}
	for _, c := range changes {
		ops.KeyWrappingChanged(ctx, c)
	}
	if err = keywrap.ClearJournal(dataDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warn("could not clear the key wrapping journal; its events may be sent again", logsafe.Error(err))
	}
}

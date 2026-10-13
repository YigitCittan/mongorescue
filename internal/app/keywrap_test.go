package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/ha"
	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/keywrap/keywraptest"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/store"
)

var appWrapSpec = keywrap.Spec{Provider: keywrap.ProviderAWSKMS, KeyURI: "arn:aws:kms:eu-west-1:123456789012:alias/mongorescue"}

// wrapOpts returns the options of an App whose KMS is kms.
func wrapOpts(kms *keywraptest.Fake) []Option {
	return []Option{WithGetenv(noEnv), WithKeyWrapper(keywraptest.Opener(map[string]*keywraptest.Fake{appWrapSpec.KeyURI: kms})),
		withKeyWrapRetry(keywrap.Retry{Attempts: 2, Initial: time.Millisecond}, keywraptest.NoSleep)}
}

func wrappedConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.KeyWrap = appWrapSpec
	return cfg
}

func TestAppStoresItsKeyWrappedByTheKMS(t *testing.T) {
	cfg := wrappedConfig(t)
	kms := keywraptest.New("kms")
	app, err := New(cfg, nil, wrapOpts(kms)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(cfg.DataDir, secretbox.KeyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a plaintext secret.key was written")
	}
	f, err := keywrap.ReadFile(filepath.Join(cfg.DataDir, keywrap.WrappedKeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if f.Provider != appWrapSpec.Provider || f.KeyURI != appWrapSpec.KeyURI {
		t.Fatalf("wrapped file = %+v", f)
	}
	report, err := app.readiness.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.KeyWrapping == nil || !report.KeyWrapping.Active || report.KeyWrapping.Provider != keywrap.ProviderAWSKMS {
		t.Fatalf("readiness key_wrapping = %+v", report.KeyWrapping)
	}
	if n := testutil.CollectAndCount(app.metrics.Registry(), "mongorescue_key_wrap_duration_seconds"); n == 0 {
		t.Error("the KMS calls of the start were not recorded")
	}
	if err = app.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := New(cfg, nil, wrapOpts(kms)...)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	_ = again.Close()

	// Without the configuration the wrapped key is refused, not replaced.
	cfg.KeyWrap = keywrap.Spec{}
	if _, err = New(cfg, nil, WithGetenv(noEnv)); !errors.Is(err, keywrap.ErrWrappedKeyNeedsConfig) {
		t.Fatalf("New without MONGORESCUE_KEY_WRAP = %v; want ErrWrappedKeyNeedsConfig", err)
	}
	if _, err = os.Stat(filepath.Join(cfg.DataDir, secretbox.KeyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a new plaintext key was generated next to the wrapped one")
	}
}

func TestAppMigratesItsPlaintextKeyToTheKMS(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	plain, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	if err = plain.metaStore.(*store.SQLiteStore).SaveConnection(ctx, &models.Connection{ID: "conn_1", Name: "prod", URI: "mongodb://u:conn-secret@db:27017/",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_ = plain.Close()
	keyFile := filepath.Join(cfg.DataDir, secretbox.KeyFileName)
	key, err := secretbox.ReadKeyFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}

	cfg.KeyWrap = appWrapSpec
	kms := keywraptest.New("kms")
	wrapped, err := New(cfg, nil, wrapOpts(kms)...)
	if err != nil {
		t.Fatal(err)
	}
	defer wrapped.Close()
	if _, err = os.Stat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the plaintext secret.key was kept")
	}
	if c, getErr := wrapped.metaStore.(*store.SQLiteStore).GetConnection(ctx, "conn_1"); getErr != nil || !strings.Contains(c.URI, "conn-secret") {
		t.Fatalf("the credentials do not open after the migration: %v", getErr)
	}
	f, err := keywrap.ReadFile(keyFile + keywrap.Suffix)
	if err != nil {
		t.Fatal(err)
	}
	if fp, _ := secretbox.Fingerprint(key); f.Fingerprint != fp {
		t.Fatal("the wrapped key is not the key of the database")
	}
	// The migration was published (security event and audit entry) and the journal
	// cleared.
	if changes, _ := keywrap.ReadJournal(cfg.DataDir); len(changes) != 0 {
		t.Fatalf("journal after the start = %+v", changes)
	}
}

func TestAppFailsFastWhenTheKMSIsDown(t *testing.T) {
	cfg := wrappedConfig(t)
	kms := keywraptest.New("kms")
	app, err := New(cfg, nil, wrapOpts(kms)...)
	if err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	kms.SetDown(-1)
	_, err = New(cfg, nil, wrapOpts(kms)...)
	if !errors.Is(err, keywrap.ErrUnavailable) || !strings.Contains(err.Error(), "never falls back") {
		t.Fatalf("New with the KMS down = %v; want ErrUnavailable", err)
	}
	if _, err = os.Stat(filepath.Join(cfg.DataDir, secretbox.KeyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a plaintext key appeared")
	}

	// Another KMS key is refused at once.
	_, err = New(cfg, nil, wrapOpts(keywraptest.New("other"))...)
	if !errors.Is(err, keywrap.ErrUnwrap) {
		t.Fatalf("New with another KMS key = %v; want ErrUnwrap", err)
	}
}

func TestAppRefusesKeyWrapWithALegacySecretKey(t *testing.T) {
	cfg := wrappedConfig(t)
	key, _ := secretbox.GenerateKey()
	env := envMap(map[string]string{"MONGORESCUE_SECRET_KEY": secretbox.EncodeKey(key)})
	o := &options{getenv: env}
	if _, err := loadSecretKey(context.Background(), cfg, o, secretbox.EncodeKey(key), slog.New(slog.DiscardHandler)); !errors.Is(err, config.ErrKeyWrapWithSecretKey) {
		t.Fatalf("loadSecretKey with an env key and key wrapping = %v", err)
	}
}

func TestHASharedKeyCheckAcceptsAWrappedKey(t *testing.T) {
	cfg := testConfig(t)
	if err := checkHASharedKey(cfg); !errors.Is(err, ErrHANeedsSharedKey) {
		t.Fatalf("no key = %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, keywrap.WrappedKeyFileName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkHASharedKey(cfg); !errors.Is(err, ErrHANeedsSharedKey) {
		t.Fatalf("a wrapped key without key wrapping = %v", err)
	}
	cfg.KeyWrap = appWrapSpec
	if err := checkHASharedKey(cfg); err != nil {
		t.Fatalf("a wrapped key with key wrapping = %v", err)
	}
}

// TestHAMembersShareAWrappedKey checks that members whose data directories hold
// the same secret.key.wrapped pass the shared-key check of the lease, that a
// member with another wrapped key fails it, and that a member reopening in
// another role does not need the KMS again.
func TestHAMembersShareAWrappedKey(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	ctx := context.Background()
	shared := t.TempDir()
	kms := keywraptest.New("kms")
	member := func() (*config.Config, *haSupervisor) {
		cfg := haTestConfig(t, shared, "")
		cfg.KeyWrap = appWrapSpec
		sup := newTestSupervisor(t, cfg)
		sup.opts = append(sup.opts, wrapOpts(kms)...)
		return cfg, sup
	}
	cfgA, supA := member()
	appA, err := supA.open(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = supA.bootstrap(ctx, appA); err != nil {
		t.Fatal(err)
	}
	_ = appA.Close()
	supA.rt.mgr().Step(ctx)
	if supA.rt.mgr().Role() != ha.RoleActive {
		t.Fatal("the first member did not acquire the lease")
	}
	wrapped, err := os.ReadFile(filepath.Join(cfgA.DataDir, keywrap.WrappedKeyFileName))
	if err != nil {
		t.Fatal(err)
	}

	cfgB, supB := member()
	if err = os.MkdirAll(cfgB.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(cfgB.DataDir, keywrap.WrappedKeyFileName), wrapped, 0o600); err != nil {
		t.Fatal(err)
	}
	appB, err := supB.open(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = supB.bootstrap(ctx, appB); err != nil {
		t.Fatalf("bootstrap with the same wrapped key = %v", err)
	}
	if !bytes.Equal(appB.haKey, supA.rt.wrapped.key.Key) {
		t.Fatal("the members do not share the unwrapped key")
	}
	_ = appB.Close()
	// The next role opens from the unwrapped key kept in memory.
	kms.SetDown(-1)
	appB, err = supB.open(false)
	if err != nil {
		t.Fatalf("reopen with the KMS down = %v", err)
	}
	_ = appB.Close()
	kms.SetDown(0)

	_, supC := member() // a data directory of its own: a new key is generated and wrapped
	appC, err := supC.open(false)
	if err != nil {
		t.Fatal(err)
	}
	defer appC.Close()
	if err = supC.bootstrap(ctx, appC); !errors.Is(err, ha.ErrKeyMismatch) {
		t.Fatalf("bootstrap with another wrapped key = %v; want ErrKeyMismatch", err)
	}
}

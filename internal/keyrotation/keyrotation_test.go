package keyrotation_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Plaintext of every seeded secret.
const (
	connURI       = "mongodb://backup:conn-password@db.internal:27017/"
	botToken      = "123:bot-token"
	hookHeader    = "Bearer header-token"
	hookURL       = "https://hooks.example.com/T0KEN"
	s3Secret      = "s3-secret-access-key"
	heartbeatURL  = "https://hc.example.com/ping/abc"
	passphraseRaw = `"a long backup passphrase"`
)

var errCrash = errors.New("injected crash")

type env struct {
	dir   string
	files keyrotation.Files
	db    string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	return &env{dir: dir, files: keyrotation.FilesIn(dir), db: filepath.Join(dir, "mongorescue.db")}
}

func (e *env) open(ctx context.Context, box *secretbox.Box) (*store.SQLiteStore, error) {
	return store.OpenSQLite(ctx, e.db, slog.New(slog.DiscardHandler), store.WithSecretBox(box))
}

// start loads secret.key (creating it) and opens the store through keyrotation.Open.
func (e *env) start(t *testing.T) *keyrotation.Opened {
	t.Helper()
	loaded, err := secretbox.LoadKey(secretbox.KeySource{File: e.files.Current}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := keyrotation.Open(context.Background(), e.files, loaded.Key, false, e.open, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = opened.Store.Close() })
	return opened
}

// seed stores one secret of every kind, a user with a session and an imported API key.
func seed(t *testing.T, st *store.SQLiteStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.SaveConnection(ctx, &models.Connection{ID: "conn_1", Name: "prod", URI: connURI, CreatedAt: now, UpdatedAt: now}))
	must(st.SaveChannel(ctx, &notify.Channel{ID: "ch_1", Name: "tg", Type: notify.ChannelTelegram,
		Telegram: &notify.TelegramConfig{BotToken: botToken, ChatID: "42"}}))
	must(st.SaveChannel(ctx, &notify.Channel{ID: "ch_2", Name: "hook", Type: notify.ChannelWebhook,
		Webhook: &notify.WebhookConfig{URL: hookURL, Headers: map[string]string{"Authorization": hookHeader}}}))
	must(st.CreateStorageTarget(ctx, &models.StorageTarget{ID: "tgt_s3", Name: "Offsite", Type: models.StorageS3,
		S3: &models.S3Target{Bucket: "b", AccessKeyID: "AKIA", SecretAccessKey: s3Secret}, CreatedAt: now, UpdatedAt: now}))
	must(st.SaveJob(ctx, &models.Job{ID: "job_1", Name: "nightly", Database: "app", HeartbeatURL: heartbeatURL}))
	must(st.SaveSettings(ctx, map[string]string{settings.KeyEncryptionPassphrase: passphraseRaw}))
	must(st.CreateUser(ctx, &auth.User{ID: "usr_1", Username: "alice", Role: auth.RoleAdmin, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}))
	must(st.CreateSession(ctx, &auth.Session{TokenHash: "tok", UserID: "usr_1", CSRFToken: "c", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}))
	must(st.CreateAPIKey(ctx, &auth.APIKey{ID: "key_imp", Name: "imported", Prefix: "Kabcdef0", Scope: auth.ScopeAdmin,
		Hash: "hmac-sha256:00", CreatedAt: now}))
}

// checkSecrets reads every seeded secret back.
func checkSecrets(t *testing.T, st *store.SQLiteStore) {
	t.Helper()
	ctx := context.Background()
	c, err := st.GetConnection(ctx, "conn_1")
	if err != nil || c.URI != connURI {
		t.Fatalf("connection = %v, %v", c, err)
	}
	ch, err := st.GetChannel(ctx, "ch_1")
	if err != nil || ch.Telegram.BotToken != botToken {
		t.Fatalf("telegram channel = %v", err)
	}
	ch, err = st.GetChannel(ctx, "ch_2")
	if err != nil || ch.Webhook.URL != hookURL || ch.Webhook.Headers["Authorization"] != hookHeader {
		t.Fatalf("webhook channel = %v", err)
	}
	tgt, err := st.GetStorageTarget(ctx, "tgt_s3")
	if err != nil || tgt.S3.SecretAccessKey != s3Secret {
		t.Fatalf("target = %v", err)
	}
	job, err := st.GetJob(ctx, "job_1")
	if err != nil || job.HeartbeatURL != heartbeatURL {
		t.Fatalf("job = %v, %v", job, err)
	}
	vals, err := st.LoadSettings(ctx)
	if err != nil || vals[settings.KeyEncryptionPassphrase] != passphraseRaw {
		t.Fatalf("settings = %v", err)
	}
}

func readKey(t *testing.T, path string) []byte {
	t.Helper()
	k, err := secretbox.ReadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	ok, err := secretbox.KeyFileExists(path)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func rotator(e *env, opened *keyrotation.Opened, fault func(keyrotation.Step) error, apply keyrotation.ApplyFunc) *keyrotation.Rotator {
	return keyrotation.New(keyrotation.Config{
		Files: e.files, Store: opened.Store, Key: opened.Key, Fault: fault, Apply: apply,
		RetiredMAC: func(old []byte) ([]byte, error) { return secretbox.DeriveSubkey(old, auth.ImportedKeySubkeyPurpose) },
		Logger:     slog.New(slog.DiscardHandler),
	})
}

func TestRotateResealsEverythingAndRefusesTheOldKey(t *testing.T) {
	e := newEnv(t)
	opened := e.start(t)
	seed(t, opened.Store)
	oldKey := opened.Key
	var applied []byte
	res, err := rotator(e, opened, nil, func(next, _ []byte) { applied = next }).Rotate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newKey := readKey(t, e.files.Current)
	if bytes.Equal(newKey, oldKey) || !bytes.Equal(applied, newKey) {
		t.Fatal("secret.key was not replaced by the applied new key")
	}
	if !bytes.Equal(readKey(t, e.files.Previous), oldKey) || exists(t, e.files.Next) {
		t.Fatal("want secret.key.previous = old key and no secret.key.next")
	}
	if res.SessionsRevoked != 1 || res.Resealed < 7 {
		t.Fatalf("result = %+v", res)
	}
	oldFP, _ := secretbox.Fingerprint(oldKey)
	newFP, _ := secretbox.Fingerprint(newKey)
	if res.OldFingerprint != oldFP || res.NewFingerprint != newFP {
		t.Fatalf("fingerprints = %+v", res)
	}
	// The running store uses the new key at once.
	checkSecrets(t, opened.Store)
	if sess, _ := opened.Store.ListSessions(context.Background(), ""); len(sess) != 0 {
		t.Fatalf("sessions after rotation = %d; want 0", len(sess))
	}
	macs, err := opened.Store.RetiredImportedKeyMACs(context.Background())
	wantMAC, _ := secretbox.DeriveSubkey(oldKey, auth.ImportedKeySubkeyPurpose)
	if err != nil || len(macs) != 1 || !bytes.Equal(macs[0], wantMAC) {
		t.Fatalf("retired MAC keys = %d, %v", len(macs), err)
	}
	if err = opened.Store.Close(); err != nil {
		t.Fatal(err)
	}
	// The old key no longer opens the database; the new one does.
	oldBox, _ := secretbox.New(oldKey)
	if _, err = e.open(context.Background(), oldBox); !errors.Is(err, secretbox.ErrSecretKeyMismatch) {
		t.Fatalf("open with the old key = %v; want ErrSecretKeyMismatch", err)
	}
	again := e.start(t)
	if again.Outcome != keyrotation.OutcomeNone {
		t.Fatalf("outcome = %q", again.Outcome)
	}
	checkSecrets(t, again.Store)
}

func TestRotateTwiceKeepsImportedKeyMACs(t *testing.T) {
	e := newEnv(t)
	opened := e.start(t)
	seed(t, opened.Store)
	r := rotator(e, opened, nil, nil)
	for range 2 {
		if _, err := r.Rotate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	macs, err := opened.Store.RetiredImportedKeyMACs(context.Background())
	if err != nil || len(macs) != 2 {
		t.Fatalf("retired MAC keys = %d, %v; want 2", len(macs), err)
	}
	checkSecrets(t, opened.Store)
}

func TestEnvKeyIsRefused(t *testing.T) {
	r := keyrotation.New(keyrotation.Config{FromEnv: true})
	if _, err := r.Rotate(context.Background()); !errors.Is(err, keyrotation.ErrEnvKey) {
		t.Fatalf("rotate = %v; want ErrEnvKey", err)
	}
}

// TestCrashAtEveryStep stops a rotation after each step, as a crash would, and
// restarts: the store must open, every secret must read back, the key files must be
// settled and the old or the new key must be secret.key as the step implies.
func TestCrashAtEveryStep(t *testing.T) {
	cases := []struct {
		step    keyrotation.Step
		newKey  bool
		outcome keyrotation.Outcome
	}{
		{keyrotation.StepMarked, false, keyrotation.OutcomeRolledBack},
		{keyrotation.StepNextWritten, false, keyrotation.OutcomeRolledBack},
		{keyrotation.StepBeforeCommit, false, keyrotation.OutcomeRolledBack},
		{keyrotation.StepCommitted, true, keyrotation.OutcomeCompleted},
		{keyrotation.StepPreviousKept, true, keyrotation.OutcomeCompleted},
		{keyrotation.StepInstalled, true, keyrotation.OutcomeCompleted},
	}
	for _, tc := range cases {
		t.Run(string(tc.step), func(t *testing.T) {
			e := newEnv(t)
			opened := e.start(t)
			seed(t, opened.Store)
			oldKey := opened.Key
			_, err := rotator(e, opened, func(s keyrotation.Step) error {
				if s == tc.step {
					return errCrash
				}
				return nil
			}, nil).Rotate(context.Background())
			if !errors.Is(err, errCrash) {
				t.Fatalf("rotate = %v; want the injected crash", err)
			}
			if err = opened.Store.Close(); err != nil {
				t.Fatal(err)
			}

			again := e.start(t)
			if again.Outcome != tc.outcome {
				t.Fatalf("outcome = %q; want %q", again.Outcome, tc.outcome)
			}
			checkSecrets(t, again.Store)
			cur := readKey(t, e.files.Current)
			if tc.newKey == bytes.Equal(cur, oldKey) {
				t.Fatalf("secret.key is the old key = %v; want %v", bytes.Equal(cur, oldKey), !tc.newKey)
			}
			if !bytes.Equal(again.Key, cur) {
				t.Fatal("the store key is not secret.key")
			}
			if exists(t, e.files.Next) {
				t.Fatal("secret.key.next was left behind")
			}
			if tc.newKey && !bytes.Equal(readKey(t, e.files.Previous), oldKey) {
				t.Fatal("secret.key.previous is not the old key")
			}
			if m, err := again.Store.PendingKeyRotation(context.Background()); err != nil || m != nil {
				t.Fatalf("marker = %+v, %v; want none", m, err)
			}
			// A new rotation works after the recovery.
			if _, err = rotator(e, again, nil, nil).Rotate(context.Background()); err != nil {
				t.Fatalf("rotate after recovery: %v", err)
			}
			checkSecrets(t, again.Store)
		})
	}
}

// TestLostCommitIsRolledBack covers a power loss that drops the committed
// transaction after the new key was installed: the database is sealed with the old
// key, which secret.key.previous still holds.
func TestLostCommitIsRolledBack(t *testing.T) {
	e := newEnv(t)
	opened := e.start(t)
	seed(t, opened.Store)
	oldKey := opened.Key
	_, err := rotator(e, opened, func(s keyrotation.Step) error {
		if s == keyrotation.StepBeforeCommit {
			return errCrash
		}
		return nil
	}, nil).Rotate(context.Background())
	if !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	_ = opened.Store.Close()
	// What the files look like when the commit is lost after step 5.
	if err = secretbox.WriteKeyFile(e.files.Previous, oldKey); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(e.files.Next, e.files.Current); err != nil {
		t.Fatal(err)
	}
	again := e.start(t)
	if again.Outcome != keyrotation.OutcomeRolledBack || !bytes.Equal(again.Key, oldKey) || !bytes.Equal(readKey(t, e.files.Current), oldKey) {
		t.Fatalf("outcome = %q; want a roll back to the old key", again.Outcome)
	}
	if exists(t, e.files.Previous) || exists(t, e.files.Next) {
		t.Fatal("rotation files were left behind")
	}
	checkSecrets(t, again.Store)
}

// TestForeignKeyIsStillRefused makes sure the recovery never accepts a key the
// marker does not name.
func TestForeignKeyIsStillRefused(t *testing.T) {
	e := newEnv(t)
	opened := e.start(t)
	seed(t, opened.Store)
	_ = opened.Store.Close()
	other, _ := secretbox.GenerateKey()
	if err := secretbox.WriteKeyFile(e.files.Current, other); err != nil {
		t.Fatal(err)
	}
	if err := secretbox.WriteKeyFile(e.files.Next, other); err != nil {
		t.Fatal(err)
	}
	_, err := keyrotation.Open(context.Background(), e.files, other, false, e.open, slog.New(slog.DiscardHandler))
	if !errors.Is(err, secretbox.ErrSecretKeyMismatch) {
		t.Fatalf("open = %v; want ErrSecretKeyMismatch", err)
	}
}

func TestConcurrentRotationIsRefused(t *testing.T) {
	e := newEnv(t)
	opened := e.start(t)
	block := make(chan struct{})
	entered := make(chan struct{})
	r := rotator(e, opened, func(s keyrotation.Step) error {
		if s == keyrotation.StepMarked {
			close(entered)
			<-block
		}
		return nil
	}, nil)
	done := make(chan error, 1)
	go func() { _, err := r.Rotate(context.Background()); done <- err }()
	<-entered
	if _, err := r.Rotate(context.Background()); !errors.Is(err, keyrotation.ErrBusy) {
		t.Fatalf("second rotate = %v; want ErrBusy", err)
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// memSettings is an in-memory settings.Repository.
type memSettings struct {
	mu     sync.Mutex
	values map[string]string
}

func (m *memSettings) LoadSettings(context.Context) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.values), nil
}

func (m *memSettings) SaveSettings(_ context.Context, v map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		m.values = map[string]string{}
	}
	maps.Copy(m.values, v)
	return nil
}

func ptrTo[T any](v T) *T { return &v }

// keyLifecycle wires a backup and a restore engine to one settings service, as the
// application does, so key changes apply to the next run.
type keyLifecycle struct {
	t       *testing.T
	svc     *settings.Service
	store   *storage.MockStorage
	backups *backup.Engine
	runner  *capturingRunner
	restore *Engine
}

func newKeyLifecycle(t *testing.T, repo *memSettings) *keyLifecycle {
	t.Helper()
	svc, err := settings.NewService(context.Background(), repo, settings.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	k := &keyLifecycle{t: t, svc: svc, store: storage.NewMockStorage(), runner: &capturingRunner{}}
	k.backups = backup.NewEngine(k.store, "mongodb://localhost:27017", backup.WithRunConfig(func() backup.RunConfig {
		return backup.RunConfig{Encryptor: svc.Encryptor()}
	}), backup.WithRunner(func(context.Context, string, ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("dump of " + t.Name())), strings.NewReader(""), func() error { return nil }, nil
	}))
	k.restore = NewEngine(k.store, "mongodb://localhost:27017", WithRunner(k.runner.run), WithRunConfig(func() RunConfig {
		return RunConfig{Decryptor: svc.Decryptor(), VerifyPolicy: models.VerifyAlways}
	}))
	return k
}

func (k *keyLifecycle) update(p settings.EncryptionPatch) {
	k.t.Helper()
	if _, err := k.svc.Update(context.Background(), settings.Patch{Encryption: &p}); err != nil {
		k.t.Fatal(err)
	}
}

func (k *keyLifecycle) backup(db string) *models.BackupRecord {
	k.t.Helper()
	rec, err := k.backups.Run(context.Background(), models.BackupOptions{Database: db})
	if err != nil {
		k.t.Fatal(err)
	}
	return rec
}

func (k *keyLifecycle) restoreOf(rec *models.BackupRecord) (*models.RestoreRecord, error) {
	k.runner.called, k.runner.stdin = false, nil
	return k.restore.Run(context.Background(), models.RestoreRequest{BackupID: rec.ID}, rec)
}

// TestRotatedKeysKeepOldBackupsRestorable rotates the X25519 key pair twice and then
// removes the identity: every backup taken before stays restorable, because replaced
// identities are retired, not forgotten.
func TestRotatedKeysKeepOldBackupsRestorable(t *testing.T) {
	k := newKeyLifecycle(t, &memSettings{})
	first, _ := settings.GenerateKey()
	second, _ := settings.GenerateKey()
	third, _ := settings.GenerateKey()

	k.update(settings.EncryptionPatch{Enabled: ptrTo(true), Recipients: ptrTo([]string{first.Recipient}), Identity: ptrTo(first.Identity)})
	old := k.backup("before_rotation")
	k.update(settings.EncryptionPatch{Recipients: ptrTo([]string{second.Recipient}), Identity: ptrTo(second.Identity)})
	middle := k.backup("after_first_rotation")
	k.update(settings.EncryptionPatch{Recipients: ptrTo([]string{third.Recipient}), Identity: ptrTo(third.Identity)})
	latest := k.backup("after_second_rotation")
	// The last identity leaves as well (a backup-only instance from now on), but
	// it is retired, so restores keep working.
	k.update(settings.EncryptionPatch{Identity: ptrTo("")})

	for _, rec := range []*models.BackupRecord{old, middle, latest} {
		if !rec.Encrypted {
			t.Fatalf("%s is not encrypted", rec.ID)
		}
		out, err := k.restoreOf(rec)
		if err != nil {
			t.Fatalf("restore %s after rotation: %v", rec.Database, err)
		}
		if !out.Verified || string(k.runner.stdin) != "dump of "+t.Name() {
			t.Fatalf("restore %s: verified=%v stdin=%q", rec.Database, out.Verified, k.runner.stdin)
		}
	}
	if n := len(k.svc.Current().Encryption.RetiredKeys); n != 3 {
		t.Fatalf("retired keys = %d; want 3", n)
	}
}

// TestLostIdentityMakesBackupsUnrestorable documents key loss: an instance that
// never held the identity (backup-only) or a new installation without it cannot
// restore the encrypted backups. The restore fails before mongorestore starts, says
// what is missing, and never falls back to treating the ciphertext as a dump.
func TestLostIdentityMakesBackupsUnrestorable(t *testing.T) {
	key, _ := settings.GenerateKey()
	repo := &memSettings{}
	k := newKeyLifecycle(t, repo)
	k.update(settings.EncryptionPatch{Enabled: ptrTo(true), Recipients: ptrTo([]string{key.Recipient})})
	rec := k.backup("shop")
	if k.svc.Decryptor() != nil {
		t.Fatal("a backup-only instance must not hold a decryptor")
	}

	out, err := k.restoreOf(rec)
	if !errors.Is(err, encryption.ErrEncryptionKeyRequired) {
		t.Fatalf("restore without identity = %v; want ErrEncryptionKeyRequired", err)
	}
	if k.runner.called {
		t.Fatal("mongorestore must not start without the key")
	}
	for _, s := range []string{err.Error(), out.ErrorMessage} {
		if !strings.Contains(s, "Settings → Encryption") || !strings.Contains(s, "cannot be restored") {
			t.Fatalf("the error must tell the operator what is missing: %q", s)
		}
	}

	// A different identity does not help either: the data is unrecoverable without
	// the original one.
	other, _ := settings.GenerateKey()
	k.update(settings.EncryptionPatch{Identity: ptrTo(other.Identity)})
	if _, err = k.restoreOf(rec); !errors.Is(err, encryption.ErrDecryptionFailed) || k.runner.called {
		t.Fatalf("restore with another identity = %v (mongorestore started: %v); want ErrDecryptionFailed", err, k.runner.called)
	}

	// Escrowed offline, the original identity restores the backup.
	k.update(settings.EncryptionPatch{Identity: ptrTo(key.Identity)})
	if _, err = k.restoreOf(rec); err != nil || !bytes.Equal(k.runner.stdin, []byte("dump of "+t.Name())) {
		t.Fatalf("restore with the escrowed identity = %v", err)
	}
}

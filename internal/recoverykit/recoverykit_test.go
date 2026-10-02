package recoverykit_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/recoverykit"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

const passphrase = "a long kit passphrase"

type fakeSettings struct {
	cur          settings.Settings
	noted        string
	downloaded   string
	downloadedAt time.Time
}

func (f *fakeSettings) Current() settings.Settings           { return f.cur }
func (f *fakeSettings) NoteRecoveryKitFingerprint(fp string) { f.noted = fp }
func (f *fakeSettings) MarkRecoveryKitDownloaded(_ context.Context, fp string, at time.Time) error {
	f.downloaded, f.downloadedAt = fp, at
	return nil
}

type fakeTargets struct{ list []*models.StorageTarget }

func (f *fakeTargets) List(context.Context) ([]*models.StorageTarget, error) {
	out := make([]*models.StorageTarget, 0, len(f.list))
	for _, t := range f.list {
		out = append(out, t.Redacted())
	}
	return out, nil
}

func (f *fakeTargets) Resolve(_ context.Context, id string) (*models.StorageTarget, error) {
	for _, t := range f.list {
		if t.ID == id {
			return t.Clone(), nil
		}
	}
	return nil, errors.New("not found")
}

type fixture struct {
	svc      *recoverykit.Service
	settings *fakeSettings
	targets  *fakeTargets
	key      []byte
	identity string
}

const testPrefix = metabackup.Prefix + "0123456789abcdef/"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, nil)
}

// newFixtureWith applies mutate to the settings before building the kit service.
func newFixtureWith(t *testing.T, mutate func(*settings.Settings)) *fixture {
	t.Helper()
	key, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	identity, recipient, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	cur := settings.Defaults()
	cur.Encryption.Enabled, cur.Encryption.Recipients, cur.Encryption.Identity = true, []string{recipient}, identity
	cur.Encryption.Passphrase = "a stored backup passphrase"
	if mutate != nil {
		mutate(&cur)
	}
	f := &fixture{
		settings: &fakeSettings{cur: cur},
		targets: &fakeTargets{list: []*models.StorageTarget{{
			ID: "stg_1", Name: "bucket", Type: models.StorageS3, UpdatedAt: time.Unix(100, 0),
			S3: &models.S3Target{Bucket: "b", AccessKeyID: "AKIA", SecretAccessKey: "s3-secret-value"},
		}}},
		key:      key,
		identity: identity,
	}
	f.svc, err = recoverykit.New(recoverykit.Config{
		SecretKey: key, Settings: f.settings, Targets: f.targets, Version: "v0.14.0", WorkFactor: 10,
		LatestSnapshot: func(context.Context) *metabackup.Snapshot {
			return &metabackup.Snapshot{TargetID: "stg_1", InstallID: "0123456789abcdef", Key: testPrefix + "mongorescue-20261002T100000000Z.db.age", Encrypted: true}
		},
		MetadataPrefix: testPrefix,
		Now:            func() time.Time { return time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// open decrypts a sealed kit with passphrase and returns its files.
func open(t *testing.T, sealed []byte, pass string) map[string]string {
	t.Helper()
	dec, err := encryption.NewDecryptor(encryption.DecryptorConfig{Passphrase: pass})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := dec.Decrypt(bytes.NewReader(sealed))
	if err != nil {
		t.Fatalf("decrypt kit: %v", err)
	}
	files := map[string]string{}
	tr := tar.NewReader(plain)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name] = string(b)
	}
	return files
}

// TestKitRoundTrip seals a kit, opens it with the passphrase and checks its
// content: secret.key, the identities, the targets with their credentials and the
// snapshot, but never a passphrase.
func TestKitRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	kit, err := f.svc.Prepare(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	if err = f.svc.Seal(&sealed, kit, passphrase); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed.Bytes(), []byte(secretbox.EncodeKey(f.key))) {
		t.Fatal("the sealed kit contains secret.key in plain form")
	}
	files := open(t, sealed.Bytes(), passphrase)
	if got := strings.TrimSpace(files[recoverykit.SecretKeyName]); got != secretbox.EncodeKey(f.key) {
		t.Fatalf("secret.key = %q", got)
	}
	if parsed, parseErr := secretbox.ParseKey(files[recoverykit.SecretKeyName]); parseErr != nil || !bytes.Equal(parsed, f.key) {
		t.Fatalf("secret.key does not parse back: %v", parseErr)
	}
	if !strings.Contains(files[recoverykit.IdentitiesName], f.identity) {
		t.Fatal("identities.txt lacks the identity")
	}
	if !strings.Contains(files[recoverykit.ReadmeName], "age -d") {
		t.Fatal("README lacks the recovery steps")
	}
	var m recoverykit.Manifest
	if err = json.Unmarshal([]byte(files[recoverykit.ManifestName]), &m); err != nil {
		t.Fatal(err)
	}
	if m.Format != recoverykit.Format || len(m.StorageTargets) != 1 || m.StorageTargets[0].S3.SecretAccessKey != "s3-secret-value" {
		t.Fatalf("manifest = %+v", m)
	}
	if m.MetadataSnapshot == nil || m.MetadataSnapshot.TargetID != "stg_1" || m.MetadataPrefix != testPrefix || !m.Encryption.PassphraseConfigured {
		t.Fatalf("manifest = %+v", m)
	}
	for name, body := range files {
		if strings.Contains(body, "a stored backup passphrase") {
			t.Fatalf("%s contains the backup passphrase", name)
		}
	}
	if err = f.svc.MarkDownloaded(ctx, kit); err != nil {
		t.Fatal(err)
	}
	fp, _ := f.svc.Fingerprint(ctx)
	if f.settings.downloaded != fp {
		t.Fatal("MarkDownloaded must record the kit's fingerprint")
	}
}

// sealAndOpen prepares, seals and opens a kit of f.
func sealAndOpen(t *testing.T, f *fixture) map[string]string {
	t.Helper()
	kit, err := f.svc.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	if err = f.svc.Seal(&sealed, kit, passphrase); err != nil {
		t.Fatal(err)
	}
	return open(t, sealed.Bytes(), passphrase)
}

// TestReadmeFollowsTheKeyMaterial checks the decryption step of the README: with
// identities on the server it points to identities.txt; in recipient-only mode the
// kit has no identities.txt and the README points to the administrator's own key.
func TestReadmeFollowsTheKeyMaterial(t *testing.T) {
	withIdentity := sealAndOpen(t, newFixture(t))
	readme := withIdentity[recoverykit.ReadmeName]
	if !strings.Contains(readme, "age -d -i identities.txt") || strings.Contains(readme, "<your key file>") || withIdentity[recoverykit.IdentitiesName] == "" {
		t.Fatalf("README with identities:\n%s", readme)
	}
	if !strings.Contains(readme, testPrefix) {
		t.Fatal("README must name this installation's snapshot prefix")
	}

	recipientOnly := sealAndOpen(t, newFixtureWith(t, func(s *settings.Settings) {
		s.Encryption.Identity, s.Encryption.Passphrase = "", ""
	}))
	readme = recipientOnly[recoverykit.ReadmeName]
	if _, ok := recipientOnly[recoverykit.IdentitiesName]; ok {
		t.Fatal("a recipient-only kit must not contain identities.txt")
	}
	if strings.Contains(readme, "identities.txt") || !strings.Contains(readme, "age -d -i <your key file>") {
		t.Fatalf("README in recipient-only mode:\n%s", readme)
	}
	if strings.Contains(readme, "passphrase encryption") {
		t.Fatal("README must not describe passphrase decryption without a passphrase")
	}
}

// TestWrongPassphraseCannotOpenTheKit checks that the kit only opens with its
// passphrase.
func TestWrongPassphraseCannotOpenTheKit(t *testing.T) {
	f := newFixture(t)
	kit, err := f.svc.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	if err = f.svc.Seal(&sealed, kit, passphrase); err != nil {
		t.Fatal(err)
	}
	dec, err := encryption.NewDecryptor(encryption.DecryptorConfig{Passphrase: "another passphrase"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dec.Decrypt(bytes.NewReader(sealed.Bytes())); err == nil {
		t.Fatal("the kit opened with a wrong passphrase")
	}
}

// TestPassphraseRules checks the passphrase length rules.
func TestPassphraseRules(t *testing.T) {
	for _, p := range []string{"", "short", "elevenchars", strings.Repeat(" ", 20), strings.Repeat("x", recoverykit.MaxPassphraseLength+1)} {
		if err := recoverykit.ValidatePassphrase(p); !errors.Is(err, recoverykit.ErrPassphrase) {
			t.Errorf("ValidatePassphrase(%d chars) = %v", len(p), err)
		}
	}
	if err := recoverykit.ValidatePassphrase("twelve chars"); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	kit, err := f.svc.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.svc.Seal(io.Discard, kit, "short"); !errors.Is(err, recoverykit.ErrPassphrase) {
		t.Fatalf("Seal with a short passphrase = %v", err)
	}
}

// TestFingerprintFollowsTheMaterial checks that the fingerprint changes with the
// storage targets and the keys, and not otherwise.
func TestFingerprintFollowsTheMaterial(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	first := f.settings.noted
	if first == "" {
		t.Fatal("Refresh must note a fingerprint")
	}
	if again, _ := f.svc.Fingerprint(ctx); again != first {
		t.Fatal("the fingerprint must be stable")
	}
	f.targets.list[0].UpdatedAt = time.Unix(200, 0)
	changed, _ := f.svc.Fingerprint(ctx)
	if changed == first {
		t.Fatal("a changed storage target must change the fingerprint")
	}
	_, recipient, _ := encryption.GenerateX25519()
	f.settings.cur.Encryption.Recipients = append(f.settings.cur.Encryption.Recipients, recipient)
	if next, _ := f.svc.Fingerprint(ctx); next == changed {
		t.Fatal("a new recipient must change the fingerprint")
	}
	other, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	svc2, err := recoverykit.New(recoverykit.Config{SecretKey: other, Settings: f.settings, Targets: f.targets})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := f.svc.Fingerprint(ctx)
	b, _ := svc2.Fingerprint(ctx)
	if a == b {
		t.Fatal("another secret.key must change the fingerprint")
	}
}

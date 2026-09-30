package restore

//go:generate go run ./testdata/compat/gen -dir testdata/compat

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// Golden artifacts in testdata/compat guard the restore pipeline against format
// regressions: each is a tiny fake mongodump archive stored in a layout released
// versions produced, under its storage key, with the backup record the store kept.
// Regenerate them with "go generate ./internal/restore" (see testdata/compat/gen).
//
// TEST ONLY: testdata/compat/TEST-ONLY-age-identity.txt and compatPassphrase are
// public keys for these fixtures and must never be used for real backups.

const compatDir = "testdata/compat"

// compatPassphrase is the TEST ONLY passphrase of the scrypt fixture (see gen/main.go).
const compatPassphrase = "TEST ONLY passphrase - never use for real backups"

// storageKeyLayout is the storage key naming scheme of generated backups since
// v0.1.0: <db>/<yyyy>/<mm>/bkp_<db>_<yyyymmdd>_<hhmmss>_<suffix>.archive[.gz][.age].
var storageKeyLayout = regexp.MustCompile(`^([^/]+)/\d{4}/\d{2}/bkp_([^/]+)_\d{8}_\d{6}_[a-z0-9]+\.archive(\.gz)?(\.age)?$`)

type compatFixture struct {
	Name      string              `json:"name"`
	WantGzip  bool                `json:"want_gzip"`
	WantStdin string              `json:"want_stdin"`
	Record    models.BackupRecord `json:"record"`
}

func loadCompatFixtures(t *testing.T) []compatFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(compatDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Fixtures []compatFixture `json:"fixtures"`
	}
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Fixtures) < 5 {
		t.Fatalf("manifest lists %d fixtures; want every layout", len(m.Fixtures))
	}
	return m.Fixtures
}

func compatDecryptor(t *testing.T) *encryption.Decryptor {
	t.Helper()
	identity, err := os.ReadFile(filepath.Join(compatDir, "TEST-ONLY-age-identity.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return mustDecryptor(t, encryption.DecryptorConfig{Identity: string(identity), Passphrase: compatPassphrase})
}

// TestGoldenArtifactsStillRestore restores every golden artifact through the
// current pipeline from a local storage target, with and without verification, and
// checks the decoding path chosen for it: decryption, --gzip, the checksum and the
// exact bytes mongorestore receives.
func TestGoldenArtifactsStillRestore(t *testing.T) {
	store, err := storage.NewLocalStorage(filepath.Join(compatDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(filepath.Join(compatDir, "fake-mongodump.archive"))
	if err != nil {
		t.Fatal(err)
	}
	dec := compatDecryptor(t)

	for _, fx := range loadCompatFixtures(t) {
		for _, verify := range []bool{true, false} {
			name := fx.Name
			if verify {
				name += "/verify"
			}
			t.Run(name, func(t *testing.T) {
				rec := fx.Record
				m := storageKeyLayout.FindStringSubmatch(rec.StorageKey)
				if m == nil || m[1] != rec.Database || m[2] != rec.Database {
					t.Fatalf("storage key %q does not follow the naming scheme", rec.StorageKey)
				}
				if (m[3] != "") != fx.WantGzip || (m[4] != "") != rec.Encrypted {
					t.Fatalf("storage key %q disagrees with the record (gzip %v, encrypted %v)", rec.StorageKey, fx.WantGzip, rec.Encrypted)
				}

				want, err := os.ReadFile(filepath.Join(compatDir, filepath.FromSlash(fx.WantStdin)))
				if err != nil {
					t.Fatal(err)
				}
				// Every layout carries the same dump.
				plain := want
				if fx.WantGzip {
					zr, zerr := gzip.NewReader(bytes.NewReader(want))
					if zerr != nil {
						t.Fatal(zerr)
					}
					if plain, err = io.ReadAll(zr); err != nil {
						t.Fatal(err)
					}
				}
				if !bytes.Equal(plain, archive) {
					t.Fatal("the fixture does not hold the fake mongodump archive")
				}

				runner := &capturingRunner{}
				engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run), WithDecryptor(dec))
				out, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: rec.ID, Verify: &verify}, &rec)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if out.Status != models.RestoreStatusCompleted || out.Verified != verify {
					t.Fatalf("record = %+v", out)
				}
				if got := slices.Contains(runner.args, "--gzip"); got != fx.WantGzip {
					t.Fatalf("--gzip = %v; want %v (args %v)", got, fx.WantGzip, runner.args)
				}
				if !bytes.Equal(runner.stdin, want) {
					t.Fatalf("mongorestore received %d bytes; want the %d bytes of %s", len(runner.stdin), len(want), fx.WantStdin)
				}
			})
		}
	}
}

// TestGoldenArtifactsDetectCorruption flips one byte of every golden artifact:
// verification must refuse it before mongorestore starts.
func TestGoldenArtifactsDetectCorruption(t *testing.T) {
	dec := compatDecryptor(t)
	for _, fx := range loadCompatFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			stored, err := os.ReadFile(filepath.Join(compatDir, "store", filepath.FromSlash(fx.Record.StorageKey)))
			if err != nil {
				t.Fatal(err)
			}
			stored[len(stored)/2] ^= 0x01
			mock := storage.NewMockStorage()
			saveArtifact(t, mock, fx.Record.StorageKey, stored)

			rec := fx.Record
			runner := &capturingRunner{}
			engine := NewEngine(mock, "mongodb://localhost:27017", WithRunner(runner.run), WithDecryptor(dec), WithVerifyPolicy(models.VerifyAlways))
			_, err = engine.Run(context.Background(), models.RestoreRequest{BackupID: rec.ID}, &rec)
			if !errors.Is(err, ErrChecksumMismatch) && !errors.Is(err, encryption.ErrDecryptionFailed) {
				t.Fatalf("Run = %v; want a checksum or decryption failure", err)
			}
			if runner.called {
				t.Fatal("mongorestore must not start for a corrupted artifact")
			}
		})
	}
}

// TestGoldenEncryptedArtifactsNeedTheirKey restores the encrypted golden artifacts
// without key material and with an unrelated key: both fail before mongorestore
// starts and never hand ciphertext over as if it were an archive.
func TestGoldenEncryptedArtifactsNeedTheirKey(t *testing.T) {
	store, err := storage.NewLocalStorage(filepath.Join(compatDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := newKeyPair(t)
	for _, fx := range loadCompatFixtures(t) {
		if !fx.Record.Encrypted {
			continue
		}
		for name, tc := range map[string]struct {
			opts []Option
			want error
		}{
			"no key":    {nil, encryption.ErrEncryptionKeyRequired},
			"wrong key": {[]Option{WithDecryptor(mustDecryptor(t, encryption.DecryptorConfig{Identity: other, Passphrases: []string{"wrong passphrase"}}))}, encryption.ErrDecryptionFailed},
		} {
			t.Run(fx.Name+"/"+name, func(t *testing.T) {
				rec := fx.Record
				runner := &capturingRunner{}
				engine := NewEngine(store, "mongodb://localhost:27017", append(tc.opts, WithRunner(runner.run))...)
				_, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: rec.ID}, &rec)
				if !errors.Is(err, tc.want) {
					t.Fatalf("Run = %v; want %v", err, tc.want)
				}
				if runner.called {
					t.Fatal("mongorestore must not start")
				}
			})
		}
	}
}

// TestBackupEngineKeepsTheStorageKeyLayout checks that backups created today are
// named like the golden artifacts, so the restore pipeline keeps seeing the layout
// it was tested against.
func TestBackupEngineKeepsTheStorageKeyLayout(t *testing.T) {
	_, r := newKeyPair(t)
	enc, _ := encryption.NewX25519Encryptor([]string{r})
	for _, tc := range []struct {
		gzip bool
		enc  *encryption.Encryptor
		want string
	}{
		{false, nil, ".archive"},
		{true, nil, ".archive.gz"},
		{false, enc, ".archive.age"},
		{true, enc, ".archive.gz.age"},
	} {
		engine := backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", backup.WithEncryptor(tc.enc))
		rec, err := engine.Prepare(models.BackupOptions{Database: "shop", Gzip: tc.gzip})
		if err != nil {
			t.Fatal(err)
		}
		m := storageKeyLayout.FindStringSubmatch(rec.StorageKey)
		if m == nil || m[1] != "shop" || !strings.HasSuffix(rec.StorageKey, tc.want) || rec.Encrypted != (tc.enc != nil) ||
			!strings.HasPrefix(rec.StorageKey, "shop/"+rec.StartedAt.Format("2006/01")+"/"+rec.ID+".") {
			t.Fatalf("gzip=%v encrypted=%v: key %q, record %+v", tc.gzip, tc.enc != nil, rec.StorageKey, rec)
		}
	}
}

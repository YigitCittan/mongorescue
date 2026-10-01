package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// shopArchive is a real mongodump archive (see internal/mongotools/testdata/prelude).
func shopArchive(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "mongotools", "testdata", "prelude", "shop.archive"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var wantShopCollections = []models.BackupCollection{
	{Name: "metrics", Type: models.CollectionTypeTimeseries},
	{Name: "customers", Type: models.CollectionTypeCollection},
	{Name: "orders", Type: models.CollectionTypeCollection},
	{Name: "big_orders", Type: models.CollectionTypeView, ViewOn: "orders"},
}

// meteredStorage counts the bytes read from its objects and whether they were closed.
type meteredStorage struct {
	*storage.MockStorage
	read   int64
	closed bool
}

type meteredReader struct {
	rc io.ReadCloser
	s  *meteredStorage
}

func (m *meteredReader) Read(p []byte) (int, error) {
	n, err := m.rc.Read(p)
	m.s.read += int64(n)
	return n, err
}

func (m *meteredReader) Close() error {
	m.s.closed = true
	return m.rc.Close()
}

func (s *meteredStorage) Retrieve(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := s.MockStorage.Retrieve(ctx, key)
	if err != nil {
		return nil, err
	}
	return &meteredReader{rc: rc, s: s}, nil
}

func saveObject(t *testing.T, st storage.Storage, key string, b []byte) {
	t.Helper()
	if _, err := st.Save(context.Background(), key, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveCollectionsLayers(t *testing.T) {
	archive := shopArchive(t)
	id, recipient := newKeyPair(t)
	x25519, err := encryption.NewX25519Encryptor([]string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	scrypt, err := encryption.NewScryptEncryptor("prelude-pass", testScryptWorkFactor)
	if err != nil {
		t.Fatal(err)
	}
	dec := mustDecryptor(t, encryption.DecryptorConfig{Identity: id, Passphrase: "prelude-pass"})

	cases := []struct {
		name      string
		key       string
		body      []byte
		encrypted bool
	}{
		{"plain", "shop/a.archive", archive, false},
		{"gzip", "shop/a.archive.gz", gzipBytes(t, archive), false},
		{"age x25519 over gzip", "shop/a.archive.gz.age", sealPayload(t, x25519, gzipBytes(t, archive)), true},
		{"age scrypt over plain", "shop/a.archive.age", sealPayload(t, scrypt, archive), true},
		// Content decides over the key: a gzip archive under a plain key, and an
		// encrypted artifact whose record lost its encryption flag.
		{"gzip under a plain key", "shop/b.archive", gzipBytes(t, archive), false},
		{"unrecorded encryption", "shop/b.archive.gz", sealPayload(t, x25519, gzipBytes(t, archive)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := storage.NewMockStorage()
			saveObject(t, st, tc.key, tc.body)
			e := NewEngine(st, "", WithDecryptor(dec))
			got, err := e.ArchiveCollections(context.Background(), &models.BackupRecord{ID: "bkp_1", Database: "shop", StorageKey: tc.key, Encrypted: tc.encrypted})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, wantShopCollections) {
				t.Fatalf("collections = %+v; want %+v", got, wantShopCollections)
			}
		})
	}
}

// TestArchiveCollectionsReadsOnlyThePrelude stores a prelude followed by 8 MiB of
// data and checks that listing reads a small fraction of it and closes the stream.
func TestArchiveCollectionsReadsOnlyThePrelude(t *testing.T) {
	archive := shopArchive(t)
	// Incompressible filler, so the gzip layout stores megabytes as well.
	data := make([]byte, 8<<20)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range data {
		data[i] = byte(rng.Uint32())
	}
	big := append(slices.Clone(archive), data...)
	for _, layout := range []string{"plain", "gzip", "age"} {
		t.Run(layout, func(t *testing.T) {
			body, key, rec := big, "shop/big.archive", &models.BackupRecord{ID: "bkp_big", Database: "shop"}
			var dec *encryption.Decryptor
			switch layout {
			case "gzip":
				body, key = gzipBytes(t, big), "shop/big.archive.gz"
			case "age":
				id, r := newKeyPair(t)
				enc, err := encryption.NewX25519Encryptor([]string{r})
				if err != nil {
					t.Fatal(err)
				}
				body, key = sealPayload(t, enc, big), "shop/big.archive.age"
				rec.Encrypted = true
				dec = mustDecryptor(t, encryption.DecryptorConfig{Identity: id})
			}
			rec.StorageKey = key
			st := &meteredStorage{MockStorage: storage.NewMockStorage()}
			saveObject(t, st.MockStorage, key, body)
			got, err := NewEngine(st, "", WithDecryptor(dec)).ArchiveCollections(context.Background(), rec)
			if err != nil || len(got) != len(wantShopCollections) {
				t.Fatalf("collections = %+v, %v", got, err)
			}
			if st.read > 512<<10 || !st.closed {
				t.Fatalf("read %d of %d stored bytes (closed %v); only the prelude may be read", st.read, len(body), st.closed)
			}
		})
	}
}

func TestArchiveCollectionsKeys(t *testing.T) {
	archive := shopArchive(t)
	_, recipient := newKeyPair(t)
	enc, err := encryption.NewX25519Encryptor([]string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	st := storage.NewMockStorage()
	saveObject(t, st, "shop/a.archive.age", sealPayload(t, enc, archive))
	saveObject(t, st, "shop/b.archive", sealPayload(t, enc, archive))

	// No key: refused before anything is read, with the restore hint.
	for _, rec := range []*models.BackupRecord{
		{ID: "bkp_a", Database: "shop", StorageKey: "shop/a.archive.age", Encrypted: true},
		{ID: "bkp_a", Database: "shop", StorageKey: "shop/a.archive.age"},
		{ID: "bkp_b", Database: "shop", StorageKey: "shop/b.archive"}, // encryption flag lost
	} {
		_, err := NewEngine(st, "").ArchiveCollections(context.Background(), rec)
		if !errors.Is(err, encryption.ErrEncryptionKeyRequired) || !strings.Contains(err.Error(), KeyRequiredHint) {
			t.Fatalf("%s without a key: %v; want ErrEncryptionKeyRequired with the hint", rec.StorageKey, err)
		}
	}

	// A key that does not match.
	otherID, _ := newKeyPair(t)
	_, err = NewEngine(st, "", WithDecryptor(mustDecryptor(t, encryption.DecryptorConfig{Identity: otherID}))).
		ArchiveCollections(context.Background(), &models.BackupRecord{ID: "bkp_a", Database: "shop", StorageKey: "shop/a.archive.age", Encrypted: true})
	if !errors.Is(err, encryption.ErrDecryptionFailed) {
		t.Fatalf("wrong key: %v; want ErrDecryptionFailed", err)
	}
}

func TestArchiveCollectionsUnreadable(t *testing.T) {
	archive := shopArchive(t)
	st := storage.NewMockStorage()
	saveObject(t, st, "shop/trunc.archive", archive[:300])
	saveObject(t, st, "shop/trunc.archive.gz", gzipBytes(t, archive)[:200])
	saveObject(t, st, "shop/junk.archive", []byte("not an archive at all"))
	e := NewEngine(st, "")
	cases := map[string]error{
		"shop/trunc.archive":    mongotools.ErrArchiveTruncated,
		"shop/trunc.archive.gz": mongotools.ErrArchiveTruncated,
		"shop/junk.archive":     mongotools.ErrNotArchive,
		"shop/missing.archive":  storage.ErrNotFound,
	}
	for key, want := range cases {
		_, err := e.ArchiveCollections(context.Background(), &models.BackupRecord{ID: "bkp_x", Database: "shop", StorageKey: key})
		if !errors.Is(err, want) {
			t.Fatalf("%s: %v; want %v", key, err, want)
		}
	}
	if _, err := e.ArchiveCollections(context.Background(), &models.BackupRecord{ID: "bkp_x", Database: "shop"}); !errors.Is(err, ErrNoArtifact) {
		t.Fatalf("record without a storage key: %v; want ErrNoArtifact", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	saveObject(t, st, "shop/ok.archive", archive)
	if _, err := e.ArchiveCollections(ctx, &models.BackupRecord{ID: "bkp_x", Database: "shop", StorageKey: "shop/ok.archive"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v; want context.Canceled", err)
	}
}

// TestSelectiveRestoreArgs checks the arguments of a selective restore: one
// --nsInclude per selected collection and no database-wide one, so that --drop (which
// mongorestore applies to each collection right before restoring it) drops only the
// selected collections. The record keeps the selection.
func TestSelectiveRestoreArgs(t *testing.T) {
	runner := &capturingRunner{}
	st := storage.NewMockStorage()
	saveObject(t, st, "shop/a.archive", shopArchive(t))
	e := NewEngine(st, "mongodb://h", WithRunner(runner.run))
	no := false
	for _, inPlace := range []bool{false, true} {
		req := models.RestoreRequest{BackupID: "bkp_1", DropTarget: true, SelectedCollections: []string{" orders ", "", "big_orders"}}
		if inPlace {
			req.SafeClone, req.ConfirmInPlace = &no, true
		}
		rec, err := e.Run(context.Background(), req, &models.BackupRecord{ID: "bkp_1", Database: "shop", StorageKey: "shop/a.archive"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(rec.SelectedCollections, []string{"orders", "big_orders"}) {
			t.Fatalf("record selection = %q", rec.SelectedCollections)
		}
		var ns []string
		for _, a := range runner.args {
			if strings.HasPrefix(a, "--nsInclude=") {
				ns = append(ns, a)
			}
		}
		if !slices.Contains(runner.args, "--drop") || !slices.Equal(ns, []string{"--nsInclude=shop.orders", "--nsInclude=shop.big_orders"}) {
			t.Fatalf("in place %v: args = %q; want --drop and only the selected namespaces", inPlace, runner.args)
		}
	}

	// Without a selection the whole database is restored and the record says nothing.
	rec, err := e.Run(context.Background(), models.RestoreRequest{BackupID: "bkp_1", SelectedCollections: []string{" "}},
		&models.BackupRecord{ID: "bkp_1", Database: "shop", StorageKey: "shop/a.archive"})
	if err != nil || rec.SelectedCollections != nil || !slices.Contains(runner.args, "--nsInclude=shop.*") {
		t.Fatalf("whole-database restore: %+v, %v, args %q", rec, err, runner.args)
	}
}

// TestArchiveCollectionsOfGoldenArtifacts reads the golden artifacts of every layout
// released versions produced: their layers are peeled, and the fake archive inside
// is reported as unreadable (so the API falls back to the record).
func TestArchiveCollectionsOfGoldenArtifacts(t *testing.T) {
	store, err := storage.NewLocalStorage(filepath.Join(compatDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(store, "", WithDecryptor(compatDecryptor(t)))
	for _, fx := range loadCompatFixtures(t) {
		rec := fx.Record
		_, err := e.ArchiveCollections(context.Background(), &rec)
		if !errors.Is(err, mongotools.ErrMalformedPrelude) {
			t.Fatalf("%s: %v; want ErrMalformedPrelude from the fake archive", fx.Name, err)
		}
	}
}

//go:build integration

package integration

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestPITRBaseBackupOfTheWholeInstance takes an instance-scope base backup of the
// replica set (mongodump --oplog) and checks its key, its T_before/T_after and that
// the encrypted, gzipped archive carries the dump's oplog namespace.
func TestPITRBaseBackupOfTheWholeInstance(t *testing.T) {
	env := requireMongo(t)
	requireReplicaSet(t, env)
	db := env.uniqueDB(t, "pitrbase")
	env.seed(t, db, "items", 50)

	identity, recipient, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.NewX25519Encryptor([]string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := encryption.NewDecryptor(encryption.DecryptorConfig{Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prober := mongoconn.New()
	engine := newBackupEngine(env, st, backup.WithEncryptor(enc), backup.WithOpTimeReader(prober.WriteOpTimes))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rec, err := engine.Run(ctx, models.BackupOptions{
		Scope: models.ScopeInstance, ConnectionID: "conn_it", ReplicaSet: "rs0", PITRStreamID: "str_it", MongoURI: env.URI,
	})
	if err != nil {
		t.Fatalf("base backup: %v", err)
	}
	if rec.Status != models.StatusCompleted || rec.TBefore == nil || rec.TAfter == nil || rec.TAfter.TS.Compare(rec.TBefore.TS) < 0 {
		t.Fatalf("record = %+v", rec)
	}
	if !strings.HasPrefix(rec.StorageKey, backup.BaseKeyPrefix+"conn_it/rs0/") || !strings.HasSuffix(rec.StorageKey, ".archive.gz.age") {
		t.Fatalf("key = %s", rec.StorageKey)
	}
	assertNoSecret(t, env.Password, "base record", rec.ErrorMessage, rec.StorageKey)

	r, err := st.Retrieve(ctx, rec.StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	plain, err := dec.Decrypt(r)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(archive, []byte(db)) || !bytes.Contains(archive, []byte("oplog")) {
		t.Fatalf("the archive (%d bytes) lacks the seeded database or the oplog namespace", len(archive))
	}
}

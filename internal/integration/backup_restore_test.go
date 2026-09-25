//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

const (
	ordersCount    = 500
	customersCount = 50
)

// TestBackupRestoreRoundTrip dumps a seeded database with the real mongodump (URI passed
// via --config), stores it on every storage target, and restores it with the real
// mongorestore into a safe clone, a filtered target, and an uncompressed variant.
func TestBackupRestoreRoundTrip(t *testing.T) {
	env := requireMongo(t)

	for _, target := range storageTargets(t) {
		t.Run(target.Name, func(t *testing.T) {
			t.Run("gzip safe clone", func(t *testing.T) {
				runRoundTrip(t, env, target.Storage, true)
			})
			t.Run("no gzip safe clone", func(t *testing.T) {
				runRoundTrip(t, env, target.Storage, false)
			})
			t.Run("collections filter", func(t *testing.T) {
				runFilteredRestore(t, env, target.Storage)
			})
		})
	}
}

// seedSource creates a unique source database with orders and customers collections.
func seedSource(t *testing.T, env *mongoEnv, tag string) string {
	t.Helper()
	db := env.uniqueDB(t, tag)
	env.seed(t, db, "orders", ordersCount)
	env.seed(t, db, "customers", customersCount)
	return db
}

// runBackup executes the real backup engine and validates the resulting record and artifact.
func runBackup(t *testing.T, env *mongoEnv, st storage.Storage, db string, gzip bool) *models.BackupRecord {
	t.Helper()
	logger, logs := captureLogger()
	engine := backup.NewEngine(st, env.URI, backup.WithLogger(logger))

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	rec, err := engine.Run(ctx, models.BackupOptions{Database: db, Gzip: gzip, MongoURI: env.URI})
	if err != nil {
		assertNoSecret(t, env.Password, "backup error", err.Error())
		t.Fatalf("backup: %v", err)
	}

	raw, _ := json.Marshal(rec)
	assertNoSecret(t, env.Password, "backup record/logs", string(raw), logs.String())

	if rec.Status != models.StatusCompleted {
		t.Fatalf("backup status = %s; want completed (%s)", rec.Status, rec.ErrorMessage)
	}
	if rec.SizeBytes <= 0 {
		t.Fatalf("backup size = %d; want > 0", rec.SizeBytes)
	}
	if len(rec.SHA256) != sha256.Size*2 {
		t.Fatalf("backup sha256 = %q; want 64 hex chars", rec.SHA256)
	}
	if err = models.ValidateID(rec.ID); err != nil {
		t.Fatalf("backup id %q invalid: %v", rec.ID, err)
	}
	if gzip != strings.HasSuffix(rec.StorageKey, ".gz") {
		t.Fatalf("storage key %q does not reflect gzip=%v", rec.StorageKey, gzip)
	}

	// The checksum recorded while streaming must match the stored artifact.
	rc, err := st.Retrieve(ctx, rec.StorageKey)
	if err != nil {
		t.Fatalf("retrieve artifact: %v", err)
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if n != rec.SizeBytes || hex.EncodeToString(h.Sum(nil)) != rec.SHA256 {
		t.Fatalf("stored artifact (%d bytes) does not match record (%d bytes, sha %s)", n, rec.SizeBytes, rec.SHA256)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_ = st.Delete(ctx, rec.StorageKey)
	})
	return rec
}

// runRestore executes the real restore engine and validates the record.
func runRestore(t *testing.T, env *mongoEnv, st storage.Storage, req models.RestoreRequest, src *models.BackupRecord) *models.RestoreRecord {
	t.Helper()
	logger, logs := captureLogger()
	engine := restore.NewEngine(st, env.URI, restore.WithLogger(logger))

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	rec, err := engine.Run(ctx, req, src)
	if err != nil {
		assertNoSecret(t, env.Password, "restore error", err.Error())
		t.Fatalf("restore: %v", err)
	}
	raw, _ := json.Marshal(rec)
	assertNoSecret(t, env.Password, "restore record/logs", string(raw), logs.String())

	if rec.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore status = %s; want completed (%s)", rec.Status, rec.ErrorMessage)
	}
	return rec
}

func runRoundTrip(t *testing.T, env *mongoEnv, st storage.Storage, gzip bool) {
	db := seedSource(t, env, "rt")
	bkp := runBackup(t, env, st, db, gzip)

	// Default request (safe_clone omitted) routes into <db>_rescue_<timestamp>.
	rst := runRestore(t, env, st, models.RestoreRequest{BackupID: bkp.ID}, bkp)
	if !strings.HasPrefix(rst.TargetDatabase, db+"_rescue_") {
		t.Fatalf("restore target = %q; want %s_rescue_*", rst.TargetDatabase, db)
	}

	if got := env.count(t, rst.TargetDatabase, "orders"); got != ordersCount {
		t.Fatalf("restored orders = %d; want %d", got, ordersCount)
	}
	if got := env.count(t, rst.TargetDatabase, "customers"); got != customersCount {
		t.Fatalf("restored customers = %d; want %d", got, customersCount)
	}

	// The source database must be untouched.
	if got := env.count(t, db, "orders"); got != ordersCount {
		t.Fatalf("source orders = %d after restore; want %d", got, ordersCount)
	}
	if got := env.count(t, db, "customers"); got != customersCount {
		t.Fatalf("source customers = %d after restore; want %d", got, customersCount)
	}
}

func runFilteredRestore(t *testing.T, env *mongoEnv, st storage.Storage) {
	db := seedSource(t, env, "flt")
	bkp := runBackup(t, env, st, db, true)

	target := db + "_sel"
	inPlace := false
	rst := runRestore(t, env, st, models.RestoreRequest{
		BackupID:            bkp.ID,
		TargetDatabase:      target,
		SafeClone:           &inPlace,
		ConfirmInPlace:      true,
		SelectedCollections: []string{"customers"},
	}, bkp)
	if rst.TargetDatabase != target {
		t.Fatalf("restore target = %q; want %q", rst.TargetDatabase, target)
	}

	if got := env.count(t, target, "customers"); got != customersCount {
		t.Fatalf("filtered restore customers = %d; want %d", got, customersCount)
	}
	if got := env.count(t, target, "orders"); got != 0 {
		t.Fatalf("filtered restore must skip orders, got %d", got)
	}
	if got := env.count(t, db, "orders"); got != ordersCount {
		t.Fatalf("source orders = %d after restore; want %d", got, ordersCount)
	}
}

//go:build integration

package integration

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/integrity"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// itTargetID is the storage target the integrity tests register their storage as.
const itTargetID = "tgt_it"

// oneTarget serves the storage under test as the only storage target.
type oneTarget struct{ st storage.Storage }

func (o oneTarget) List(context.Context) ([]*models.StorageTarget, error) {
	return []*models.StorageTarget{{ID: itTargetID, Name: "integration", Type: models.StorageLocal}}, nil
}

func (o oneTarget) Resolve(_ context.Context, id string) (*models.StorageTarget, error) {
	if id != "" && id != itTargetID {
		return nil, errors.New("unknown target")
	}
	return &models.StorageTarget{ID: itTargetID, Name: "integration", Type: models.StorageLocal}, nil
}

func (o oneTarget) Storage(context.Context, string) (storage.Storage, error) { return o.st, nil }

// uriConnections resolves every connection ID to a URI: "conn_it" to the test
// server's URI, others from the map.
type uriConnections struct {
	uri    string
	others map[string]string
}

func (c uriConnections) Resolve(_ context.Context, id string) (*models.Connection, error) {
	if uri, ok := c.others[id]; ok {
		return &models.Connection{ID: id, Name: id, URI: uri}, nil
	}
	return &models.Connection{ID: id, Name: "integration server", URI: c.uri}, nil
}

// integrityFixture wires the production integrity service to a real server, the
// real restore engine and the storage under test.
type integrityFixture struct {
	svc  *integrity.Service
	meta *store.SQLiteStore
	runs *runs.Manager
}

func newIntegrityFixture(t *testing.T, env *mongoEnv, st storage.Storage, conns uriConnections) *integrityFixture {
	t.Helper()
	meta := storetest.New(t)
	manager := runs.NewManager(discardLogger)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	svc := integrity.New(integrity.Config{
		Store: meta, Targets: oneTarget{st}, Runs: manager,
		Restore: newRestoreEngine(env, st), Admin: mongoconn.New(), Connections: conns, Logger: discardLogger,
	})
	return &integrityFixture{svc: svc, meta: meta, runs: manager}
}

func (f *integrityFixture) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(opTimeout)
	for len(f.runs.Active()) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("background work still running: %v", f.runs.Active())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// seedIndexed seeds db with orders (with a unique index) and customers.
func seedIndexed(t *testing.T, env *mongoEnv, tag string) string {
	t.Helper()
	db := seedSource(t, env, tag)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if _, err := env.Client.Database(db).Collection("orders").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "sku", Value: 1}}, Options: nil,
	}); err != nil {
		t.Fatalf("create index: %v", err)
	}
	return db
}

// manifestBackup runs a backup with manifest capture and post-upload verification,
// stores its record and returns it.
func manifestBackup(t *testing.T, env *mongoEnv, st storage.Storage, meta *store.SQLiteStore, db, jobID string) *models.BackupRecord {
	t.Helper()
	rec := mustBackup(t, env, st, models.BackupOptions{Database: db, Gzip: true, JobID: jobID, ConnectionID: "conn_it", StorageTargetID: itTargetID},
		backup.WithManifestCapturer(mongoconn.New().Manifest), backup.WithVerifyAfterUpload(true))
	if meta != nil {
		if err := meta.SaveBackupRecord(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	return rec
}

// TestPostBackupVerification checks the post-upload verification and the manifest
// captured by a real backup, and that a damaged copy of the archive is reported as
// a mismatch by an on-demand verification, on every storage target.
func TestPostBackupVerification(t *testing.T) {
	env := requireMongo(t)
	for _, target := range storageTargets(t) {
		t.Run(target.Name, func(t *testing.T) {
			ctx := context.Background()
			db := seedIndexed(t, env, "verify")
			f := newIntegrityFixture(t, env, target.Storage, uriConnections{uri: env.URI})
			rec := manifestBackup(t, env, target.Storage, f.meta, db, "")
			if rec.Verification != models.VerificationOK || rec.VerifiedAt == nil || !rec.HasManifest {
				t.Fatalf("backup = verification %q at %v, manifest %v", rec.Verification, rec.VerifiedAt, rec.HasManifest)
			}
			m, err := f.meta.GetManifest(ctx, rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			orders := m.Collection("orders")
			if orders == nil || orders.DocumentsMax != ordersCount || len(orders.Indexes) != 2 || m.Collection("customers") == nil {
				t.Fatalf("manifest = %+v", m)
			}

			// A damaged copy of the archive under another key.
			damagedKey := rec.StorageKey + ".damaged.archive.gz"
			copyObject(t, target.Storage, rec.StorageKey, damagedKey, flipByteAt(rec.SizeBytes/2))
			bad := *rec
			bad.ID, bad.StorageKey, bad.Verification, bad.VerifiedAt, bad.Manifest = rec.ID+"_dmg", damagedKey, "", nil, nil
			if err := f.meta.SaveBackupRecord(ctx, &bad); err != nil {
				t.Fatal(err)
			}
			got, err := f.svc.Verify(ctx, bad.ID, events.VerificationOnDemand, 0)
			if err != nil || got.Verification != models.VerificationMismatch || !strings.Contains(got.VerificationError, "checksum") {
				t.Fatalf("damaged archive = %+v, %v", got, err)
			}
			assertNoSecret(t, env.Password, "verification error", got.VerificationError)
		})
	}
}

// rescueVerifyDatabases lists the restore test databases derived from db.
func rescueVerifyDatabases(t *testing.T, env *mongoEnv, db string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	names, err := env.Client.ListDatabaseNames(ctx, bson.D{{Key: "name", Value: bson.D{{Key: "$regex", Value: "^" + db + "_rescue_verify_"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// TestRestoreTestEndToEnd restores a real backup into a temporary database, compares
// it with the manifest, drops it and leaves the source untouched; a manifest that
// does not match is reported, and a user without the privileges is refused.
func TestRestoreTestEndToEnd(t *testing.T) {
	env := requireMongo(t)
	for _, target := range storageTargets(t) {
		t.Run(target.Name, func(t *testing.T) {
			ctx := context.Background()
			db := seedIndexed(t, env, "rtest")
			f := newIntegrityFixture(t, env, target.Storage, uriConnections{uri: env.URI})
			job := &models.Job{ID: "job_" + db, Name: db, Database: db, CronExpression: "@daily", ConnectionID: "conn_it",
				StorageTargetID: itTargetID, RestoreTest: &models.RestoreTestPolicy{Enabled: true, Frequency: models.RestoreTestWeekly}}
			if err := f.meta.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			rec := manifestBackup(t, env, target.Storage, f.meta, db, job.ID)

			// The scheduler hook runs the due test synchronously.
			f.svc.AfterBackup(ctx, job, rec)
			tests, err := f.svc.ListRestoreTests(ctx, job.ID, 10)
			if err != nil || len(tests) != 1 {
				t.Fatalf("restore tests = %+v, %v", tests, err)
			}
			res := tests[0]
			if res.Status != models.RestoreTestOK || res.Collections != 2 || res.Documents != ordersCount+customersCount || !res.Dropped {
				t.Fatalf("restore test = %+v", res)
			}
			if !strings.HasPrefix(res.TempDatabase, db+"_rescue_verify_") {
				t.Fatalf("temporary database = %q", res.TempDatabase)
			}
			if left := rescueVerifyDatabases(t, env, db); len(left) != 0 {
				t.Fatalf("temporary databases left behind: %v", left)
			}
			if n := env.count(t, db, "orders"); n != ordersCount {
				t.Fatalf("the source changed: %d orders", n)
			}
			stored, _ := f.meta.GetJob(ctx, job.ID)
			if stored.LastRestoreTest == nil || stored.LastRestoreTest.Status != models.RestoreTestOK {
				t.Fatalf("job summary = %+v", stored.LastRestoreTest)
			}

			// A manifest that expects more documents: the comparison reports it.
			time.Sleep(1100 * time.Millisecond) // a new temporary name (per second)
			m, _ := f.meta.GetManifest(ctx, rec.ID)
			for i := range m.Collections {
				if m.Collections[i].Name == "orders" {
					m.Collections[i].DocumentsMin += 5
					m.Collections[i].DocumentsMax += 5
				}
			}
			if _, err := f.meta.UpdateBackupRecord(ctx, rec.ID, func(r *models.BackupRecord) error { r.Manifest = m; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := f.svc.StartRestoreTest(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
			f.waitIdle(t)
			tests, _ = f.svc.ListRestoreTests(ctx, job.ID, 10)
			if tests[0].Status != models.RestoreTestMismatch || len(tests[0].Mismatches) != 1 || !tests[0].Dropped {
				t.Fatalf("mismatch test = %+v", tests[0])
			}
			if left := rescueVerifyDatabases(t, env, db); len(left) != 0 {
				t.Fatalf("temporary databases left behind: %v", left)
			}
		})
	}

	t.Run("user without privileges", func(t *testing.T) {
		ctx := context.Background()
		db := seedIndexed(t, env, "rtpriv")
		local, err := storage.NewLocalStorage(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		user, password := "it_ro_"+randomHex(t, 4), randomHex(t, 12)
		if err := env.Client.Database(db).RunCommand(ctx, bson.D{{Key: "createUser", Value: user}, {Key: "pwd", Value: password},
			{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: db}}}}}).Err(); err != nil {
			t.Skipf("cannot create a test user (server without access control?): %v", err)
		}
		t.Cleanup(func() {
			_ = env.Client.Database(db).RunCommand(context.Background(), bson.D{{Key: "dropUser", Value: user}}).Err()
		})
		u, _ := url.Parse(env.URI)
		u.User = url.UserPassword(user, password)
		q := u.Query()
		q.Set("authSource", db)
		u.RawQuery = q.Encode()

		f := newIntegrityFixture(t, env, local, uriConnections{uri: env.URI, others: map[string]string{"conn_ro": u.String()}})
		job := &models.Job{ID: "job_" + db, Name: db, Database: db, CronExpression: "@daily", ConnectionID: "conn_it",
			RestoreTest: &models.RestoreTestPolicy{Enabled: true, Frequency: models.RestoreTestDaily, ConnectionID: "conn_ro"}}
		if err := f.meta.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		rec := manifestBackup(t, env, local, f.meta, db, job.ID)
		f.svc.AfterBackup(ctx, job, rec)
		tests, _ := f.svc.ListRestoreTests(ctx, job.ID, 10)
		if len(tests) != 1 || tests[0].Status != models.RestoreTestError || !strings.Contains(tests[0].Error, "privileges") {
			t.Fatalf("restore test with a read-only user = %+v", tests)
		}
		assertNoSecret(t, password, "restore test error", tests[0].Error)
		if left := rescueVerifyDatabases(t, env, db); len(left) != 0 {
			t.Fatalf("a refused test must create nothing: %v", left)
		}
	})
}

// TestOrphanImportEndToEnd stores an archive without a record, finds it with a
// storage scan, imports it and restores the imported backup, on every target.
func TestOrphanImportEndToEnd(t *testing.T) {
	env := requireMongo(t)
	for _, target := range storageTargets(t) {
		t.Run(target.Name, func(t *testing.T) {
			ctx := context.Background()
			db := seedSource(t, env, "orphan")
			f := newIntegrityFixture(t, env, target.Storage, uriConnections{uri: env.URI})
			// The archive exists, its record does not (lost metadata database).
			orphan := manifestBackup(t, env, target.Storage, nil, db, "")

			report, err := f.svc.ScanTarget(ctx, itTargetID, integrity.TriggerManual)
			if err != nil || report.Error != "" {
				t.Fatalf("scan = %+v, %v", report, err)
			}
			found := false
			for _, o := range report.Orphans {
				found = found || o.Key == orphan.StorageKey
			}
			if !found {
				t.Fatalf("orphan %s not reported among %d orphans", orphan.StorageKey, report.OrphanCount)
			}

			imported, err := f.svc.StartImport(ctx, itTargetID, orphan.StorageKey)
			if err != nil {
				t.Fatal(err)
			}
			f.waitIdle(t)
			rec, err := f.meta.GetBackupRecord(ctx, imported.ID)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Status != models.StatusCompleted || !rec.Imported || rec.SHA256 != orphan.SHA256 || rec.SizeBytes != orphan.SizeBytes ||
				rec.Database != db || rec.ID != orphan.ID {
				t.Fatalf("imported = %+v, original = %+v", rec, orphan)
			}
			restored := mustRestore(t, env, target.Storage, models.RestoreRequest{BackupID: rec.ID, MongoURI: env.URI}, rec)
			if n := env.count(t, restored.TargetDatabase, "orders"); n != ordersCount {
				t.Fatalf("restored %d orders from the imported backup", n)
			}
			report, _ = f.svc.ScanTarget(ctx, itTargetID, integrity.TriggerManual)
			for _, o := range report.Orphans {
				if o.Key == orphan.StorageKey {
					t.Fatalf("an imported archive is no orphan: %+v", o)
				}
			}
		})
	}
}

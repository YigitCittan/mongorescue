//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// itConnections serves the one connection of a PITR rig to the operations service.
type itConnections map[string]*models.Connection

func (c itConnections) Resolve(_ context.Context, id string) (*models.Connection, error) {
	if conn, ok := c[id]; ok {
		cp := *conn
		return &cp, nil
	}
	return nil, operations.ErrUnknownConnection
}

func (c itConnections) Get(ctx context.Context, id string) (*models.Connection, error) {
	return c.Resolve(ctx, id)
}

func (c itConnections) List(context.Context) ([]*models.Connection, error) { return nil, nil }

// pitrRig is a running collector on the replica set under test, the engines and the
// operations service that restores from it.
type pitrRig struct {
	env     *mongoEnv
	ctx     context.Context
	repo    *store.SQLiteStore
	prober  *mongoconn.Prober
	stream  *pitr.Stream
	backups *backup.Engine
	ops     *operations.Service
	// scheduled is ops for the collector's chain test schedule, set once built.
	scheduled atomic.Pointer[operations.Service]
}

// newPITRRig starts a collector with one-second chunks and waits for its chain.
// A chainTestCron schedules chain tests through the collector, as internal/app
// wires it.
func newPITRRig(t *testing.T, chainTestCron ...string) *pitrRig {
	t.Helper()
	env := requireMongo(t)
	requireReplicaSet(t, env)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

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
	r := &pitrRig{env: env, ctx: ctx, repo: storetest.New(t), prober: mongoconn.New()}
	win, err := r.prober.OplogWindow(ctx, env.URI)
	if err != nil {
		t.Fatal(err)
	}
	r.stream = &pitr.Stream{ID: "str_it", ConnectionID: "conn_it", ReplicaSet: win.ReplicaSet, TargetID: "tgt_it", Enabled: true,
		BaseCron: "@daily", BaseKeepCount: 7, BaseKeepDays: 14, ChunkSeconds: 1}
	if len(chainTestCron) > 0 {
		r.stream.ChainTestCron = chainTestCron[0]
	}
	if err = r.repo.CreateStream(ctx, r.stream); err != nil {
		t.Fatal(err)
	}
	col := collector.New(collector.Config{
		// Scheduled chain tests run as the application itself, once r.ops exists.
		StartChainTest: func(ctx context.Context, id string) error {
			svc := r.scheduled.Load()
			if svc == nil {
				return operations.ErrPITRUnavailable
			}
			_, chainErr := svc.StartChainTest(auth.WithPrincipal(ctx, auth.SystemPrincipal()), id)
			return chainErr
		},
		LastChainTest: func(ctx context.Context, id string) time.Time {
			svc := r.scheduled.Load()
			if svc == nil {
				return time.Time{}
			}
			return svc.LastChainTestStart(ctx, id)
		},
		NextRun: func(expr string, from time.Time) (time.Time, bool) {
			next := scheduler.NextRuns(expr, from, 1)
			if len(next) == 0 {
				return time.Time{}, false
			}
			return next[0], true
		},
		Repo: r.repo,
		Open: func(ctx context.Context, s *pitr.Stream) (collector.Session, error) {
			sess, openErr := r.prober.OpenOplogSession(ctx, env.URI, s.ReadPreference)
			if openErr != nil {
				return nil, openErr
			}
			return itSession{sess}, nil
		},
		Storage:           func(context.Context, string) (storage.Storage, error) { return st, nil },
		Encryptor:         func() *encryption.Encryptor { return enc },
		Logger:            slog.New(slog.DiscardHandler),
		ReconcileInterval: 100 * time.Millisecond,
	})
	col.Start(ctx)
	t.Cleanup(col.Stop)
	deadline := time.Now().Add(time.Minute)
	for {
		if _, loadErr := r.repo.LoadState(ctx, r.stream.ID); loadErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the collector did not start a chain")
		}
		time.Sleep(50 * time.Millisecond)
	}

	names := func(ctx context.Context, uri string) ([]string, error) {
		dbs, listErr := r.prober.ListDatabases(ctx, uri)
		out := make([]string, 0, len(dbs))
		for _, d := range dbs {
			out = append(out, d.Name)
		}
		return out, listErr
	}
	r.backups = newBackupEngine(env, st, backup.WithEncryptor(enc), backup.WithOpTimeReader(r.prober.WriteOpTimes),
		backup.WithManifestCapturer(r.prober.Manifest), backup.WithDatabaseLister(names))
	restores := newRestoreEngine(env, st, restore.WithDecryptor(dec), restore.WithDatabaseLister(names),
		restore.WithServerVersion(func(ctx context.Context, uri string) (string, error) {
			info, pingErr := r.prober.Ping(ctx, uri)
			return info.Version, pingErr
		}))
	manager := runs.NewManager(discardLogger)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	r.ops = operations.New(operations.Config{
		Store: r.repo, Backup: r.backups, Restore: restores, Runs: manager,
		Connections: itConnections{"conn_it": {ID: "conn_it", Name: "replica set", URI: env.URI}},
		PITR:        r.repo, PITRRestore: restores, PITRBases: r.repo.ListBaseBackups, Inspector: r.prober,
		ToolsVersion: func(ctx context.Context) (string, error) {
			return mongotools.NewResolver("").ToolVersion(ctx, "mongorestore")
		},
		Logger: discardLogger,
	})
	r.scheduled.Store(r.ops)
	return r
}

// base takes an instance-scope base backup and records it for the stream.
func (r *pitrRig) base(t *testing.T) *models.BackupRecord {
	t.Helper()
	rec, err := r.backups.Run(r.ctx, models.BackupOptions{
		Scope: models.ScopeInstance, ConnectionID: "conn_it", ReplicaSet: r.stream.ReplicaSet, PITRStreamID: r.stream.ID,
		MongoURI: r.env.URI, StorageTargetID: "tgt_it", Gzip: true,
	})
	if err != nil || rec.Status != models.StatusCompleted {
		t.Fatalf("base backup: %v (%+v)", err, rec)
	}
	if err = r.repo.SaveBackupRecord(r.ctx, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// waitCovered waits until the collector has stored the newest majority-committed
// oplog entry.
func (r *pitrRig) waitCovered(t *testing.T) {
	t.Helper()
	target := mustNewest(r.ctx, t, r.prober, r.env.URI)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if s, err := r.repo.LoadState(r.ctx, r.stream.ID); err == nil && s.Last.TS.Compare(target) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the collector did not pass %s", target)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// nextSecond waits until the server's clock has left the second of the newest oplog
// entry and returns that second: every write so far is restored by a target at it,
// and none made from now on.
func (r *pitrRig) nextSecond(t *testing.T) uint32 {
	t.Helper()
	sec := r.env.lastOplogTS(t).T
	for time.Now().Unix() <= int64(sec) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	return sec
}

// restore runs a point-in-time restore through the operations service, waits for
// it and drops its clones on cleanup.
func (r *pitrRig) restore(t *testing.T, req models.RestoreRequest) *models.RestoreRecord {
	t.Helper()
	admin := auth.WithPrincipal(r.ctx, auth.SystemPrincipal())
	rec, err := r.ops.StartRestore(admin, req)
	var pre *operations.PreflightError
	if errors.As(err, &pre) {
		t.Fatalf("preflight refused the restore: %+v", pre.Result.Checks)
	}
	if err != nil {
		t.Fatalf("StartRestore: %v", err)
	}
	t.Cleanup(func() { r.dropSuffix(t, rec.PITR.CloneSuffix) })
	deadline := time.Now().Add(5 * time.Minute)
	for {
		got, getErr := r.repo.GetRestoreRecord(r.ctx, rec.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Status != models.RestoreStatusInProgress && got.Status != models.RestoreStatusPending {
			if got.Status != models.RestoreStatusCompleted {
				t.Fatalf("restore %s: %s", got.Status, got.ErrorMessage)
			}
			assertNoSecret(t, r.env.Password, "restore record", got.ErrorMessage, got.Warning)
			return got
		}
		if time.Now().After(deadline) {
			t.Fatal("the restore did not finish")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// dropSuffix drops every database whose name ends in suffix.
func (r *pitrRig) dropSuffix(t *testing.T, suffix string) {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	for _, name := range r.databases(t) {
		if strings.HasSuffix(name, suffix) {
			_ = r.env.Client.Database(name).Drop(ctx)
		}
	}
}

// databases lists the database names of the server.
func (r *pitrRig) databases(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	names, err := r.env.Client.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// systemState captures what a restore must never touch: the collections of admin
// and config and the number of users.
func (r *pitrRig) systemState(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	var parts []string
	for _, db := range []string{"admin", "config"} {
		names, err := r.env.Client.Database(db).ListCollectionNames(ctx, bson.D{})
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(names)
		parts = append(parts, db+":"+strings.Join(names, ","))
	}
	users, err := r.env.Client.Database("admin").Collection("system.users").CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s;users=%d", strings.Join(parts, ";"), users)
}

// TestPITRRestoreToJustBeforeADrop takes a base, writes (including an index build
// and a rename), drops one database and keeps writing, then restores the whole
// instance and one database to the second before the drop into safe clones: the
// clones hold the state before the drop, while the source databases and admin,
// config and local stay untouched.
func TestPITRRestoreToJustBeforeADrop(t *testing.T) {
	r := newPITRRig(t)
	env, ctx := r.env, r.ctx
	dbA, dbB := env.uniqueDB(t, "pitra"), env.uniqueDB(t, "pitrb")
	env.seed(t, dbA, "orders", 40)
	env.seed(t, dbB, "items", 30)
	env.seed(t, dbB, "staging", 5)
	r.base(t)

	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	a, b := env.Client.Database(dbA), env.Client.Database(dbB)
	_, err := a.Collection("orders").InsertMany(ctx, []any{bson.D{{Key: "seq", Value: 100}}, bson.D{{Key: "seq", Value: 101}}})
	must("insert", err)
	_, err = b.Collection("items").UpdateMany(ctx, bson.D{{Key: "seq", Value: bson.D{{Key: "$lt", Value: 10}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "sold", Value: true}}}})
	must("update", err)
	_, err = b.Collection("items").DeleteOne(ctx, bson.D{{Key: "seq", Value: 20}})
	must("delete", err)
	// An index build and a rename in the window.
	_, err = b.Collection("staging").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "sku", Value: 1}}})
	must("index build", err)
	must("rename", env.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "renameCollection", Value: dbB + ".staging"}, {Key: "to", Value: dbB + ".final"}}).Err())
	_, err = a.Collection("orders").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "seq", Value: -1}}})
	must("index build on the dropped database", err)

	wantA, wantB := env.state(t, dbA), env.state(t, dbB)
	system := r.systemState(t)
	at := r.nextSecond(t)

	// The disaster, and life goes on.
	must("dropDatabase", a.Drop(ctx))
	if drop := env.lastOplogTS(t); drop.T <= at {
		t.Fatalf("the drop at %d.%d is in the restored second %d (clock skew)", drop.T, drop.I, at)
	}
	_, err = b.Collection("items").InsertOne(ctx, bson.D{{Key: "seq", Value: 999}, {Key: "after", Value: true}})
	must("write after the drop", err)
	r.waitCovered(t)
	srcB := env.state(t, dbB)

	target := time.Unix(int64(at), 0).UTC()
	whole := r.restore(t, models.RestoreRequest{PITR: &models.PITRTarget{StreamID: r.stream.ID, At: &target}})
	suffix := whole.PITR.CloneSuffix
	// Every clone created is on the record, which names nothing else.
	for _, name := range r.databases(t) {
		if strings.HasSuffix(name, suffix) && !slices.Contains(whole.PITR.Clones, name) {
			t.Errorf("clone %s is not recorded on the restore", name)
		}
	}
	for _, clone := range whole.PITR.Clones {
		if !strings.HasSuffix(clone, suffix) || models.IsRescueClone(strings.TrimSuffix(clone, suffix)) {
			t.Errorf("recorded clone %s", clone)
		}
	}
	if !slices.Contains(whole.PITR.Clones, dbA+suffix) || !slices.Contains(whole.PITR.Clones, dbB+suffix) {
		t.Errorf("recorded clones %v lack %s or %s", whole.PITR.Clones, dbA+suffix, dbB+suffix)
	}
	assertSameState(t, "clone of the dropped database", env.state(t, dbA+suffix), wantA)
	assertSameState(t, "clone of the other database", env.state(t, dbB+suffix), wantB)
	if whole.PITR.OpsApplied == nil || *whole.PITR.OpsApplied != whole.PITR.OpsReplayed {
		t.Errorf("mongorestore applied %v operations, the filter wrote %d", whole.PITR.OpsApplied, whole.PITR.OpsReplayed)
	}

	// The sources are untouched: the dropped database stays dropped, the other one
	// keeps the write made after the target.
	names := r.databases(t)
	if slices.Contains(names, dbA) {
		t.Error("the restore recreated the dropped source database")
	}
	assertSameState(t, "source database after the restore", env.state(t, dbB), srcB)
	for _, sys := range []string{"admin", "config", "local"} {
		if slices.Contains(names, sys+suffix) {
			t.Errorf("the restore cloned %s", sys)
		}
	}
	if got := r.systemState(t); got != system {
		t.Errorf("admin or config changed: %s, was %s", got, system)
	}

	// One database only.
	time.Sleep(time.Second) // a new clone suffix
	one := r.restore(t, models.RestoreRequest{PITR: &models.PITRTarget{StreamID: r.stream.ID, At: &target}, Databases: []string{dbA}})
	if one.TargetDatabase != dbA+one.PITR.CloneSuffix {
		t.Fatalf("target = %s", one.TargetDatabase)
	}
	assertSameState(t, "clone of one database", env.state(t, dbA+one.PITR.CloneSuffix), wantA)
	if slices.Contains(r.databases(t), dbB+one.PITR.CloneSuffix) {
		t.Error("a one-database restore cloned another database")
	}
	assertSameState(t, "source database after the second restore", env.state(t, dbB), srcB)
}

// TestPITRRestoreStopsInsideATransaction restores to the commit of a transaction
// split into several oplog entries: the limit falls inside the transaction, which
// must not be applied at all.
func TestPITRRestoreStopsInsideATransaction(t *testing.T) {
	r := newPITRRig(t)
	env, ctx := r.env, r.ctx
	db := env.uniqueDB(t, "pitrtx")
	env.seed(t, db, "orders", 10)
	r.base(t)
	coll := env.Client.Database(db).Collection("orders")
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "seq", Value: 50}, {Key: "before", Value: true}}); err != nil {
		t.Fatal(err)
	}
	want := env.state(t, db)
	from := env.lastOplogTS(t)

	big := strings.Repeat("x", 5<<20)
	sess, err := env.Client.StartSession()
	if err != nil {
		t.Fatal(err)
	}
	_, err = sess.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		for i := 0; i < 4; i++ {
			if _, ierr := coll.InsertOne(ctx, bson.D{{Key: "seq", Value: 1000 + i}, {Key: "pad", Value: big}}); ierr != nil {
				return nil, ierr
			}
		}
		return nil, nil
	})
	sess.EndSession(ctx)
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}
	// The transaction's entries: partialTxn ones, then the commit.
	var txn []pitr.Timestamp
	for _, e := range splitOplog(t, env.readOplog(t, from)) {
		if e.NS == "admin.$cmd" && e.Op == "c" {
			txn = append(txn, e.TS)
		}
	}
	if len(txn) < 2 {
		t.Fatalf("the transaction was not split (%d entries)", len(txn))
	}
	commit := txn[len(txn)-1]
	r.waitCovered(t)

	rec := r.restore(t, models.RestoreRequest{PITR: &models.PITRTarget{StreamID: r.stream.ID, TS: &commit}, Databases: []string{db}})
	assertSameState(t, "clone stopped inside the transaction", env.state(t, db+rec.PITR.CloneSuffix), want)
	if rec.PITR.OpsApplied == nil || *rec.PITR.OpsApplied != rec.PITR.OpsReplayed {
		t.Errorf("mongorestore applied %v operations, the filter counted %d", rec.PITR.OpsApplied, rec.PITR.OpsReplayed)
	}
}

// TestPITRChainTest takes two bases and lets the stream's chain_test_cron run a
// chain test: base one is restored to the consistent point of base two (every
// write up to and including its t_after), the clones match base two's manifest,
// and they go. The test's measured replay rate then drives the RTO estimate.
func TestPITRChainTest(t *testing.T) {
	r := newPITRRig(t, "* * * * *")
	env, ctx := r.env, r.ctx
	db := env.uniqueDB(t, "pitrct")
	env.seed(t, db, "orders", 20)
	first := r.base(t)
	if _, err := env.Client.Database(db).Collection("orders").InsertMany(ctx, []any{bson.D{{Key: "seq", Value: 500}}, bson.D{{Key: "seq", Value: 501}}}); err != nil {
		t.Fatal(err)
	}
	// A typical write-heavy oplog between the bases, for the measured replay rate:
	// 5,000 small inserts and an update of each.
	events := env.Client.Database(db).Collection("events")
	batch := make([]any, 0, 5000)
	for i := range 5000 {
		batch = append(batch, bson.D{{Key: "n", Value: int64(i)}, {Key: "status", Value: "new"}, {Key: "pad", Value: strings.Repeat("e", 200)}})
	}
	if _, err := events.InsertMany(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := events.UpdateMany(ctx, bson.D{}, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "done"}}}}); err != nil {
		t.Fatal(err)
	}
	second := r.base(t)
	if !second.HasManifest {
		t.Fatal("the base has no instance manifest")
	}
	r.waitCovered(t)
	// The schedule fires once a minute; a run before both bases were eligible
	// found nothing to test and waits for the next minute.
	deadline := time.Now().Add(4 * time.Minute)
	var res *operations.ChainTestResult
	for res == nil {
		if time.Now().After(deadline) {
			t.Fatal("no scheduled chain test finished")
		}
		time.Sleep(time.Second)
		res = r.ops.LastChainTest(ctx, r.stream.ID)
	}
	final, err := r.repo.GetRestoreRecord(ctx, res.RestoreID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed || res.Verification != models.RestoreVerificationPassed {
		t.Fatalf("chain test %+v: %s %s %+v", res, final.ErrorMessage, final.Warning, final.Verification)
	}
	limit := pitr.Timestamp{T: second.TAfter.TS.T, I: second.TAfter.TS.I + 1}
	if !final.PITR.ChainTest || final.PITR.BaseID != first.ID || final.PITR.Limit != limit {
		t.Fatalf("the chain test restored base %s up to %s; want base %s up to %s", final.PITR.BaseID, final.PITR.Limit, first.ID, limit)
	}
	if v := final.Verification; v.Collections == 0 || len(v.Mismatches) != 0 {
		t.Fatalf("the comparison with the manifest of %s: %+v", second.ID, v)
	}
	for _, name := range r.databases(t) {
		if strings.HasSuffix(name, final.PITR.CloneSuffix) {
			t.Errorf("the chain test left clone %s", name)
		}
	}

	// Measured RTO: both passes were timed, and the estimate uses them.
	perSec, bytesPerSec, ok := final.PITR.PITRReplayRate()
	if !ok || final.PITR.BaseSeconds <= 0 {
		t.Fatalf("the chain test did not time its passes: %+v", final.PITR)
	}
	t.Logf("chain test replay: %d entries, %d stored bytes in %.2fs (%.0f entries/s, %.2f MiB/s); base %d bytes in %.2fs",
		final.PITR.OpsReplayed, final.PITR.OplogBytes, final.PITR.ReplaySeconds, perSec, bytesPerSec/(1<<20),
		final.PITR.BaseBytes, final.PITR.BaseSeconds)
	if e := r.ops.EstimatePITR(ctx, r.stream.ID, 1<<30, 1<<30, 1_000_000); e.Source != models.PITREstimateMeasured || e.Samples != 1 {
		t.Fatalf("the estimate after a timed chain test: %+v", e)
	}
}

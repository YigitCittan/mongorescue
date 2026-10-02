//go:build integration

package integration

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// eventLog records published events.
type eventLog struct {
	mu     sync.Mutex
	events []events.Event
}

func (l *eventLog) Publish(_ context.Context, e events.Event) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return true
}

func (l *eventLog) of(t events.EventType) []events.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []events.Event
	for _, e := range l.events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

// TestRestorePreflightAndVerification backs up a database with its manifest and server
// version, preflights restores of it against the real server, restores it into safe
// clones with verification (whole and selective) and checks that a restore that does
// not match the manifest is flagged and announced, all through the operations service
// wired as internal/app wires it.
func TestRestorePreflightAndVerification(t *testing.T) {
	env := requireMongo(t)
	db := env.uniqueDB(t, "preflt")
	env.seed(t, db, "orders", 50)
	env.seed(t, db, "users", 5)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if _, err := env.Client.Database(db).Collection("orders").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "sku", Value: 1}}, Options: options.Index().SetUnique(true),
	}); err != nil {
		t.Fatalf("create index: %v", err)
	}

	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta := storetest.New(t)
	manager := runs.NewManager(discardLogger)
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), opTimeout)
		defer scancel()
		_ = manager.Shutdown(sctx)
	})
	prober := mongoconn.New()
	published := &eventLog{}
	conn := models.Connection{ID: "conn_it", Name: "integration", URI: env.URI}
	svc := operations.New(operations.Config{
		Store:       meta,
		Backup:      newBackupEngine(env, st, backup.WithManifestCapturer(prober.Manifest)),
		Restore:     newRestoreEngine(env, st),
		Runs:        manager,
		Connections: staticConnections{conn: conn},
		Inspector:   prober,
		Publisher:   published,
		Logger:      discardLogger,
	})
	admin := auth.WithPrincipal(context.Background(), auth.SystemPrincipal())

	started, err := svc.StartBackup(admin, operations.BackupRequest{BackupOptions: models.BackupOptions{ConnectionID: conn.ID, Database: db}})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	bkp := waitBackup(t, svc, started.ID)
	if bkp.Status != models.StatusCompleted || !bkp.HasManifest {
		t.Fatalf("backup = %s (%s), manifest %v", bkp.Status, bkp.ErrorMessage, bkp.HasManifest)
	}
	if _, ok := models.ParseServerVersion(bkp.ServerVersion); !ok {
		t.Fatalf("server_version = %q; want the source server's buildInfo version", bkp.ServerVersion)
	}
	waitIdle(t, manager)

	// A safe clone into the same server passes.
	pre, err := svc.PreflightRestore(admin, models.RestoreRequest{BackupID: bkp.ID})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !pre.OK {
		t.Fatalf("preflight of a safe clone = %+v", pre.Checks)
	}
	for _, id := range []string{models.PreflightCheckConnection, models.PreflightCheckServerVersion, models.PreflightCheckTargetDatabase, models.PreflightCheckCollections} {
		if c := pre.Check(id); c == nil || c.Status != models.PreflightPass {
			t.Errorf("check %s = %+v; want pass", id, c)
		}
	}
	assertNoSecret(t, env.Password, "preflight messages", checkMessages(pre)...)

	// An in-place restore with drop into the source lists what it replaces.
	f := false
	pre, err = svc.PreflightRestore(admin, models.RestoreRequest{BackupID: bkp.ID, SafeClone: &f, DropTarget: true})
	if err != nil {
		t.Fatalf("in-place preflight: %v", err)
	}
	if c := pre.Check(models.PreflightCheckCollections); c == nil || c.Status != models.PreflightWarn ||
		!strings.Contains(c.Message, "orders") || !strings.Contains(c.Message, "users") {
		t.Fatalf("collections check = %+v; want both collections listed", c)
	}

	// A restore with verification into a safe clone matches the manifest.
	nextSecond() // safe clone names have a resolution of one second
	rst, err := svc.StartRestore(admin, models.RestoreRequest{BackupID: bkp.ID, VerifyRestore: true})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	done := waitRestore(t, svc, rst.ID)
	waitIdle(t, manager)
	if done.Status != models.RestoreStatusCompleted || done.Preflight == nil || !done.Preflight.OK {
		t.Fatalf("restore = %s (%s), preflight %+v", done.Status, done.ErrorMessage, done.Preflight)
	}
	if v := done.Verification; v == nil || v.Status != models.RestoreVerificationPassed || v.Collections != 2 {
		t.Fatalf("verification = %+v; want passed for 2 collections", done.Verification)
	}

	// A selective restore is verified against the selection only.
	nextSecond() // safe clone names have a resolution of one second
	rst, err = svc.StartRestore(admin, models.RestoreRequest{BackupID: bkp.ID, VerifyRestore: true, SelectedCollections: []string{"users"}})
	if err != nil {
		t.Fatalf("selective restore: %v", err)
	}
	done = waitRestore(t, svc, rst.ID)
	waitIdle(t, manager)
	if v := done.Verification; done.Status != models.RestoreStatusCompleted || v == nil || v.Status != models.RestoreVerificationPassed || v.Collections != 1 {
		t.Fatalf("selective restore = %s, verification %+v; want passed for 1 collection", done.Status, done.Verification)
	}

	// A manifest that no longer matches (here: edited in the store) fails the
	// verification: the restore stays completed with a warning and an event.
	stored, err := meta.GetManifest(context.Background(), bkp.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range stored.Collections {
		if stored.Collections[i].Name == "orders" {
			stored.Collections[i].DocumentsMin, stored.Collections[i].DocumentsMax = 60, 60
		}
	}
	bkp.Manifest = stored
	if err = meta.SaveBackupRecord(context.Background(), bkp); err != nil {
		t.Fatal(err)
	}
	nextSecond() // safe clone names have a resolution of one second
	rst, err = svc.StartRestore(admin, models.RestoreRequest{BackupID: bkp.ID, VerifyRestore: true})
	if err != nil {
		t.Fatalf("restore against the edited manifest: %v", err)
	}
	done = waitRestore(t, svc, rst.ID)
	waitIdle(t, manager)
	v := done.Verification
	if done.Status != models.RestoreStatusCompleted || v == nil || v.Status != models.RestoreVerificationFailed ||
		!strings.Contains(strings.Join(v.Mismatches, "|"), "collection orders: 50 documents restored, 60 expected") {
		t.Fatalf("restore = %s, verification %+v; want a failed verification of orders", done.Status, v)
	}
	if !strings.Contains(done.Warning, "verification failed") {
		t.Fatalf("warning = %q; want the failed verification", done.Warning)
	}
	if got := published.of(events.RestoreVerificationFailed); len(got) != 1 || got[0].RestoreID != done.ID {
		t.Fatalf("restore.verification_failed events = %+v; want one for %s", got, done.ID)
	}
}

// nextSecond waits for the next wall-clock second, so a new safe clone gets a name of
// its own (the preflight refuses one that exists).
func nextSecond() {
	now := time.Now()
	time.Sleep(now.Truncate(time.Second).Add(time.Second).Sub(now) + 10*time.Millisecond)
}

// checkMessages returns the messages of p's checks.
func checkMessages(p *models.PreflightResult) []string {
	out := make([]string, 0, len(p.Checks))
	for _, c := range p.Checks {
		out = append(out, c.Message)
	}
	return out
}

package operations_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// fakePITR records the point-in-time restores it is asked to run.
type fakePITR struct {
	mu   sync.Mutex
	runs []restore.PITRRun
	reqs []models.RestoreRequest
	keys bool
	// dropped lists the clone suffixes dropped.
	dropped []string
	done    chan struct{}
}

func (f *fakePITR) CanDecryptMode(string) bool { return f.keys }

func (f *fakePITR) PreparePITR(req models.RestoreRequest, run restore.PITRRun) (*models.RestoreRecord, error) {
	return restore.NewEngine(nil, "mongodb://x").PreparePITR(req, run)
}

func (f *fakePITR) DropPITRClones(_ context.Context, _ string, info *models.PITRRestore) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, info.CloneSuffix)
	return "; dropped"
}

func (f *fakePITR) ExecutePITR(_ context.Context, req models.RestoreRequest, run restore.PITRRun, rec *models.RestoreRecord) (*models.RestoreRecord, error) {
	f.mu.Lock()
	f.runs = append(f.runs, run)
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	rec.Status = models.RestoreStatusCompleted
	rec.DurationSeconds = 10
	defer close(f.done)
	return rec, nil
}

// pitrService returns a service with stream str_a on conn_a: one chain of chunks
// (100, 110], (110, 120], (120, 130] and base b1 consistent at 108.
func pitrService(t *testing.T, inspector operations.RestoreInspector, bases ...*models.BackupRecord) (*operations.Service, *fakePITR) {
	t.Helper()
	return pitrServiceFor(t, &models.Connection{ID: "conn_a", Name: "rs", URI: "mongodb://u:pw@db.internal/?replicaSet=rs0"}, inspector, bases...)
}

// pitrServiceFor is pitrService with conn as the stream's connection conn_a.
func pitrServiceFor(t *testing.T, conn *models.Connection, inspector operations.RestoreInspector, bases ...*models.BackupRecord) (*operations.Service, *fakePITR) {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	if err := st.CreateStream(ctx, &pitr.Stream{ID: "str_a", ConnectionID: "conn_a", ReplicaSet: "rs0", TargetID: "tgt",
		BaseCron: "@daily", BaseKeepCount: 7, BaseKeepDays: 14, ChunkSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	ts := func(s uint32) pitr.Timestamp { return pitr.Timestamp{T: s, I: 1} }
	if err := st.StartChain(ctx, "str_a", "ch1", pitr.OpTime{TS: ts(100), Term: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, to := range []uint32{110, 120, 130} {
		if err := st.CommitChunk(ctx, &pitr.Chunk{ID: fmt.Sprintf("k%d", to), StreamID: "str_a", ChainID: "ch1", TargetID: "tgt",
			StorageKey: fmt.Sprintf("_mongorescue/oplog/conn_a/rs0/ch1/%d.bson.gz.age", to), From: ts(to - 10), To: ts(to),
			FirstTerm: 1, LastTerm: 1, SizeBytes: 100, SHA256: "ab", Encrypted: true, EncryptionMode: "x25519"}); err != nil {
			t.Fatal(err)
		}
	}
	before, after := pitr.OpTime{TS: ts(105), Term: 1}, pitr.OpTime{TS: ts(108), Term: 1}
	if err := st.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b1", Scope: models.ScopeInstance, PITRStreamID: "str_a",
		ConnectionID: "conn_a", StorageTargetID: "tgt", StorageKey: "_mongorescue/base/conn_a/rs0/2026/10/b1.archive.gz.age",
		Status: models.StatusCompleted, StartedAt: time.Unix(104, 0), SizeBytes: 1000, Encrypted: true, EncryptionMode: "x25519",
		ServerVersion: "8.0.4", TBefore: &before, TAfter: &after, InstanceDatabases: b1Databases}); err != nil {
		t.Fatal(err)
	}
	for _, b := range bases {
		if err := st.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	mock := storage.NewMockStorage()
	fake := &fakePITR{keys: true, done: make(chan struct{})}
	svc := operations.New(operations.Config{
		Store:       st,
		Backup:      backup.NewEngine(mock, ""),
		Restore:     restore.NewEngine(mock, ""),
		Runs:        manager,
		Connections: fakeConnections{"conn_a": conn},
		PITR:        st,
		PITRRestore: fake,
		PITRBases:   st.ListBaseBackups,
		Inspector:   inspector,
	})
	pitrStores[svc] = st
	return svc, fake
}

// pitrStores are the stores of the services pitrService built.
var pitrStores = map[*operations.Service]*store.SQLiteStore{}

// svcStore returns the store of a service pitrService built.
func svcStore(t *testing.T, svc *operations.Service) *store.SQLiteStore {
	t.Helper()
	return pitrStores[svc]
}

// b1Databases are the databases of base b1; a test may change it before
// pitrService. The clone of an earlier restore is never planned again.
var b1Databases = []string{"crm", "shop", "shop_rescue_20261001_000000_abcd"}

func pitrAt(sec int64, dbs ...string) models.RestoreRequest {
	at := time.Unix(sec, 0).UTC()
	return models.RestoreRequest{PITR: &models.PITRTarget{StreamID: "str_a", At: &at}, Databases: dbs}
}

func TestStartPITRRestore(t *testing.T) {
	svc, fake := pitrService(t, nil)
	rec, err := svc.StartRestore(admin(), pitrAt(125, "shop"))
	if err != nil {
		t.Fatalf("StartRestore: %v", err)
	}
	if rec.PITR == nil || rec.PITR.BaseID != "b1" || rec.PITR.Chunks != 3 || rec.PITR.StreamID != "str_a" ||
		rec.SourceConnectionID != "conn_a" || rec.TargetConnectionID != "conn_a" || !strings.HasPrefix(rec.TargetDatabase, "shop_rescue_") {
		t.Fatalf("record = %+v, pitr = %+v", rec, rec.PITR)
	}
	select {
	case <-fake.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the restore did not run")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if run := fake.runs[0]; run.Base == nil || run.Base.ID != "b1" || run.Plan.Limit != (pitr.Timestamp{T: 126}) {
		t.Fatalf("run = %+v", run)
	}
	if fake.reqs[0].MongoURI == "" {
		t.Fatal("the target URI was not resolved")
	}
}

func TestStartPITRRestoreByConnectionID(t *testing.T) {
	svc, _ := pitrService(t, nil)
	req := pitrAt(125)
	req.PITR.StreamID = "conn_a"
	rec, err := svc.StartRestore(admin(), req)
	if err != nil || rec.PITR.StreamID != "str_a" || !strings.HasPrefix(rec.TargetDatabase, "*_rescue_") {
		t.Fatalf("rec = %+v, err = %v", rec, err)
	}
}

func TestPITRRestoreRefusals(t *testing.T) {
	svc, fake := pitrService(t, nil)
	inPlace := pitrAt(125)
	no := false
	inPlace.SafeClone, inPlace.ConfirmInPlace = &no, true
	elsewhere := pitrAt(125)
	elsewhere.TargetConnectionID = "conn_b"
	for name, c := range map[string]struct {
		ctx  context.Context
		req  models.RestoreRequest
		want error
	}{
		"reader":                       {readerCtx(), pitrAt(125), auth.ErrForbidden},
		"operator into another server": {operator(), elsewhere, auth.ErrForbidden},
		"operator in place":            {operator(), inPlace, models.ErrPITRInPlace},
		"in place":                     {admin(), inPlace, models.ErrPITRInPlace},
		"before the base":              {admin(), pitrAt(107), operations.ErrPITRNotRestorable},
		"after the window":             {admin(), pitrAt(130), operations.ErrPITRNotRestorable},
		"unknown stream":               {admin(), models.RestoreRequest{PITR: &models.PITRTarget{StreamID: "nope", At: pitrAt(1).PITR.At}}, operations.ErrNotFound},
		"system database":              {admin(), pitrAt(125, "admin"), operations.ErrInvalid},
		"databases alone":              {admin(), models.RestoreRequest{BackupID: "b1", Databases: []string{"shop"}}, operations.ErrInvalid},
	} {
		_, err := svc.StartRestore(c.ctx, c.req)
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	// Design decision 8: an operator restores into safe clones of the stream's
	// connection.
	if rec, err := svc.StartRestore(operator(), pitrAt(125)); err != nil || rec.PITR == nil || rec.TargetConnectionID != "conn_a" {
		t.Errorf("operator: %+v, %v; want a point-in-time restore into conn_a", rec, err)
	}
	fake.keys = false
	if _, err := svc.StartRestore(admin(), pitrAt(125)); !errors.Is(err, operations.ErrKeyRequired) {
		t.Errorf("no key: err = %v, want ErrKeyRequired", err)
	}
}

// TestPreflightPITRUsesTheConnectionTLS checks that the preflight of a
// point-in-time restore into a TLS-only server with a custom CA opens the target
// with the connection's TLS material and a URI that asks for TLS, so it never
// connects in plain text.
func TestPreflightPITRUsesTheConnectionTLS(t *testing.T) {
	material := models.ConnectionTLS{CAPEM: "-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n"}
	conn := &models.Connection{ID: "conn_a", Name: "rs", URI: "mongodb://u:pw@db.internal/?replicaSet=rs0&tls=true", ConnectionTLS: material}
	inspector := &fakeInspector{version: "8.0.4", free: 1 << 40, freeKnown: true, freeSource: connections.DiskSpaceDBStats}
	svc, _ := pitrServiceFor(t, conn, inspector)
	res, err := svc.PreflightRestore(admin(), pitrAt(125))
	if err != nil {
		t.Fatal(err)
	}
	if c := res.Check(models.PreflightCheckConnection); c == nil || c.Status != models.PreflightPass {
		t.Fatalf("connection check = %+v", c)
	}
	inspector.mu.Lock()
	defer inspector.mu.Unlock()
	if inspector.opened == 0 || inspector.openedTLS == nil || inspector.openedTLS.CAPEM != material.CAPEM {
		t.Fatalf("OpenTarget got TLS %+v (opened %d)", inspector.openedTLS, inspector.opened)
	}
	if v, ok := mongotools.OptionValue(inspector.openedURI, "tls"); !ok || v != "true" {
		t.Fatalf("OpenTarget got a URI without tls=true")
	}
}

func TestPreflightPITR(t *testing.T) {
	svc, fake := pitrService(t, nil)
	res, err := svc.PreflightRestore(admin(), pitrAt(125))
	if err != nil {
		t.Fatal(err)
	}
	chain := res.Check(models.PreflightCheckPITRChain)
	if chain == nil || chain.Status != models.PreflightPass || res.PITR == nil || res.PITR.BaseID != "b1" ||
		res.PITR.Chunks != 3 || res.PITR.OplogBytes != 300 || res.PITR.EstimatedSeconds <= 0 || res.PITR.EstimateFrom != "default" {
		t.Fatalf("result = %+v (pitr %+v)", res, res.PITR)
	}
	if res.Check(models.PreflightCheckToolsVersion) == nil {
		t.Fatal("no tools version check")
	}
	// A target no plan reaches fails a check instead of the call.
	res, err = svc.PreflightRestore(admin(), pitrAt(131))
	if err != nil {
		t.Fatal(err)
	}
	if c := res.Check(models.PreflightCheckPITRChain); c == nil || c.Status != models.PreflightFail || res.OK || res.PITR != nil {
		t.Fatalf("result = %+v", res)
	}
	if len(fake.runs) != 0 {
		t.Fatal("a preflight started a restore")
	}
	if res, err := svc.PreflightRestore(operator(), pitrAt(125)); err != nil || res.PITR == nil {
		t.Fatalf("an operator's point-in-time preflight: %+v, %v", res, err)
	}
	if _, err := svc.PreflightRestore(readerCtx(), pitrAt(125)); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("a reader's point-in-time preflight: %v; want ErrForbidden", err)
	}
}

// readerCtx is a context with a read-scope principal.
func readerCtx() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodAPIKey, APIKeyID: "key_read", Scope: auth.ScopeRead})
}

package operations_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// fakeInspector is a RestoreInspector with canned answers; its target is itself.
type fakeInspector struct {
	mu          sync.Mutex
	pingErr     error
	version     string
	exists      map[string]bool
	collections []connections.Collection
	missing     []string
	uncertain   bool
	privErr     error
	privColls   []string
	free        int64
	freeKnown   bool
	freeSource  string
	manifest    *models.Manifest
	manifestErr error
	inspected   []string
	opened      int
	closed      int
	deadline    time.Duration
}

func (f *fakeInspector) OpenTarget(ctx context.Context, _ string) (connections.RestoreTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened++
	if d, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(d)
	}
	return f, nil
}

func (f *fakeInspector) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
}

func (f *fakeInspector) Ping(context.Context) (connections.ServerInfo, error) {
	if f.pingErr != nil {
		return connections.ServerInfo{}, f.pingErr
	}
	return connections.ServerInfo{Version: f.version}, nil
}

func (f *fakeInspector) DatabaseExists(_ context.Context, database string) (bool, error) {
	return f.exists[database], nil
}

func (f *fakeInspector) ListCollections(context.Context, string) ([]connections.Collection, error) {
	return f.collections, nil
}

func (f *fakeInspector) Privileges(_ context.Context, _ string, actions, collections []string) (connections.PrivilegeReport, error) {
	f.mu.Lock()
	f.privColls = collections
	f.mu.Unlock()
	var out []string
	for _, a := range f.missing {
		for _, want := range actions {
			if a == want {
				out = append(out, a)
			}
		}
	}
	return connections.PrivilegeReport{Missing: out, Certain: !f.uncertain}, f.privErr
}

func (f *fakeInspector) FreeSpace(context.Context, string) (connections.DiskSpace, error) {
	source := f.freeSource
	if source == "" {
		source = connections.DiskSpaceDBStats
	}
	return connections.DiskSpace{Known: f.freeKnown, Free: f.free, Source: source}, nil
}

func (f *fakeInspector) Manifest(_ context.Context, _, database string) (*models.Manifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspected = append(f.inspected, database)
	return f.manifest, f.manifestErr
}

// healthyInspector answers like a reachable MongoDB 7.0 with room to spare.
func healthyInspector() *fakeInspector {
	return &fakeInspector{version: "7.0.14", exists: map[string]bool{}, free: 1 << 30, freeKnown: true}
}

// completingEngine prepares restores like the real engine and completes them at once.
type completingEngine struct {
	prep       *restore.Engine
	canDecrypt bool
	mu         sync.Mutex
	executed   int
}

func (e *completingEngine) CanDecrypt() bool { return e.canDecrypt }

func (e *completingEngine) Prepare(req models.RestoreRequest, src *models.BackupRecord) (*models.RestoreRecord, error) {
	return e.prep.Prepare(req, src)
}

func (e *completingEngine) Execute(_ context.Context, _ models.RestoreRequest, _ *models.BackupRecord, rec *models.RestoreRecord) (*models.RestoreRecord, error) {
	e.mu.Lock()
	e.executed++
	e.mu.Unlock()
	now := time.Now().UTC()
	rec.Status, rec.CompletedAt = models.RestoreStatusCompleted, &now
	return rec, nil
}

func (e *completingEngine) runs() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.executed
}

// preflightEnv is a service with one connection, one backup and a fake inspector.
type preflightEnv struct {
	svc       *operations.Service
	st        *store.SQLiteStore
	runs      *runs.Manager
	ins       *fakeInspector
	engine    *completingEngine
	publisher *recordingPublisher
	backup    *models.BackupRecord
}

// backupManifest is the manifest of the env's backup.
func backupManifest() *models.Manifest {
	return &models.Manifest{Collections: []models.CollectionManifest{
		{Name: "orders", DocumentsMin: 10, DocumentsMax: 10, Indexes: []models.IndexSpec{{Name: "_id_", Keys: "_id:1"}, {Name: "sku_1", Keys: "sku:1", Unique: true}}},
		{Name: "users", DocumentsMin: 3, DocumentsMax: 4, Indexes: []models.IndexSpec{{Name: "_id_", Keys: "_id:1"}}},
	}}
}

func newPreflightEnv(t *testing.T, ins *fakeInspector, mutate func(*models.BackupRecord)) *preflightEnv {
	t.Helper()
	st := storetest.New(t)
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	env := &preflightEnv{st: st, runs: manager, ins: ins, engine: &completingEngine{prep: restore.NewEngine(nil, "")}, publisher: &recordingPublisher{}}
	cfg := operations.Config{
		Store:       st,
		Backup:      backup.NewEngine(storage.NewMockStorage(), ""),
		Restore:     env.engine,
		Runs:        manager,
		Connections: fakeConnections{"conn_a": {ID: "conn_a", Name: "prod", URI: "mongodb://u:secret@db.internal:27017"}},
		Publisher:   env.publisher,
	}
	if ins != nil {
		cfg.Inspector = ins
	}
	env.svc = operations.New(cfg)
	env.backup = &models.BackupRecord{
		ID: "bkp_shop", Database: "shop", ConnectionID: "conn_a", Status: models.StatusCompleted, StartedAt: time.Now().UTC(),
		StorageKey: "shop/2026/10/bkp_shop.archive.gz", SizeBytes: 1000, SHA256: "abc", ServerVersion: "7.0.12",
		Manifest: backupManifest(), HasManifest: true,
	}
	if mutate != nil {
		mutate(env.backup)
	}
	if err := st.SaveBackupRecord(context.Background(), env.backup); err != nil {
		t.Fatal(err)
	}
	return env
}

func inPlace(req models.RestoreRequest) models.RestoreRequest {
	f := false
	req.SafeClone, req.ConfirmInPlace = &f, true
	return req
}

func TestPreflightCheckMatrix(t *testing.T) {
	type want map[string]models.PreflightStatus
	cases := []struct {
		name   string
		ins    func(*fakeInspector)
		backup func(*models.BackupRecord)
		req    func(models.RestoreRequest) models.RestoreRequest
		key    bool
		ok     bool
		want   want
		say    map[string]string
	}{
		{
			name: "a safe clone that passes everything",
			ok:   true,
			want: want{"connection": "pass", "encryption": "pass", "server_version": "pass", "target_database": "pass",
				"privileges": "pass", "disk_space": "pass", "collections": "pass", "users_and_roles": "pass"},
			say: map[string]string{"target_database": "shop_rescue_", "server_version": "7.0.12"},
		},
		{
			name: "an unreachable target fails and skips the server checks",
			ins: func(f *fakeInspector) {
				f.pingErr = errors.New("server selection timeout for mongodb://u:secret@db.internal")
			},
			want: want{"connection": "fail", "server_version": "warn", "target_database": "warn", "privileges": "warn", "disk_space": "warn"},
			say:  map[string]string{"server_version": "did not answer"},
		},
		{
			name: "a target of an older major version only warns",
			ins:  func(f *fakeInspector) { f.version = "6.0.5" },
			ok:   true,
			want: want{"server_version": "warn"},
			say:  map[string]string{"server_version": "may still work"},
		},
		{
			name: "a target of a newer major version warns",
			ins:  func(f *fakeInspector) { f.version = "8.0.1" },
			ok:   true,
			want: want{"server_version": "warn"},
		},
		{
			name:   "a backup without a recorded version warns unknown",
			backup: func(b *models.BackupRecord) { b.ServerVersion = "" },
			ok:     true,
			want:   want{"server_version": "warn"},
			say:    map[string]string{"server_version": "unknown"},
		},
		{
			name: "a taken clone name fails",
			ins: func(f *fakeInspector) {
				f.exists = map[string]bool{models.RescueDatabaseName("shop", time.Now()): true, models.RescueDatabaseName("shop", time.Now().Add(time.Second)): true}
			},
			want: want{"target_database": "fail"},
		},
		{
			name: "missing write privileges fail",
			ins:  func(f *fakeInspector) { f.missing = []string{"createIndex", "insert"} },
			want: want{"privileges": "fail"},
			say:  map[string]string{"privileges": "createIndex, insert"},
		},
		{
			name: "privileges missing under a custom role only warn",
			ins:  func(f *fakeInspector) { f.missing, f.uncertain = []string{"insert"}, true },
			ok:   true,
			want: want{"privileges": "warn"},
			say:  map[string]string{"privileges": "custom roles"},
		},
		{
			name: "unreadable privileges warn",
			ins:  func(f *fakeInspector) { f.privErr = errors.New("connectionStatus: boom") },
			ok:   true,
			want: want{"privileges": "warn"},
		},
		{
			name: "too little free space fails",
			ins:  func(f *fakeInspector) { f.free = 500 },
			want: want{"disk_space": "fail"},
		},
		{
			name: "too little local free space only warns",
			ins:  func(f *fakeInspector) { f.free, f.freeSource = 500, connections.DiskSpaceLocal },
			ok:   true,
			want: want{"disk_space": "warn"},
			say:  map[string]string{"disk_space": "loopback"},
		},
		{
			name: "too little free space for an in-place restore with drop only warns",
			ins:  func(f *fakeInspector) { f.free = 500 },
			req: func(r models.RestoreRequest) models.RestoreRequest {
				r = inPlace(r)
				r.DropTarget = true
				return r
			},
			ok:   true,
			want: want{"disk_space": "warn"},
			say:  map[string]string{"disk_space": "drop_target frees"},
		},
		{
			name: "little headroom warns",
			ins:  func(f *fakeInspector) { f.free = 2000 },
			ok:   true,
			want: want{"disk_space": "warn"},
		},
		{
			name: "unknown free space warns",
			ins:  func(f *fakeInspector) { f.freeKnown = false },
			ok:   true,
			want: want{"disk_space": "warn"},
			say:  map[string]string{"disk_space": "unknown"},
		},
		{
			name: "an in-place restore lists the collections it drops and replaces",
			ins: func(f *fakeInspector) {
				f.exists = map[string]bool{"shop": true}
				f.collections = []connections.Collection{{Name: "orders"}, {Name: "audit"}, {Name: "system.views"}}
			},
			req: func(r models.RestoreRequest) models.RestoreRequest {
				r = inPlace(r)
				r.DropTarget = true
				return r
			},
			ok:   true,
			want: want{"target_database": "pass", "collections": "warn"},
			say:  map[string]string{"collections": "dropped and replaced: orders"},
		},
		{
			name: "an in-place restore of other collections replaces nothing",
			ins: func(f *fakeInspector) {
				f.exists = map[string]bool{"shop": true}
				f.collections = []connections.Collection{{Name: "audit"}}
			},
			req:  inPlace,
			ok:   true,
			want: want{"collections": "pass"},
		},
		{
			name: "a selective in-place restore only counts the selection",
			ins: func(f *fakeInspector) {
				f.collections = []connections.Collection{{Name: "orders"}, {Name: "users"}}
			},
			req: func(r models.RestoreRequest) models.RestoreRequest {
				r = inPlace(r)
				r.SelectedCollections = []string{"users"}
				return r
			},
			ok:   true,
			want: want{"collections": "warn"},
			say:  map[string]string{"collections": "1 existing collection(s) of shop receive the backup's documents without being dropped"},
		},
		{
			name:   "users and roles from a backup without them fail",
			backup: func(b *models.BackupRecord) { b.UsersAndRoles = false },
			req: func(r models.RestoreRequest) models.RestoreRequest {
				r = inPlace(r)
				r.RestoreUsersAndRoles = true
				return r
			},
			want: want{"users_and_roles": "fail"},
		},
		{
			name:   "users and roles that can be restored warn",
			backup: func(b *models.BackupRecord) { b.UsersAndRoles = true },
			ins:    func(f *fakeInspector) { f.missing = []string{"createUser"} },
			req: func(r models.RestoreRequest) models.RestoreRequest {
				r = inPlace(r)
				r.RestoreUsersAndRoles = true
				return r
			},
			ok:   true,
			want: want{"users_and_roles": "warn", "privileges": "warn"},
		},
		{
			name:   "an encrypted backup without a key fails",
			backup: func(b *models.BackupRecord) { b.Encrypted, b.StorageKey = true, b.StorageKey+".age" },
			want:   want{"encryption": "fail"},
		},
		{
			name:   "an encrypted backup with a key passes",
			backup: func(b *models.BackupRecord) { b.Encrypted, b.StorageKey = true, b.StorageKey+".age" },
			key:    true,
			ok:     true,
			want:   want{"encryption": "pass"},
		},
		{
			name: "a dry run needs neither privileges nor space",
			ins:  func(f *fakeInspector) { f.missing, f.free = []string{"insert"}, 1 },
			req: func(r models.RestoreRequest) models.RestoreRequest {
				r.DryRun = true
				return r
			},
			ok:   true,
			want: want{"privileges": "pass", "disk_space": "pass"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ins := healthyInspector()
			if tc.ins != nil {
				tc.ins(ins)
			}
			env := newPreflightEnv(t, ins, tc.backup)
			env.engine.canDecrypt = tc.key
			req := models.RestoreRequest{BackupID: env.backup.ID}
			if tc.req != nil {
				req = tc.req(req)
			}
			res, err := env.svc.PreflightRestore(admin(), req)
			if err != nil {
				t.Fatal(err)
			}
			if res.OK != tc.ok {
				t.Errorf("ok = %v; want %v: %+v", res.OK, tc.ok, res.Checks)
			}
			if len(res.Checks) != 8 {
				t.Errorf("%d checks; want all 8: %+v", len(res.Checks), res.Checks)
			}
			for id, status := range tc.want {
				c := res.Check(id)
				if c == nil || c.Status != status {
					t.Errorf("check %s = %+v; want %s", id, c, status)
				}
			}
			for id, text := range tc.say {
				if c := res.Check(id); c == nil || !strings.Contains(c.Message, text) {
					t.Errorf("check %s = %+v; want it to mention %q", id, c, text)
				}
			}
			for _, c := range res.Checks {
				if strings.Contains(c.Message, "secret") {
					t.Errorf("check %s leaks the password: %q", c.ID, c.Message)
				}
			}
		})
	}
}

func TestPreflightWithoutAnInspectorOnlyWarns(t *testing.T) {
	env := newPreflightEnv(t, nil, nil)
	res, err := env.svc.PreflightRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Check(models.PreflightCheckConnection).Status != models.PreflightWarn {
		t.Fatalf("preflight = %+v; want only warnings", res)
	}
}

func TestPreflightAppliesTheRestoreScopeRules(t *testing.T) {
	env := newPreflightEnv(t, healthyInspector(), nil)
	operator := operator()
	f := false
	// In place, even without confirm_in_place, needs admin.
	if _, err := env.svc.PreflightRestore(operator, models.RestoreRequest{BackupID: env.backup.ID, SafeClone: &f}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("operator in-place preflight: %v; want ErrForbidden", err)
	}
	if _, err := env.svc.PreflightRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID, SafeClone: &f}); err != nil {
		t.Fatalf("admin in-place preflight without confirmation: %v", err)
	}
	if _, err := env.svc.PreflightRestore(operator, models.RestoreRequest{BackupID: env.backup.ID}); err != nil {
		t.Fatalf("operator safe-clone preflight: %v", err)
	}
	if _, err := env.svc.PreflightRestore(operator, models.RestoreRequest{BackupID: "bkp_missing"}); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("missing backup: %v; want ErrNotFound", err)
	}
}

func TestStartRestoreIsRefusedByAFailedPreflight(t *testing.T) {
	ins := healthyInspector()
	ins.free = 10 // less than the 1000-byte archive
	env := newPreflightEnv(t, ins, nil)
	_, err := env.svc.StartRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID})
	var pe *operations.PreflightError
	if !errors.Is(err, operations.ErrPreflightFailed) || !errors.As(err, &pe) {
		t.Fatalf("StartRestore = %v; want a PreflightError", err)
	}
	if pe.Result.OK || pe.Result.Check(models.PreflightCheckDiskSpace).Status != models.PreflightFail || !strings.Contains(err.Error(), "disk_space") {
		t.Fatalf("preflight = %+v (%v)", pe.Result, err)
	}
	if list, _ := env.svc.ListRestores(context.Background()); len(list) != 0 || env.engine.runs() != 0 {
		t.Fatalf("a refused restore left %d record(s) and %d run(s)", len(list), env.engine.runs())
	}

	// force starts it anyway and records why.
	rec, err := env.svc.StartRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID, Force: true})
	if err != nil {
		t.Fatalf("forced restore: %v", err)
	}
	done := waitRestoreDone(t, env.svc, rec.ID)
	if done.Status != models.RestoreStatusCompleted || !done.Forced || done.Preflight == nil || done.Preflight.OK {
		t.Fatalf("forced restore = %+v", done)
	}
	// The record is completed a moment before the run releases its lock.
	waitIdle(t, env.runs)

	// A dry run is never refused.
	dry, err := env.svc.StartRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	waitRestoreDone(t, env.svc, dry.ID)
}

func TestDryRunTakesNoTargetLock(t *testing.T) {
	env := newPreflightEnv(t, healthyInspector(), nil)
	f := false
	req := models.RestoreRequest{BackupID: env.backup.ID, SafeClone: &f, ConfirmInPlace: true}
	release, err := env.runs.Acquire(runs.RestoreKey("conn_a", "shop"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// A restore into a database that is being restored is refused; a dry run is not.
	if _, err = env.svc.StartRestore(admin(), req); !errors.Is(err, operations.ErrBusy) {
		t.Fatalf("restore while the target is locked: %v; want ErrBusy", err)
	}
	req.DryRun = true
	dry, err := env.svc.StartRestore(admin(), req)
	if err != nil {
		t.Fatalf("dry run while the target is locked: %v", err)
	}
	if done := waitRestoreDone(t, env.svc, dry.ID); done.Status != models.RestoreStatusCompleted {
		t.Fatalf("dry run = %+v", done)
	}
}

// waitIdle waits until m runs nothing.
func waitIdle(t *testing.T, m *runs.Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(m.Active()) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("runs still active: %v", m.Active())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitVerificationEvents polls for want restore.verification_failed events.
func (p *recordingPublisher) awaitVerificationEvents(t *testing.T, want int) []events.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := p.verificationEvents()
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStartRestoreRecordsAPassingPreflight(t *testing.T) {
	ins := healthyInspector()
	ins.version = "8.0.0" // a warning never blocks
	env := newPreflightEnv(t, ins, nil)
	rec, err := env.svc.StartRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Preflight == nil || !rec.Preflight.OK || rec.Forced || len(rec.Preflight.Warnings()) != 1 {
		t.Fatalf("preflight on the record = %+v, forced %v", rec.Preflight, rec.Forced)
	}
}

// waitRestoreDone polls GetRestore until the restore left in_progress.
func waitRestoreDone(t *testing.T, svc *operations.Service, id string) *models.RestoreRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, err := svc.GetRestore(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != models.RestoreStatusInProgress {
			return rec
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("restore %s did not finish", id)
	return nil
}

// verificationEvents returns the restore.verification_failed events published.
func (p *recordingPublisher) verificationEvents() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []events.Event
	for _, e := range p.events {
		if e.Type == events.RestoreVerificationFailed {
			out = append(out, e)
		}
	}
	return out
}

func TestRestoreVerification(t *testing.T) {
	cases := []struct {
		name     string
		actual   *models.Manifest
		inspErr  error
		backup   func(*models.BackupRecord)
		req      func(models.RestoreRequest) models.RestoreRequest
		status   models.RestoreVerificationStatus
		mismatch string
		note     string
		warning  string
	}{
		{name: "a faithful copy passes", actual: backupManifest(), status: models.RestoreVerificationPassed},
		{
			name: "a missing document and index fail",
			actual: &models.Manifest{Collections: []models.CollectionManifest{
				{Name: "orders", DocumentsMin: 9, DocumentsMax: 9, Indexes: []models.IndexSpec{{Name: "_id_", Keys: "_id:1"}}},
				{Name: "users", DocumentsMin: 4, DocumentsMax: 4, Indexes: []models.IndexSpec{{Name: "_id_", Keys: "_id:1"}}},
			}},
			status:   models.RestoreVerificationFailed,
			mismatch: "collection orders: 9 documents restored, 10 expected",
			warning:  "verification failed: 2 mismatch(es)",
		},
		{
			name:    "a backup without a manifest is skipped",
			backup:  func(b *models.BackupRecord) { b.Manifest, b.HasManifest = nil, false },
			actual:  backupManifest(),
			status:  models.RestoreVerificationSkipped,
			note:    "no manifest",
			warning: "verification skipped",
		},
		{
			name:    "an uninspectable target is skipped",
			inspErr: errors.New("listCollections: not authorized"),
			status:  models.RestoreVerificationSkipped,
			note:    "not authorized",
		},
		{
			name:   "a dry run is skipped",
			actual: &models.Manifest{},
			req: func(r models.RestoreRequest) models.RestoreRequest {
				r.DryRun = true
				return r
			},
			status: models.RestoreVerificationSkipped,
			note:   "dry run",
		},
		{
			name: "a selective restore only compares the selection",
			actual: &models.Manifest{Collections: []models.CollectionManifest{
				{Name: "users", DocumentsMin: 3, DocumentsMax: 3, Indexes: []models.IndexSpec{{Name: "_id_", Keys: "_id:1"}}},
			}},
			req: func(r models.RestoreRequest) models.RestoreRequest {
				r.SelectedCollections = []string{"users"}
				return r
			},
			status: models.RestoreVerificationPassed,
		},
		{
			name: "an in-place restore without drop tolerates documents that were there",
			actual: &models.Manifest{Collections: []models.CollectionManifest{
				{Name: "orders", DocumentsMin: 15, DocumentsMax: 15, Indexes: backupManifest().Collections[0].Indexes},
				{Name: "users", DocumentsMin: 4, DocumentsMax: 4, Indexes: backupManifest().Collections[1].Indexes},
				{Name: "audit", DocumentsMin: 1, DocumentsMax: 1},
			}},
			req:    inPlace,
			status: models.RestoreVerificationPassed,
			note:   "already held documents",
		},
		{
			name: "an in-place restore with drop must match exactly",
			actual: &models.Manifest{Collections: []models.CollectionManifest{
				{Name: "orders", DocumentsMin: 15, DocumentsMax: 15, Indexes: backupManifest().Collections[0].Indexes},
				{Name: "users", DocumentsMin: 4, DocumentsMax: 4, Indexes: backupManifest().Collections[1].Indexes},
			}},
			req: func(r models.RestoreRequest) models.RestoreRequest {
				r = inPlace(r)
				r.DropTarget = true
				return r
			},
			status:   models.RestoreVerificationFailed,
			mismatch: "15 documents restored, 10 expected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ins := healthyInspector()
			ins.manifest, ins.manifestErr = tc.actual, tc.inspErr
			env := newPreflightEnv(t, ins, tc.backup)
			req := models.RestoreRequest{BackupID: env.backup.ID, VerifyRestore: true}
			if tc.req != nil {
				req = tc.req(req)
			}
			rec, err := env.svc.StartRestore(admin(), req)
			if err != nil {
				t.Fatal(err)
			}
			done := waitRestoreDone(t, env.svc, rec.ID)
			v := done.Verification
			if done.Status != models.RestoreStatusCompleted || v == nil || v.Status != tc.status || v.CheckedAt.IsZero() {
				t.Fatalf("restore = %s, verification %+v; want completed, %s", done.Status, v, tc.status)
			}
			if tc.mismatch != "" && !strings.Contains(strings.Join(v.Mismatches, "|"), tc.mismatch) {
				t.Errorf("mismatches = %q; want %q", v.Mismatches, tc.mismatch)
			}
			if tc.status != models.RestoreVerificationFailed && len(v.Mismatches) != 0 {
				t.Errorf("mismatches = %q; want none", v.Mismatches)
			}
			if tc.note != "" && !strings.Contains(strings.Join(v.Notes, "|"), tc.note) {
				t.Errorf("notes = %q; want %q", v.Notes, tc.note)
			}
			if tc.warning != "" && !strings.Contains(done.Warning, tc.warning) {
				t.Errorf("warning = %q; want %q", done.Warning, tc.warning)
			}
			if !req.DryRun && v.Status != models.RestoreVerificationSkipped || tc.inspErr != nil {
				if len(ins.inspected) != 1 || ins.inspected[0] != done.TargetDatabase {
					t.Errorf("inspected %v; want the restored database %s", ins.inspected, done.TargetDatabase)
				}
			}
			want := 0
			if tc.status == models.RestoreVerificationFailed {
				want = 1
			}
			failed := env.publisher.awaitVerificationEvents(t, want)
			switch {
			case tc.status == models.RestoreVerificationFailed && (len(failed) != 1 || failed[0].RestoreID != done.ID || failed[0].Detail == ""):
				t.Errorf("verification events = %+v; want one for %s", failed, done.ID)
			case tc.status != models.RestoreVerificationFailed && len(failed) != 0:
				t.Errorf("verification events = %+v; want none", failed)
			}
		})
	}
}

func TestRestoreWithoutVerifyRestoreIsNotVerified(t *testing.T) {
	ins := healthyInspector()
	ins.manifest = backupManifest()
	env := newPreflightEnv(t, ins, nil)
	rec, err := env.svc.StartRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID})
	if err != nil {
		t.Fatal(err)
	}
	if done := waitRestoreDone(t, env.svc, rec.ID); done.Verification != nil || len(ins.inspected) != 0 {
		t.Fatalf("verification = %+v, inspected %v; want none (verify_restore defaults to false)", done.Verification, ins.inspected)
	}
}

func TestPreflightSharesOneBoundedClient(t *testing.T) {
	ins := healthyInspector()
	env := newPreflightEnv(t, ins, nil)
	if _, err := env.svc.PreflightRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID}); err != nil {
		t.Fatal(err)
	}
	if ins.opened != 1 || ins.closed != 1 {
		t.Fatalf("opened %d, closed %d target clients; want one shared client, closed", ins.opened, ins.closed)
	}
	if ins.deadline <= 0 || ins.deadline > 15*time.Second {
		t.Fatalf("deadline %s; want at most 15s", ins.deadline)
	}
	// The request's context bounds the checks too.
	ctx, cancel := context.WithTimeout(admin(), 2*time.Second)
	defer cancel()
	if _, err := env.svc.PreflightRestore(ctx, models.RestoreRequest{BackupID: env.backup.ID}); err != nil {
		t.Fatal(err)
	}
	if ins.deadline > 2*time.Second {
		t.Fatalf("deadline %s; want the request's 2s", ins.deadline)
	}
}

func TestPreflightPrivilegesCountTheRestoredCollections(t *testing.T) {
	ins := healthyInspector()
	env := newPreflightEnv(t, ins, nil)
	if _, err := env.svc.PreflightRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID, SelectedCollections: []string{"users"}}); err != nil {
		t.Fatal(err)
	}
	if len(ins.privColls) != 1 || ins.privColls[0] != "users" {
		t.Fatalf("privileges checked for %v; want the selection", ins.privColls)
	}
	if _, err := env.svc.PreflightRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID}); err != nil {
		t.Fatal(err)
	}
	if len(ins.privColls) != 2 {
		t.Fatalf("privileges checked for %v; want the manifest's collections", ins.privColls)
	}
}

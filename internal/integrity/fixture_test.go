package integrity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// fakeTargets serves storage targets backed by in-memory storage.
type fakeTargets struct {
	targets map[string]*models.StorageTarget
	drivers map[string]storage.Storage
}

func (f *fakeTargets) List(context.Context) ([]*models.StorageTarget, error) {
	out := make([]*models.StorageTarget, 0, len(f.targets))
	for _, t := range f.targets {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b *models.StorageTarget) int { return compare(a.ID, b.ID) })
	return out, nil
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (f *fakeTargets) Resolve(_ context.Context, id string) (*models.StorageTarget, error) {
	if id == "" {
		id = "tgt_local"
	}
	t, ok := f.targets[id]
	if !ok {
		return nil, errors.New("targets: storage target not found")
	}
	return t, nil
}

func (f *fakeTargets) Storage(_ context.Context, id string) (storage.Storage, error) {
	if id == "" {
		id = "tgt_local"
	}
	d, ok := f.drivers[id]
	if !ok {
		return nil, errors.New("targets: storage target not found")
	}
	return d, nil
}

// fakeAdmin records every MongoDB call and serves canned answers.
type fakeAdmin struct {
	mu     sync.Mutex
	calls  []string // "op database"
	exists map[string]bool
	// existsPrefix makes every database with this prefix exist.
	existsPrefix string
	missing      []string
	privErr      error
	manifest     *models.Manifest
	dropErr      error
	dropped      []string
}

func (a *fakeAdmin) record(op, db string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, op+" "+db)
}

func (a *fakeAdmin) DatabaseExists(_ context.Context, _, db string) (bool, error) {
	a.record("exists", db)
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.exists[db] || (a.existsPrefix != "" && strings.HasPrefix(db, a.existsPrefix)), nil
}

func (a *fakeAdmin) DropDatabase(ctx context.Context, _, db string) error {
	a.record("drop", db)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropped = append(a.dropped, db)
	return a.dropErr
}

func (a *fakeAdmin) Manifest(_ context.Context, _, db string) (*models.Manifest, error) {
	a.record("manifest", db)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.manifest == nil {
		return &models.Manifest{}, nil
	}
	m := *a.manifest
	return &m, nil
}

func (a *fakeAdmin) RestoreTestPrivileges(_ context.Context, _, db string) ([]string, error) {
	a.record("privileges", db)
	return a.missing, a.privErr
}

func (a *fakeAdmin) touched() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

// fakeRestorer records restores; execute decides their outcome.
type fakeRestorer struct {
	mu         sync.Mutex
	requests   []models.RestoreRequest
	canDecrypt bool
	execute    func(ctx context.Context, req models.RestoreRequest) error
	// postRestore, when set, runs the deferred post-restore commands.
	postRestore func(ctx context.Context, req models.RestoreRequest, rec *models.RestoreRecord) (*models.RestoreRecord, error)
}

func (r *fakeRestorer) CanDecrypt() bool { return r.canDecrypt }

func (r *fakeRestorer) Prepare(req models.RestoreRequest, src *models.BackupRecord) (*models.RestoreRecord, error) {
	target := req.CloneDatabase
	if target == "" {
		id, err := models.NewCloneID()
		if err != nil {
			return nil, err
		}
		if target, err = models.RescueDatabaseName(src.Database, time.Now(), id); err != nil {
			return nil, err
		}
	}
	return &models.RestoreRecord{ID: "rst_x", BackupID: src.ID, TargetDatabase: target, Status: models.RestoreStatusInProgress}, nil
}

func (r *fakeRestorer) Execute(ctx context.Context, req models.RestoreRequest, _ *models.BackupRecord, rec *models.RestoreRecord) (*models.RestoreRecord, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	exec := r.execute
	r.mu.Unlock()
	if exec != nil {
		if err := exec(ctx, req); err != nil {
			rec.Status, rec.ErrorMessage = models.RestoreStatusFailed, err.Error()
			return rec, err
		}
	}
	rec.Status = models.RestoreStatusCompleted
	return rec, nil
}

func (r *fakeRestorer) RunPostRestore(ctx context.Context, req models.RestoreRequest, rec *models.RestoreRecord) (*models.RestoreRecord, error) {
	r.mu.Lock()
	post := r.postRestore
	r.mu.Unlock()
	if post == nil || len(req.PostRestoreCommands) == 0 {
		return rec, nil
	}
	return post(ctx, req, rec)
}

// fakeConnections resolves any id to a connection, with the post-restore commands
// cmds returns (when set).
type fakeConnections struct {
	cmds func() []models.PostRestoreCommand
}

func (c fakeConnections) Resolve(_ context.Context, id string) (*models.Connection, error) {
	if id == "missing" {
		return nil, errors.New("connections: not found")
	}
	conn := &models.Connection{ID: id, Name: "conn " + id, URI: "mongodb://user:pass@" + id + ":27017"}
	if c.cmds != nil {
		conn.PostRestoreCommands = c.cmds()
	}
	return conn, nil
}

// recordingPublisher captures events.
type recordingPublisher struct {
	mu  sync.Mutex
	got []events.Event
}

func (p *recordingPublisher) Publish(_ context.Context, e events.Event) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, e)
	return true
}

func (p *recordingPublisher) types() []events.EventType {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]events.EventType, 0, len(p.got))
	for _, e := range p.got {
		out = append(out, e.Type)
	}
	return out
}

// fixture is a Service on a real (temporary) SQLite store with fakes for storage,
// MongoDB and the restore engine.
type fixture struct {
	svc      *Service
	st       *store.SQLiteStore
	mem      *storage.MockStorage
	targets  *fakeTargets
	admin    *fakeAdmin
	restorer *fakeRestorer
	pub      *recordingPublisher
	runs     *runs.Manager
	settings settings.Settings
	now      time.Time
	mu       sync.Mutex
	// postRestore are the post-restore commands of every connection.
	postRestore []models.PostRestoreCommand
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		st:       storetest.New(t),
		mem:      storage.NewMockStorage(),
		admin:    &fakeAdmin{exists: map[string]bool{}},
		restorer: &fakeRestorer{canDecrypt: true},
		pub:      &recordingPublisher{},
		runs:     runs.NewManager(nil),
		settings: settings.Defaults(),
		now:      time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
	f.targets = &fakeTargets{
		targets: map[string]*models.StorageTarget{"tgt_local": {ID: "tgt_local", Name: "Local", Type: models.StorageLocal}},
		drivers: map[string]storage.Storage{"tgt_local": f.mem},
	}
	f.svc = New(Config{
		Store: f.st, Targets: f.targets, Runs: f.runs, Restore: f.restorer, Admin: f.admin,
		Connections: fakeConnections{cmds: func() []models.PostRestoreCommand { f.mu.Lock(); defer f.mu.Unlock(); return f.postRestore }},
		Settings:    func() settings.Settings { f.mu.Lock(); defer f.mu.Unlock(); return f.settings },
		Publisher:   f.pub,
		Now:         func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
	})
	t.Cleanup(func() { _ = f.runs.Shutdown(context.Background()) })
	return f
}

func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// putBackup stores a completed backup with its archive and returns it.
func (f *fixture) putBackup(t *testing.T, id, jobID string, started time.Time, data []byte, mutate func(*models.BackupRecord)) *models.BackupRecord {
	t.Helper()
	ctx := context.Background()
	key := fmt.Sprintf("shop/%s/%s.archive.gz", started.Format("2006/01"), id)
	if _, err := f.mem.Save(ctx, key, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	done := started.Add(time.Minute)
	rec := &models.BackupRecord{
		ID: id, JobID: jobID, Trigger: models.TriggerScheduled, Database: "shop", ConnectionID: "conn_src", Status: models.StatusCompleted,
		StorageType: models.StorageLocal, StorageTargetID: "tgt_local", StorageKey: key, SizeBytes: int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]), StartedAt: started, CompletedAt: &done,
	}
	if mutate != nil {
		mutate(rec)
	}
	if err := f.st.SaveBackupRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func (f *fixture) get(t *testing.T, id string) *models.BackupRecord {
	t.Helper()
	rec, err := f.st.GetBackupRecord(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// waitIdle waits until the runs.Manager has no integrity work left.
func (f *fixture) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.runs.Active()) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("background work still running: %v", f.runs.Active())
}

// corrupt replaces the stored archive of rec with different bytes.
func (f *fixture) corrupt(t *testing.T, rec *models.BackupRecord) {
	t.Helper()
	rc, err := f.mem.Retrieve(context.Background(), rec.StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	data[0] ^= 0xff
	if _, err := f.mem.Save(context.Background(), rec.StorageKey, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

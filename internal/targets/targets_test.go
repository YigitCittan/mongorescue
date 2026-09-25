package targets_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// fixture is a targets service on a real store. S3 targets are served by one mock
// driver per bucket; builds are counted to observe the driver cache.
type fixture struct {
	svc    *targets.Service
	store  *store.SQLiteStore
	base   string
	builds atomic.Int32
	mocks  map[string]*storage.MockStorage
	failS3 atomic.Bool
	repo   *hookRepo
}

// hookRepo runs beforeWrite before every update or test result is written, to
// simulate a concurrent request between a read and the write.
type hookRepo struct {
	*store.SQLiteStore
	beforeWrite func()
}

func (h *hookRepo) hook() {
	if h.beforeWrite != nil {
		fn := h.beforeWrite
		h.beforeWrite = nil
		fn()
	}
}

func (h *hookRepo) UpdateStorageTarget(ctx context.Context, t *models.StorageTarget, expected time.Time, moved bool) error {
	h.hook()
	return h.SQLiteStore.UpdateStorageTarget(ctx, t, expected, moved)
}

func (h *hookRepo) RecordStorageTargetTest(ctx context.Context, id string, tested, at time.Time, ok bool, msg string) (bool, error) {
	h.hook()
	return h.SQLiteStore.RecordStorageTargetTest(ctx, id, tested, at, ok, msg)
}

// abs returns an absolute path below the fixture's base directory.
func (f *fixture) abs(name string) string { return filepath.Join(f.base, name) }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{store: storetest.New(t), base: t.TempDir(), mocks: map[string]*storage.MockStorage{}}
	factory := func(_ context.Context, tg *models.StorageTarget, localPath string) (storage.Storage, error) {
		f.builds.Add(1)
		if tg.Type == models.StorageLocal {
			return storage.NewLocalStorage(localPath)
		}
		if f.failS3.Load() {
			return nil, errors.New("dial tcp: connection refused, secret " + tg.S3.SecretAccessKey)
		}
		m, ok := f.mocks[tg.S3.Bucket]
		if !ok {
			m = storage.NewMockStorage()
			f.mocks[tg.S3.Bucket] = m
		}
		return m, nil
	}
	f.repo = &hookRepo{SQLiteStore: f.store}
	f.svc = targets.NewService(f.repo, factory, filepath.Join(f.base, "data"), targets.WithTestTimeout(2*time.Second))
	return f
}

func s3Input(name, bucket, secret string) targets.Input {
	return targets.Input{Name: name, Type: models.StorageS3, S3: &models.S3Target{
		Endpoint: "https://s3.example.com/", Region: "auto", Bucket: bucket, Prefix: "/team/",
		AccessKeyID: "AKID", SecretAccessKey: secret,
	}}
}

func TestCreateListDefaultAndMasking(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	local, err := f.svc.Create(ctx, targets.Input{Name: " Local ", Type: models.StorageLocal, Local: &models.LocalTarget{Path: f.abs("backups")}})
	if err != nil {
		t.Fatal(err)
	}
	if !local.IsDefault || local.Name != "Local" || !strings.HasPrefix(local.ID, "stg_") {
		t.Fatalf("first target = %+v; want the default", local)
	}
	if _, err = os.Stat(filepath.Join(f.base, "backups")); err != nil {
		t.Fatalf("local directory not created on save: %v", err)
	}
	remote, err := f.svc.Create(ctx, s3Input("R2", "bucket-a", "top-secret-key"))
	if err != nil {
		t.Fatal(err)
	}
	if remote.IsDefault || remote.S3.SecretAccessKey != models.SecretMask || remote.S3.Prefix != "team/" || remote.S3.Endpoint != "https://s3.example.com" {
		t.Fatalf("s3 target = %+v", remote.S3)
	}
	list, err := f.svc.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %d, %v", len(list), err)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "top-secret-key") {
		t.Fatalf("list leaks the secret: %s", raw)
	}
	full, err := f.svc.Resolve(ctx, remote.ID)
	if err != nil || full.S3.SecretAccessKey != "top-secret-key" {
		t.Fatalf("Resolve = %+v, %v", full, err)
	}
	if def, err := f.svc.Resolve(ctx, ""); err != nil || def.ID != local.ID {
		t.Fatalf("default = %+v, %v", def, err)
	}

	// Switch the default; exactly one target is the default.
	if _, err := f.svc.SetDefault(ctx, remote.ID); err != nil {
		t.Fatal(err)
	}
	list, _ = f.svc.List(ctx)
	defaults := 0
	for _, tg := range list {
		if tg.IsDefault {
			defaults++
			if tg.ID != remote.ID {
				t.Fatalf("wrong default: %+v", tg)
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("%d default targets", defaults)
	}
	if _, err := f.svc.SetDefault(ctx, "stg_missing"); !errors.Is(err, targets.ErrNotFound) {
		t.Fatalf("SetDefault(missing) = %v", err)
	}
	// Updating a target keeps its default flag.
	if _, err := f.svc.Update(ctx, remote.ID, s3Input("R2 renamed", "bucket-a", models.SecretMask)); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.Get(ctx, remote.ID); !got.IsDefault || got.Name != "R2 renamed" {
		t.Fatalf("after update = %+v", got)
	}
}

func TestValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	long := strings.Repeat("x", 101)
	for name, in := range map[string]targets.Input{
		"no name":       {Type: models.StorageLocal, Local: &models.LocalTarget{Path: f.abs("b")}},
		"long name":     {Name: long, Type: models.StorageLocal, Local: &models.LocalTarget{Path: f.abs("b")}},
		"bad type":      {Name: "x", Type: "ftp"},
		"no local":      {Name: "x", Type: models.StorageLocal},
		"empty path":    {Name: "x", Type: models.StorageLocal, Local: &models.LocalTarget{Path: " "}},
		"relative path": {Name: "x", Type: models.StorageLocal, Local: &models.LocalTarget{Path: "backups"}},
		"root":          {Name: "x", Type: models.StorageLocal, Local: &models.LocalTarget{Path: string(filepath.Separator)}},
		"data dir":      {Name: "x", Type: models.StorageLocal, Local: &models.LocalTarget{Path: f.abs("data")}},
		"inside data":   {Name: "x", Type: models.StorageLocal, Local: &models.LocalTarget{Path: f.abs("data/backups/x")}},
		"traversal":     {Name: "x", Type: models.StorageLocal, Local: &models.LocalTarget{Path: "a/../../etc"}},
		"win traversal": {Name: "x", Type: models.StorageLocal, Local: &models.LocalTarget{Path: `a\..\..\etc`}},
		"no s3":         {Name: "x", Type: models.StorageS3},
		"short bucket":  s3Input("x", "ab", "k"),
		"slash bucket":  s3Input("x", "a/b", "k"),
		"bad endpoint":  {Name: "x", Type: models.StorageS3, S3: &models.S3Target{Endpoint: "s3.example.com", Bucket: "bucket"}},
		"placeholder":   {Name: "x", Type: models.StorageS3, S3: &models.S3Target{Endpoint: "https://{region}.example.com", Bucket: "bucket"}},
		"half creds":    {Name: "x", Type: models.StorageS3, S3: &models.S3Target{Bucket: "bucket", AccessKeyID: "AKID"}},
		"prefix dotdot": {Name: "x", Type: models.StorageS3, S3: &models.S3Target{Bucket: "bucket", Prefix: "../x"}},
		"masked new":    s3Input("x", "bucket", models.SecretMask),
	} {
		if _, err := f.svc.Create(ctx, in); !errors.Is(err, targets.ErrInvalid) && !errors.Is(err, targets.ErrMaskedSecret) {
			t.Errorf("%s: %v; want a validation error", name, err)
		}
	}
	// No credentials at all is valid: the AWS default chain applies.
	if _, err := f.svc.Create(ctx, targets.Input{Name: "iam", Type: models.StorageS3, S3: &models.S3Target{Bucket: "bucket"}}); err != nil {
		t.Fatalf("s3 without static credentials: %v", err)
	}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		ro := filepath.Join(t.TempDir(), "ro")
		if err := os.MkdirAll(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		_, err := f.svc.Create(ctx, targets.Input{Name: "ro", Type: models.StorageLocal, Local: &models.LocalTarget{Path: filepath.Join(ro, "sub")}})
		if !errors.Is(err, targets.ErrInvalid) || !strings.Contains(err.Error(), "not writable") {
			t.Fatalf("unwritable path = %v", err)
		}
	}
}

func TestDataDirectoryIsRefusedThroughSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	f := newFixture(t)
	if err := os.MkdirAll(f.abs("data"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.abs("data"), f.abs("link")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{f.abs("link"), f.abs("link/new/dir"), f.abs("data/../data")} {
		_, err := f.svc.Create(context.Background(), targets.Input{Name: "x", Type: models.StorageLocal, Local: &models.LocalTarget{Path: p}})
		if !errors.Is(err, targets.ErrInvalid) {
			t.Errorf("%s: %v; want ErrInvalid", p, err)
		}
	}
	if _, err := os.Stat(f.abs("link/new")); !os.IsNotExist(err) {
		t.Fatal("a refused path must not be created")
	}
	// A sibling of the data directory is fine.
	if _, err := f.svc.Create(context.Background(), targets.Input{Name: "ok", Type: models.StorageLocal, Local: &models.LocalTarget{Path: f.abs("database-backups")}}); err != nil {
		t.Fatalf("sibling of the data dir: %v", err)
	}
}

func TestTestInputHasNoSideEffects(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	missing := f.abs("not/there")
	res, err := f.svc.TestInput(ctx, targets.Input{Name: "x", Type: models.StorageLocal, Local: &models.LocalTarget{Path: missing}}, "")
	if err != nil || res.OK || !strings.Contains(res.Error, "does not exist") {
		t.Fatalf("TestInput(missing) = %+v, %v", res, err)
	}
	if _, err = os.Stat(f.abs("not")); !os.IsNotExist(err) {
		t.Fatal("testing an unsaved path must not create directories")
	}
	existing := f.abs("existing")
	if err = os.MkdirAll(existing, 0o750); err != nil {
		t.Fatal(err)
	}
	res, err = f.svc.TestInput(ctx, targets.Input{Name: "x", Type: models.StorageLocal, Local: &models.LocalTarget{Path: existing}}, "")
	if err != nil || !res.OK {
		t.Fatalf("TestInput(existing) = %+v, %v", res, err)
	}
	if entries, _ := os.ReadDir(existing); len(entries) != 0 {
		t.Fatalf("probe left files or directories behind: %v", entries)
	}
}

func TestConcurrentChangesAreNotLostOrResurrected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, targets.Input{Name: "default", Type: models.StorageLocal, Local: &models.LocalTarget{Path: f.abs("def")}}); err != nil {
		t.Fatal(err)
	}
	victim, err := f.svc.Create(ctx, s3Input("victim", "bucket-v", "secret-v"))
	if err != nil {
		t.Fatal(err)
	}
	// Deleted while its test runs: the result must not recreate it.
	f.repo.beforeWrite = func() {
		if delErr := f.store.DeleteStorageTarget(ctx, victim.ID); delErr != nil {
			t.Error(delErr)
		}
	}
	if _, err = f.svc.Test(ctx, victim.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.Get(ctx, victim.ID); !errors.Is(err, targets.ErrNotFound) {
		t.Fatalf("target deleted during its test = %v; want ErrNotFound", err)
	}

	// Edited while its test runs: the edit is kept.
	edited, err := f.svc.Create(ctx, s3Input("edited", "bucket-e", "secret-e"))
	if err != nil {
		t.Fatal(err)
	}
	f.repo.beforeWrite = func() {
		if _, err := f.svc.Update(ctx, edited.ID, s3Input("renamed during test", "bucket-e", models.SecretMask)); err != nil {
			t.Error(err)
		}
	}
	if _, err := f.svc.Test(ctx, edited.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.Get(ctx, edited.ID); got.Name != "renamed during test" {
		t.Fatalf("edit during a test was reverted: %+v", got)
	}

	// Deleted or changed while an update is prepared: 404 / 409, nothing recreated.
	f.repo.beforeWrite = func() {
		if err := f.store.DeleteStorageTarget(ctx, edited.ID); err != nil {
			t.Error(err)
		}
	}
	if _, err := f.svc.Update(ctx, edited.ID, s3Input("late", "bucket-e", models.SecretMask)); !errors.Is(err, targets.ErrNotFound) {
		t.Fatalf("update of a target deleted meanwhile = %v; want ErrNotFound", err)
	}
	if list, _ := f.svc.List(ctx); len(list) != 1 {
		t.Fatalf("targets = %+v; want only the default", list)
	}
	other, _ := f.svc.Create(ctx, s3Input("other", "bucket-o", "k"))
	f.repo.beforeWrite = func() {
		// A concurrent request (another instance of the service would hold its own lock).
		cur, _ := f.store.GetStorageTarget(ctx, other.ID)
		read := cur.UpdatedAt
		cur.Name, cur.UpdatedAt = "first", read.Add(time.Second)
		if err := f.store.UpdateStorageTarget(ctx, cur, read, false); err != nil {
			t.Error(err)
		}
	}
	if _, err := f.svc.Update(ctx, other.ID, s3Input("second", "bucket-o", models.SecretMask)); !errors.Is(err, targets.ErrConflict) {
		t.Fatalf("concurrent update = %v; want ErrConflict", err)
	}
	if got, _ := f.svc.Get(ctx, other.ID); got.Name != "first" {
		t.Fatalf("after conflict = %+v", got)
	}
}

func TestLocationOfATargetWithBackupsIsLocked(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tg, err := f.svc.Create(ctx, s3Input("r2", "bucket-a", "s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b", Status: models.StatusCompleted, StorageTargetID: tg.ID}); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*targets.Input){
		"bucket":   func(in *targets.Input) { in.S3.Bucket, in.S3.SecretAccessKey = "bucket-b", "s3cret" },
		"endpoint": func(in *targets.Input) { in.S3.Endpoint, in.S3.SecretAccessKey = "https://other.example.com", "s3cret" },
		"prefix":   func(in *targets.Input) { in.S3.Prefix = "elsewhere/" },
		"type": func(in *targets.Input) {
			in.Type, in.S3, in.Local = models.StorageLocal, nil, &models.LocalTarget{Path: f.abs("moved")}
		},
	} {
		in := s3Input("r2", "bucket-a", models.SecretMask)
		mutate(&in)
		_, err := f.svc.Update(ctx, tg.ID, in)
		if !errors.Is(err, targets.ErrLocationInUse) || !strings.Contains(err.Error(), "create a new target instead") {
			t.Errorf("changing the %s of a target with backups = %v", name, err)
		}
	}
	// Name, credentials, region and path style may change.
	in := s3Input("renamed", "bucket-a", "new-secret")
	in.S3.AccessKeyID, in.S3.Region, in.S3.UsePathStyle = "NEWKEY", "eu-west-1", true
	if _, err := f.svc.Update(ctx, tg.ID, in); err != nil {
		t.Fatalf("changing name and credentials: %v", err)
	}
	if full, _ := f.svc.Resolve(ctx, tg.ID); full.S3.SecretAccessKey != "new-secret" || full.S3.Region != "eu-west-1" {
		t.Fatalf("after update = %+v", full.S3)
	}
}

func TestKeepSecretRule(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created, err := f.svc.Create(ctx, s3Input("r2", "bucket-a", "s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	// Unchanged endpoint, bucket and access key: the mask keeps the secret.
	if _, err := f.svc.Update(ctx, created.ID, s3Input("r2", "bucket-a", models.SecretMask)); err != nil {
		t.Fatal(err)
	}
	if full, _ := f.svc.Resolve(ctx, created.ID); full.S3.SecretAccessKey != "s3cret" {
		t.Fatal("masked update lost the secret")
	}
	// Moving the credentials to another bucket, endpoint or access key needs the secret.
	moved := s3Input("r2", "bucket-b", models.SecretMask)
	if _, err := f.svc.Update(ctx, created.ID, moved); !errors.Is(err, targets.ErrMaskedSecret) {
		t.Fatalf("masked secret with another bucket = %v", err)
	}
	other := s3Input("r2", "bucket-a", models.SecretMask)
	other.S3.Endpoint = "https://evil.example.com"
	if _, err := f.svc.TestInput(ctx, other, created.ID); !errors.Is(err, targets.ErrMaskedSecret) {
		t.Fatalf("test with a masked secret on another endpoint = %v", err)
	}
	key := s3Input("r2", "bucket-a", models.SecretMask)
	key.S3.AccessKeyID = "OTHER"
	if _, err := f.svc.Update(ctx, created.ID, key); !errors.Is(err, targets.ErrMaskedSecret) {
		t.Fatalf("masked secret with another access key = %v", err)
	}
	// The form can be tested with the masked secret while editing.
	if res, err := f.svc.TestInput(ctx, s3Input("r2", "bucket-a", models.SecretMask), created.ID); err != nil || !res.OK {
		t.Fatalf("TestInput with the stored secret = %+v, %v", res, err)
	}
}

func TestProbeAndRecordedResult(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created, err := f.svc.Create(ctx, s3Input("r2", "bucket-a", "s3cret-value"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.Test(ctx, created.ID)
	if err != nil || !res.OK || res.Error != "" {
		t.Fatalf("Test = %+v, %v", res, err)
	}
	if objs, _ := f.mocks["bucket-a"].List(ctx, targets.ProbePrefix); len(objs) != 0 {
		t.Fatalf("probe objects left behind: %+v", objs)
	}
	got, _ := f.svc.Get(ctx, created.ID)
	if got.LastTestAt == nil || !got.LastTestOK {
		t.Fatalf("test result not recorded: %+v", got)
	}
	f.failS3.Store(true)
	res, _ = f.svc.Test(ctx, created.ID)
	if res.OK || res.Error == "" || strings.Contains(res.Error, "s3cret-value") {
		t.Fatalf("failing test = %+v; want a scrubbed error", res)
	}
	got, _ = f.svc.Get(ctx, created.ID)
	if got.LastTestOK || got.LastTestError == "" {
		t.Fatalf("failure not recorded: %+v", got)
	}
	if _, err := f.svc.Test(ctx, "stg_missing"); !errors.Is(err, targets.ErrNotFound) {
		t.Fatalf("Test(missing) = %v", err)
	}
}

func TestDriverCacheIsRebuiltOnUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created, err := f.svc.Create(ctx, s3Input("r2", "bucket-a", "k"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.svc.Storage(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	builds := f.builds.Load()
	again, _ := f.svc.Storage(ctx, created.ID)
	if again != a || f.builds.Load() != builds {
		t.Fatal("driver must be cached per target")
	}
	// Recording a test result does not invalidate the driver.
	_, _ = f.svc.Test(ctx, created.ID)
	builds = f.builds.Load()
	if d, _ := f.svc.Storage(ctx, created.ID); d != a || f.builds.Load() != builds {
		t.Fatal("a test result must not rebuild the driver")
	}
	if _, err := f.svc.Update(ctx, created.ID, s3Input("r2", "bucket-b", "k2")); err != nil {
		t.Fatal(err)
	}
	b, _ := f.svc.Storage(ctx, created.ID)
	if b == a || b != storage.Storage(f.mocks["bucket-b"]) {
		t.Fatal("updated target must get a new driver")
	}
	// The default target is served for an empty id.
	if d, err := f.svc.Storage(ctx, ""); err != nil || d != b {
		t.Fatalf("Storage(\"\") = %v, %v", d, err)
	}
}

func TestDeleteRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	def, _ := f.svc.Create(ctx, targets.Input{Name: "local", Type: models.StorageLocal, Local: &models.LocalTarget{Path: f.abs("b")}})
	other, _ := f.svc.Create(ctx, s3Input("r2", "bucket-a", "k"))
	if err := f.svc.Delete(ctx, def.ID); !errors.Is(err, targets.ErrIsDefault) {
		t.Fatalf("delete default = %v; want ErrIsDefault", err)
	}
	if err := f.store.SaveJob(ctx, &models.Job{ID: "j", Name: "j", StorageTargetID: other.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Delete(ctx, other.ID); !errors.Is(err, targets.ErrInUse) {
		t.Fatalf("delete target used by a job = %v", err)
	}
	if err := f.store.DeleteJob(ctx, "j"); err != nil {
		t.Fatal(err)
	}
	for _, st := range []models.BackupStatus{models.StatusCompleted, models.StatusInProgress} {
		if err := f.store.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b", Status: st, StorageTargetID: other.ID}); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.Delete(ctx, other.ID); !errors.Is(err, targets.ErrInUse) {
			t.Fatalf("delete target with a %s backup = %v", st, err)
		}
	}
	// Failed and pruned backups have no artifact left: they do not pin the target.
	if err := f.store.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b", Status: models.StatusPruned, StorageTargetID: other.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Delete(ctx, other.ID); err != nil {
		t.Fatalf("delete target with only pruned backups = %v", err)
	}
	if err := f.svc.Delete(ctx, other.ID); !errors.Is(err, targets.ErrNotFound) {
		t.Fatalf("delete twice = %v", err)
	}
}

func TestEnsureDefault(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backups")
	created, ok, err := f.svc.EnsureDefault(ctx, path)
	if err != nil || !ok || created.Name != targets.DefaultLocalName || !created.IsDefault || created.Local.Path != path {
		t.Fatalf("EnsureDefault = %+v, %v, %v", created, ok, err)
	}
	again, ok, err := f.svc.EnsureDefault(ctx, "/elsewhere")
	if err != nil || ok || again.ID != created.ID {
		t.Fatalf("second EnsureDefault = %+v, %v, %v", again, ok, err)
	}
}

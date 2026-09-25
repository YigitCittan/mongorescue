package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// targetsFixture wires settings, storage targets and the engines like internal/app,
// with local storage in a temp dir and fake tool runners.
type targetsFixture struct {
	h        http.Handler
	store    *store.SQLiteStore
	settings *settings.Service
	targets  *targets.Service
	base     string
}

func newTargetsFixture(t *testing.T) *targetsFixture {
	t.Helper()
	st := storetest.New(t)
	f := &targetsFixture{store: st, base: t.TempDir()}
	var err error
	if f.settings, err = settings.NewService(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	f.targets = targets.NewService(st, storage.NewForTarget, filepath.Join(f.base, "data"))
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("backup-data"))), strings.NewReader(""), func() error { return nil }, nil
	}
	rRunner := func(_ context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		_, _ = io.Copy(io.Discard, stdin)
		return strings.NewReader(""), func() error { return nil }, nil
	}
	bEngine := backup.NewEngine(nil, "", backup.WithRunner(bRunner), backup.WithStorageResolver(f.targets.Storage),
		backup.WithRunConfig(func() backup.RunConfig { return backup.RunConfig{Encryptor: f.settings.Encryptor()} }))
	rEngine := restore.NewEngine(nil, "", restore.WithRunner(rRunner), restore.WithStorageResolver(f.targets.Storage),
		restore.WithRunConfig(func() restore.RunConfig { return restore.RunConfig{Decryptor: f.settings.Decryptor()} }))
	sched := scheduler.NewScheduler(st, bEngine, nil, nil, scheduler.WithStorageTargets(f.targets))
	full := NewServer(bootConfig(), st, bEngine, rEngine, nil, sched, nil, nil,
		WithAuth(newTestAuth(t, st, testAPIKey)), withTestConnection(t, st, nil),
		WithSettings(f.settings), WithStorageTargets(f.targets))
	f.h = keyed{full.Handler()}
	return f
}

func (f *targetsFixture) do(method, path string, body any) *httptest.ResponseRecorder {
	var raw []byte
	switch b := body.(type) {
	case nil:
	case string:
		raw = []byte(b)
	default:
		raw, _ = json.Marshal(b)
	}
	return serve(f.h, method, path, raw, map[string]string{"Content-Type": "application/json"})
}

func (f *targetsFixture) createLocal(t *testing.T, name, path string) models.StorageTarget {
	t.Helper()
	rec := f.do("POST", "/api/v1/storage-targets", map[string]any{"name": name, "type": "local", "local": map[string]string{"path": path}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create target: %d %s", rec.Code, rec.Body)
	}
	var tg models.StorageTarget
	decodeData(t, rec, &tg)
	return tg
}

func TestSettingsEndpoints(t *testing.T) {
	f := newTargetsFixture(t)
	rec := f.do("GET", "/api/v1/settings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings: %d", rec.Code)
	}
	var got map[string]any
	decodeData(t, rec, &got)
	for _, group := range []string{"general", "security", "encryption", "restart_required"} {
		if _, ok := got[group]; !ok {
			t.Fatalf("settings lack %s: %v", group, got)
		}
	}
	if g := got["general"].(map[string]any); g["backup_timeout"] != "6h0m0s" || g["restore_verify_policy"] != "auto" {
		t.Fatalf("general = %v", g)
	}

	rec = f.do("PUT", "/api/v1/settings", `{"general":{"default_retention_days":3,"backup_timeout":"90m","default_gzip":false}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT general: %d %s", rec.Code, rec.Body)
	}
	decodeData(t, rec, &got)
	if g := got["general"].(map[string]any); g["backup_timeout"] != "1h30m0s" || g["default_retention_days"] != float64(3) || g["default_retention_count"] != float64(10) {
		t.Fatalf("after PUT = %v", g)
	}
	for body, want := range map[string]string{
		`{"general":{"backup_timeout":"soon"}}`: "duration",
		`{"general":{"backup_timeout":3600}}`:   "duration",
		`{"general":{"nope":1}}`:                "unknown field",
		`{"security":{"cors_origins":["*"]}}`:   "cors_origins",
		`{"encryption":{"identity":"******"}}`:  "masked",
		`{"encryption":{"passphrase":"short"}}`: "passphrase",
		`{"encryption":{"enabled":true}}`:       "recipients",
		`not json`:                              "malformed",
	} {
		rec = f.do("PUT", "/api/v1/settings", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("PUT %s: %d %s; want 400 mentioning %q", body, rec.Code, rec.Body, want)
		}
	}

	rec = f.do("POST", "/api/v1/settings/encryption/generate-key", nil)
	var key settings.GeneratedKey
	decodeData(t, rec, &key)
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(key.Identity, "AGE-SECRET-KEY-1") || !strings.HasPrefix(key.Recipient, "age1") {
		t.Fatalf("generate-key = %+v (%s)", key, rec.Header().Get("Cache-Control"))
	}
	if f.settings.Current().Encryption.Identity != "" {
		t.Fatal("generate-key must not store anything")
	}
	rec = f.do("PUT", "/api/v1/settings", map[string]any{"encryption": map[string]any{
		"enabled": true, "mode": "x25519", "recipients": []string{key.Recipient}, "identity": key.Identity}})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), key.Identity) || !strings.Contains(rec.Body.String(), `"identity":"******"`) {
		t.Fatalf("PUT encryption: %d %s", rec.Code, rec.Body)
	}
	if f.settings.Encryptor() == nil {
		t.Fatal("encryption not applied live")
	}
}

func TestStorageTargetEndpointsAndPerTargetBackups(t *testing.T) {
	f := newTargetsFixture(t)
	ctx := context.Background()
	a := f.createLocal(t, "disk a", filepath.Join(f.base, "a"))
	b := f.createLocal(t, "disk b", filepath.Join(f.base, "b"))
	if !a.IsDefault || b.IsDefault {
		t.Fatalf("defaults: a=%v b=%v", a.IsDefault, b.IsDefault)
	}
	for _, bad := range []string{"../x", "relative", "/", filepath.Join(f.base, "data", "x")} {
		if rec := f.do("POST", "/api/v1/storage-targets", map[string]any{"name": "x", "type": "local", "local": map[string]string{"path": bad}}); rec.Code != http.StatusBadRequest {
			t.Fatalf("path %q: %d %s; want 400", bad, rec.Code, rec.Body)
		}
	}
	var res targets.TestResult
	rec := f.do("POST", "/api/v1/storage-targets/"+b.ID+"/test", nil)
	decodeData(t, rec, &res)
	if !res.OK {
		t.Fatalf("test b = %+v", res)
	}
	rec = f.do("POST", "/api/v1/storage-targets/test", map[string]any{"name": "s3", "type": "s3", "s3": map[string]any{"endpoint": "http://127.0.0.1:1", "bucket": "bucket", "access_key_id": "a", "secret_access_key": "b"}})
	decodeData(t, rec, &res)
	if rec.Code != http.StatusOK || res.OK || res.Error == "" {
		t.Fatalf("failing unsaved test must answer 200 with ok=false: %d %+v", rec.Code, res)
	}
	if rec = f.do("GET", "/api/v1/storage-targets/stg_missing", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing target: %d", rec.Code)
	}

	// A job and a manual backup on target b; the default gzip setting applies.
	if _, err := f.settings.Update(ctx, settings.Patch{General: &settings.GeneralPatch{DefaultGzip: new(bool), DefaultRetentionCount: ptrTo(4)}}); err != nil {
		t.Fatal(err)
	}
	rec = f.do("POST", "/api/v1/jobs", map[string]any{"name": "j", "database": "shop", "connection_id": testConnID, "storage_target_id": b.ID})
	var job models.Job
	decodeData(t, rec, &job)
	if job.StorageTargetID != b.ID || job.RetentionCount != 4 || job.RetentionDays != 30 || job.Gzip {
		t.Fatalf("job = %+v; want target b and the default retention/gzip", job)
	}
	rec = f.do("POST", "/api/v1/jobs", map[string]any{"name": "j2", "database": "shop", "connection_id": testConnID, "retention_days": 0})
	decodeData(t, rec, &job)
	if job.StorageTargetID != a.ID || job.RetentionDays != 0 {
		t.Fatalf("job without target = %+v; want the default target and an explicit 0", job)
	}
	if rec := f.do("POST", "/api/v1/jobs", map[string]any{"name": "j3", "database": "shop", "connection_id": testConnID, "storage_target_id": "stg_nope"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown target: %d %s", rec.Code, rec.Body)
	}

	id := acceptedID(t, f.do("POST", "/api/v1/backups", map[string]any{"connection_id": testConnID, "database": "shop", "storage_target_id": b.ID}))
	rec0 := awaitRecord(t, f.h, "/api/v1/backups", id)
	if rec0["status"] != "completed" || rec0["storage_target_id"] != b.ID || rec0["storage_target_name"] != "disk b" {
		t.Fatalf("backup = %v", rec0)
	}
	key := rec0["storage_key"].(string)
	if strings.HasSuffix(key, ".gz") {
		t.Fatalf("default_gzip=false must apply to manual backups: %s", key)
	}
	artifact := filepath.Join(f.base, "b", filepath.FromSlash(key))
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("artifact not on target b: %v", err)
	}

	// Changing the default does not move existing backups: restore and delete use b.
	if rec := f.do("POST", "/api/v1/storage-targets/"+b.ID+"/default", nil); rec.Code != http.StatusOK {
		t.Fatalf("set default: %d", rec.Code)
	}
	if rec := f.do("POST", "/api/v1/storage-targets/"+a.ID+"/default", nil); rec.Code != http.StatusOK {
		t.Fatalf("set default back: %d", rec.Code)
	}
	// The source connection is renamed after the backup: the restore record keeps the
	// name at restore time.
	conn, err := f.store.GetConnection(ctx, testConnID)
	if err != nil {
		t.Fatal(err)
	}
	conn.Name = "renamed server"
	if err := f.store.SaveConnection(ctx, conn); err != nil {
		t.Fatal(err)
	}
	rst := acceptedID(t, f.do("POST", "/api/v1/restore", map[string]any{"backup_id": id}))
	got := awaitRecord(t, f.h, "/api/v1/restores", rst)
	if got["status"] != "completed" {
		t.Fatalf("restore from target b = %v", got)
	}
	if got["source_connection_id"] != testConnID || got["source_connection_name"] != "renamed server" ||
		got["target_connection_id"] != testConnID || got["target_connection_name"] != "renamed server" {
		t.Fatalf("restore record connections = %v", got)
	}
	if rec := f.do("DELETE", "/api/v1/storage-targets/"+b.ID, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "jobs") {
		t.Fatalf("delete target in use: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("DELETE", "/api/v1/storage-targets/"+a.ID, nil); rec.Code != http.StatusConflict {
		t.Fatalf("delete default: %d", rec.Code)
	}
	// The location of a target holding backups cannot change; its name can.
	moved := map[string]any{"name": "disk b", "type": "local", "local": map[string]string{"path": filepath.Join(f.base, "c")}}
	if rec := f.do("PUT", "/api/v1/storage-targets/"+b.ID, moved); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "create a new target instead") {
		t.Fatalf("moving a target with backups: %d %s", rec.Code, rec.Body)
	}
	renamed := map[string]any{"name": "disk b2", "type": "local", "local": map[string]string{"path": filepath.Join(f.base, "b")}}
	if rec := f.do("PUT", "/api/v1/storage-targets/"+b.ID, renamed); rec.Code != http.StatusOK {
		t.Fatalf("renaming a target with backups: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("PUT", "/api/v1/storage-targets/stg_missing", renamed); rec.Code != http.StatusNotFound {
		t.Fatalf("update of a missing target: %d", rec.Code)
	}
	if rec := f.do("DELETE", "/api/v1/backups/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete backup: %d", rec.Code)
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("artifact not deleted from target b: %v", err)
	}

	var stats map[string]any
	decodeData(t, f.do("GET", "/api/v1/stats", nil), &stats)
	if def, _ := stats["default_storage_target"].(map[string]any); def["id"] != a.ID || stats["storage_type"] != "local" {
		t.Fatalf("stats = %v", stats)
	}
}

func ptrTo[T any](v T) *T { return &v }

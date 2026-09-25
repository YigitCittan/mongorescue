package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// noEnv hides the process environment from the deprecated-variable import.
func noEnv(string) string { return "" }

// testConfig returns a bootstrap config with a data directory in a fresh temp dir
// (the default "Local disk" target lands next to it).
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	return cfg
}

func TestAppInitialization(t *testing.T) {
	cfg := testConfig(t)

	application, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatalf("expected clean app initialization, got error: %v", err)
	}
	t.Cleanup(func() { _ = application.Close() })

	if application == nil {
		t.Fatal("expected non-nil application instance")
	}

	if application.server == nil {
		t.Fatal("expected non-nil server component")
	}

	if application.scheduler == nil {
		t.Fatal("expected non-nil scheduler component")
	}

	if application.bus == nil || application.notifications == nil || application.metrics == nil {
		t.Fatal("expected event bus, notification service and metrics to be wired")
	}
}

func TestAppBackgroundLifecycle(t *testing.T) {
	cfg := testConfig(t)

	application, err := New(cfg, nil, WithBuildInfo("9.9.9", "deadbeef"), WithGetenv(noEnv))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = application.Close() })

	stop := application.startBackground()
	application.bus.Publish(context.Background(), events.Event{Type: events.BackupFailed, JobID: "job_x"})

	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("background workers did not stop")
	}

	rec := httptest.NewRecorder()
	application.metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `mongorescue_backups_total{job="job_x",status="failed"} 1`) {
		t.Error("event published before shutdown must be drained into metrics")
	}
	if !strings.Contains(body, `version="9.9.9"`) || !strings.Contains(body, `commit="deadbeef"`) {
		t.Error("build info not propagated")
	}
}

func TestAppMigratesLegacyState(t *testing.T) {
	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(cfg.DataDir, "state.json")
	legacy := `{"jobs": {"job_1": {"id": "job_1", "name": "Nightly", "database": "shop", "mongo_uri": "mongodb://u:legacy-pw@legacy-db:27017/"}},
		"backups": {"bkp_1": {"id": "bkp_1", "job_id": "job_1", "database": "shop", "status": "in_progress"}}, "restores": {}}`
	if err := os.WriteFile(statePath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	application, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	job, err := application.metaStore.GetJob(ctx, "job_1")
	if err != nil {
		t.Fatalf("legacy job not imported: %v", err)
	}
	// Its connection string became a managed connection.
	conn, err := application.metaStore.(*store.SQLiteStore).GetConnection(ctx, job.ConnectionID)
	if err != nil || conn.URI != "mongodb://u:legacy-pw@legacy-db:27017/" || conn.Name != "legacy-db:27017" {
		t.Fatalf("legacy job uri not migrated: %+v, %v", conn, err)
	}
	// Crash recovery also covers records imported from state.json.
	application.failInterruptedRuns(ctx)
	if b, err := application.metaStore.GetBackupRecord(ctx, "bkp_1"); err != nil || b.Status != models.StatusFailed {
		t.Fatalf("imported in-progress backup = %+v, %v; want failed", b, err)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state.json must be archived after import: %v", err)
	}
	if _, err := os.Stat(cfg.MetadataDBPath()); err != nil {
		t.Fatalf("metadata database missing: %v", err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("second Close = %v; want nil", err)
	}
}

func TestAppSetupModeAndDefaultConnection(t *testing.T) {
	cfg := testConfig(t)
	const uri = "mongodb://root:env-pw@mongo:27017/?authSource=admin"

	application, err := New(cfg, nil, WithGetenv(func(k string) string {
		if k == config.EnvMongoURI {
			return uri
		}
		return ""
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if code := application.auth.SetupCode(); len(code) != 29 {
		t.Fatalf("fresh install must be in setup mode with a code, got %q", code)
	}
	conns, err := application.metaStore.(*store.SQLiteStore).ListConnections(context.Background())
	if err != nil || len(conns) != 1 || conns[0].Name != "default" || conns[0].URI != uri {
		t.Fatalf("default connection = %+v, %v", conns, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(cfg.DataDir, "secret.key"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("secret.key = %v, %v; want mode 0600", info, err)
		}
	}
}

func TestAppRefusesWrongSecretKey(t *testing.T) {
	cfg := testConfig(t)
	keyFile := filepath.Join(cfg.DataDir, "secret.key")

	first, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}

	// A lost key file: the regenerated key does not match and is removed again.
	if err = os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	if _, err = New(cfg, nil, WithGetenv(noEnv)); !errors.Is(err, secretbox.ErrSecretKeyMismatch) {
		t.Fatalf("New with a lost key file = %v; want ErrSecretKeyMismatch", err)
	}
	if _, err = os.Stat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a freshly generated, mismatching key file must not be left behind")
	}

	// A wrong MONGORESCUE_SECRET_KEY.
	other, _ := secretbox.GenerateKey()
	cfg.SecretKey = secretbox.EncodeKey(other)
	if _, err = New(cfg, nil, WithGetenv(noEnv)); !errors.Is(err, secretbox.ErrSecretKeyMismatch) {
		t.Fatalf("New with a wrong env key = %v; want ErrSecretKeyMismatch", err)
	}

	// The original key (as env value) opens it again.
	cfg.SecretKey = strings.TrimSpace(string(original))
	again, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatalf("New with the original key: %v", err)
	}
	_ = again.Close()
}

func TestAppRefusesSharedDataDir(t *testing.T) {
	cfg := testConfig(t)

	first, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err = New(cfg, nil, WithGetenv(noEnv)); !errors.Is(err, store.ErrDataDirLocked) {
		t.Fatalf("second New on the same data dir = %v; want ErrDataDirLocked", err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatalf("New after Close: %v", err)
	}
	_ = again.Close()
}

func TestAppFailsOnBrokenLegacyState(t *testing.T) {
	cfg := testConfig(t)
	statePath := filepath.Join(cfg.DataDir, "state.json")
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(`{"jobs": [`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := New(cfg, nil, WithGetenv(noEnv)); !errors.Is(err, store.ErrLegacyImport) {
		t.Fatalf("New with broken state.json = %v; want ErrLegacyImport", err)
	}
	if content, err := os.ReadFile(statePath); err != nil || string(content) != `{"jobs": [` {
		t.Fatalf("broken state.json must be left untouched: %q, %v", content, err)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	tests := map[string]bool{
		"127.0.0.1": true,
		"127.0.0.5": true,
		"::1":       true,
		"[::1]":     true,
		"localhost": true,
		"LocalHost": true,
		"":          false,
		"0.0.0.0":   false,
		"::":        false,
		"10.0.0.1":  false,
		"example":   false,
	}
	for host, want := range tests {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v; want %v", host, got, want)
		}
	}
}

func TestAppCreatesTheDefaultLocalTarget(t *testing.T) {
	cfg := testConfig(t)
	application, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	def, err := application.targets.Resolve(context.Background(), "")
	want := filepath.Join(filepath.Dir(cfg.DataDir), config.DefaultBackupsDirName)
	if err != nil || def.Name != "Local disk" || def.Type != models.StorageLocal || def.Local.Path != want || !def.IsDefault {
		t.Fatalf("default target = %+v, %v; want Local disk at %s", def, err, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("backups directory not created: %v", err)
	}
}

// envMap returns a getenv backed by m.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestAppImportsDeprecatedEnvironmentOnce(t *testing.T) {
	cfg := testConfig(t)
	legacyBackups := filepath.Join(t.TempDir(), "legacy-backups")
	env := map[string]string{
		config.EnvBackupTimeout:  "2h",
		config.EnvRestoreVerify:  "always",
		config.EnvTrustProxy:     "true",
		config.EnvSecureCookies:  "true",
		config.EnvCORSOrigins:    "https://ops.example.com, https://b.example.com",
		config.EnvStorageType:    "local",
		config.EnvLocalPath:      legacyBackups,
		config.EnvAPIKey:         "legacy-static-api-key-123",
		config.EnvMetricsPublic:  "not-a-bool",
		config.EnvRestoreTimeout: "forever",
	}
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&lockedWriter{w: &logs}, nil))
	first, err := New(cfg, logger, WithGetenv(envMap(env)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cur := first.settings.Current()
	if cur.General.BackupTimeout.Std() != 2*time.Hour || cur.General.RestoreVerifyPolicy != models.VerifyAlways ||
		!cur.Security.TrustProxyHeaders || cur.Security.SecureCookies != settings.CookiesAlways ||
		len(cur.Security.CORSOrigins) != 2 || cur.General.RestoreTimeout.Std() != 12*time.Hour {
		t.Fatalf("imported settings = %+v", cur)
	}
	def, err := first.targets.Resolve(ctx, "")
	if err != nil || def.Local == nil || def.Local.Path != legacyBackups {
		t.Fatalf("default target = %+v, %v; want the imported local path", def, err)
	}
	if p, authErr := first.auth.AuthenticateAPIKey(ctx, env[config.EnvAPIKey]); authErr != nil || p.APIKeyID == "" {
		t.Fatalf("imported api key = %+v, %v", p, authErr)
	}
	out := logs.String()
	for _, want := range []string{
		config.EnvBackupTimeout + " is deprecated and was imported",
		config.EnvRestoreTimeout + " is deprecated and ignored",
		config.EnvMetricsPublic,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, env[config.EnvAPIKey]) {
		t.Fatal("the legacy api key must never be logged")
	}
	// The operator changes a setting in the dashboard.
	later := settings.Duration(3 * time.Hour)
	if _, err = first.settings.Update(ctx, settings.Patch{General: &settings.GeneralPatch{BackupTimeout: &later}}); err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}

	// Later starts ignore the variables: the dashboard value wins.
	env[config.EnvBackupTimeout] = "30m"
	logs.Reset()
	second, err := New(cfg, logger, WithGetenv(envMap(env)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if got := second.settings.Current().General.BackupTimeout.Std(); got != 3*time.Hour {
		t.Fatalf("backup timeout after restart = %v; want the dashboard value", got)
	}
	if !strings.Contains(logs.String(), config.EnvBackupTimeout+" is deprecated and ignored") {
		t.Fatalf("second start must warn that the variable is ignored:\n%s", logs.String())
	}
}

func TestAppImportsLegacyConfigFile(t *testing.T) {
	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	file := `{"defaults": {"retention_days": 7, "gzip": false}, "backup": {"stall_timeout": 300000000000},
		"server": {"metrics_public": true}}`
	if err := os.WriteFile(filepath.Join(cfg.DataDir, config.LegacyFileName), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	cur := application.settings.Current()
	if cur.General.DefaultRetentionDays != 7 || cur.General.DefaultGzip || cur.General.BackupStallTimeout.Std() != 5*time.Minute || !cur.Security.MetricsPublic {
		t.Fatalf("settings from config.json = %+v", cur)
	}
}

func TestAppRefusesAMovedDatabasePath(t *testing.T) {
	cfg := testConfig(t)
	elsewhere := filepath.Join(t.TempDir(), "old.db")
	if err := os.WriteFile(elsewhere, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(cfg, nil, WithGetenv(envMap(map[string]string{config.EnvDBPath: elsewhere})))
	if err == nil || !strings.Contains(err.Error(), config.EnvDBPath) {
		t.Fatalf("New with MONGORESCUE_DB_PATH elsewhere = %v; want a refusal", err)
	}
	// A path that does not exist (or is the default location) only warns.
	if app, err := New(cfg, nil, WithGetenv(envMap(map[string]string{config.EnvDBPath: elsewhere + ".missing"}))); err != nil {
		t.Fatalf("missing legacy db path: %v", err)
	} else {
		_ = app.Close()
	}
}

// lockedWriter serialises writes of concurrent loggers.
type lockedWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestAppResolvesLegacyRelativeStoragePathAgainstTheWorkingDirectory(t *testing.T) {
	cfg := testConfig(t)
	wd := t.TempDir()
	t.Chdir(wd)
	application, err := New(cfg, nil, WithGetenv(envMap(map[string]string{config.EnvLocalPath: "./legacy-backups"})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	def, err := application.targets.Resolve(context.Background(), "")
	want := filepath.Join(wd, "legacy-backups")
	if err != nil || def.Local == nil || !filepath.IsAbs(def.Local.Path) {
		t.Fatalf("default target = %+v, %v", def, err)
	}
	if got, _ := filepath.EvalSymlinks(def.Local.Path); got != mustEval(t, want) {
		t.Fatalf("legacy path stored as %q; want %q (resolved against the working directory)", def.Local.Path, want)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

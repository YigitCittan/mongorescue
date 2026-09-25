//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/metrics"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/server"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
	"github.com/yigitcittan/mongorescue/web"
)

// apiClient issues requests against the in-process server like the dashboard does
// (cookie jar + X-CSRF-Token) and records every response body so the test can assert
// that no credential ever leaves the API.
type apiClient struct {
	t      *testing.T
	base   string
	client *http.Client
	csrf   string
	apiKey string
	bodies []string
}

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

func newAPIClient(t *testing.T, base string) *apiClient {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &apiClient{t: t, base: base, client: &http.Client{Jar: jar, Timeout: 2 * opTimeout}}
}

func (c *apiClient) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		req.Header.Set(server.CSRFHeader, c.csrf)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("read %s %s: %v", method, path, err)
	}
	c.bodies = append(c.bodies, string(raw))
	return resp.StatusCode, raw
}

// data calls do, requires the expected status, and decodes the envelope data into out.
func (c *apiClient) data(method, path string, body any, want int, out any) {
	c.t.Helper()
	code, raw := c.do(method, path, body)
	if code != want {
		c.t.Fatalf("%s %s: status %d, want %d (body: %s)", method, path, code, want, raw)
	}
	if out == nil {
		return
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		c.t.Fatalf("%s %s: decode envelope: %v", method, path, err)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		c.t.Fatalf("%s %s: decode data: %v", method, path, err)
	}
}

// TestServerEndToEnd drives the full HTTP API in-process with real engines, a real
// metadata store, the real MongoDB driver adapter, local storage and the embedded
// dashboard: setup -> session -> connection -> job -> backup -> restore.
func TestServerEndToEnd(t *testing.T) {
	env := requireMongo(t)
	db := seedSource(t, env, "e2e")

	logger, logs := captureLogger()
	dir := t.TempDir()

	// Only the bootstrap options are set; storage, encryption and security are
	// configured through the API below, as in the dashboard.
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config validation: %v", err)
	}
	ctx := context.Background()

	metaStore := storetest.Open(t, cfg.MetadataDBPath())
	settingsSvc, err := settings.NewService(ctx, metaStore, settings.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	targetSvc := targets.NewService(metaStore, storage.NewForTarget, cfg.DataDir, targets.WithLogger(logger))
	bEngine := backup.NewEngine(nil, "", backup.WithLogger(logger),
		backup.WithStorageResolver(targetSvc.Storage),
		backup.WithRunConfig(func() backup.RunConfig {
			g := settingsSvc.Current().General
			return backup.RunConfig{Encryptor: settingsSvc.Encryptor(), Timeout: g.BackupTimeout.Std(), StallTimeout: g.BackupStallTimeout.Std()}
		}))
	rEngine := restore.NewEngine(nil, "", restore.WithLogger(logger),
		restore.WithStorageResolver(targetSvc.Storage),
		restore.WithRunConfig(func() restore.RunConfig {
			g := settingsSvc.Current().General
			return restore.RunConfig{Decryptor: settingsSvc.Decryptor(), VerifyPolicy: g.RestoreVerifyPolicy, Timeout: g.RestoreTimeout.Std()}
		}))

	// Event bus, metrics and notifications wired as in internal/app.
	metricSet := metrics.New(metrics.BuildInfo{Version: "e2e"})
	bus := events.NewBus(events.WithLogger(logger), events.WithDropHook(metricSet.IncEventsDropped))
	bus.Subscribe(metricSet.ObserveEvent)
	notifySvc := notify.NewService(metaStore, notify.WithLogger(logger))
	bus.Subscribe(notifySvc.HandleEvent)
	bgCtx, stopBackground := context.WithCancel(context.Background())
	bgDone := make(chan struct{}, 2)
	go func() { _ = bus.Run(bgCtx); bgDone <- struct{}{} }()
	go func() { _ = notifySvc.Run(bgCtx); bgDone <- struct{}{} }()
	t.Cleanup(func() {
		stopBackground()
		<-bgDone
		<-bgDone
	})

	connSvc := connections.NewService(metaStore, mongoconn.New(), connections.WithLogger(logger),
		connections.WithTestTimeout(5*time.Second))
	authSvc, err := auth.NewService(metaStore, auth.WithLogger(logger), auth.WithBcryptCost(bcrypt.MinCost))
	if err != nil {
		t.Fatal(err)
	}
	required, err := authSvc.Init(context.Background())
	if err != nil || !required {
		t.Fatalf("fresh database must start in setup mode: %v, %v", required, err)
	}

	sched := scheduler.NewScheduler(metaStore, bEngine, nil, logger,
		scheduler.WithPublisher(bus), scheduler.WithConnectionResolver(connSvc), scheduler.WithStorageTargets(targetSvc))
	t.Cleanup(sched.Stop)

	staticFS, err := web.GetSubFS()
	if err != nil {
		t.Fatalf("embedded ui: %v", err)
	}
	srv := server.NewServer(cfg, metaStore, bEngine, rEngine, nil, sched, staticFS, logger,
		server.WithSettings(settingsSvc),
		server.WithStorageTargets(targetSvc),
		server.WithEventPublisher(bus),
		server.WithNotifications(notifySvc),
		server.WithMetricsHandler(metricSet.Handler()),
		server.WithJobDeletedHook(metricSet.ForgetJob),
		server.WithAuth(authSvc),
		server.WithConnections(connSvc),
	)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	api := newAPIClient(t, ts.URL)

	// Public endpoints; everything else needs a session or an API key.
	if code, _ := api.do("GET", "/api/v1/health", nil); code != http.StatusOK {
		t.Fatalf("health: status %d", code)
	}
	var status struct {
		SetupRequired bool `json:"setup_required"`
	}
	api.data("GET", "/api/v1/setup/status", nil, http.StatusOK, &status)
	if !status.SetupRequired {
		t.Fatal("setup_required = false on a fresh database")
	}
	for _, path := range []string{"/metrics", "/api/v1/notifications/channels", "/api/v1/jobs", "/api/v1/connections", "/api/v1/settings", "/api/v1/storage-targets"} {
		if code, _ := api.do("GET", path, nil); code != http.StatusUnauthorized {
			t.Fatalf("GET %s without credentials: status %d, want 401", path, code)
		}
	}
	code, page := api.do("GET", "/", nil)
	if code != http.StatusOK || !strings.Contains(string(page), "app.js") {
		t.Fatalf("GET / did not serve the embedded dashboard (status %d)", code)
	}

	// First-run setup with the one-time code, then a second setup is refused.
	const password = "correct horse battery staple"
	setup := map[string]string{"setup_code": authSvc.SetupCode(), "username": "admin", "password": password}
	var session struct {
		User      auth.User `json:"user"`
		CSRFToken string    `json:"csrf_token"`
	}
	api.data("POST", "/api/v1/setup", setup, http.StatusCreated, &session)
	if session.CSRFToken == "" || session.User.Username != "admin" {
		t.Fatalf("unexpected setup response: %+v", session)
	}
	api.csrf = session.CSRFToken
	if code, _ := api.do("POST", "/api/v1/setup", setup); code != http.StatusConflict {
		t.Fatalf("second setup: status %d, want 409", code)
	}

	// Unsafe requests with the session cookie but without the CSRF token are refused.
	api.csrf = ""
	if code, _ := api.do("POST", "/api/v1/connections", map[string]string{"name": "x", "uri": env.URI}); code != http.StatusForbidden {
		t.Fatalf("POST without CSRF token: status %d, want 403", code)
	}
	api.csrf = session.CSRFToken

	// Test the URI from the form, save the connection, test it and discover databases.
	var probe connections.TestResult
	api.data("POST", "/api/v1/connections/test", map[string]string{"uri": env.URI}, http.StatusOK, &probe)
	if !probe.OK || probe.ServerVersion == "" {
		t.Fatalf("connection test of the real server failed: %+v", probe)
	}
	api.data("POST", "/api/v1/connections/test", map[string]string{"uri": "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=500"}, http.StatusOK, &probe)
	if probe.OK || probe.Error == "" {
		t.Fatalf("connection test of an unreachable server must fail: %+v", probe)
	}

	var conn models.Connection
	api.data("POST", "/api/v1/connections", map[string]string{"name": "integration", "uri": env.URI}, http.StatusCreated, &conn)
	if env.Password != "" && !strings.Contains(conn.URI, "******") {
		t.Fatalf("connection URI not redacted: %q", conn.URI)
	}
	api.data("POST", "/api/v1/connections/"+conn.ID+"/test", nil, http.StatusOK, &probe)
	if !probe.OK {
		t.Fatalf("stored connection test failed: %+v", probe)
	}
	var dbs []connections.Database
	api.data("GET", "/api/v1/connections/"+conn.ID+"/databases", nil, http.StatusOK, &dbs)
	if !slices.ContainsFunc(dbs, func(d connections.Database) bool { return d.Name == db }) ||
		slices.ContainsFunc(dbs, func(d connections.Database) bool { return d.Name == "admin" }) {
		t.Fatalf("databases = %+v; want %s and no system databases", dbs, db)
	}
	api.data("GET", "/api/v1/connections/"+conn.ID+"/databases?system=true", nil, http.StatusOK, &dbs)
	if !slices.ContainsFunc(dbs, func(d connections.Database) bool { return d.Name == "admin" }) {
		t.Fatalf("system=true must include admin: %+v", dbs)
	}
	var cols []connections.Collection
	api.data("GET", "/api/v1/connections/"+conn.ID+"/databases/"+db+"/collections", nil, http.StatusOK, &cols)
	if !slices.ContainsFunc(cols, func(c connections.Collection) bool { return c.Name == "orders" && c.Type == "collection" }) {
		t.Fatalf("collections = %+v; want orders", cols)
	}
	// Re-saving the redacted URI keeps the stored credentials.
	api.data("PUT", "/api/v1/connections/"+conn.ID, map[string]string{"name": "integration", "uri": conn.URI}, http.StatusOK, &conn)
	api.data("POST", "/api/v1/connections/"+conn.ID+"/test", nil, http.StatusOK, &probe)
	if !probe.OK {
		t.Fatalf("connection test after a redacted round trip failed: %+v", probe)
	}

	// Storage: the first target becomes the default; a probe object is written,
	// read back and deleted.
	var targetList []*models.StorageTarget
	api.data("GET", "/api/v1/storage-targets", nil, http.StatusOK, &targetList)
	if len(targetList) != 0 {
		t.Fatalf("fresh database has storage targets: %+v", targetList)
	}
	var target models.StorageTarget
	api.data("POST", "/api/v1/storage-targets", map[string]any{
		"name": "integration disk", "type": "local", "local": map[string]string{"path": filepath.Join(dir, "backups")},
	}, http.StatusCreated, &target)
	if !target.IsDefault || target.Local == nil || target.Local.Path != filepath.Join(dir, "backups") {
		t.Fatalf("created target = %+v; want the default local target", target)
	}
	var targetTest struct {
		OK        bool   `json:"ok"`
		LatencyMS int64  `json:"latency_ms"`
		Error     string `json:"error"`
	}
	api.data("POST", "/api/v1/storage-targets/"+target.ID+"/test", nil, http.StatusOK, &targetTest)
	if !targetTest.OK {
		t.Fatalf("local target test failed: %+v", targetTest)
	}
	if code, _ := api.do("POST", "/api/v1/storage-targets", map[string]any{
		"name": "escape", "type": "local", "local": map[string]string{"path": "../../etc"},
	}); code != http.StatusBadRequest {
		t.Fatalf("path traversal: status %d, want 400", code)
	}
	for _, p := range configuredS3Providers() {
		if p.CreateBucket {
			ensureBucket(ctx, t, p.Config)
		}
		s3in := map[string]any{"name": "s3 " + p.Name, "type": "s3", "s3": map[string]any{
			"endpoint": p.Config.Endpoint, "region": p.Config.Region, "bucket": p.Config.Bucket, "prefix": "it-e2e/",
			"access_key_id": p.Config.AccessKey, "secret_access_key": p.Config.SecretKey, "use_path_style": p.Config.UsePathStyle,
		}}
		api.data("POST", "/api/v1/storage-targets/test", s3in, http.StatusOK, &targetTest)
		if !targetTest.OK {
			t.Fatalf("%s: unsaved target test failed: %+v", p.Name, targetTest)
		}
		var s3t models.StorageTarget
		api.data("POST", "/api/v1/storage-targets", s3in, http.StatusCreated, &s3t)
		if s3t.IsDefault || s3t.S3 == nil || s3t.S3.SecretAccessKey != "******" {
			t.Fatalf("%s: created target = %+v; want a masked, non-default target", p.Name, s3t)
		}
		api.data("POST", "/api/v1/storage-targets/"+s3t.ID+"/test", nil, http.StatusOK, &targetTest)
		if !targetTest.OK {
			t.Fatalf("%s: stored target test failed: %+v", p.Name, targetTest)
		}
		api.data("DELETE", "/api/v1/storage-targets/"+s3t.ID, nil, http.StatusOK, nil)
		// Emulators use the same value for the (public) access key and the secret.
		if p.Config.SecretKey != p.Config.AccessKey {
			assertNoSecret(t, p.Config.SecretKey, "API responses", api.bodies...)
		}
	}

	// Settings: encrypt new backups with a generated key pair.
	var key struct {
		Identity  string `json:"identity"`
		Recipient string `json:"recipient"`
	}
	api.data("POST", "/api/v1/settings/encryption/generate-key", nil, http.StatusOK, &key)
	var cur struct {
		General    map[string]any `json:"general"`
		Encryption struct {
			Enabled  bool   `json:"enabled"`
			Identity string `json:"identity"`
		} `json:"encryption"`
		RestartRequired []string `json:"restart_required"`
	}
	api.data("PUT", "/api/v1/settings", map[string]any{"encryption": map[string]any{
		"enabled": true, "mode": "x25519", "recipients": []string{key.Recipient}, "identity": key.Identity,
	}}, http.StatusOK, &cur)
	if !cur.Encryption.Enabled || cur.Encryption.Identity != "******" || len(cur.RestartRequired) != 0 {
		t.Fatalf("settings after enabling encryption = %+v", cur)
	}
	if code, _ := api.do("PUT", "/api/v1/settings", map[string]any{"general": map[string]any{"backup_timeout": "soon"}}); code != http.StatusBadRequest {
		t.Fatalf("invalid duration: status %d, want 400", code)
	}
	api.data("PUT", "/api/v1/settings", map[string]any{"general": map[string]any{"backup_timeout": "2h"}}, http.StatusOK, &cur)
	if cur.General["backup_timeout"] != "2h0m0s" {
		t.Fatalf("backup_timeout = %v", cur.General["backup_timeout"])
	}

	// Create a job on the connection and trigger it.
	var job models.Job
	api.data("POST", "/api/v1/jobs", models.Job{
		Name:           "integration",
		Database:       db,
		CronExpression: "@daily",
		Enabled:        true,
		Gzip:           true,
		ConnectionID:   conn.ID,
	}, http.StatusCreated, &job)
	if job.ID == "" || job.StorageTargetID != target.ID {
		t.Fatalf("job = %+v; want the default storage target", job)
	}
	if code, _ := api.do("DELETE", "/api/v1/connections/"+conn.ID, nil); code != http.StatusConflict {
		t.Fatalf("deleting a connection used by a job: status %d, want 409", code)
	}

	var triggered models.BackupRecord
	// Runs are asynchronous: 202 Accepted with the in-progress record.
	api.data("POST", "/api/v1/jobs/"+job.ID+"/run", nil, http.StatusAccepted, &triggered)
	if triggered.Status != models.StatusInProgress {
		t.Fatalf("accepted run status = %q; want in_progress", triggered.Status)
	}

	// Poll the backup list until the run is visible as completed.
	var bkp *models.BackupRecord
	deadline := time.Now().Add(opTimeout)
	for bkp == nil && time.Now().Before(deadline) {
		var list []*models.BackupRecord
		api.data("GET", "/api/v1/backups?database="+db, nil, http.StatusOK, &list)
		for _, b := range list {
			if b.ID == triggered.ID && b.Status == models.StatusCompleted {
				bkp = b
			}
		}
		if bkp == nil {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if bkp == nil {
		t.Fatalf("backup %s never reached completed", triggered.ID)
	}
	if bkp.JobID != job.ID || bkp.SizeBytes <= 0 || len(bkp.SHA256) != 64 ||
		bkp.ConnectionID != conn.ID || bkp.ConnectionName != "integration" ||
		bkp.StorageTargetID != target.ID || bkp.StorageTargetName != "integration disk" ||
		!bkp.Encrypted || !strings.HasSuffix(bkp.StorageKey, ".age") {
		t.Fatalf("unexpected backup record: %+v", bkp)
	}
	if _, err := os.Stat(filepath.Join(dir, "backups", filepath.FromSlash(bkp.StorageKey))); err != nil {
		t.Fatalf("artifact not on the job's target: %v", err)
	}
	if code, _ := api.do("DELETE", "/api/v1/storage-targets/"+target.ID, nil); code != http.StatusConflict {
		t.Fatalf("deleting the target of a job and a backup: status %d, want 409", code)
	}

	// Rotate the key: the retired identity keeps decrypting the existing backup.
	api.data("POST", "/api/v1/settings/encryption/generate-key", nil, http.StatusOK, &key)
	api.data("PUT", "/api/v1/settings", map[string]any{"encryption": map[string]any{
		"enabled": true, "recipients": []string{key.Recipient}, "identity": key.Identity,
	}}, http.StatusOK, nil)

	// Restore through the API with the default safe clone into the backup's connection.
	var accepted models.RestoreRecord
	api.data("POST", "/api/v1/restore", models.RestoreRequest{BackupID: bkp.ID}, http.StatusAccepted, &accepted)
	var rst models.RestoreRecord
	for deadline := time.Now().Add(opTimeout); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		var list []*models.RestoreRecord
		api.data("GET", "/api/v1/restores", nil, http.StatusOK, &list)
		for _, r := range list {
			if r.ID == accepted.ID && r.Status != models.RestoreStatusInProgress {
				rst = *r
			}
		}
		if rst.ID != "" {
			break
		}
	}
	if rst.Status != models.RestoreStatusCompleted || !strings.HasPrefix(rst.TargetDatabase, db+"_rescue_") ||
		rst.TargetConnectionID != conn.ID {
		t.Fatalf("unexpected restore record: %+v", rst)
	}
	if got := env.count(t, rst.TargetDatabase, "orders"); got != ordersCount {
		t.Fatalf("restored orders = %d; want %d", got, ordersCount)
	}
	if got := env.count(t, db, "orders"); got != ordersCount {
		t.Fatalf("source orders = %d; want %d", got, ordersCount)
	}

	var restores []*models.RestoreRecord
	api.data("GET", "/api/v1/restores", nil, http.StatusOK, &restores)
	if len(restores) != 1 || restores[0].ID != rst.ID {
		t.Fatalf("restore history = %+v; want [%s]", restores, rst.ID)
	}
	var masked map[string]any
	api.data("GET", "/api/v1/settings", nil, http.StatusOK, &masked)
	encSettings, _ := masked["encryption"].(map[string]any)
	if retired, _ := encSettings["retired_keys"].([]any); len(retired) != 1 {
		t.Fatalf("retired keys = %v; want the rotated identity", encSettings["retired_keys"])
	}

	// An API key created in the session works without cookies and for /metrics.
	var created struct {
		APIKey auth.APIKey `json:"api_key"`
		Key    string      `json:"key"`
	}
	api.data("POST", "/api/v1/api-keys", map[string]string{"name": "ci"}, http.StatusCreated, &created)
	keyClient := &apiClient{t: t, base: ts.URL, client: &http.Client{Timeout: opTimeout}, apiKey: created.Key}
	keyClient.data("GET", "/api/v1/jobs", nil, http.StatusOK, nil)
	keyClient.data("DELETE", "/api/v1/backups/"+bkp.ID, nil, http.StatusOK, nil) // no CSRF needed
	metricsClient := &apiClient{t: t, base: ts.URL, client: &http.Client{Timeout: opTimeout}, apiKey: created.Key}
	if code, _ := metricsClient.do("GET", "/metrics", nil); code != http.StatusOK {
		t.Fatalf("metrics with an api key: status %d", code)
	}
	var after []*models.BackupRecord
	api.data("GET", "/api/v1/backups?database="+db, nil, http.StatusOK, &after)
	if len(after) != 0 {
		t.Fatalf("backup still listed after delete: %+v", after)
	}

	// Logout revokes the session.
	api.data("POST", "/api/v1/auth/logout", nil, http.StatusOK, nil)
	if code, _ := api.do("GET", "/api/v1/jobs", nil); code != http.StatusUnauthorized {
		t.Fatalf("after logout: status %d, want 401", code)
	}

	all := append(append(api.bodies, keyClient.bodies...), metricsClient.bodies...)
	assertNoSecret(t, env.Password, "API responses", all...)
	assertNoSecret(t, env.Password, "server logs", logs.String())
	assertNoSecret(t, password, "server logs", logs.String())
	assertNoSecret(t, created.Key, "server logs", logs.String())
	// Identities appear only in the generate-key responses, never in settings or logs.
	maskedJSON, _ := json.Marshal(masked)
	for _, secret := range []string{key.Identity, strings.TrimPrefix(key.Identity, "AGE-SECRET-KEY-")} {
		assertNoSecret(t, secret, "settings responses", string(maskedJSON))
		assertNoSecret(t, secret, "server logs", logs.String())
	}
}

// TestMongoConnProber exercises the driver adapter directly against the real server.
func TestMongoConnProber(t *testing.T) {
	env := requireMongo(t)
	db := seedSource(t, env, "probe")
	p := mongoconn.New()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	info, err := p.Ping(ctx, env.URI)
	if err != nil || info.Version == "" {
		t.Fatalf("Ping = %+v, %v", info, err)
	}
	dbs, err := p.ListDatabases(ctx, env.URI)
	if err != nil || !slices.ContainsFunc(dbs, func(d connections.Database) bool { return d.Name == db && !d.Empty }) {
		t.Fatalf("ListDatabases = %+v, %v", dbs, err)
	}
	cols, err := p.ListCollections(ctx, env.URI, db)
	if err != nil || !slices.ContainsFunc(cols, func(c connections.Collection) bool { return c.Name == "orders" }) {
		t.Fatalf("ListCollections = %+v, %v", cols, err)
	}
	if env.Password != "" {
		bad := strings.Replace(env.URI, env.Password, "wrong-password", 1)
		if _, err := p.Ping(ctx, bad); err == nil {
			t.Fatal("Ping with a wrong password must fail")
		} else {
			assertNoSecret(t, "wrong-password", "prober errors", err.Error())
		}
	}
}

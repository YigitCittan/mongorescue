package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yigitcittan/mongorescue/internal/mcp"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// Secrets planted by TestNoSecretLeavesTheServer. Each carries a marker that must
// never appear in a response, a header, a log line or the database files.
const (
	leakAdminPassword = "LEAKadminPASSWORD-long"
	leakConnPassword  = "LEAKconnPW7x"
	leakTestPassword  = "LEAKadhocS3SECRET"
	leakHMAC          = "LEAKhmacSECRET"
	leakHeader        = "LEAKhdrTOKEN"
	leakWebhookPath   = "LEAKwhPATH"
	leakWebhookQuery  = "LEAKwhQUERY"
	leakTelegram      = "LEAKtgTOKENabcdefghijkl"
	leakSMTP          = "LEAKsmtpPW"
	leakTwilio        = "LEAKtwilioTOKEN"
	// leakS3Access is an access key ID: an identifier shown in the dashboard like a
	// user name, so it is not checked; the secret access key is.
	leakS3Access   = "s3-access-key-id"
	leakS3Secret   = "LEAKs3SECRET"
	leakPassphrase = "LEAKpassphrase-long-enough-123"
)

// leakBuffer is a goroutine-safe log sink.
type leakBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *leakBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *leakBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// exchange is one recorded HTTP response.
type exchange struct {
	label  string
	status int
	header http.Header
	body   string
	// secret marks the responses that return a secret by design, once: a new API
	// key and a generated encryption key.
	secret bool
}

// recorder records every response that passes through it.
type recorder struct {
	mu    sync.Mutex
	base  http.RoundTripper
	seen  []exchange
	label string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	r.mu.Lock()
	r.seen = append(r.seen, exchange{label: r.label + " " + req.Method + " " + req.URL.RequestURI(), status: resp.StatusCode, header: resp.Header.Clone(), body: string(raw)})
	r.mu.Unlock()
	return resp, nil
}

// leakClient drives the API like the dashboard (cookie and CSRF token) or like an
// automation client (API key).
type leakClient struct {
	t      *testing.T
	base   string
	http   *http.Client
	rec    *recorder
	csrf   string
	apiKey string
}

func (c *leakClient) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	} else if method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// data decodes the envelope's data field of a successful response.
func (c *leakClient) data(method, path string, body any, want int, v any) {
	c.t.Helper()
	code, raw := c.do(method, path, body)
	if code != want {
		c.t.Fatalf("%s %s = %d %s; want %d", method, path, code, raw, want)
	}
	if v == nil {
		return
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		c.t.Fatalf("%s %s data: %v (%s)", method, path, err, env.Data)
	}
}

// markSecret flags the last recorded response as one that returns a secret by design.
func (r *recorder) markSecret() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen[len(r.seen)-1].secret = true
}

// TestNoSecretLeavesTheServer plants a secret of every kind (admin password,
// connection passwords, webhook URL secrets, HMAC and header tokens, Telegram, SMTP
// and Twilio credentials, S3 keys, the encryption passphrase and identity, an API
// key), then exercises every read endpoint, the failing connection, storage and
// notification tests, /metrics, the dashboard and the MCP tools and resources, as a
// session and as an API key. No response body or header, no log line and no
// database file may contain any of them.
func TestNoSecretLeavesTheServer(t *testing.T) {
	logs := &leakBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := testConfig(t)
	cfg.Dashboard = true
	application, err := New(cfg, logger, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	ts := httptest.NewServer(application.Handler())
	t.Cleanup(ts.Close)

	rec := &recorder{base: http.DefaultTransport, label: "session"}
	jar, _ := cookiejar.New(nil)
	s := &leakClient{t: t, base: ts.URL, http: &http.Client{Transport: rec, Jar: jar}, rec: rec}

	// Setup and configuration, as the dashboard.
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	s.data("POST", "/api/v1/setup", map[string]string{"setup_code": application.SetupCode(), "username": "admin", "password": leakAdminPassword}, http.StatusCreated, &session)
	s.csrf = session.CSRFToken

	var generated struct {
		Identity  string `json:"identity"`
		Recipient string `json:"recipient"`
	}
	s.data("POST", "/api/v1/settings/encryption/generate-key", nil, http.StatusOK, &generated)
	rec.markSecret()
	if !strings.HasPrefix(generated.Identity, "AGE-SECRET-KEY-1") {
		t.Fatalf("generated identity %q", generated.Identity)
	}
	s.data("PUT", "/api/v1/settings", map[string]any{
		"security":   map[string]any{"mcp_enabled": true},
		"encryption": map[string]any{"enabled": true, "mode": "passphrase", "passphrase": leakPassphrase, "identity": generated.Identity},
	}, http.StatusOK, nil)

	var key struct {
		Key string `json:"key"`
	}
	s.data("POST", "/api/v1/api-keys", map[string]string{"name": "ci", "scope": "admin"}, http.StatusCreated, &key)
	rec.markSecret()
	keySecret := key.Key[strings.LastIndex(key.Key, "_")+1:]

	fast := "serverSelectionTimeoutMS=300&connectTimeoutMS=300"
	var conn models.Connection
	s.data("POST", "/api/v1/connections", map[string]string{"name": "prod",
		"uri": "mongodb://admin:" + leakConnPassword + "@127.0.0.1:1/?authSource=admin&" + fast}, http.StatusCreated, &conn)

	// A fake S3 endpoint that refuses every request with a non-retryable 403, so the
	// storage tests fail at once instead of retrying against a closed port.
	s3stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
	}))
	t.Cleanup(s3stub.Close)

	var target models.StorageTarget
	s.data("POST", "/api/v1/storage-targets", map[string]any{"name": "offsite", "type": "s3", "s3": map[string]any{
		"endpoint": s3stub.URL, "region": "us-east-1", "bucket": "backups", "use_path_style": true,
		"access_key_id": leakS3Access, "secret_access_key": leakS3Secret,
	}}, http.StatusCreated, &target)

	channels := []map[string]any{
		{"id": "hook", "name": "Hook", "type": "webhook", "enabled": true, "webhook": map[string]any{
			"url":     "http://127.0.0.1:1/services/" + leakWebhookPath + "?token=" + leakWebhookQuery,
			"secret":  leakHMAC,
			"headers": map[string]string{"Authorization": "Bearer " + leakHeader, "X-Api-Key": leakHeader},
		}},
		{"id": "mail", "name": "Mail", "type": "email", "enabled": true, "email": map[string]any{
			"host": "127.0.0.1", "port": 1, "security": "none", "username": "ops", "password": leakSMTP,
			"from": "backup@example.com", "to": []string{"ops@example.com"},
		}},
		{"id": "tg", "name": "TG", "type": "telegram", "enabled": true, "telegram": map[string]any{"bot_token": "123456:" + leakTelegram, "chat_id": "42"}},
		{"id": "sms", "name": "SMS", "type": "twilio", "enabled": true, "twilio": map[string]any{
			"account_sid": "AC" + strings.Repeat("0", 32), "auth_token": leakTwilio, "from": "+15551234567", "to": []string{"+15557654321"},
		}},
	}
	for _, ch := range channels {
		s.data("POST", "/api/v1/notifications/channels", ch, http.StatusCreated, nil)
	}
	s.data("POST", "/api/v1/notifications/rules", map[string]any{"id": "all", "name": "All", "enabled": true,
		"events": []string{"backup.failed"}, "channel_ids": []string{"hook", "mail", "tg", "sms"}}, http.StatusCreated, nil)
	var job models.Job
	s.data("POST", "/api/v1/jobs", map[string]any{"name": "nightly", "database": "shop", "connection_id": conn.ID, "cron_expression": "@daily"}, http.StatusCreated, &job)

	// History the read endpoints and MCP resources serialise.
	ctx := context.Background()
	now := time.Now().UTC()
	backup := &models.BackupRecord{ID: "bkp_shop_1", JobID: job.ID, Database: "shop", ConnectionID: conn.ID, ConnectionName: conn.Name,
		StorageTargetID: target.ID, StorageTargetName: target.Name, Status: models.StatusFailed, StartedAt: now,
		StorageKey: "shop/2026/09/bkp_shop_1.archive.gz", ErrorMessage: "mongodump failed"}
	if err = application.metaStore.SaveBackupRecord(ctx, backup); err != nil {
		t.Fatal(err)
	}
	restore := &models.RestoreRecord{ID: "rst_shop_1", BackupID: backup.ID, SourceDatabase: "shop", TargetDatabase: "shop_rescue_1",
		SourceConnectionID: conn.ID, TargetConnectionID: conn.ID, Status: models.RestoreStatusFailed, StartedAt: now, ErrorMessage: "mongorestore failed"}
	if err = application.metaStore.SaveRestoreRecord(ctx, restore); err != nil {
		t.Fatal(err)
	}

	// Failing tests: their errors come from the S3 client and the notifiers, and must
	// be scrubbed. (Connection tests and discovery wait for the driver's server
	// selection timeout; their redaction is covered with a fake driver in the server
	// and mcp packages.)
	s.do("POST", "/api/v1/storage-targets/"+target.ID+"/test", nil)
	s.do("POST", "/api/v1/storage-targets/test", map[string]any{"name": "adhoc", "type": "s3", "s3": map[string]any{
		"endpoint": s3stub.URL, "region": "us-east-1", "bucket": "b", "use_path_style": true,
		"access_key_id": "adhoc", "secret_access_key": leakTestPassword,
	}})
	for _, id := range []string{"hook", "mail"} {
		s.do("POST", "/api/v1/notifications/channels/"+id+"/test", nil)
	}
	// Echoing a masked secret into an update must not unmask it.
	s.do("PUT", "/api/v1/connections/"+conn.ID, map[string]string{"name": "prod", "uri": "mongodb://admin:******@127.0.0.1:1/?authSource=admin&" + fast})
	// A storage scan of the S3 target fails against the stub; its error is stored in
	// the drift report and must be scrubbed too.
	s.do("POST", "/api/v1/storage-targets/"+target.ID+"/scan", nil)

	reads := []string{
		"/api/v1/integrity", "/api/v1/storage-targets/" + target.ID + "/scan",
		"/api/v1/jobs/" + job.ID + "/retention/preview", "/api/v1/jobs/" + job.ID + "/retention/log",
		"/api/v1/jobs/" + job.ID + "/restore-tests",
		"/", "/index.html", "/app.js", "/trust.js", "/api/v1/health", "/api/v1/setup/status", "/api/v1/auth/me",
		"/api/v1/users", "/api/v1/api-keys", "/api/v1/connections", "/api/v1/connections/" + conn.ID,
		"/api/v1/settings", "/api/v1/storage-targets", "/api/v1/storage-targets/" + target.ID, "/api/v1/stats",
		"/api/v1/jobs", "/api/v1/jobs/" + job.ID, "/api/v1/backups", "/api/v1/backups?database=shop", "/api/v1/restores",
		"/api/v1/backups/" + backup.ID + "/collections",
		"/api/v1/notifications/channels", "/api/v1/notifications/rules", "/api/v1/audit",
		"/api/v1/connections/nope", "/api/v1/jobs/nope", "/api/v1/storage-targets/nope",
	}
	for _, path := range reads {
		s.do("GET", path, nil)
	}

	// The same reads and /metrics with the API key.
	rec.label = "api key"
	k := &leakClient{t: t, base: ts.URL, http: &http.Client{Transport: rec}, rec: rec, apiKey: key.Key}
	for _, path := range append(reads, "/metrics") {
		k.do("GET", path, nil)
	}

	// MCP: every read tool, every resource and the prompt list, over Streamable HTTP.
	rec.label = "mcp"
	mcpHTTP := &http.Client{Transport: bearerTransport{key: key.Key, base: rec}}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "leak-test", Version: "v0"}, nil).Connect(ctx,
		&sdk.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: mcpHTTP}, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	// list_databases and list_collections contact the server; see above.
	discovery := map[string]bool{mcp.ToolListDatabases: true, mcp.ToolListCollections: true, mcp.ToolPreviewJobDatabases: true}
	calls := map[string]map[string]any{
		mcp.ToolListConnections:       {},
		mcp.ToolListJobs:              {},
		mcp.ToolGetJob:                {"id": job.ID},
		mcp.ToolListBackups:           {},
		mcp.ToolGetBackup:             {"id": backup.ID},
		mcp.ToolListRestores:          {},
		mcp.ToolGetRestore:            {"id": restore.ID},
		mcp.ToolListStorageTargets:    {},
		mcp.ToolGetStatus:             {},
		mcp.ToolListBackupCollections: {"backup_id": backup.ID},
		mcp.ToolRetentionPreview:      {"job_id": job.ID},
		mcp.ToolListJobRuns:           {"job_id": job.ID},
	}
	for tool, args := range calls {
		if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args}); err != nil {
			t.Errorf("mcp %s: %v", tool, err)
		}
	}
	for tool, scope := range mcp.ToolScopes {
		if _, ok := calls[tool]; !ok && scope == "read" && !discovery[tool] {
			t.Errorf("read tool %s is not exercised; add it to the sweep", tool)
		}
	}
	for _, uri := range []string{mcp.StatusURI, "mongorescue://backups/" + backup.ID, "mongorescue://jobs/" + job.ID, "mongorescue://restores/" + restore.ID} {
		if _, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri}); err != nil {
			t.Errorf("mcp resource %s: %v", uri, err)
		}
	}
	if _, err := cs.ListPrompts(ctx, nil); err != nil {
		t.Errorf("mcp prompts: %v", err)
	}
	_ = cs.Close()

	// The sweep must have reached every kind of output.
	rec.mu.Lock()
	seen := append([]exchange(nil), rec.seen...)
	rec.mu.Unlock()
	if len(seen) < 80 {
		t.Fatalf("only %d responses recorded", len(seen))
	}

	markers := map[string]string{
		"admin password": leakAdminPassword, "connection password": leakConnPassword, "tested S3 secret": leakTestPassword,
		"webhook HMAC secret": leakHMAC, "webhook header token": leakHeader, "webhook URL path": leakWebhookPath,
		"webhook URL query": leakWebhookQuery, "Telegram bot token": leakTelegram, "SMTP password": leakSMTP,
		"Twilio auth token": leakTwilio, "S3 secret key": leakS3Secret,
		"encryption passphrase": leakPassphrase, "encryption identity": generated.Identity[len("AGE-SECRET-KEY-1"):], "API key": keySecret,
	}
	for _, x := range seen {
		if x.secret {
			if x.header.Get("Cache-Control") != "no-store" {
				t.Errorf("%s returns a secret without Cache-Control: no-store", x.label)
			}
			continue
		}
		var headers strings.Builder
		for name, values := range x.header {
			fmt.Fprintf(&headers, "%s: %s\n", name, strings.Join(values, ", "))
		}
		for what, m := range markers {
			if strings.Contains(x.body, m) || strings.Contains(headers.String(), m) {
				t.Errorf("%s (%d) leaks the %s: %.400s", x.label, x.status, what, x.body)
			}
		}
	}
	allLogs := logs.String()
	for what, m := range markers {
		if strings.Contains(allLogs, m) {
			t.Errorf("the log leaks the %s", what)
		}
	}
	if !strings.Contains(allLogs, "connection created") {
		t.Fatal("the log hook captured nothing useful")
	}

	// At rest: the metadata database stores secrets encrypted, API keys and passwords
	// as hashes.
	files, _ := filepath.Glob(filepath.Join(cfg.DataDir, "mongorescue.db*"))
	if len(files) == 0 {
		t.Fatal("no database files")
	}
	for _, name := range files {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for what, m := range markers {
			if bytes.Contains(raw, []byte(m)) {
				t.Errorf("%s stores the %s in plain text", filepath.Base(name), what)
			}
		}
	}
}

// bearerTransport adds an API key to every request.
type bearerTransport struct {
	key  string
	base http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return b.base.RoundTrip(r)
}

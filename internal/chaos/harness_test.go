//go:build chaos

// Package chaos is the fault-injection suite (build tag "chaos"): it runs the real
// mongorescue binary against MongoDB and MinIO behind Toxiproxy, injects failures in
// the middle of backups, restores, retention, migrations and key rotations (cut,
// slowed or black-holed connections, a replica set stepdown, a full disk, SIGKILL,
// clock steps) and checks what the issue behind it asks of every scenario: a broken
// run never ends completed, no object that looks complete is left behind, the next
// run succeeds and the alerts fire. scripts/test-chaos-docker.sh starts the services
// and sets the MONGORESCUE_CHAOS_* variables; docs/testing.md lists the scenarios
// and their expected outcomes.
package chaos

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/server"
)

// Environment set by scripts/test-chaos-docker.sh.
const (
	envToxiproxy     = "MONGORESCUE_CHAOS_TOXIPROXY_URL"
	envMongoURI      = "MONGORESCUE_CHAOS_MONGO_URI"
	envMongoProxyURI = "MONGORESCUE_CHAOS_MONGO_PROXY_URI"
	envS3Endpoint    = "MONGORESCUE_CHAOS_S3_ENDPOINT"
	envS3Proxy       = "MONGORESCUE_CHAOS_S3_PROXY_ENDPOINT"
	envS3Bucket      = "MONGORESCUE_CHAOS_S3_BUCKET"
	envS3AccessKey   = "MONGORESCUE_CHAOS_S3_ACCESS_KEY"
	envS3SecretKey   = "MONGORESCUE_CHAOS_S3_SECRET_KEY"
	envSmallDir      = "MONGORESCUE_CHAOS_SMALL_DIR"
	envReportDir     = "MONGORESCUE_CHAOS_REPORT_DIR"
)

// Toxiproxy proxy names (created by the script).
const (
	proxyMongo = "mongo"
	proxyMinio = "minio"
)

// opTimeout bounds one API call or driver operation; runTimeout one backup or restore.
const (
	opTimeout  = 2 * time.Minute
	runTimeout = 5 * time.Minute
	adminUser  = "chaos-admin"
	adminPass  = "chaos-password-1"
)

// env holds the services of the suite.
type env struct {
	MongoURI, MongoProxyURI, MongoPassword string
	Mongo                                  *mongo.Client
	Toxi                                   *toxiproxy
	S3                                     *s3.Client
	S3Endpoint, S3Proxy, Bucket            string
	AccessKey, SecretKey                   string
}

// requireEnv skips the test unless the chaos services are configured.
func requireEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		MongoURI: os.Getenv(envMongoURI), MongoProxyURI: os.Getenv(envMongoProxyURI),
		S3Endpoint: os.Getenv(envS3Endpoint), S3Proxy: os.Getenv(envS3Proxy), Bucket: os.Getenv(envS3Bucket),
		AccessKey: os.Getenv(envS3AccessKey), SecretKey: os.Getenv(envS3SecretKey),
	}
	if e.MongoURI == "" || e.MongoProxyURI == "" || e.S3Endpoint == "" || os.Getenv(envToxiproxy) == "" {
		t.Skip("the chaos services are not configured; run scripts/test-chaos-docker.sh")
	}
	for _, bin := range []string{"mongodump", "mongorestore"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s is not on PATH: %v", bin, err)
		}
	}
	if u, err := url.Parse(e.MongoURI); err == nil {
		e.MongoPassword, _ = u.User.Password()
	}
	client, err := mongo.Connect(options.Client().ApplyURI(e.MongoURI).SetTimeout(opTimeout))
	if err != nil {
		t.Fatalf("connect mongo: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = client.Disconnect(ctx)
	})
	e.Mongo = client
	e.Toxi = &toxiproxy{base: os.Getenv(envToxiproxy)}
	e.Toxi.reset(t)
	t.Cleanup(func() { e.Toxi.reset(t) })
	e.S3 = s3.New(s3.Options{
		BaseEndpoint: aws.String(e.S3Endpoint), Region: "us-east-1", UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(e.AccessKey, e.SecretKey, ""),
	})
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if _, err := e.S3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(e.Bucket)}); err != nil {
		if _, err := e.S3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(e.Bucket)}); err != nil {
			t.Fatalf("create bucket %s: %v", e.Bucket, err)
		}
	}
	return e
}

// randomHex returns n random bytes, hex-encoded.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// uniqueDB returns a fresh database name and drops it and its clones on cleanup.
func (e *env) uniqueDB(t *testing.T, tag string) string {
	t.Helper()
	name := fmt.Sprintf("ch_%s_%s", tag, randomHex(3))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		names, err := e.Mongo.ListDatabaseNames(ctx, bson.D{{Key: "name", Value: bson.D{{Key: "$regex", Value: "^" + name}}}})
		if err != nil {
			return
		}
		for _, n := range names {
			_ = e.Mongo.Database(n).Drop(ctx)
		}
	})
	return name
}

// seedBlobs fills db.coll with about mib MiB of incompressible 256 KiB documents,
// numbered by seq.
func (e *env) seedBlobs(t *testing.T, db, coll string, mib int) {
	t.Helper()
	const docSize = 256 << 10
	n := mib * 4
	ctx, cancel := context.WithTimeout(context.Background(), 5*opTimeout)
	defer cancel()
	for start := 0; start < n; start += 16 {
		docs := make([]any, 0, 16)
		for i := start; i < start+16 && i < n; i++ {
			blob := make([]byte, docSize)
			_, _ = rand.Read(blob)
			docs = append(docs, bson.D{{Key: "seq", Value: i}, {Key: "blob", Value: bson.Binary{Data: blob}}})
		}
		if _, err := e.Mongo.Database(db).Collection(coll).InsertMany(ctx, docs); err != nil {
			t.Fatalf("seed %s.%s: %v", db, coll, err)
		}
	}
}

// count returns the number of documents in db.coll.
func (e *env) count(t *testing.T, db, coll string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	n, err := e.Mongo.Database(db).Collection(coll).CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("count %s.%s: %v", db, coll, err)
	}
	return n
}

// databaseExists reports whether db exists.
func (e *env) databaseExists(t *testing.T, db string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	names, err := e.Mongo.ListDatabaseNames(ctx, bson.D{{Key: "name", Value: db}})
	if err != nil {
		t.Fatalf("list databases: %v", err)
	}
	return len(names) > 0
}

// --- Toxiproxy ------------------------------------------------------------------

// toxiproxy drives the Toxiproxy HTTP API.
type toxiproxy struct{ base string }

func (tp *toxiproxy) call(t *testing.T, method, path string, body any, want ...int) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, tp.base+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("toxiproxy %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	ok := resp.StatusCode/100 == 2
	for _, w := range want {
		ok = ok || resp.StatusCode == w
	}
	if !ok {
		t.Fatalf("toxiproxy %s %s: %d %s", method, path, resp.StatusCode, raw)
	}
}

// reset enables every proxy and removes every toxic.
func (tp *toxiproxy) reset(t *testing.T) { tp.call(t, "POST", "/reset", nil) }

// toxic adds a toxic of typ to proxy on stream ("upstream": client to server,
// "downstream": server to client).
func (tp *toxiproxy) toxic(t *testing.T, proxy, name, typ, stream string, attrs map[string]any) {
	t.Helper()
	tp.call(t, "POST", "/proxies/"+proxy+"/toxics", map[string]any{
		"name": name, "type": typ, "stream": stream, "toxicity": 1.0, "attributes": attrs,
	})
}

// setEnabled enables or disables a proxy; disabling closes its connections and
// refuses new ones (an outage).
func (tp *toxiproxy) setEnabled(t *testing.T, proxy string, enabled bool) {
	t.Helper()
	tp.call(t, "POST", "/proxies/"+proxy, map[string]any{"enabled": enabled})
}

// --- The process under test ------------------------------------------------------

var (
	buildOnce sync.Once
	binPath   string
	errBuild  error
)

// binary builds cmd/mongorescue once per test process.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mongorescue-chaos-bin-")
		if err != nil {
			errBuild = err
			return
		}
		binPath = filepath.Join(dir, "mongorescue")
		cmd := exec.Command("go", "build", "-o", binPath, "github.com/yigitcittan/mongorescue/cmd/mongorescue")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			errBuild = fmt.Errorf("build mongorescue: %w\n%s", err, out)
		}
	})
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	return binPath
}

// syncBuffer is a goroutine-safe buffer for process output. With killWhen set, the
// process is killed from inside the Write that makes killWhen true, so the kill
// follows the output that triggered it within microseconds.
type syncBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	killWhen  func(output string) bool
	process   atomic.Pointer[os.Process]
	killFired bool
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if b.killWhen != nil && !b.killFired && b.killWhen(b.buf.String()) {
		if pr := b.process.Load(); pr != nil {
			_ = pr.Signal(syscall.SIGKILL)
			b.killFired = true
		}
	}
	return n, err
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// proc is one run of the mongorescue server on a data directory.
type proc struct {
	t       *testing.T
	bin     string
	dataDir string
	port    int
	base    string
	cmd     *exec.Cmd
	done    chan error
	logs    *syncBuffer
	// exited is set once the process was reaped.
	exited bool
}

// freePort returns a free loopback TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// cleanEnv is the environment without MONGORESCUE_* variables.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MONGORESCUE_") {
			out = append(out, kv)
		}
	}
	return out
}

// launch starts bin on dataDir without waiting for it to be healthy.
func launch(t *testing.T, bin, dataDir string, port int) *proc {
	t.Helper()
	return launchKilling(t, bin, dataDir, port, nil)
}

// launchKilling starts bin like launch and kills it as soon as its output makes
// killWhen true (nil: never).
func launchKilling(t *testing.T, bin, dataDir string, port int, killWhen func(output string) bool) *proc {
	t.Helper()
	p := &proc{t: t, bin: bin, dataDir: dataDir, port: port, logs: &syncBuffer{killWhen: killWhen}, done: make(chan error, 1)}
	p.base = fmt.Sprintf("http://127.0.0.1:%d", port)
	p.cmd = exec.Command(bin, "-data-dir", dataDir, "-host", "127.0.0.1", "-port", strconv.Itoa(port))
	// The full-disk scenarios run on a 48 MiB filesystem: a 1 MiB minimum keeps
	// the pre-run free-space check on without refusing every run there.
	p.cmd.Env = append(cleanEnv(), "MONGORESCUE_MIN_FREE_SPACE_MB=1")
	p.cmd.Stdout, p.cmd.Stderr = p.logs, p.logs
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.logs.process.Store(p.cmd.Process)
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() {
		p.stop()
		saveLogs(t, p)
	})
	return p
}

// startProc starts bin on dataDir and waits until it answers /api/v1/health.
func startProc(t *testing.T, bin, dataDir string, port int) *proc {
	t.Helper()
	p := launch(t, bin, dataDir, port)
	p.waitHealthy()
	return p
}

// waitHealthy waits until the process answers /api/v1/health with 200.
func (p *proc) waitHealthy() {
	p.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get(p.base + "/api/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case err := <-p.done:
			p.exited = true
			p.t.Fatalf("server exited: %v\n%s", err, p.logs.String())
		default:
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("server did not become healthy\n%s", p.logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// kill sends SIGKILL and reaps the process.
func (p *proc) kill() {
	p.t.Helper()
	if p.exited {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	<-p.done
	p.exited = true
}

// stop shuts the process down gracefully (SIGINT), killing it after 60 seconds.
func (p *proc) stop() {
	if p.exited {
		return
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
	case <-time.After(60 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	p.exited = true
}

// saveLogs writes the process output to the report directory (when set) and logs
// its tail when the test failed.
func saveLogs(t *testing.T, p *proc) {
	out := p.logs.String()
	if dir := os.Getenv(envReportDir); dir != "" {
		name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
		f := filepath.Join(dir, fmt.Sprintf("%s-%d.log", name, time.Now().UnixNano()))
		_ = os.WriteFile(f, []byte(out), 0o600)
	}
	if t.Failed() {
		if len(out) > 8000 {
			out = out[len(out)-8000:]
		}
		t.Logf("server output (tail):\n%s", out)
	}
}

// --- API client -----------------------------------------------------------------

// api calls the REST API with an admin API key, or with a session and CSRF token.
type api struct {
	t      *testing.T
	base   string
	client *http.Client
	key    string
	csrf   string
}

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

func newAPI(t *testing.T, base string) *api {
	jar, _ := cookiejar.New(nil)
	return &api{t: t, base: base, client: &http.Client{Jar: jar, Timeout: opTimeout}}
}

// do sends a request and returns the status and body; a transport error fails the test.
func (c *api) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	code, raw, err := c.try(method, path, body)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return code, raw
}

// try sends a request and returns the transport error instead of failing.
func (c *api) try(method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		req.Header.Set(server.CSRFHeader, c.csrf)
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

// data requires status want and decodes the envelope's data into out.
func (c *api) data(method, path string, body any, want int, out any) {
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
		c.t.Fatalf("%s %s: decode data: %v (%s)", method, path, err, env.Data)
	}
}

// login opens a session for the admin user (key rotation needs one).
func (c *api) login() {
	c.t.Helper()
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	c.data("POST", "/api/v1/auth/login", map[string]string{"username": adminUser, "password": adminPass}, http.StatusOK, &session)
	c.csrf = session.CSRFToken
}

var setupCodePattern = regexp.MustCompile(`setup_code=([A-Z2-7]{4}(?:-[A-Z2-7]{4})+)`)

// --- Webhook receiver --------------------------------------------------------------

// hook receives webhook notifications.
type hook struct {
	srv    *httptest.Server
	mu     sync.Mutex
	events []map[string]any
}

func newHook(t *testing.T) *hook {
	h := &hook{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			h.mu.Lock()
			h.events = append(h.events, payload)
			h.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// find returns the received events of type typ whose field key equals value
// (any value when key is empty).
func (h *hook) find(typ, key, value string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, e := range h.events {
		if e["event"] != typ {
			continue
		}
		if key != "" && fmt.Sprint(e[key]) != value {
			continue
		}
		out = append(out, e)
	}
	return out
}

// wait waits until an event of type typ with key=value arrives.
func (h *hook) wait(t *testing.T, typ, key, value string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.find(typ, key, value); len(got) > 0 {
			return got[0]
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t.Fatalf("no %s event with %s=%s; received %v", typ, key, value, h.events)
	return nil
}

// --- A configured installation --------------------------------------------------

// rig is a running, configured MongoRescue: an admin, an admin API key, a
// connection through the MongoDB proxy, an S3 target through the MinIO proxy (the
// default) and a webhook channel with a rule for every alert the suite checks.
type rig struct {
	t        *testing.T
	env      *env
	bin      string
	dataDir  string
	port     int
	proc     *proc
	api      *api
	hook     *hook
	connID   string
	targetID string
	prefix   string
}

// rigOptions customise newRig.
type rigOptions struct {
	// dataDir overrides the data directory (default: a temporary directory).
	dataDir string
	// mongoURI overrides the connection URI (default: through the proxy).
	mongoURI string
}

// alertEvents are the events the webhook rule subscribes to.
var alertEvents = []string{
	"backup.failed", "backup.succeeded", "restore.failed", "restore.succeeded",
	"job.rpo_missed", "job.rpo_recovered", "security.key_rotated",
	"pitr.chain_broken", "pitr.collector_failed", "pitr.collector_recovered",
	"metadata_backup.failed", "storage.drift_detected",
}

func newRig(t *testing.T, e *env, o rigOptions) *rig {
	t.Helper()
	r := &rig{t: t, env: e, bin: binary(t), dataDir: o.dataDir, port: freePort(t)}
	if r.dataDir == "" {
		r.dataDir = t.TempDir()
	}
	r.proc = startProc(t, r.bin, r.dataDir, r.port)
	r.api = newAPI(t, r.proc.base)

	match := setupCodePattern.FindStringSubmatch(r.proc.logs.String())
	if match == nil {
		t.Fatalf("no setup code in the server logs:\n%s", r.proc.logs.String())
	}
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	r.api.data("POST", "/api/v1/setup", map[string]string{"setup_code": match[1], "username": adminUser, "password": adminPass}, http.StatusCreated, &session)
	r.api.csrf = session.CSRFToken
	var key struct {
		Key string `json:"key"`
	}
	r.api.data("POST", "/api/v1/api-keys", map[string]string{"name": "chaos", "scope": "admin"}, http.StatusCreated, &key)
	r.api = newAPI(t, r.proc.base)
	r.api.key = key.Key

	uri := o.mongoURI
	if uri == "" {
		uri = e.MongoProxyURI
	}
	var conn models.Connection
	r.api.data("POST", "/api/v1/connections", map[string]string{"name": "chaos", "uri": uri}, http.StatusCreated, &conn)
	r.connID = conn.ID

	r.prefix = "chaos/" + randomHex(4) + "/"
	var target models.StorageTarget
	r.api.data("POST", "/api/v1/storage-targets", map[string]any{"name": "minio via toxiproxy", "type": "s3", "s3": map[string]any{
		"endpoint": e.S3Proxy, "region": "us-east-1", "bucket": e.Bucket, "prefix": r.prefix,
		"access_key_id": e.AccessKey, "secret_access_key": e.SecretKey, "use_path_style": true, "part_size_mb": 5,
	}}, http.StatusCreated, &target)
	r.targetID = target.ID
	r.api.data("POST", "/api/v1/storage-targets/"+target.ID+"/default", nil, http.StatusOK, nil)

	r.hook = newHook(t)
	var ch struct {
		ID string `json:"id"`
	}
	r.api.data("POST", "/api/v1/notifications/channels", map[string]any{
		"name": "chaos webhook", "type": "webhook", "enabled": true, "webhook": map[string]any{"url": r.hook.srv.URL},
	}, http.StatusCreated, &ch)
	r.api.data("POST", "/api/v1/notifications/rules", map[string]any{
		"name": "chaos alerts", "enabled": true, "events": alertEvents, "channel_ids": []string{ch.ID},
	}, http.StatusCreated, nil)
	return r
}

// restart starts the binary again on the same data directory and port (after a
// kill or a stop).
func (r *rig) restart() {
	r.t.Helper()
	r.proc = startProc(r.t, r.bin, r.dataDir, r.port)
}

// settings applies a partial settings update.
func (r *rig) settings(update map[string]any) {
	r.t.Helper()
	r.api.data("PUT", "/api/v1/settings", update, http.StatusOK, nil)
}

// startBackup starts an on-demand backup of db (no gzip, so sizes are predictable).
func (r *rig) startBackup(db string) *models.BackupRecord {
	r.t.Helper()
	var rec models.BackupRecord
	r.api.data("POST", "/api/v1/backups", map[string]any{
		"connection_id": r.connID, "database": db, "gzip": false,
	}, http.StatusAccepted, &rec)
	return &rec
}

// backup returns the record of id.
func (r *rig) backup(db, id string) *models.BackupRecord {
	r.t.Helper()
	var list []*models.BackupRecord
	r.api.data("GET", "/api/v1/backups?database="+url.QueryEscape(db), nil, http.StatusOK, &list)
	for _, b := range list {
		if b.ID == id {
			return b
		}
	}
	r.t.Fatalf("backup %s of %s not listed", id, db)
	return nil
}

// waitBytes waits until run id (a backup or a restore) has streamed at least n
// bytes, so a fault lands in the middle of the transfer.
func (r *rig) waitBytes(id string, n int64) {
	r.t.Helper()
	deadline := time.Now().Add(runTimeout)
	seen := false
	for {
		var active []struct {
			ID    string `json:"id"`
			Bytes int64  `json:"bytes"`
		}
		r.api.data("GET", "/api/v1/runs/active", nil, http.StatusOK, &active)
		found := false
		for _, a := range active {
			if a.ID == id {
				found = true
				if a.Bytes >= n {
					return
				}
			}
		}
		if seen && !found {
			r.t.Fatalf("run %s finished before it streamed %d bytes", id, n)
		}
		seen = seen || found
		if time.Now().After(deadline) {
			r.t.Fatalf("run %s did not reach %d bytes", id, n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitBackup waits until backup id leaves in_progress and returns it.
func (r *rig) waitBackup(db, id string) *models.BackupRecord {
	r.t.Helper()
	deadline := time.Now().Add(runTimeout)
	for {
		b := r.backup(db, id)
		if b.Status != models.StatusInProgress && b.Status != models.StatusPending {
			return b
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("backup %s still %s after %s", id, b.Status, runTimeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// restore returns the restore record of id.
func (r *rig) restore(id string) *models.RestoreRecord {
	r.t.Helper()
	var list []*models.RestoreRecord
	r.api.data("GET", "/api/v1/restores", nil, http.StatusOK, &list)
	for _, x := range list {
		if x.ID == id {
			return x
		}
	}
	r.t.Fatalf("restore %s not listed", id)
	return nil
}

// waitRestore waits until restore id leaves in_progress and returns it.
func (r *rig) waitRestore(id string) *models.RestoreRecord {
	r.t.Helper()
	deadline := time.Now().Add(runTimeout)
	for {
		x := r.restore(id)
		if x.Status != models.RestoreStatusInProgress {
			return x
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("restore %s still %s after %s", id, x.Status, runTimeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// objectKey is the full S3 key of a backup's storage key on the rig's target.
func (r *rig) objectKey(storageKey string) string { return r.prefix + storageKey }

// assertBrokenBackup checks a backup that a fault broke: it ended failed (or
// cancelled), carries no checksum and no size, no object exists under its key
// (looked up directly, not through the proxy) and a backup.failed alert fired.
func (r *rig) assertBrokenBackup(b *models.BackupRecord, wantAlert bool) {
	r.t.Helper()
	if b.Status == models.StatusCompleted {
		r.t.Fatalf("a broken backup ended completed: %+v", b)
	}
	if b.Status != models.StatusFailed && b.Status != models.StatusCancelled {
		r.t.Fatalf("broken backup status = %s; want failed or cancelled (%+v)", b.Status, b)
	}
	if b.ErrorMessage == "" {
		r.t.Fatalf("broken backup has no reason: %+v", b)
	}
	if b.SHA256 != "" || b.SizeBytes != 0 {
		r.t.Fatalf("broken backup carries a checksum or a size: %+v", b)
	}
	if b.StorageKey != "" {
		r.assertNoObject(r.objectKey(b.StorageKey))
	}
	if wantAlert {
		r.hook.wait(r.t, "backup.failed", "backup_id", b.ID)
	}
	assertNoSecret(r.t, r.env.MongoPassword, "backup error", b.ErrorMessage)
}

// assertNoObject fails when key exists in the bucket.
func (r *rig) assertNoObject(key string) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	_, err := r.env.S3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(r.env.Bucket), Key: aws.String(key)})
	if err == nil {
		r.t.Fatalf("an object that looks complete exists at %s", key)
	}
}

// incompleteUploads counts the multipart uploads in progress for the S3 key (MinIO
// lists uploads by their full key only).
func (r *rig) incompleteUploads(key string) int {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	out, err := r.env.S3.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(r.env.Bucket), Prefix: aws.String(key)})
	if err != nil {
		r.t.Fatalf("list multipart uploads: %v", err)
	}
	n := 0
	for _, u := range out.Uploads {
		if aws.ToString(u.Key) == key {
			n++
		}
	}
	return n
}

// waitNoIncompleteUploads waits up to d for the multipart uploads of the S3 key to
// be aborted.
func (r *rig) waitNoIncompleteUploads(key string, d time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(d)
	for {
		n := r.incompleteUploads(key)
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%d incomplete multipart upload(s) left behind for %s", n, key)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// assertNextBackupSucceeds runs a backup of db with every fault removed and requires
// it to complete with a checksum and an object, then restores it into a safe clone
// and compares the document count of coll.
func (r *rig) assertNextBackupSucceeds(db, coll string) *models.BackupRecord {
	r.t.Helper()
	r.env.Toxi.reset(r.t)
	b := r.waitBackup(db, r.startBackup(db).ID)
	if b.Status != models.StatusCompleted || b.SHA256 == "" || b.SizeBytes == 0 {
		r.t.Fatalf("the next backup after the fault did not complete: %+v", b)
	}
	r.hook.wait(r.t, "backup.succeeded", "backup_id", b.ID)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if _, err := r.env.S3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(r.env.Bucket), Key: aws.String(r.objectKey(b.StorageKey))}); err != nil {
		r.t.Fatalf("the completed backup has no object: %v", err)
	}
	var accepted models.RestoreRecord
	r.api.data("POST", "/api/v1/restore", map[string]any{"backup_id": b.ID}, http.StatusAccepted, &accepted)
	rst := r.waitRestore(accepted.ID)
	if rst.Status != models.RestoreStatusCompleted {
		r.t.Fatalf("restore of the next backup did not complete: %+v", rst)
	}
	if got, want := r.env.count(r.t, rst.TargetDatabase, coll), r.env.count(r.t, db, coll); got != want {
		r.t.Fatalf("restored clone holds %d documents; the source %d", got, want)
	}
	return b
}

// assertNoSecret fails if secret appears in any value.
func assertNoSecret(t *testing.T, secret, what string, values ...string) {
	t.Helper()
	if secret == "" {
		return
	}
	for _, v := range values {
		if strings.Contains(v, secret) {
			t.Fatalf("%s leaked the MongoDB password", what)
		}
	}
}

// waitFor polls cond every 100 ms until it is true or d passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

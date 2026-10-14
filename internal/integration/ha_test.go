//go:build integration && !windows

package integration

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/ha"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// HA timing of the integration test: short, so a takeover happens within seconds.
const (
	haTestTTL   = 10 * time.Second
	haTestRenew = 3 * time.Second
)

// haMember is a mongorescue process running in active-passive HA mode.
type haMember struct {
	t       *testing.T
	name    string
	bin     string
	dataDir string
	port    int
	env     []string
	logs    *syncBuffer
	cmd     *exec.Cmd
	// exited is closed once the process has exited.
	exited chan struct{}
	// clashes is the number of port clashes in logs when the process started.
	clashes int
	// restarts counts the restarts after port clashes.
	restarts int
}

// start runs the process (again, with the same data directory).
func (m *haMember) start() {
	m.t.Helper()
	m.cmd = exec.Command(m.bin, "-data-dir", m.dataDir, "-host", "127.0.0.1", "-port", strconv.Itoa(m.port))
	m.cmd.Env = m.env
	m.cmd.Stdout, m.cmd.Stderr = m.logs, m.logs
	m.clashes = portClashes(m.logs.String())
	if err := m.cmd.Start(); err != nil {
		m.t.Fatal(err)
	}
	exited := make(chan struct{})
	m.exited = exited
	go func(cmd *exec.Cmd) {
		_ = cmd.Wait()
		close(exited)
	}(m.cmd)
}

// kill ends the process with SIGKILL, like an OOM kill or a lost node.
func (m *haMember) kill() {
	m.t.Helper()
	_ = m.cmd.Process.Kill()
	<-m.exited
}

// stop interrupts the process (graceful shutdown) and waits for it.
func (m *haMember) stop() {
	if m.cmd == nil {
		return
	}
	select {
	case <-m.exited:
		return
	default:
	}
	_ = m.cmd.Process.Signal(os.Interrupt)
	select {
	case <-m.exited:
	case <-time.After(60 * time.Second):
		_ = m.cmd.Process.Kill()
		<-m.exited
	}
}

// restartOnPortClash starts the exited process again on a fresh port when its port
// was taken (see freePort), and reports whether it did.
func (m *haMember) restartOnPortClash() bool {
	m.t.Helper()
	if portClashes(m.logs.String()) <= m.clashes || m.restarts == maxPortClashes {
		return false
	}
	m.restarts++
	m.t.Logf("the port %d of %s was taken; starting it again on another one", m.port, m.name)
	m.port = freePort(m.t)
	m.start()
	return true
}

// base is the member's base URL.
func (m *haMember) base() string { return "http://127.0.0.1:" + strconv.Itoa(m.port) }

// role returns the role the member's health check reports ("" when it does not
// answer).
func (m *haMember) role() string {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(m.base() + "/api/v1/health")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var body struct {
		Data struct {
			Role string `json:"role"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return ""
	}
	return body.Data.Role
}

// waitRole waits until the member reports role and returns how long that took.
func (m *haMember) waitRole(role string, within time.Duration) time.Duration {
	m.t.Helper()
	start := time.Now()
	for time.Since(start) < within {
		if m.role() == role {
			return time.Since(start)
		}
		select {
		case <-m.exited:
			if m.restartOnPortClash() {
				continue
			}
			m.t.Fatalf("%s exited while waiting for role %s\n%s", m.name, role, m.logs.String())
		case <-time.After(250 * time.Millisecond):
		}
	}
	m.t.Fatalf("%s did not become %s within %s (now %q)\n%s", m.name, role, within, m.role(), m.logs.String())
	return 0
}

// haPoller polls the API of a member continuously, so the test sees every refused
// connection of a role change (#192).
type haPoller struct {
	name   string
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	codes   map[int]int
	roles   map[string]int
	refused []error
	other   []error
}

// haAPIPollInterval spaces the unauthenticated job list requests of a poller: 200
// a minute, within the REST rate limit's 300 a minute (bursts of 100) per address,
// so a 429 still fails the test. The health check is never rate limited.
const haAPIPollInterval = 300 * time.Millisecond

// startPoller polls the health check of m every 20 ms and its job list every
// haAPIPollInterval, until check or the end of t.
func startPoller(t *testing.T, m *haMember) *haPoller {
	ctx, cancel := context.WithCancel(context.Background())
	p := &haPoller{name: m.name, cancel: cancel, done: make(chan struct{}), codes: map[int]int{}, roles: map[string]int{}}
	t.Cleanup(func() {
		cancel()
		<-p.done
	})
	base := m.base()
	go func() {
		defer close(p.done)
		client := &http.Client{Timeout: 10 * time.Second}
		defer client.CloseIdleConnections()
		var lastAPI time.Time
		for {
			path := "/api/v1/health"
			if time.Since(lastAPI) >= haAPIPollInterval {
				path, lastAPI = "/api/v1/jobs", time.Now()
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
			if err != nil {
				return
			}
			resp, err := client.Do(req)
			var body struct {
				Data struct {
					Role string `json:"role"`
				} `json:"data"`
			}
			if err == nil {
				if path == "/api/v1/health" {
					_ = json.NewDecoder(resp.Body).Decode(&body)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			if ctx.Err() != nil {
				return
			}
			p.mu.Lock()
			switch {
			case errors.Is(err, syscall.ECONNREFUSED):
				p.refused = append(p.refused, err)
			case err != nil:
				p.other = append(p.other, err)
			default:
				p.codes[resp.StatusCode]++
				if body.Data.Role != "" {
					p.roles[body.Data.Role]++
				}
			}
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	return p
}

// check stops the poller and fails t when a connection was refused or an answer
// had a status other than 200, 401 (the job list without credentials) or 503
// (a standby, or the switch between two roles).
func (p *haPoller) check(t *testing.T) {
	t.Helper()
	p.cancel()
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Logf("requests to %s: status codes %v, health roles %v, refused %d, other errors %d", p.name, p.codes, p.roles, len(p.refused), len(p.other))
	for _, err := range p.other {
		t.Logf("request to %s failed: %v", p.name, err)
	}
	if len(p.refused) > 0 {
		t.Errorf("%d connections to %s were refused through its role changes, the first: %v", len(p.refused), p.name, p.refused[0])
	}
	for code, n := range p.codes {
		if code != http.StatusOK && code != http.StatusUnauthorized && code != http.StatusServiceUnavailable {
			t.Errorf("%d answers of %s with status %d; want 200, 401 or 503", n, p.name, code)
		}
	}
}

// linesWith returns the lines of logs that contain one of words.
func linesWith(logs string, words ...string) string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		for _, w := range words {
			if strings.Contains(line, w) {
				out = append(out, line)
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

// completedBackups returns the completed backups of db the member records.
func completedBackups(api *apiClient, db string) []*models.BackupRecord {
	var list []*models.BackupRecord
	api.data("GET", "/api/v1/backups?database="+db, nil, http.StatusOK, &list)
	var out []*models.BackupRecord
	for _, b := range list {
		if b.Status == models.StatusCompleted {
			out = append(out, b)
		}
	}
	return out
}

// TestHAActivePassiveFailover runs two real processes in active-passive HA mode on
// one MinIO bucket and one MongoDB, with a job every minute. The active process is
// killed with SIGKILL; the standby must become active within ttl + renew + 30s with
// the killed one's metadata, run the next scheduled backup, and never complete two
// backups for the same cron fire. The old process then comes back as standby. At
// last the lease is deleted, so the active process steps down to standby and a
// member takes the lease over again. B, from the kill on, and A, once back, are
// polled throughout: no connection is refused, every request gets 200, 401 or 503.
func TestHAActivePassiveFailover(t *testing.T) {
	t.Parallel()
	m := requireMongo(t)
	var provider *s3Provider
	for _, p := range configuredS3Providers() {
		if p.Name == "minio" {
			provider = &p
		}
	}
	if provider == nil {
		t.Skip("MinIO is not configured (MONGORESCUE_TEST_S3_MINIO_*)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if provider.CreateBucket {
		ensureBucket(ctx, t, provider.Config)
	}
	db := m.uniqueDB(t, "ha")
	m.seed(t, db, "orders", 50)

	run := randomHex(t, 6)
	haPrefix, cluster := "it-ha-"+run, "it-"+run
	haCfg := provider.Config
	haCfg.Prefix = haPrefix
	haStore, err := storage.NewS3Storage(ctx, haCfg)
	if err != nil {
		t.Fatal(err)
	}
	cleanupPrefix(t, haStore, "")
	backupStore := provider.Config
	backupStore.Prefix = "it-ha-backups-" + run
	backupDriver, err := storage.NewS3Storage(ctx, backupStore)
	if err != nil {
		t.Fatal(err)
	}
	cleanupPrefix(t, backupDriver, "")

	keyBytes := make([]byte, 32)
	if _, err = rand.Read(keyBytes); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	env := cleanEnv(
		"MONGORESCUE_SECRET_KEY="+base64.StdEncoding.EncodeToString(keyBytes),
		"MONGORESCUE_HA_MODE=active-passive",
		fmt.Sprintf("MONGORESCUE_HA_TARGET=s3://%s/%s?endpoint=%s&region=%s&path_style=%t",
			haCfg.Bucket, haPrefix, haCfg.Endpoint, haCfg.Region, haCfg.UsePathStyle),
		"MONGORESCUE_HA_CLUSTER="+cluster,
		"MONGORESCUE_HA_LEASE_TTL="+haTestTTL.String(),
		"MONGORESCUE_HA_RENEW_INTERVAL="+haTestRenew.String(),
		"MONGORESCUE_HA_SNAPSHOT_INTERVAL=30s",
		"AWS_ACCESS_KEY_ID="+haCfg.AccessKey, "AWS_SECRET_ACCESS_KEY="+haCfg.SecretKey, "AWS_REGION="+haCfg.Region,
		"AWS_CONFIG_FILE="+missing, "AWS_SHARED_CREDENTIALS_FILE="+missing, "AWS_PROFILE=",
	)
	bin := buildBinary(t)
	newMember := func(name string) *haMember {
		return &haMember{t: t, name: name, bin: bin, dataDir: filepath.Join(t.TempDir(), "data"), port: freePort(t), env: env, logs: &syncBuffer{}}
	}
	a, b := newMember("A"), newMember("B")
	t.Cleanup(func() {
		b.stop()
		a.stop()
		if t.Failed() {
			t.Logf("A logs:\n%s\nB logs:\n%s", a.logs.String(), b.logs.String())
		}
	})

	// A starts first and takes the lease; B joins as standby.
	a.start()
	a.waitRole("active", 60*time.Second)
	b.start()
	b.waitRole("standby", 60*time.Second)

	// Set up A: the first user, a connection, an S3 target and a job every minute.
	setupCode := waitSetupCode(t, a.logs, 30*time.Second)
	const password = "integration-password-1"
	apiA := newAPIClient(t, a.base())
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	apiA.data("POST", "/api/v1/setup", map[string]string{"setup_code": setupCode, "username": "admin", "password": password}, http.StatusCreated, &session)
	apiA.csrf = session.CSRFToken
	var conn models.Connection
	apiA.data("POST", "/api/v1/connections", map[string]string{"name": "ha mongo", "uri": m.URI}, http.StatusCreated, &conn)
	var target models.StorageTarget
	apiA.data("POST", "/api/v1/storage-targets", map[string]any{"name": "ha minio", "type": "s3", "s3": map[string]any{
		"endpoint": backupStore.Endpoint, "region": backupStore.Region, "bucket": backupStore.Bucket, "prefix": backupStore.Prefix,
		"access_key_id": backupStore.AccessKey, "secret_access_key": backupStore.SecretKey, "use_path_style": backupStore.UsePathStyle,
	}}, http.StatusCreated, &target)
	var job models.Job
	apiA.data("POST", "/api/v1/jobs", models.Job{Name: "ha every minute", Database: db, CronExpression: "* * * * *", Enabled: true,
		Gzip: true, ConnectionID: conn.ID, StorageTargetID: target.ID}, http.StatusCreated, &job)
	// The standby refuses writes.
	apiB := newAPIClient(t, b.base())
	if code, _ := apiB.do("POST", "/api/v1/jobs", map[string]string{}); code != http.StatusServiceUnavailable {
		t.Fatalf("a write on the standby answered %d; want 503", code)
	}

	// Wait for a scheduled backup on A and for the snapshot written after it.
	var first *models.BackupRecord
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if done := completedBackups(apiA, db); len(done) > 0 {
			first = done[0]
			break
		}
		time.Sleep(time.Second)
	}
	if first == nil {
		t.Fatalf("A completed no scheduled backup\n%s", a.logs.String())
	}
	for time.Now().Before(deadline) {
		snaps, listErr := ha.ListSnapshots(ctx, haStore, cluster)
		if listErr == nil && len(snaps) > 0 && first.CompletedAt != nil && snaps[0].CreatedAt.After(*first.CompletedAt) {
			break
		}
		time.Sleep(time.Second)
	}

	// Kill A a few seconds before a cron fire, so the fire falls in the gap
	// without an active instance and B must catch it up. B must take over within
	// ttl + renew + 30s.
	for sec := time.Now().Second(); sec < 50 || sec >= 55; sec = time.Now().Second() {
		time.Sleep(200 * time.Millisecond)
	}
	// B is polled from now on, through its takeover and its step-down: its
	// listener stays open, so no connection is refused.
	pollB := startPoller(t, b)
	a.kill()
	killedAt := time.Now()
	missedFire := killedAt.Truncate(time.Minute).Add(time.Minute)
	took := b.waitRole("active", haTestTTL+haTestRenew+30*time.Second)
	t.Logf("B became active %s after A was killed", took)
	t.Cleanup(func() { t.Logf("B's takeover:\n%s", linesWith(b.logs.String(), "takeover", "missed", "HA lease")) })

	// B has A's metadata: the user, the job and A's backup.
	apiB = newAPIClient(t, b.base())
	apiB.data("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": password}, http.StatusOK, &session)
	apiB.csrf = session.CSRFToken
	var jobs []models.Job
	apiB.data("GET", "/api/v1/jobs", nil, http.StatusOK, &jobs)
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("B's jobs after the takeover = %+v; want A's job", jobs)
	}

	// B catches the missed fire up once, then runs the next scheduled backup.
	var caughtUp, next *models.BackupRecord
	deadline = time.Now().Add(3 * time.Minute)
	for (caughtUp == nil || next == nil) && time.Now().Before(deadline) {
		for _, bk := range completedBackups(apiB, db) {
			switch {
			case bk.Trigger != models.TriggerScheduled || !bk.StartedAt.After(killedAt):
			case bk.StartedAt.Truncate(time.Minute).Equal(missedFire.Truncate(time.Minute)):
				caughtUp = bk
			case bk.StartedAt.After(missedFire.Add(time.Minute - time.Second)):
				next = bk
			}
		}
		time.Sleep(time.Second)
	}
	if caughtUp == nil || next == nil {
		t.Fatalf("after the takeover B caught the missed fire up: %v, ran the next fire: %v\n%s", caughtUp != nil, next != nil, b.logs.String())
	}
	if caughtUp.HAEpoch < 2 || next.HAEpoch < 2 {
		t.Fatalf("B's backups carry epochs %d and %d; want the epoch of B's lease (>= 2)", caughtUp.HAEpoch, next.HAEpoch)
	}

	// No cron fire has two completed backups.
	perFire := map[string][]string{}
	for _, bk := range completedBackups(apiB, db) {
		fire := bk.StartedAt.UTC().Truncate(time.Minute).Format(time.RFC3339)
		perFire[fire] = append(perFire[fire], bk.ID)
	}
	for fire, ids := range perFire {
		if len(ids) > 1 {
			t.Fatalf("the fire at %s has %d completed backups: %v", fire, len(ids), ids)
		}
	}

	// A comes back as standby and stays there.
	a.logs = &syncBuffer{}
	a.start()
	a.waitRole("standby", 60*time.Second)
	time.Sleep(2 * haTestRenew)
	if a.role() != "standby" || b.role() != "active" {
		t.Fatalf("after A's restart: A %q, B %q; want standby and active", a.role(), b.role())
	}

	// B steps down: the lease disappears, so its next renewal fails its condition.
	// B reopens as standby, and a member takes the new lease over. Neither member
	// refuses a connection meanwhile.
	pollA := startPoller(t, a)
	time.Sleep(time.Second)
	if err = haStore.Delete(ctx, ha.LeaseKey(cluster)); err != nil {
		t.Fatal(err)
	}
	// B may take the new lease over again at once, so its log tells the step-down.
	deadline = time.Now().Add(2*haTestRenew + 30*time.Second)
	for !strings.Contains(b.logs.String(), "this instance lost the HA lease") {
		if time.Now().After(deadline) {
			t.Fatalf("B did not step down once the lease was deleted\n%s", b.logs.String())
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Cleanup(func() {
		t.Logf("B's step-down:\n%s", linesWith(b.logs.String(), "lost the HA lease", "stepping down", "stepped down"))
	})
	deadline = time.Now().Add(haTestTTL + haTestRenew + 60*time.Second)
	for a.role() != "active" && b.role() != "active" {
		if time.Now().After(deadline) {
			t.Fatalf("no member became active after B's step-down: A %q, B %q", a.role(), b.role())
		}
		time.Sleep(250 * time.Millisecond)
	}
	time.Sleep(time.Second)
	pollA.check(t)
	pollB.check(t)
}

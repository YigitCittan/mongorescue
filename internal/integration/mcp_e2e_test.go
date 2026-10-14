//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// sharedBinary is the mongorescue binary every test that runs the real process
// shares: it is built once per test run, into a directory TestMain removes.
var sharedBinary struct {
	once sync.Once
	dir  string
	path string
	err  error
}

// buildBinary compiles cmd/mongorescue on first use and returns its path. Tests
// running in parallel wait for the one build.
func buildBinary(t *testing.T) string {
	t.Helper()
	b := &sharedBinary
	b.once.Do(func() {
		if b.dir, b.err = os.MkdirTemp("", "mongorescue-it-bin-"); b.err != nil {
			return
		}
		bin := filepath.Join(b.dir, "mongorescue")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", bin, "github.com/yigitcittan/mongorescue/cmd/mongorescue")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
			b.err = fmt.Errorf("build mongorescue: %w\n%s", buildErr, out)
			return
		}
		b.path = bin
	})
	if b.err != nil {
		t.Fatal(b.err)
	}
	return b.path
}

// The processes under test listen on ports from testPorts, below the ephemeral
// ranges (Linux 32768-60999, macOS and Windows 49152-65535): neither a listener on
// port 0 (httptest servers, docker) nor an outgoing connection can take one between
// freePort and the child's bind, and freePort never hands a port out twice in a run.
// Only a process outside the suite can still hold one; startServer and haMember
// then retry on a fresh port (portClashes).
const (
	testPortsFirst = 20000
	testPortsCount = 10000
	// maxPortClashes bounds the retries after a bind failure.
	maxPortClashes = 5
)

// testPorts is the allocator of freePort.
var testPorts struct {
	mu   sync.Mutex
	next int // offset of the next port to try; random at first, so concurrent runs start apart
	used int
}

// freePort returns a loopback TCP port that is free now and that no other test of
// this run gets.
func freePort(t *testing.T) int {
	t.Helper()
	testPorts.mu.Lock()
	defer testPorts.mu.Unlock()
	if testPorts.used == 0 {
		testPorts.next = rand.IntN(testPortsCount) //nolint:gosec // a start offset, not a secret
	}
	for testPorts.used < testPortsCount {
		port := testPortsFirst + testPorts.next
		testPorts.next = (testPorts.next + 1) % testPortsCount
		testPorts.used++
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue // held by a process outside the suite
		}
		if err = l.Close(); err != nil {
			t.Fatal(err)
		}
		return port
	}
	t.Fatalf("no free port left in %d-%d", testPortsFirst, testPortsFirst+testPortsCount-1)
	return 0
}

// portClashes counts the times processes under test logged that their port was taken.
func portClashes(logs string) int {
	return strings.Count(logs, "address already in use") + // Linux, macOS
		strings.Count(logs, "Only one usage of each socket address") // Windows
}

// cleanEnv is the environment without MONGORESCUE_* variables, so the processes under
// test only see what the test sets.
func cleanEnv(extra ...string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "MONGORESCUE_") })
	return append(env, extra...)
}

// startServer runs the real binary as a server on a free port and returns its base URL
// and its captured logs. The process is interrupted (graceful shutdown) on cleanup.
// When the port turns out to be taken, it starts again on another one.
func startServer(t *testing.T, bin string) (string, *syncBuffer) {
	t.Helper()
	for clashes := 0; ; clashes++ {
		base, logs, ok := tryStartServer(t, bin)
		if ok {
			return base, logs
		}
		if clashes == maxPortClashes {
			t.Fatalf("every port tried was taken\n%s", logs.String())
		}
		t.Logf("the port of %s was taken; starting again on another one", base)
	}
}

// tryStartServer is one attempt of startServer: ok is false when the server exited
// because its port was taken.
func tryStartServer(t *testing.T, bin string) (base string, logs *syncBuffer, ok bool) {
	t.Helper()
	port := freePort(t)
	logs = &syncBuffer{}
	cmd := exec.Command(bin, "-data-dir", t.TempDir(), "-host", "127.0.0.1", "-port", strconv.Itoa(port))
	cmd.Env = cleanEnv()
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		if runtime.GOOS == "windows" {
			_ = cmd.Process.Kill()
		} else {
			_ = cmd.Process.Signal(os.Interrupt)
		}
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	})

	base = fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(base + "/api/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base, logs, true
			}
		}
		select {
		case <-exited:
			if portClashes(logs.String()) > 0 {
				return base, logs, false
			}
			t.Fatalf("server exited: %v\n%s", waitErr, logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not become healthy\n%s", logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// setupCodePattern finds the one-time setup code in the server logs.
var setupCodePattern = regexp.MustCompile(`setup_code=([A-Z2-7]{4}(?:-[A-Z2-7]{4})+)`)

// waitSetupCode waits at most within for the setup code in logs and returns the
// last one logged. The server may answer its health check before it logs the
// code, so the line is waited for rather than expected at once.
func waitSetupCode(t *testing.T, logs *syncBuffer, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if matches := setupCodePattern.FindAllStringSubmatch(logs.String(), -1); len(matches) > 0 {
			return matches[len(matches)-1][1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no setup code in the server logs within %s:\n%s", within, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// bridgeSession spawns "mongorescue mcp" with key and connects an MCP client to it.
func bridgeSession(t *testing.T, bin, base, key string) (*sdk.ClientSession, *syncBuffer) {
	t.Helper()
	stderr := &syncBuffer{}
	cmd := exec.Command(bin, "mcp", "--url", base, "--log-level", "debug")
	cmd.Env = cleanEnv("MONGORESCUE_MCP_API_KEY=" + key)
	cmd.Stderr = stderr
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "integration-assistant", Version: "v0"}, nil).
		Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to the stdio bridge: %v\nbridge stderr:\n%s", err, stderr.String())
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, stderr
}

// mcpCall calls tool and decodes its structured content into out.
func mcpCall(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any, out any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	raw, _ := json.Marshal(res)
	if res.IsError {
		t.Fatalf("%s: tool error %s", tool, raw)
	}
	if out != nil {
		sc, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(sc, out); err != nil {
			t.Fatalf("%s: decode %s: %v", tool, sc, err)
		}
	}
	return string(raw)
}

// TestMCPStdioBridgeEndToEnd drives the real binary: a server with a real MongoDB,
// and "mongorescue mcp" spawned as an assistant would, driven by the MCP Go SDK
// client: start_backup -> completed -> restore_to_safe_clone -> completed, and a
// read-only key that cannot start a backup.
func TestMCPStdioBridgeEndToEnd(t *testing.T) {
	t.Parallel()
	m := requireMongo(t)
	db := m.uniqueDB(t, "mcp")
	m.seed(t, db, "orders", 120)

	bin := buildBinary(t)
	base, serverLogs := startServer(t, bin)

	setupCode := waitSetupCode(t, serverLogs, 30*time.Second)
	api := newAPIClient(t, base)
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	api.data("POST", "/api/v1/setup", map[string]string{"setup_code": setupCode, "username": "admin", "password": "integration-password-1"}, http.StatusCreated, &session)
	api.csrf = session.CSRFToken
	var conn models.Connection
	api.data("POST", "/api/v1/connections", map[string]string{"name": "integration mongo", "uri": m.URI}, http.StatusCreated, &conn)
	var opKey, readKey struct {
		Key string `json:"key"`
	}
	api.data("POST", "/api/v1/api-keys", map[string]string{"name": "mcp operator", "scope": "operator"}, http.StatusCreated, &opKey)
	api.data("POST", "/api/v1/api-keys", map[string]string{"name": "mcp reader"}, http.StatusCreated, &readKey)

	var outputs []string
	cs, bridgeLogs := bridgeSession(t, bin, base, opKey.Key)
	var names []string
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, "start_backup") || !slices.Contains(names, "restore_to_safe_clone") {
		t.Fatalf("operator bridge tools = %v", names)
	}

	var started struct {
		Backup models.BackupRecord `json:"backup"`
	}
	outputs = append(outputs, mcpCall(t, cs, "start_backup", map[string]any{"connection_id": conn.ID, "database": db}, &started))
	if started.Backup.Status != models.StatusInProgress {
		t.Fatalf("start_backup = %+v", started.Backup)
	}
	var backup models.BackupRecord
	deadline := time.Now().Add(opTimeout)
	for backup.Status != models.StatusCompleted {
		outputs = append(outputs, mcpCall(t, cs, "get_backup", map[string]any{"id": started.Backup.ID}, &backup))
		if backup.Status == models.StatusFailed || time.Now().After(deadline) {
			t.Fatalf("backup did not complete: %+v", backup)
		}
		time.Sleep(250 * time.Millisecond)
	}

	var rst struct {
		Restore models.RestoreRecord `json:"restore"`
	}
	outputs = append(outputs, mcpCall(t, cs, "restore_to_safe_clone", map[string]any{"backup_id": backup.ID, "verify": true}, &rst))
	if !strings.HasPrefix(rst.Restore.TargetDatabase, db+"_rescue_") {
		t.Fatalf("restore target = %q; want a %s_rescue_ clone", rst.Restore.TargetDatabase, db)
	}
	var restored models.RestoreRecord
	deadline = time.Now().Add(opTimeout)
	for restored.Status != models.RestoreStatusCompleted {
		outputs = append(outputs, mcpCall(t, cs, "get_restore", map[string]any{"id": rst.Restore.ID}, &restored))
		if restored.Status == models.RestoreStatusFailed || time.Now().After(deadline) {
			t.Fatalf("restore did not complete: %+v", restored)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !restored.Verified {
		t.Fatal("the restore was asked to verify the archive first")
	}
	if n := m.count(t, restored.TargetDatabase, "orders"); n != 120 {
		t.Fatalf("clone holds %d orders; want 120", n)
	}
	if n := m.count(t, db, "orders"); n != 120 {
		t.Fatalf("the source database changed: %d orders", n)
	}
	outputs = append(outputs, mcpCall(t, cs, "get_status", nil, nil))

	// A read-only key sees no action tools and cannot start a backup.
	readCS, readLogs := bridgeSession(t, bin, base, readKey.Key)
	names = names[:0]
	for tool, err := range readCS.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	if slices.Contains(names, "start_backup") || !slices.Contains(names, "list_backups") {
		t.Fatalf("read bridge tools = %v", names)
	}
	res, err := readCS.CallTool(context.Background(), &sdk.CallToolParams{Name: "start_backup", Arguments: map[string]any{"connection_id": conn.ID, "database": db}})
	if err == nil && !res.IsError {
		t.Fatalf("a read-only key started a backup: %+v", res)
	}
	var backups []models.BackupRecord
	api.data("GET", "/api/v1/backups?database="+db, nil, http.StatusOK, &backups)
	if len(backups) != 1 {
		t.Fatalf("backups of %s = %d; the read key must not have started one", db, len(backups))
	}

	// Every tool call is in the audit log, attributed to its key and the stdio transport.
	var entries []audit.Entry
	api.data("GET", "/api/v1/audit", nil, http.StatusOK, &entries)
	if !slices.ContainsFunc(entries, func(e audit.Entry) bool {
		return e.Tool == "start_backup" && e.APIKeyName == "mcp operator" && e.Transport == audit.TransportStdio && e.Result == audit.ResultOK
	}) {
		t.Fatalf("audit log lacks the stdio start_backup call: %+v", entries)
	}
	// ... and mirrored into the hash-chained audit log of every action.
	// Entries are written asynchronously: poll briefly.
	var chain auditlog.Page
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		api.data("GET", "/api/v1/audit/events?action=MCP+start_backup", nil, http.StatusOK, &chain)
		if slices.ContainsFunc(chain.Events, func(e *auditlog.Event) bool {
			return e.ActorKind == auditlog.ActorAPIKey && e.ActorKeyName == "mcp operator" && e.Outcome == auditlog.OutcomeOK
		}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit log of every action lacks the start_backup call: %+v", chain.Events)
		}
	}
	var verified auditlog.Verification
	api.data("GET", "/api/v1/audit/events/verify", nil, http.StatusOK, &verified)
	if !verified.OK || verified.Checked == 0 {
		t.Fatalf("audit chain verification = %+v", verified)
	}

	auditRaw, _ := json.Marshal(entries)
	assertNoSecret(t, m.Password, "MCP tool outputs", outputs...)
	assertNoSecret(t, m.Password, "MCP audit log", string(auditRaw))
	assertNoSecret(t, m.Password, "logs", serverLogs.String(), bridgeLogs.String(), readLogs.String())
	for _, k := range []string{opKey.Key, readKey.Key} {
		if strings.Contains(serverLogs.String()+bridgeLogs.String()+readLogs.String(), k) {
			t.Fatal("an API key appeared in the logs")
		}
	}
}

package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/ha"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// haTestKey is the secret key shared by the members of a test cluster.
func haTestKey(t *testing.T) string {
	t.Helper()
	key, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return secretbox.EncodeKey(key)
}

// freeLoopbackPort returns a free port on 127.0.0.1.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// haTestConfig returns the configuration of one member of a cluster on the shared
// directory.
func haTestConfig(t *testing.T, shared, key string) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Host, cfg.Port, cfg.SecretKey = "127.0.0.1", freeLoopbackPort(t), key
	cfg.HA = config.HAConfig{Mode: config.HAModeActivePassive, Target: "file://" + filepath.ToSlash(shared), Cluster: "test",
		LeaseTTL: ha.MinTTL, RenewInterval: ha.MinRenew, SnapshotInterval: config.MinHASnapshotInterval}
	return cfg
}

// newTestSupervisor returns the supervisor RunHA would run for cfg.
func newTestSupervisor(t *testing.T, cfg *config.Config) *haSupervisor {
	t.Helper()
	target, err := config.ParseHATarget(cfg.HA.Target)
	if err != nil {
		t.Fatal(err)
	}
	state, err := ha.LoadState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := &haRuntime{cfg: cfg.HA, target: target, state: state, name: "test-" + state.InstanceID, logger: logger}
	return &haSupervisor{cfg: cfg, logger: logger, opts: []Option{WithGetenv(noEnv)}, rt: rt}
}

func TestRunHANeedsTheSharedKey(t *testing.T) {
	cfg := haTestConfig(t, t.TempDir(), "")
	if err := RunHA(cfg, nil); !errors.Is(err, ErrHANeedsSharedKey) {
		t.Fatalf("RunHA without a key = %v; want ErrHANeedsSharedKey", err)
	}
	cfg.HA.Mode = config.HAModeOff
	if err := RunHA(cfg, nil); err == nil {
		t.Fatal("RunHA with HA off succeeded")
	}
}

func TestStandbyAppIsReadOnlyAndRunsNothing(t *testing.T) {
	cfg := haTestConfig(t, t.TempDir(), haTestKey(t))
	sup := newTestSupervisor(t, cfg)
	app, err := sup.open(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = sup.bootstrap(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if err = app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		app.Stop()
		_ = app.Close()
	}()
	if !app.scheduler.LastTick().IsZero() {
		t.Fatal("the scheduler runs on a standby")
	}
	if _, err = app.runs.Acquire(runs.BackupKey("conn", "db")); !errors.Is(err, ha.ErrNotActive) {
		t.Fatalf("a standby admitted a backup: %v", err)
	}

	h := app.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "standby") {
		t.Fatalf("POST on a standby = %d %q (Retry-After %q); want 503 naming the standby", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("MCP on a standby = %d; want 503", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"x","password":"y"}`)))
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatal("sign-in is refused on a standby")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	var body struct {
		Data struct {
			Role string          `json:"role"`
			HA   json.RawMessage `json:"ha"`
		} `json:"data"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK || body.Data.Role != "standby" || len(body.Data.HA) == 0 {
		t.Fatalf("health on a standby = %d %s", rec.Code, rec.Body.String())
	}
	if st := sup.rt.metricsState(); st.Role != "standby" {
		t.Fatalf("metrics role = %q", st.Role)
	}
}

// roleOf reads the role from the health endpoint of a member ("" when it does not
// answer).
func roleOf(port int) string {
	client := http.Client{Timeout: time.Second}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/health")
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

// waitRole waits until the member on port reports role.
func waitRole(t *testing.T, port int, role string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if roleOf(port) == role {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the member on port %d did not become %s within %s (now %q)", port, role, within, roleOf(port))
}

// haMarker is a state document the test writes on the active member and reads on
// the member that takes over.
const haMarker = "ha_test.marker"

// TestSupervisorHandsOverBetweenTwoMembers runs two members of a cluster in the
// process: the first becomes active, the second standby; a graceful stop of the
// active member writes a last snapshot and releases the lease, and the standby takes
// over at its next read with the first member's metadata (its own database kept as
// .bak); a restarted member comes back as standby.
func TestSupervisorHandsOverBetweenTwoMembers(t *testing.T) {
	if testing.Short() {
		t.Skip("runs two HA members for several seconds")
	}
	shared, key := t.TempDir(), haTestKey(t)
	cfgA, cfgB := haTestConfig(t, shared, key), haTestConfig(t, shared, key)
	handedOver := make(chan string, 4)

	start := func(cfg *config.Config, onActive func(*App)) (stop func() error) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		sup := newTestSupervisor(t, cfg)
		sup.onActive = onActive
		go func() { done <- sup.run(ctx) }()
		return func() error {
			cancel()
			select {
			case err := <-done:
				return err
			case <-time.After(30 * time.Second):
				t.Fatal("the member did not stop")
				return nil
			}
		}
	}

	stopA := start(cfgA, func(app *App) {
		if err := app.metaStore.(*store.SQLiteStore).SaveIntegrityState(context.Background(), haMarker, map[string]string{"from": "A"}); err != nil {
			t.Error(err)
		}
	})
	waitRole(t, cfgA.Port, "active", 10*time.Second)
	stopB := start(cfgB, func(app *App) {
		var v map[string]string
		ok, err := app.metaStore.(*store.SQLiteStore).LoadIntegrityState(context.Background(), haMarker, &v)
		if err != nil || !ok {
			handedOver <- ""
			return
		}
		handedOver <- v["from"]
	})
	waitRole(t, cfgB.Port, "standby", 10*time.Second)
	if roleOf(cfgA.Port) != "active" {
		t.Fatal("the first member lost the active role when the second joined")
	}

	if err := stopA(); err != nil {
		t.Fatalf("graceful stop of the active member: %v", err)
	}
	// The lease was released: the standby takes over at its next read.
	waitRole(t, cfgB.Port, "active", 3*cfgB.HA.RenewInterval+5*time.Second)
	select {
	case from := <-handedOver:
		if from != "A" {
			t.Fatalf("the new active member does not have the metadata of the previous one (marker %q)", from)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the new active member did not start")
	}
	if _, err := os.Stat(cfgB.MetadataDBPath() + store.BackupSuffix); err != nil {
		t.Fatalf("the standby's own database was not kept as .bak: %v", err)
	}

	stopA = start(cfgA, nil)
	waitRole(t, cfgA.Port, "standby", 10*time.Second)
	time.Sleep(2 * cfgA.HA.RenewInterval)
	if roleOf(cfgA.Port) != "standby" || roleOf(cfgB.Port) != "active" {
		t.Fatalf("after the restart: A %q, B %q; want standby and active", roleOf(cfgA.Port), roleOf(cfgB.Port))
	}
	if err := stopB(); err != nil {
		t.Fatal(err)
	}
	if err := stopA(); err != nil {
		t.Fatal(err)
	}
}

func TestStartupRefusesAnotherSecretKey(t *testing.T) {
	shared := t.TempDir()
	cfgA := haTestConfig(t, shared, haTestKey(t))
	supA := newTestSupervisor(t, cfgA)
	app, err := supA.open(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = supA.bootstrap(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	// A writes the lease.
	supA.rt.mgr().Step(context.Background())
	if supA.rt.mgr().Role() != ha.RoleActive {
		t.Fatal("the first member did not acquire the lease")
	}

	cfgB := haTestConfig(t, shared, haTestKey(t))
	supB := newTestSupervisor(t, cfgB)
	appB, err := supB.open(false)
	if err != nil {
		t.Fatal(err)
	}
	defer appB.Close()
	if err = supB.bootstrap(context.Background(), appB); !errors.Is(err, ha.ErrKeyMismatch) {
		t.Fatalf("bootstrap with another secret key = %v; want ErrKeyMismatch", err)
	}
}

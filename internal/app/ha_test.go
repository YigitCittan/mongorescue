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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/ha"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/server"
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
	cfg.Host, cfg.Port, cfg.SecretKey, cfg.Dashboard = "127.0.0.1", freeLoopbackPort(t), key, true
	cfg.HA = config.HAConfig{Mode: config.HAModeActivePassive, Target: config.FileURL(shared), Cluster: "test",
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
	t.Parallel() // independent: own data directories, HA targets and ports
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
	// No data and no sign-in: the standby trusts neither its data nor its users.
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"x","password":"y"}`)),
		httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/backups", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/setup/status", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/readiness", nil),
		httptest.NewRequest(http.MethodGet, "/auth/oidc/start", nil),
	} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s %s on a standby = %d; want 503 with Retry-After", req.Method, req.URL.Path, rec.Code)
		}
	}
	// The dashboard is replaced by the static standby page; its files stay public.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="ha-standby-page"`) || strings.Contains(rec.Body.String(), "app.js") {
		t.Fatalf("GET / on a standby = %d, want the standby page:\n%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ha.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ha.js on a standby = %d", rec.Code)
	}
	// Metrics without credentials, but only process, build and HA series.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `mongorescue_ha_role{cluster="test",role="standby"} 1`) ||
		strings.Contains(rec.Body.String(), "mongorescue_backups_total") || strings.Contains(rec.Body.String(), "mongorescue_settings_warnings") {
		t.Fatalf("GET /metrics on a standby = %d:\n%s", rec.Code, rec.Body.String())
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

// fakeClock is a lease clock that moves only when the test advances it, so a lease
// expires only when the test means it to, however slowly the test runs.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// newFakeClock returns a fake clock standing at the current time.
func newFakeClock() *fakeClock { return &fakeClock{now: time.Now()} }

// Now returns the time of the clock.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// haWaitBound bounds a wait for something that must happen. It is no timing
// assumption: it only turns a hang into a failure shortly before the test binary's
// own timeout would.
func haWaitBound(t *testing.T) time.Duration {
	t.Helper()
	if deadline, ok := t.Deadline(); ok {
		return max(time.Until(deadline)-10*time.Second, time.Second)
	}
	return 10 * time.Minute
}

// await receives the next value of ch.
func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	timer := time.NewTimer(haWaitBound(t))
	defer timer.Stop()
	select {
	case v := <-ch:
		return v
	case <-timer.C:
		t.Fatalf("%s did not happen before the test deadline", what)
		var zero T
		return zero
	}
}

// healthRole reads the role from the health endpoint of app ("" when it does not
// answer with one).
func healthRole(app *App) string {
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	var body struct {
		Data struct {
			Role string `json:"role"`
		} `json:"data"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		return ""
	}
	return body.Data.Role
}

// haServed is an App a member started serving: its role, the role its health
// endpoint reported then, and what the test's hook of an active App returned.
type haServed struct {
	active bool
	health string
	marker string
}

// haMember is a member of a test cluster run by its supervisor in the process. Its
// lease manager runs no loop of its own: the test steps it.
type haMember struct {
	name   string
	sup    *haSupervisor
	served chan haServed
	done   chan error
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

// startHAMember starts a member on cfg with the lease clock clock; onActive, when
// set, runs on every App that started as active, and its result is the marker of
// the haServed.
func startHAMember(t *testing.T, name string, cfg *config.Config, clock *fakeClock, onActive func(*App) string) *haMember {
	t.Helper()
	sup := newTestSupervisor(t, cfg)
	m := &haMember{name: name, sup: sup, served: make(chan haServed, 8), done: make(chan error, 1)}
	sup.clock = clock.Now
	sup.leaseLoop = func(ctx context.Context, _ *ha.Manager) { <-ctx.Done() }
	sup.onStandby = func(app *App) { m.served <- haServed{health: healthRole(app)} }
	sup.onActive = func(app *App) {
		s := haServed{active: true, health: healthRole(app)}
		if onActive != nil {
			s.marker = onActive(app)
		}
		m.served <- s
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go func() { m.done <- sup.run(ctx) }()
	t.Cleanup(func() { _ = m.stop(t) })
	return m
}

// stop stops the member gracefully, once, and returns the error of its run.
func (m *haMember) stop(t *testing.T) error {
	t.Helper()
	m.once.Do(func() {
		m.cancel()
		m.err = await(t, m.done, "the stop of member "+m.name)
	})
	return m.err
}

// expectServed waits for the next App the member serves and checks its role.
func (m *haMember) expectServed(t *testing.T, active bool) haServed {
	t.Helper()
	role := server.HARoleStandby
	if active {
		role = server.HARoleActive
	}
	s := await(t, m.served, "member "+m.name+" serving as "+role)
	if s.active != active || s.health != role {
		t.Fatalf("member %s served active=%v with health role %q; want %s", m.name, s.active, s.health, role)
	}
	return s
}

// role returns the role the member reports (its health endpoint's).
func (m *haMember) role() string { return m.sup.rt.serverStatus().Role }

// step runs one lease step of the member, then checks that at most one member of
// the cluster is active.
func (m *haMember) step(t *testing.T, cluster ...*haMember) {
	t.Helper()
	m.sup.rt.mgr().Step(context.Background())
	var active []string
	for _, c := range cluster {
		if c.role() == server.HARoleActive {
			active = append(active, c.name)
		}
	}
	if len(active) > 1 {
		t.Fatalf("more than one active member: %v", active)
	}
}

// stepUntilActive steps the member until it holds the lease. Only a failed write of
// the lease takes more than one step.
func (m *haMember) stepUntilActive(t *testing.T, cluster ...*haMember) {
	t.Helper()
	for range 20 {
		m.step(t, cluster...)
		if m.role() == server.HARoleActive {
			return
		}
	}
	t.Fatalf("member %s did not acquire the lease: %+v", m.name, m.sup.rt.status())
}

// expectRoles checks the role every member reports.
func expectRoles(t *testing.T, want map[*haMember]string) {
	t.Helper()
	for m, role := range want {
		if got := m.role(); got != role {
			t.Fatalf("member %s is %s; want %s", m.name, got, role)
		}
	}
}

// haMarker is a state document the test writes on the active member and reads on
// the member that takes over.
const haMarker = "ha_test.marker"

// TestSupervisorHandsOverBetweenTwoMembers runs two members of a cluster in the
// process, on a lease clock and lease steps the test drives: the first becomes
// active, the second standby while the first renews; a graceful stop of the active
// member writes a last snapshot and releases the lease, and the standby takes over
// at its next read with the first member's metadata (its own database kept as
// .bak); a restarted member comes back as standby. At most one member is active
// after every step, and no member serves a role twice.
func TestSupervisorHandsOverBetweenTwoMembers(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	if testing.Short() {
		t.Skip("opens five Apps of two HA members")
	}
	shared, key := t.TempDir(), haTestKey(t)
	cfgA, cfgB := haTestConfig(t, shared, key), haTestConfig(t, shared, key)
	renew := cfgA.HA.RenewInterval
	clock := newFakeClock()

	// Both members start at once (opening an App is slow under -race); neither
	// holds the lease before its manager steps.
	a := startHAMember(t, "A", cfgA, clock, func(app *App) string {
		if err := app.metaStore.(*store.SQLiteStore).SaveIntegrityState(context.Background(), haMarker, map[string]string{"from": "A"}); err != nil {
			t.Error(err)
		}
		return ""
	})
	b := startHAMember(t, "B", cfgB, clock, func(app *App) string {
		var v map[string]string
		if ok, err := app.metaStore.(*store.SQLiteStore).LoadIntegrityState(context.Background(), haMarker, &v); err != nil || !ok {
			return ""
		}
		return v["from"]
	})
	a.expectServed(t, false)
	b.expectServed(t, false)

	// A creates the lease and reopens as active; B reads it and stays standby
	// while A renews it. The lease clock never gets near the TTL.
	a.stepUntilActive(t, a, b)
	a.expectServed(t, true)
	for range 3 {
		clock.Advance(renew)
		a.step(t, a, b)
		b.step(t, a, b)
		expectRoles(t, map[*haMember]string{a: server.HARoleActive, b: server.HARoleStandby})
	}

	// A graceful stop releases the lease: B takes over at its next read, from A's
	// last snapshot.
	if err := a.stop(t); err != nil {
		t.Fatalf("graceful stop of the active member: %v", err)
	}
	expectRoles(t, map[*haMember]string{a: server.HARoleStandby, b: server.HARoleStandby})
	b.stepUntilActive(t, a, b)
	if s := b.expectServed(t, true); s.marker != "A" {
		t.Fatalf("the new active member does not have the metadata of the previous one (marker %q)", s.marker)
	}
	if backups, err := store.DatabaseBackups(cfgB.MetadataDBPath()); err != nil || len(backups) != 1 {
		t.Fatalf("the standby's own database was not kept as a backup: %v, %v", backups, err)
	}

	// A restarted member comes back as standby and stays one while B renews.
	restarted := *cfgA
	restarted.Port = freeLoopbackPort(t)
	a2 := startHAMember(t, "A (restarted)", &restarted, clock, nil)
	a2.expectServed(t, false)
	for range 3 {
		clock.Advance(renew)
		b.step(t, b, a2)
		a2.step(t, b, a2)
		expectRoles(t, map[*haMember]string{a2: server.HARoleStandby, b: server.HARoleActive})
	}

	if err := b.stop(t); err != nil {
		t.Fatal(err)
	}
	if err := a2.stop(t); err != nil {
		t.Fatal(err)
	}
	// No member served a role twice: no second active App, no takeover run twice.
	for _, m := range []*haMember{a, b, a2} {
		select {
		case s := <-m.served:
			t.Fatalf("member %s served once more (active=%v)", m.name, s.active)
		default:
		}
	}
}

func TestStartupRefusesAnotherSecretKey(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
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

// TestStandbyAppStaysStandbyOnceTheLeaseIsAcquired checks the moment between the
// acquisition of the lease and the start of the active App: the standby App still
// serving must report standby and refuse sign-ins (its database and users are not
// trusted), and only the App opened as active reports active.
func TestStandbyAppStaysStandbyOnceTheLeaseIsAcquired(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	ctx := context.Background()
	sup := newTestSupervisor(t, haTestConfig(t, t.TempDir(), haTestKey(t)))
	sup.clock = newFakeClock().Now // the lease cannot expire however slowly the Apps open
	standby, err := sup.open(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = sup.bootstrap(ctx, standby); err != nil {
		_ = standby.Close()
		t.Fatal(err)
	}
	sup.rt.mgr().Step(ctx)
	if sup.rt.mgr().Role() != ha.RoleActive {
		_ = standby.Close()
		t.Fatal("did not acquire the lease")
	}
	role, metricsRole := healthRole(standby), standby.ha.metricsState().Role
	rec := httptest.NewRecorder()
	standby.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"x","password":"y"}`)))
	if err = standby.Close(); err != nil {
		t.Fatal(err)
	}
	if role != server.HARoleStandby || metricsRole != server.HARoleStandby {
		t.Fatalf("the standby App reports %q (metrics %q) once the lease is acquired; want standby", role, metricsRole)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("sign-in on the standby App once the lease is acquired = %d; want 503", rec.Code)
	}

	active, err := sup.open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = active.Close() }()
	if role = healthRole(active); role != server.HARoleActive {
		t.Fatalf("the active App reports %q; want active", role)
	}
}

package heartbeat

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// monitor is a fake dead-man's-switch service recording the requested paths.
type monitor struct {
	mu     sync.Mutex
	paths  []string
	status func(n int) int // status of the n-th request (1-based); nil = 200
	srv    *httptest.Server
}

func newMonitor(t *testing.T) *monitor {
	t.Helper()
	m := &monitor{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.paths = append(m.paths, r.URL.RequestURI())
		n := len(m.paths)
		status := m.status
		m.mu.Unlock()
		if status != nil {
			w.WriteHeader(status(n))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *monitor) got() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.paths)
}

// run starts s.Run and returns a function that stops it and waits for it.
func run(s *Service) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { s.Run(ctx) })
	// Wait until Run accepts pings.
	for {
		s.mu.Lock()
		ok := s.accepting
		s.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	return func() {
		cancel()
		wg.Wait()
	}
}

func testService(m *monitor, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return New(Config{Client: m.srv.Client(), Logger: logger, RetryDelay: time.Millisecond, Timeout: 2 * time.Second})
}

func TestJobPingSequence(t *testing.T) {
	cases := []struct {
		status models.JobRunStatus
		last   string
	}{
		{models.JobRunOK, "/ping/abc"},
		{models.JobRunFailed, "/ping/abc/fail"},
		{models.JobRunPartial, "/ping/abc/fail"},
		{models.JobRunCancelled, "/ping/abc/fail"},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			m := newMonitor(t)
			s := testService(m, nil)
			stop := run(s)
			job := &models.Job{ID: "job_1", HeartbeatURL: m.srv.URL + "/ping/abc"}
			s.JobRunStarted(job, &models.JobRun{Status: models.JobRunRunning})
			s.JobRunFinished(job, &models.JobRun{Status: tc.status})
			stop() // drains the pings in flight
			want := []string{"/ping/abc/start", tc.last}
			if got := m.got(); !slices.Equal(got, want) {
				t.Fatalf("pings = %v; want %v", got, want)
			}
		})
	}
}

func TestJobPingsStayInOrder(t *testing.T) {
	m := newMonitor(t)
	// The first ping is slow to fail: the outcome must still come after it.
	m.status = func(n int) int {
		if n == 1 {
			time.Sleep(20 * time.Millisecond)
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	}
	s := testService(m, nil)
	stop := run(s)
	job := &models.Job{ID: "job_1", HeartbeatURL: m.srv.URL + "/p"}
	s.JobRunStarted(job, nil)
	s.JobRunFinished(job, &models.JobRun{Status: models.JobRunOK})
	stop()
	want := []string{"/p/start", "/p/start", "/p"}
	if got := m.got(); !slices.Equal(got, want) {
		t.Fatalf("pings = %v; want %v (the retried start before the outcome)", got, want)
	}
}

func TestJobWithoutHeartbeatIsNotPinged(t *testing.T) {
	m := newMonitor(t)
	s := testService(m, nil)
	stop := run(s)
	s.JobRunStarted(&models.Job{ID: "job_1"}, nil)
	s.JobRunFinished(&models.Job{ID: "job_1"}, &models.JobRun{Status: models.JobRunOK})
	stop()
	if got := m.got(); len(got) != 0 {
		t.Fatalf("pings = %v; want none", got)
	}
}

func TestPingsOutsideRunAreDropped(t *testing.T) {
	m := newMonitor(t)
	s := testService(m, nil)
	s.JobRunFinished(&models.Job{ID: "job_1", HeartbeatURL: m.srv.URL}, &models.JobRun{Status: models.JobRunOK})
	stop := run(s)
	stop()
	s.JobRunFinished(&models.Job{ID: "job_1", HeartbeatURL: m.srv.URL}, &models.JobRun{Status: models.JobRunOK})
	if got := m.got(); len(got) != 0 {
		t.Fatalf("pings = %v; want none before Run and after it returned", got)
	}
}

func TestRetries(t *testing.T) {
	cases := []struct {
		name   string
		status func(n int) int
		want   int
		ok     bool
	}{
		{"retryable then ok", func(n int) int {
			if n < 3 {
				return http.StatusBadGateway
			}
			return http.StatusOK
		}, 3, true},
		{"too many requests", func(int) int { return http.StatusTooManyRequests }, DefaultAttempts, false},
		{"permanent", func(int) int { return http.StatusNotFound }, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMonitor(t)
			m.status = tc.status
			s := testService(m, nil)
			err := s.send(context.Background(), m.srv.URL+"/p", SignalSuccess)
			if (err == nil) != tc.ok {
				t.Fatalf("send error = %v; want ok %v", err, tc.ok)
			}
			if got := len(m.got()); got != tc.want {
				t.Fatalf("attempts = %d; want %d", got, tc.want)
			}
		})
	}
}

// clock is a settable time source.
type clock struct{ ns atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *clock) advance(d time.Duration) { c.ns.Add(int64(d)) }

func waitPings(t *testing.T, m *monitor, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(m.got()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("pings = %v; want %d", m.got(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestGlobalHeartbeatInterval(t *testing.T) {
	m := newMonitor(t)
	clk := &clock{}
	clk.ns.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	var healthy atomic.Bool
	healthy.Store(true)
	var mu sync.Mutex
	target := m.srv.URL + "/global"
	s := New(Config{
		Client: m.srv.Client(), Logger: slog.New(slog.DiscardHandler), CheckEvery: 2 * time.Millisecond, Now: clk.now,
		Global: func() (string, time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			return target, 5 * time.Minute
		},
		Healthy: healthy.Load,
	})
	stop := run(s)
	defer stop()

	waitPings(t, m, 1) // at start
	clk.advance(4 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if got := len(m.got()); got != 1 {
		t.Fatalf("pings = %d before the interval passed; want 1", got)
	}
	clk.advance(time.Minute)
	waitPings(t, m, 2) // the interval passed

	// An unhealthy scheduler stops the heartbeat, so the monitor alerts.
	healthy.Store(false)
	clk.advance(10 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if got := len(m.got()); got != 2 {
		t.Fatalf("pings = %d while the scheduler is stale; want 2", got)
	}
	healthy.Store(true)
	waitPings(t, m, 3) // resumed at once

	// A new URL is pinged at once.
	mu.Lock()
	target = m.srv.URL + "/other"
	mu.Unlock()
	waitPings(t, m, 4)
	if got := m.got(); got[3] != "/other" {
		t.Fatalf("pings = %v; want the new URL last", got)
	}
}

func TestURLIsNeverLogged(t *testing.T) {
	m := newMonitor(t)
	m.status = func(int) int { return http.StatusInternalServerError }
	var buf bytes.Buffer
	var bufMu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &bufMu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := testService(m, logger)
	stop := run(s)
	secretURL := m.srv.URL + "/ping/secret-token-123?key=hidden-query"
	s.JobRunStarted(&models.Job{ID: "job_1", HeartbeatURL: secretURL}, nil)
	stop()

	err := s.Test(context.Background(), secretURL)
	if !errors.Is(err, ErrPingFailed) {
		t.Fatalf("Test error = %v; want ErrPingFailed", err)
	}
	bufMu.Lock()
	logs := buf.String()
	bufMu.Unlock()
	for _, text := range []string{logs, err.Error()} {
		if strings.Contains(text, "secret-token-123") || strings.Contains(text, "hidden-query") {
			t.Fatalf("the heartbeat URL leaked: %s", text)
		}
	}
	if !strings.Contains(logs, "job heartbeat ping failed") || !strings.Contains(logs, "127.0.0.1") {
		t.Fatalf("the failure with its host was not logged: %s", logs)
	}
}

// lockedWriter serialises writes to w.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestTestPing(t *testing.T) {
	m := newMonitor(t)
	// The default client (notify.NewHTTPClient) allows loopback receivers.
	s := New(Config{Logger: slog.New(slog.DiscardHandler)})
	if err := s.Test(context.Background(), m.srv.URL+"/check"); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if got := m.got(); !slices.Equal(got, []string{"/check"}) {
		t.Fatalf("pings = %v", got)
	}
	for _, bad := range []string{"", "ftp://example.com/x", "https://user:pw@example.com/x"} {
		if err := s.Test(context.Background(), bad); !errors.Is(err, ErrPingFailed) {
			t.Errorf("Test(%q) = %v; want ErrPingFailed", bad, err)
		}
	}
	// Link-local destinations (cloud metadata) are refused.
	if err := s.Test(context.Background(), "http://169.254.169.254/latest"); !errors.Is(err, ErrPingFailed) {
		t.Fatalf("Test(metadata) = %v; want ErrPingFailed", err)
	}
}

func TestPingURL(t *testing.T) {
	cases := []struct {
		raw  string
		sig  Signal
		want string
	}{
		{"https://hc-ping.com/uuid", SignalSuccess, "https://hc-ping.com/uuid"},
		{"https://hc-ping.com/uuid", SignalStart, "https://hc-ping.com/uuid/start"},
		{"https://hc-ping.com/uuid/", SignalFail, "https://hc-ping.com/uuid/fail"},
		{"https://push.example.com/api/push/x?status=up", SignalFail, "https://push.example.com/api/push/x/fail?status=up"},
	}
	for _, tc := range cases {
		got, err := PingURL(tc.raw, tc.sig)
		if err != nil || got != tc.want {
			t.Errorf("PingURL(%q, %q) = %q, %v; want %q", tc.raw, tc.sig, got, err, tc.want)
		}
	}
	if _, err := PingURL("not a url", SignalStart); err == nil {
		t.Error("PingURL accepted an invalid URL")
	}
}

func TestSignalOf(t *testing.T) {
	if SignalOf(models.JobRunOK) != SignalSuccess || SignalOf(models.JobRunPartial) != SignalFail ||
		SignalOf(models.JobRunFailed) != SignalFail || SignalOf(models.JobRunCancelled) != SignalFail {
		t.Fatal("SignalOf maps run statuses wrongly")
	}
}

package app

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// storedBytesScrape returns the lines of the stored bytes gauge of the metrics.
func storedBytesScrape(a *App) string {
	rec := httptest.NewRecorder()
	a.metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	var lines []string
	for _, l := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(l, "mongorescue_storage_stored_bytes{") {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// TestCostMonitorRunsOnTheActiveInstanceOnly proves the snapshot job runs when the
// instance is active (it sets the gauge of every target at once and stops with the
// background workers) and never on an HA standby.
func TestCostMonitorRunsOnTheActiveInstanceOnly(t *testing.T) {
	standby, err := New(testConfig(t), nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = standby.Close() })
	standby.startBackground(true)()
	if got := storedBytesScrape(standby); got != "" {
		t.Errorf("a standby exports the stored bytes gauge:\n%s", got)
	}

	active, err := New(testConfig(t), nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = active.Close() })
	stop := active.startBackground(false)
	deadline := time.Now().Add(10 * time.Second)
	for storedBytesScrape(active) == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	got := storedBytesScrape(active)
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the snapshot job did not stop with the background workers")
	}
	if !strings.Contains(got, `{target="stg_`) || !strings.HasSuffix(got, " 0") {
		t.Errorf("the default target has no gauge (or a wrong one):\n%q", got)
	}
}

// TestCostMonitorTakesTheSnapshot proves one snapshot records today's rows and
// sets the gauge.
func TestCostMonitorTakesTheSnapshot(t *testing.T) {
	a, err := New(testConfig(t), nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	a.costUsage.snapshot(t.Context())
	if got := storedBytesScrape(a); !strings.Contains(got, "mongorescue_storage_stored_bytes{target=") {
		t.Errorf("gauge missing after a snapshot: %q", got)
	}
	// Snapshots the same day are idempotent: the gauge keeps a single series per target.
	a.costUsage.snapshot(t.Context())
	if lines := strings.Split(storedBytesScrape(a), "\n"); len(lines) != 1 {
		t.Errorf("series = %v", lines)
	}
}

package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/update"
)

// fakeClock is a clock whose time only moves with Advance. It records the
// duration of every timer.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	waits  []time.Duration
}

// fakeTimer is a timer of fakeClock.
type fakeTimer struct {
	at   time.Time
	c    chan time.Time
	done bool // fired or stopped
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), c: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	c.waits = append(c.waits, d)
	return t.c, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.done
		t.done = true
		return was
	}
}

// Advance moves the time on by d and fires the timers that are due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		if !t.done && !t.at.After(c.now) {
			t.done = true
			t.c <- c.now
		}
	}
}

// wait waits for the n-th timer (from 1) and returns its duration.
func (c *fakeClock) wait(t *testing.T, n int) time.Duration {
	t.Helper()
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.waits) >= n
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waits[n-1]
}

// waitFor waits until ok reports true.
func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition never reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// upToDate is a check result without a newer version.
func upToDate() update.Result {
	return update.Result{Current: update.Version{Major: 1}, Latest: update.Version{Major: 1}}
}

// setGate makes the checks wait for gate (nil: not at all).
func (f *fakeSource) setGate(gate chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = gate
}

// clocked returns a started updater for src on the fake clock clk.
func clocked(t *testing.T, src *fakeSource, clk *fakeClock) *Updater {
	t.Helper()
	opts := newRecorder(t).options(src, "1.0.0", "linux")
	opts.clock = clk
	u, _ := started(t, opts)
	return u
}

func TestUpdaterIntervalAndBackoff(t *testing.T) {
	if UpdateCheckInterval != 30*time.Minute || UpdateCheckMaxBackoff != 6*time.Hour || UpdateRecheckAge != 5*time.Minute {
		t.Fatalf("interval %v, backoff %v, recheck %v", UpdateCheckInterval, UpdateCheckMaxBackoff, UpdateRecheckAge)
	}
	src := &fakeSource{res: upToDate()}
	clk := newFakeClock()
	clocked(t, src, clk)
	if got := clk.wait(t, 1); got != UpdateCheckInterval {
		t.Fatalf("after the startup check: %v", got)
	}

	// Offline: the wait doubles up to 6 hours.
	src.set(update.Result{}, errors.New("offline"))
	n := 1
	for _, want := range []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour, 6 * time.Hour} {
		clk.Advance(clk.wait(t, n))
		n++
		if got := clk.wait(t, n); got != want {
			t.Fatalf("after failure %d: %v; want %v", n-1, got, want)
		}
	}

	// A success returns to the interval, and the next failure starts over.
	src.set(upToDate(), nil)
	clk.Advance(clk.wait(t, n))
	n++
	if got := clk.wait(t, n); got != UpdateCheckInterval {
		t.Fatalf("after the success: %v", got)
	}
	src.set(update.Result{}, errors.New("offline"))
	clk.Advance(clk.wait(t, n))
	n++
	if got := clk.wait(t, n); got != time.Hour {
		t.Fatalf("after a new failure: %v", got)
	}
	if got := src.checkCount(); got != n {
		t.Errorf("checks = %d; want %d", got, n)
	}
}

func TestUpdaterHonoursRateLimit(t *testing.T) {
	src := &fakeSource{res: upToDate()}
	clk := newFakeClock()
	u := clocked(t, src, clk)
	clk.wait(t, 1)

	reset := clk.Now().Add(UpdateCheckInterval + 3*time.Hour)
	src.set(update.Result{}, &update.RateLimitError{Status: "403 Forbidden", Reset: reset})
	clk.Advance(UpdateCheckInterval)
	if got := clk.wait(t, 2); got != 3*time.Hour {
		t.Fatalf("wait after the rate limit: %v; want until the reset", got)
	}

	// Until the reset, a requested check does not ask GitHub and keeps the wait.
	n := src.checkCount()
	if err := u.CheckNow(context.Background()); !errors.Is(err, update.ErrRateLimited) {
		t.Fatalf("CheckNow = %v", err)
	}
	if src.checkCount() != n {
		t.Error("a check during the rate limit asked GitHub")
	}
	if got := clk.wait(t, 3); got != 3*time.Hour {
		t.Errorf("wait after the skipped check: %v", got)
	}

	src.set(upToDate(), nil)
	clk.Advance(3 * time.Hour)
	if got := clk.wait(t, 4); got != UpdateCheckInterval {
		t.Errorf("wait after the reset: %v", got)
	}

	// A reset beyond the maximum backoff is capped.
	src.set(update.Result{}, &update.RateLimitError{Status: "429", Reset: clk.Now().Add(UpdateCheckInterval + 24*time.Hour)})
	clk.Advance(UpdateCheckInterval)
	if got := clk.wait(t, 5); got != UpdateCheckMaxBackoff {
		t.Errorf("capped wait: %v", got)
	}
}

func TestUpdaterCheckNowCoalesces(t *testing.T) {
	src := &fakeSource{res: upToDate()}
	clk := newFakeClock()
	u := clocked(t, src, clk)
	clk.wait(t, 1)

	gate := make(chan struct{})
	src.setGate(gate)
	const n = 8
	errs := make(chan error, n)
	for range n {
		go func() { errs <- u.CheckNow(context.Background()) }()
	}
	waitFor(t, func() bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.checking && u.round != nil && u.round.requests == n
	})
	if !u.Status().Checking {
		t.Error("status does not report the check")
	}
	// A caller that gives up does not end the shared check.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := u.CheckNow(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled CheckNow = %v", err)
	}
	src.set(available(false), nil)
	close(gate)
	for range n {
		if err := <-errs; err != nil {
			t.Errorf("CheckNow = %v", err)
		}
	}
	if got := src.checkCount(); got != 2 {
		t.Errorf("checks = %d; want the startup check and one shared check", got)
	}
	if s := u.Status(); !s.Available || s.Checking || s.Latest != "1.1.0" {
		t.Errorf("status = %+v", s)
	}
	if got := clk.wait(t, 2); got != UpdateCheckInterval {
		t.Errorf("wait after the requested check: %v", got)
	}

	// Errors and states that do not check.
	src.set(update.Result{}, errors.New("offline"))
	if err := u.CheckNow(context.Background()); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("failed CheckNow = %v", err)
	}
	if err := NewUpdater(UpdaterOptions{Version: "1.0.0", Source: src}).CheckNow(context.Background()); !errors.Is(err, ErrUpdaterNotStarted) {
		t.Errorf("before Start: %v", err)
	}
	dev, _ := started(t, newRecorder(t).options(src, "dev", "linux"))
	if err := dev.CheckNow(context.Background()); !errors.Is(err, ErrUpdatesDisabled) {
		t.Errorf("dev build: %v", err)
	}
}

func TestUpdaterCheckNowStopsWithTheUpdater(t *testing.T) {
	src := &fakeSource{res: upToDate()}
	opts := newRecorder(t).options(src, "1.0.0", "linux")
	opts.clock = newFakeClock()
	u, cancel := started(t, opts)
	src.setGate(make(chan struct{}))
	errs := make(chan error, 1)
	go func() { errs <- u.CheckNow(context.Background()) }()
	waitFor(t, func() bool { return u.Status().Checking })
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Errorf("CheckNow = %v", err)
	}
	u.Wait()
}

func TestUpdaterWindowShownRechecksAfterFiveMinutes(t *testing.T) {
	src := &fakeSource{res: upToDate()}
	clk := newFakeClock()
	u := clocked(t, src, clk)
	clk.wait(t, 1)

	if u.WindowShown() {
		t.Error("checked right after the startup check")
	}
	clk.Advance(UpdateRecheckAge)
	if u.WindowShown() {
		t.Error("checked at exactly five minutes")
	}
	clk.Advance(time.Second)
	if !u.WindowShown() {
		t.Fatal("did not check after five minutes")
	}
	waitFor(t, func() bool { return src.checkCount() == 2 })
	clk.wait(t, 2)
	if u.WindowShown() {
		t.Error("checked again right after the check")
	}

	// No second check while one runs.
	gate := make(chan struct{})
	src.setGate(gate)
	clk.Advance(UpdateRecheckAge + time.Second)
	if !u.WindowShown() {
		t.Fatal("did not check")
	}
	waitFor(t, func() bool { return u.Status().Checking })
	if u.WindowShown() {
		t.Error("asked for a second check while one runs")
	}
	close(gate)
	clk.wait(t, 3)
	if got := src.checkCount(); got != 3 {
		t.Errorf("checks = %d", got)
	}

	dev, _ := started(t, newRecorder(t).options(src, "dev", "linux"))
	if dev.WindowShown() {
		t.Error("a dev build checked")
	}
}

func TestUpdaterCheckEndpoint(t *testing.T) {
	src := &fakeSource{res: upToDate()}
	u := clocked(t, src, newFakeClock())
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := u.Handler(next)
	desktopHdr := map[string]string{UpdateHeader: "1", "Origin": "http://wails.localhost"}

	post := func(hdr map[string]string) (int, map[string]any) {
		rec := serveUpdate(h, http.MethodPost, UpdateCheckPath, hdr)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	code, body := post(desktopHdr)
	if code != http.StatusOK || body["available"] != false || body["current"] != "1.0.0" || body["checking"] != false {
		t.Fatalf("check = %d %v", code, body)
	}
	if src.checkCount() != 2 {
		t.Errorf("checks = %d", src.checkCount())
	}
	src.set(available(false), nil)
	if code, body = post(desktopHdr); code != http.StatusOK || body["available"] != true || body["latest"] != "1.1.0" {
		t.Errorf("check = %d %v", code, body)
	}

	for _, hdr := range []map[string]string{
		{"Origin": "http://wails.localhost"},
		{UpdateHeader: "1", "Origin": "https://evil.example"},
		{UpdateHeader: "1", "Sec-Fetch-Site": "cross-site"},
	} {
		if got, _ := post(hdr); got != http.StatusForbidden {
			t.Errorf("POST %v = %d; want 403", hdr, got)
		}
	}
	if rec := serveUpdate(h, http.MethodGet, UpdateCheckPath, nil); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("GET = %d", rec.Code)
	}
	if src.checkCount() != 3 {
		t.Errorf("a refused request checked: %d checks", src.checkCount())
	}

	src.set(update.Result{}, errors.New("offline"))
	if code, body = post(desktopHdr); code != http.StatusBadGateway || !strings.Contains(fmt.Sprint(body["error"]), "offline") {
		t.Errorf("failed check = %d %v", code, body)
	}

	dev, _ := started(t, newRecorder(t).options(src, "dev", "linux"))
	if rec := serveUpdate(dev.Handler(next), http.MethodPost, UpdateCheckPath, desktopHdr); rec.Code != http.StatusConflict {
		t.Errorf("dev build = %d", rec.Code)
	}
	idle := NewUpdater(UpdaterOptions{Version: "1.0.0", Source: src})
	if rec := serveUpdate(idle.Handler(next), http.MethodPost, UpdateCheckPath, desktopHdr); rec.Code != http.StatusConflict {
		t.Errorf("not started = %d", rec.Code)
	}
}

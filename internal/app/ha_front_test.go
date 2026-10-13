package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/server"
)

// httpPoller requests paths of a base URL continuously from several workers and
// counts the answers.
type httpPoller struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	tr     *http.Transport

	mu    sync.Mutex
	codes map[int]int
	errs  []error
}

// startPoller starts workers polling paths of base in turn.
func startPoller(base string, workers int, paths ...string) *httpPoller {
	ctx, cancel := context.WithCancel(context.Background())
	p := &httpPoller{cancel: cancel, tr: &http.Transport{MaxIdleConnsPerHost: workers}, codes: map[int]int{}}
	client := &http.Client{Transport: p.tr, Timeout: 30 * time.Second}
	for w := range workers {
		p.wg.Go(func() {
			for i := w; ctx.Err() == nil; i++ {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+paths[i%len(paths)], nil)
				if err != nil {
					p.record(0, err)
					return
				}
				resp, err := client.Do(req)
				if err == nil {
					_, err = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				if ctx.Err() != nil {
					return
				}
				code := 0
				if resp != nil {
					code = resp.StatusCode
				}
				p.record(code, err)
			}
		})
	}
	return p
}

// record counts one answer.
func (p *httpPoller) record(code int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.errs = append(p.errs, err)
		return
	}
	p.codes[code]++
}

// stop stops the workers and returns the answers by status code and the errors.
func (p *httpPoller) stop() (map[int]int, []error) {
	p.cancel()
	p.wg.Wait()
	p.tr.CloseIdleConnections()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.codes, p.errs
}

// check fails t when the poller saw an error or a status code not in allowed.
func (p *httpPoller) check(t *testing.T, what string, allowed ...int) map[int]int {
	t.Helper()
	codes, errs := p.stop()
	if len(errs) > 0 {
		t.Errorf("%s: %d requests failed without an HTTP answer, the first: %v", what, len(errs), errs[0])
	}
	for code, n := range codes {
		if !slices.Contains(allowed, code) {
			t.Errorf("%s: %d answers with status %d; want only %v", what, n, code, allowed)
		}
	}
	return codes
}

// startTestFront starts a front on a loopback listener.
func startTestFront(t *testing.T) (*haFront, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	status := func() server.HAStatus { return server.HAStatus{Role: server.HARoleStandby, Cluster: "test"} }
	f := newFrontOn(ln, slog.New(slog.NewTextHandler(io.Discard, nil)), server.SwitchingHandler(status, "test", nil))
	f.start()
	t.Cleanup(f.close)
	return f, "http://" + ln.Addr().String()
}

// fakeRoleApp is the handler of an App with a store that closes after its detach.
type fakeRoleApp struct {
	closed     atomic.Bool
	violations atomic.Int64
	requests   atomic.Int64
}

// ServeHTTP answers 200 after a short time (0 to 3 ms, varying), and counts the
// requests that saw the store closed.
func (a *fakeRoleApp) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if a.closed.Load() {
		a.violations.Add(1)
	}
	time.Sleep(time.Duration(a.requests.Add(1)*997%3000) * time.Microsecond)
	if a.closed.Load() {
		a.violations.Add(1)
	}
	w.WriteHeader(http.StatusOK)
}

// TestHAFrontSwitchesUnderLoad switches the App behind a front again and again
// while requests keep coming: every request gets an answer, 200 from an App or 503
// between two Apps, and none reaches an App after its detach (its store closed).
func TestHAFrontSwitchesUnderLoad(t *testing.T) {
	t.Parallel()
	f, base := startTestFront(t)
	p := startPoller(base, 8, "/api/v1/jobs", "/api/v1/backups", "/")
	const switches = 30
	apps := make([]*fakeRoleApp, switches)
	for i := range apps {
		apps[i] = &fakeRoleApp{}
		f.attach(apps[i])
		time.Sleep(15 * time.Millisecond)
		if err := f.detach(time.Second); err != nil {
			t.Fatalf("switch %d: %v", i, err)
		}
		apps[i].closed.Store(true)
		time.Sleep(5 * time.Millisecond)
	}
	codes := p.check(t, "requests during the switches", http.StatusOK, http.StatusServiceUnavailable)
	if codes[http.StatusOK] == 0 || codes[http.StatusServiceUnavailable] == 0 {
		t.Fatalf("answers %v; want both 200 and 503", codes)
	}
	for i, a := range apps {
		if n := a.violations.Load(); n > 0 {
			t.Fatalf("App %d served %d requests after its store was closed", i, n)
		}
	}
}

// TestHAFrontAbortsRequestsAfterTheDrain checks that a request still running when
// the drain ends (an event stream) is aborted: its context is cancelled, the
// detach waits for it to end, and the client sees the connection cut rather than a
// complete response.
func TestHAFrontAbortsRequestsAfterTheDrain(t *testing.T) {
	t.Parallel()
	f, base := startTestFront(t)
	started := make(chan struct{})
	var ended atomic.Bool
	f.attach(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: hello\n\n"))
		http.NewResponseController(w).Flush() //nolint:errcheck // a test stream
		close(started)
		<-r.Context().Done()
		ended.Store(true)
	}))
	result := make(chan error, 1)
	go func() {
		resp, err := http.Get(base + "/api/v1/events") //nolint:noctx // bounded by the abort
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		result <- err
	}()
	await(t, started, "the start of the stream")
	if err := f.detach(50 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !ended.Load() {
		t.Fatal("detach returned while the aborted request still ran")
	}
	if err := await(t, result, "the end of the stream"); err == nil {
		t.Fatal("the aborted stream ended like a complete response")
	}
	resp, err := http.Get(base + "/api/v1/events") //nolint:noctx // a local test server
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a request after the detach = %d; want 503", resp.StatusCode)
	}
}

// TestHAFrontReportsStuckRequests checks that a request that ignores its abort
// keeps the App from being closed: detach returns errHTTPHandlersRunning.
func TestHAFrontReportsStuckRequests(t *testing.T) {
	t.Parallel()
	f, base := startTestFront(t)
	f.abortWait = 50 * time.Millisecond
	started, unblock := make(chan struct{}), make(chan struct{})
	f.attach(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-unblock
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.Get(base + "/"); err == nil { //nolint:noctx // bounded by unblock
			_ = resp.Body.Close()
		}
	}()
	await(t, started, "the start of the request")
	err := f.detach(0)
	close(unblock)
	<-done
	if !errors.Is(err, errHTTPHandlersRunning) {
		t.Fatalf("detach with a stuck request = %v; want errHTTPHandlersRunning", err)
	}
	if err = closeApp(nil, fmt.Errorf("shutdown: %w", err)); !errors.Is(err, errHTTPHandlersRunning) {
		t.Fatalf("closeApp under a stuck request = %v", err)
	}
}

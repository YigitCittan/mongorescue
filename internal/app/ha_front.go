package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/metrics"
	"github.com/yigitcittan/mongorescue/internal/server"
)

// haHandlerAbortWait bounds the wait for the requests of a detached App once their
// contexts are cancelled and their connections' deadlines have passed.
const haHandlerAbortWait = 5 * time.Second

// errHTTPHandlersRunning is returned when requests of a detached App did not end
// after they were aborted: the App must not be closed under them, so the process
// exits and its restart policy starts a fresh standby.
var errHTTPHandlersRunning = errors.New("app: HTTP requests did not end after they were aborted; exiting so that the restart policy starts a fresh standby")

// haFront serves the HTTP listener of an active-passive HA member across the Apps
// of its roles, so a role change never refuses a connection. It dispatches every
// request to the App attached to it; while none is (between the close of one App
// and the start of the next), server.SwitchingHandler answers.
type haFront struct {
	logger    *slog.Logger
	srv       *http.Server
	ln        net.Listener
	switching http.Handler
	// abortWait bounds the wait for aborted requests (haHandlerAbortWait; tests
	// shorten it).
	abortWait time.Duration
	// afterRelease, when set, runs after a handler returned and its request was
	// released (tests).
	afterRelease func()

	cur   atomic.Pointer[frontSlot]
	errCh chan error
	wg    sync.WaitGroup
	once  sync.Once
}

// newHAFront listens on the address of cfg. The HA status of the switching
// responses and metrics is rt's; version and commit label the build.
func newHAFront(cfg *config.Config, logger *slog.Logger, rt *haRuntime, version, commit string) (*haFront, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("http server failed: %w", err)
	}
	// The metrics of the switch: the process, build and HA series of a standby (the
	// lease may already be held, but no App serves as active).
	ms := metrics.New(metrics.BuildInfo{Version: version, Commit: commit, GoVersion: runtime.Version()})
	ms.SetHASource(func() metrics.HAState {
		st := rt.metricsState()
		st.Role = server.HARoleStandby
		return st
	})
	f := newFrontOn(ln, logger, server.SwitchingHandler(rt.serverStatus, version, ms.StandbyHandler()))
	f.srv.Addr = addr
	return f, nil
}

// newFrontOn returns a front serving ln, with switching answering while no App is
// attached.
func newFrontOn(ln net.Listener, logger *slog.Logger, switching http.Handler) *haFront {
	f := &haFront{logger: logger, ln: ln, switching: switching, abortWait: haHandlerAbortWait, errCh: make(chan error, 1)}
	f.srv = server.NewHTTPServer(ln.Addr().String(), f)
	return f
}

// start serves the listener until shutdown or close.
func (f *haFront) start() {
	f.logger.Info("starting mongorescue http web dashboard & api", slog.String("addr", f.srv.Addr))
	f.wg.Go(func() {
		if err := f.srv.Serve(f.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			f.errCh <- fmt.Errorf("http server failed: %w", err)
		}
	})
}

// errs receives the error of the server when it fails.
func (f *haFront) errs() <-chan error { return f.errCh }

// attach makes h serve the requests from now on. The App of h must be started: no
// request reaches it before.
func (f *haFront) attach(h http.Handler) {
	ctx, cancel := context.WithCancel(context.Background())
	slot := &frontSlot{h: h, ctx: ctx, abort: cancel, inflight: map[*frontRequest]struct{}{}, idle: make(chan struct{})}
	if old := f.cur.Swap(slot); old != nil {
		// Not reached: every App is detached before the next one is attached.
		old.refuse()
		old.abortAll()
	}
}

// detach stops dispatching requests to the attached App: new requests get the
// switching answers at once, and the requests in progress may finish within drain.
// Then they are aborted (their contexts cancelled, their connections cut). It
// returns once none of them runs, so the App can be closed, or
// errHTTPHandlersRunning when some did not end within abortWait after the abort.
// A nil front (an App driven without a listener, in tests) has nothing to detach.
func (f *haFront) detach(drain time.Duration) error {
	return f.settle(f.take(), drain)
}

// cut detaches the App without any drain and without waiting: its requests in
// progress are aborted at once. When the lease is lost, the App's runs are stopped
// right after, and settle(slot, 0) waits for the requests only then.
func (f *haFront) cut() *frontSlot {
	slot := f.take()
	if slot != nil {
		f.abortSlot(slot)
	}
	return slot
}

// take stops dispatching requests to the attached App and returns its slot (nil
// when none is attached, or for a nil front).
func (f *haFront) take() *frontSlot {
	if f == nil {
		return nil
	}
	slot := f.cur.Swap(nil)
	if slot != nil {
		slot.refuse()
	}
	return slot
}

// abortSlot aborts the requests in progress of slot.
func (f *haFront) abortSlot(slot *frontSlot) {
	if n := slot.abortAll(); n > 0 {
		f.logger.Info("aborted the HTTP requests still running on the previous App", slog.Int("requests", n))
	}
}

// settle lets the requests in progress of a detached slot finish within drain,
// aborts the rest and waits at most abortWait for them (see detach).
func (f *haFront) settle(slot *frontSlot, drain time.Duration) error {
	if slot == nil {
		return nil
	}
	select {
	case <-slot.idle:
		slot.abort()
		return nil
	default:
	}
	if drain > 0 {
		timer := time.NewTimer(drain)
		select {
		case <-slot.idle:
			timer.Stop()
			slot.abort()
			return nil
		case <-timer.C:
		}
	}
	f.abortSlot(slot)
	timer := time.NewTimer(f.abortWait)
	defer timer.Stop()
	select {
	case <-slot.idle:
		return nil
	case <-timer.C:
		return fmt.Errorf("%w (%d requests)", errHTTPHandlersRunning, slot.running())
	}
}

// shutdown is the HTTP part of a graceful shutdown: the listener closes, and the
// requests in progress may finish until ctx ends; then the connections are closed
// and the App detached.
func (f *haFront) shutdown(ctx context.Context) error {
	err := f.srv.Shutdown(ctx)
	if err != nil {
		_ = f.srv.Close()
	}
	return errors.Join(err, f.detach(0))
}

// stop shuts the server down, waiting at most drain for the requests in progress.
func (f *haFront) stop(drain time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	return f.shutdown(ctx)
}

// close closes the listener and every connection and waits for the server's
// goroutine. The requests of an attached App are aborted.
func (f *haFront) close() {
	f.once.Do(func() {
		_ = f.srv.Close()
		if slot := f.cur.Swap(nil); slot != nil {
			slot.refuse()
			slot.abortAll()
		}
		f.wg.Wait()
	})
}

// ServeHTTP dispatches r to the attached App, or answers with the switching
// handler.
func (f *haFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slot := f.cur.Load()
	if slot == nil {
		f.switching.ServeHTTP(w, r)
		return
	}
	req, ok := slot.acquire(w)
	if !ok {
		f.switching.ServeHTTP(w, r)
		return
	}
	released := false
	defer func() {
		if !released {
			slot.release(req)
		}
	}()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(slot.ctx, cancel)
	slot.h.ServeHTTP(w, r.WithContext(ctx))
	stop()
	cancel()
	// The handler has finished: from now on an abort leaves its response alone.
	released = true
	aborted := slot.release(req)
	if f.afterRelease != nil {
		f.afterRelease()
	}
	if aborted {
		// The handler was still running when its request was aborted: cut the
		// connection, so a partial response is not taken for a complete one.
		panic(http.ErrAbortHandler)
	}
}

// frontSlot is an App attached to a haFront, with its requests in progress.
type frontSlot struct {
	h http.Handler
	// ctx is cancelled when the requests in progress are aborted.
	ctx   context.Context
	abort context.CancelFunc

	mu       sync.Mutex
	refused  bool
	inflight map[*frontRequest]struct{}
	// idle is closed once the slot refuses requests and none runs.
	idle   chan struct{}
	closed bool
}

// frontRequest is a request in progress on a frontSlot.
type frontRequest struct {
	rc *http.ResponseController
	// aborted is set (under the slot's mutex) when the request is aborted while
	// its handler runs.
	aborted bool
}

// acquire registers a request; false once the slot refuses requests.
func (s *frontSlot) acquire(w http.ResponseWriter) (*frontRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refused {
		return nil, false
	}
	req := &frontRequest{rc: http.NewResponseController(w)}
	s.inflight[req] = struct{}{}
	return req, true
}

// release unregisters a request whose handler returned, and reports whether it
// was aborted while the handler ran.
func (s *frontSlot) release(req *frontRequest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, req)
	s.closeIdleLocked()
	return req.aborted
}

// refuse makes the slot refuse new requests.
func (s *frontSlot) refuse() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused = true
	s.closeIdleLocked()
}

// closeIdleLocked closes idle once the slot refuses requests and none runs.
func (s *frontSlot) closeIdleLocked() {
	if s.refused && len(s.inflight) == 0 && !s.closed {
		s.closed = true
		close(s.idle)
	}
}

// running returns the number of requests in progress.
func (s *frontSlot) running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inflight)
}

// abortAll cancels the contexts of the requests in progress and makes their
// connections' reads and writes fail at once, like the close of a server. A
// request whose handler has returned is not touched: its response is delivered.
// It returns the number of requests aborted.
func (s *frontSlot) abortAll() int {
	s.abort()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for req := range s.inflight {
		req.aborted = true
		_ = req.rc.SetReadDeadline(now)
		_ = req.rc.SetWriteDeadline(now)
	}
	return len(s.inflight)
}

package heartbeat

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
)

// TestPendingPingsAreCapped proves dispatch never blocks: beyond MaxPending queued
// or in-flight pings, new ones are dropped and counted.
func TestPendingPingsAreCapped(t *testing.T) {
	release := make(chan struct{})
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
		served.Add(1)
	}))
	t.Cleanup(srv.Close)
	var dropped atomic.Int32
	s := New(Config{Client: srv.Client(), Logger: slog.New(slog.DiscardHandler), MaxPending: 2,
		OnDrop: func() { dropped.Add(1) }})
	stop := run(s)

	start := time.Now()
	for i := range 5 {
		s.JobRunStarted(&models.Job{ID: fmt.Sprintf("job_%d", i), HeartbeatURL: srv.URL + "/p"}, nil)
	}
	if time.Since(start) > time.Second {
		t.Fatal("dispatch blocked on a slow monitor")
	}
	if got := dropped.Load(); got != 3 {
		t.Fatalf("dropped = %d; want 3 of 5 with MaxPending 2", got)
	}
	close(release)
	stop()
	if got := served.Load(); got != 2 {
		t.Fatalf("served = %d; want 2", got)
	}
	// Finished pings free their slots.
	s2 := New(Config{Client: srv.Client(), Logger: slog.New(slog.DiscardHandler), MaxPending: 1,
		OnDrop: func() { dropped.Add(1) }})
	stop2 := run(s2)
	before := dropped.Load()
	for range 3 {
		s2.JobRunStarted(&models.Job{ID: "job_x", HeartbeatURL: srv.URL + "/p"}, nil)
		waitFor(t, func() bool { s2.mu.Lock(); defer s2.mu.Unlock(); return s2.pending == 0 })
	}
	stop2()
	if dropped.Load() != before {
		t.Fatal("pings were dropped although the earlier ones had finished")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTransportProblem(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 1, 2, 3), Port: 8443},
		Err: &net.OpError{Err: syscall.ECONNREFUSED}}
	cases := []struct {
		err  error
		want string
	}{
		{&url.Error{Op: "Get", URL: "https://hc/secret", Err: refused}, "connection refused"},
		{&url.Error{Op: "Get", URL: "https://hc/secret", Err: &net.DNSError{Err: "no such host", Name: "hc"}}, "DNS error"},
		{&url.Error{Op: "Get", URL: "https://hc/secret", Err: context.DeadlineExceeded}, "timeout"},
		{&url.Error{Op: "Get", URL: "https://hc/secret", Err: x509.UnknownAuthorityError{}}, "TLS error"},
		{fmt.Errorf("dial: %w", notify.ErrBlockedDestination), "destination not allowed"},
		{context.Canceled, "cancelled"},
		{errors.New("something else at 10.1.2.3:8443"), "connection failed"},
	}
	for _, tc := range cases {
		if got := TransportProblem(tc.err); got != tc.want {
			t.Errorf("TransportProblem(%v) = %q; want %q", tc.err, got, tc.want)
		}
	}
}

// TestFailuresNameTheHostOnly proves that ping errors carry a fixed reason and the
// URL's host, never the dialled address or the URL.
func TestFailuresNameTheHostOnly(t *testing.T) {
	// A port that refuses connections: a server that was closed.
	closed := httptest.NewServer(http.NotFoundHandler())
	addr := closed.Listener.Addr().String()
	closed.Close()
	// A plain-HTTP server spoken to over TLS.
	plain := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(plain.Close)
	plainAddr := strings.TrimPrefix(plain.URL, "http://")

	// A TLS server whose certificate the client does not trust.
	untrusted := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(untrusted.Close)

	s := New(Config{Logger: slog.New(slog.DiscardHandler), Timeout: 2 * time.Second})
	for raw, want := range map[string]string{
		"http://" + addr + "/ping/secret-token":       "connection refused",
		"https://" + plainAddr + "/ping/secret-token": "TLS error",
		untrusted.URL + "/ping/secret-token":          "TLS error",
	} {
		err := s.Test(context.Background(), raw)
		if !errors.Is(err, ErrPingFailed) || !strings.Contains(err.Error(), want) {
			t.Fatalf("Test(%s) = %v; want %q", raw, err, want)
		}
		u, _ := url.Parse(raw)
		if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), ":"+u.Port()) {
			t.Fatalf("the error leaks the URL or the address: %v", err)
		}
	}
}

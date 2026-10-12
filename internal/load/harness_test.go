//go:build load

// Package load is the load test (build tag "load"): it runs the real mongorescue
// binary with a large database, hundreds of connections and thousands of scheduled
// jobs, and measures scheduler tick latency, memory, dashboard API latencies,
// SQLite contention under concurrent backups, backup throughput and the impact of a
// dump on a busy primary (with primary and secondary read preference, and
// throttled). It writes a JSON report and fails on regressions against
// testdata/baseline.json. scripts/test-chaos-docker.sh with SUITE=load starts the
// services; docs/testing.md has the knobs and the published numbers.
package load

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/server"
)

// syncBuffer is a goroutine-safe buffer for process output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// proc is the server under test.
type proc struct {
	cmd  *exec.Cmd
	base string
	logs *syncBuffer
	done chan error
}

// startServer builds the binary and runs it on a fresh data directory.
func startServer(t *testing.T) *proc {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mongorescue")
	build := exec.Command("go", "build", "-o", bin, "github.com/yigitcittan/mongorescue/cmd/mongorescue")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	p := &proc{logs: &syncBuffer{}, done: make(chan error, 1), base: fmt.Sprintf("http://127.0.0.1:%d", port)}
	p.cmd = exec.Command(bin, "-data-dir", t.TempDir(), "-host", "127.0.0.1", "-port", strconv.Itoa(port))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MONGORESCUE_") {
			p.cmd.Env = append(p.cmd.Env, kv)
		}
	}
	p.cmd.Stdout, p.cmd.Stderr = p.logs, p.logs
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() {
		_ = p.cmd.Process.Signal(os.Interrupt)
		select {
		case <-p.done:
		case <-time.After(60 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		if dir := os.Getenv("MONGORESCUE_CHAOS_REPORT_DIR"); dir != "" {
			_ = os.WriteFile(filepath.Join(dir, "load-server.log"), []byte(p.logs.String()), 0o600)
		}
	})
	deadline := time.Now().Add(60 * time.Second)
	for {
		if resp, err := http.Get(p.base + "/api/v1/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return p
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start:\n%s", p.logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// rssMiB returns the resident set size of the process in MiB.
func (p *proc) rssMiB() float64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(p.cmd.Process.Pid)).Output()
	if err != nil {
		return 0
	}
	kb, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return kb / 1024
}

// api calls the REST API with an API key (or a session during setup).
type api struct {
	t      *testing.T
	base   string
	client *http.Client
	key    string
	csrf   string
}

type envelope struct {
	Data json.RawMessage `json:"data"`
}

func newAPI(t *testing.T, base string) *api {
	return &api{t: t, base: base, client: &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{MaxIdleConnsPerHost: 64}}}
}

// do sends a request and returns its status, body and duration.
func (c *api) do(method, path string, body any) (int, []byte, time.Duration) {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		req.Header.Set(server.CSRFHeader, c.csrf)
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	start := time.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, time.Since(start)
}

// data requires status want and decodes the envelope's data into out.
func (c *api) data(method, path string, body any, want int, out any) time.Duration {
	c.t.Helper()
	code, raw, d := c.do(method, path, body)
	if code != want {
		c.t.Fatalf("%s %s: %d, want %d: %s", method, path, code, want, raw)
	}
	if out != nil {
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			c.t.Fatal(err)
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			c.t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	return d
}

var setupCodePattern = regexp.MustCompile(`setup_code=([A-Z2-7]{4}(?:-[A-Z2-7]{4})+)`)

// setup creates the admin and returns a client with an admin API key.
func setup(t *testing.T, p *proc) *api {
	t.Helper()
	m := setupCodePattern.FindStringSubmatch(p.logs.String())
	if m == nil {
		t.Fatal("no setup code")
	}
	c := newAPI(t, p.base)
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	c.client.Jar, _ = cookiejar.New(nil)
	c.data("POST", "/api/v1/setup", map[string]string{"setup_code": m[1], "username": "load-admin", "password": "load-password-1"}, http.StatusCreated, &session)
	c.csrf = session.CSRFToken
	var key struct {
		Key string `json:"key"`
	}
	c.data("POST", "/api/v1/api-keys", map[string]string{"name": "load", "scope": "admin"}, http.StatusCreated, &key)
	out := newAPI(t, p.base)
	out.key = key.Key
	return out
}

// latencies summarises durations in milliseconds.
type latencies struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
	Max   float64 `json:"max_ms"`
}

func summarize(ds []time.Duration) latencies {
	if len(ds) == 0 {
		return latencies{}
	}
	ms := make([]float64, len(ds))
	for i, d := range ds {
		ms[i] = float64(d.Microseconds()) / 1000
	}
	sort.Float64s(ms)
	q := func(p float64) float64 { return ms[min(len(ms)-1, int(p*float64(len(ms))))] }
	return latencies{Count: len(ms), P50: q(0.50), P95: q(0.95), P99: q(0.99), Max: ms[len(ms)-1]}
}

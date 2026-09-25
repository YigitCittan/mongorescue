package mcp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
)

// Keys accepted by the test HTTP server.
const (
	readKey     = "mr_read_key"
	operatorKey = "mr_operator_key"
)

// httpServer serves f's MCP handler behind a stand-in for the auth middleware of
// internal/server: bearer keys map to principals, anything else gets no principal.
func (f *fixture) httpServer(t *testing.T) *httptest.Server {
	t.Helper()
	keys := map[string]*auth.Principal{readKey: principal(auth.ScopeRead), operatorKey: principal(auth.ScopeOperator)}
	h := f.srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := keys[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]; p != nil {
			r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		} else if r.Header.Get("Authorization") != "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// bearer adds an Authorization header to every request.
type bearer struct {
	key  string
	base http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return b.base.RoundTrip(r)
}

func TestStreamableHTTP(t *testing.T) {
	f := newFixture(t, nil)
	ts := f.httpServer(t)

	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("request without a key = %d; want 401 with WWW-Authenticate", resp.StatusCode)
	}

	client := &http.Client{Transport: bearer{key: operatorKey, base: http.DefaultTransport}}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "http-client", Version: "v0"}, nil).Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: client}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	if names := toolNames(t, cs); !slices.Contains(names, ToolStartBackup) {
		t.Fatalf("operator over HTTP sees %v", names)
	}
	var started backupStarted
	structured(t, ToolStartBackup, call(t, cs, ToolStartBackup, map[string]any{"connection_id": testConnID, "database": "shop"}), &started)
	if b := awaitBackup(t, cs, started.Backup.ID); b.Status != "completed" {
		t.Fatalf("backup over HTTP = %+v", b)
	}
	entries, _ := f.audit.List(context.Background(), 1)
	if len(entries) != 1 || entries[0].Transport != audit.TransportHTTP || entries[0].APIKeyID != "key_operator" {
		t.Fatalf("HTTP calls must be audited with the key and transport: %+v", entries)
	}

	// GET (standalone SSE) is not offered by the stateless endpoint.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+readKey)
	req.Header.Set("Accept", "text/event-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d; want 405", resp.StatusCode)
	}
}

// bridge runs RunBridge against ts with key and returns a client session connected
// to its stdio side.
func bridge(t *testing.T, ts *httptest.Server, key string) (*sdk.ClientSession, <-chan error) {
	t.Helper()
	inR, inW := io.Pipe()   // client -> bridge
	outR, outW := io.Pipe() // bridge -> client
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunBridge(ctx, BridgeConfig{URL: ts.URL, APIKey: key, Version: "test", In: inR, Out: outW})
		_ = outW.Close()
	}()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "assistant", Version: "v0"}, nil).Connect(context.Background(),
		&sdk.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect to the bridge: %v (bridge: %v)", err, <-done)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		_ = inW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("bridge did not stop")
		}
	})
	return cs, done
}

func TestBridgeForwardsToolsResourcesAndPrompts(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.RateLimit = RateLimit{PerMinute: 1, Burst: 4} })
	ts := f.httpServer(t)
	cs, _ := bridge(t, ts, readKey)

	init := cs.InitializeResult()
	if init.ServerInfo.Name != ServerName || !strings.Contains(init.Instructions, "safe") && !strings.Contains(init.Instructions, "rescue") {
		t.Fatalf("bridge must mirror the server identity and instructions: %+v", init.ServerInfo)
	}
	if names := toolNames(t, cs); len(names) != 11 || slices.Contains(names, ToolStartBackup) {
		t.Fatalf("a read key through the bridge sees %v; want the 11 read tools", names)
	}
	var status struct {
		Health string `json:"health"`
	}
	structured(t, ToolGetStatus, call(t, cs, ToolGetStatus, nil), &status)
	if status.Health != "healthy" {
		t.Fatalf("get_status via bridge = %+v", status)
	}
	res, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: "mongorescue://jobs/" + testJobID})
	if err != nil || !strings.Contains(res.Contents[0].Text, testJobID) {
		t.Fatalf("resource via bridge = %+v, %v", res, err)
	}
	pr, err := cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: PromptVerifyRecentBackups})
	if err != nil || len(pr.Messages) != 1 {
		t.Fatalf("prompt via bridge = %+v, %v", pr, err)
	}
	// A tool the key may not call is not even offered by the bridge.
	if refused, callErr := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolStartBackup,
		Arguments: map[string]any{"connection_id": testConnID, "database": "shop"}}); callErr == nil && !refused.IsError {
		t.Fatalf("read key started a backup through the bridge: %+v", refused)
	}

	entries, _ := f.audit.List(context.Background(), 10)
	if len(entries) == 0 || entries[0].Transport != audit.TransportStdio {
		t.Fatalf("bridge calls must be audited as stdio: %+v", entries)
	}
	// JSON-RPC errors (here the rate limit) keep their code through the bridge.
	var wire *jsonrpc.Error
	for range 5 {
		if _, err = cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolGetStatus, Arguments: map[string]any{}}); err != nil {
			break
		}
	}
	if !errors.As(err, &wire) || wire.Code != CodeRateLimited {
		t.Fatalf("rate limit through the bridge = %v; want code %d", err, CodeRateLimited)
	}
}

func TestBridgeRefusedKey(t *testing.T) {
	f := newFixture(t, nil)
	ts := f.httpServer(t)
	err := RunBridge(context.Background(), BridgeConfig{URL: ts.URL, APIKey: "mr_wrong", In: strings.NewReader(""), Out: io.Discard})
	if !errors.Is(err, ErrBridgeAuth) || strings.Contains(err.Error(), "mr_wrong") {
		t.Fatalf("wrong key = %v; want ErrBridgeAuth without the key", err)
	}
	if err := RunBridge(context.Background(), BridgeConfig{URL: ts.URL}); !errors.Is(err, ErrBridgeConfig) {
		t.Fatalf("missing key = %v; want ErrBridgeConfig", err)
	}
	if err := RunBridge(context.Background(), BridgeConfig{URL: "http://127.0.0.1:1", APIKey: "k", In: strings.NewReader(""), Out: io.Discard}); !errors.Is(err, ErrBridgeConnect) {
		t.Fatalf("unreachable server = %v; want ErrBridgeConnect", err)
	}
}

// TestBridgeDoesNotFollowRedirects proves a 3xx cannot send the API key to another
// host: the bridge refuses to follow it and the other host sees no request.
func TestBridgeDoesNotFollowRedirects(t *testing.T) {
	const key = "mr_redirect_test_key"
	var leaked, hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(r.Header.Get("Authorization"), key) {
			leaked.Add(1)
		}
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	t.Cleanup(other.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	err := RunBridge(context.Background(), BridgeConfig{URL: redirector.URL, APIKey: key, In: strings.NewReader(""), Out: io.Discard})
	if !errors.Is(err, ErrBridgeConnect) || !strings.Contains(err.Error(), "redirect") || strings.Contains(err.Error(), key) {
		t.Fatalf("redirected bridge = %v; want ErrBridgeConnect naming the redirect", err)
	}
	if hits.Load() != 0 || leaked.Load() != 0 {
		t.Fatalf("the redirect target got %d request(s), %d with the API key; want none", hits.Load(), leaked.Load())
	}
}

// TestCredentialTransportOnlyAuthenticatesTheEndpoint proves the key is only added
// to requests for the configured scheme and host.
func TestCredentialTransportOnlyAuthenticatesTheEndpoint(t *testing.T) {
	var got []string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = append(got, r.URL.String()+" "+r.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	tr := &credentialTransport{base: base, key: "k", scheme: "https", host: "backup.example:8443"}
	for _, u := range []string{"https://backup.example:8443/mcp", "https://evil.example/mcp", "http://backup.example:8443/mcp", "https://backup.example/mcp"} {
		req, _ := http.NewRequest(http.MethodPost, u, nil)
		req.Header.Set("Authorization", "Bearer smuggled")
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	want := []string{
		"https://backup.example:8443/mcp Bearer k",
		"https://evil.example/mcp ",
		"http://backup.example:8443/mcp ",
		"https://backup.example/mcp ",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("requests = %q; want %q", got, want)
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper.
func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBridgeEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"":                            "http://127.0.0.1:8080/mcp",
		"http://localhost:8080":       "http://localhost:8080/mcp",
		"https://rescue.example.com/": "https://rescue.example.com/mcp",
		"https://example.com/mr/mcp":  "https://example.com/mr/mcp",
	} {
		got, err := BridgeConfig{URL: in}.Endpoint()
		if err != nil || got != want {
			t.Errorf("Endpoint(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"ftp://x", "localhost:8080", "http://user:pw@host", "http://host/?q=1"} {
		if _, err := (BridgeConfig{URL: bad}).Endpoint(); !errors.Is(err, ErrBridgeConfig) {
			t.Errorf("Endpoint(%q) must fail", bad)
		}
	}
}

func TestLimiter(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	l := newLimiter(RateLimit{PerMinute: 60, Burst: 2}, func() time.Time { return now })
	for i := range 2 {
		if ok, _ := l.allow("k"); !ok {
			t.Fatalf("call %d within the burst was refused", i)
		}
	}
	ok, wait := l.allow("k")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("over the burst: ok=%v wait=%v; want a refusal with a wait of about 1s", ok, wait)
	}
	if ok, _ := l.allow("other"); !ok {
		t.Fatal("keys have separate budgets")
	}
	now = now.Add(time.Second)
	if ok, _ := l.allow("k"); !ok {
		t.Fatal("a token must be available after a second at 60/min")
	}
	if d := newLimiter(RateLimit{}, time.Now); d.burst != DefaultRateLimit.Burst {
		t.Fatalf("zero RateLimit must use the default, got burst %d", d.burst)
	}
}

package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

const testKey = "mr_abc_secretsecret"

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL + "/prefix/", APIKey: testKey, UserAgent: "mongorescue-cli/test", Transport: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestClientSendsBearerKeyAndIdentity(t *testing.T) {
	var got http.Header
	var path, query string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got, path, query = r.Header.Clone(), r.URL.Path, r.URL.RawQuery
		_, _ = io.WriteString(w, `{"success":true,"data":[{"id":"bkp_1","database":"shop","status":"completed"}],"meta":{"total":7,"limit":1,"offset":0}}`)
	})
	res, err := c.ListBackups(context.Background(), url.Values{"status": {"completed"}, "limit": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer "+testKey || got.Get("User-Agent") != "mongorescue-cli/test" || got.Get(TransportHeader) != "cli" {
		t.Fatalf("headers = %v", got)
	}
	if path != "/prefix/api/v1/backups" || query != "limit=1&status=completed" {
		t.Fatalf("request = %s?%s", path, query)
	}
	if len(res.Value) != 1 || res.Value[0].ID != "bkp_1" || res.Meta == nil || res.Meta.Total != 7 || !strings.Contains(string(res.Raw), `"bkp_1"`) {
		t.Fatalf("result = %+v", res)
	}
}

func TestClientNeverFollowsRedirects(t *testing.T) {
	var foreign atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		foreign.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the key was sent to another origin")
		}
	}))
	defer other.Close()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/api/v1/health", http.StatusTemporaryRedirect)
	})
	_, err := c.Health(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTemporaryRedirect || !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "not followed") {
		t.Fatalf("err = %v; want the redirect explained", err)
	}
	if foreign.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestBearerTransportOnlyAuthenticatesTheOrigin(t *testing.T) {
	var got []string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = append(got, r.URL.String()+" "+r.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusTeapot, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	u, _ := url.Parse("https://backup.example:8443")
	tr := NewBearerTransport(base, u, "k", "ua", "cli")
	for _, target := range []string{"https://backup.example:8443/api/v1/stats", "https://evil.example/api", "http://backup.example:8443/api", "https://backup.example/api", "https://BACKUP.example:8443/x"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer smuggled")
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	want := []string{
		"https://backup.example:8443/api/v1/stats Bearer k",
		"https://evil.example/api ",
		"http://backup.example:8443/api ",
		"https://backup.example/api ",
		"https://BACKUP.example:8443/x Bearer k",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("requests = %q; want %q", got, want)
	}
	if tr.LastStatus() != http.StatusTeapot {
		t.Fatalf("LastStatus = %d", tr.LastStatus())
	}
}

func TestClientDecodesErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   error
		text   string
	}{
		{http.StatusUnauthorized, `{"success":false,"error":"authentication required"}`, ErrUnauthorized, "401 authentication required"},
		{http.StatusForbidden, `{"success":false,"error":"api key scope read does not allow this request (needs operator)"}`, ErrForbidden, "needs operator"},
		{http.StatusNotFound, `{"success":false,"error":"backup not found"}`, ErrNotFound, "backup not found"},
		{http.StatusConflict, `{"success":false,"error":"already running"}`, ErrConflict, "already running"},
		{http.StatusBadRequest, `{"success":false,"error":"invalid namespace"}`, ErrBadRequest, "invalid namespace"},
		{http.StatusTooManyRequests, `{"success":false,"error":"slow down"}`, ErrRateLimited, "slow down"},
		{http.StatusBadGateway, `<html>bad gateway</html>`, ErrServer, "502 Bad Gateway"},
		{http.StatusInternalServerError, `{"success":false,"error":"dial mongodb://admin:hunter2@db:27017 failed"}`, ErrServer, "mongodb://admin:"},
		{http.StatusOK, `<html>not the api</html>`, ErrUnexpectedResponse, "not a MongoRescue API response"},
	} {
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		})
		_, err := c.Stats(context.Background())
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
			t.Errorf("%d: err = %v; want %v mentioning %q", tc.status, err, tc.want, tc.text)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%d: the error leaks a password: %v", tc.status, err)
		}
	}
}

func TestClientUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()
	c, err := New(Config{URL: addr, APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Health(context.Background())
	if !errors.Is(err, ErrUnreachable) || strings.Contains(err.Error(), testKey) {
		t.Fatalf("err = %v", err)
	}
}

func TestPreflightInConflict(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"success":false,"error":"preflight failed: disk_space","data":{"ok":false,"checks":[{"id":"disk_space","status":"fail","message":"too small"}]}}`)
	})
	_, err := c.StartRestore(context.Background(), models.RestoreRequest{BackupID: "bkp_1"})
	apiErr, ok := AsAPIError(err)
	if !ok || !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	p, ok := apiErr.Preflight()
	if !ok || p.OK || len(p.Checks) != 1 || p.Checks[0].ID != "disk_space" {
		t.Fatalf("preflight = %+v, %v", p, ok)
	}
}

func TestRunJobAndGetByID(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prefix/api/v1/jobs/single/run":
			_, _ = io.WriteString(w, `{"success":true,"data":{"id":"bkp_1","database":"shop","status":"in_progress"}}`)
		case "/prefix/api/v1/jobs/multi/run":
			_, _ = io.WriteString(w, `{"success":true,"data":{"id":"run_1","job_id":"multi","status":"running","databases":[]}}`)
		case "/prefix/api/v1/backups":
			if r.URL.Query().Get("id") == "bkp_1" {
				_, _ = io.WriteString(w, `{"success":true,"data":[{"id":"bkp_1","database":"shop","status":"completed","extra":1}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"success":true,"data":[]}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	})
	ctx := context.Background()
	single, err := c.RunJob(ctx, "single")
	if err != nil || single.Value.Backup == nil || single.Value.Backup.ID != "bkp_1" || single.Value.Run != nil {
		t.Fatalf("single = %+v, %v", single, err)
	}
	multi, err := c.RunJob(ctx, "multi")
	if err != nil || multi.Value.Run == nil || multi.Value.Run.ID != "run_1" || multi.Value.Backup != nil {
		t.Fatalf("multi = %+v, %v", multi, err)
	}
	got, err := c.GetBackup(ctx, "bkp_1")
	if err != nil || got.Value.Status != models.StatusCompleted || !strings.Contains(string(got.Raw), `"extra":1`) {
		t.Fatalf("GetBackup = %+v, %v", got, err)
	}
	if _, err := c.GetBackup(ctx, "bkp_404"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing backup = %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		url, key, want string
	}{
		{"ftp://host", testKey, "http(s)"},
		{"http://user:pass@host", testKey, "credentials"},
		{"http://host?x=1", testKey, "query"},
		{"http://host#frag", testKey, "fragment"},
		{"localhost:8080", testKey, "http(s)"},
		{"http://host", "", "no API key"},
		{"http://host", "a b", "spaces"},
	} {
		_, err := New(Config{URL: tc.url, APIKey: tc.key})
		if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "pass@") {
			t.Errorf("New(%q) = %v; want %q", tc.url, err, tc.want)
		}
	}
	u, err := ParseURL("")
	if err != nil || u.String() != DefaultURL {
		t.Fatalf("default = %v, %v", u, err)
	}
	if u, err = ParseURL(" https://backup.example/mr/ "); err != nil || u.String() != "https://backup.example/mr" {
		t.Fatalf("prefix = %v, %v", u, err)
	}
	for host, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "backup.example": false, "10.0.0.1": false} {
		if IsLoopback(host) != want {
			t.Errorf("IsLoopback(%q) = %v", host, !want)
		}
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper.
func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

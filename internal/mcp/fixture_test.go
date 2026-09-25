package mcp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// Fixture data.
const (
	testConnID = "conn_test"
	// testPassword must never appear in any MCP output or audit entry.
	testPassword = "s3cret-pw-do-not-leak"
	testConnURI  = "mongodb://admin:" + testPassword + "@db.internal:27017/?authSource=admin"
	testJobID    = "job_shop"
)

// fakeProber answers discovery calls without a network.
type fakeProber struct{}

func (fakeProber) Ping(context.Context, string) (connections.ServerInfo, error) {
	return connections.ServerInfo{Version: "7.0.14"}, nil
}

func (fakeProber) ListDatabases(context.Context, string) ([]connections.Database, error) {
	return []connections.Database{{Name: "shop", SizeBytes: 4096}, {Name: "admin"}}, nil
}

func (fakeProber) ListCollections(context.Context, string, string) ([]connections.Collection, error) {
	return []connections.Collection{{Name: "orders", Type: "collection"}}, nil
}

// fixture is an MCP server over real services, a SQLite store and fake tool runners.
type fixture struct {
	srv   *Server
	store *store.SQLiteStore
	audit *audit.Service

	mu       sync.Mutex
	observed map[string]int
}

func newFixture(t *testing.T, mutate func(*Config)) *fixture {
	t.Helper()
	st := storetest.New(t)
	mock := storage.NewMockStorage()
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	rRunner := func(_ context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		_, _ = io.Copy(io.Discard, stdin)
		return strings.NewReader(""), func() error { return nil }, nil
	}
	bEngine := backup.NewEngine(mock, "", backup.WithRunner(bRunner))
	rEngine := restore.NewEngine(mock, "", restore.WithRunner(rRunner))
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })

	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.SaveConnection(ctx, &models.Connection{ID: testConnID, Name: "test server", URI: testConnURI, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveJob(ctx, &models.Job{ID: testJobID, Name: "nightly shop", Database: "shop", ConnectionID: testConnID, CronExpression: "@daily", Enabled: true, Gzip: true}); err != nil {
		t.Fatal(err)
	}
	conns := connections.NewService(st, fakeProber{})
	sched := scheduler.NewScheduler(st, bEngine, mock, nil, scheduler.WithConnectionResolver(conns))
	ops := operations.New(operations.Config{
		Store: st, Backup: bEngine, Restore: rEngine, Jobs: sched, Runs: manager, Connections: conns, Version: "test",
	})
	f := &fixture{store: st, audit: audit.NewService(st, nil), observed: map[string]int{}}
	cfg := Config{
		Operations: ops, Connections: conns, Audit: f.audit, Version: "test",
		ObserveCall: func(tool, result string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.observed[tool+"/"+result]++
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	f.srv = New(cfg)
	return f
}

// principal returns an API key principal with scope.
func principal(scope auth.Scope) *auth.Principal {
	return &auth.Principal{Method: auth.MethodAPIKey, APIKeyID: "key_" + string(scope), APIKeyName: string(scope) + " key", Scope: scope}
}

// session connects an in-memory client acting as p.
func (f *fixture) session(t *testing.T, p *auth.Principal) *sdk.ClientSession {
	t.Helper()
	clientT, serverT := sdk.NewInMemoryTransports()
	ss, err := f.srv.Connect(auth.WithPrincipal(context.Background(), p), serverT)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "v0"}, nil).Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Wait()
	})
	return cs
}

// call invokes tool and fails the test on protocol errors.
func call(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error %v", tool, err)
	}
	return res
}

// structured decodes the structured content of res into v, failing on tool errors.
func structured(t *testing.T, tool string, res *sdk.CallToolResult, v any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("%s: tool error %s", tool, resultText(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: decode %s: %v", tool, raw, err)
	}
}

// awaitBackup polls get_backup until the backup leaves in_progress.
func awaitBackup(t *testing.T, cs *sdk.ClientSession, id string) models.BackupRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var b models.BackupRecord
		structured(t, ToolGetBackup, call(t, cs, ToolGetBackup, map[string]any{"id": id}), &b)
		if b.Status != models.StatusInProgress || time.Now().After(deadline) {
			return b
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// awaitRestore polls get_restore until the restore leaves in_progress.
func awaitRestore(t *testing.T, cs *sdk.ClientSession, id string) models.RestoreRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var r models.RestoreRecord
		structured(t, ToolGetRestore, call(t, cs, ToolGetRestore, map[string]any{"id": id}), &r)
		if r.Status != models.RestoreStatusInProgress || time.Now().After(deadline) {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertNoSecret fails when v's JSON contains the connection password.
func assertNoSecret(t *testing.T, what string, v any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), testPassword) {
		t.Fatalf("%s leaks the connection password: %s", what, raw)
	}
}

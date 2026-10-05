package mcp

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestPITRToolsFollowConnectionAccess checks pitr_status and pitr_restore for keys
// limited to connection A: B's stream is neither listed nor found, also for an
// admin-scope principal limited to A (which the auth rules never create; defence in
// depth behind the admin-only tool).
func TestPITRToolsFollowConnectionAccess(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	now := time.Now().UTC()
	for _, c := range []string{"conn_a", "conn_b"} {
		if err := st.SaveConnection(ctx, &models.Connection{ID: c, Name: c, URI: "mongodb://" + c + ":27017/?replicaSet=rs0", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateStream(ctx, &pitr.Stream{ID: "pst_" + c, ConnectionID: c, ReplicaSet: "rs0", ChunkSeconds: 60}); err != nil {
			t.Fatal(err)
		}
	}
	mock := storage.NewMockStorage()
	_, recipient, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.NewX25519Encryptor([]string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	col := collector.New(collector.Config{
		Repo:      st,
		Open:      func(context.Context, *pitr.Stream) (collector.Session, error) { return nil, io.ErrUnexpectedEOF },
		Storage:   func(context.Context, string) (storage.Storage, error) { return mock, nil },
		Encryptor: func() *encryption.Encryptor { return enc },
		Logger:    slog.New(slog.DiscardHandler),
	})
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	rEngine := restore.NewEngine(mock, "")
	conns := connections.NewService(st, fakeProber{})
	ops := operations.New(operations.Config{Store: st, Backup: backup.NewEngine(mock, ""), Restore: rEngine, Runs: manager,
		Connections: conns, PITR: st, PITRRestore: rEngine, PITRBases: st.ListBaseBackups})
	f := &fixture{store: st, audit: audit.NewService(st, nil), observed: map[string]int{}}
	f.srv = New(Config{Operations: ops, Connections: conns, PITR: col, Audit: f.audit, Version: "test"})

	reader := principal(auth.ScopeRead)
	reader.Connections = auth.OnlyConnections("conn_a")
	cs := f.session(t, reader)
	out := call(t, cs, ToolPITRStatus, nil)
	if text := resultText(out); out.IsError || !strings.Contains(text, "pst_conn_a") || strings.Contains(text, "pst_conn_b") {
		t.Fatalf("pitr_status of a reader limited to A: %s", text)
	}
	for _, id := range []string{"pst_conn_b", "conn_b"} {
		if out = call(t, cs, ToolPITRStatus, map[string]any{"stream_id": id}); !out.IsError || !strings.Contains(resultText(out), "not found") {
			t.Errorf("pitr_status %s: %s; want not found", id, resultText(out))
		}
	}
	admin := principal(auth.ScopeAdmin)
	admin.Connections = auth.OnlyConnections("conn_a")
	cs = f.session(t, admin)
	for _, id := range []string{"pst_conn_b", "conn_b"} {
		out = call(t, cs, ToolPITRRestore, map[string]any{"stream_id": id, "at": "2026-10-05T12:00:00Z"})
		if !out.IsError || !strings.Contains(resultText(out), "not found") {
			t.Errorf("pitr_restore of %s: %s; want not found", id, resultText(out))
		}
	}
}

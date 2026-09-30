package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// syncBuffer is a goroutine-safe log sink.
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

// leakyProber fails every discovery call with a driver-style error that repeats the
// connection string, credentials included.
type leakyProber struct{}

func (leakyProber) Ping(_ context.Context, uri string) (connections.ServerInfo, error) {
	return connections.ServerInfo{}, errors.New("server selection error: " + uri + " (password " + testPassword + ")")
}

func (leakyProber) ListDatabases(_ context.Context, uri string) ([]connections.Database, error) {
	return nil, errors.New("connection() error occurred during connection handshake: auth error for " + uri)
}

func (leakyProber) ListCollections(_ context.Context, uri, _ string) ([]connections.Collection, error) {
	return nil, errors.New("dial tcp: lookup failed for " + uri + " user=admin pass=" + testPassword)
}

// TestToolErrorsAreRedacted makes discovery fail with errors that carry the
// connection password and checks that neither the tool results, nor the audit log,
// nor the server's log output contain it.
func TestToolErrorsAreRedacted(t *testing.T) {
	logs := &syncBuffer{}
	st := storetest.New(t)
	now := time.Now().UTC()
	if err := st.SaveConnection(context.Background(), &models.Connection{ID: testConnID, Name: "leaky", URI: testConnURI, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(c *Config) {
		c.Connections = connections.NewService(st, leakyProber{})
		c.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	for _, scope := range []auth.Scope{auth.ScopeRead, auth.ScopeOperator} {
		cs := f.session(t, principal(scope))
		for tool, args := range map[string]map[string]any{
			ToolListDatabases:   {"connection_id": testConnID},
			ToolListCollections: {"connection_id": testConnID, "database": "shop"},
			ToolListConnections: {},
			ToolListBackups:     {"database": testConnURI},
			ToolStartBackup:     {"connection_id": testConnID, "database": testConnURI},
		} {
			res := call(t, cs, tool, args)
			raw, _ := json.Marshal(res)
			if strings.Contains(string(raw), testPassword) {
				t.Errorf("%s (%s key) leaks the password: %s", tool, scope, raw)
			}
		}
	}
	entries, err := f.audit.List(context.Background(), 100)
	if err != nil || len(entries) == 0 {
		t.Fatalf("audit = %d entries, %v", len(entries), err)
	}
	for _, e := range entries {
		assertNoSecret(t, "audit entry "+e.Tool, e)
	}
	if strings.Contains(logs.String(), testPassword) {
		t.Fatalf("server logs leak the password:\n%s", logs.String())
	}
}

// TestActionToolsRejectUnknownArguments checks that every tool refuses arguments its
// schema does not declare, so a model cannot smuggle REST-only options (in-place
// restores, drop, custom storage keys) into a call.
func TestActionToolsRejectUnknownArguments(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeAdmin))
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(tool.InputSchema)
		if !strings.Contains(string(raw), `"additionalProperties":false`) {
			t.Errorf("tool %s accepts undeclared arguments: %s", tool.Name, raw)
		}
	}
	for tool, args := range map[string]map[string]any{
		ToolStartBackup:      {"connection_id": testConnID, "database": "shop", "target_key": "shop/victim.archive"},
		ToolRestoreSafeClone: {"backup_id": "bkp_x", "confirm_in_place": true},
		ToolRunJob:           {"job_id": testJobID, "drop": true},
	} {
		if res := call(t, cs, tool, args); !res.IsError {
			t.Errorf("%s accepted %v", tool, args)
		}
	}
	if list, _ := f.store.ListBackupRecords(context.Background(), ""); len(list) != 0 {
		t.Fatalf("a rejected call started a backup: %+v", list)
	}
}

// TestSafeCloneToolNeverTouchesExistingData runs the restore tool and checks the
// record: a fresh _rescue_ database, never dropped, even for an admin key.
func TestSafeCloneToolNeverTouchesExistingData(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeAdmin))
	var started backupStarted
	structured(t, ToolStartBackup, call(t, cs, ToolStartBackup, map[string]any{"connection_id": testConnID, "database": "shop"}), &started)
	done := awaitBackup(t, cs, started.Backup.ID)
	var restored restoreStarted
	structured(t, ToolRestoreSafeClone, call(t, cs, ToolRestoreSafeClone, map[string]any{"backup_id": done.ID}), &restored)
	r := awaitRestore(t, cs, restored.Restore.ID)
	if r.TargetDatabase == "shop" || !strings.HasPrefix(r.TargetDatabase, "shop_rescue_") {
		t.Fatalf("restore target %q; want a new shop_rescue_ database", r.TargetDatabase)
	}
}

// TestHostileNamespacesAreRefusedByTools checks that tools pass database and
// collection names through the same validation as the REST API.
func TestHostileNamespacesAreRefusedByTools(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeOperator))
	for _, args := range []map[string]any{
		{"connection_id": testConnID, "database": "shop\n--drop"},
		{"connection_id": testConnID, "database": "../../etc"},
		{"connection_id": testConnID, "database": "shop", "collections": []string{"a\x00b"}},
	} {
		res := call(t, cs, ToolStartBackup, args)
		if !res.IsError || !strings.Contains(resultText(res), "invalid namespace") {
			t.Errorf("start_backup %q = %s; want an invalid namespace error", args, resultText(res))
		}
	}
	if list, _ := f.store.ListBackupRecords(context.Background(), ""); len(list) != 0 {
		t.Fatalf("refused backups were recorded: %+v", list)
	}
}

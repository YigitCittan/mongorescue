package operations_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// withConnections rebuilds env's operations service on a real connections service
// and creates connection "prod" with cmds.
func (e *adminEnv) withConnections(t *testing.T, cmds []models.PostRestoreCommand) (*connections.Service, string) {
	t.Helper()
	conns := connections.NewService(e.st, nil)
	cfg := e.cfg
	cfg.Connections = conns
	e.cfg = cfg
	e.svc = rebuildWithUsers(t, e.protEnv, e.auth)
	e.auth.SetAdminGrantGate(e.svc)
	c, err := conns.Create(e.ctxOf(t, "alice"), connections.Input{Name: "prod", URI: "mongodb://db.internal:27017", PostRestoreCommands: &cmds})
	if err != nil {
		t.Fatal(err)
	}
	return conns, c.ID
}

func commandsOf(t *testing.T, conns *connections.Service, id string) []models.PostRestoreCommand {
	t.Helper()
	c, err := conns.Resolve(auth.WithPrincipal(context.Background(), auth.SystemPrincipal()), id)
	if err != nil {
		t.Fatal(err)
	}
	return c.PostRestoreCommands
}

func TestRemovingPostRestoreCommandsNeedsASecondAdministrator(t *testing.T) {
	env := newAdminEnv(t)
	two := []models.PostRestoreCommand{
		{Database: "*", Command: json.RawMessage(`{"delete":"users","deletes":[{"q":{"_id":7},"limit":0}]}`)},
		{Database: "shop", Command: json.RawMessage(`{"drop":"sessions"}`)},
	}
	conns, id := env.withConnections(t, two)
	alice := auditlog.WithAnnotations(env.ctxOf(t, "alice"))

	// Adding a command needs no approval; the audit log gets the counts.
	three := append(append([]models.PostRestoreCommand{}, two...), models.PostRestoreCommand{Database: "crm", Command: json.RawMessage(`{"drop":"leads"}`)})
	if _, err := env.svc.UpdateConnection(alice, id, connections.Input{Name: "prod", URI: "mongodb://db.internal:27017", PostRestoreCommands: &three}); err != nil {
		t.Fatalf("adding a command: %v", err)
	}
	if a := auditlog.Annotations(alice); a["post_restore_commands_changed"] != "true" || a["post_restore_commands_from"] != "2" || a["post_restore_commands_to"] != "3" {
		t.Fatalf("audit annotations = %v", a)
	}
	for k, v := range auditlog.Annotations(alice) {
		if strings.Contains(v, "users") || strings.Contains(v, "leads") {
			t.Fatalf("annotation %s holds command contents: %q", k, v)
		}
	}

	// Removing one is held for a second administrator; the rest of the update applies.
	alice = auditlog.WithAnnotations(env.ctxOf(t, "alice"))
	_, err := env.svc.UpdateConnection(alice, id, connections.Input{Name: "prod-renamed", URI: "mongodb://db.internal:27017", PostRestoreCommands: &two})
	req := pendingApproval(t, err)
	if req.Action != models.ApprovalPostRestoreCommands || req.Subject != id || !strings.Contains(req.Summary, "from 3 to 2") || req.Secret != "" {
		t.Fatalf("approval request = %+v", req)
	}
	if got := commandsOf(t, conns, id); len(got) != 3 {
		t.Fatalf("commands before the approval = %d; want 3", len(got))
	}
	if c, _ := conns.Resolve(alice, id); c.Name != "prod-renamed" {
		t.Fatalf("the other fields must apply at once: %+v", c)
	}
	// The held list is sealed in the store.
	if raw := rawApprovalSecret(t, env, req.ID); strings.Contains(raw, "sessions") || raw == "" {
		t.Fatalf("stored approval secret = %q; want it sealed", raw)
	}

	// The requester cannot approve; another administrator can.
	if _, err = env.svc.Approve(env.ctxOf(t, "alice"), req.ID); err == nil {
		t.Fatal("the requester approved her own request")
	}
	if _, err = env.svc.Approve(env.ctxOf(t, "bob"), req.ID); err != nil {
		t.Fatalf("approval: %v", err)
	}
	if got := commandsOf(t, conns, id); len(got) != 2 || got[1].Database != "shop" {
		t.Fatalf("commands after the approval = %+v", got)
	}
}

func rawApprovalSecret(t *testing.T, env *adminEnv, id string) string {
	t.Helper()
	db, err := sql.Open("sqlite", env.st.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var secret string
	if err := db.QueryRow("SELECT secret FROM approvals WHERE id = ?", id).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	return secret
}

// recordingDropper records dropped databases.
type recordingDropper struct {
	mu      sync.Mutex
	dropped []string
	err     error
}

func (d *recordingDropper) DropDatabase(_ context.Context, _, db string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropped = append(d.dropped, db)
	return d.err
}

func TestDropKeptClones(t *testing.T) {
	env := newPreflightEnv(t, healthyInspector(), nil)
	dropper := &recordingDropper{}
	svc := operations.New(operations.Config{Store: env.st, Backup: backup.NewEngine(storage.NewMockStorage(), ""), Restore: env.engine, Runs: env.runs,
		Connections: fakeConnections{"conn_a": {ID: "conn_a", Name: "prod", URI: "mongodb://u:secret@db.internal:27017"}}, Dropper: dropper})
	ctx := context.Background()
	failed := &models.RestoreRecord{ID: "rst_1", BackupID: env.backup.ID, SourceDatabase: "shop", TargetDatabase: "shop_rescue_20261006_101500",
		TargetConnectionID: "conn_a", Status: models.RestoreStatusFailed,
		PostRestore: &models.PostRestoreReport{Status: models.PostRestoreFailed, ClonesKept: []string{"shop_rescue_20261006_101500"}}}
	if err := env.st.SaveRestoreRecord(ctx, failed); err != nil {
		t.Fatal(err)
	}
	kept, err := svc.KeptClones(admin())
	if err != nil || len(kept) != 1 || kept[0].ID != "rst_1" {
		t.Fatalf("KeptClones = %+v, %v", kept, err)
	}
	if _, err = svc.DropKeptClones(operator(), "rst_1"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("operator: %v; want ErrForbidden", err)
	}
	rec, err := svc.DropKeptClones(admin(), "rst_1")
	if err != nil || rec.PostRestore.ClonesDroppedAt == nil || len(dropper.dropped) != 1 || dropper.dropped[0] != "shop_rescue_20261006_101500" {
		t.Fatalf("drop = %+v, %v, dropped %v", rec, err, dropper.dropped)
	}
	if kept, _ = svc.KeptClones(admin()); len(kept) != 0 {
		t.Fatalf("still listed after the drop: %+v", kept)
	}
	if _, err = svc.DropKeptClones(admin(), "rst_1"); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("second drop: %v; want ErrInvalid", err)
	}

	// Only recorded clones of the restore are ever dropped: never its source.
	for i, names := range [][]string{{"shop"}, {"other_rescue_20261006_101500"}, {"shop_backup"}} {
		bad := *failed
		bad.ID = "rst_bad_" + string(rune('a'+i))
		bad.PostRestore = &models.PostRestoreReport{Status: models.PostRestoreFailed, ClonesKept: names}
		if err = env.st.SaveRestoreRecord(ctx, &bad); err != nil {
			t.Fatal(err)
		}
		dropper.dropped = nil
		if _, err = svc.DropKeptClones(admin(), bad.ID); !errors.Is(err, operations.ErrInvalid) || len(dropper.dropped) != 0 {
			t.Fatalf("kept %v: %v, dropped %v; want a refusal", names, err, dropper.dropped)
		}
	}
	if _, err = svc.DropKeptClones(admin(), "rst_missing"); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("missing restore: %v", err)
	}
}

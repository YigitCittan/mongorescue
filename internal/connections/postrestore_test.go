package connections_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// checkingProber also checks post-restore commands, like mongoconn.Prober.
type checkingProber struct {
	prober
	checked int
}

func (p *checkingProber) CheckPostRestoreCommand(command json.RawMessage) error {
	p.checked++
	if strings.Contains(string(command), "bad-oid") {
		return errors.New("not valid extended JSON")
	}
	return nil
}

func TestPostRestoreCommandsAreStoredAndAdminOnly(t *testing.T) {
	svc, _, _ := newService(t)
	admin := auth.WithPrincipal(context.Background(), auth.SystemPrincipal())
	viewer := auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodSystem, Scope: auth.ScopeRead})
	cmds := []models.PostRestoreCommand{
		{Database: "*", Command: json.RawMessage(`{"delete":"users","deletes":[{"q":{"_id":42},"limit":0}]}`)},
	}
	c, err := svc.Create(admin, connections.Input{Name: "prod", URI: secretURI, PostRestoreCommands: &cmds})
	if err != nil {
		t.Fatal(err)
	}
	full, err := svc.Resolve(admin, c.ID)
	if err != nil || len(full.PostRestoreCommands) != 1 || string(full.PostRestoreCommands[0].Command) != string(cmds[0].Command) {
		t.Fatalf("stored commands = %+v, %v", full, err)
	}
	if got, _ := svc.Get(admin, c.ID); len(got.PostRestoreCommands) != 1 {
		t.Fatalf("an administrator must see the commands: %+v", got)
	}
	// Other callers never see them: they may name the people whose data was erased.
	got, _ := svc.Get(viewer, c.ID)
	list, _ := svc.List(viewer)
	if got.PostRestoreCommands != nil || len(list) != 1 || list[0].PostRestoreCommands != nil {
		t.Fatalf("a viewer sees the commands: %+v / %+v", got, list)
	}
	if got, _ := svc.Get(context.Background(), c.ID); got.PostRestoreCommands != nil {
		t.Fatal("a caller without a principal sees the commands")
	}

	// Omitted keeps them, an empty list removes them.
	if _, err = svc.Update(admin, c.ID, connections.Input{Name: "prod2", URI: redactedOf(admin, t, svc, c.ID)}); err != nil {
		t.Fatal(err)
	}
	if full, _ = svc.Resolve(admin, c.ID); len(full.PostRestoreCommands) != 1 {
		t.Fatalf("an update without the field dropped the commands")
	}
	empty := []models.PostRestoreCommand{}
	if _, err = svc.Update(admin, c.ID, connections.Input{Name: "prod2", URI: redactedOf(admin, t, svc, c.ID), PostRestoreCommands: &empty}); err != nil {
		t.Fatal(err)
	}
	if full, _ = svc.Resolve(admin, c.ID); full.PostRestoreCommands != nil {
		t.Fatalf("an empty list kept the commands: %+v", full.PostRestoreCommands)
	}
}

func redactedOf(ctx context.Context, t *testing.T, svc *connections.Service, id string) string {
	t.Helper()
	c, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return c.URI
}

func TestPostRestoreCommandsAreValidated(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	for _, bad := range [][]models.PostRestoreCommand{
		{{Database: "admin", Command: json.RawMessage(`{"drop": "x"}`)}},
		{{Database: "*", Command: json.RawMessage(`{"applyOps": []}`)}},
		{{Database: "*", Command: json.RawMessage(`{"aggregate": "x", "pipeline": [{"$out": "y"}]}`)}},
		{{Database: "*", Command: json.RawMessage(`not json`)}},
	} {
		_, err := svc.Create(ctx, connections.Input{Name: "prod", URI: secretURI, PostRestoreCommands: &bad})
		if !errors.Is(err, connections.ErrInvalid) || !strings.Contains(err.Error(), "post_restore_commands") {
			t.Errorf("Create with %s: %v; want ErrInvalid naming post_restore_commands", bad[0].Command, err)
		}
	}

	p := &checkingProber{}
	svc = connections.NewService(storetest.New(t), p)
	bad := []models.PostRestoreCommand{{Database: "*", Command: json.RawMessage(`{"delete": "u", "deletes": [{"q": {"_id": {"$oid": "bad-oid"}}, "limit": 0}]}`)}}
	if _, err := svc.Create(ctx, connections.Input{Name: "prod", URI: secretURI, PostRestoreCommands: &bad}); !errors.Is(err, connections.ErrInvalid) || p.checked != 1 {
		t.Fatalf("extended JSON check: %v (checked %d)", err, p.checked)
	}
}

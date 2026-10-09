package operations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// limitedTo is a context with a principal of scope limited to connections.
func limitedTo(scope auth.Scope, connections ...string) context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodAPIKey, APIKeyID: "key_limited", Scope: scope,
		Connections: auth.OnlyConnections(connections...)})
}

// TestPITRRestoresFollowConnectionAccess checks that point-in-time restores, their
// preflight and chain tests treat another connection's stream as unknown (404),
// before the admin check, by stream or connection ID; and that even an admin-scope
// principal limited to other connections (which the auth rules never create) is
// refused, as defence in depth.
func TestPITRRestoresFollowConnectionAccess(t *testing.T) {
	svc, _ := pitrService(t, nil)
	byConnection := pitrAt(125)
	byConnection.PITR.StreamID = "conn_a"
	for _, ctx := range []context.Context{limitedTo(auth.ScopeOperator, "conn_b"), limitedTo(auth.ScopeRead, "conn_b"),
		limitedTo(auth.ScopeAdmin, "conn_b"), limitedTo(auth.ScopeAdmin)} {
		if _, err := svc.StartRestore(ctx, pitrAt(125)); !errors.Is(err, operations.ErrNotFound) {
			t.Errorf("restore of another connection's stream: %v; want ErrNotFound", err)
		}
		if _, err := svc.StartRestore(ctx, byConnection); !errors.Is(err, operations.ErrNotFound) {
			t.Errorf("restore by another connection's ID: %v; want ErrNotFound", err)
		}
		if _, err := svc.PreflightRestore(ctx, pitrAt(125)); !errors.Is(err, operations.ErrNotFound) {
			t.Errorf("preflight of another connection's stream: %v; want ErrNotFound", err)
		}
		if _, err := svc.StartChainTest(ctx, "str_a"); !errors.Is(err, operations.ErrNotFound) {
			t.Errorf("chain test of another connection's stream: %v; want ErrNotFound", err)
		}
	}
	// The stream's own connection: the usual scope rules. A reader may not restore,
	// an operator may (safe clones), a chain test needs admin.
	if _, err := svc.StartRestore(limitedTo(auth.ScopeRead, "conn_a"), pitrAt(125)); !errors.Is(err, auth.ErrForbidden) {
		t.Errorf("reader on the stream's connection: %v; want ErrForbidden", err)
	}
	if _, err := svc.StartChainTest(limitedTo(auth.ScopeOperator, "conn_a"), "str_a"); !errors.Is(err, auth.ErrForbidden) {
		t.Errorf("operator chain test on the stream's connection: %v; want ErrForbidden", err)
	}
	if rec, err := svc.StartRestore(limitedTo(auth.ScopeOperator, "conn_a"), pitrAt(125)); err != nil || rec.PITR == nil {
		t.Errorf("operator on the stream's connection: %+v, %v; want a point-in-time restore", rec, err)
	}
	// A restore into a connection outside the caller's access is not found either,
	// for the restore and for its preflight.
	into := pitrAt(125)
	into.TargetConnectionID = "conn_b"
	for _, scope := range []auth.Scope{auth.ScopeOperator, auth.ScopeAdmin} {
		if _, err := svc.PreflightRestore(limitedTo(scope, "conn_a"), into); !errors.Is(err, operations.ErrNotFound) {
			t.Errorf("%s preflight into another connection: %v; want ErrNotFound", scope, err)
		}
		if _, err := svc.StartRestore(limitedTo(scope, "conn_a"), into); !errors.Is(err, operations.ErrNotFound) {
			t.Errorf("%s restore into another connection: %v; want ErrNotFound", scope, err)
		}
	}
}

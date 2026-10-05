package operations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// fakeRotator counts rotations.
type fakeRotator struct {
	env   bool
	calls int
}

func (f *fakeRotator) FromEnv() bool { return f.env }

func (f *fakeRotator) Rotate(context.Context) (*keyrotation.Result, error) {
	f.calls++
	return &keyrotation.Result{OldFingerprint: "old-fp", NewFingerprint: "new-fp"}, nil
}

func rotEnv(t *testing.T, r *fakeRotator) *protEnv {
	t.Helper()
	env := newProtEnv(t)
	env.cfg.KeyRotator = r
	env.svc = operations.New(env.cfg)
	operations.SetNow(env.svc, env.clock.Now)
	return env
}

func keyRotatedEvents(env *protEnv) []events.Event {
	env.pub.mu.Lock()
	defer env.pub.mu.Unlock()
	var out []events.Event
	for _, e := range env.pub.events {
		if e.Type == events.SecurityKeyRotated {
			out = append(out, e)
		}
	}
	return out
}

func TestRotateSecretKeyNeedsAnAdminSession(t *testing.T) {
	r := &fakeRotator{}
	env := rotEnv(t, r)
	if _, err := env.svc.RotateSecretKey(asUser("bob", auth.ScopeOperator)); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("operator = %v; want ErrForbidden", err)
	}
	if _, err := env.svc.RotateSecretKey(keyOf("alice")); !errors.Is(err, auth.ErrSessionRequired) {
		t.Fatalf("API key = %v; want ErrSessionRequired", err)
	}
	res, err := env.svc.RotateSecretKey(asUser("alice", auth.ScopeAdmin))
	if err != nil || r.calls != 1 || res.NewFingerprint != "new-fp" {
		t.Fatalf("rotate = %+v, %v (calls %d)", res, err, r.calls)
	}
	ev := keyRotatedEvents(env)
	if len(ev) != 1 || ev[0].Action != operations.KeyKindSecretKey || ev[0].Actor != "alice" ||
		ev[0].Detail != "old fingerprint old-fp, new fingerprint new-fp" {
		t.Fatalf("events = %+v", ev)
	}
}

func TestRotateSecretKeyRefusesEnvKeys(t *testing.T) {
	r := &fakeRotator{env: true}
	env := rotEnv(t, r)
	if _, err := env.svc.RotateSecretKey(asUser("alice", auth.ScopeAdmin)); !errors.Is(err, keyrotation.ErrEnvKey) {
		t.Fatalf("rotate = %v; want ErrEnvKey", err)
	}
	if r.calls != 0 {
		t.Fatal("an env key was rotated")
	}
}

func TestRotateSecretKeyWaitsForASecondAdmin(t *testing.T) {
	r := &fakeRotator{}
	env := rotEnv(t, r)
	env.enableTwoPerson(t)
	_, err := env.svc.RotateSecretKey(asUser("alice", auth.ScopeAdmin))
	a := pendingApproval(t, err)
	if r.calls != 0 {
		t.Fatal("rotated before the approval")
	}
	if _, err = env.svc.Approve(asUser("bob", auth.ScopeAdmin), a.ID); err != nil {
		t.Fatalf("approve = %v", err)
	}
	if r.calls != 1 {
		t.Fatalf("rotations after the approval = %d; want 1", r.calls)
	}
	ev := keyRotatedEvents(env)
	if len(ev) != 1 || ev[0].ApprovalID != a.ID {
		t.Fatalf("events = %+v; want one naming the approval", ev)
	}
}

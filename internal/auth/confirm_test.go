package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// TestConfirmPassword checks re-authentication: the right password of a signed-in
// admin passes; wrong passwords, API keys, the system and anonymous callers are
// refused, and repeated wrong passwords are throttled.
func TestConfirmPassword(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor := f.session(t, f.setup(t).Token)
	if err := f.svc.ConfirmPassword(ctx, actor, adminPassword); err != nil {
		t.Fatalf("right password: %v", err)
	}
	if err := f.svc.ConfirmPassword(ctx, actor, "wrong password"); !errors.Is(err, auth.ErrCurrentPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if err := f.svc.ConfirmPassword(ctx, actor, ""); !errors.Is(err, auth.ErrCurrentPassword) {
		t.Fatalf("empty password: %v", err)
	}
	_, plain, err := f.svc.CreateAPIKey(ctx, actor, "ci", auth.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	key, err := f.svc.AuthenticateAPIKey(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.svc.ConfirmPassword(ctx, key, adminPassword); !errors.Is(err, auth.ErrSessionRequired) {
		t.Fatalf("API key: %v", err)
	}
	if err = f.svc.ConfirmPassword(ctx, auth.SystemPrincipal(), adminPassword); !errors.Is(err, auth.ErrSessionRequired) {
		t.Fatalf("system: %v", err)
	}
	if err = f.svc.ConfirmPassword(ctx, nil, adminPassword); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("anonymous: %v", err)
	}
	var throttled *auth.ThrottledError
	for range 20 {
		if err = f.svc.ConfirmPassword(ctx, actor, "wrong password"); errors.As(err, &throttled) {
			break
		}
	}
	if throttled == nil {
		t.Fatal("repeated wrong passwords must be throttled")
	}
}

package operations_test

import (
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// haRotator is a rotator of an instance in active-passive HA mode.
type haRotator struct{ fakeRotator }

func (*haRotator) HAMode() bool { return true }

// TestRotateSecretKeyRefusedInHAMode checks that secret.key is never rotated by one
// instance of an HA cluster, before any approval is asked for.
func TestRotateSecretKeyRefusedInHAMode(t *testing.T) {
	r := &haRotator{}
	env := newProtEnv(t)
	env.cfg.KeyRotator = r
	env.svc = operations.New(env.cfg)
	if _, err := env.svc.RotateSecretKey(asUser("alice", auth.ScopeAdmin)); !errors.Is(err, keyrotation.ErrHAMode) {
		t.Fatalf("rotate in HA mode = %v; want ErrHAMode", err)
	}
	if r.calls != 0 {
		t.Fatal("the key was rotated in HA mode")
	}
}

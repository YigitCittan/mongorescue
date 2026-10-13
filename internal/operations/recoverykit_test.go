package operations_test

import (
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// TestPlaintextRecoveryKitNeedsASecondAdmin walks the approval of a plaintext
// recovery kit: allowed at once without the two-person rule; with it, a request
// first, usable once by its requester only, within the grant window.
func TestPlaintextRecoveryKitNeedsASecondAdmin(t *testing.T) {
	env := newProtEnv(t)
	operations.SetNow(env.svc, env.clock.Now)
	alice, bob := asUser("alice", auth.ScopeAdmin), asUser("bob", auth.ScopeAdmin)
	if err := env.svc.AuthorizePlaintextRecoveryKit(alice, ""); err != nil {
		t.Fatalf("without the two-person rule = %v", err)
	}
	if err := env.svc.AuthorizePlaintextRecoveryKit(asUser("olga", auth.ScopeOperator), ""); err == nil {
		t.Fatal("an operator may download the plaintext key")
	}
	env.enableTwoPerson(t)

	a := pendingApproval(t, env.svc.AuthorizePlaintextRecoveryKit(alice, ""))
	if a.Action != models.ApprovalPlaintextRecoveryKit || a.Subject != "usr_alice" {
		t.Fatalf("request = %+v", a)
	}
	if err := env.svc.AuthorizePlaintextRecoveryKit(alice, a.ID); !errors.Is(err, operations.ErrPlaintextKitApproval) {
		t.Fatalf("use while pending = %v; want ErrPlaintextKitApproval", err)
	}
	approved, err := env.svc.Approve(bob, a.ID)
	if err != nil || approved.Status != models.ApprovalApproved {
		t.Fatalf("approve = %+v, %v", approved, err)
	}
	if err = env.svc.AuthorizePlaintextRecoveryKit(bob, a.ID); !errors.Is(err, operations.ErrPlaintextKitApproval) {
		t.Fatalf("use by the approver = %v; want ErrPlaintextKitApproval", err)
	}
	if err = env.svc.AuthorizePlaintextRecoveryKit(alice, a.ID); err != nil {
		t.Fatalf("use by the requester = %v", err)
	}
	if err = env.svc.AuthorizePlaintextRecoveryKit(alice, a.ID); !errors.Is(err, operations.ErrPlaintextKitApproval) {
		t.Fatalf("second use = %v; want ErrPlaintextKitApproval", err)
	}
	if err = env.svc.AuthorizePlaintextRecoveryKit(alice, "apr_missing"); !errors.Is(err, operations.ErrPlaintextKitApproval) {
		t.Fatalf("unknown approval = %v; want ErrPlaintextKitApproval", err)
	}

	// A grant not used within the window expires.
	late := pendingApproval(t, env.svc.AuthorizePlaintextRecoveryKit(alice, ""))
	if _, err = env.svc.Approve(bob, late.ID); err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(operations.PlaintextKitGrantTTL + time.Minute)
	if err = env.svc.AuthorizePlaintextRecoveryKit(alice, late.ID); !errors.Is(err, operations.ErrPlaintextKitApproval) {
		t.Fatalf("use after the window = %v; want ErrPlaintextKitApproval", err)
	}
}

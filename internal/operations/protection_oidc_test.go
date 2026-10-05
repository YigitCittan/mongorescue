package operations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

const testIssuer = "https://idp.example.com/realms/ops"

// oidcEnv is a protection environment where alice is a local administrator and bob
// an administrator through the identity provider group backup-admins; the
// two-person rule is on.
type oidcEnv struct {
	*protEnv
	auth  *auth.Service
	alice *auth.User
}

func newOIDCEnv(t *testing.T) *oidcEnv {
	t.Helper()
	ctx := context.Background()
	pe := newProtEnv(t)
	policy := func() auth.OIDCPolicy {
		o := pe.settings.Current().OIDC
		p := auth.OIDCPolicy{Enabled: o.Enabled, LocalLogin: auth.LocalLogin(o.LocalLogin), DefaultRole: auth.Role(o.DefaultRole),
			AllowedEmailDomains: o.AllowedEmailDomains, AutoCreateUsers: o.AutoCreateUsers}
		for _, m := range o.RoleMappings {
			p.RoleMappings = append(p.RoleMappings, auth.RoleMapping{Group: m.Group, Role: auth.Role(m.Role)})
		}
		return p
	}
	svc, err := auth.NewService(pe.st, auth.WithBcryptCost(bcrypt.MinCost), auth.WithClock(pe.clock.Now), auth.WithOIDCPolicy(policy))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Init(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Setup(ctx, "192.0.2.1", svc.SetupCode(), "alice", adminTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	env := &oidcEnv{protEnv: pe, auth: svc, alice: res.User}
	on, issuer, client, redirect := true, testIssuer, "mongorescue", "https://backup.example.com/auth/oidc/callback"
	mappings := []settings.OIDCRoleMapping{{Group: "backup-admins", Role: "admin"}, {Group: "backup-viewers", Role: "viewer"}}
	if _, _, err = pe.settings.UpdateChanged(ctx, settings.Patch{OIDC: &settings.OIDCPatch{Enabled: &on, Issuer: &issuer, ClientID: &client,
		RedirectURL: &redirect, RoleMappings: &mappings, AutoCreateUsers: &on}}); err != nil {
		t.Fatal(err)
	}
	if login := env.signInBob(t, "backup-admins"); login.User.Role != auth.RoleAdmin {
		t.Fatalf("bob = %s; want admin", login.User.Role)
	}
	pe.svc = rebuildWithUsers(t, pe, svc)
	svc.SetAdminGrantGate(pe.svc)
	pe.clock.Advance(time.Minute)
	on2 := true
	aliceCtx := auth.WithPrincipal(ctx, env.aliceSession(t))
	if _, err = pe.svc.UpdateSettings(aliceCtx, settings.Patch{Security: &settings.SecurityPatch{RequireSecondApprover: &on2}}); err != nil {
		t.Fatalf("turn the rule on with alice and bob = %v", err)
	}
	pe.clock.Advance(time.Minute)
	return env
}

// signInBob signs bob in through single sign-on as a member of groups.
func (e *oidcEnv) signInBob(t *testing.T, groups ...string) *auth.OIDCLogin {
	t.Helper()
	login, err := e.auth.LoginOIDC(context.Background(), &auth.ExternalIdentity{Issuer: testIssuer, Subject: "bob-sub", Username: "bob", Groups: groups})
	if err != nil {
		t.Fatal(err)
	}
	return login
}

func (e *oidcEnv) aliceSession(t *testing.T) *auth.Principal {
	t.Helper()
	res, err := e.auth.Login(context.Background(), "192.0.2.1", "alice", adminTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.auth.AuthenticateSession(context.Background(), res.Token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *oidcEnv) role(t *testing.T, username string) auth.Role {
	t.Helper()
	u, err := e.st.GetUserByUsername(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	return u.Role
}

// openApprovals returns the pending requests of action.
func (e *oidcEnv) openApprovals(t *testing.T, action models.ApprovalAction) []*models.Approval {
	t.Helper()
	list, err := e.svc.ListApprovals(asUser("carol", auth.ScopeAdmin), models.ApprovalPending)
	if err != nil {
		t.Fatal(err)
	}
	var out []*models.Approval
	for _, a := range list {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

// TestACompromisedAdminCannotDemoteAnSSOAdminToTurnTheRuleOff walks the attack: a
// compromised local administrator (alice) maps the group of the other
// administrator (bob, single sign-on) to viewer, bob signs in, and alice, now
// seemingly alone, turns the two-person rule off. The mapping change waits for an
// approval, a sign-in under such a mapping keeps bob's admin role and asks for an
// approval instead, and the rule never switches off.
func TestACompromisedAdminCannotDemoteAnSSOAdminToTurnTheRuleOff(t *testing.T) {
	env := newOIDCEnv(t)
	ctx := context.Background()
	alice := auth.WithPrincipal(ctx, env.aliceSession(t))

	lowered := []settings.OIDCRoleMapping{{Group: "backup-admins", Role: "viewer"}}
	res, err := env.svc.UpdateSettings(alice, settings.Patch{OIDC: &settings.OIDCPatch{RoleMappings: &lowered}})
	if err != nil || len(res.Approvals) != 1 || res.Approvals[0].Action != models.ApprovalOIDCAdminMapping {
		t.Fatalf("lowering bob's mapping = %+v, %v; want an approval request", res, err)
	}
	if got := env.settings.Current().OIDC.RoleMappings; len(got) != 2 || got[0].Role != "admin" {
		t.Fatalf("mappings = %+v; want the admin mapping kept", got)
	}
	removed := []settings.OIDCRoleMapping{{Group: "backup-viewers", Role: "viewer"}}
	if res, err = env.svc.UpdateSettings(alice, settings.Patch{OIDC: &settings.OIDCPatch{RoleMappings: &removed}}); err != nil || len(res.Approvals) != 1 {
		t.Fatalf("removing bob's mapping = %+v, %v; want an approval request", res, err)
	}
	if _, _, err = env.settings.UpdateChanged(ctx, settings.Patch{OIDC: &settings.OIDCPatch{RoleMappings: &lowered}}); err == nil {
		t.Fatal("the settings service lowered an admin mapping directly while the rule is on")
	}

	// Even if the mapping changed anyway (here: around the operations service), a
	// sign-in does not demote bob.
	if _, _, err = env.settings.UpdateChanged(settings.WithLoweredProtection(ctx), settings.Patch{OIDC: &settings.OIDCPatch{RoleMappings: &lowered}}); err != nil {
		t.Fatal(err)
	}
	login := env.signInBob(t, "backup-admins")
	if !login.RoleKept || !login.DemotionHeld || login.User.Role != auth.RoleAdmin || env.role(t, "bob") != auth.RoleAdmin {
		t.Fatalf("sign-in under the viewer mapping = %+v; want the admin role kept", login)
	}
	env.signInBob(t, "backup-admins")
	held := env.openApprovals(t, models.ApprovalSSODemoteAdmin)
	if len(held) != 1 || held[0].Subject != login.User.ID || held[0].Role != string(auth.RoleViewer) {
		t.Fatalf("demotion requests = %+v; want one for bob to viewer", held)
	}

	off := false
	res, err = env.svc.UpdateSettings(alice, settings.Patch{Security: &settings.SecurityPatch{RequireSecondApprover: &off}})
	if err != nil || len(res.Pending) != 0 || len(res.Approvals) != 1 {
		t.Fatalf("turning the rule off = %+v, %v; want an approval request", res, err)
	}
	env.clock.Advance(models.GraceDuration(models.MaxDeleteGraceDays) + time.Hour)
	env.svc.ApplyDueChanges(ctx)
	if !env.settings.Current().Security.RequireSecondApprover {
		t.Fatal("the two-person rule switched off")
	}
}

// TestAnApprovedSSODemotionApplies proves a second administrator can apply the role
// a sign-in held back.
func TestAnApprovedSSODemotionApplies(t *testing.T) {
	env := newOIDCEnv(t)
	ctx := context.Background()
	lowered := []settings.OIDCRoleMapping{{Group: "backup-admins", Role: "operator"}}
	if _, _, err := env.settings.UpdateChanged(settings.WithLoweredProtection(ctx), settings.Patch{OIDC: &settings.OIDCPatch{RoleMappings: &lowered}}); err != nil {
		t.Fatal(err)
	}
	env.signInBob(t, "backup-admins")
	held := env.openApprovals(t, models.ApprovalSSODemoteAdmin)
	if len(held) != 1 {
		t.Fatalf("demotion requests = %+v", held)
	}
	if _, err := env.svc.Approve(auth.WithPrincipal(ctx, env.aliceSession(t)), held[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := env.role(t, "bob"); got != auth.RoleOperator {
		t.Fatalf("bob = %s after the approval; want operator", got)
	}
}

// TestAFailedAdminGrantRequestLeavesNothingBehind proves that when the approval
// request of an admin grant cannot be stored (here: an imported API key without a
// creator, ErrRequesterUnknown), neither the viewer account nor the operator key
// created for it stays.
func TestAFailedAdminGrantRequestLeavesNothingBehind(t *testing.T) {
	env := newAdminEnv(t)
	ctx := context.Background()
	const imported = "imported-legacy-key-0123456789"
	if _, err := env.auth.ImportAPIKey(ctx, imported); err != nil {
		t.Fatal(err)
	}
	p, err := env.auth.AuthenticateAPIKey(ctx, imported)
	if err != nil || p.User != nil {
		t.Fatalf("imported key = %+v, %v; want a key without a creator", p, err)
	}
	keyCtx := auth.WithPrincipal(ctx, p)
	keysBefore, err := env.st.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}

	u, err := env.auth.CreateUser(keyCtx, p, "mallory", adminTestPassword, auth.RoleAdmin)
	if !errors.Is(err, operations.ErrRequesterUnknown) || u != nil {
		t.Fatalf("admin user from a key without a creator = %+v, %v; want ErrRequesterUnknown", u, err)
	}
	if _, err = env.st.GetUserByUsername(ctx, "mallory"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("mallory after the failed request: %v; want ErrUserNotFound", err)
	}

	k, plain, err := env.auth.CreateAPIKey(keyCtx, p, "sneaky", auth.ScopeAdmin)
	if !errors.Is(err, operations.ErrRequesterUnknown) || k != nil || plain != "" {
		t.Fatalf("admin key from a key without a creator = %+v, %v; want ErrRequesterUnknown", k, err)
	}
	if keysAfter, _ := env.st.ListAPIKeys(ctx); len(keysAfter) != len(keysBefore) {
		t.Fatalf("API keys after the failed request: %d; want %d", len(keysAfter), len(keysBefore))
	}
}

// TestThePendingRuleOffIsDroppedWhenAnAdminIsLost proves the pending change that
// turns the rule off without approvers is dropped when an administrator recorded
// with it loses the admin role without an approval while it waits.
func TestThePendingRuleOffIsDroppedWhenAnAdminIsLost(t *testing.T) {
	env := newAdminEnv(t)
	ctx := context.Background()
	carol, err := env.auth.CreateUser(auth.WithPrincipal(ctx, auth.SystemPrincipal()), auth.SystemPrincipal(), "carol", adminTestPassword, auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	// A data edit leaves alice the only administrator, and the rule off is pending.
	if _, err = env.st.UpdateUserRole(ctx, "", env.bob.ID, auth.RoleViewer, env.clock.Now(), false); err != nil {
		t.Fatal(err)
	}
	off := false
	res, err := env.svc.UpdateSettings(env.ctxOf(t, "alice"), settings.Patch{Security: &settings.SecurityPatch{RequireSecondApprover: &off}})
	if err != nil || len(res.Pending) != 1 || len(res.Pending[0].Admins) != 1 || res.Pending[0].Admins[0] != env.alice.ID {
		t.Fatalf("disable without approvers = %+v, %v; want a pending change recording alice", res, err)
	}
	// While it waits, a data edit makes carol an administrator and alice a viewer.
	if _, err = env.st.UpdateUserRole(ctx, "", carol.ID, auth.RoleAdmin, env.clock.Now(), false); err != nil {
		t.Fatal(err)
	}
	if _, err = env.st.UpdateUserRole(ctx, "", env.alice.ID, auth.RoleViewer, env.clock.Now(), false); err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(models.GraceDuration(models.DefaultDeleteGraceDays))
	env.svc.ApplyDueChanges(ctx)
	if !env.settings.Current().Security.RequireSecondApprover {
		t.Fatal("the rule went off although an administrator was lost without an approval")
	}
	if list, _ := env.svc.PendingChanges(asUser("carol", auth.ScopeAdmin)); len(list) != 0 {
		t.Fatalf("pending changes = %+v; want the change dropped", list)
	}
}

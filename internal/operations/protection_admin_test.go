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

const adminTestPassword = "correct horse battery staple"

// adminEnv is a protection environment with a real auth service: alice and bob are
// administrators of long standing; the two-person rule is on.
type adminEnv struct {
	*protEnv
	auth       *auth.Service
	alice, bob *auth.User
	aliceKey   string
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	pe := newProtEnv(t)
	svc, err := auth.NewService(pe.st, auth.WithBcryptCost(bcrypt.MinCost), auth.WithClock(pe.clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	env := &adminEnv{protEnv: pe, auth: svc}
	res, err := svc.Setup(context.Background(), "192.0.2.1", svc.SetupCode(), "alice", adminTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	env.alice = res.User
	if env.bob, err = svc.CreateUser(context.Background(), auth.SystemPrincipal(), "bob", adminTestPassword, auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	_, env.aliceKey, err = svc.CreateAPIKey(context.Background(), env.session(t, "alice"), "alice admin", auth.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	pe.svc = rebuildWithUsers(t, pe, svc)
	svc.SetAdminGrantGate(pe.svc)
	pe.admins = 2
	pe.clock.Advance(time.Minute)
	env.enableTwoPerson(t)
	pe.clock.Advance(time.Minute)
	return env
}

// rebuildWithUsers is the protection service of pe with users from a.
func rebuildWithUsers(t *testing.T, pe *protEnv, a *auth.Service) *operations.Service {
	t.Helper()
	cfg := pe.cfg
	cfg.Users = a
	cfg.SecondApproverCheck = a.CheckSecondApproverPossible
	svc := operations.New(cfg)
	operations.SetNow(svc, pe.clock.Now)
	return svc
}

// session signs name in and returns the principal of the session.
func (e *adminEnv) session(t *testing.T, name string) *auth.Principal {
	t.Helper()
	res, err := e.auth.Login(context.Background(), "192.0.2.1", name, adminTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.auth.AuthenticateSession(context.Background(), res.Token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *adminEnv) ctxOf(t *testing.T, name string) context.Context {
	t.Helper()
	return auth.WithPrincipal(context.Background(), e.session(t, name))
}

// keyCtx is a context with alice's admin API key.
func (e *adminEnv) keyCtx(t *testing.T) context.Context {
	t.Helper()
	p, err := e.auth.AuthenticateAPIKey(context.Background(), e.aliceKey)
	if err != nil {
		t.Fatal(err)
	}
	return auth.WithPrincipal(context.Background(), p)
}

// TestAStolenKeyCannotMakeItsOwnApprover walks the bypass: alice's admin key
// creates a second administrator and that administrator tries to approve the key's
// request. The new user is a viewer until a long-standing administrator approves,
// and even then, an administrator newer than a request can never approve it.
func TestAStolenKeyCannotMakeItsOwnApprover(t *testing.T) {
	env := newAdminEnv(t)
	env.backup(t, "b1", "", time.Hour)
	key := env.keyCtx(t)
	del := pendingApproval(t, func() error { _, err := env.svc.DeleteBackup(key, "b1", ""); return err }())

	mallory, err := env.auth.CreateUser(key, auth.PrincipalFrom(key), "mallory", adminTestPassword, auth.RoleAdmin)
	grant := pendingApproval(t, err)
	if mallory == nil || mallory.Role != auth.RoleViewer || grant.Action != models.ApprovalGrantAdminRole {
		t.Fatalf("admin user with the rule on = %+v / %+v; want a viewer and an approval request", mallory, grant)
	}
	if _, err = env.svc.Approve(env.ctxOf(t, "mallory"), del.ID); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("approval by the viewer = %v; want ErrForbidden", err)
	}
	if _, err = env.svc.Approve(env.ctxOf(t, "mallory"), grant.ID); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("self-promotion = %v; want ErrForbidden", err)
	}

	// An approver promoted after the request is refused, also once promoted.
	env.clock.Advance(time.Minute)
	if done, aErr := env.svc.Approve(env.ctxOf(t, "bob"), grant.ID); aErr != nil || done.Status != models.ApprovalApproved {
		t.Fatalf("bob approves mallory's promotion = %+v, %v", done, aErr)
	}
	if _, err = env.svc.Approve(env.ctxOf(t, "mallory"), del.ID); !errors.Is(err, auth.ErrApproverTooRecent) {
		t.Fatalf("approval by an administrator newer than the request = %v; want ErrApproverTooRecent", err)
	}
	if env.status(t, "b1") != models.StatusCompleted {
		t.Fatal("the backup was deleted")
	}
}

// TestPromotionsDemotionsAndDeletionsOfAdminsWait checks that promoting, demoting and
// deleting administrators and admin keys wait for a second administrator.
func TestPromotionsDemotionsAndDeletionsOfAdminsWait(t *testing.T) {
	env := newAdminEnv(t)
	aliceCtx := env.ctxOf(t, "alice")
	carol, err := env.auth.CreateUser(aliceCtx, auth.PrincipalFrom(aliceCtx), "carol", adminTestPassword, auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.auth.SetUserRole(aliceCtx, auth.PrincipalFrom(aliceCtx), carol.ID, auth.RoleAdmin)
	promote := pendingApproval(t, err)
	if u, _ := env.st.GetUser(context.Background(), carol.ID); u.Role != auth.RoleOperator {
		t.Fatalf("carol = %s; want operator until approved", u.Role)
	}
	_, err = env.auth.SetUserRole(aliceCtx, auth.PrincipalFrom(aliceCtx), env.bob.ID, auth.RoleViewer)
	demote := pendingApproval(t, err)
	delAdmin := pendingApproval(t, env.auth.DeleteUser(aliceCtx, auth.PrincipalFrom(aliceCtx), env.bob.ID))
	k, plain, err := env.auth.CreateAPIKey(aliceCtx, auth.PrincipalFrom(aliceCtx), "ci", auth.ScopeAdmin)
	keyGrant := pendingApproval(t, err)
	if k == nil || plain == "" || k.Scope != auth.ScopeOperator {
		t.Fatalf("admin key with the rule on = %+v; want an operator key and an approval request", k)
	}
	if u, _ := env.st.GetUser(context.Background(), env.bob.ID); u.Role != auth.RoleAdmin {
		t.Fatal("bob changed without approval")
	}
	for _, a := range []*models.Approval{promote, demote, delAdmin, keyGrant} {
		if a.RequestedByUserID != env.alice.ID {
			t.Fatalf("request %+v", a)
		}
	}
	bob := env.ctxOf(t, "bob")
	if _, err = env.svc.Approve(bob, keyGrant.ID); err != nil {
		t.Fatal(err)
	}
	keys, _ := env.st.ListAPIKeys(context.Background())
	for _, key := range keys {
		if key.ID == k.ID && key.Scope != auth.ScopeAdmin {
			t.Fatalf("approved key scope = %s", key.Scope)
		}
	}
	if _, err = env.svc.Approve(bob, promote.ID); err != nil {
		t.Fatal(err)
	}
	if u, _ := env.st.GetUser(context.Background(), carol.ID); u.Role != auth.RoleAdmin {
		t.Fatalf("carol = %s after approval", u.Role)
	}
	// Carol cannot approve requests made before she became an administrator.
	if _, err = env.svc.Approve(env.ctxOf(t, "carol"), demote.ID); !errors.Is(err, auth.ErrApproverTooRecent) {
		t.Fatalf("carol approving an older request = %v; want ErrApproverTooRecent", err)
	}
}

// TestAPasswordResetCannotRecruitAnApprover is the review's scenario: a stolen admin
// key resets bob's password to sign in as bob and approve. The key cannot reset it
// at all; a reset from a session waits for approval; and once applied, bob counts as
// a fresh administrator who cannot approve earlier requests.
func TestAPasswordResetCannotRecruitAnApprover(t *testing.T) {
	env := newAdminEnv(t)
	env.backup(t, "b1", "", time.Hour)
	key := env.keyCtx(t)
	del := pendingApproval(t, func() error { _, err := env.svc.DeleteBackup(key, "b1", ""); return err }())
	newPassword := "attacker chosen password 42"
	if err := env.auth.ChangePassword(key, auth.PrincipalFrom(key), env.bob.ID, "", newPassword); !errors.Is(err, auth.ErrSessionRequired) {
		t.Fatalf("password reset with an API key = %v; want ErrSessionRequired", err)
	}
	aliceCtx := env.ctxOf(t, "alice")
	reset := pendingApproval(t, env.auth.ChangePassword(aliceCtx, auth.PrincipalFrom(aliceCtx), env.bob.ID, "", newPassword))
	if reset.Secret != "" {
		t.Fatal("the request handed out the password hash")
	}
	if listed, _ := env.svc.GetApproval(aliceCtx, reset.ID); listed.Secret != "" {
		t.Fatal("the stored request shows the password hash")
	}
	if _, err := env.auth.Login(context.Background(), "192.0.2.1", "bob", newPassword); err == nil {
		t.Fatal("bob's password changed before approval")
	}
	// A third, long-standing administrator approves the reset.
	carol, err := env.auth.CreateUser(context.Background(), auth.SystemPrincipal(), "carol", adminTestPassword, auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.st.UpdateUserRole(context.Background(), "", carol.ID, auth.RoleAdmin, env.clock.Now().Add(-time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if done, aErr := env.svc.Approve(env.ctxOf(t, "carol"), reset.ID); aErr != nil || done.Status != models.ApprovalApproved {
		t.Fatalf("approve the reset = %+v, %v", done, aErr)
	}
	res, err := env.auth.Login(context.Background(), "192.0.2.1", "bob", newPassword)
	if err != nil {
		t.Fatalf("bob's new password: %v", err)
	}
	asBob, err := env.auth.AuthenticateSession(context.Background(), res.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.svc.Approve(auth.WithPrincipal(context.Background(), asBob), del.ID); !errors.Is(err, auth.ErrApproverTooRecent) {
		t.Fatalf("bob, reset after the request, approves = %v; want ErrApproverTooRecent", err)
	}
	if env.status(t, "b1") != models.StatusCompleted {
		t.Fatal("the backup was deleted")
	}
}

// TestDisablingTheRuleWithoutApproversWaitsForTheGracePeriod proves that a lockout
// ends: with fewer than two administrators, turning the rule off is a pending change
// that applies after the grace period, without an approval.
func TestDisablingTheRuleWithoutApproversWaitsForTheGracePeriod(t *testing.T) {
	env := newAdminEnv(t)
	// A data edit leaves alice the only administrator.
	if _, err := env.st.UpdateUserRole(context.Background(), "", env.bob.ID, auth.RoleViewer, env.clock.Now(), false); err != nil {
		t.Fatal(err)
	}
	off := false
	res, err := env.svc.UpdateSettings(env.ctxOf(t, "alice"), settings.Patch{Security: &settings.SecurityPatch{RequireSecondApprover: &off}})
	if err != nil || len(res.Approvals) != 0 || len(res.Pending) != 1 || res.Pending[0].Kind != models.PendingDisableSecondApprover {
		t.Fatalf("disable without approvers = %+v, %v; want a pending change", res, err)
	}
	env.clock.Advance(models.GraceDuration(models.DefaultDeleteGraceDays) - time.Minute)
	env.svc.ApplyDueChanges(context.Background())
	if !env.settings.Current().Security.RequireSecondApprover {
		t.Fatal("the rule went off before the grace period")
	}
	env.clock.Advance(time.Minute)
	env.svc.ApplyDueChanges(context.Background())
	if env.settings.Current().Security.RequireSecondApprover {
		t.Fatal("the rule is still on after the grace period")
	}
}

// TestRetentionApprovalIsBoundToTheJob proves an approval to shorten the retention
// of a job does not apply to a job recreated under the same ID.
func TestRetentionApprovalIsBoundToTheJob(t *testing.T) {
	env := newAdminEnv(t)
	aliceCtx := env.ctxOf(t, "alice")
	job := &models.Job{ID: "job_b", Name: "b", Database: "shop", ConnectionID: "conn_ok", CronExpression: "@daily", RetentionDays: 30,
		CreatedAt: env.clock.Now().Add(-time.Hour)}
	if err := env.st.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	one := 1
	res, err := env.svc.UpdateJob(aliceCtx, "job_b", operations.JobUpdate{Name: "b", Database: "shop", ConnectionID: "conn_ok",
		CronExpression: "@daily", RetentionDays: &one})
	if err != nil || res.Approval == nil || res.Approval.SubjectCreatedAt == nil {
		t.Fatalf("update = %+v, %v; want a bound approval request", res, err)
	}
	if err = env.svc.DeleteJob(aliceCtx, "job_b"); err != nil {
		t.Fatal(err)
	}
	job.CreatedAt = env.clock.Now()
	if err = env.st.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	done, err := env.svc.Approve(env.ctxOf(t, "bob"), res.Approval.ID)
	if err != nil || done.Status != models.ApprovalFailed {
		t.Fatalf("approval for a recreated job = %+v, %v; want failed", done, err)
	}
	if list, _ := env.svc.PendingChanges(aliceCtx); len(list) != 0 {
		t.Fatalf("pending = %+v; want nothing for the new job", list)
	}
}

// TestDropTargetRestoresAndMetadataRetentionWait checks that an in-place restore
// dropping the target waits for approval, and that a lower metadata snapshot count
// waits for approval and then the grace period.
func TestDropTargetRestoresAndMetadataRetentionWait(t *testing.T) {
	env := newAdminEnv(t)
	env.backup(t, "b1", "", time.Hour)
	aliceCtx := env.ctxOf(t, "alice")
	_, err := env.svc.StartRestore(aliceCtx, models.RestoreRequest{BackupID: "b1", TargetConnectionID: "conn_ok",
		SafeClone: new(bool), ConfirmInPlace: true, DropTarget: true})
	a := pendingApproval(t, err)
	if a.Action != models.ApprovalRestoreDropTarget || a.Restore == nil || !a.Restore.DropTarget {
		t.Fatalf("request = %+v", a)
	}
	three := 3
	res, err := env.svc.UpdateSettings(aliceCtx, settings.Patch{MetadataBackup: &settings.MetadataBackupPatch{RetentionCount: &three}})
	if err != nil || len(res.Approvals) != 1 || env.settings.Current().MetadataBackup.RetentionCount == 3 {
		t.Fatalf("lower metadata retention = %+v, %v; want an approval request", res, err)
	}
	if _, err = env.svc.Approve(env.ctxOf(t, "bob"), res.Approvals[0].ID); err != nil {
		t.Fatal(err)
	}
	if env.settings.Current().MetadataBackup.RetentionCount == 3 {
		t.Fatal("applied before the grace period")
	}
	env.clock.Advance(models.GraceDuration(models.DefaultDeleteGraceDays))
	env.svc.ApplyDueChanges(context.Background())
	if got := env.settings.Current().MetadataBackup.RetentionCount; got != 3 {
		t.Fatalf("metadata retention = %d after the grace period; want 3", got)
	}
}

// TestOIDCAdminMappingsWait checks that an oidc change that can grant admin waits
// for a second administrator, and that a new client secret is never kept in one.
func TestOIDCAdminMappingsWait(t *testing.T) {
	env := newAdminEnv(t)
	aliceCtx := env.ctxOf(t, "alice")
	on, issuer, client, redirect := true, "https://idp.example.com/realms/ops", "mongorescue", "https://backup.example.com/auth/oidc/callback"
	base := &settings.OIDCPatch{Enabled: &on, Issuer: &issuer, ClientID: &client, RedirectURL: &redirect}
	if _, err := env.svc.UpdateSettings(aliceCtx, settings.Patch{OIDC: base}); err != nil {
		t.Fatalf("oidc without admin mappings = %v", err)
	}
	mappings := []settings.OIDCRoleMapping{{Group: "backup-admins", Role: "admin"}}
	res, err := env.svc.UpdateSettings(aliceCtx, settings.Patch{OIDC: &settings.OIDCPatch{RoleMappings: &mappings}})
	if err != nil || len(res.Approvals) != 1 || len(env.settings.Current().OIDC.RoleMappings) != 0 {
		t.Fatalf("admin mapping = %+v, %v; want an approval request and nothing applied", res, err)
	}
	secret := "s3cret"
	if _, err = env.svc.UpdateSettings(aliceCtx, settings.Patch{OIDC: &settings.OIDCPatch{RoleMappings: &mappings, ClientSecret: &secret}}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("admin mapping with a secret = %v; want ErrInvalid", err)
	}
	if _, err = env.svc.Approve(env.ctxOf(t, "bob"), res.Approvals[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := env.settings.Current().OIDC.RoleMappings; len(got) != 1 || got[0].Role != "admin" {
		t.Fatalf("mappings after approval = %+v", got)
	}
}

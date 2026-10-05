package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// sessionHeaders signs username in and returns the headers of a dashboard request.
func sessionHeaders(t *testing.T, f *scopeFixture, username string) map[string]string {
	t.Helper()
	res, err := f.auth.Login(context.Background(), "192.0.2.1", username, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"Cookie": SessionCookieName + "=" + res.Token, CSRFHeader: res.CSRFToken, "Content-Type": "application/json"}
}

// TestTwoPersonRuleOverHTTP walks the two-person rule through the API: it cannot be
// turned on with one administrator; once on, a delete with an admin key answers 202
// with a request that neither the requester's session nor any API key can approve,
// and a second administrator's session approves it.
func TestTwoPersonRuleOverHTTP(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	setup, err := f.auth.Setup(ctx, "192.0.2.1", f.auth.SetupCode(), "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	alice := sessionHeaders(t, f, "admin")
	// An admin key created by alice before the rule is on.
	p, err := f.auth.AuthenticateSession(ctx, strings.TrimPrefix(alice["Cookie"], SessionCookieName+"="))
	if err != nil {
		t.Fatal(err)
	}
	_, aliceKey, err := f.auth.CreateAPIKey(ctx, p, "alice admin", auth.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	on := []byte(`{"security":{"require_second_approver":true}}`)
	if rec := serve(f.h, "PUT", "/api/v1/settings", on, alice); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "two administrators") {
		t.Fatalf("enable with one admin: %d %s; want 409", rec.Code, rec.Body)
	}
	if _, err = f.auth.CreateUser(ctx, auth.SystemPrincipal(), "bob", testPassword, auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if rec := serve(f.h, "PUT", "/api/v1/settings", on, alice); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"require_second_approver":true`) {
		t.Fatalf("enable with two admins: %d %s", rec.Code, rec.Body)
	}

	// alice's admin key requests a deletion.
	if err = f.store.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bkp_tp", Database: "shop", Status: models.StatusCompleted, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	key := map[string]string{"Authorization": "Bearer " + aliceKey, "Content-Type": "application/json"}
	rec := serve(f.h, "DELETE", "/api/v1/backups/bkp_tp", nil, key)
	var resp struct {
		Data struct {
			ApprovalRequired bool             `json:"approval_required"`
			Approval         *models.Approval `json:"approval"`
		} `json:"data"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusAccepted || !resp.Data.ApprovalRequired ||
		resp.Data.Approval == nil || resp.Data.Approval.RequestedByUserID != setup.User.ID {
		t.Fatalf("delete with the rule on: %d %s; want 202 with an approval request", rec.Code, rec.Body)
	}
	id := resp.Data.Approval.ID
	if b, _ := f.store.GetBackupRecord(ctx, "bkp_tp"); b.Status != models.StatusCompleted {
		t.Fatalf("backup = %s before approval", b.Status)
	}
	if rec = serve(f.h, "GET", "/api/v1/approvals?status=pending", nil, key); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("pending approvals: %d %s", rec.Code, rec.Body)
	}
	if rec = serve(f.h, "GET", "/api/v1/approvals", nil, map[string]string{"Authorization": "Bearer " + f.keys[auth.ScopeRead]}); rec.Code != http.StatusForbidden {
		t.Fatalf("approvals with a read key: %d; want 403", rec.Code)
	}
	approve := "/api/v1/approvals/" + id + "/approve"
	if rec = serve(f.h, "POST", approve, nil, key); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "never approve") {
		t.Fatalf("approve with an API key: %d %s; want 403", rec.Code, rec.Body)
	}
	if rec = serve(f.h, "POST", approve, nil, sessionHeaders(t, f, "admin")); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "requester") {
		t.Fatalf("self-approval: %d %s; want 403", rec.Code, rec.Body)
	}
	if rec = serve(f.h, "POST", approve, nil, sessionHeaders(t, f, "bob")); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"approved"`) {
		t.Fatalf("approval by bob: %d %s", rec.Code, rec.Body)
	}
	if b, _ := f.store.GetBackupRecord(ctx, "bkp_tp"); b.Status != models.StatusDeleted || b.DeleteApprovedBy != "bob" {
		t.Fatalf("backup after approval = %+v", b)
	}

	// The key cannot make a second administrator to approve its own requests: the
	// new user is a viewer until approved, and resetting bob's password needs a
	// session (from one it would wait for approval too).
	rec = serve(f.h, "POST", "/api/v1/users", []byte(`{"username":"mallory","password":"`+testPassword+`","role":"admin"}`), key)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"role":"viewer"`) || !strings.Contains(rec.Body.String(), "grant_admin_role") {
		t.Fatalf("admin user with the rule on: %d %s; want 202, a viewer and an approval request", rec.Code, rec.Body)
	}
	bob, err := f.store.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	reset := []byte(`{"new_password":"another long password 123"}`)
	if rec = serve(f.h, "PUT", "/api/v1/users/"+bob.ID+"/password", reset, key); rec.Code != http.StatusForbidden {
		t.Fatalf("password reset with a key: %d %s; want 403", rec.Code, rec.Body)
	}
	if rec = serve(f.h, "PUT", "/api/v1/users/"+bob.ID+"/password", reset, alice); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "reset_password") {
		t.Fatalf("password reset from a session: %d %s; want 202", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "$2a$") {
		t.Fatalf("the answer shows the new password hash: %s", rec.Body)
	}
	if _, err = f.auth.Login(ctx, "192.0.2.1", "bob", testPassword); err != nil {
		t.Fatalf("bob's password changed before approval: %v", err)
	}
	// An API key without a creator cannot request anything.
	legacy := map[string]string{"Authorization": "Bearer " + f.keys[auth.ScopeAdmin], "Content-Type": "application/json"}
	if err = f.store.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bkp_tp2", Database: "shop", Status: models.StatusCompleted, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if rec = serve(f.h, "DELETE", "/api/v1/backups/bkp_tp2", nil, legacy); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "no creator") {
		t.Fatalf("request with a key without creator: %d %s; want 403", rec.Code, rec.Body)
	}

	// Turning the rule off waits for a second administrator too.
	off := []byte(`{"security":{"require_second_approver":false}}`)
	rec = serve(f.h, "PUT", "/api/v1/settings", off, alice)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"require_second_approver":true`) || !strings.Contains(rec.Body.String(), `"approvals_requested":[`) {
		t.Fatalf("disable: %d %s; want the rule kept and an approval requested", rec.Code, rec.Body)
	}
}

// TestLowerGraceOverHTTP checks that lowering the grace period answers with the
// pending change instead of applying it, lists it, and lets an admin cancel it.
func TestLowerGraceOverHTTP(t *testing.T) {
	f := newScopeFixture(t)
	admin := map[string]string{"Authorization": "Bearer " + f.keys[auth.ScopeAdmin], "Content-Type": "application/json"}
	rec := serve(f.h, "PUT", "/api/v1/settings", []byte(`{"security":{"delete_grace_days":2}}`), admin)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"delete_grace_days":7`) || !strings.Contains(rec.Body.String(), `"kind":"delete_grace_days"`) {
		t.Fatalf("lower grace: %d %s; want 7 kept and a pending change", rec.Code, rec.Body)
	}
	if rec = serve(f.h, "PUT", "/api/v1/settings", []byte(`{"security":{"delete_grace_days":0}}`), admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("grace 0: %d %s; want 400", rec.Code, rec.Body)
	}
	read := map[string]string{"Authorization": "Bearer " + f.keys[auth.ScopeRead]}
	rec = serve(f.h, "GET", "/api/v1/pending-changes", nil, read)
	var list struct {
		Data []models.PendingChange `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK || len(list.Data) != 1 || *list.Data[0].DeleteGraceDays != 2 {
		t.Fatalf("pending changes: %d %s", rec.Code, rec.Body)
	}
	if rec = serve(f.h, "DELETE", "/api/v1/pending-changes/"+list.Data[0].ID, nil, read); rec.Code != http.StatusForbidden {
		t.Fatalf("cancel with a read key: %d; want 403", rec.Code)
	}
	if rec = serve(f.h, "DELETE", "/api/v1/pending-changes/"+list.Data[0].ID, nil, admin); rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	if rec = serve(f.h, "GET", "/api/v1/settings", nil, read); !strings.Contains(rec.Body.String(), `"pending_changes":[]`) {
		t.Fatalf("settings after cancel: %s", rec.Body)
	}
}

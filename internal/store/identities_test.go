package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

var identityT0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func localUser(id, name string, role auth.Role) *auth.User {
	return &auth.User{ID: id, Username: name, Role: role, AuthProvider: auth.ProviderLocal, PasswordHash: "$2a$04$hash",
		CreatedAt: identityT0, UpdatedAt: identityT0}
}

func ssoSignIn(subject, name string, role auth.Role) *auth.ExternalSignIn {
	return &auth.ExternalSignIn{Subject: subject, NewUserID: "usr_" + name, Username: name, Role: role, AutoCreate: true, At: identityT0}
}

func mustCreate(t *testing.T, st *store.SQLiteStore, u *auth.User) {
	t.Helper()
	if err := st.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("create %s: %v", u.Username, err)
	}
}

func sessionFor(t *testing.T, st *store.SQLiteStore, userID, hash string) {
	t.Helper()
	if err := st.CreateSession(context.Background(), &auth.Session{TokenHash: hash, UserID: userID, CSRFToken: "c",
		CreatedAt: identityT0, LastSeenAt: identityT0, ExpiresAt: identityT0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func hasSession(t *testing.T, st *store.SQLiteStore, hash string) bool {
	t.Helper()
	_, err := st.GetSession(context.Background(), hash)
	if err != nil && !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// TestUserIdentityInvariant pins oidc ⇔ subject set ⇔ empty password hash.
func TestUserIdentityInvariant(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	for name, u := range map[string]*auth.User{
		"oidc without a subject":   {AuthProvider: auth.ProviderOIDC},
		"oidc with a password":     {AuthProvider: auth.ProviderOIDC, Subject: "iss#sub", PasswordHash: "$2a$04$x"},
		"local with a subject":     {AuthProvider: auth.ProviderLocal, Subject: "iss#sub", PasswordHash: "$2a$04$x"},
		"local without a password": {AuthProvider: auth.ProviderLocal},
		"an unknown provider":      {AuthProvider: "saml", PasswordHash: "$2a$04$x"},
	} {
		u.ID, u.Username, u.Role, u.CreatedAt, u.UpdatedAt = "usr_bad", "bad", auth.RoleViewer, identityT0, identityT0
		if err := st.CreateUser(ctx, u); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
	// An empty provider is a local user.
	u := localUser("usr_plain", "plain", auth.RoleViewer)
	u.AuthProvider = ""
	mustCreate(t, st, u)
	if got, err := st.GetUser(ctx, "usr_plain"); err != nil || got.AuthProvider != auth.ProviderLocal || got.Subject != "" {
		t.Errorf("plain user = %+v, %v", got, err)
	}
	// An OIDC user can never get a password.
	res, err := st.SignInExternalUser(ctx, ssoSignIn("https://idp#1", "sso", auth.RoleViewer))
	if err != nil {
		t.Fatal(err)
	}
	if err = st.UpdatePassword(ctx, res.User.ID, "$2a$04$new", identityT0, ""); !errors.Is(err, auth.ErrNoPassword) {
		t.Errorf("UpdatePassword of an oidc user = %v; want ErrNoPassword", err)
	}
	if err = st.UpdatePassword(ctx, "usr_plain", "", identityT0, ""); err == nil {
		t.Error("a local user's password hash was emptied")
	}
}

func TestSignInExternalUserCreatesAndFindsBySubjectOnly(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	mustCreate(t, st, localUser("usr_admin", "admin", auth.RoleAdmin))

	first, err := st.SignInExternalUser(ctx, ssoSignIn("https://idp#42", "jane", auth.RoleViewer))
	if err != nil || !first.Created || first.User.AuthProvider != auth.ProviderOIDC || first.User.Role != auth.RoleViewer ||
		first.User.LastLoginAt == nil || first.RoleFrom != "" {
		t.Fatalf("first sign-in = %+v, %v", first, err)
	}
	// The second sign-in finds the user by subject, even with another name.
	in := ssoSignIn("https://idp#42", "renamed", auth.RoleViewer)
	in.At = identityT0.Add(time.Hour)
	again, err := st.SignInExternalUser(ctx, in)
	if err != nil || again.Created || again.User.ID != first.User.ID || again.User.Username != "jane" ||
		!again.User.LastLoginAt.Equal(in.At) {
		t.Fatalf("second sign-in = %+v, %v", again, err)
	}
	// A changed issuer is a new user, and its name collides with the first one.
	if _, err = st.SignInExternalUser(ctx, ssoSignIn("https://other-idp#42", "jane", auth.RoleViewer)); !errors.Is(err, auth.ErrAccountConflict) {
		t.Errorf("same name from another issuer = %v; want ErrAccountConflict", err)
	}
	// A local user is never linked by name, case-insensitively either.
	if _, err = st.SignInExternalUser(ctx, ssoSignIn("https://idp#7", "ADMIN", auth.RoleAdmin)); !errors.Is(err, auth.ErrAccountConflict) {
		t.Errorf("identity named like the local admin = %v; want ErrAccountConflict", err)
	}
	admin, err := st.GetUser(ctx, "usr_admin")
	if err != nil || admin.AuthProvider != auth.ProviderLocal || admin.Subject != "" || admin.PasswordHash == "" {
		t.Errorf("local admin after the conflict = %+v, %v", admin, err)
	}
	// Without auto-creation unknown subjects are refused.
	off := ssoSignIn("https://idp#99", "newcomer", auth.RoleViewer)
	off.AutoCreate = false
	if _, err = st.SignInExternalUser(ctx, off); !errors.Is(err, auth.ErrUnknownExternalUser) {
		t.Errorf("unknown subject without auto-create = %v; want ErrUnknownExternalUser", err)
	}
	// ... while known ones still sign in.
	known := ssoSignIn("https://idp#42", "jane", auth.RoleViewer)
	known.AutoCreate = false
	if res, knownErr := st.SignInExternalUser(ctx, known); knownErr != nil || res.User.ID != first.User.ID {
		t.Errorf("known subject without auto-create = %+v, %v", res, knownErr)
	}
}

func TestSignInExternalUserRecomputesTheRole(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	mustCreate(t, st, localUser("usr_admin", "admin", auth.RoleAdmin))
	created, err := st.SignInExternalUser(ctx, ssoSignIn("https://idp#1", "jane", auth.RoleViewer))
	if err != nil {
		t.Fatal(err)
	}
	id := created.User.ID
	sessionFor(t, st, id, "s1")

	// Unchanged role: sessions stay.
	if _, err = st.SignInExternalUser(ctx, ssoSignIn("https://idp#1", "jane", auth.RoleViewer)); err != nil || !hasSession(t, st, "s1") {
		t.Fatalf("unchanged role: %v, session kept %v", err, hasSession(t, st, "s1"))
	}
	// A promotion revokes the sessions, like UpdateUserRole.
	res, err := st.SignInExternalUser(ctx, ssoSignIn("https://idp#1", "jane", auth.RoleAdmin))
	if err != nil || res.RoleFrom != auth.RoleViewer || res.User.Role != auth.RoleAdmin || res.RoleKept || hasSession(t, st, "s1") {
		t.Fatalf("promotion = %+v, %v", res, err)
	}
	// The demotion of an admin while another admin exists applies.
	if res, err = st.SignInExternalUser(ctx, ssoSignIn("https://idp#1", "jane", auth.RoleOperator)); err != nil ||
		res.User.Role != auth.RoleOperator || res.RoleKept {
		t.Fatalf("demotion = %+v, %v", res, err)
	}
	// The last admin is never demoted: the stored role is kept and the sign-in goes on.
	if _, err = st.SignInExternalUser(ctx, ssoSignIn("https://idp#1", "jane", auth.RoleAdmin)); err != nil {
		t.Fatal(err)
	}
	if err = st.DeleteUser(ctx, "", "usr_admin", false); err != nil {
		t.Fatal(err)
	}
	sessionFor(t, st, id, "s2")
	res, err = st.SignInExternalUser(ctx, ssoSignIn("https://idp#1", "jane", auth.RoleViewer))
	if err != nil || !res.RoleKept || res.User.Role != auth.RoleAdmin || !hasSession(t, st, "s2") {
		t.Fatalf("demoting the last admin = %+v, %v; want the role kept", res, err)
	}
	if u, getErr := st.GetUser(ctx, id); getErr != nil || u.Role != auth.RoleAdmin {
		t.Errorf("stored role = %+v, %v; want admin", u, getErr)
	}
}

func TestLastLocalAdminIsKeptWhileSSOIsOn(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	mustCreate(t, st, localUser("usr_local", "breakglass", auth.RoleAdmin))
	sso, err := st.SignInExternalUser(ctx, ssoSignIn("https://idp#1", "jane", auth.RoleAdmin))
	if err != nil {
		t.Fatal(err)
	}
	if n, countErr := st.CountLocalAdmins(ctx); countErr != nil || n != 1 {
		t.Fatalf("CountLocalAdmins = %d, %v; want 1", n, countErr)
	}
	// Two admins exist, but only one is local.
	if _, err = st.UpdateUserRole(ctx, sso.User.ID, "usr_local", auth.RoleViewer, identityT0, true); !errors.Is(err, auth.ErrLastLocalAdmin) {
		t.Errorf("demoting the last local admin = %v; want ErrLastLocalAdmin", err)
	}
	if err = st.DeleteUser(ctx, sso.User.ID, "usr_local", true); !errors.Is(err, auth.ErrLastLocalAdmin) {
		t.Errorf("deleting the last local admin = %v; want ErrLastLocalAdmin", err)
	}
	// The OIDC admin can be demoted: the local admin remains.
	if _, err = st.UpdateUserRole(ctx, "usr_local", sso.User.ID, auth.RoleViewer, identityT0, true); err != nil {
		t.Errorf("demoting the oidc admin: %v", err)
	}
	// With single sign-on off the rule does not apply (ErrLastAdmin still does).
	if _, err = st.UpdateUserRole(ctx, "", sso.User.ID, auth.RoleAdmin, identityT0, false); err != nil {
		t.Fatal(err)
	}
	if _, err = st.UpdateUserRole(ctx, sso.User.ID, "usr_local", auth.RoleViewer, identityT0, false); err != nil {
		t.Errorf("demoting the local admin with sso off: %v", err)
	}
}

func TestDeleteLocalNonAdminSessions(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	mustCreate(t, st, localUser("usr_admin", "admin", auth.RoleAdmin))
	mustCreate(t, st, localUser("usr_op", "op", auth.RoleOperator))
	sso, err := st.SignInExternalUser(ctx, ssoSignIn("https://idp#1", "jane", auth.RoleViewer))
	if err != nil {
		t.Fatal(err)
	}
	sessionFor(t, st, "usr_admin", "admin")
	sessionFor(t, st, "usr_op", "op")
	sessionFor(t, st, sso.User.ID, "sso")
	n, err := st.DeleteLocalNonAdminSessions(ctx)
	if err != nil || n != 1 {
		t.Fatalf("DeleteLocalNonAdminSessions = %d, %v; want 1", n, err)
	}
	if !hasSession(t, st, "admin") || hasSession(t, st, "op") || !hasSession(t, st, "sso") {
		t.Error("only the local non-admin's session may end")
	}
}

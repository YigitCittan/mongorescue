package settings

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func validOIDC() *OIDCPatch {
	return &OIDCPatch{
		Enabled:     ptr(true),
		Issuer:      ptr("https://idp.example.com/realms/ops"),
		ClientID:    ptr("mongorescue"),
		RedirectURL: ptr("https://backup.example.com/auth/oidc/callback"),
	}
}

func TestOIDCDefaults(t *testing.T) {
	o := newSvc(t, &memRepo{}).Current().OIDC
	if o.Enabled || o.DisplayName != "Single sign-on" || !slices.Equal(o.Scopes, []string{"openid", "email", "profile"}) ||
		o.UsernameClaim != "preferred_username" || o.GroupsClaim != "groups" || o.DefaultRole != "" ||
		!o.AutoCreateUsers || o.LocalLogin != OIDCLocalLoginAll || o.RPLogout || len(o.RoleMappings) != 0 ||
		len(o.AllowedEmailDomains) != 0 {
		t.Fatalf("defaults = %+v", o)
	}
}

func TestOIDCValidation(t *testing.T) {
	ctx := context.Background()
	mappings := make([]OIDCRoleMapping, MaxOIDCRoleMappings+1)
	for i := range mappings {
		mappings[i] = OIDCRoleMapping{Group: "g" + strings.Repeat("x", i), Role: "viewer"}
	}
	for name, mutate := range map[string]func(p *OIDCPatch){
		"no issuer":                 func(p *OIDCPatch) { p.Issuer = ptr("") },
		"no client":                 func(p *OIDCPatch) { p.ClientID = ptr("") },
		"no redirect url":           func(p *OIDCPatch) { p.RedirectURL = ptr("") },
		"http issuer":               func(p *OIDCPatch) { p.Issuer = ptr("http://idp.example.com") },
		"issuer with a query":       func(p *OIDCPatch) { p.Issuer = ptr("https://idp.example.com/?x=1") },
		"issuer with credentials":   func(p *OIDCPatch) { p.Issuer = ptr("https://u:p@idp.example.com") },
		"entra common":              func(p *OIDCPatch) { p.Issuer = ptr("https://login.microsoftonline.com/common/v2.0") },
		"entra organizations":       func(p *OIDCPatch) { p.Issuer = ptr("https://login.microsoftonline.com/Organizations/v2.0") },
		"redirect to another path":  func(p *OIDCPatch) { p.RedirectURL = ptr("https://backup.example.com/callback") },
		"redirect with a query":     func(p *OIDCPatch) { p.RedirectURL = ptr("https://backup.example.com/auth/oidc/callback?next=/") },
		"redirect encoded path":     func(p *OIDCPatch) { p.RedirectURL = ptr("https://backup.example.com/auth/oidc%2fcallback") },
		"redirect without a host":   func(p *OIDCPatch) { p.RedirectURL = ptr("/auth/oidc/callback") },
		"default role admin":        func(p *OIDCPatch) { p.DefaultRole = ptr("admin") },
		"default role unknown":      func(p *OIDCPatch) { p.DefaultRole = ptr("root") },
		"mapping to an unknown":     func(p *OIDCPatch) { p.RoleMappings = &[]OIDCRoleMapping{{Group: "ops", Role: "root"}} },
		"mapping without a group":   func(p *OIDCPatch) { p.RoleMappings = &[]OIDCRoleMapping{{Group: " ", Role: "admin"}} },
		"too many mappings":         func(p *OIDCPatch) { p.RoleMappings = &mappings },
		"a bad domain":              func(p *OIDCPatch) { p.AllowedEmailDomains = &[]string{"corp com"} },
		"a bare domain":             func(p *OIDCPatch) { p.AllowedEmailDomains = &[]string{"localhost"} },
		"a bad local login":         func(p *OIDCPatch) { p.LocalLogin = ptr("nobody") },
		"a bad scope":               func(p *OIDCPatch) { p.Scopes = &[]string{"openid", "a b"} },
		"a dotted username claim":   func(p *OIDCPatch) { p.UsernameClaim = ptr("a.b") },
		"a bad groups claim":        func(p *OIDCPatch) { p.GroupsClaim = ptr("realm_access..roles") },
		"a control in display name": func(p *OIDCPatch) { p.DisplayName = ptr("S\x01SO") },
	} {
		svc := newSvc(t, &memRepo{})
		p := validOIDC()
		mutate(p)
		if _, err := svc.Update(ctx, Patch{OIDC: p}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Update = %v; want ErrInvalid", name, err)
		}
	}

	svc := newSvc(t, &memRepo{})
	p := validOIDC()
	p.Issuer = ptr("http://127.0.0.1:8080/realms/dev") // loopback may use http
	p.Scopes = &[]string{"email", "groups", "email"}
	p.AllowedEmailDomains = &[]string{" @Corp.COM ", "corp.com", "example.org"}
	p.RoleMappings = &[]OIDCRoleMapping{{Group: " ops-admins ", Role: "admin"}, {Group: "ops-admins", Role: "admin"}}
	p.DefaultRole = ptr("operator")
	got, err := svc.Update(ctx, Patch{OIDC: p})
	if err != nil {
		t.Fatal(err)
	}
	o := got.OIDC
	if !slices.Equal(o.Scopes, []string{"openid", "email", "groups"}) || !slices.Equal(o.AllowedEmailDomains, []string{"corp.com", "example.org"}) ||
		len(o.RoleMappings) != 1 || o.RoleMappings[0].Group != "ops-admins" || o.DefaultRole != "operator" {
		t.Fatalf("normalised = %+v", o)
	}
	// A disabled section may stay incomplete.
	if _, err = newSvc(t, &memRepo{}).Update(ctx, Patch{OIDC: &OIDCPatch{DisplayName: ptr("Corp SSO")}}); err != nil {
		t.Fatalf("partial disabled section: %v", err)
	}
	// Entra tenant issuers are fine.
	if err = ValidateOIDCIssuer("https://login.microsoftonline.com/9188040d-6c67-4c5b-b112-36a304b66dad/v2.0"); err != nil {
		t.Errorf("tenant issuer: %v", err)
	}
}

func TestOIDCClientSecretIsMaskedSealedAndKept(t *testing.T) {
	repo := &memRepo{}
	svc := newSvc(t, repo)
	ctx := context.Background()
	const secret = "oidc-client-secret-value"
	p := validOIDC()
	p.ClientSecret = ptr(secret)
	masked, err := svc.Update(ctx, Patch{OIDC: p})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(masked)
	if strings.Contains(string(raw), secret) || masked.OIDC.ClientSecret != SecretMask {
		t.Fatalf("masked settings leak the client secret: %s", raw)
	}
	if !IsSecret(KeyOIDCClientSecret) {
		t.Fatal("the client secret must be sealed at rest")
	}
	if _, err = svc.Update(ctx, Patch{OIDC: &OIDCPatch{ClientSecret: ptr(SecretMask), DisplayName: ptr("Corp")}}); err != nil {
		t.Fatal(err)
	}
	if svc.Current().OIDC.ClientSecret != secret {
		t.Fatal("sending the mask back must keep the secret")
	}
	if _, err = svc.Update(ctx, Patch{OIDC: &OIDCPatch{ClientSecret: ptr("")}}); err != nil {
		t.Fatal(err)
	}
	if svc.Current().OIDC.ClientSecret != "" {
		t.Fatal("an empty secret makes a public client")
	}
	if _, err = svc.Update(ctx, Patch{OIDC: &OIDCPatch{ClientSecret: ptr(SecretMask)}}); !errors.Is(err, ErrMaskedSecret) {
		t.Fatalf("mask without a stored secret = %v", err)
	}
	// The section survives a reload.
	if o := newSvc(t, repo).Current().OIDC; !o.Enabled || o.Issuer != "https://idp.example.com/realms/ops" {
		t.Fatalf("reloaded = %+v", o)
	}
}

type recordingGuard struct {
	refuse  error
	checks  int
	applied []OIDC
}

func (g *recordingGuard) CheckOIDC(_ context.Context, _, _ OIDC) error {
	g.checks++
	return g.refuse
}

func (g *recordingGuard) OIDCChanged(_ context.Context, _, next OIDC) error {
	g.applied = append(g.applied, next)
	return nil
}

func TestOIDCGuardRunsOnSectionChanges(t *testing.T) {
	ctx := context.Background()
	g := &recordingGuard{refuse: errors.New("no local admin")}
	svc := newSvc(t, &memRepo{})
	svc.SetOIDCGuard(g)
	if _, err := svc.Update(ctx, Patch{OIDC: validOIDC()}); err == nil || svc.Current().OIDC.Enabled {
		t.Fatalf("refused change = %v; enabled %v", err, svc.Current().OIDC.Enabled)
	}
	g.refuse = nil
	if _, err := svc.Update(ctx, Patch{OIDC: validOIDC()}); err != nil || len(g.applied) != 1 || !g.applied[0].Enabled {
		t.Fatalf("accepted change = %v, applied %+v", err, g.applied)
	}
	// Other sections never reach the guard.
	before := g.checks
	if _, err := svc.Update(ctx, Patch{General: &GeneralPatch{DefaultGzip: ptr(false)}}); err != nil || g.checks != before {
		t.Fatalf("unrelated change: %v, guard ran %d times", err, g.checks-before)
	}
}

func TestOIDCRoleKeptWarning(t *testing.T) {
	repo := &memRepo{}
	svc := newSvc(t, repo)
	ctx := context.Background()
	has := func(s *Service) bool {
		return slices.ContainsFunc(s.Warnings(), func(w Warning) bool { return w.ID == WarningOIDCRoleKept })
	}
	if has(svc) {
		t.Fatal("warning active on a fresh installation")
	}
	if err := svc.RaiseOIDCRoleKeptWarning(ctx); err != nil || !has(svc) || !has(newSvc(t, repo)) {
		t.Fatalf("raise: %v; active %v", err, has(svc))
	}
	if err := svc.DismissWarning(ctx, WarningOIDCRoleKept); err != nil || has(svc) || has(newSvc(t, repo)) {
		t.Fatalf("dismiss: %v; active %v", err, has(svc))
	}
	if err := svc.RaiseOIDCRoleKeptWarning(ctx); err != nil || !has(svc) {
		t.Fatalf("raise again: %v", err)
	}
}

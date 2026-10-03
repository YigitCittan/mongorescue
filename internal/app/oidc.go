package app

import (
	"context"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/auth/oidc"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// oidcPolicy maps the single sign-on settings to the policy the auth service
// enforces. In the desktop app single sign-on is always off.
func oidcPolicy(o settings.OIDC, desktop bool) auth.OIDCPolicy {
	p := auth.OIDCPolicy{
		Enabled:             o.Enabled && !desktop,
		LocalLogin:          auth.LocalLogin(o.LocalLogin),
		DefaultRole:         auth.Role(o.DefaultRole),
		AllowedEmailDomains: o.AllowedEmailDomains,
		AutoCreateUsers:     o.AutoCreateUsers,
	}
	for _, m := range o.RoleMappings {
		p.RoleMappings = append(p.RoleMappings, auth.RoleMapping{Group: m.Group, Role: auth.Role(m.Role)})
	}
	return p
}

// oidcGuard checks and applies changes of the single sign-on settings (see
// settings.OIDCGuard): a local administrator must exist to turn single sign-on on
// or limit the password form, turning it on (or changing the issuer while on)
// needs a provider that answers discovery, and limiting the password form ends the
// sessions of local users without the admin role.
type oidcGuard struct {
	auth    *auth.Service
	client  *oidc.Client
	desktop bool
}

// CheckOIDC implements settings.OIDCGuard. The local administrator is checked here
// for a clear early answer and again, as a precondition, in the transaction that
// stores the change.
func (g *oidcGuard) CheckOIDC(ctx context.Context, prev, next settings.OIDC) ([]settings.Precondition, error) {
	if g.desktop && next.Enabled && !prev.Enabled {
		return nil, fmt.Errorf("%w: single sign-on is not available in the desktop app", settings.ErrInvalid)
	}
	pp, np := oidcPolicy(prev, false), oidcPolicy(next, false)
	if err := g.auth.CheckOIDCChange(ctx, pp, np); err != nil {
		return nil, fmt.Errorf("%w: %w", settings.ErrInvalid, err)
	}
	if next.Enabled && (!prev.Enabled || prev.Issuer != next.Issuer) && g.client != nil {
		if _, err := g.client.Discover(ctx, next.Issuer); err != nil {
			return nil, fmt.Errorf("%w: oidc.issuer: %w", settings.ErrInvalid, err)
		}
	}
	if auth.OIDCChangeNeedsLocalAdmin(pp, np) {
		return []settings.Precondition{settings.PreconditionLocalAdmin}, nil
	}
	return nil, nil
}

// OIDCChanged implements settings.OIDCGuard.
func (g *oidcGuard) OIDCChanged(ctx context.Context, prev, next settings.OIDC) error {
	return g.auth.ApplyOIDCChange(ctx, oidcPolicy(prev, g.desktop), oidcPolicy(next, g.desktop))
}

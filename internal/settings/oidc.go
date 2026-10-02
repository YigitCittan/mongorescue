package settings

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
)

// OIDCCallbackPath is the only path a redirect URL may have: the callback route of
// the single sign-on flow.
const OIDCCallbackPath = "/auth/oidc/callback"

// Local login modes of the single sign-on settings (see OIDC.LocalLogin).
const (
	// OIDCLocalLoginAll lets every local user use the password form.
	OIDCLocalLoginAll = "all"
	// OIDCLocalLoginAdminsOnly lets only local administrators use the password form
	// while single sign-on is on (break-glass).
	OIDCLocalLoginAdminsOnly = "admins_only"
)

// Defaults and limits of the single sign-on settings.
const (
	// DefaultOIDCDisplayName labels the sign-in button of a fresh installation.
	DefaultOIDCDisplayName = "Single sign-on"
	// DefaultOIDCUsernameClaim names new users.
	DefaultOIDCUsernameClaim = "preferred_username"
	// DefaultOIDCGroupsClaim holds the groups that role mappings match.
	DefaultOIDCGroupsClaim = "groups"
	// MaxOIDCRoleMappings bounds OIDC.RoleMappings.
	MaxOIDCRoleMappings = 100

	maxOIDCDisplayName  = 64
	maxOIDCURLLength    = 2048
	maxOIDCClientID     = 256
	maxOIDCClientSecret = 1024
	maxOIDCScopes       = 20
	maxOIDCScopeLength  = 128
	maxOIDCClaimLength  = 128
	maxOIDCGroupLength  = 256
	maxOIDCDomains      = 100
	maxOIDCDomainLength = 253
)

// OIDCRoleMapping gives the members of an identity provider group a dashboard role.
type OIDCRoleMapping struct {
	// Group is the value in the groups claim, matched exactly (an Entra ID group
	// object ID, an Okta group name, a Keycloak role).
	Group string `json:"group"`
	// Role is viewer, operator or admin.
	Role string `json:"role"`
}

// OIDC configures single sign-on through an OpenID Connect provider. Local
// accounts remain, and a local administrator is the break-glass way in.
type OIDC struct {
	// Enabled shows the single sign-on button and accepts sign-ins. Turning it on
	// needs a valid section, a reachable provider and a local administrator.
	Enabled bool `json:"enabled"`
	// DisplayName labels the sign-in button.
	DisplayName string `json:"display_name"`
	// Issuer is the provider's issuer URL: https (http only for loopback), and for
	// Entra ID a tenant issuer, never /common or /organizations.
	Issuer string `json:"issuer"`
	// ClientID is the client registered at the provider.
	ClientID string `json:"client_id"`
	// ClientSecret authenticates the client ("" for a public client, which relies on
	// PKCE alone). Secret.
	ClientSecret string `json:"client_secret"`
	// Scopes are requested at sign-in; "openid" is always included.
	Scopes []string `json:"scopes"`
	// RedirectURL is the callback URL registered at the provider. It is never
	// derived from the request; its path must be OIDCCallbackPath.
	RedirectURL string `json:"redirect_url"`
	// UsernameClaim names new users (falling back to email, then sub).
	UsernameClaim string `json:"username_claim"`
	// GroupsClaim is the claim, or dot path (realm_access.roles), with the groups
	// role mappings match ("" = none).
	GroupsClaim string `json:"groups_claim"`
	// RoleMappings map groups to roles; the highest matching role wins.
	RoleMappings []OIDCRoleMapping `json:"role_mappings"`
	// DefaultRole applies when no mapping matches: "" denies, viewer or operator
	// grant that role. Admin only ever comes from a mapping.
	DefaultRole string `json:"default_role"`
	// AllowedEmailDomains limits sign-in to verified emails of these domains
	// (empty: any).
	AllowedEmailDomains []string `json:"allowed_email_domains"`
	// AutoCreateUsers creates a user at an identity's first sign-in; when off,
	// unknown identities are refused.
	AutoCreateUsers bool `json:"auto_create_users"`
	// LocalLogin is OIDCLocalLoginAll or OIDCLocalLoginAdminsOnly.
	LocalLogin string `json:"local_login"`
	// RPLogout also signs single sign-on users out at the provider (its
	// end_session_endpoint) when they sign out.
	RPLogout bool `json:"rp_logout"`
}

// defaultOIDC returns the single sign-on settings of a fresh installation: off.
func defaultOIDC() OIDC {
	return OIDC{
		DisplayName:         DefaultOIDCDisplayName,
		Scopes:              []string{"openid", "email", "profile"},
		UsernameClaim:       DefaultOIDCUsernameClaim,
		GroupsClaim:         DefaultOIDCGroupsClaim,
		RoleMappings:        []OIDCRoleMapping{},
		AllowedEmailDomains: []string{},
		AutoCreateUsers:     true,
		LocalLogin:          OIDCLocalLoginAll,
	}
}

// clone returns a deep copy of o.
func (o OIDC) clone() OIDC {
	o.Scopes = slices.Clone(o.Scopes)
	o.RoleMappings = slices.Clone(o.RoleMappings)
	o.AllowedEmailDomains = slices.Clone(o.AllowedEmailDomains)
	return o
}

// masked returns o with the client secret replaced by SecretMask and nil slices
// as empty ones.
func (o OIDC) masked() OIDC {
	o = o.clone()
	if o.ClientSecret != "" {
		o.ClientSecret = SecretMask
	}
	if o.Scopes == nil {
		o.Scopes = []string{}
	}
	if o.RoleMappings == nil {
		o.RoleMappings = []OIDCRoleMapping{}
	}
	if o.AllowedEmailDomains == nil {
		o.AllowedEmailDomains = []string{}
	}
	return o
}

// Equal reports whether o and other are the same settings.
func (o OIDC) Equal(other OIDC) bool {
	return o.Enabled == other.Enabled && o.DisplayName == other.DisplayName && o.Issuer == other.Issuer &&
		o.ClientID == other.ClientID && o.ClientSecret == other.ClientSecret && slices.Equal(o.Scopes, other.Scopes) &&
		o.RedirectURL == other.RedirectURL && o.UsernameClaim == other.UsernameClaim && o.GroupsClaim == other.GroupsClaim &&
		slices.Equal(o.RoleMappings, other.RoleMappings) && o.DefaultRole == other.DefaultRole &&
		slices.Equal(o.AllowedEmailDomains, other.AllowedEmailDomains) && o.AutoCreateUsers == other.AutoCreateUsers &&
		o.LocalLogin == other.LocalLogin && o.RPLogout == other.RPLogout
}

// OIDCPatch updates OIDC; see OIDC for the fields. ClientSecret follows the
// keep-secret rule: SecretMask keeps the stored secret, "" removes it (a public
// client) and any other value replaces it.
type OIDCPatch struct {
	Enabled             *bool              `json:"enabled,omitempty"`
	DisplayName         *string            `json:"display_name,omitempty"`
	Issuer              *string            `json:"issuer,omitempty"`
	ClientID            *string            `json:"client_id,omitempty"`
	ClientSecret        *string            `json:"client_secret,omitempty"`
	Scopes              *[]string          `json:"scopes,omitempty"`
	RedirectURL         *string            `json:"redirect_url,omitempty"`
	UsernameClaim       *string            `json:"username_claim,omitempty"`
	GroupsClaim         *string            `json:"groups_claim,omitempty"`
	RoleMappings        *[]OIDCRoleMapping `json:"role_mappings,omitempty"`
	DefaultRole         *string            `json:"default_role,omitempty"`
	AllowedEmailDomains *[]string          `json:"allowed_email_domains,omitempty"`
	AutoCreateUsers     *bool              `json:"auto_create_users,omitempty"`
	LocalLogin          *string            `json:"local_login,omitempty"`
	RPLogout            *bool              `json:"rp_logout,omitempty"`
}

// apply sets the fields of p that are present on o. A nil p changes nothing.
func (p *OIDCPatch) apply(o *OIDC) error {
	if p == nil {
		return nil
	}
	setIf(&o.Enabled, p.Enabled)
	setIf(&o.DisplayName, p.DisplayName)
	setIf(&o.Issuer, p.Issuer)
	setIf(&o.ClientID, p.ClientID)
	if p.ClientSecret != nil {
		v, err := keepSecret(*p.ClientSecret, o.ClientSecret)
		if err != nil {
			return fmt.Errorf("oidc.client_secret: %w", err)
		}
		o.ClientSecret = v
	}
	if p.Scopes != nil {
		o.Scopes = slices.Clone(*p.Scopes)
	}
	setIf(&o.RedirectURL, p.RedirectURL)
	setIf(&o.UsernameClaim, p.UsernameClaim)
	setIf(&o.GroupsClaim, p.GroupsClaim)
	if p.RoleMappings != nil {
		o.RoleMappings = slices.Clone(*p.RoleMappings)
	}
	setIf(&o.DefaultRole, p.DefaultRole)
	if p.AllowedEmailDomains != nil {
		o.AllowedEmailDomains = slices.Clone(*p.AllowedEmailDomains)
	}
	setIf(&o.AutoCreateUsers, p.AutoCreateUsers)
	setIf(&o.LocalLogin, p.LocalLogin)
	setIf(&o.RPLogout, p.RPLogout)
	return nil
}

// hasControl reports whether s contains a control character.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// isLoopbackHost reports whether host is localhost or a loopback address.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// entraHosts are the sign-in hosts of Microsoft Entra ID (public and national
// clouds), where /common and /organizations are multi-tenant endpoints, not
// issuers.
var entraHosts = []string{"login.microsoftonline.com", "login.microsoftonline.us", "login.chinacloudapi.cn", "login.partner.microsoftonline.cn"}

// ValidateOIDCIssuer checks an issuer URL: an absolute https URL without user
// information, query or fragment (http only for loopback hosts), and for Entra ID a
// tenant issuer rather than the multi-tenant /common or /organizations.
func ValidateOIDCIssuer(raw string) error {
	if raw == "" || len(raw) > maxOIDCURLLength || hasControl(raw) {
		return fmt.Errorf("%w: oidc.issuer must be an https URL", ErrInvalid)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("%w: oidc.issuer must be an https URL without credentials, query or fragment", ErrInvalid)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
	default:
		return fmt.Errorf("%w: oidc.issuer must use https (http only for localhost)", ErrInvalid)
	}
	if slices.Contains(entraHosts, strings.ToLower(u.Hostname())) {
		tenant, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
		if strings.EqualFold(tenant, "common") || strings.EqualFold(tenant, "organizations") {
			return fmt.Errorf("%w: oidc.issuer: use the issuer of your Entra ID tenant (https://login.microsoftonline.com/<tenant-id>/v2.0), not /%s", ErrInvalid, tenant)
		}
	}
	return nil
}

// ValidateOIDCRedirectURL checks a redirect URL: an absolute http or https URL
// whose path is exactly OIDCCallbackPath, without user information, query or
// fragment.
func ValidateOIDCRedirectURL(raw string) error {
	if raw == "" || len(raw) > maxOIDCURLLength || hasControl(raw) {
		return fmt.Errorf("%w: oidc.redirect_url must be this server's URL ending in %s", ErrInvalid, OIDCCallbackPath)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.Path != OIDCCallbackPath || u.RawPath != "" {
		return fmt.Errorf("%w: oidc.redirect_url must be this server's URL ending in %s, such as https://backup.example.com%s",
			ErrInvalid, OIDCCallbackPath, OIDCCallbackPath)
	}
	return nil
}

// validClaimPath reports whether p is a claim name or dot path of claim names.
func validClaimPath(p string) bool {
	if p == "" || len(p) > maxOIDCClaimLength {
		return false
	}
	for _, seg := range strings.Split(p, ".") {
		if seg == "" || strings.ContainsFunc(seg, func(r rune) bool { return !isClaimRune(r) }) {
			return false
		}
	}
	return true
}

// isClaimRune reports whether r may appear in a claim name.
func isClaimRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == ':' || r == '/'
}

// isDomainRune reports whether r may appear in a lower-case DNS name.
func isDomainRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.'
}

// validScope reports whether s is a scope token (RFC 6749 NQCHAR without space).
func validScope(s string) bool {
	if s == "" || len(s) > maxOIDCScopeLength {
		return false
	}
	for _, r := range s {
		if r <= 0x20 || r >= 0x7f || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}

// validDomain reports whether d is a lower-case DNS name with at least one dot.
func validDomain(d string) bool {
	if len(d) > maxOIDCDomainLength || !strings.Contains(d, ".") || strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") ||
		strings.Contains(d, "..") {
		return false
	}
	return !strings.ContainsFunc(d, func(r rune) bool { return !isDomainRune(r) })
}

// validateOIDC normalises and checks o. The issuer, client and redirect URL are
// required only while single sign-on is enabled, but are always checked when set.
// Whether a local administrator exists and the provider answers is checked by the
// OIDCGuard when the section changes.
func validateOIDC(o *OIDC) error {
	o.DisplayName = strings.TrimSpace(o.DisplayName)
	if o.DisplayName == "" {
		o.DisplayName = DefaultOIDCDisplayName
	}
	if len([]rune(o.DisplayName)) > maxOIDCDisplayName || hasControl(o.DisplayName) {
		return fmt.Errorf("%w: oidc.display_name must be at most %d printable characters", ErrInvalid, maxOIDCDisplayName)
	}
	o.Issuer = strings.TrimSpace(o.Issuer)
	o.ClientID = strings.TrimSpace(o.ClientID)
	o.RedirectURL = strings.TrimSpace(o.RedirectURL)
	if o.Issuer != "" {
		if err := ValidateOIDCIssuer(o.Issuer); err != nil {
			return err
		}
	}
	if len(o.ClientID) > maxOIDCClientID || hasControl(o.ClientID) {
		return fmt.Errorf("%w: oidc.client_id must be at most %d printable characters", ErrInvalid, maxOIDCClientID)
	}
	if len(o.ClientSecret) > maxOIDCClientSecret {
		return fmt.Errorf("%w: oidc.client_secret is longer than %d bytes", ErrInvalid, maxOIDCClientSecret)
	}
	if o.RedirectURL != "" {
		if err := ValidateOIDCRedirectURL(o.RedirectURL); err != nil {
			return err
		}
	}
	if o.Enabled {
		switch {
		case o.Issuer == "":
			return fmt.Errorf("%w: oidc.issuer is required to enable single sign-on", ErrInvalid)
		case o.ClientID == "":
			return fmt.Errorf("%w: oidc.client_id is required to enable single sign-on", ErrInvalid)
		case o.RedirectURL == "":
			return fmt.Errorf("%w: oidc.redirect_url is required to enable single sign-on", ErrInvalid)
		}
	}

	scopes := []string{"openid"}
	for _, raw := range o.Scopes {
		sc := strings.TrimSpace(raw)
		if sc == "" || slices.Contains(scopes, sc) {
			continue
		}
		if !validScope(sc) {
			return fmt.Errorf("%w: oidc.scopes: %q is not a scope", ErrInvalid, truncate(sc, 40))
		}
		scopes = append(scopes, sc)
	}
	if len(scopes) > maxOIDCScopes {
		return fmt.Errorf("%w: oidc.scopes allows at most %d entries", ErrInvalid, maxOIDCScopes)
	}
	o.Scopes = scopes

	o.UsernameClaim = strings.TrimSpace(o.UsernameClaim)
	if o.UsernameClaim == "" {
		o.UsernameClaim = DefaultOIDCUsernameClaim
	}
	if !validClaimPath(o.UsernameClaim) || strings.Contains(o.UsernameClaim, ".") {
		return fmt.Errorf("%w: oidc.username_claim must be a claim name such as preferred_username", ErrInvalid)
	}
	o.GroupsClaim = strings.TrimSpace(o.GroupsClaim)
	if o.GroupsClaim != "" && !validClaimPath(o.GroupsClaim) {
		return fmt.Errorf("%w: oidc.groups_claim must be a claim name or dot path such as realm_access.roles", ErrInvalid)
	}

	if len(o.RoleMappings) > MaxOIDCRoleMappings {
		return fmt.Errorf("%w: oidc.role_mappings allows at most %d entries", ErrInvalid, MaxOIDCRoleMappings)
	}
	mappings := make([]OIDCRoleMapping, 0, len(o.RoleMappings))
	for _, m := range o.RoleMappings {
		m.Group = strings.TrimSpace(m.Group)
		m.Role = strings.TrimSpace(m.Role)
		if m.Group == "" || len(m.Group) > maxOIDCGroupLength || hasControl(m.Group) {
			return fmt.Errorf("%w: oidc.role_mappings: a group must be 1-%d printable characters", ErrInvalid, maxOIDCGroupLength)
		}
		switch m.Role {
		case "viewer", "operator", "admin":
		default:
			return fmt.Errorf("%w: oidc.role_mappings: the role of %q must be viewer, operator or admin", ErrInvalid, truncate(m.Group, 40))
		}
		if !slices.Contains(mappings, m) {
			mappings = append(mappings, m)
		}
	}
	o.RoleMappings = mappings

	o.DefaultRole = strings.TrimSpace(o.DefaultRole)
	switch o.DefaultRole {
	case "", "viewer", "operator":
	case "admin":
		return fmt.Errorf("%w: oidc.default_role cannot be admin: the admin role only comes from a group mapping", ErrInvalid)
	default:
		return fmt.Errorf("%w: oidc.default_role must be empty (deny), viewer or operator", ErrInvalid)
	}

	domains := make([]string, 0, len(o.AllowedEmailDomains))
	for _, raw := range o.AllowedEmailDomains {
		d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(raw), "@"))
		if d == "" || slices.Contains(domains, d) {
			continue
		}
		if !validDomain(d) {
			return fmt.Errorf("%w: oidc.allowed_email_domains: %q is not a domain such as example.com", ErrInvalid, truncate(d, 80))
		}
		domains = append(domains, d)
	}
	if len(domains) > maxOIDCDomains {
		return fmt.Errorf("%w: oidc.allowed_email_domains allows at most %d entries", ErrInvalid, maxOIDCDomains)
	}
	o.AllowedEmailDomains = domains

	if o.LocalLogin == "" {
		o.LocalLogin = OIDCLocalLoginAll
	}
	if o.LocalLogin != OIDCLocalLoginAll && o.LocalLogin != OIDCLocalLoginAdminsOnly {
		return fmt.Errorf("%w: oidc.local_login must be all or admins_only", ErrInvalid)
	}
	return nil
}

// OIDCGuard checks and applies changes of the single sign-on settings with what
// this package cannot see: users and the provider. The application wires it (see
// WithOIDCGuard).
type OIDCGuard interface {
	// CheckOIDC runs before a changed OIDC section is stored; an error refuses the
	// whole update. It checks, for example, that a local administrator exists and
	// that the provider answers discovery when single sign-on is turned on.
	CheckOIDC(ctx context.Context, prev, next OIDC) error
	// OIDCChanged runs after a changed OIDC section was stored, for example to end
	// the sessions the change no longer allows. Its error is logged.
	OIDCChanged(ctx context.Context, prev, next OIDC) error
}

// WithOIDCGuard sets the guard of single sign-on changes.
func WithOIDCGuard(g OIDCGuard) Option { return func(s *Service) { s.oidcGuard = g } }

// SetOIDCGuard sets the guard of single sign-on changes after construction (the
// application builds the guard from services that need the settings first).
func (s *Service) SetOIDCGuard(g OIDCGuard) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.oidcGuard = g
}

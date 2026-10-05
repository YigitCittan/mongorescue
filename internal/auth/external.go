package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
)

// Single sign-on errors. Each maps to one OIDC failure code (see OIDCErrorCode).
var (
	// ErrLastLocalAdmin is returned while single sign-on is enabled when a change
	// would leave no local administrator, the break-glass way in.
	ErrLastLocalAdmin = errors.New("auth: the last local administrator cannot be demoted or deleted while single sign-on is enabled")
	// ErrLocalLoginDisabled is returned, after the password matched, when password
	// sign-in is limited to local administrators (local_login admins_only).
	ErrLocalLoginDisabled = errors.New("auth: password sign-in is limited to local administrators; sign in with single sign-on")
	// ErrAccountConflict is returned when a new single sign-on user would take the
	// name of an existing user. Users are never linked by name or email: an
	// administrator renames one of the two.
	ErrAccountConflict = errors.New("auth: a user with this name already exists; an administrator must rename one of them")
	// ErrRoleManagedByProvider is returned for a manual role change of a single
	// sign-on user while group mappings decide the roles.
	ErrRoleManagedByProvider = errors.New("auth: the role of this user is managed by the identity provider")
	// ErrOIDCDisabled is returned by LoginOIDC while single sign-on is off.
	ErrOIDCDisabled = errors.New("auth: single sign-on is not enabled")
	// ErrDomainNotAllowed is returned when the email domain filter refuses an
	// identity (the domain is not listed or the email is not verified).
	ErrDomainNotAllowed = errors.New("auth: the email domain is not allowed to sign in")
	// ErrNoRole is returned when neither a group mapping nor the default role gives
	// an identity a dashboard role.
	ErrNoRole = errors.New("auth: no dashboard role is mapped to this identity")
	// ErrUnknownExternalUser is returned for an identity without a user while
	// automatic user creation is off.
	ErrUnknownExternalUser = errors.New("auth: no user exists for this identity and automatic creation is off")
	// ErrInvalidIdentity is returned for an identity without an issuer or subject,
	// or without a usable username.
	ErrInvalidIdentity = errors.New("auth: the identity from the provider is incomplete")
)

// OIDC failure codes: the only detail a failed single sign-on shows the browser
// (/?oidc_error=<code>) and records in the audit log.
const (
	// OIDCStateMismatch: the flow cookie is missing, expired, forged or replayed,
	// or the state does not match.
	OIDCStateMismatch = "state_mismatch"
	// OIDCIdPError: the provider reported an error or the code exchange failed.
	OIDCIdPError = "idp_error"
	// OIDCTokenInvalid: the ID token failed verification or lacks required claims.
	OIDCTokenInvalid = "token_invalid"
	// OIDCDomainNotAllowed: the email domain filter refused the identity.
	OIDCDomainNotAllowed = "domain_not_allowed"
	// OIDCNoRole: no role is mapped, or no user exists and creation is off.
	OIDCNoRole = "no_role"
	// OIDCAccountConflict: a new user would take an existing user's name.
	OIDCAccountConflict = "account_conflict"
	// OIDCDisabled: single sign-on is off.
	OIDCDisabled = "disabled"
	// OIDCThrottled: too many sign-in attempts from the client.
	OIDCThrottled = "throttled"
)

// OIDCErrorCodes lists every OIDC failure code.
func OIDCErrorCodes() []string {
	return []string{OIDCStateMismatch, OIDCIdPError, OIDCTokenInvalid, OIDCDomainNotAllowed, OIDCNoRole,
		OIDCAccountConflict, OIDCDisabled, OIDCThrottled}
}

// OIDCErrorCode returns the failure code of an error of LoginOIDC or
// AllowOIDCAttempt, or "" for an internal error.
func OIDCErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrThrottled):
		return OIDCThrottled
	case errors.Is(err, ErrOIDCDisabled):
		return OIDCDisabled
	case errors.Is(err, ErrDomainNotAllowed):
		return OIDCDomainNotAllowed
	case errors.Is(err, ErrNoRole), errors.Is(err, ErrUnknownExternalUser):
		return OIDCNoRole
	case errors.Is(err, ErrAccountConflict):
		return OIDCAccountConflict
	case errors.Is(err, ErrInvalidIdentity):
		return OIDCTokenInvalid
	}
	return ""
}

// LocalLogin decides who may use the password form while single sign-on is on.
type LocalLogin string

// Local login modes.
const (
	// LocalLoginAll lets every local user sign in with a password.
	LocalLoginAll LocalLogin = "all"
	// LocalLoginAdminsOnly lets only local administrators sign in with a password
	// (break-glass); everyone else uses single sign-on.
	LocalLoginAdminsOnly LocalLogin = "admins_only"
)

// RoleMapping gives the members of an identity provider group a dashboard role.
type RoleMapping struct {
	// Group is the group (or role) name in the groups claim, matched exactly.
	Group string
	// Role is the dashboard role its members get.
	Role Role
	// ConnectionAccess is the connections the mapping grants with its role: every
	// connection or exactly a list (none when empty); see
	// OIDCPolicy.MapConnections.
	ConnectionAccess
}

// OIDCPolicy is the part of the single sign-on settings the sign-in rules depend
// on. The application supplies it from the live settings (WithOIDCPolicy).
type OIDCPolicy struct {
	// Enabled turns single sign-on on.
	Enabled bool
	// LocalLogin decides who may use the password form while Enabled.
	LocalLogin LocalLogin
	// RoleMappings map groups to roles; the highest matching role wins.
	RoleMappings []RoleMapping
	// DefaultRole applies when no mapping matches: "" denies, otherwise viewer or
	// operator (admin only ever comes from a mapping).
	DefaultRole Role
	// AllowedEmailDomains limits sign-in to verified emails of these domains (empty:
	// any).
	AllowedEmailDomains []string
	// AutoCreateUsers creates a user at the first sign-in of an identity.
	AutoCreateUsers bool
}

// AdminsOnly reports whether the password form is limited to local administrators:
// single sign-on is on and LocalLogin is LocalLoginAdminsOnly.
func (p OIDCPolicy) AdminsOnly() bool {
	return p.Enabled && p.LocalLogin == LocalLoginAdminsOnly
}

// roleRank orders roles; unknown roles rank 0.
func roleRank(r Role) int {
	switch r {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

// MapRole returns the role of a member of groups: the highest role of the mappings
// whose group is in groups (exact match), else the default role when it is viewer
// or operator, else "" (deny). The default never grants admin.
func (p OIDCPolicy) MapRole(groups []string) Role {
	var best Role
	for _, m := range p.RoleMappings {
		if m.Group != "" && roleRank(m.Role) > roleRank(best) && slices.Contains(groups, m.Group) {
			best = m.Role
		}
	}
	if best == "" && (p.DefaultRole == RoleViewer || p.DefaultRole == RoleOperator) {
		best = p.DefaultRole
	}
	return best
}

// MapConnections returns the connections a member of groups gets with role, the
// role MapRole chose. Only the mappings that grant role count: the union of their
// connections, or every connection when one of them has AllConnections. A mapping
// of a lower role never widens the connections of a higher one. An administrator,
// and a user whose role is the default role (no mapping matched), get every
// connection.
func (p OIDCPolicy) MapConnections(groups []string, role Role) ConnectionAccess {
	if role == RoleAdmin {
		return EveryConnection()
	}
	union := []string{}
	matched := false
	for _, m := range p.RoleMappings {
		if m.Group == "" || m.Role != role || !slices.Contains(groups, m.Group) {
			continue
		}
		if m.AllConnections {
			return EveryConnection()
		}
		matched = true
		union = append(union, m.ConnectionIDs...)
	}
	if !matched {
		return EveryConnection()
	}
	slices.Sort(union)
	return ConnectionAccess{ConnectionIDs: slices.Compact(union)}
}

// EmailDomainAllowed reports whether the email domain filter lets email through.
// Without allowed domains every identity passes. Otherwise the email must be
// verified (the email_verified claim the JSON boolean true) and its domain, the
// lower-cased part after the last "@", must equal one of the allowed domains.
func (p OIDCPolicy) EmailDomainAllowed(email string, verified bool) bool {
	if len(p.AllowedEmailDomains) == 0 {
		return true
	}
	if !verified {
		return false
	}
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return false
	}
	domain := strings.ToLower(email[at+1:])
	for _, d := range p.AllowedEmailDomains {
		if d != "" && strings.ToLower(strings.TrimSpace(d)) == domain {
			return true
		}
	}
	return false
}

// WithOIDCPolicy reads the single sign-on policy from fn on every request, so
// changed settings apply immediately. Without it single sign-on is off.
func WithOIDCPolicy(fn func() OIDCPolicy) Option {
	return func(s *Service) { s.oidcPolicy = fn }
}

// OIDC returns the current single sign-on policy.
func (s *Service) OIDC() OIDCPolicy {
	if s.oidcPolicy == nil {
		return OIDCPolicy{}
	}
	return s.oidcPolicy()
}

// ExternalIdentity is an identity the OIDC provider vouched for: the claims of a
// verified ID token, already extracted (see internal/auth/oidc).
type ExternalIdentity struct {
	// Issuer is the iss claim.
	Issuer string
	// Subject is the sub claim.
	Subject string
	// Username is the name a new user gets, already cleaned to the username rules.
	Username string
	// Email is the email claim ("" when absent).
	Email string
	// EmailVerified is true only when the email_verified claim is the JSON boolean
	// true.
	EmailVerified bool
	// Groups are the values of the groups claim.
	Groups []string
}

// Limits of external identities.
const (
	maxSubjectLength = 255
	maxIssuerLength  = 2048
	// subjectSeparator joins the issuer and the subject of a stored OIDC user.
	subjectSeparator = "#"
)

// ExternalSubject returns the stored subject of an OIDC identity: issuer and
// subject joined by "#". A changed issuer therefore means new users.
func ExternalSubject(issuer, subject string) string {
	return issuer + subjectSeparator + subject
}

// ExternalSignIn is the input of Repository.SignInExternalUser.
type ExternalSignIn struct {
	// Subject is the stored subject (ExternalSubject).
	Subject string
	// NewUserID is the ID of the user when one is created.
	NewUserID string
	// Username is the name of a new user; existing users keep theirs.
	Username string
	// Role is the role the identity has now. A change follows the rules of
	// UpdateUserRole: the user's sessions end, and the last admin is never demoted
	// (the stored role is kept instead, see ExternalSignInResult.RoleKept). With
	// RoleOnCreate it is only the role of a new user and may be "" (no new users).
	Role Role
	// RoleOnCreate applies Role only when the user is created: an existing user
	// keeps the stored role, such as one an administrator set by hand. A new user
	// without a Role is refused with ErrNoRole.
	RoleOnCreate bool
	// Connections are the connections the identity may touch now. They are applied
	// with Role, unless RoleOnCreate is set (then only to a new user, with every
	// connection); an administrator always has every connection.
	Connections ConnectionAccess
	// AutoCreate creates the user when no user has Subject.
	AutoCreate bool
	// KeepAdmin keeps the admin role of an administrator whom Role would demote
	// (the two-person rule holds the demotion back for a second administrator); the
	// result reports RoleKept and DemotionHeld.
	KeepAdmin bool
	// At is the time of the sign-in (created, updated and last login).
	At time.Time
}

// ExternalSignInResult is the outcome of Repository.SignInExternalUser.
type ExternalSignInResult struct {
	// User is the user after the sign-in.
	User *User
	// Created reports that the user was created.
	Created bool
	// RoleFrom is the role before the sign-in ("" for a created user).
	RoleFrom Role
	// RoleKept reports that the new role was not applied because it would have
	// demoted the last administrator, or (DemotionHeld) an administrator while
	// KeepAdmin was set.
	RoleKept bool
	// DemotionHeld reports that the demotion of an administrator was held back for
	// KeepAdmin.
	DemotionHeld bool
}

// OIDCLogin is the outcome of LoginOIDC: a new session and what changed.
type OIDCLogin struct {
	*LoginResult
	// Created reports that the user was created by this sign-in.
	Created bool
	// RoleFrom is the role before the sign-in ("" for a created user) and RoleTo the
	// role after it.
	RoleFrom, RoleTo Role
	// RoleKept reports that a demotion was refused to keep the last administrator,
	// or held back for a second administrator (DemotionHeld).
	RoleKept bool
	// DemotionHeld reports that the two-person rule held the demotion of an
	// administrator back: the admin role stays, and an approval request asks a
	// second administrator to apply MappedRole.
	DemotionHeld bool
	// MappedRole is the role the identity provider's groups gave the user.
	MappedRole Role
}

// LoginOIDC signs in an identity the OIDC provider vouched for and starts a
// session. The rules, in order: single sign-on must be on (ErrOIDCDisabled), the
// email domain filter must let the identity through (ErrDomainNotAllowed), and a
// group mapping or the default role must give it a role (ErrNoRole). The user is
// then found by subject only, never by name or email, or created when
// auto_create_users is on (ErrUnknownExternalUser, ErrAccountConflict). With group
// mappings the role is recomputed on every sign-in; without any, the default role
// is only the role of a new user, and existing users keep theirs (a role an
// administrator set by hand stays).
func (s *Service) LoginOIDC(ctx context.Context, id *ExternalIdentity) (*OIDCLogin, error) {
	policy := s.OIDC()
	if !policy.Enabled {
		return nil, ErrOIDCDisabled
	}
	if id == nil || id.Issuer == "" || id.Subject == "" || len(id.Issuer) > maxIssuerLength || len(id.Subject) > maxSubjectLength {
		return nil, ErrInvalidIdentity
	}
	if !policy.EmailDomainAllowed(id.Email, id.EmailVerified) {
		return nil, ErrDomainNotAllowed
	}
	role := policy.MapRole(id.Groups)
	// Without mappings the role is the default role of new users only; whether the
	// identity has a user (and so may sign in without one) is decided by the store.
	onCreate := len(policy.RoleMappings) == 0
	if role == "" && !onCreate {
		return nil, ErrNoRole
	}
	username := strings.TrimSpace(id.Username)
	if err := ValidateUsername(username); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidIdentity, err)
	}
	userID, err := newID("usr_")
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	// With the two-person rule an administrator keeps the admin role, and the
	// demotion waits for a second administrator.
	gate := s.holdsAdminGrant(ctx)
	res, err := s.repo.SignInExternalUser(ctx, &ExternalSignIn{
		Subject: ExternalSubject(id.Issuer, id.Subject), NewUserID: userID, Username: username,
		Role: role, RoleOnCreate: onCreate, Connections: policy.MapConnections(id.Groups, role),
		AutoCreate: policy.AutoCreateUsers, KeepAdmin: gate != nil, At: now,
	})
	if err != nil {
		return nil, err
	}
	if res.DemotionHeld && gate != nil {
		if reqErr := gate.RequestSignInDemotion(ctx, AdminGrant{Kind: DemoteAdmin, UserID: res.User.ID, Username: res.User.Username, Role: role}); reqErr != nil {
			// The admin role stays either way; the warning tells the administrators.
			s.logger.Warn("could not ask for approval of a single sign-on demotion", slog.String("user_id", res.User.ID), logsafe.Error(reqErr))
		}
	}
	idle, _ := s.sessionTimeouts()
	if n, purgeErr := s.repo.DeleteExpiredSessions(ctx, now, idle); purgeErr != nil {
		s.logger.Warn("failed to purge expired sessions", logsafe.Error(purgeErr))
	} else if n > 0 {
		s.logger.Debug("purged expired sessions", slog.Int("count", n))
	}
	session, err := s.startSession(ctx, res.User)
	if err != nil {
		return nil, err
	}
	out := &OIDCLogin{LoginResult: session, Created: res.Created, RoleFrom: res.RoleFrom, RoleTo: res.User.Role, RoleKept: res.RoleKept,
		DemotionHeld: res.DemotionHeld, MappedRole: role}
	s.logger.Info("single sign-on", slog.String("user_id", res.User.ID), logsafe.Attr("username", res.User.Username),
		slog.Bool("created", res.Created), logsafe.Attr("role", string(res.User.Role)))
	switch {
	case res.DemotionHeld:
		s.logger.Warn("single sign-on would demote an administrator; the admin role stays until a second administrator approves",
			slog.String("user_id", res.User.ID), logsafe.Attr("mapped_role", string(role)))
	case res.RoleKept:
		s.logger.Warn("single sign-on would demote the last administrator; the stored role was kept",
			slog.String("user_id", res.User.ID), logsafe.Attr("mapped_role", string(role)))
	}
	return out, nil
}

// OIDC attempt budget per client address: each start of a sign-in and each
// callback counts, and more than OIDCAttemptsPerWindow within OIDCAttemptWindow are
// refused with a *ThrottledError.
const (
	OIDCAttemptsPerWindow = 30
	OIDCAttemptWindow     = time.Minute
)

// AllowOIDCAttempt counts one single sign-on request (kind "start" or "callback")
// of clientIP and returns a *ThrottledError once the per-address budget is spent.
func (s *Service) AllowOIDCAttempt(kind, clientIP string) error {
	if wait := s.throttle.CountFailure("oidc|"+kind+"|"+clientIP, OIDCAttemptsPerWindow, OIDCAttemptWindow); wait > 0 {
		return &ThrottledError{RetryAfter: wait}
	}
	return nil
}

// CountLocalAdmins returns the number of local users with the admin role, the
// users who can still sign in when the identity provider cannot.
func (s *Service) CountLocalAdmins(ctx context.Context) (int, error) {
	return s.repo.CountLocalAdmins(ctx)
}

// CheckOIDCChange enforces the break-glass rule for a change of the single sign-on
// settings: turning single sign-on on, or limiting the password form to local
// administrators, needs at least one local administrator (ErrLastLocalAdmin).
// The caller must check it again in the transaction that stores the change
// (OIDCChangeNeedsLocalAdmin says when).
func (s *Service) CheckOIDCChange(ctx context.Context, prev, next OIDCPolicy) error {
	if !OIDCChangeNeedsLocalAdmin(prev, next) {
		return nil
	}
	n, err := s.repo.CountLocalAdmins(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: create a local administrator first", ErrLastLocalAdmin)
	}
	return nil
}

// OIDCChangeNeedsLocalAdmin reports whether a change of the single sign-on policy
// needs a local administrator: it turns single sign-on on, or limits the password
// form to local administrators.
func OIDCChangeNeedsLocalAdmin(prev, next OIDCPolicy) bool {
	enabling := next.Enabled && !prev.Enabled
	limiting := next.LocalLogin == LocalLoginAdminsOnly && prev.LocalLogin != LocalLoginAdminsOnly
	return enabling || limiting
}

// ApplyOIDCChange runs the effects of a stored change of the single sign-on
// settings: when the password form becomes limited to local administrators, the
// sessions of local users without the admin role end.
func (s *Service) ApplyOIDCChange(ctx context.Context, prev, next OIDCPolicy) error {
	if !next.AdminsOnly() || prev.AdminsOnly() {
		return nil
	}
	n, err := s.repo.DeleteLocalNonAdminSessions(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		s.logger.Info("password sign-in limited to local administrators; ended the sessions of other local users", slog.Int("count", n))
	}
	return nil
}

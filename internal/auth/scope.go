package auth

import (
	"context"
	"errors"
	"fmt"
)

// Scope limits what a caller may do. Scopes are ordered: every scope includes the
// rights of the ones below it (read < operator < admin). API keys carry a scope;
// signed-in users get the scope of their dashboard role (see Role.Scope).
type Scope string

// API key scopes.
const (
	// ScopeRead allows reading: every GET of the REST API and the read-only MCP tools.
	ScopeRead Scope = "read"
	// ScopeOperator adds starting backups, running jobs and safe-clone restores.
	ScopeOperator Scope = "operator"
	// ScopeAdmin allows everything, including deletions, in-place restores, settings,
	// users, API keys, connections and storage targets.
	ScopeAdmin Scope = "admin"
)

// Scope errors.
var (
	// ErrForbidden is returned when the caller's scope does not include the scope an
	// operation requires. It is wrapped in a *ScopeError naming both scopes.
	ErrForbidden = errors.New("auth: insufficient scope")
	// ErrInvalidScope is returned for an unknown scope name.
	ErrInvalidScope = errors.New("auth: invalid scope")
)

// ScopeSource names what limits a caller's scope, so refusals can say why.
type ScopeSource string

// Scope sources.
const (
	// SourceRole: a signed-in user, limited by their dashboard role.
	SourceRole ScopeSource = "role"
	// SourceKey: an API key, limited by its own scope.
	SourceKey ScopeSource = "key"
	// SourceKeyCappedByRole: an API key whose scope is above its creator's current
	// role, limited by that role.
	SourceKeyCappedByRole ScopeSource = "key_capped_by_role"
)

// ScopeError reports which scope an operation required and which the caller had.
type ScopeError struct {
	// Have is the caller's effective scope ("" when there is no authenticated caller).
	Have Scope
	// Need is the scope the operation requires.
	Need Scope
	// Source is what limits Have ("" for the system or no caller).
	Source ScopeSource
	// Role is the dashboard role behind Have (SourceRole and SourceKeyCappedByRole).
	Role Role
	// KeyScope is the API key's own scope (SourceKey and SourceKeyCappedByRole).
	KeyScope Scope
}

// Error implements error.
func (e *ScopeError) Error() string {
	return fmt.Sprintf("%s: %s", ErrForbidden, e.Message())
}

// Message describes the refusal for API clients, without the error prefix.
func (e *ScopeError) Message() string {
	switch {
	case e.Have == "":
		return fmt.Sprintf("this request needs the %q scope", e.Need)
	case e.Source == SourceRole:
		return fmt.Sprintf("your role (%s) has the %q scope; this request needs %q", e.Role, e.Have, e.Need)
	case e.Source == SourceKeyCappedByRole:
		return fmt.Sprintf("this API key has the %q scope, capped at %q by its creator's role (%s); this request needs %q",
			e.KeyScope, e.Have, e.Role, e.Need)
	default:
		return fmt.Sprintf("this API key has the %q scope; this request needs %q", e.Have, e.Need)
	}
}

// Unwrap makes errors.Is(err, ErrForbidden) work.
func (e *ScopeError) Unwrap() error { return ErrForbidden }

// Scopes lists every scope from the least to the most privileged.
func Scopes() []Scope { return []Scope{ScopeRead, ScopeOperator, ScopeAdmin} }

// rank orders scopes; unknown scopes rank below read and allow nothing.
func (s Scope) rank() int {
	switch s {
	case ScopeRead:
		return 1
	case ScopeOperator:
		return 2
	case ScopeAdmin:
		return 3
	default:
		return 0
	}
}

// Valid reports whether s is a known scope.
func (s Scope) Valid() bool { return s.rank() > 0 }

// Allows reports whether s includes need. An unknown scope allows nothing, and an
// unknown need is allowed by no scope.
func (s Scope) Allows(need Scope) bool {
	return s.Valid() && need.Valid() && s.rank() >= need.rank()
}

// ParseScope returns the scope named name. An empty name is the least privileged
// scope, ScopeRead, so that a key created without a scope can never do more than
// read.
func ParseScope(name string) (Scope, error) {
	if name == "" {
		return ScopeRead, nil
	}
	s := Scope(name)
	if !s.Valid() {
		return "", fmt.Errorf("%w: %q (use read, operator or admin)", ErrInvalidScope, name)
	}
	return s, nil
}

// Allows reports whether the principal's scope includes need. A nil principal allows
// nothing.
func (p *Principal) Allows(need Scope) bool {
	return p != nil && p.Scope.Allows(need)
}

// Require returns nil when the principal's scope includes need, and a *ScopeError
// (wrapping ErrForbidden) otherwise. A nil principal is refused.
func (p *Principal) Require(need Scope) error {
	if p.Allows(need) {
		return nil
	}
	if p == nil {
		return &ScopeError{Need: need}
	}
	return &ScopeError{Have: p.Scope, Need: need, Source: p.ScopeSource(), Role: p.Role, KeyScope: p.KeyScope}
}

// ScopeSource reports what limits the principal's scope: its role for sessions, the
// key's scope for API keys, or the creator's role for keys it caps. It is "" for
// the system and a nil principal.
func (p *Principal) ScopeSource() ScopeSource {
	if p == nil {
		return ""
	}
	switch p.Method {
	case MethodSession:
		return SourceRole
	case MethodAPIKey:
		if p.KeyScope.rank() > p.Scope.rank() {
			return SourceKeyCappedByRole
		}
		return SourceKey
	}
	return ""
}

// Access is what a principal may do, as computed by effectiveAccess: its scope and
// the connections it may touch.
type Access struct {
	// Scope is the effective scope.
	Scope Scope
	// Connections limits the caller to some connections; nil allows every one (see
	// effectiveConnections).
	Connections ConnectionSet
}

// effectiveScope is the one place that decides what a caller may do:
//   - a session has the scope of the user's role;
//   - an API key with a creator has the lower of its own scope and the creator's
//     current role, recomputed on every request, so demoting a user caps their keys;
//   - an API key without a creator (imported from MONGORESCUE_API_KEY, or created
//     by the system) has its own scope;
//   - the system is admin.
//
// Unknown scopes and roles fail closed to read.
func effectiveScope(method Method, role Role, keyScope Scope, hasCreator bool) Access {
	switch method {
	case MethodSession:
		return Access{Scope: role.Scope()}
	case MethodAPIKey:
		if !keyScope.Valid() {
			keyScope = ScopeRead
		}
		if hasCreator {
			return Access{Scope: minScope(keyScope, role.Scope())}
		}
		return Access{Scope: keyScope}
	case MethodSystem:
		return Access{Scope: ScopeAdmin}
	}
	return Access{Scope: ScopeRead}
}

// minScope returns the less privileged of a and b.
func minScope(a, b Scope) Scope {
	if a.rank() <= b.rank() {
		return a
	}
	return b
}

// RequireScope checks the principal stored in ctx (see WithPrincipal) against need.
// A context without a principal is refused: callers that act on behalf of the system
// (the scheduler, tests) must attach a principal explicitly, for example SystemPrincipal.
func RequireScope(ctx context.Context, need Scope) error {
	return PrincipalFrom(ctx).Require(need)
}

// SystemPrincipal returns an admin principal for operations the application performs
// on its own behalf (for example start-up tasks). It has no user and no API key.
func SystemPrincipal() *Principal {
	return &Principal{Method: MethodSystem, Scope: ScopeAdmin}
}

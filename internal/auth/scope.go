package auth

import (
	"context"
	"errors"
	"fmt"
)

// Scope limits what an API key may do. Scopes are ordered: every scope includes the
// rights of the ones below it (read < operator < admin).
type Scope string

// API key scopes.
const (
	// ScopeRead allows reading: every GET of the REST API and the read-only MCP tools.
	ScopeRead Scope = "read"
	// ScopeOperator adds starting backups, running jobs and safe-clone restores.
	ScopeOperator Scope = "operator"
	// ScopeAdmin allows everything, including deletions, in-place restores, settings,
	// users, API keys, connections and storage targets. Browser sessions are admin.
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

// ScopeError reports which scope an operation required and which the caller had.
type ScopeError struct {
	// Have is the caller's scope ("" when there is no authenticated caller).
	Have Scope
	// Need is the scope the operation requires.
	Need Scope
}

// Error implements error.
func (e *ScopeError) Error() string {
	if e.Have == "" {
		return fmt.Sprintf("%s: the %q scope is required", ErrForbidden, e.Need)
	}
	return fmt.Sprintf("%s: the %q scope is required, this API key has %q", ErrForbidden, e.Need, e.Have)
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
	have := Scope("")
	if p != nil {
		have = p.Scope
	}
	return &ScopeError{Have: have, Need: need}
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

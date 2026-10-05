package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Connection access errors.
var (
	// ErrInvalidConnections is returned for a malformed list of connection IDs.
	ErrInvalidConnections = errors.New("auth: invalid connection_ids")
	// ErrAdminConnections is returned when a connection limit is set on an
	// administrator or an admin-scope API key: administrators always reach every
	// connection.
	ErrAdminConnections = errors.New("auth: administrators and admin API keys always have access to every connection")
	// ErrConnectionsExceedAccess is returned when an API key would reach a
	// connection its creator cannot.
	ErrConnectionsExceedAccess = errors.New("auth: an API key cannot reach connections its creator cannot")
	// ErrConnectionsLimited is returned for a request that covers every connection
	// (the Prometheus scrape) from a caller limited to some connections. It wraps
	// ErrForbidden.
	ErrConnectionsLimited = fmt.Errorf("%w: this caller is limited to some connections and this request covers every connection", ErrForbidden)
)

// Limits of a connection list.
const (
	// MaxConnectionIDs is the most connection IDs a user or API key may be limited to.
	MaxConnectionIDs = 256
	// maxConnectionIDLength bounds one connection ID.
	maxConnectionIDLength = 128
)

// ConnectionSet is the set of managed MongoDB connections a principal may touch. A
// nil set allows every connection (the default, and the only access of
// administrators); a non-nil set, even an empty one, allows only its members.
type ConnectionSet map[string]struct{}

// NewConnectionSet returns the set of ids, or nil (every connection) when ids is
// empty: a user or key stored without connection IDs is not limited.
func NewConnectionSet(ids []string) ConnectionSet {
	if len(ids) == 0 {
		return nil
	}
	return OnlyConnections(ids...)
}

// OnlyConnections returns a limited set of exactly ids; with no ids it allows no
// connection at all.
func OnlyConnections(ids ...string) ConnectionSet {
	s := make(ConnectionSet, len(ids))
	for _, id := range ids {
		s[id] = struct{}{}
	}
	return s
}

// Limited reports whether s allows only some connections.
func (s ConnectionSet) Limited() bool { return s != nil }

// Allows reports whether s includes connection id. An unlimited set allows every
// ID; a limited one never allows the empty ID (a record without a connection).
func (s ConnectionSet) Allows(id string) bool {
	if s == nil {
		return true
	}
	if id == "" {
		return false
	}
	_, ok := s[id]
	return ok
}

// IDs returns the sorted members of s, or nil when s is unlimited. A limited set
// without members returns an empty, non-nil slice.
func (s ConnectionSet) IDs() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s))
	for id := range s {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// Intersect returns the connections both s and o allow.
func (s ConnectionSet) Intersect(o ConnectionSet) ConnectionSet {
	switch {
	case s == nil:
		return o
	case o == nil:
		return s
	}
	out := make(ConnectionSet)
	for id := range s {
		if _, ok := o[id]; ok {
			out[id] = struct{}{}
		}
	}
	return out
}

// Covers reports whether every connection o allows is allowed by s.
func (s ConnectionSet) Covers(o ConnectionSet) bool {
	if s == nil {
		return true
	}
	if o == nil {
		return false
	}
	for id := range o {
		if _, ok := s[id]; !ok {
			return false
		}
	}
	return true
}

// NormalizeConnectionIDs trims, de-duplicates and sorts ids. It returns
// ErrInvalidConnections for an empty or overlong ID, an ID with control characters
// or more than MaxConnectionIDs IDs. An empty list stays empty (nil): every
// connection.
func NormalizeConnectionIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > MaxConnectionIDs {
		return nil, fmt.Errorf("%w: at most %d connections", ErrInvalidConnections, MaxConnectionIDs)
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || len(id) > maxConnectionIDLength ||
			strings.ContainsFunc(id, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return nil, fmt.Errorf("%w: %q is not a connection ID", ErrInvalidConnections, id)
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// AllowsConnection reports whether the principal may touch connection id: read it,
// back it up, restore from or into it, or see the records taken from it. A nil
// principal allows nothing.
func (p *Principal) AllowsConnection(id string) bool {
	return p != nil && p.Connections.Allows(id)
}

// RequireKeyCreator returns nil when the principal may create API keys: a signed-in
// user of any role, an admin-scope key or the system. A nil principal gets
// ErrUnauthenticated, any other caller a *ScopeError.
func (p *Principal) RequireKeyCreator() error {
	if p == nil {
		return ErrUnauthenticated
	}
	if p.Method == MethodSession {
		return nil
	}
	return p.Require(ScopeAdmin)
}

// RequireAllConnections refuses (ErrConnectionsLimited) a principal limited to some
// connections, for requests whose answer covers every connection, such as the
// Prometheus metrics. A nil principal is refused with a *ScopeError.
func (p *Principal) RequireAllConnections() error {
	if p == nil {
		return &ScopeError{Need: ScopeRead}
	}
	if p.Connections.Limited() {
		return ErrConnectionsLimited
	}
	return nil
}

// ConnectionFilter returns the connections the principal in ctx may touch: nil
// (every connection) for unlimited principals and for a context without a principal.
// Every request carries a principal (the routes and MCP tools require one), so a
// context without one is the application acting on its own behalf, for example the
// scheduler; scope checks (RequireScope) refuse such contexts where they apply.
func ConnectionFilter(ctx context.Context) ConnectionSet {
	if p := PrincipalFrom(ctx); p != nil {
		return p.Connections
	}
	return nil
}

// ConnectionAllowed reports whether the caller in ctx may touch connection id (see
// ConnectionFilter).
func ConnectionAllowed(ctx context.Context, id string) bool {
	return ConnectionFilter(ctx).Allows(id)
}

// effectiveConnections decides which connections a caller may touch, next to
// effectiveScope:
//   - a session: the user's connections, every connection for an administrator;
//   - an API key with a creator: the key's connections within its creator's current
//     ones (an administrator creator does not narrow the key);
//   - an API key without a creator, and the system: every connection.
func effectiveConnections(method Method, role Role, hasCreator bool, user, key ConnectionSet) ConnectionSet {
	switch method {
	case MethodSession:
		if role == RoleAdmin {
			return nil
		}
		return user
	case MethodAPIKey:
		if !hasCreator {
			return nil
		}
		if role == RoleAdmin {
			return key
		}
		return key.Intersect(user)
	}
	return nil
}

// effectiveAccess combines effectiveScope and effectiveConnections. A caller limited
// to some connections never holds the admin scope (users, settings, connections and
// storage targets are global), so it is capped at operator.
func effectiveAccess(method Method, role Role, keyScope Scope, hasCreator bool, user, key ConnectionSet) Access {
	a := effectiveScope(method, role, keyScope, hasCreator)
	a.Connections = effectiveConnections(method, role, hasCreator, user, key)
	if a.Connections.Limited() && a.Scope == ScopeAdmin {
		a.Scope = ScopeOperator
	}
	return a
}

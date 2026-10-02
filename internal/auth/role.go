package auth

import (
	"errors"
	"fmt"
)

// Role is the dashboard role of a user: what a signed-in user may do, and the
// ceiling of every API key the user creates. It is unrelated to the MongoDB users
// and roles a dump may contain.
type Role string

// Dashboard roles, from the least to the most privileged.
const (
	// RoleViewer may view everything a read key may read.
	RoleViewer Role = "viewer"
	// RoleOperator may also start backups, run jobs and restore into safe clones.
	RoleOperator Role = "operator"
	// RoleAdmin may do everything, including users, settings and deletions.
	RoleAdmin Role = "admin"
)

// Role errors.
var (
	// ErrInvalidRole is returned for an unknown role name.
	ErrInvalidRole = errors.New("auth: invalid role")
	// ErrLastAdmin is returned when a change would leave no administrator: demoting
	// or deleting the last user with the admin role.
	ErrLastAdmin = errors.New("auth: the last administrator cannot be demoted or deleted")
	// ErrChangeOwnRole is returned when users try to change their own role.
	ErrChangeOwnRole = errors.New("auth: cannot change your own role")
	// ErrScopeExceedsRole is returned when an API key would get a scope above what its
	// creator may do.
	ErrScopeExceedsRole = errors.New("auth: an API key cannot have more rights than its creator")
)

// Roles lists every role from the least to the most privileged.
func Roles() []Role { return []Role{RoleViewer, RoleOperator, RoleAdmin} }

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleViewer, RoleOperator, RoleAdmin:
		return true
	}
	return false
}

// Scope returns the scope the role grants: viewer read, operator operator and admin
// admin. An unknown or empty role grants read only (fail closed).
func (r Role) Scope() Scope {
	switch r {
	case RoleOperator:
		return ScopeOperator
	case RoleAdmin:
		return ScopeAdmin
	default:
		return ScopeRead
	}
}

// ParseRole returns the role named name, or ErrInvalidRole for an empty or unknown
// name.
func ParseRole(name string) (Role, error) {
	r := Role(name)
	if !r.Valid() {
		return "", fmt.Errorf("%w: %q (use viewer, operator or admin)", ErrInvalidRole, name)
	}
	return r, nil
}

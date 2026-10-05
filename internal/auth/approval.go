package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
)

// Errors of the two-person rule (security.require_second_approver).
var (
	// ErrApprovalNeedsSession is returned when an API key (or the system) tries to
	// approve a destructive action: approvals need a signed-in administrator.
	ErrApprovalNeedsSession = errors.New("auth: approvals need a signed-in administrator; API keys can request but never approve")
	// ErrSelfApproval is returned when the requester of an action tries to approve
	// it: a second, different administrator must.
	ErrSelfApproval = errors.New("auth: the requester of an action cannot approve it; another administrator must")
	// ErrTooFewAdmins is returned when the two-person rule is turned on while fewer
	// than MinApprovalAdmins administrators exist.
	ErrTooFewAdmins = errors.New("auth: the two-person rule needs at least two administrators")
	// ErrApproverTooRecent is returned when an administrator who got the admin role
	// at or after the time a request was made tries to approve it: a freshly created
	// or promoted administrator can never approve a request that predates them.
	ErrApproverTooRecent = errors.New("auth: only an administrator who was one before the request was made can approve it")
	// ErrAwaitingApproval is wrapped by the error an AdminGrantGate returns once it
	// stored the request: the grant waits for a second administrator. Any other
	// error means no request exists.
	ErrAwaitingApproval = errors.New("auth: the change waits for a second administrator")
)

// rollbackUnrequested undoes what a held grant created (undo) when gate could not
// store the approval request (err does not wrap ErrAwaitingApproval), so a failed
// request leaves no viewer account or operator key behind. It returns err.
func (s *Service) rollbackUnrequested(ctx context.Context, err error, what string, undo func(context.Context) error) error {
	if err == nil || errors.Is(err, ErrAwaitingApproval) {
		return err
	}
	if undoErr := undo(context.WithoutCancel(ctx)); undoErr != nil {
		s.logger.Error("could not remove the "+what+" of a failed approval request", logsafe.Error(undoErr))
		return errors.Join(err, undoErr)
	}
	return err
}

// AdminGrantKind names a grant of admin rights.
type AdminGrantKind string

// Admin grants the two-person rule holds back.
const (
	// GrantAdminRole gives user UserID the admin role (creating an admin, or a
	// promotion).
	GrantAdminRole AdminGrantKind = "user_role"
	// GrantAdminKey gives API key KeyID the admin scope.
	GrantAdminKey AdminGrantKind = "api_key"
	// ResetUserPassword sets another user's password (PasswordHash): whoever chose it
	// could sign in as that user.
	ResetUserPassword AdminGrantKind = "password_reset"
	// DemoteAdmin changes administrator UserID to Role.
	DemoteAdmin AdminGrantKind = "demote_admin"
	// DeleteAdmin deletes administrator UserID.
	DeleteAdmin AdminGrantKind = "delete_admin"
)

// AdminGrant is a grant of admin rights that waits for a second administrator.
type AdminGrant struct {
	// Kind is what is granted.
	Kind AdminGrantKind
	// UserID and Username name the user of GrantAdminRole.
	UserID, Username string
	// KeyID and KeyName name the key of GrantAdminKey.
	KeyID, KeyName string
	// Created reports that the user or key was created by this request (with a lower
	// role or scope until the grant is approved).
	Created bool
	// Role is the new role of DemoteAdmin.
	Role Role
	// PasswordHash is the bcrypt hash of the new password of ResetUserPassword.
	PasswordHash string
}

// ApplyPasswordReset sets the password hash of userID chosen by another user: the
// approved half of a password reset while the two-person rule is on. It needs the
// admin scope; the user's sessions end and their RoleChangedAt moves to now.
func (s *Service) ApplyPasswordReset(ctx context.Context, actor *Principal, userID, hash string) error {
	if err := actor.Require(ScopeAdmin); err != nil {
		return err
	}
	if g := s.holdsAdminGrant(ctx); g != nil {
		return g.RequestAdminGrant(ctx, AdminGrant{Kind: ResetUserPassword, UserID: userID, PasswordHash: hash})
	}
	if err := s.repo.ResetPassword(ctx, userID, hash, s.now().UTC()); err != nil {
		return err
	}
	s.logger.Info("password reset", slog.String("user_id", userID), slog.String("by", actor.UserID()))
	return nil
}

// AdminGrantGate holds back grants of admin rights while the two-person rule is on
// (implemented by the operations service): otherwise one stolen administrator
// credential could create a second administrator and approve its own requests.
type AdminGrantGate interface {
	// HoldsAdminGrants reports whether an admin grant in ctx must wait for a second
	// administrator.
	HoldsAdminGrants(ctx context.Context) bool
	// RequestAdminGrant records the request for g and returns the error the caller
	// returns (the operations service's *ApprovalPendingError).
	RequestAdminGrant(ctx context.Context, g AdminGrant) error
	// RequestSignInDemotion asks a second administrator to apply g.Role, the role a
	// single sign-on gave administrator g.UserID, who kept the admin role (at most
	// one open request per user and role). It returns nil once the request exists.
	RequestSignInDemotion(ctx context.Context, g AdminGrant) error
}

// ApplyProviderRole sets the role of single sign-on user id to role, the role the
// identity provider's groups gave them: the approved half of a demotion that a
// sign-in held back while the two-person rule is on. It needs the admin scope; it
// refuses local users (ErrInvalidRole) and follows the last-admin rules of
// SetUserRole, and the user's sessions end.
func (s *Service) ApplyProviderRole(ctx context.Context, actor *Principal, id string, role Role) (*RoleChange, error) {
	if err := actor.Require(ScopeAdmin); err != nil {
		return nil, err
	}
	role, err := ParseRole(string(role))
	if err != nil {
		return nil, err
	}
	target, err := s.repo.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	if target.Local() {
		return nil, fmt.Errorf("%w: %s is not a single sign-on user", ErrInvalidRole, target.Username)
	}
	if g := s.holdsAdminGrant(ctx); g != nil && target.Role == RoleAdmin && role != RoleAdmin {
		return nil, g.RequestAdminGrant(ctx, AdminGrant{Kind: DemoteAdmin, UserID: target.ID, Username: target.Username, Role: role})
	}
	from, err := s.repo.UpdateUserRole(ctx, actor.UserID(), id, role, s.now().UTC(), s.OIDC().Enabled)
	if err != nil {
		return nil, err
	}
	user, err := s.repo.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	s.logger.Info("single sign-on role applied", slog.String("user_id", user.ID), logsafe.Attr("role_from", string(from)),
		logsafe.Attr("role_to", string(role)), slog.String("by", actor.UserID()))
	return &RoleChange{User: user, From: from}, nil
}

// SetAdminGrantGate makes CreateUser, SetUserRole and CreateAPIKey hold back admin
// grants through g. Set it while wiring, before requests are served.
func (s *Service) SetAdminGrantGate(g AdminGrantGate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grantGate = g
}

// holdsAdminGrant returns the gate when an admin grant in ctx must wait.
func (s *Service) holdsAdminGrant(ctx context.Context) AdminGrantGate {
	s.mu.Lock()
	g := s.grantGate
	s.mu.Unlock()
	if g == nil || !g.HoldsAdminGrants(ctx) {
		return nil
	}
	return g
}

// GrantAPIKeyAdmin raises key id to the admin scope: the approved half of creating
// an admin-scope key while the two-person rule is on. It needs the admin scope, and
// while the rule holds admin grants in ctx it asks for an approval instead.
func (s *Service) GrantAPIKeyAdmin(ctx context.Context, actor *Principal, id string) (*APIKey, error) {
	if err := actor.Require(ScopeAdmin); err != nil {
		return nil, err
	}
	k, err := s.findAPIKey(ctx, id)
	if err != nil {
		return nil, err
	}
	if g := s.holdsAdminGrant(ctx); g != nil {
		return nil, g.RequestAdminGrant(ctx, AdminGrant{Kind: GrantAdminKey, KeyID: k.ID, KeyName: k.Name})
	}
	if err := s.repo.UpdateAPIKeyScope(ctx, id, ScopeAdmin); err != nil {
		return nil, err
	}
	k.Scope = ScopeAdmin
	s.logger.Info("api key raised to the admin scope", slog.String("api_key_id", k.ID), slog.String("by", actor.UserID()))
	return k, nil
}

// MinApprovalAdmins is the number of administrators the two-person rule needs.
const MinApprovalAdmins = 2

// CheckApprover returns nil when p may approve an action that the user
// requesterUserID requested ("" for an API key without a user or the system) at
// requestedAt: p must be a signed-in session (ErrApprovalNeedsSession otherwise) of
// an administrator (a *ScopeError wrapping ErrForbidden otherwise) other than the
// requester (ErrSelfApproval otherwise) who already had the admin role before
// requestedAt (ErrApproverTooRecent otherwise). A zero requestedAt skips the last
// check (to tell early whether p may approve anything at all).
func CheckApprover(p *Principal, requesterUserID string, requestedAt time.Time) error {
	if p == nil || p.Method != MethodSession || p.User == nil {
		return ErrApprovalNeedsSession
	}
	if err := p.Require(ScopeAdmin); err != nil {
		return err
	}
	if requesterUserID != "" && p.User.ID == requesterUserID {
		return ErrSelfApproval
	}
	if !requestedAt.IsZero() && (p.User.RoleChangedAt.IsZero() || !p.User.RoleChangedAt.Before(requestedAt)) {
		return ErrApproverTooRecent
	}
	return nil
}

// CountAdmins returns the number of users with the admin role.
func CountAdmins(users []*User) int {
	n := 0
	for _, u := range users {
		if u != nil && u.Role == RoleAdmin {
			n++
		}
	}
	return n
}

// CheckSecondApproverPossible returns ErrTooFewAdmins unless at least
// MinApprovalAdmins administrators exist, so the two-person rule never locks every
// destructive action behind an approval nobody can give.
func (s *Service) CheckSecondApproverPossible(ctx context.Context) error {
	users, err := s.repo.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("auth: list users: %w", err)
	}
	if n := CountAdmins(users); n < MinApprovalAdmins {
		return fmt.Errorf("%w (there are %d); add another administrator first", ErrTooFewAdmins, n)
	}
	return nil
}

package auth

import (
	"context"
	"errors"
	"fmt"
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
)

// MinApprovalAdmins is the number of administrators the two-person rule needs.
const MinApprovalAdmins = 2

// CheckApprover returns nil when p may approve an action that the user
// requesterUserID requested ("" for an API key without a user or the system): p
// must be a signed-in session (ErrApprovalNeedsSession otherwise) of an administrator
// (a *ScopeError wrapping ErrForbidden otherwise) other than the requester
// (ErrSelfApproval otherwise).
func CheckApprover(p *Principal, requesterUserID string) error {
	if p == nil || p.Method != MethodSession || p.User == nil {
		return ErrApprovalNeedsSession
	}
	if err := p.Require(ScopeAdmin); err != nil {
		return err
	}
	if requesterUserID != "" && p.User.ID == requesterUserID {
		return ErrSelfApproval
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

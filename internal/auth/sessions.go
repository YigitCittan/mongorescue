package auth

import (
	"context"
	"log/slog"
	"time"
)

const (
	// sessionIDPrefix starts every public session ID.
	sessionIDPrefix = "ses_"
	// sessionIDLen is the number of hex characters of a session ID after the prefix.
	sessionIDLen = 24
)

// SessionInfo describes a session for the session list. It never carries the token,
// its hash or the CSRF token: the ID is derived one-way from the token hash, so it
// can neither authenticate nor be turned back into the hash.
type SessionInfo struct {
	// ID identifies the session for revocation ("ses_" + hex).
	ID string `json:"id"`
	// UserID is the owner.
	UserID string `json:"user_id"`
	// Username is the owner's name.
	Username string `json:"username,omitempty"`
	// CreatedAt is the login time.
	CreatedAt time.Time `json:"created_at"`
	// LastSeenAt is the last authenticated request, at most a minute behind.
	LastSeenAt time.Time `json:"last_seen_at"`
	// ExpiresAt is when the session ends at the latest under the current idle and
	// absolute timeouts.
	ExpiresAt time.Time `json:"expires_at"`
	// Current marks the session the request itself was made with.
	Current bool `json:"current"`
}

// sessionID derives the public ID of the session with tokenHash.
func sessionID(tokenHash string) string {
	return sessionIDPrefix + HashToken("mongorescue-session-id\x00" + tokenHash)[:sessionIDLen]
}

// sessionExpired reports whether sess has passed its absolute expiry, the current
// absolute timeout measured from its creation (so shortening the timeout applies to
// sessions that already exist) or the idle timeout.
func (s *Service) sessionExpired(sess *Session, now time.Time) bool {
	idle, absolute := s.sessionTimeouts()
	return !now.Before(sess.ExpiresAt) || !now.Before(sess.CreatedAt.Add(absolute)) || now.Sub(sess.LastSeenAt) >= idle
}

// sessionEnd returns when sess ends at the latest under the current timeouts.
func (s *Service) sessionEnd(sess *Session) time.Time {
	idle, absolute := s.sessionTimeouts()
	end := sess.ExpiresAt
	for _, t := range []time.Time{sess.CreatedAt.Add(absolute), sess.LastSeenAt.Add(idle)} {
		if t.Before(end) {
			end = t
		}
	}
	return end
}

// sessionOwner returns whose sessions actor may list or revoke: every user's ("")
// with all, which needs the admin scope, otherwise actor's own. ok is false for an
// actor without a user asking for its own sessions (the imported static key).
func sessionOwner(actor *Principal, all bool) (userID string, ok bool, err error) {
	if all {
		if err := actor.Require(ScopeAdmin); err != nil {
			return "", false, err
		}
		return "", true, nil
	}
	if actor.UserID() == "" {
		return "", false, nil
	}
	return actor.UserID(), true, nil
}

// ListSessions returns the live sessions of actor's own user, most recently active
// first, or with all those of every user, which needs the admin scope. Expired
// sessions not yet cleaned up are left out.
func (s *Service) ListSessions(ctx context.Context, actor *Principal, all bool) ([]*SessionInfo, error) {
	owner, ok, err := sessionOwner(actor, all)
	if err != nil {
		return nil, err
	}
	out := []*SessionInfo{}
	if !ok {
		return out, nil
	}
	sessions, err := s.repo.ListSessions(ctx, owner)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	if all {
		users, err := s.repo.ListUsers(ctx)
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			names[u.ID] = u.Username
		}
	} else {
		names[actor.User.ID] = actor.User.Username
	}
	now := s.now().UTC()
	for _, sess := range sessions {
		if s.sessionExpired(sess, now) {
			continue
		}
		out = append(out, &SessionInfo{
			ID:         sessionID(sess.TokenHash),
			UserID:     sess.UserID,
			Username:   names[sess.UserID],
			CreatedAt:  sess.CreatedAt,
			LastSeenAt: sess.LastSeenAt,
			ExpiresAt:  s.sessionEnd(sess),
			Current:    isCurrentSession(actor, sess),
		})
	}
	return out, nil
}

// isCurrentSession reports whether actor is authenticated by sess.
func isCurrentSession(actor *Principal, sess *Session) bool {
	return actor.Method == MethodSession && actor.SessionHash != "" && equalHashes(sess.TokenHash, actor.SessionHash)
}

// RevokeSession ends the session with the public ID id. A signed-in user may end
// the sessions of their own user; ending another user's session needs the admin
// scope, and an API key below admin may end no session at all, not even its
// creator's (a *ScopeError wrapping ErrForbidden). An unknown ID answers
// ErrSessionNotFound. It reports whether the revoked session is the one actor
// is using.
func (s *Service) RevokeSession(ctx context.Context, actor *Principal, id string) (current bool, err error) {
	if err = actor.Require(ScopeRead); err != nil {
		return false, err
	}
	if actor.Method != MethodSession {
		if err = actor.Require(ScopeAdmin); err != nil {
			return false, err
		}
	}
	if len(id) != len(sessionIDPrefix)+sessionIDLen {
		return false, ErrSessionNotFound
	}
	sessions, err := s.repo.ListSessions(ctx, "")
	if err != nil {
		return false, err
	}
	for _, sess := range sessions {
		if sessionID(sess.TokenHash) != id {
			continue
		}
		if own := actor.UserID() != "" && sess.UserID == actor.UserID(); !own {
			if err = actor.Require(ScopeAdmin); err != nil {
				return false, err
			}
		}
		if err := s.repo.DeleteSession(ctx, sess.TokenHash); err != nil {
			return false, err
		}
		s.logger.Info("session revoked", slog.String("session_id", id), slog.String("user_id", sess.UserID),
			slog.String("by", actor.UserID()))
		return isCurrentSession(actor, sess), nil
	}
	return false, ErrSessionNotFound
}

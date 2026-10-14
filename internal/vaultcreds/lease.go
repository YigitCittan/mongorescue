package vaultcreds

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// lease is a dynamic credential issued by Vault. Its fields are guarded by
// Resolver.mu.
type lease struct {
	key string
	ref models.VaultRef
	id  string
	// values holds the issued credentials (username/password, or access_key,
	// secret_key and security_token).
	values map[string]string
	// ttl0 is the TTL first granted; expires when the lease ends unless renewed.
	ttl0      time.Duration
	expires   time.Time
	renewable bool
	// lastUse is when the lease was last handed out or released; lastShared when
	// it was last handed out without a hold (Resolver.Connection).
	lastUse, lastShared time.Time
	holders             map[*holder]struct{}
	// watching is set while keep runs; stop ends it.
	watching bool
	stop     chan struct{}
	// ended is the cause once the lease can no longer be used.
	ended error
}

// holder is one run holding a lease since since.
type holder struct {
	cancel context.CancelCauseFunc
	since  time.Time
}

// margin is how long before its expiry a lease that cannot be renewed ends for
// its holders.
func (l *lease) margin() time.Duration {
	return min(l.ttl0/marginFraction, maxMargin)
}

// fresh reports whether the lease may be handed to a new use at now.
func (l *lease) fresh(now time.Time) bool {
	if l.ended != nil {
		return false
	}
	if l.ttl0 <= 0 {
		return true
	}
	return l.expires.Sub(now) > max(l.ttl0/freshFraction, l.margin())
}

// endLocked ends the lease for its holders with cause (Resolver.mu held).
func (l *lease) endLocked(cause error) {
	if l.ended == nil {
		l.ended = cause
	}
	for h := range l.holders {
		h.cancel(l.ended)
	}
	if l.watching {
		l.watching = false
		close(l.stop)
	}
}

// keyLock returns the mutex that serialises the issuing of key's leases.
func (r *Resolver) keyLock(key string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.keyLocks[key]
	if !ok {
		m = &sync.Mutex{}
		r.keyLocks[key] = m
	}
	return m
}

// shared returns the current lease of ref while it is fresh, or issues a new
// one. A use without a hold (share) is remembered, so the end of a run that
// started earlier does not revoke the lease under it. Idle leases past their
// grace are revoked on the way.
func (r *Resolver) shared(ctx context.Context, ref models.VaultRef, share bool) (*lease, error) {
	key := ref.Key()
	lock := r.keyLock(key)
	lock.Lock()
	defer lock.Unlock()
	r.mu.Lock()
	now := r.now()
	stale := r.sweepLocked(now)
	if l := r.leases[key]; l != nil && l.fresh(now) {
		l.lastUse = now
		if share {
			l.lastShared = now
		}
		r.mu.Unlock()
		r.revokeAll(stale)
		return l, nil
	}
	r.mu.Unlock()
	r.revokeAll(stale)

	l, err := r.issue(ctx, ref)
	if err != nil {
		return nil, err
	}
	l.key = key
	if share {
		l.lastShared = l.lastUse
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		r.revokeAll([]string{l.id})
		return nil, fmt.Errorf("%w: MongoRescue is shutting down", ErrLeaseLost)
	}
	old := r.leases[key]
	r.leases[key] = l
	var drop []string
	if old != nil && len(old.holders) == 0 {
		// Superseded and unused: revoke it now.
		old.endLocked(fmt.Errorf("%w: superseded", ErrLeaseLost))
		drop = append(drop, old.id)
	}
	r.mu.Unlock()
	r.revokeAll(drop)
	return l, nil
}

// sweepLocked removes the idle leases (no holder) past their grace or no longer
// fresh, and returns their IDs to revoke (Resolver.mu held).
func (r *Resolver) sweepLocked(now time.Time) []string {
	var ids []string
	for key, l := range r.leases {
		if len(l.holders) > 0 {
			continue
		}
		if now.Sub(l.lastUse) >= idleGrace || !l.fresh(now) {
			l.endLocked(fmt.Errorf("%w: idle", ErrLeaseLost))
			delete(r.leases, key)
			ids = append(ids, l.id)
		}
	}
	return ids
}

// issue requests new credentials of the dynamic engine of ref.
func (r *Resolver) issue(ctx context.Context, ref models.VaultRef) (*lease, error) {
	var out struct {
		LeaseID       string         `json:"lease_id"`
		LeaseDuration int64          `json:"lease_duration"`
		Renewable     bool           `json:"renewable"`
		Data          map[string]any `json:"data"`
	}
	if err := r.client.Do(ctx, http.MethodGet, "/v1/"+ref.Mount+"/creds/"+ref.Role, nil, &out); err != nil {
		return nil, classify(ref.String(), err)
	}
	fields := []string{"username", "password"}
	if ref.Engine == models.VaultAWS {
		fields = []string{"access_key", "secret_key"}
	}
	vals, err := pick(ref, out.Data, fields)
	if err != nil {
		if out.LeaseID != "" {
			r.revokeAll([]string{out.LeaseID})
		}
		return nil, err
	}
	if tok, ok := out.Data["security_token"].(string); ok && tok != "" {
		vals["security_token"] = tok
	}
	now := r.now()
	ttl := time.Duration(out.LeaseDuration) * r.unit
	return &lease{ref: ref, id: out.LeaseID, values: vals, ttl0: ttl, expires: now.Add(ttl), renewable: out.Renewable && out.LeaseID != "",
		lastUse: now, holders: map[*holder]struct{}{}}, nil
}

// release drops holder h of l. The last holder revokes the lease (best effort),
// unless another use resolved it since h held it, within idleGrace: then the
// lease stays for that use and is revoked by its own run, or swept later.
func (r *Resolver) release(l *lease, h *holder) {
	r.mu.Lock()
	if _, ok := l.holders[h]; !ok {
		r.mu.Unlock()
		return
	}
	delete(l.holders, h)
	now := r.now()
	var drop []string
	if len(l.holders) == 0 {
		if l.watching {
			l.watching = false
			close(l.stop)
		}
		current := r.leases[l.key] == l
		// Shared since this run held it: another run is about to use it.
		sharedSince := l.lastShared.After(h.since) && now.Sub(l.lastShared) < idleGrace
		if !current || l.ended != nil || !sharedSince || r.closed {
			if current {
				delete(r.leases, l.key)
			}
			if l.ended == nil {
				l.ended = fmt.Errorf("%w: released", ErrLeaseLost)
			}
			drop = append(drop, l.id)
		} else {
			l.lastUse = now
		}
	}
	r.mu.Unlock()
	r.revokeAll(drop)
}

// keep renews l while it has holders and ends it for them when it can no longer
// be kept valid. It runs on the Resolver's WaitGroup until stop closes.
func (r *Resolver) keep(l *lease, stop <-chan struct{}) {
	defer r.wg.Done()
	var lastErr error
	for {
		select {
		case <-stop:
			return
		default:
		}
		r.mu.Lock()
		now := r.now()
		remaining := l.expires.Sub(now)
		margin := l.margin()
		var wait time.Duration
		switch {
		case l.ttl0 <= 0:
			// No expiry: nothing to renew.
			r.mu.Unlock()
			select {
			case <-stop:
			case <-r.ctx.Done():
			}
			return
		case lastErr != nil:
			// Retry while the lease is still valid beyond the margin.
			wait = min(max((remaining-margin)/3, r.unit/10), time.Minute)
		case l.renewable:
			wait = remaining - l.ttl0/renewFraction
		default:
			wait = remaining - margin
		}
		renewable := l.renewable
		r.mu.Unlock()

		if lastErr != nil && remaining <= margin {
			cause := fmt.Errorf("%w: renewal of %s failed: %w", ErrLeaseLost, l.ref.String(), lastErr)
			r.end(l, cause)
			r.logger.Warn("a Vault lease could not be renewed and ended for its runs", logsafe.Error(cause))
			return
		}
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-stop:
				t.Stop()
				return
			case <-r.ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		if !renewable {
			r.end(l, fmt.Errorf("%w: the lease of %s is not renewable and ends", ErrLeaseLost, l.ref.String()))
			r.logger.Info("a Vault lease that cannot be renewed ended for its runs", logsafe.Attr("vault", l.ref.String()))
			return
		}
		granted, err := r.renew(r.ctx, l)
		if errors.Is(err, context.Canceled) && r.ctx.Err() != nil {
			return
		}
		if err != nil {
			if lastErr == nil {
				r.logger.Warn("renewal of a Vault lease failed; retrying while it is valid",
					logsafe.Attr("vault", l.ref.String()), logsafe.Error(err))
			}
			lastErr = r.failed(err)
			continue
		}
		if lastErr != nil {
			r.succeeded()
		}
		lastErr = nil
		r.mu.Lock()
		l.expires = r.now().Add(granted)
		short := granted <= l.margin()
		r.mu.Unlock()
		if short {
			// The role's maximum TTL is reached: the lease cannot outlive it.
			r.end(l, fmt.Errorf("%w: the lease of %s reached the maximum TTL of its role; raise max_ttl, or shorten the run",
				ErrLeaseLost, l.ref.String()))
			r.logger.Info("a Vault lease reached its maximum TTL and ended for its runs", logsafe.Attr("vault", l.ref.String()))
			return
		}
	}
}

// end ends l for its holders with cause and drops it from the cache. Their
// release revokes it.
func (r *Resolver) end(l *lease, cause error) {
	r.mu.Lock()
	if r.leases[l.key] == l {
		delete(r.leases, l.key)
	}
	l.endLocked(cause)
	r.mu.Unlock()
}

// renew extends the lease of l by its first TTL and returns the TTL granted.
func (r *Resolver) renew(ctx context.Context, l *lease) (time.Duration, error) {
	var out struct {
		LeaseDuration int64 `json:"lease_duration"`
	}
	increment := int64(l.ttl0 / r.unit)
	err := r.client.Do(ctx, http.MethodPut, "/v1/sys/leases/renew", map[string]any{"lease_id": l.id, "increment": increment}, &out)
	if err != nil {
		return 0, classify("renewal of "+l.ref.String(), err)
	}
	return time.Duration(out.LeaseDuration) * r.unit, nil
}

// revokeAll revokes leases ids in the background, best effort.
func (r *Resolver) revokeAll(ids []string) {
	if len(ids) == 0 || r.client == nil {
		return
	}
	r.mu.Lock()
	closed := r.closed
	if !closed {
		r.wg.Add(1)
	}
	r.mu.Unlock()
	if closed {
		return
	}
	go func() {
		defer r.wg.Done()
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(r.ctx, revokeTimeout)
			r.revoke(ctx, id)
			cancel()
		}
	}()
}

// revoke revokes lease id, logging a failure.
func (r *Resolver) revoke(ctx context.Context, id string) {
	if id == "" {
		return
	}
	if err := r.client.Do(ctx, http.MethodPut, "/v1/sys/leases/revoke", map[string]string{"lease_id": id}, nil); err != nil &&
		!errors.Is(err, context.Canceled) {
		r.logger.Warn("revocation of a Vault lease failed; it ends with its TTL", logsafe.Error(err))
	}
}

// Package sharedcache shares a computed value among the callers that ask for it at
// the same time and for a while after: a time to live (TTL), a change stamp of the
// data it was computed from, and invalidation. Values are keyed by what they depend
// on besides the data, such as the caller's connection access (AccessKey), so a
// caller never gets a value computed for another caller's access.
//
// The operations service shares the overview history and statistics through it, the
// readiness service its report.
package sharedcache

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// DefaultMaxEntries bounds the cached keys when Options.MaxEntries is 0; finished
// entries are dropped when it is reached.
const DefaultMaxEntries = 64

// Options configure a Cache.
type Options struct {
	// TTL is how long a computed value is served again while its stamp is
	// current and nothing invalidated it.
	TTL time.Duration
	// MaxEntries bounds the cached keys (DefaultMaxEntries when 0).
	MaxEntries int
	// ServeStale lets the callers that ask while one caller recomputes a value
	// whose TTL ran out (nothing changed, see Get) have the previous value instead
	// of waiting: a value is then at most TTL plus one computation old.
	// Invalidation and a new stamp never serve the previous value.
	ServeStale bool
}

// AccessKey renders the connection access of a caller (auth.ConnectionFilter) for
// a cache key: "*" for every connection, else the sorted IDs ("" for none).
func AccessKey(set auth.ConnectionSet) string {
	if !set.Limited() {
		return "*"
	}
	ids := set.IDs()
	slices.Sort(ids)
	return strings.Join(ids, ",")
}

// entry is one cached or in-flight value. v, err and at are written before done
// is closed and read only after.
type entry[V any] struct {
	done  chan struct{}
	gen   uint64
	stamp int64
	v     V
	err   error
	at    time.Time
	// prev is the value this computation replaces, served meanwhile with
	// Options.ServeStale. Both are set before the entry is published and never
	// written again.
	prev    V
	hasPrev bool
}

// finished reports whether e's computation ended.
func (e *entry[V]) finished() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// Cache shares the values of keys K. The zero value is not usable; call New. It is
// safe for concurrent use.
type Cache[K comparable, V any] struct {
	opts Options
	now  func() time.Time
	mu   sync.Mutex
	// gen counts invalidations: entries of an older generation are never served.
	gen     uint64
	entries map[K]*entry[V]
}

// New returns an empty Cache.
func New[K comparable, V any](opts Options) *Cache[K, V] {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}
	return &Cache[K, V]{opts: opts, now: time.Now, entries: map[K]*entry[V]{}}
}

// Get returns the value of key at stamp, the change stamp of the data it is
// computed from (callers without one pass 0): a value computed at the same stamp,
// younger than the TTL and not invalidated since; the one being computed by
// another caller at the same stamp (waiting for it, or ctx; with ServeStale, the
// value it replaces when there is one); or a new one from compute. A failed
// computation is not cached: the callers that waited for it compute their own, so
// one cancelled request never fails the others.
func (c *Cache[K, V]) Get(ctx context.Context, key K, stamp int64, compute func(context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	var prev *entry[V]
	if e := c.entries[key]; e != nil && e.gen == c.gen && e.stamp == stamp {
		if !e.finished() {
			c.mu.Unlock()
			if e.hasPrev {
				return e.prev, nil
			}
			select {
			case <-e.done:
			case <-ctx.Done():
				var zero V
				return zero, ctx.Err()
			}
			if e.err == nil {
				return e.v, nil
			}
			return compute(ctx)
		}
		if e.err == nil && c.now().Sub(e.at) < c.opts.TTL {
			c.mu.Unlock()
			return e.v, nil
		}
		if e.err == nil && c.opts.ServeStale {
			prev = e
		}
	}
	if len(c.entries) >= c.opts.MaxEntries {
		for k, e := range c.entries {
			if e.finished() {
				delete(c.entries, k)
			}
		}
	}
	e := &entry[V]{done: make(chan struct{}), gen: c.gen, stamp: stamp}
	if prev != nil {
		e.prev, e.hasPrev = prev.v, true
	}
	c.entries[key] = e
	c.mu.Unlock()

	finished := false
	defer func() {
		if finished {
			return
		}
		// compute panicked: the entry fails, so its waiters wake up and compute
		// their own and the next caller computes again; the panic goes on (not
		// recovered, so it keeps its stack).
		e.err = errPanicked
		c.finish(key, e)
	}()
	e.v, e.err = compute(ctx)
	finished = true
	c.finish(key, e)
	return e.v, e.err
}

// errPanicked is the error of an entry whose computation panicked.
var errPanicked = errors.New("sharedcache: the computation panicked")

// finish ends e's computation: waiters wake up, and a failed entry is dropped.
func (c *Cache[K, V]) finish(key K, e *entry[V]) {
	e.at = c.now()
	close(e.done)
	if e.err == nil {
		return
	}
	c.mu.Lock()
	if c.entries[key] == e {
		delete(c.entries, key)
	}
	c.mu.Unlock()
}

// Invalidate drops every cached value; computations under way finish for the
// callers waiting for them but are not served afterwards.
func (c *Cache[K, V]) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	clear(c.entries)
}

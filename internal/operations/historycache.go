package operations

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
)

// DefaultHistoryCacheTTL is how long a computed History is served again when
// Config.HistoryCacheTTL asks for the default; events and job changes drop it
// earlier.
const DefaultHistoryCacheTTL = 10 * time.Second

// maxHistoryCacheEntries bounds the cached History windows (window, zone and
// connection access); finished entries are dropped when it is reached.
const maxHistoryCacheEntries = 64

// historyKey identifies a cached History: the window, the zone its days are cut
// in and the caller's connection access.
type historyKey struct {
	days   int
	zone   string
	access string
}

// accessKey renders a connection set for historyKey: "*" for every connection,
// else the sorted IDs (an empty limited set is "").
func accessKey(set auth.ConnectionSet) string {
	if !set.Limited() {
		return "*"
	}
	ids := set.IDs()
	slices.Sort(ids)
	return strings.Join(ids, ",")
}

// historyEntry is one cached or in-flight History. h, err and at are written
// before done is closed and read only after.
type historyEntry struct {
	done chan struct{}
	gen  uint64
	h    *History
	err  error
	at   time.Time
}

// finished reports whether e's computation ended.
func (e *historyEntry) finished() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// historyCache shares computed Histories for a TTL and computes each one once
// however many callers ask at the same time (single flight).
type historyCache struct {
	ttl time.Duration
	mu  sync.Mutex
	// gen counts invalidations: entries of an older generation are never served.
	gen     uint64
	entries map[historyKey]*historyEntry
}

func newHistoryCache(ttl time.Duration) *historyCache {
	return &historyCache{ttl: ttl, entries: map[historyKey]*historyEntry{}}
}

// get returns the History of key: a cached one younger than the TTL, the one
// being computed by another caller (waiting for it, or ctx), or a new one from
// compute. A failed computation is not cached; callers that waited for it compute
// their own, so one cancelled request never fails the others.
func (c *historyCache) get(ctx context.Context, key historyKey, compute func(context.Context) (*History, error)) (*History, error) {
	c.mu.Lock()
	if e := c.entries[key]; e != nil && e.gen == c.gen {
		if !e.finished() {
			c.mu.Unlock()
			select {
			case <-e.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if e.err == nil {
				return e.h, nil
			}
			return compute(ctx)
		}
		if e.err == nil && time.Since(e.at) < c.ttl {
			c.mu.Unlock()
			return e.h, nil
		}
	}
	if len(c.entries) >= maxHistoryCacheEntries {
		for k, e := range c.entries {
			if e.finished() {
				delete(c.entries, k)
			}
		}
	}
	e := &historyEntry{done: make(chan struct{}), gen: c.gen}
	c.entries[key] = e
	c.mu.Unlock()

	e.h, e.err = compute(ctx)
	e.at = time.Now()
	close(e.done)
	if e.err != nil {
		c.mu.Lock()
		if c.entries[key] == e {
			delete(c.entries, key)
		}
		c.mu.Unlock()
	}
	return e.h, e.err
}

// invalidate drops every cached History; computations under way finish for the
// callers waiting for them but are not served afterwards.
func (c *historyCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	clear(c.entries)
}

// InvalidateHistory drops the cached History (see Config.HistoryCacheTTL), for
// callers that changed jobs outside the operations service. It does nothing
// without the cache.
func (s *Service) InvalidateHistory() {
	if s.historyCache != nil {
		s.historyCache.invalidate()
	}
}

// historyEvents are the events after which the cached History is out of date: a
// backup or job run ended, a verification or retention changed what is stored, a
// job's databases changed, a bulk action ran, or a destructive action (a deleted
// backup or job, a purge) went through.
var historyEvents = map[events.EventType]bool{
	events.SecurityDestructiveAction: true,
	events.BackupSucceeded:           true,
	events.BackupFailed:              true,
	events.BackupCancelled:           true,
	events.BackupSkipped:             true,
	events.VerificationSucceeded:     true,
	events.VerificationFailed:        true,
	events.DriftDetected:             true,
	events.RetentionDeleted:          true,
	events.JobDatabasesAdded:         true,
	events.BulkCompleted:             true,
}

// HandleEvent drops the cached History after the events that change it (subscribe
// it to the event bus).
func (s *Service) HandleEvent(_ context.Context, e events.Event) {
	if historyEvents[e.Type] {
		s.InvalidateHistory()
	}
}

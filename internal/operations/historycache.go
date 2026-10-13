package operations

import (
	"context"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/sharedcache"
)

// DefaultHistoryCacheTTL is how long a computed History is served again when
// Config.HistoryCacheTTL asks for the default; events and job changes drop it
// earlier.
const DefaultHistoryCacheTTL = 10 * time.Second

// DefaultStatsCacheTTL is how long computed Stats are served again when
// Config.StatsCacheTTL asks for the default; any write to the jobs, backups, runs
// or restores (the store's change stamp), events and job changes drop them
// earlier.
const DefaultStatsCacheTTL = 10 * time.Second

// maxHistoryCacheEntries bounds the cached History windows (window, zone and
// connection access); finished entries are dropped when it is reached.
const maxHistoryCacheEntries = 64

// historyKey identifies a cached History: the window, the zone its days are cut
// in and the caller's connection access (sharedcache.AccessKey).
type historyKey struct {
	days   int
	zone   string
	access string
}

// historyCache shares computed Histories for a TTL and computes each one once
// however many callers ask at the same time (single flight).
type historyCache = sharedcache.Cache[historyKey, *History]

func newHistoryCache(ttl time.Duration) *historyCache {
	return sharedcache.New[historyKey, *History](sharedcache.Options{TTL: ttl, MaxEntries: maxHistoryCacheEntries})
}

// statsCache shares computed Stats among the callers with the same connection
// access (sharedcache.AccessKey) while the store's change stamp is unchanged, for
// a TTL; a caller that asks while an expired value is recomputed gets the
// previous one.
type statsCache = sharedcache.Cache[string, *Stats]

func newStatsCache(ttl time.Duration) *statsCache {
	return sharedcache.New[string, *Stats](sharedcache.Options{TTL: ttl, ServeStale: true})
}

// changeStamper is implemented by stores that count the writes to their tables
// (*store.SQLiteStore, see store.SQLiteStore.ChangeStamp).
type changeStamper interface {
	ChangeStamp(ctx context.Context) (int64, error)
}

// InvalidateHistory drops the cached History and Stats (see
// Config.HistoryCacheTTL and Config.StatsCacheTTL), for callers that changed jobs
// outside the operations service. It does nothing without the caches.
func (s *Service) InvalidateHistory() {
	if s.historyCache != nil {
		s.historyCache.Invalidate()
	}
	if s.statsCache != nil {
		s.statsCache.Invalidate()
	}
}

// historyEvents are the events after which the cached History and Stats are out of
// date: a backup or job run ended, a verification or retention changed what is
// stored, a job's databases changed, a bulk action ran, a destructive action (a
// deleted backup or job, a purge) went through, or a restore ended.
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
	events.RestoreSucceeded:          true,
	events.RestoreFailed:             true,
	events.RestoreCancelled:          true,
}

// HandleEvent drops the cached History and Stats after the events that change them
// (subscribe it to the event bus).
func (s *Service) HandleEvent(_ context.Context, e events.Event) {
	if historyEvents[e.Type] {
		s.InvalidateHistory()
	}
}

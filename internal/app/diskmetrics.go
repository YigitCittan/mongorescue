package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/metrics"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// diskMetricsTTL is how long a free-space measurement is reused, so frequent
// scrapes do not stat the filesystems each time.
const diskMetricsTTL = 15 * time.Second

// diskMetrics measures the free space of the data directory and the local storage
// targets for the metrics collector. It runs in the scrape's goroutine, caches its
// answer for ttl and starts no goroutine of its own.
type diskMetrics struct {
	dataDir   string
	listLocal func(ctx context.Context) ([]*models.StorageTarget, error)
	localPath func(string) string
	free      func(path string) (uint64, error)
	logger    *slog.Logger
	ttl       time.Duration
	now       func() time.Time

	mu     sync.Mutex
	at     time.Time
	cached metrics.DiskUsage
}

// usage implements metrics.DiskSource.
func (d *diskMetrics) usage(ctx context.Context) metrics.DiskUsage {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now := d.now(); !d.at.IsZero() && now.Sub(d.at) < d.ttl {
		return d.cached
	}
	var u metrics.DiskUsage
	if n, err := d.free(d.dataDir); err == nil {
		u.DataDirFree, u.DataDirKnown = n, true
	} else {
		d.logger.Debug("data directory free space unavailable", "error", err)
	}
	list, err := d.listLocal(ctx)
	if err != nil {
		d.logger.Debug("storage targets unavailable for free space metrics", "error", err)
	}
	for _, t := range list {
		if t.Type != models.StorageLocal || t.Local == nil {
			continue
		}
		n, ferr := d.free(d.localPath(t.Local.Path))
		if ferr != nil {
			d.logger.Debug("storage target free space unavailable", "target", t.ID, "error", ferr)
			continue
		}
		u.Targets = append(u.Targets, metrics.DiskTarget{Target: t.ID, FreeBytes: n})
	}
	d.cached, d.at = u, d.now()
	return u
}

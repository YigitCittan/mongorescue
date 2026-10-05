package backup

import (
	"context"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// SizeEstimator estimates the archive size of a backup of database (taken from
// connection connectionID at uri) before it runs, typically from the database's last
// completed backup or else the server's dbStats. It returns the size in bytes (0
// when unknown) and a short description of where it comes from. Implementations
// must never include the URI's credentials in errors.
type SizeEstimator func(ctx context.Context, connectionID, uri, database string) (size int64, source string, err error)

// WithSizeEstimator makes every backup to a storage target with an archive size
// limit (S3) estimate its size first, and warn when it exceeds
// models.ArchiveSizeWarnPercent of the limit.
func WithSizeEstimator(fn SizeEstimator) Option {
	return func(e *Engine) {
		e.estimateSize = fn
	}
}

// sizeEstimateTimeout bounds the size estimate before a backup.
const sizeEstimateTimeout = 10 * time.Second

// warnArchiveSize adds a warning to record, the run log and the log when the backup's
// expected archive exceeds models.ArchiveSizeWarnPercent of the largest archive its
// storage target can hold. It never fails the backup: an estimate that fails is only
// logged.
func (e *Engine) warnArchiveSize(ctx context.Context, uri string, opts models.BackupOptions, record *models.BackupRecord) {
	limit := storage.MaxArchiveSize(e.storage)
	if limit <= 0 || e.estimateSize == nil || record.InstanceScope() {
		return
	}
	estCtx, cancel := context.WithTimeout(ctx, sizeEstimateTimeout)
	defer cancel()
	size, source, err := e.estimateSize(estCtx, opts.ConnectionID, uri, opts.Database)
	if err != nil {
		e.logger.Debug("could not estimate the archive size",
			logsafe.Attr("backup_id", record.ID), logsafe.Attr("database", opts.Database), logsafe.Error(err))
		return
	}
	msg := models.ArchiveSizeWarning(opts.Database, size, source, limit)
	if msg == "" {
		return
	}
	record.Warnings = append(record.Warnings, msg)
	runs.FromContext(ctx).Printf("warning: %s", msg)
	e.logger.Warn("backup may exceed the largest archive of its storage target",
		logsafe.Attr("backup_id", record.ID),
		logsafe.Attr("database", opts.Database),
		logsafe.Attr("storage_target_id", opts.StorageTargetID),
		slog.Int64("estimated_bytes", size),
		slog.Int64("max_archive_bytes", limit),
	)
}

package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/yigitcittan/mongorescue/internal/metrics"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// backupGaugeSource is the part of the metadata store that seeds the backup gauges.
type backupGaugeSource interface {
	ListJobs(ctx context.Context) ([]*models.Job, error)
	LatestJobDatabaseBackupsAll(ctx context.Context, verified bool) (map[string]map[string]*models.BackupRecord, error)
}

// seedBackupGauges loads the newest successful backup of every database of every
// existing job and sets the last-success and size gauges from them, so a restart
// or an HA takeover does not leave the series absent until the next backup. A
// failure is logged and leaves the gauges to the next BackupSucceeded event.
func seedBackupGauges(ctx context.Context, src backupGaugeSource, m *metrics.Metrics, logger *slog.Logger) {
	seeds, err := backupGaugeSeeds(ctx, src)
	if err != nil {
		logger.Warn("seeding the backup gauges failed", slog.Any("error", err))
		return
	}
	m.SeedLastBackups(seeds)
}

// backupGaugeSeeds returns the seeds of the jobs that still exist.
func backupGaugeSeeds(ctx context.Context, src backupGaugeSource) ([]metrics.BackupSeed, error) {
	jobs, err := src.ListJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: list jobs for the backup gauges: %w", err)
	}
	live := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		live[j.ID] = true
	}
	latest, err := src.LatestJobDatabaseBackupsAll(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("app: latest backups for the backup gauges: %w", err)
	}
	var seeds []metrics.BackupSeed
	for jobID, byDB := range latest {
		if !live[jobID] {
			continue
		}
		for db, rec := range byDB {
			at := rec.StartedAt
			if rec.CompletedAt != nil {
				at = *rec.CompletedAt
			}
			seeds = append(seeds, metrics.BackupSeed{JobID: jobID, Database: db, At: at, SizeBytes: rec.SizeBytes})
		}
	}
	return seeds, nil
}

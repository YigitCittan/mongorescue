package app

import (
	"context"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// backupHistory is the part of the metadata store the archive size estimate reads.
type backupHistory interface {
	QueryBackupRecords(ctx context.Context, f store.BackupFilter) (*store.BackupPage, error)
}

// archiveSizeEstimator estimates the archive size of a backup from the size of the
// database's last completed backup on the same connection, or, when there is none,
// from the server's dbStats (dataSize, uncompressed). dbSize may be nil.
func archiveSizeEstimator(history backupHistory, dbSize func(ctx context.Context, uri, database string) (int64, error)) backup.SizeEstimator {
	return func(ctx context.Context, connectionID, uri, database string) (int64, string, error) {
		page, err := history.QueryBackupRecords(ctx, store.BackupFilter{
			Status: models.StatusCompleted, Database: database, ConnectionID: connectionID, Limit: 1,
		})
		if err != nil {
			return 0, "", err
		}
		for _, row := range page.Rows {
			if row.Record != nil && row.Record.SizeBytes > 0 && !row.Record.InstanceScope() {
				return row.Record.SizeBytes, "size of its last backup", nil
			}
		}
		if dbSize == nil || uri == "" {
			return 0, "", nil
		}
		n, err := dbSize(ctx, uri, database)
		if err != nil {
			return 0, "", err
		}
		return n, "dbStats data size", nil
	}
}

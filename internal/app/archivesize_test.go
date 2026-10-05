package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestArchiveSizeEstimatorPrefersTheLastBackup proves the estimate is the size of
// the database's last completed backup on the connection, and dbStats otherwise.
func TestArchiveSizeEstimatorPrefersTheLastBackup(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	now := time.Now().UTC()
	for _, rec := range []*models.BackupRecord{
		{ID: "old", Database: "sales", ConnectionID: "conn_a", Status: models.StatusCompleted, SizeBytes: 100, StartedAt: now.Add(-2 * time.Hour)},
		{ID: "new", Database: "sales", ConnectionID: "conn_a", Status: models.StatusCompleted, SizeBytes: 300, StartedAt: now.Add(-time.Hour)},
		{ID: "failed", Database: "sales", ConnectionID: "conn_a", Status: models.StatusFailed, SizeBytes: 999, StartedAt: now},
		{ID: "other", Database: "sales", ConnectionID: "conn_b", Status: models.StatusCompleted, SizeBytes: 999, StartedAt: now},
	} {
		if err := st.SaveBackupRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	var statted string
	dbSize := func(_ context.Context, uri, database string) (int64, error) {
		statted = uri + "|" + database
		if database == "denied" {
			return 0, errors.New("dbStats: not authorized")
		}
		return 4242, nil
	}
	estimate := archiveSizeEstimator(st, dbSize)

	size, source, err := estimate(ctx, "conn_a", "mongodb://db/", "sales")
	if err != nil || size != 300 || source != "size of its last backup" || statted != "" {
		t.Fatalf("estimate = %d %q %v (dbStats %q); want the last completed backup", size, source, err, statted)
	}
	size, source, err = estimate(ctx, "conn_a", "mongodb://db/", "fresh")
	if err != nil || size != 4242 || source != "dbStats data size" || statted != "mongodb://db/|fresh" {
		t.Fatalf("estimate = %d %q %v (dbStats %q); want dbStats", size, source, err, statted)
	}
	if _, _, err = estimate(ctx, "conn_a", "mongodb://db/", "denied"); err == nil {
		t.Fatal("a failing dbStats must return its error")
	}
	if size, _, err = archiveSizeEstimator(st, nil)(ctx, "conn_a", "mongodb://db/", "fresh"); err != nil || size != 0 {
		t.Fatalf("estimate without dbStats = %d, %v; want unknown", size, err)
	}
}

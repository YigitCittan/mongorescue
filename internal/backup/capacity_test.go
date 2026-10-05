package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// limitedStorage is a mock driver with an archive size limit, like S3.
type limitedStorage struct {
	*storage.MockStorage
	limit int64
}

func (l limitedStorage) MaxArchiveSize() int64 { return l.limit }

// TestArchiveSizeWarningFiresAt80Percent proves a backup warns when its expected
// archive exceeds 80% of the storage target's largest archive, and only then.
func TestArchiveSizeWarningFiresAt80Percent(t *testing.T) {
	const limit = 1000
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
	}
	for _, tc := range []struct {
		name     string
		estimate int64
		err      error
		limit    int64
		warn     bool
	}{
		{name: "below", estimate: 799, limit: limit},
		{name: "at 80%", estimate: 800, limit: limit},
		{name: "above 80%", estimate: 801, limit: limit, warn: true},
		{name: "above the limit", estimate: 5000, limit: limit, warn: true},
		{name: "unknown size", estimate: 0, limit: limit},
		{name: "estimate fails", estimate: 5000, err: errors.New("dbStats: unauthorized"), limit: limit},
		{name: "no limit", estimate: 5000, limit: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotConn, gotURI, gotDB string
			estimate := func(_ context.Context, connectionID, uri, database string) (int64, string, error) {
				gotConn, gotURI, gotDB = connectionID, uri, database
				return tc.estimate, "last backup", tc.err
			}
			driver := limitedStorage{MockStorage: storage.NewMockStorage(), limit: tc.limit}
			engine := NewEngine(driver, "mongodb://localhost:27017", WithRunner(runner), WithSizeEstimator(estimate))
			record, err := engine.Run(context.Background(), models.BackupOptions{Database: "sales", ConnectionID: "conn_1"})
			if err != nil || record.Status != models.StatusCompleted {
				t.Fatalf("backup = %v, %v; a warning must never fail it", record.Status, err)
			}
			if got := len(record.Warnings) > 0; got != tc.warn {
				t.Fatalf("warnings = %q; want warning %v", record.Warnings, tc.warn)
			}
			if tc.warn && (!strings.Contains(record.Warnings[0], "part_size_mb") || !strings.Contains(record.Warnings[0], "sales")) {
				t.Fatalf("warning %q must name the database and the setting", record.Warnings[0])
			}
			if tc.limit > 0 && (gotConn != "conn_1" || gotURI == "" || gotDB != "sales") {
				t.Fatalf("estimator called with (%q, %q, %q)", gotConn, gotURI, gotDB)
			}
		})
	}
}

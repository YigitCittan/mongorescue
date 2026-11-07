package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestDiskMetricsUsage(t *testing.T) {
	now := time.Unix(1000, 0)
	calls := 0
	d := &diskMetrics{
		dataDir: "/data",
		listLocal: func(context.Context) ([]*models.StorageTarget, error) {
			return []*models.StorageTarget{
				{ID: "stg_local", Type: models.StorageLocal, Local: &models.LocalTarget{Path: "/backups"}},
				{ID: "stg_s3", Type: models.StorageS3},
				{ID: "stg_gone", Type: models.StorageLocal, Local: &models.LocalTarget{Path: "/gone"}},
			}, nil
		},
		localPath: func(p string) string { return p },
		free: func(p string) (uint64, error) {
			calls++
			switch p {
			case "/data":
				return 100, nil
			case "/backups":
				return 200, nil
			}
			return 0, errors.New("no such directory")
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ttl:    time.Minute,
		now:    func() time.Time { return now },
	}
	u := d.usage(context.Background())
	if !u.DataDirKnown || u.DataDirFree != 100 {
		t.Errorf("data dir = %+v", u)
	}
	if len(u.Targets) != 1 || u.Targets[0].Target != "stg_local" || u.Targets[0].FreeBytes != 200 {
		t.Errorf("targets = %+v, want only the readable local target", u.Targets)
	}
	before := calls
	d.usage(context.Background())
	if calls != before {
		t.Error("a second call within the TTL measured again")
	}
	now = now.Add(2 * time.Minute)
	d.usage(context.Background())
	if calls == before {
		t.Error("a call after the TTL did not measure again")
	}
}

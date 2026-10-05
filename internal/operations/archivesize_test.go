package operations_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// s3Targets serves one S3 target with a 5 MiB part size (about 48.8 GiB per
// archive) and one local target.
type s3Targets struct{}

func (s3Targets) Resolve(_ context.Context, id string) (*models.StorageTarget, error) {
	switch id {
	case "", "tgt_s3":
		return &models.StorageTarget{ID: "tgt_s3", Type: models.StorageS3, IsDefault: true,
			S3: &models.S3Target{Bucket: "b", PartSizeMB: models.MinS3PartSizeMB}}, nil
	case "tgt_local":
		return &models.StorageTarget{ID: "tgt_local", Type: models.StorageLocal, Local: &models.LocalTarget{Path: "/backups"}}, nil
	}
	return nil, errors.New("targets: storage target not found")
}

func (s3Targets) List(context.Context) ([]*models.StorageTarget, error) { return nil, nil }

// TestPreviewWarnsAboutArchivesNearTheTargetLimit proves a job preview warns about
// every database whose expected archive exceeds 80% of the largest archive its
// storage target can hold, and that a target without a limit never warns.
func TestPreviewWarnsAboutArchivesNearTheTargetLimit(t *testing.T) {
	e := newMultiEnv(t, "big", "small", "unknown")
	ctx := context.Background()
	limit := (&models.S3Target{PartSizeMB: models.MinS3PartSizeMB}).MaxArchiveBytes()
	sizes := map[string]int64{"big": limit*8/10 + 1, "small": limit * 8 / 10}
	var calls []string
	mock := storage.NewMockStorage()
	svc := operations.New(operations.Config{
		Store: e.st, Backup: backup.NewEngine(mock, ""), Restore: restore.NewEngine(mock, ""),
		Jobs: e.sched, Scheduler: e.sched, Runs: e.runs,
		Connections: fakeConnections{"conn_a": {ID: "conn_a", Name: "primary", URI: "mongodb://u:pw@db.internal/"}},
		Targets:     s3Targets{},
		ArchiveSize: func(_ context.Context, connectionID, uri, database string) (int64, string, error) {
			calls = append(calls, connectionID+"|"+uri+"|"+database)
			if database == "unknown" {
				return 0, "", errors.New("dbStats: not authorized")
			}
			return sizes[database], "size of its last backup", nil
		},
	})
	all := &models.DatabaseSelection{Mode: models.SelectionAll}
	p, err := svc.PreviewJobDatabases(ctx, "", operations.DatabasePreviewRequest{ConnectionID: "conn_a", Selection: all})
	if err != nil {
		t.Fatal(err)
	}
	var size []string
	for _, w := range p.Warnings {
		if strings.Contains(w, "part_size_mb") {
			size = append(size, w)
		}
	}
	if len(size) != 1 || !strings.Contains(size[0], "database big ") {
		t.Fatalf("size warnings = %q; want one for big only", size)
	}
	if len(calls) != 3 || !strings.HasPrefix(calls[0], "conn_a|mongodb://u:pw@db.internal/|") {
		t.Fatalf("estimator calls = %q", calls)
	}

	calls = nil
	local, err := svc.PreviewJobDatabases(ctx, "", operations.DatabasePreviewRequest{ConnectionID: "conn_a", Selection: all, StorageTargetID: "tgt_local"})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range local.Warnings {
		if strings.Contains(w, "part_size_mb") {
			t.Fatalf("a local target warned: %q", w)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a target without a limit estimated sizes: %q", calls)
	}
}

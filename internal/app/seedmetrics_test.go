package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/metrics"
	"github.com/yigitcittan/mongorescue/internal/models"
)

type fakeGaugeSource struct {
	jobs   []*models.Job
	latest map[string]map[string]*models.BackupRecord
}

func (f fakeGaugeSource) ListJobs(context.Context) ([]*models.Job, error) { return f.jobs, nil }

func (f fakeGaugeSource) LatestJobDatabaseBackupsAll(context.Context, bool) (map[string]map[string]*models.BackupRecord, error) {
	return f.latest, nil
}

func TestSeedBackupGauges(t *testing.T) {
	done := time.Unix(1_790_000_000, 0).UTC()
	src := fakeGaugeSource{
		jobs: []*models.Job{{ID: "nightly"}},
		latest: map[string]map[string]*models.BackupRecord{
			"nightly": {"shop": {JobID: "nightly", Database: "shop", StartedAt: done.Add(-time.Minute), CompletedAt: &done, SizeBytes: 42}},
			"deleted": {"shop": {JobID: "deleted", Database: "shop", StartedAt: done, SizeBytes: 1}},
		},
	}
	m := metrics.New(metrics.BuildInfo{Version: "1", Commit: "c", GoVersion: "go"})
	seedBackupGauges(context.Background(), src, m, slog.New(slog.NewTextHandler(io.Discard, nil)))

	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	seeded := map[string]bool{}
	for _, mf := range mfs {
		for _, metric := range mf.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetName() != "job" {
					continue
				}
				if l.GetValue() == "deleted" {
					t.Errorf("%s has a series for a deleted job", mf.GetName())
				}
				seeded[mf.GetName()] = seeded[mf.GetName()] || l.GetValue() == "nightly"
			}
		}
	}
	for _, name := range []string{
		"mongorescue_backup_size_bytes", "mongorescue_last_successful_backup_timestamp_seconds",
		"mongorescue_database_backup_size_bytes", "mongorescue_database_last_successful_backup_timestamp_seconds",
	} {
		if !seeded[name] {
			t.Errorf("%s is not exposed for nightly", name)
		}
	}
}

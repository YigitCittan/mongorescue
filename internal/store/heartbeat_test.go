package store_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestJobHeartbeatURLIsEncryptedAtRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	const url = "https://hc-ping.com/heartbeat-token-1"
	if err := s.CreateJob(ctx, &models.Job{ID: "j1", Name: "one", HeartbeatURL: url}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveJob(ctx, &models.Job{ID: "j2", Name: "two", HeartbeatURL: url + "-2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateJob(ctx, &models.Job{ID: "j1", Name: "one", HeartbeatURL: url}); err != nil {
		t.Fatal(err)
	}
	// Writes that re-encode the stored row keep the sealed value as it is.
	now := time.Now()
	if err := s.UpdateJobRunTimes(ctx, "j1", &now, nil); err != nil {
		t.Fatal(err)
	}
	raw := rawData(t, path, "jobs")
	if strings.Contains(raw, "heartbeat-token-1") || !strings.Contains(raw, secretbox.Prefix) {
		t.Fatalf("job heartbeat URL stored in plaintext: %s", raw)
	}
	got, err := s.GetJob(ctx, "j1")
	if err != nil || got.HeartbeatURL != url {
		t.Fatalf("GetJob = %+v, %v", got, err)
	}
	jobs, err := s.ListJobs(ctx)
	if err != nil || len(jobs) != 2 || jobs[1].HeartbeatURL != url+"-2" {
		t.Fatalf("ListJobs = %+v, %v", jobs, err)
	}
	// Saving a job read back never seals its URL twice.
	if err = s.SaveJob(ctx, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.GetJob(ctx, "j1"); again.HeartbeatURL != url {
		t.Fatalf("heartbeat after a re-save = %q", again.HeartbeatURL)
	}
}

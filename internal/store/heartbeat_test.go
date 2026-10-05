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

// TestUnreadableHeartbeatURLKeepsTheJob proves that a heartbeat URL that can no
// longer be decrypted (here: a ciphertext swapped in from another job, which passes
// the startup check but is bound to another row) never drops the job: it is listed
// and read without its heartbeat, so its backups keep running, and the field is
// reported until the URL is entered again.
func TestUnreadableHeartbeatURLKeepsTheJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	for _, j := range []*models.Job{
		{ID: "j1", Name: "one", Database: "shop", HeartbeatURL: "https://hc-ping.com/one"},
		{ID: "j2", Name: "two", Database: "crm", HeartbeatURL: "https://hc-ping.com/two"},
	} {
		if err := s.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB(t, path).Exec(`UPDATE jobs SET data = json_set(data, '$.heartbeat_url',
		(SELECT json_extract(data, '$.heartbeat_url') FROM jobs WHERE id = 'j2')) WHERE id = 'j1'`); err != nil {
		t.Fatal(err)
	}

	s, err := openWith(t, path, testBox)
	if err != nil {
		t.Fatalf("open with a swapped heartbeat ciphertext: %v", err)
	}
	jobs, err := s.ListJobs(ctx)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("ListJobs = %d jobs, %v; want both", len(jobs), err)
	}
	if jobs[0].ID != "j1" || jobs[0].HeartbeatURL != "" || jobs[0].Database != "shop" || jobs[1].HeartbeatURL != "https://hc-ping.com/two" {
		t.Fatalf("ListJobs = %+v, %+v", jobs[0], jobs[1])
	}
	got, err := s.GetJob(ctx, "j1")
	if err != nil || got.HeartbeatURL != "" {
		t.Fatalf("GetJob = %+v, %v; want the job without its heartbeat", got, err)
	}
	bad, err := s.CorruptRecords(ctx)
	if err != nil || len(bad) != 1 || bad[0].Table != "jobs.heartbeat_url" || bad[0].ID != "j1" {
		t.Fatalf("CorruptRecords = %+v, %v; want the heartbeat URL of j1", bad, err)
	}
	if strings.Contains(bad[0].Error, "hc-ping") {
		t.Fatalf("the report quotes stored data: %+v", bad[0])
	}

	// Entering the URL again repairs it.
	got.HeartbeatURL = "https://hc-ping.com/one-again"
	if err = s.UpdateJob(ctx, got); err != nil {
		t.Fatal(err)
	}
	if bad, err = s.CorruptRecords(ctx); err != nil || len(bad) != 0 {
		t.Fatalf("CorruptRecords after a repair = %+v, %v", bad, err)
	}
	if again, _ := s.GetJob(ctx, "j1"); again.HeartbeatURL != "https://hc-ping.com/one-again" {
		t.Fatalf("heartbeat after a repair = %q", again.HeartbeatURL)
	}
}

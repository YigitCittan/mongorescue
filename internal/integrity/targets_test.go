package integrity

import (
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestRestoreTestTargetsRotateAndAll(t *testing.T) {
	recs := []*models.BackupRecord{
		{ID: "c", Database: "crm", Status: models.StatusCompleted},
		{ID: "a", Database: "app", Status: models.StatusCompleted},
		{ID: "f", Database: "fail", Status: models.StatusFailed},
		{ID: "b", Database: "billing", Status: models.StatusCompleted},
	}
	job := &models.Job{RestoreTest: &models.RestoreTestPolicy{Enabled: true}}
	ids := func(list []*models.BackupRecord) string {
		out := ""
		for _, r := range list {
			out += r.ID
		}
		return out
	}
	// Rotate (the default): one per run, in name order, wrapping around.
	for _, tt := range []struct{ last, want string }{{"", "a"}, {"app", "b"}, {"billing", "c"}, {"crm", "a"}, {"zzz", "a"}} {
		job.LastRestoreTest = nil
		if tt.last != "" {
			job.LastRestoreTest = &models.RestoreTestSummary{Database: tt.last}
		}
		if got := ids(RestoreTestTargets(job, recs)); got != tt.want {
			t.Errorf("after %q: tested %q; want %q", tt.last, got, tt.want)
		}
	}
	job.RestoreTest.Databases = models.RestoreTestAllDatabases
	if got := ids(RestoreTestTargets(job, recs)); got != "abc" {
		t.Errorf("all: tested %q; want every completed backup", got)
	}
	if got := RestoreTestTargets(job, []*models.BackupRecord{{Status: models.StatusFailed}}); got != nil {
		t.Errorf("nothing completed: %v", got)
	}
	bad := &models.RestoreTestPolicy{Enabled: true, Databases: "some"}
	if err := bad.Validate(); err == nil {
		t.Error("an unknown databases scope must be refused")
	}
}

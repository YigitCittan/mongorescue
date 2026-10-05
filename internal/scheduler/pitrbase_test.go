package scheduler

import (
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestJobRetentionNeverTouchesPITRBases(t *testing.T) {
	now := time.Now().UTC()
	job := &models.Job{ID: "job_a", ConnectionID: "conn_a"}
	var records []*models.BackupRecord
	for i := range 5 {
		records = append(records, &models.BackupRecord{
			ID: "bkp_base_" + string(rune('a'+i)), JobID: "job_a", ConnectionID: "conn_a", Trigger: models.TriggerScheduled,
			Status: models.StatusCompleted, StartedAt: now.Add(-time.Duration(30+i) * 24 * time.Hour), Scope: models.ScopeInstance,
		})
	}
	if got := JobRetentionHistory(job, "", records); len(got) != 0 {
		t.Fatalf("JobRetentionHistory kept %d bases", len(got))
	}
	if plan := PlanRetention(now, 1, 1, records); len(plan.Delete) != 0 || plan.Considered != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

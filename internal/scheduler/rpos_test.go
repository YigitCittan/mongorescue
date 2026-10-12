package scheduler

import (
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestRPOsMatchEffectiveRPO(t *testing.T) {
	night := &models.BackupWindow{Start: "01:00", End: "05:00"}
	weekend := &models.BackupWindow{Days: []string{"sat", "sun"}, Start: "01:00", End: "05:00"}
	jobs := []*models.Job{
		{ID: "a", CronExpression: "0 * * * *"},
		{ID: "b", CronExpression: "0 * * * *", BackupWindow: night},
		{ID: "c", CronExpression: "0 * * * *", BackupWindow: weekend},
		{ID: "d", CronExpression: "0 * * * *", RPOMinutes: 90},
		{ID: "e", CronExpression: "0 9 * * 1-5"},
		{ID: "f", CronExpression: "not a schedule"},
		{ID: "g", CronExpression: "0 * * * *"},
		{ID: "h", CronExpression: "0 * * * *", BackupWindow: night},
	}
	r := NewRPOs(rpoFrom)
	for _, j := range jobs {
		got, gotDefault := r.Effective(j)
		want, wantDefault := EffectiveRPO(j, rpoFrom)
		if got != want || gotDefault != wantDefault {
			t.Errorf("job %s: %v, %v; want %v, %v", j.ID, got, gotDefault, want, wantDefault)
		}
	}
	// a and g, b and h share their interval; d sets its own RPO.
	if len(r.intervals) != 5 {
		t.Errorf("%d intervals computed; want 5", len(r.intervals))
	}
}

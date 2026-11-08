package operations_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

func TestCreateJobGeneratesIDAndSchedules(t *testing.T) {
	e := newMultiEnv(t, "shop")
	ctx := context.Background()
	res, err := e.svc.CreateJob(ctx, operations.JobCreate{Job: models.Job{Name: "n", Database: "shop", ConnectionID: "conn_a",
		CronExpression: "@daily", Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.ID, "job_shop_") || models.ValidateID(res.ID) != nil {
		t.Errorf("generated id = %q", res.ID)
	}
	stored, err := e.st.GetJob(ctx, res.ID)
	if err != nil || stored.Name != "n" {
		t.Fatalf("stored job = %+v, %v", stored, err)
	}
	if stored.NextRun == nil {
		t.Error("an enabled job has no next run: it was not scheduled")
	}
	if stored.CreatedAt.IsZero() {
		t.Error("created_at is not set")
	}
}

func TestCreateJobNamesMultiDatabaseJobsAfterTheirMode(t *testing.T) {
	e := newMultiEnv(t, "a", "b")
	res, err := e.svc.CreateJob(context.Background(), operations.JobCreate{Job: models.Job{Name: "m", ConnectionID: "conn_a",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionAll}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.ID, "job_all_") {
		t.Errorf("generated id = %q; want a job_all_ prefix", res.ID)
	}
}

func TestCreateJobNeverOverwrites(t *testing.T) {
	e := newMultiEnv(t, "shop")
	ctx := context.Background()
	in := operations.JobCreate{Job: models.Job{ID: "job_fixed", Name: "first", Database: "shop", ConnectionID: "conn_a"}}
	if _, err := e.svc.CreateJob(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Name = "second"
	_, err := e.svc.CreateJob(ctx, in)
	if !errors.Is(err, operations.ErrAlreadyExists) {
		t.Fatalf("second create = %v; want ErrAlreadyExists", err)
	}
	if stored, _ := e.st.GetJob(ctx, "job_fixed"); stored == nil || stored.Name != "first" {
		t.Fatalf("stored job = %+v; want the first one untouched", stored)
	}
}

func TestCreateJobRejectsInvalidInput(t *testing.T) {
	e := newMultiEnv(t, "shop")
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	base := func() models.Job {
		return models.Job{Name: "n", Database: "shop", ConnectionID: "conn_a", Enabled: true}
	}
	cases := map[string]func(j *models.Job){
		"bad id":           func(j *models.Job) { j.ID = "job a/b" },
		"masked heartbeat": func(j *models.Job) { j.HeartbeatURL = "https://hc-ping.com/" + models.SecretMask },
		"past pause":       func(j *models.Job) { j.Enabled, j.PausedUntil = false, &past },
		"no database":      func(j *models.Job) { j.Database = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			j := base()
			mutate(&j)
			if _, err := e.svc.CreateJob(ctx, operations.JobCreate{Job: j}); !errors.Is(err, operations.ErrInvalid) {
				t.Fatalf("CreateJob = %v; want ErrInvalid", err)
			}
		})
	}
	if list, _ := e.st.ListJobs(ctx); len(list) != 0 {
		t.Errorf("%d jobs stored after refused creates", len(list))
	}
}

func TestCreateJobTakesServerManagedFieldsFromTheServer(t *testing.T) {
	e := newMultiEnv(t, "shop")
	ctx := context.Background()
	forged := time.Now().Add(-48 * time.Hour)
	zero, one := 0, 1
	res, err := e.svc.CreateJob(ctx, operations.JobCreate{
		Job: models.Job{ID: "job_srv", Name: "n", Database: "shop", ConnectionID: "conn_a", LastRun: &forged,
			KnownDatabases: []string{"forged"}},
		RetentionDays: &zero, RetentionCount: &one,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := e.st.GetJob(ctx, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastRun != nil || slices.Contains(stored.KnownDatabases, "forged") {
		t.Errorf("client-sent last_run %v or known databases %v were stored", stored.LastRun, stored.KnownDatabases)
	}
	if stored.RetentionDays != 0 || stored.RetentionCount != 1 {
		t.Errorf("retention = %d days, %d count; want the request's 0 and 1", stored.RetentionDays, stored.RetentionCount)
	}
}

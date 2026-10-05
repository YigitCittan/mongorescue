package integrity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestRestoreTestsSkipBackupsOfOtherConnections checks that a restore test started
// by a caller limited to some connections picks the newest backup of the job taken
// from those connections: a job that moved from another connection never tests
// that connection's backups.
func TestRestoreTestsSkipBackupsOfOtherConnections(t *testing.T) {
	f := newFixture(t)
	job := &models.Job{ID: "job_moved", Name: "moved", Database: "shop", ConnectionID: "conn_src", CronExpression: "@daily"}
	f.putBackup(t, "bkp_old_a", job.ID, f.now.Add(-2*time.Hour), []byte("a"), nil)
	f.putBackup(t, "bkp_new_b", job.ID, f.now.Add(-time.Hour), []byte("b"), func(r *models.BackupRecord) { r.ConnectionID = "conn_b" })

	limited := auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodSession, Scope: auth.ScopeOperator,
		Connections: auth.OnlyConnections("conn_src")})
	b, err := f.svc.latestBackup(limited, job)
	if err != nil || b.ID != "bkp_old_a" {
		t.Fatalf("latest backup for a limited caller = %+v, %v; want bkp_old_a", b, err)
	}
	if b, err = f.svc.latestBackup(context.Background(), job); err != nil || b.ID != "bkp_new_b" {
		t.Fatalf("latest backup = %+v, %v; want bkp_new_b", b, err)
	}
	none := auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodSession, Scope: auth.ScopeOperator,
		Connections: auth.OnlyConnections("conn_other")})
	if _, err = f.svc.latestBackup(none, job); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("a caller without the backups' connections: %v; want ErrNoBackup", err)
	}
}

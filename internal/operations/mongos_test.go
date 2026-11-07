package operations_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestRestoreIntoAMongosIsRefusedUnlessAllowed uses a target whose hello answers
// msg: isdbgrid: the restore is refused with models.ErrShardedUnsupported, also
// with "force": true, until the connection sets allow_mongos, which only warns.
func TestRestoreIntoAMongosIsRefusedUnlessAllowed(t *testing.T) {
	ins := healthyInspector()
	ins.mongos = true
	env := newPreflightEnv(t, ins, nil)

	pre, err := env.svc.PreflightRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID})
	if err != nil {
		t.Fatal(err)
	}
	if c := pre.Check(models.PreflightCheckTopology); c == nil || c.Status != models.PreflightFail || pre.OK {
		t.Fatalf("preflight %+v, want a failed topology check", pre.Checks)
	}

	for _, req := range []models.RestoreRequest{
		{BackupID: env.backup.ID},
		{BackupID: env.backup.ID, Force: true},
		{BackupID: env.backup.ID, DryRun: true},
	} {
		_, err = env.svc.StartRestore(admin(), req)
		if !errors.Is(err, models.ErrShardedUnsupported) || !strings.Contains(err.Error(), "allow_mongos") {
			t.Fatalf("StartRestore(%+v) = %v, want ErrShardedUnsupported", req, err)
		}
	}
	if env.engine.runs() != 0 {
		t.Fatal("a restore ran into a mongos")
	}

	env.conn.AllowMongos = true
	pre, err = env.svc.PreflightRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID})
	if err != nil {
		t.Fatal(err)
	}
	if c := pre.Check(models.PreflightCheckTopology); c == nil || c.Status != models.PreflightWarn ||
		!strings.Contains(c.Message, models.WarningShardedUnsupported) || !pre.OK {
		t.Fatalf("preflight %+v, want a topology warning", pre.Checks)
	}
	if _, err = env.svc.StartRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID}); err != nil {
		t.Fatalf("StartRestore with allow_mongos: %v", err)
	}
}

// TestRestoreIntoAReplicaSetHasNoTopologyCheck keeps the preflight of an ordinary
// target unchanged.
func TestRestoreIntoAReplicaSetHasNoTopologyCheck(t *testing.T) {
	env := newPreflightEnv(t, healthyInspector(), nil)
	pre, err := env.svc.PreflightRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID})
	if err != nil {
		t.Fatal(err)
	}
	if pre.Check(models.PreflightCheckTopology) != nil {
		t.Fatalf("unexpected topology check: %+v", pre.Checks)
	}
}

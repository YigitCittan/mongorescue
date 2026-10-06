package operations_test

import (
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
)

// TestSameSecondRestoresBothSucceed starts two restores of one database with a
// clock stopped in one second: both pass their preflight (the first clone exists by
// then) and get clones of their own, and each preflight names its restore's clone.
func TestSameSecondRestoresBothSucceed(t *testing.T) {
	ins := healthyInspector()
	env := newPreflightEnv(t, ins, nil)
	at := time.Date(2026, 10, 6, 9, 30, 5, 0, time.UTC)
	ids := []string{"ab12", "cd34"}
	env.engine.prep = restore.NewEngine(nil, "", restore.WithClock(func() time.Time { return at }),
		restore.WithCloneID(func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil }))

	first, err := env.svc.StartRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID})
	if err != nil {
		t.Fatal(err)
	}
	ins.mu.Lock()
	ins.exists[first.TargetDatabase] = true // the first clone now exists
	ins.mu.Unlock()
	second, err := env.svc.StartRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID})
	if err != nil {
		t.Fatalf("second restore in the same second: %v", err)
	}
	if first.TargetDatabase != "shop_rescue_20261006_093005_ab12" || second.TargetDatabase != "shop_rescue_20261006_093005_cd34" {
		t.Fatalf("clones %s and %s", first.TargetDatabase, second.TargetDatabase)
	}
	for _, rec := range []*models.RestoreRecord{first, second} {
		// The preflight checked the very name the restore uses.
		if c := rec.Preflight.Check(models.PreflightCheckTargetDatabase); c == nil || c.Status != models.PreflightPass ||
			!strings.Contains(c.Message, rec.TargetDatabase) {
			t.Fatalf("target_database check of %s = %+v", rec.TargetDatabase, c)
		}
		if done := waitRestoreDone(t, env.svc, rec.ID); done.Status != models.RestoreStatusCompleted || done.TargetDatabase != rec.TargetDatabase {
			t.Fatalf("restore = %+v", done)
		}
	}
	waitIdle(t, env.runs)
}

// TestPreflightShowsTheCloneNamePattern checks that a preflight on its own, whose
// clone is only named when the restore starts, shows the name's pattern instead of
// a name the restore would not use.
func TestPreflightShowsTheCloneNamePattern(t *testing.T) {
	env := newPreflightEnvWith(t, healthyInspector(), nil, connErasures)
	res, err := env.svc.PreflightRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID})
	if err != nil {
		t.Fatal(err)
	}
	const pattern = "shop_rescue_<YYYYMMDD_HHMMSS>_<id>"
	for _, id := range []string{models.PreflightCheckTargetDatabase, models.PreflightCheckPrivileges, models.PreflightCheckPostRestore} {
		if c := res.Check(id); c == nil || !strings.Contains(c.Message, pattern) {
			t.Fatalf("%s check = %+v; want the pattern %s", id, c, pattern)
		}
	}
	for _, p := range res.PostRestore {
		if p.Database != pattern {
			t.Fatalf("planned post-restore command = %+v; want the pattern", p)
		}
	}

	// In place, the target is known and named.
	res, err = env.svc.PreflightRestore(admin(), inPlace(models.RestoreRequest{BackupID: env.backup.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if c := res.Check(models.PreflightCheckTargetDatabase); c == nil || !strings.Contains(c.Message, "database shop ") {
		t.Fatalf("in-place target_database check = %+v", c)
	}
}

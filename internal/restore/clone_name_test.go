package restore

import (
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestNamedCloneIsEscaped checks that a restore test's named clone goes through the
// same namespace escaping as every other target, and never names the source.
func TestNamedCloneIsEscaped(t *testing.T) {
	e := NewEngine(nil, "mongodb://localhost")
	src := &models.BackupRecord{ID: "bkp_1", Database: `a*b\c`}
	temp, err := models.RescueVerifyDatabaseName(src.Database, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "abcdef")
	if err != nil {
		t.Fatal(err)
	}
	req := models.RestoreRequest{BackupID: src.ID, MongoURI: "mongodb://localhost", CloneDatabase: temp}
	rec, err := e.Prepare(req, src)
	if err != nil || rec.TargetDatabase != temp {
		t.Fatalf("prepare = %+v, %v", rec, err)
	}
	args := e.buildRestoreArgs("--config=x", src.Database, rec.TargetDatabase, req, true)
	for _, want := range []string{`--nsFrom=a\*b\\c.*`, `--nsTo=a\*b\\c_rescue_verify_20261001_000000_abcdef.*`, `--nsInclude=a\*b\\c.*`} {
		if !slices.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if _, err := e.Prepare(models.RestoreRequest{BackupID: src.ID, MongoURI: "mongodb://localhost", CloneDatabase: src.Database}, src); err == nil {
		t.Fatal("a named clone equal to the source database must be refused")
	}
}

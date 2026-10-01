package events

import (
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestCancelledRunsAreNotFailures(t *testing.T) {
	b := BackupEvent(&models.BackupRecord{ID: "bkp_1", Database: "shop", Status: models.StatusCancelled,
		ErrorMessage: "backup cancelled by alice (mongodb://u:pw@h)"}, errors.New("backup cancelled by alice"), "job_1", "shop")
	if b.Type != BackupCancelled || b.Type.Failed() || b.Status != string(models.StatusCancelled) || b.Error != "backup cancelled by alice (mongodb://u:******@h)" {
		t.Fatalf("backup event = %+v", b)
	}
	r := RestoreEvent(&models.RestoreRecord{ID: "rst_1", BackupID: "bkp_1", TargetDatabase: "shop_rescue", Status: models.RestoreStatusCancelled,
		ErrorMessage: "restore cancelled by alice"}, errors.New("restore cancelled by alice"), "bkp_1")
	if r.Type != RestoreCancelled || r.Type.Failed() || r.Error != "restore cancelled by alice" {
		t.Fatalf("restore event = %+v", r)
	}
	for _, typ := range []EventType{BackupCancelled, RestoreCancelled} {
		if !typ.Subscribable() || typ.Broadcast() {
			t.Errorf("%s: rules must be able to select it, and it is not broadcast", typ)
		}
	}
	// A plain abort (shutdown, timeout) stays a failure.
	if e := BackupEvent(&models.BackupRecord{Status: models.StatusFailed}, errors.New("backup cancelled: context canceled"), "", "shop"); e.Type != BackupFailed {
		t.Fatalf("failed backup = %s", e.Type)
	}
}

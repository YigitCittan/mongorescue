package operations_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// withB1Databases runs fn with base b1 listing dbs.
func withB1Databases(t *testing.T, dbs []string) {
	t.Helper()
	old := b1Databases
	b1Databases = dbs
	t.Cleanup(func() { b1Databases = old })
}

func TestChainTestFitsA27ByteDatabaseName(t *testing.T) {
	db := strings.Repeat("n", 27)
	withB1Databases(t, []string{db})
	inspector := healthyInspector()
	inspector.manifest = &models.Manifest{Collections: []models.CollectionManifest{{Name: "orders", DocumentsMin: 7, DocumentsMax: 7}}}
	b2 := chainTestBase(7)
	b2.Manifest.Collections[0].Name = db + ".orders"
	svc, fake := pitrService(t, inspector, b2)
	rec, err := svc.StartChainTest(admin(), "str_a")
	if err != nil {
		t.Fatalf("StartChainTest: %v", err)
	}
	if len(rec.PITR.Clones) != 1 || rec.PITR.Clones[0] != db+rec.PITR.CloneSuffix || len(rec.PITR.Clones[0]) > models.MaxDatabaseNameLength {
		t.Fatalf("clones %v", rec.PITR.Clones)
	}
	if res := awaitChainTest(t, svc, fake); res.Failed {
		t.Fatalf("result = %+v", res)
	}
}

func TestPITRCloneNameTooLongIsRefused(t *testing.T) {
	long := strings.Repeat("w", 40)
	withB1Databases(t, []string{"shop", long})
	svc, fake := pitrService(t, healthyInspector())
	res, err := svc.PreflightRestore(admin(), pitrAt(125))
	if err != nil {
		t.Fatal(err)
	}
	c := res.Check(models.PreflightCheckTargetDatabase)
	if c == nil || c.Status != models.PreflightFail || !strings.Contains(c.Message, long) || res.OK {
		t.Fatalf("target_database = %+v", c)
	}
	if _, err = svc.StartRestore(admin(), pitrAt(125)); !errors.Is(err, operations.ErrInvalid) || !strings.Contains(err.Error(), long) {
		t.Fatalf("StartRestore: %v", err)
	}
	// One database whose clone fits is fine.
	if _, err = svc.StartRestore(admin(), pitrAt(125, "shop")); err != nil {
		t.Fatalf("one short database: %v", err)
	}
	<-fake.done
}

func TestPITRSelectionMustBeInTheBase(t *testing.T) {
	svc, _ := pitrService(t, nil)
	if _, err := svc.StartRestore(admin(), pitrAt(125, "billing")); !errors.Is(err, operations.ErrInvalid) || !strings.Contains(err.Error(), "billing") {
		t.Fatalf("err = %v, want ErrInvalid naming billing", err)
	}
	if _, err := svc.PreflightRestore(admin(), pitrAt(125, "billing")); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("preflight: err = %v, want ErrInvalid", err)
	}
	if _, err := svc.StartRestore(admin(), pitrAt(125, "shop_rescue_20261001_000000_abcd")); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("a clone selected: err = %v, want ErrInvalid", err)
	}
}

func TestChainTestRespectsThePreflight(t *testing.T) {
	inspector := healthyInspector()
	inspector.free = 10 // bytes: the base and the oplog do not fit
	svc, fake := pitrService(t, inspector, chainTestBase(7))
	var pre *operations.PreflightError
	if _, err := svc.StartChainTest(admin(), "str_a"); !errors.As(err, &pre) {
		t.Fatalf("err = %v, want a refused preflight", err)
	}
	if c := pre.Result.Check(models.PreflightCheckDiskSpace); c == nil || c.Status != models.PreflightFail {
		t.Fatalf("disk_space = %+v", c)
	}
	if len(fake.runs) != 0 {
		t.Fatal("a refused chain test ran")
	}
}

func TestLastChainTestKeepsThePreviousResultWhileOneRuns(t *testing.T) {
	svc, _ := pitrService(t, nil)
	st := svcStore(t, svc)
	ctx := context.Background()
	done := &models.RestoreRecord{ID: "rst_pitr_old", Status: models.RestoreStatusCompleted, StartedAt: time.Unix(1000, 0), DurationSeconds: 5,
		PITR: &models.PITRRestore{StreamID: "str_a", ChainTest: true}, Verification: &models.RestoreVerification{Status: models.RestoreVerificationFailed}}
	running := &models.RestoreRecord{ID: "rst_pitr_new", Status: models.RestoreStatusInProgress, StartedAt: time.Unix(2000, 0),
		PITR: &models.PITRRestore{StreamID: "str_a", ChainTest: true}}
	for _, r := range []*models.RestoreRecord{done, running} {
		if err := st.SaveRestoreRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	res := svc.LastChainTest(ctx, "str_a")
	if res == nil || res.RestoreID != "rst_pitr_old" || !res.Failed {
		t.Fatalf("result = %+v, want the finished failed test", res)
	}
	if !svc.LastChainTestStart(ctx, "str_a").Equal(time.Unix(2000, 0)) {
		t.Fatal("the schedule does not see the running test")
	}
}

func TestCleanupInterruptedPITRDropsTheRecordedClones(t *testing.T) {
	svc, fake := pitrService(t, nil)
	rec := &models.RestoreRecord{ID: "rst_pitr_x", TargetConnectionID: "conn_a", Status: models.RestoreStatusInProgress,
		PITR: &models.PITRRestore{StreamID: "str_a", CloneSuffix: "_rescue_cvabc1234", Clones: []string{"shop_rescue_cvabc1234"}}}
	if note := svc.CleanupInterruptedPITR(context.Background(), rec); note != "; dropped" {
		t.Fatalf("note = %q", note)
	}
	if len(fake.dropped) != 1 || fake.dropped[0] != "_rescue_cvabc1234" {
		t.Fatalf("dropped %v", fake.dropped)
	}
	if note := svc.CleanupInterruptedPITR(context.Background(), &models.RestoreRecord{ID: "rst_plain"}); note != "" {
		t.Fatalf("a database restore was cleaned: %q", note)
	}
}

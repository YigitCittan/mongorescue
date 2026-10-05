package operations_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// chainTestBase is base b2 of stream str_a, consistent at 125, whose manifest
// holds shop.orders with docs documents.
func chainTestBase(docs int64) *models.BackupRecord {
	before, after := pitr.OpTime{TS: pitr.Timestamp{T: 122, I: 1}, Term: 1}, pitr.OpTime{TS: pitr.Timestamp{T: 125, I: 1}, Term: 1}
	return &models.BackupRecord{ID: "b2", Scope: models.ScopeInstance, PITRStreamID: "str_a",
		ConnectionID: "conn_a", StorageTargetID: "tgt", StorageKey: "_mongorescue/base/conn_a/rs0/2026/10/b2.archive.gz.age",
		Status: models.StatusCompleted, StartedAt: time.Unix(121, 0), SizeBytes: 1000, Encrypted: true, EncryptionMode: "x25519",
		ServerVersion: "8.0.4", TBefore: &before, TAfter: &after, HasManifest: true,
		Manifest: &models.Manifest{Collections: []models.CollectionManifest{{Name: "shop.orders", DocumentsMin: docs, DocumentsMax: docs}}}}
}

func awaitChainTest(t *testing.T, svc *operations.Service, fake *fakePITR) *operations.ChainTestResult {
	t.Helper()
	select {
	case <-fake.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the chain test did not run")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r := svc.LastChainTest(admin(), "str_a"); r != nil {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatal("no chain test result")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestChainTestComparesWithTheNewerBase(t *testing.T) {
	inspector := healthyInspector()
	inspector.manifest = &models.Manifest{Collections: []models.CollectionManifest{{Name: "orders", DocumentsMin: 7, DocumentsMax: 7}}}
	svc, fake := pitrService(t, inspector, chainTestBase(7))
	rec, err := svc.StartChainTest(admin(), "str_a")
	if err != nil {
		t.Fatalf("StartChainTest: %v", err)
	}
	if rec.PITR == nil || !rec.PITR.ChainTest || rec.PITR.BaseID != "b1" || !strings.HasPrefix(rec.PITR.CloneSuffix, "_rescue_verify_") ||
		rec.PITR.Limit != (pitr.Timestamp{T: 125, I: 2}) {
		t.Fatalf("record = %+v, pitr = %+v", rec, rec.PITR)
	}
	res := awaitChainTest(t, svc, fake)
	if res.Failed || res.Verification != models.RestoreVerificationPassed {
		t.Fatalf("result = %+v", res)
	}
	fake.mu.Lock()
	dropped := len(fake.dropped)
	fake.mu.Unlock()
	if dropped != 1 {
		t.Fatalf("clones dropped %d times, want 1", dropped)
	}
	inspector.mu.Lock()
	defer inspector.mu.Unlock()
	if len(inspector.inspected) != 1 || inspector.inspected[0] != "shop"+rec.PITR.CloneSuffix {
		t.Fatalf("inspected %v", inspector.inspected)
	}
	if !svc.LastChainTestStart(admin(), "str_a").Equal(rec.StartedAt) {
		t.Fatal("the schedule does not see the chain test")
	}
}

func TestChainTestMismatchFails(t *testing.T) {
	inspector := healthyInspector()
	inspector.manifest = &models.Manifest{Collections: []models.CollectionManifest{{Name: "orders", DocumentsMin: 6, DocumentsMax: 6}}}
	svc, fake := pitrService(t, inspector, chainTestBase(7))
	if _, err := svc.StartChainTest(admin(), "str_a"); err != nil {
		t.Fatal(err)
	}
	if res := awaitChainTest(t, svc, fake); !res.Failed || res.Verification != models.RestoreVerificationFailed {
		t.Fatalf("result = %+v", res)
	}
	// The RTO estimate now uses the chain test's rate.
	pre, err := svc.PreflightRestore(admin(), pitrAt(125))
	if err != nil {
		t.Fatal(err)
	}
	if pre.PITR == nil || pre.PITR.EstimateFrom != "chain_test" {
		t.Fatalf("estimate = %+v", pre.PITR)
	}
}

func TestChainTestRefusals(t *testing.T) {
	svc, _ := pitrService(t, nil)
	if _, err := svc.StartChainTest(admin(), "str_a"); !errors.Is(err, operations.ErrNoChainTest) {
		t.Fatalf("one base: err = %v, want ErrNoChainTest", err)
	}
	b2 := chainTestBase(1)
	b2.HasManifest, b2.Manifest = false, nil
	svc, _ = pitrService(t, nil, b2)
	if _, err := svc.StartChainTest(admin(), "str_a"); !errors.Is(err, operations.ErrNoChainTest) {
		t.Fatalf("no manifest: err = %v, want ErrNoChainTest", err)
	}
	svc, _ = pitrService(t, nil, chainTestBase(1))
	if _, err := svc.StartChainTest(operator(), "str_a"); err == nil {
		t.Fatal("an operator started a chain test")
	}
}

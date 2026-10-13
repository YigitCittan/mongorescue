package operations_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// chainTestBase is base b2 of stream str_a, consistent at 125, whose manifest
// holds shop.orders with docs documents there (and before and after the dump).
func chainTestBase(docs int64) *models.BackupRecord {
	return chainTestBaseCounts(docs, docs, &docs)
}

// chainTestBaseCounts is chainTestBase with shop.orders counted min and max
// documents around the dump and exact at the consistent point (nil: a manifest of
// an earlier release, without exact counts).
func chainTestBaseCounts(minDocs, maxDocs int64, exact *int64) *models.BackupRecord {
	before, after := pitr.OpTime{TS: pitr.Timestamp{T: 122, I: 1}, Term: 1}, pitr.OpTime{TS: pitr.Timestamp{T: 125, I: 1}, Term: 1}
	m := &models.Manifest{Collections: []models.CollectionManifest{{Name: "shop.orders", DocumentsMin: minDocs, DocumentsMax: maxDocs}}}
	if exact != nil {
		m.SetExact(&models.ExactCounts{At: after.TS, Method: models.CountMethodSnapshot, Counts: map[string]int64{"shop.orders": *exact}})
	}
	return &models.BackupRecord{ID: "b2", Scope: models.ScopeInstance, PITRStreamID: "str_a",
		ConnectionID: "conn_a", StorageTargetID: "tgt", StorageKey: "_mongorescue/base/conn_a/rs0/2026/10/b2.archive.gz.age",
		Status: models.StatusCompleted, StartedAt: time.Unix(121, 0), SizeBytes: 1000, Encrypted: true, EncryptionMode: "x25519",
		ServerVersion: "8.0.4", TBefore: &before, TAfter: &after, HasManifest: true, Manifest: m}
}

// chainTestVerification runs a chain test of a clone holding docs orders against
// base b2 and returns its comparison.
func chainTestVerification(t *testing.T, b2 *models.BackupRecord, docs int64) (*operations.ChainTestResult, *models.RestoreVerification) {
	t.Helper()
	inspector := healthyInspector()
	inspector.manifest = &models.Manifest{Collections: []models.CollectionManifest{{Name: "orders", DocumentsMin: docs, DocumentsMax: docs}}}
	svc, fake := pitrService(t, inspector, b2)
	if _, err := svc.StartChainTest(admin(), "str_a"); err != nil {
		t.Fatal(err)
	}
	res := awaitChainTest(t, svc, fake)
	rec, err := pitrStores[svc].GetRestoreRecord(context.Background(), res.RestoreID)
	if err != nil || rec.Verification == nil {
		t.Fatalf("record %+v: %v", rec, err)
	}
	return res, rec.Verification
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
	if rec.PITR == nil || !rec.PITR.ChainTest || rec.PITR.BaseID != "b1" || !strings.HasPrefix(rec.PITR.CloneSuffix, "_rescue_cv") || len(rec.PITR.CloneSuffix) != 20 ||
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

// TestChainTestUsesTheExactCountAtTheConsistentPoint is issue #181: a write raced
// the dump, so the counts before and after it (20000) are not the state at the
// consistent point (20001), which the restore holds.
func TestChainTestUsesTheExactCountAtTheConsistentPoint(t *testing.T) {
	exact := int64(20001)
	res, v := chainTestVerification(t, chainTestBaseCounts(20000, 20000, &exact), 20001)
	if res.Failed || v.Status != models.RestoreVerificationPassed || v.Unverified != 0 || len(v.Mismatches) != 0 {
		t.Fatalf("result = %+v, verification = %+v", res, v)
	}
}

// TestChainTestExactCountIsStrict: a restore that diverges from the state at the
// consistent point fails, even within the range read around the dump.
func TestChainTestExactCountIsStrict(t *testing.T) {
	exact := int64(20001)
	for _, docs := range []int64{20000, 20002} {
		res, v := chainTestVerification(t, chainTestBaseCounts(20000, 20002, &exact), docs)
		if !res.Failed || v.Status != models.RestoreVerificationFailed ||
			len(v.Mismatches) != 1 || !strings.Contains(v.Mismatches[0], fmt.Sprintf("%d documents restored, 20001 expected", docs)) {
			t.Fatalf("%d documents: result = %+v, verification = %+v", docs, res, v)
		}
	}
}

// TestChainTestWithoutExactCountsIsUnverified: a base without exact counts (one of
// an earlier release, or whose counts could not be taken) leaves the counts
// unverified rather than failing; a missing collection still fails.
func TestChainTestWithoutExactCountsIsUnverified(t *testing.T) {
	res, v := chainTestVerification(t, chainTestBaseCounts(20000, 20000, nil), 20001)
	if res.Failed || v.Status != models.RestoreVerificationPassed || v.Unverified != 1 || len(v.Mismatches) != 0 ||
		!v.UnverifiedOldBase || v.UnverifiedReason != models.ChainTestOldBaseReason ||
		len(v.Notes) != 1 || !strings.Contains(v.Notes[0], "shop.orders: 20001 documents restored, count not verified: the base was taken by an earlier release") {
		t.Fatalf("result = %+v, verification = %+v", res, v)
	}
	// The stream's newest chain test says so, not just that it passed.
	if res.Unverified != 1 || !res.OldBase || res.UnverifiedReason != models.ChainTestOldBaseReason {
		t.Fatalf("result = %+v", res)
	}
	b2 := chainTestBaseCounts(20000, 20000, nil)
	b2.Manifest.SetExact(&models.ExactCounts{At: b2.TAfter.TS, Method: models.CountMethodOplog,
		Unverified: map[string]string{"shop.orders": "dropped at Timestamp(126, 1)"}})
	b2.Manifest.Collections = append(b2.Manifest.Collections, models.CollectionManifest{Name: "shop.gone", ExactNote: "dropped"})
	res, v = chainTestVerification(t, b2, 7)
	if !res.Failed || v.Unverified != 2 || len(v.Mismatches) != 1 || !strings.Contains(v.Mismatches[0], "shop.gone is missing") ||
		v.UnverifiedOldBase || v.UnverifiedReason != "dropped" || // shop.gone's, the first by name

		!strings.Contains(strings.Join(v.Notes, "|"), "count not verified: dropped at Timestamp(126, 1)") {
		t.Fatalf("result = %+v, verification = %+v", res, v)
	}
}

// TestChainTestChecksExistenceAtTheConsistentPoint: a collection that existed at
// the consistent point must be restored, even empty; one that did not (dropped
// during the base backup) must not be.
func TestChainTestChecksExistenceAtTheConsistentPoint(t *testing.T) {
	base := func(absent bool) *models.BackupRecord {
		b2 := chainTestBase(7)
		b2.Manifest.Collections = append(b2.Manifest.Collections, models.CollectionManifest{Name: "shop.tmp", DocumentsMin: 4, DocumentsMax: 4})
		counts := &models.ExactCounts{At: b2.TAfter.TS, Method: models.CountMethodSnapshot, Counts: map[string]int64{"shop.orders": 7, "shop.tmp": 0}}
		if absent {
			counts.Absent = map[string]bool{"shop.tmp": true}
		}
		b2.Manifest.SetExact(counts)
		return b2
	}
	// Existed, empty, and missing from the restore: a divergence.
	res, v := chainTestVerification(t, base(false), 7)
	if !res.Failed || len(v.Mismatches) != 1 || !strings.Contains(v.Mismatches[0], "shop.tmp is missing") {
		t.Fatalf("existed: result = %+v, verification = %+v", res, v)
	}
	// Gone at the consistent point and not restored: a note.
	res, v = chainTestVerification(t, base(true), 7)
	if res.Failed || v.Status != models.RestoreVerificationPassed || len(v.Notes) != 1 || !strings.Contains(v.Notes[0], "shop.tmp did not exist") {
		t.Fatalf("absent: result = %+v, verification = %+v", res, v)
	}
	// Gone at the consistent point, yet restored: a divergence.
	inspector := healthyInspector()
	inspector.manifest = &models.Manifest{Collections: []models.CollectionManifest{
		{Name: "orders", DocumentsMin: 7, DocumentsMax: 7}, {Name: "tmp", DocumentsMin: 0, DocumentsMax: 0}}}
	svc, fake := pitrService(t, inspector, base(true))
	if _, err := svc.StartChainTest(admin(), "str_a"); err != nil {
		t.Fatal(err)
	}
	if res = awaitChainTest(t, svc, fake); !res.Failed || res.Verification != models.RestoreVerificationFailed {
		t.Fatalf("absent but restored: result = %+v", res)
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

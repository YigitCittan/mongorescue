package models

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCompareManifests(t *testing.T) {
	ttl := int64(60)
	expected := &Manifest{Collections: []CollectionManifest{
		{Name: "orders", DocumentsMin: 10, DocumentsMax: 12, Indexes: []IndexSpec{{Name: "_id_", Keys: "_id:1"}, {Name: "sku_1", Keys: "sku:1", Unique: true}}},
		{Name: "sessions", DocumentsMin: 3, DocumentsMax: 3, Indexes: []IndexSpec{{Name: "exp_1", Keys: "exp:1", ExpireAfterSeconds: &ttl}}},
		{Name: "users", DocumentsMin: 5, DocumentsMax: 5},
	}}
	tests := []struct {
		name       string
		actual     *Manifest
		mismatches []string
		notes      int
	}{
		{"identical within range", &Manifest{Collections: []CollectionManifest{
			{Name: "orders", DocumentsMin: 11, DocumentsMax: 11, Indexes: []IndexSpec{{Name: "_id_", Keys: "_id:1"}, {Name: "sku_1", Keys: "sku:1", Unique: true}}},
			{Name: "sessions", DocumentsMin: 3, DocumentsMax: 3, Indexes: []IndexSpec{{Name: "exp_1", Keys: "exp:1", ExpireAfterSeconds: &ttl}}},
			{Name: "users", DocumentsMin: 5, DocumentsMax: 5},
			{Name: "late", DocumentsMin: 1, DocumentsMax: 1},
		}}, nil, 1},
		{"count, index and collection differences", &Manifest{Collections: []CollectionManifest{
			{Name: "orders", DocumentsMin: 9, DocumentsMax: 9, Indexes: []IndexSpec{{Name: "_id_", Keys: "_id:1"}, {Name: "sku_1", Keys: "sku:1"}}},
			{Name: "sessions", DocumentsMin: 3, DocumentsMax: 3},
		}}, []string{"orders: 9 documents restored, 10 to 12 expected", "index sku_1 is sku:1, expected sku:1 unique", "index exp_1 (exp:1 ttl=60s) is missing", "users is missing"}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mismatches, notes := CompareManifests(expected, tc.actual)
			if len(mismatches) != len(tc.mismatches) || len(notes) != tc.notes {
				t.Fatalf("mismatches %q notes %q", mismatches, notes)
			}
			for i, want := range tc.mismatches {
				if !strings.Contains(mismatches[i], want) {
					t.Errorf("mismatch %d = %q, want it to contain %q", i, mismatches[i], want)
				}
			}
		})
	}
	if m, n := CompareManifests(nil, &Manifest{}); m != nil || n != nil {
		t.Fatalf("no expected manifest must compare nothing: %v %v", m, n)
	}
}

func TestManifestMergeCountsAndNormalize(t *testing.T) {
	m := &Manifest{Collections: []CollectionManifest{{Name: "b", DocumentsMin: 5, DocumentsMax: 5}, {Name: "a", DocumentsMin: 9, DocumentsMax: 2}}}
	m.Normalize()
	if m.Collections[0].Name != "a" || m.Collections[0].DocumentsMin != 2 || m.Collections[0].DocumentsMax != 9 {
		t.Fatalf("normalize: %+v", m.Collections)
	}
	m.MergeCounts(&Manifest{Collections: []CollectionManifest{{Name: "b", DocumentsMin: 8, DocumentsMax: 8}, {Name: "c", DocumentsMin: 1, DocumentsMax: 1}}})
	if b := m.Collection("b"); b.DocumentsMin != 5 || b.DocumentsMax != 8 || m.Collection("c") != nil {
		t.Fatalf("merge: %+v", m.Collections)
	}
	if m.Documents() != 17 {
		t.Fatalf("documents = %d", m.Documents())
	}
}

func TestRestoreTestPolicyValidate(t *testing.T) {
	p := &RestoreTestPolicy{Enabled: true}
	if err := p.Validate(); err != nil || p.Frequency != RestoreTestWeekly || p.Interval() != 7*24*time.Hour {
		t.Fatalf("default = %+v, %v", p, err)
	}
	for _, bad := range []RestoreTestPolicy{{Frequency: "hourly"}, {Frequency: RestoreTestEveryN}, {Frequency: RestoreTestEveryN, EveryN: MaxRestoreTestEveryN + 1}} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidRestoreTest) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	ok := &RestoreTestPolicy{Frequency: RestoreTestEveryN, EveryN: 5, ConnectionID: " c "}
	if err := ok.Validate(); err != nil || ok.ConnectionID != "c" || ok.Interval() != 0 {
		t.Fatalf("every_n = %+v, %v", ok, err)
	}
}

func TestRescueVerifyDatabaseName(t *testing.T) {
	at := time.Date(2026, 10, 1, 8, 9, 10, 0, time.UTC)
	if got, err := RescueVerifyDatabaseName("shop", at, "a1b2c3"); err != nil || got != "shop_rescue_verify_20261001_080910_a1b2c3" || !IsRescueVerifyDatabaseName(got) {
		t.Fatalf("name = %q, %v", got, err)
	}
	long := strings.Repeat("x", 80)
	if got, _ := RescueVerifyDatabaseName(long, at, "000000"); len(got) != MaxDatabaseNameLength || ValidateDatabaseName(got) != nil || !IsRescueVerifyDatabaseName(got) {
		t.Fatalf("long name = %q", got)
	}
	for _, bad := range []string{"", "ABCDEF", "a1b2c", "a1b2c3d", "zzzzzz"} {
		if _, err := RescueVerifyDatabaseName("shop", at, bad); !errors.Is(err, ErrInvalidVerifySuffix) {
			t.Errorf("suffix %q accepted", bad)
		}
	}
	s1, err1 := NewRescueVerifySuffix()
	s2, err2 := NewRescueVerifySuffix()
	if err1 != nil || err2 != nil || s1 == s2 || !isLowerHex(s1, RescueVerifySuffixLength) {
		t.Fatalf("suffixes %q %q", s1, s2)
	}
	for _, n := range []string{"shop", "shop_rescue_20261001_080910", "_rescue_verify_20261001_080910_a1b2c3", "shop_rescue_verify_x",
		"shop_rescue_verify_20261001_080910", "shop_rescue_verify_20261001_080910_A1B2C3", "shop_rescue_verify_20261001_080910_a1b2c3x"} {
		if IsRescueVerifyDatabaseName(n) {
			t.Errorf("%q must not look like a restore test database", n)
		}
	}
}

func TestVerifyOverride(t *testing.T) {
	if !VerifyInherit.Resolve(true) || VerifyInherit.Resolve(false) || !VerifyOn.Resolve(false) || VerifyOff.Resolve(true) {
		t.Fatal("resolve")
	}
	if VerifyOverride("maybe").Valid() || !VerifyOff.Valid() {
		t.Fatal("valid")
	}
}

func TestRestoreTestSummary(t *testing.T) {
	done := time.Now().UTC()
	r := &RestoreTestResult{ID: "rt", Status: RestoreTestMismatch, StartedAt: done.Add(-time.Minute), CompletedAt: &done, Mismatches: []string{"a", "b"}}
	s := r.Summary()
	if !s.At.Equal(done) || s.Detail != "a (+1 more)" {
		t.Fatalf("summary = %+v", s)
	}
	if (*RestoreTestResult)(nil).Summary() != nil {
		t.Fatal("nil summary")
	}
}

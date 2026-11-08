package models

import (
	"errors"
	"testing"
	"time"
)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestRetentionPolicyValidate(t *testing.T) {
	tests := []struct {
		name string
		p    RetentionPolicy
		ok   bool
	}{
		{"empty", RetentionPolicy{}, true},
		{"full", RetentionPolicy{RetentionDays: 30, RetentionCount: 5, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12, KeepYearly: 7}, true},
		{"at the bounds", RetentionPolicy{KeepDaily: MaxKeepDaily, KeepWeekly: MaxKeepWeekly, KeepMonthly: MaxKeepMonthly, KeepYearly: MaxKeepYearly}, true},
		{"negative days", RetentionPolicy{RetentionDays: -1}, false},
		{"negative daily", RetentionPolicy{KeepDaily: -1}, false},
		{"negative yearly", RetentionPolicy{KeepYearly: -3}, false},
		{"daily too large", RetentionPolicy{KeepDaily: MaxKeepDaily + 1}, false},
		{"weekly too large", RetentionPolicy{KeepWeekly: MaxKeepWeekly + 1}, false},
		{"monthly too large", RetentionPolicy{KeepMonthly: MaxKeepMonthly + 1}, false},
		{"yearly too large", RetentionPolicy{KeepYearly: MaxKeepYearly + 1}, false},
	}
	for _, tc := range tests {
		err := tc.p.Validate()
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrInvalidRetention)) {
			t.Errorf("%s: Validate() = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	if (RetentionPolicy{}).Active() || !(RetentionPolicy{KeepYearly: 1}).Active() || (RetentionPolicy{RetentionDays: 1}).GFS() {
		t.Error("Active and GFS misreport")
	}
}

func TestValidateCopyRetention(t *testing.T) {
	targets := []string{"a", "b"}
	if err := ValidateCopyRetention(nil, targets); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCopyRetention(map[string]RetentionPolicy{"a": {KeepMonthly: 12}}, targets); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]map[string]RetentionPolicy{
		"unknown target": {"c": {KeepMonthly: 1}},
		"no rule":        {"a": {}},
		"out of range":   {"a": {KeepYearly: MaxKeepYearly + 1}},
		"negative":       {"b": {RetentionDays: -5}},
	} {
		if err := ValidateCopyRetention(o, targets); !errors.Is(err, ErrInvalidRetention) {
			t.Errorf("%s: %v, want ErrInvalidRetention", name, err)
		}
	}
}

func TestJobRetentionLocation(t *testing.T) {
	for expr, want := range map[string]string{
		"@daily":                            "UTC",
		"0 2 * * *":                         "UTC",
		"CRON_TZ=Europe/Istanbul 0 2 * * *": "Europe/Istanbul",
		"TZ=America/New_York @daily":        "America/New_York",
		"CRON_TZ=Nowhere/Land @daily":       "UTC",
		"CRON_TZ=Local @daily":              "UTC",
		"":                                  "UTC",
	} {
		j := &Job{CronExpression: expr}
		if got := j.RetentionLocation().String(); got != want {
			if (want == "UTC") || got == "UTC" {
				t.Errorf("%q: zone %s, want %s", expr, got, want)
			} else {
				t.Skipf("time zone data unavailable: %v", got)
			}
		}
	}
}

func TestJobCloneCopiesCopyRetention(t *testing.T) {
	j := &Job{CopyRetention: map[string]RetentionPolicy{"a": {KeepDaily: 1}}}
	c := j.Clone()
	c.CopyRetention["a"] = RetentionPolicy{KeepDaily: 9}
	if j.CopyRetention["a"].KeepDaily != 1 {
		t.Fatal("Clone shares the copy_retention map")
	}
}

func TestRetainedView(t *testing.T) {
	r := &BackupRecord{ID: "b", Status: StatusDeleted, StorageTargetID: "p", StorageKey: "k", SHA256: "aa",
		Copies: []BackupCopy{{TargetID: "lt", StorageKey: "k", Status: CopyDone, SHA256: "aa", Retained: true}, {TargetID: "x", Status: CopyDone, SHA256: "aa"}}}
	if v := r.RetainedView("lt"); v == nil || v.Status != StatusCompleted || v.StorageTargetID != "lt" {
		t.Fatalf("view = %+v", v)
	}
	if r.RetainedView("x") != nil || r.RetainedView("") != nil || r.RetainedView("none") != nil {
		t.Fatal("a copy that is not retained gives no view")
	}
	r.Copies[0].SHA256 = "bb"
	if r.RetainedView("lt") != nil {
		t.Fatal("a copy of another archive gives no view")
	}
	if !r.HasRetainedCopies() || !r.ReleaseCopies() || r.HasRetainedCopies() || r.ReleaseCopies() {
		t.Fatal("HasRetainedCopies / ReleaseCopies")
	}
}

func TestPurgeDueIgnoresTheLockOfARetainedCopy(t *testing.T) {
	until := mustTime("2030-01-01T00:00:00Z")
	now := mustTime("2026-11-20T00:00:00Z")
	del := mustTime("2026-11-01T00:00:00Z")
	purge := mustTime("2026-11-08T00:00:00Z")
	r := &BackupRecord{ID: "b", Status: StatusDeleted, DeletedAt: &del, PurgeAfter: &purge,
		Copies: []BackupCopy{{TargetID: "lt", Status: CopyDone, RetainUntil: &until, Retained: true}}}
	if !r.PurgeDue(now, 0) {
		t.Fatal("a retained copy's lock must not hold back the purge of the primary")
	}
	if !r.LockedAt(now) {
		t.Fatal("LockedAt still reports the copy's lock")
	}
	r.Copies[0].Retained = false
	if r.PurgeDue(now, 0) {
		t.Fatal("a released locked copy holds the purge back")
	}
}

package models

import (
	"errors"
	"testing"
	"time"
)

func TestNormalizeCopyTargets(t *testing.T) {
	got, err := NormalizeCopyTargets([]string{" a ", "b", "a"}, "p")
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("NormalizeCopyTargets = %v, %v", got, err)
	}
	for _, in := range [][]string{{"p"}, {""}, {"a", "b", "c", "d"}} {
		if _, err := NormalizeCopyTargets(in, "p"); !errors.Is(err, ErrInvalidCopyTargets) {
			t.Fatalf("NormalizeCopyTargets(%v) = %v", in, err)
		}
	}
}

func TestCopyLocksAndHolds(t *testing.T) {
	now := time.Now()
	r := &BackupRecord{StorageTargetID: "p", StorageKey: "k"}
	r.PlanCopies([]CopyTarget{{ID: "c"}}, "")
	if r.CopyMode != CopyAsync || r.Copies[0].StorageKey != "k" || r.CopiesComplete() {
		t.Fatalf("PlanCopies = %+v", r)
	}
	r.Copies[0].Attempts = 1
	if !r.HoldsTarget("c") {
		t.Fatal("a pending copy that was attempted may exist")
	}
	later := now.Add(time.Hour)
	r.Copies[0].RetainUntil = &later
	if !r.LockedAt(now) {
		t.Fatal("a locked copy locks the record")
	}
	r.Copies[0].Status = CopyPurged
	if r.LockedAt(now) || r.HoldsTarget("c") {
		t.Fatal("a purged copy neither locks nor holds")
	}
}

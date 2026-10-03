package pitr_test

import (
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/pitr"
)

func TestTimestampOrder(t *testing.T) {
	a, b, c := pitr.Timestamp{T: 9, I: 7}, pitr.Timestamp{T: 10, I: 1}, pitr.Timestamp{T: 10, I: 2}
	if a.Compare(b) != -1 || b.Compare(c) != -1 || c.Compare(a) != 1 || b.Compare(b) != 0 {
		t.Fatal("Compare does not order by seconds, then ordinal")
	}
	// String sorts in time order, as chunk keys must.
	strs := []string{c.String(), a.String(), b.String()}
	sort.Strings(strs)
	if !slices.Equal(strs, []string{a.String(), b.String(), c.String()}) {
		t.Errorf("strings sort as %v", strs)
	}
	if got := (pitr.Timestamp{T: 1790000400, I: 3}).String(); got != "1790000400.0000000003" {
		t.Errorf("String = %q", got)
	}
	if !(pitr.Timestamp{}).IsZero() || a.IsZero() {
		t.Error("IsZero")
	}
	if got := b.Time(); !got.Equal(time.Unix(10, 0)) || got.Location() != time.UTC {
		t.Errorf("Time = %v", got)
	}
}

func TestChainOpen(t *testing.T) {
	c := &pitr.Chain{}
	if !c.Open() {
		t.Error("a chain without EndedAt is open")
	}
	now := time.Now()
	c.EndedAt = &now
	if c.Open() {
		t.Error("a chain with EndedAt has ended")
	}
}

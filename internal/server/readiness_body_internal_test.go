package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/readiness"
)

// TestEncodedReportsReuseTheBodyOfASharedReport checks that the same report is
// encoded once, as writeJSON would, that another report gets its own body, and
// that only the newest reports are kept.
func TestEncodedReportsReuseTheBodyOfASharedReport(t *testing.T) {
	var e encodedReports
	report := &readiness.Report{GeneratedAt: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), Rows: []readiness.Row{}}
	first, err := e.encode(report)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := e.encode(report)
	if &first[0] != &again[0] {
		t.Fatal("the same report was encoded twice")
	}
	var env struct {
		Success bool             `json:"success"`
		Data    readiness.Report `json:"data"`
	}
	if err = json.Unmarshal(first, &env); err != nil || !env.Success || !env.Data.GeneratedAt.Equal(report.GeneratedAt) || first[len(first)-1] != '\n' {
		t.Fatalf("body %s: %v", first, err)
	}
	other := &readiness.Report{GeneratedAt: report.GeneratedAt.Add(time.Second)}
	if body, _ := e.encode(other); &body[0] == &first[0] {
		t.Fatal("another report got the first one's body")
	}
	for range maxEncodedReports {
		if _, err = e.encode(&readiness.Report{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.reports) != maxEncodedReports || e.reports[0] == report {
		t.Fatalf("%d reports kept, the oldest still there: %v", len(e.reports), e.reports[0] == report)
	}
}

package store

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

func newAuditTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	key, _ := secretbox.GenerateKey()
	box, _ := secretbox.New(key)
	s, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "mongorescue.db"), slog.New(slog.DiscardHandler), WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// stepClock returns a clock starting at start that advances by step per call.
func stepClock(start time.Time, step time.Duration) func() time.Time {
	var mu sync.Mutex
	now := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := now
		now = now.Add(step)
		return t
	}
}

func TestAuditEventsChainAndFilters(t *testing.T) {
	s := newAuditTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 1, 2, 3, 4, 5, 678901234, time.UTC)
	log := auditlog.New(auditlog.Config{Repo: s, Now: stepClock(start, time.Hour)})
	log.Record(ctx, auditlog.Event{ActorKind: auditlog.ActorUser, ActorUserID: "usr_1", ActorName: "admin",
		Action: "POST /api/v1/jobs/{id}/run", Targets: map[string]string{"id": "job_1"}, Status: 202, ClientIP: "192.0.2.1", UserAgent: "ua"})
	log.Record(ctx, auditlog.Event{ActorKind: auditlog.ActorAPIKey, ActorKeyID: "key_1", ActorKeyName: "ci_100%",
		Action: "DELETE /api/v1/backups/{id}", Targets: map[string]string{"id": "bkp_1"}, Status: 403})
	log.Record(ctx, auditlog.Event{ActorKind: auditlog.ActorAnonymous, ActorName: "root", Action: "POST /api/v1/auth/login", Status: 401})

	page, err := log.List(ctx, auditlog.Filter{})
	if err != nil || len(page.Events) != 3 || page.Events[0].ID != 3 {
		t.Fatalf("List = %+v, %v", page, err)
	}
	first := page.Events[2]
	if !first.Time.Equal(start) || first.Targets["id"] != "job_1" || first.ClientIP != "192.0.2.1" || first.Count != 1 || len(first.Hash) != 64 {
		t.Fatalf("first entry = %+v", first)
	}
	if want, _ := auditlog.ChainHash(auditlog.GenesisHash, first); want != first.Hash {
		t.Fatalf("stored hash %s; want %s", first.Hash, want)
	}
	for _, tc := range []struct {
		f    auditlog.Filter
		want []int64
	}{
		{auditlog.Filter{Actor: "admin"}, []int64{1}},
		{auditlog.Filter{Actor: "key_1"}, []int64{2}},
		{auditlog.Filter{ActorKind: auditlog.ActorAnonymous}, []int64{3}},
		{auditlog.Filter{Action: "backups"}, []int64{2}},
		{auditlog.Filter{Action: "%"}, nil}, // wildcards are literal
		{auditlog.Filter{Action: "post"}, []int64{3, 1}},
		{auditlog.Filter{Outcome: auditlog.OutcomeDenied}, []int64{3, 2}},
		{auditlog.Filter{Since: start.Add(time.Hour)}, []int64{3, 2}},
		{auditlog.Filter{Until: start.Add(time.Hour)}, []int64{1}},
		{auditlog.Filter{BeforeID: 3, Limit: 1}, []int64{2}},
		{auditlog.Filter{AfterID: 1, Ascending: true}, []int64{2, 3}},
	} {
		list, err := s.ListAuditEvents(ctx, tc.f)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, e := range list {
			ids = append(ids, e.ID)
		}
		if len(ids) != len(tc.want) || (len(ids) > 0 && ids[0] != tc.want[0]) || (len(ids) > 1 && ids[1] != tc.want[1]) {
			t.Errorf("filter %+v = %v; want %v", tc.f, ids, tc.want)
		}
	}
	if v, err := log.Verify(ctx); err != nil || !v.OK || v.Checked != 3 {
		t.Fatalf("Verify = %+v, %v", v, err)
	}
}

// TestAuditEventsConcurrentAppendsKeepOneChain proves concurrent writers never fork
// the chain.
func TestAuditEventsConcurrentAppendsKeepOneChain(t *testing.T) {
	s := newAuditTestStore(t)
	ctx := context.Background()
	log := auditlog.New(auditlog.Config{Repo: s})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 10 {
				log.Record(ctx, auditlog.Event{ActorKind: auditlog.ActorSystem, ActorName: "w", Action: "SYSTEM x", Status: 200 + i})
			}
		})
	}
	wg.Wait()
	if v, err := log.Verify(ctx); err != nil || !v.OK || v.Checked != 80 || v.HeadID != 80 {
		t.Fatalf("Verify = %+v, %v", v, err)
	}
}

// TestAuditEventsAreAppendOnly proves the triggers refuse updates, deletions ahead of
// the anchor and moving the anchor back, and that a tampered row is reported.
func TestAuditEventsAreAppendOnly(t *testing.T) {
	s := newAuditTestStore(t)
	ctx := context.Background()
	log := auditlog.New(auditlog.Config{Repo: s})
	for range 3 {
		log.Record(ctx, auditlog.Event{ActorKind: auditlog.ActorUser, ActorName: "admin", Action: "POST /x", Status: 200})
	}
	for _, stmt := range []string{
		"UPDATE audit_events SET actor_name = 'mallory' WHERE id = 2",
		"DELETE FROM audit_events WHERE id = 3",
		"DELETE FROM audit_chain_anchor",
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err == nil {
			t.Errorf("%s succeeded", stmt)
		}
	}
	if n, err := s.PruneAuditEvents(ctx, time.Now().Add(time.Hour)); err != nil || n != 3 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE audit_chain_anchor SET last_id = 1 WHERE id = 1"); err == nil {
		t.Error("the anchor moved back")
	}
	// An attacker who drops the triggers still breaks the chain.
	log.Record(ctx, auditlog.Event{ActorKind: auditlog.ActorUser, ActorName: "admin", Action: "POST /y", Status: 200})
	log.Record(ctx, auditlog.Event{ActorKind: auditlog.ActorUser, ActorName: "admin", Action: "POST /z", Status: 200})
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER audit_events_append_only"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE audit_events SET action = 'POST /hidden' WHERE id = 4"); err != nil {
		t.Fatal(err)
	}
	v, err := log.Verify(ctx)
	if err != nil || v.OK || v.BrokenID != 4 || !strings.Contains(v.Reason, "hash") || v.Anchor.LastID != 3 {
		t.Fatalf("Verify after tampering = %+v, %v", v, err)
	}
}

// TestAuditRetentionMovesTheAnchor proves pruning deletes the oldest entries, keeps
// the last deleted hash as the anchor and leaves a verifiable chain.
func TestAuditRetentionMovesTheAnchor(t *testing.T) {
	s := newAuditTestStore(t)
	ctx := context.Background()
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := stepClock(start, 24*time.Hour)
	log := auditlog.New(auditlog.Config{Repo: s, Now: clock, RetentionDays: func() int { return 30 }})
	for range 50 {
		log.Record(ctx, auditlog.Event{ActorKind: auditlog.ActorUser, ActorName: "admin", Action: "POST /x", Status: 200})
	}
	all, _ := s.ListAuditEvents(ctx, auditlog.Filter{Ascending: true, Limit: 100})
	n, err := log.Prune(ctx) // now is day 50: days 0-19 go
	if err != nil || n != 20 {
		t.Fatalf("Prune = %d, %v; want 20", n, err)
	}
	anchor, err := s.AuditChainAnchor(ctx)
	if err != nil || anchor.LastID != 20 || anchor.LastHash != all[19].Hash || !anchor.LastTime.Equal(all[19].Time) || anchor.PrunedAt.IsZero() {
		t.Fatalf("anchor = %+v, %v", anchor, err)
	}
	v, err := log.Verify(ctx)
	// The prune itself is recorded (entry 51, day 51).
	if err != nil || !v.OK || v.Checked != 31 || v.Anchor.LastID != 20 || v.HeadID != 51 || v.Warning != "" {
		t.Fatalf("Verify = %+v, %v", v, err)
	}
	last, _ := s.ListAuditEvents(ctx, auditlog.Filter{Limit: 1})
	if last[0].Action != auditlog.PruneAction || last[0].Targets["removed"] != "20" || last[0].Targets["anchor_id"] != "20" {
		t.Fatalf("prune entry = %+v", last[0])
	}
	// Every clock read is a day: Verify read day 52, so this prune runs on day 53
	// and removes days 20 to 22.
	if n, _ = log.Prune(ctx); n != 3 {
		t.Fatalf("second Prune = %d; want 3", n)
	}
	log.Record(ctx, auditlog.Event{ActorKind: auditlog.ActorUser, ActorName: "admin", Action: "POST /after", Status: 200})
	if v, _ = log.Verify(ctx); !v.OK || v.HeadID != 53 || v.Checked != 30 || v.Anchor.LastID != 23 {
		t.Fatalf("Verify after prune and append = %+v", v)
	}
}

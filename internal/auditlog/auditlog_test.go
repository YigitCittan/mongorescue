package auditlog

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
)

// memRepo is an in-memory Repository that chains like the SQLite store.
type memRepo struct {
	mu     sync.Mutex
	rows   []*Event
	anchor Anchor
}

func newMemRepo() *memRepo { return &memRepo{anchor: Anchor{LastHash: GenesisHash}} }

func (m *memRepo) AppendAuditEvent(_ context.Context, e *Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	prevID, prevHash := m.anchor.LastID, m.anchor.LastHash
	if n := len(m.rows); n > 0 {
		prevID, prevHash = m.rows[n-1].ID, m.rows[n-1].Hash
	}
	e.ID = prevID + 1
	h, err := ChainHash(prevHash, e)
	if err != nil {
		return err
	}
	e.Hash = h
	cp := *e
	m.rows = append(m.rows, &cp)
	return nil
}

func (m *memRepo) ListAuditEvents(_ context.Context, f Filter) ([]*Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Event
	match := func(e *Event) bool {
		return (f.Actor == "" || f.Actor == e.ActorName || f.Actor == e.ActorUserID || f.Actor == e.ActorKeyID || f.Actor == e.ActorKeyName) &&
			(f.ActorKind == "" || f.ActorKind == e.ActorKind) &&
			(f.Action == "" || strings.Contains(strings.ToLower(e.Action), strings.ToLower(f.Action))) &&
			(f.Outcome == "" || f.Outcome == e.Outcome) &&
			(f.Since.IsZero() || !e.Time.Before(f.Since)) && (f.Until.IsZero() || e.Time.Before(f.Until)) &&
			(f.BeforeID == 0 || e.ID < f.BeforeID) && (f.AfterID == 0 || e.ID > f.AfterID)
	}
	rows := slices.Clone(m.rows)
	if !f.Ascending {
		slices.Reverse(rows)
	}
	for _, e := range rows {
		if match(e) && len(out) < f.Limit {
			cp := *e
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *memRepo) AuditChainAnchor(context.Context) (Anchor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.anchor, nil
}

func (m *memRepo) PruneAuditEvents(_ context.Context, cutoff time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	last := -1
	for i, e := range m.rows {
		if e.Time.Before(cutoff) {
			last = i
		}
	}
	if last < 0 {
		return 0, nil
	}
	m.anchor = Anchor{LastID: m.rows[last].ID, LastHash: m.rows[last].Hash, PrunedAt: time.Now()}
	m.rows = slices.Clone(m.rows[last+1:])
	return int64(last + 1), nil
}

// activityRepo is an audit.Repository that stores nothing.
type activityRepo struct{}

func (activityRepo) AppendAudit(context.Context, *audit.Entry, int) error { return nil }
func (activityRepo) ListAudit(context.Context, int) ([]*audit.Entry, error) {
	return nil, nil
}
func (activityRepo) MergeAudit(context.Context, int64, int, json.RawMessage) error { return nil }

// fixedClock returns a clock that advances by step on every call.
func fixedClock(start time.Time, step time.Duration) func() time.Time {
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

var vectorTime = time.Date(2026, 10, 2, 12, 30, 45, 123456789, time.UTC)

// TestCanonicalVector pins the canonical encoding and the chain hash: changing
// either breaks every stored chain. The hashes were checked independently with
// Python (json.dumps(e, separators=(",", ":"), ensure_ascii=False) and hashlib).
func TestCanonicalVector(t *testing.T) {
	e1 := &Event{ID: 1, Time: vectorTime, ActorKind: ActorUser, ActorUserID: "usr_1", ActorName: "admin",
		Action: "POST /api/v1/jobs/{id}/run", Targets: map[string]string{"id": "job_1", "db": "shop"}, Status: 202,
		Outcome: OutcomeOK, ClientIP: "192.0.2.10", UserAgent: `Mozilla/5.0 <test> & "q"`, Count: 1, Hash: "ignored"}
	const doc1 = `{"id":1,"time":"2026-10-02T12:30:45.123456789Z","actor_kind":"user","actor_user_id":"usr_1","actor_name":"admin","actor_key_id":"","actor_key_name":"","action":"POST /api/v1/jobs/{id}/run","targets":{"db":"shop","id":"job_1"},"status":202,"outcome":"ok","client_ip":"192.0.2.10","user_agent":"Mozilla/5.0 <test> & \"q\"","count":1}`
	const hash1 = "a0924504486a0b9ca23c3c6eb716880a3af5a0d31119c72ec1ec5cc3bf852ac5"
	got, err := Canonical(e1)
	if err != nil || string(got) != doc1 {
		t.Fatalf("Canonical =\n%s, %v\nwant\n%s", got, err, doc1)
	}
	if h, _ := ChainHash(GenesisHash, e1); h != hash1 {
		t.Fatalf("ChainHash(genesis, e1) = %s; want %s", h, hash1)
	}

	e2 := &Event{ID: 2, Time: time.Date(2026, 10, 2, 12, 31, 0, 0, time.UTC), ActorKind: ActorAnonymous, ActorName: "mallory",
		Action: "POST /api/v1/auth/login", Status: 401, Outcome: OutcomeDenied, ClientIP: "198.51.100.7", Count: 3}
	const doc2 = `{"id":2,"time":"2026-10-02T12:31:00Z","actor_kind":"anonymous","actor_user_id":"","actor_name":"mallory","actor_key_id":"","actor_key_name":"","action":"POST /api/v1/auth/login","targets":{},"status":401,"outcome":"denied","client_ip":"198.51.100.7","user_agent":"","count":3}`
	if got, _ := Canonical(e2); string(got) != doc2 {
		t.Fatalf("Canonical(e2) = %s; want %s (nil targets encode as {})", got, doc2)
	}
	if h, _ := ChainHash(hash1, e2); h != "71ea4665433dfeec33bbdff59064f103afa52f7b893748727704a418d0943e15" {
		t.Fatalf("ChainHash(hash1, e2) = %s", h)
	}
	if len(GenesisHash) != 64 || strings.Trim(GenesisHash, "0") != "" {
		t.Fatalf("GenesisHash = %q", GenesisHash)
	}
}

// TestCanonicalMatchesExportedJSON proves a verifier can rebuild the canonical form
// from an exported line: same field names and values, minus the hash.
func TestCanonicalMatchesExportedJSON(t *testing.T) {
	e := &Event{ID: 7, Time: vectorTime, ActorKind: ActorAPIKey, ActorKeyID: "key_1", ActorKeyName: "ci",
		Action: "DELETE /api/v1/backups/{id}", Targets: map[string]string{"id": "bkp_1"}, Status: 200, Outcome: OutcomeOK, Count: 1, Hash: "h"}
	exported, _ := json.Marshal(e)
	var asMap map[string]any
	_ = json.Unmarshal(exported, &asMap)
	delete(asMap, "hash")
	doc, _ := Canonical(e)
	var canon map[string]any
	_ = json.Unmarshal(doc, &canon)
	a, _ := json.Marshal(asMap)
	b, _ := json.Marshal(canon)
	if string(a) != string(b) {
		t.Fatalf("exported %s\ncanonical %s", a, b)
	}
}

func newTestService(repo Repository) *Service {
	return New(Config{Repo: repo, Now: fixedClock(vectorTime, time.Minute)})
}

func record(s *Service, n int) {
	for i := range n {
		s.Record(context.Background(), Event{ActorKind: ActorUser, ActorUserID: "usr_1", ActorName: "admin",
			Action: "POST /api/v1/jobs/{id}/run", Targets: map[string]string{"id": "job_" + string(rune('a'+i))}, Status: 202})
	}
}

func TestChainVerifies(t *testing.T) {
	repo := newMemRepo()
	s := newTestService(repo)
	v, err := s.Verify(context.Background())
	if err != nil || !v.OK || v.Checked != 0 || v.HeadHash != GenesisHash {
		t.Fatalf("empty log = %+v, %v", v, err)
	}
	record(s, 5)
	v, err = s.Verify(context.Background())
	if err != nil || !v.OK || v.Checked != 5 || v.HeadID != 5 || v.HeadHash != repo.rows[4].Hash {
		t.Fatalf("Verify = %+v, %v", v, err)
	}
	for i, e := range repo.rows {
		if e.ID != int64(i+1) || len(e.Hash) != 64 || e.Outcome != OutcomeOK || e.Count != 1 {
			t.Fatalf("row %d = %+v", i, e)
		}
	}
}

// TestTamperDetection proves that changing, removing or reordering entries breaks
// verification at the first affected entry.
func TestTamperDetection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tamper  func(rows []*Event) []*Event
		broken  int64
		message string
	}{
		{"changed field", func(r []*Event) []*Event { r[2].ActorName = "mallory"; return r }, 3, "hash does not match"},
		{"changed status", func(r []*Event) []*Event { r[1].Status = 200; return r }, 2, "hash does not match"},
		{"changed target", func(r []*Event) []*Event { r[3].Targets["id"] = "job_z"; return r }, 4, "hash does not match"},
		{"changed time", func(r []*Event) []*Event { r[0].Time = r[0].Time.Add(time.Nanosecond); return r }, 1, "hash does not match"},
		{"rehashed row", func(r []*Event) []*Event {
			r[2].ActorName = "mallory"
			r[2].Hash, _ = ChainHash(r[1].Hash, r[2])
			return r
		}, 4, "hash does not match"},
		{"removed row", func(r []*Event) []*Event { return slices.Delete(r, 2, 3) }, 4, "entries 3 to 3 are missing"},
		{"swapped rows", func(r []*Event) []*Event { r[1], r[2] = r[2], r[1]; return r }, 3, "follows entry"},
		{"renumbered rows", func(r []*Event) []*Event {
			r = slices.Delete(r, 2, 3)
			for i := 2; i < len(r); i++ {
				r[i].ID--
			}
			return r
		}, 3, "hash does not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMemRepo()
			s := newTestService(repo)
			record(s, 5)
			repo.rows = tc.tamper(repo.rows)
			v, err := s.Verify(context.Background())
			if err != nil || v.OK || v.BrokenID != tc.broken || !strings.Contains(v.Reason, tc.message) {
				t.Fatalf("Verify = %+v, %v; want broken at %d (%s)", v, err, tc.broken, tc.message)
			}
		})
	}
}

// TestRetentionKeepsTheAnchor proves pruning removes the oldest entries, moves the
// anchor to the last removed one and leaves a chain that still verifies.
func TestRetentionKeepsTheAnchor(t *testing.T) {
	repo := newMemRepo()
	clock := fixedClock(vectorTime, 24*time.Hour)
	s := New(Config{Repo: repo, Now: clock, RetentionDays: func() int { return 1 }})
	record(s, 40) // one entry per day
	lastRemovedHash := repo.rows[9].Hash
	// Retention below the minimum is raised to MinRetentionDays: now is day 40, so
	// entries of days 0-9 (before day 10) go.
	n, err := s.Prune(context.Background())
	if err != nil || n != 10 {
		t.Fatalf("Prune = %d, %v; want 10", n, err)
	}
	if repo.anchor.LastID != 10 || repo.anchor.LastHash != lastRemovedHash {
		t.Fatalf("anchor = %+v", repo.anchor)
	}
	v, err := s.Verify(context.Background())
	if err != nil || !v.OK || v.Checked != 30 || v.Anchor.LastID != 10 || v.HeadID != 40 {
		t.Fatalf("Verify after prune = %+v, %v", v, err)
	}
	// Appending continues the chain from the newest entry.
	s.Record(context.Background(), Event{ActorKind: ActorSystem, ActorName: "test", Action: "SYSTEM check", Outcome: OutcomeOK})
	if v, _ = s.Verify(context.Background()); !v.OK || v.HeadID != 41 {
		t.Fatalf("Verify after append = %+v", v)
	}
	// Pruning everything leaves the anchor as the head.
	repo.rows = repo.rows[:0]
	if v, _ = s.Verify(context.Background()); !v.OK || v.HeadID != 10 {
		t.Fatalf("Verify of an empty pruned log = %+v", v)
	}
	// An anchor that does not match the first kept entry is reported.
	repo2 := newMemRepo()
	s2 := newTestService(repo2)
	record(s2, 3)
	repo2.anchor = Anchor{LastID: 1, LastHash: GenesisHash}
	repo2.rows = repo2.rows[1:]
	if v, _ = s2.Verify(context.Background()); v.OK || v.BrokenID != 2 {
		t.Fatalf("Verify with a forged anchor = %+v", v)
	}
}

func TestRecordNormalizes(t *testing.T) {
	repo := newMemRepo()
	s := newTestService(repo)
	ctx := WithClient(context.Background(), Client{IP: "203.0.113.5", UserAgent: strings.Repeat("ü", 300)})
	s.Record(ctx, Event{ActorKind: "root", ActorName: "ad\nmin \x00", Action: "POST /x\r\n", Status: 503,
		Targets: map[string]string{"id": "a\tb", "": "dropped"}})
	e := repo.rows[0]
	switch {
	case e.ActorKind != ActorAnonymous, e.ActorName != "admin", e.Action != "POST /x", e.Outcome != OutcomeError,
		e.ClientIP != "203.0.113.5", len(e.UserAgent) > MaxUserAgentLength, !strings.HasPrefix(e.UserAgent, "üü"),
		e.Targets["id"] != "ab", len(e.Targets) != 1, e.Count != 1, e.Time.Location() != time.UTC:
		t.Fatalf("normalized = %+v", e)
	}
	// A client named by the entry wins over the context.
	s.Record(ctx, Event{ActorKind: ActorUser, Action: "POST /y", Status: 200, ClientIP: "198.51.100.1"})
	if repo.rows[1].ClientIP != "198.51.100.1" {
		t.Fatalf("client = %q", repo.rows[1].ClientIP)
	}
	var nilSvc *Service
	nilSvc.Record(ctx, Event{}) // must not panic
}

func TestOutcomeFor(t *testing.T) {
	for status, want := range map[int]string{0: OutcomeOK, 200: OutcomeOK, 302: OutcomeOK, 400: OutcomeError,
		401: OutcomeDenied, 403: OutcomeDenied, 404: OutcomeError, 429: OutcomeRateLimited, 500: OutcomeError} {
		if got := OutcomeFor(status); got != want {
			t.Errorf("OutcomeFor(%d) = %s; want %s", status, got, want)
		}
	}
}

// TestRefusalsAreCoalesced proves a flood of identical refusals stores one entry per
// window, the next one carrying the count, while successes are always stored.
func TestRefusalsAreCoalesced(t *testing.T) {
	repo := newMemRepo()
	clock := fixedClock(vectorTime, time.Second)
	s := New(Config{Repo: repo, Now: clock})
	fail := Event{ActorKind: ActorAnonymous, ActorName: "admin", Action: "POST /api/v1/auth/login", Status: 401, ClientIP: "192.0.2.9"}
	for range 25 { // 25 seconds: windows start at 0s, 10s and 20s
		s.Record(context.Background(), fail)
	}
	if len(repo.rows) != 3 || repo.rows[0].Count != 1 || repo.rows[1].Count != 10 || repo.rows[2].Count != 10 {
		t.Fatalf("rows = %d, counts %v", len(repo.rows), counts(repo.rows))
	}
	other := fail
	other.ClientIP = "192.0.2.10"
	s.Record(context.Background(), other)
	ok := Event{ActorKind: ActorUser, ActorName: "admin", Action: "POST /api/v1/auth/login", Status: 200}
	s.Record(context.Background(), ok)
	s.Record(context.Background(), ok)
	if len(repo.rows) != 6 {
		t.Fatalf("rows = %d; a new caller and successes are stored", len(repo.rows))
	}
	if v, _ := s.Verify(context.Background()); !v.OK {
		t.Fatalf("Verify = %+v", v)
	}
}

func counts(rows []*Event) []int {
	out := make([]int, 0, len(rows))
	for _, e := range rows {
		out = append(out, e.Count)
	}
	return out
}

func TestListPagesAndFilters(t *testing.T) {
	repo := newMemRepo()
	s := newTestService(repo)
	record(s, 5)
	s.Record(context.Background(), Event{ActorKind: ActorAPIKey, ActorKeyID: "key_1", ActorKeyName: "ci", Action: "DELETE /api/v1/backups/{id}", Status: 403})
	page, err := s.List(context.Background(), Filter{Limit: 4})
	if err != nil || len(page.Events) != 4 || page.Events[0].ID != 6 || page.NextBeforeID != 3 {
		t.Fatalf("page 1 = %+v, %v", page, err)
	}
	page, _ = s.List(context.Background(), Filter{Limit: 4, BeforeID: page.NextBeforeID})
	if len(page.Events) != 2 || page.Events[0].ID != 2 || page.NextBeforeID != 0 {
		t.Fatalf("page 2 = %+v", page)
	}
	page, _ = s.List(context.Background(), Filter{Actor: "ci", Outcome: OutcomeDenied})
	if len(page.Events) != 1 || page.Events[0].ActorKeyID != "key_1" {
		t.Fatalf("filtered = %+v", page)
	}
	if _, err := s.List(context.Background(), Filter{Since: vectorTime, Until: vectorTime}); !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("until == since: %v", err)
	}
	var all []int64
	if err := s.Export(context.Background(), Filter{ActorKind: ActorUser}, func(e *Event) error { all = append(all, e.ID); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(all, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("export = %v", all)
	}
	if _, err := (*Service)(nil).List(context.Background(), Filter{}); !errors.Is(err, ErrNoRepository) {
		t.Fatalf("nil service: %v", err)
	}
}

// TestExportWalksInBatches proves the export reads past one batch.
func TestExportWalksInBatches(t *testing.T) {
	repo := newMemRepo()
	s := newTestService(repo)
	for range walkBatch + 7 {
		s.Record(context.Background(), Event{ActorKind: ActorSystem, Action: "SYSTEM x", Outcome: OutcomeOK})
	}
	n := 0
	if err := s.Export(context.Background(), Filter{}, func(*Event) error { n++; return nil }); err != nil || n != walkBatch+7 {
		t.Fatalf("exported %d, %v", n, err)
	}
	if v, _ := s.Verify(context.Background()); !v.OK || v.Checked != int64(walkBatch+7) {
		t.Fatalf("Verify = %+v", v)
	}
}

func TestFromActivity(t *testing.T) {
	ev, ok := FromActivity(audit.Entry{Time: vectorTime, APIKeyID: "key_1", APIKeyName: "claude", Transport: audit.TransportHTTP,
		Tool: "start_backup", Arguments: json.RawMessage(`{"job_id":"job_1","database":"shop","password":"******","id":7}`),
		Result: audit.ResultDenied, Count: 1})
	if !ok || ev.ActorKind != ActorAPIKey || ev.ActorKeyID != "key_1" || ev.Action != "MCP start_backup" ||
		ev.Outcome != OutcomeDenied || len(ev.Targets) != 1 || ev.Targets["job_id"] != "job_1" {
		t.Fatalf("MCP call = %+v, %v", ev, ok)
	}
	ev, ok = FromActivity(audit.Entry{APIKeyName: "retention", Transport: audit.TransportSystem, Tool: "retention.delete",
		Arguments: json.RawMessage(`{"backup_id":"bkp_1"}`), Result: audit.ResultOK})
	if !ok || ev.ActorKind != ActorSystem || ev.ActorName != "retention" || ev.Action != "SYSTEM retention.delete" || ev.Targets["backup_id"] != "bkp_1" {
		t.Fatalf("system action = %+v, %v", ev, ok)
	}
	for _, e := range []audit.Entry{
		{Transport: audit.TransportREST, Tool: "POST /api/v1/backups"},
		{Transport: audit.TransportHTTP, Tool: "resources/read"},
		{Transport: audit.TransportStdio, Tool: "prompts/get"},
	} {
		if _, ok := FromActivity(e); ok {
			t.Errorf("%+v is mirrored", e)
		}
	}
	// The observer records mapped entries.
	repo := newMemRepo()
	s := newTestService(repo)
	act := audit.NewService(activityRepo{}, nil, audit.WithObserver(s.Mirror()))
	act.Record(context.Background(), audit.Entry{APIKeyID: "key_1", Transport: audit.TransportStdio, Tool: "list_backups", Result: audit.ResultOK})
	if len(repo.rows) != 1 || repo.rows[0].Action != "MCP list_backups" {
		t.Fatalf("mirrored rows = %+v", repo.rows)
	}
}

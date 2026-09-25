package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
)

// memRepo is an in-memory audit.Repository.
type memRepo struct {
	mu      sync.Mutex
	entries []*audit.Entry
	err     error
	ctxErr  error
}

func (r *memRepo) AppendAudit(ctx context.Context, e *audit.Entry, keep int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctxErr = ctx.Err()
	if r.err != nil {
		return r.err
	}
	e.ID = int64(len(r.entries) + 1)
	cp := *e
	r.entries = append(r.entries, &cp)
	if len(r.entries) > keep {
		r.entries = r.entries[len(r.entries)-keep:]
	}
	return nil
}

func (r *memRepo) AddAuditCount(_ context.Context, id int64, n int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.ID == id {
			e.Count += n
			return nil
		}
	}
	return audit.ErrEntryNotFound
}

// drop removes every stored entry, as pruning would.
func (r *memRepo) drop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = nil
}

func (r *memRepo) ListAudit(_ context.Context, limit int) ([]*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*audit.Entry, 0, limit)
	for i := len(r.entries) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, r.entries[i])
	}
	return out, nil
}

func TestRedactArguments(t *testing.T) {
	cases := []struct {
		in       string
		want     []string
		mustNot  []string
		exactOut string
	}{
		{in: ``, exactOut: `{}`},
		{in: `not json`, exactOut: `{"_invalid":true}`},
		{in: `{"database":"shop","gzip":true,"collections":["a","b"]}`, want: []string{`"database":"shop"`, `"gzip":true`, `["a","b"]`}},
		{in: `{"password":"hunter22","api_key":"mr_x_y","nested":{"uri":"mongodb://u:p@h/db"}}`,
			want: []string{`"password":"******"`, `"api_key":"******"`, `"uri":"******"`}, mustNot: []string{"hunter22", "mr_x_y", "u:p@"}},
		{in: `{"note":"connect mongodb://root:s3cret@db:27017 now"}`, mustNot: []string{"s3cret"}},
		{in: `{"big":"` + strings.Repeat("x", 5000) + `"}`, exactOut: `{"_truncated":true}`},
	}
	for _, tc := range cases {
		got := string(audit.RedactArguments(json.RawMessage(tc.in)))
		if tc.exactOut != "" && got != tc.exactOut {
			t.Errorf("RedactArguments(%.40q) = %s, want %s", tc.in, got, tc.exactOut)
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("RedactArguments(%.40q) = %s, lacks %s", tc.in, got, w)
			}
		}
		for _, m := range tc.mustNot {
			if strings.Contains(got, m) {
				t.Errorf("RedactArguments(%.40q) = %s leaks %s", tc.in, got, m)
			}
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("RedactArguments(%.40q) is not JSON: %s", tc.in, got)
		}
	}
}

func TestRecordAndList(t *testing.T) {
	repo := &memRepo{}
	svc := audit.NewService(repo, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // an aborted request is still audited
	svc.Record(ctx, audit.Entry{
		APIKeyID: "key_1", APIKeyName: "ci", Transport: audit.TransportHTTP, Tool: "start_backup",
		Arguments: json.RawMessage(`{"database":"shop","uri":"mongodb://a:b@h"}`),
		Result:    audit.ResultError, Error: "failed: mongodb://root:s3cret@db " + strings.Repeat("e", 600), DurationMS: 12,
	})
	if repo.ctxErr != nil {
		t.Fatalf("the audit write must not inherit the request's cancellation: %v", repo.ctxErr)
	}
	list, err := svc.List(context.Background(), 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	e := list[0]
	if e.Time.IsZero() || e.Time.Location() != time.UTC || strings.Contains(string(e.Arguments), "a:b@") ||
		strings.Contains(e.Error, "s3cret") || len(e.Error) > 520 {
		t.Fatalf("entry not sanitised: %+v", e)
	}
}

func TestRecordNeverFails(t *testing.T) {
	repo := &memRepo{err: errors.New("disk full")}
	audit.NewService(repo, nil).Record(context.Background(), audit.Entry{Tool: "get_status"})
	var nilSvc *audit.Service
	nilSvc.Record(context.Background(), audit.Entry{Tool: "get_status"})
	if _, err := nilSvc.List(context.Background(), 10); !errors.Is(err, audit.ErrNoRepository) {
		t.Fatalf("nil service List: %v", err)
	}
}

func TestListLimits(t *testing.T) {
	repo := &memRepo{}
	svc := audit.NewService(repo, nil)
	for range audit.DefaultListLimit + 5 {
		svc.Record(context.Background(), audit.Entry{Tool: "list_jobs", Result: audit.ResultOK})
	}
	list, _ := svc.List(context.Background(), -1)
	if len(list) != audit.DefaultListLimit || list[0].ID < list[1].ID {
		t.Fatalf("default list = %d entries; want %d newest first", len(list), audit.DefaultListLimit)
	}
	if list, _ = svc.List(context.Background(), 3); len(list) != 3 {
		t.Fatalf("limit 3 = %d", len(list))
	}
}

func TestRefusedCallsAreCoalesced(t *testing.T) {
	repo := &memRepo{}
	svc := audit.NewService(repo, nil)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	rec := func(at time.Duration, tool, result string) {
		svc.Record(ctx, audit.Entry{Time: t0.Add(at), APIKeyID: "key_1", Transport: audit.TransportHTTP, Tool: tool, Result: result})
	}
	for i := range 50 {
		rec(time.Duration(i)*100*time.Millisecond, "start_backup", audit.ResultDenied) // 0 .. 4.9s
	}
	rec(time.Second, "run_job", audit.ResultDenied)
	for i := range 20 {
		// Rate limited calls merge whatever the (client-chosen) tool name.
		rec(2*time.Second, fmt.Sprintf("tool_%d", i), audit.ResultRateLimited)
	}
	rec(3*time.Second, "get_status", audit.ResultOK)
	rec(3*time.Second, "get_status", audit.ResultOK)
	rec(11*time.Second, "start_backup", audit.ResultDenied) // a new window

	counts := map[string][]int{}
	list, _ := svc.List(ctx, 100)
	for _, e := range list {
		counts[e.Tool+"/"+e.Result] = append(counts[e.Tool+"/"+e.Result], e.Count)
	}
	want := map[string][]int{
		"start_backup/denied": {1, 50},
		"run_job/denied":      {1},
		"tool_0/rate_limited": {20},
		"get_status/ok":       {1, 1},
	}
	if fmt.Sprint(counts) != fmt.Sprint(want) {
		t.Fatalf("entries by tool/result = %v; want %v", counts, want)
	}

	// An entry pruned meanwhile is replaced by a new one.
	repo.drop()
	rec(12*time.Second, "start_backup", audit.ResultDenied)
	if list, _ = svc.List(ctx, 10); len(list) != 1 || list[0].Count != 1 {
		t.Fatalf("after pruning = %+v; want one new entry", list)
	}

	// Coalesce merges other entries too, per key.
	for range 3 {
		svc.Record(ctx, audit.Entry{Time: t0.Add(13 * time.Second), APIKeyID: "key_2", Tool: "GET /api/v1/stats", Result: audit.ResultOK, Coalesce: true})
	}
	if list, _ = svc.List(ctx, 1); list[0].APIKeyID != "key_2" || list[0].Count != 3 {
		t.Fatalf("coalesced entry = %+v; want count 3", list[0])
	}
}

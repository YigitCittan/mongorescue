package audit_test

import (
	"context"
	"encoding/json"
	"errors"
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

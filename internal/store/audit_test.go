package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestAuditLogAppendListPrune(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	for i := range 5 {
		e := &audit.Entry{
			Time: base.Add(time.Duration(i) * time.Second), APIKeyID: "key_1", APIKeyName: "ci",
			Transport: audit.TransportStdio, Tool: "get_backup", Arguments: json.RawMessage(`{"id":"bkp_1"}`),
			Result: audit.ResultOK, DurationMS: int64(i),
		}
		if err := s.AppendAudit(ctx, e, 3); err != nil {
			t.Fatal(err)
		}
		if e.ID == 0 {
			t.Fatal("AppendAudit must assign an ID")
		}
	}
	list, err := s.ListAudit(ctx, 10)
	if err != nil || len(list) != 3 {
		t.Fatalf("ListAudit = %d entries, %v; want the newest 3", len(list), err)
	}
	if list[0].DurationMS != 4 || list[2].DurationMS != 2 || !list[0].Time.Equal(base.Add(4*time.Second)) {
		t.Fatalf("entries out of order or lost fields: %+v", list[0])
	}
	if list[0].Transport != audit.TransportStdio || string(list[0].Arguments) != `{"id":"bkp_1"}` || list[0].APIKeyName != "ci" {
		t.Fatalf("round trip lost fields: %+v", list[0])
	}

	// Invalid JSON arguments are stored as an empty object instead of failing.
	if err := s.AppendAudit(ctx, &audit.Entry{Tool: "x", Arguments: json.RawMessage(`{`), Result: audit.ResultError}, 0); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.ListAudit(ctx, 1); string(list[0].Arguments) != "{}" {
		t.Fatalf("invalid arguments stored as %s", list[0].Arguments)
	}
	if err := s.AppendAudit(ctx, nil, 0); err == nil {
		t.Fatal("a nil entry must be rejected")
	}
}

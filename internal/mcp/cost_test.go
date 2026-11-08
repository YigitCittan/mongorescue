package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/cost"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// costTargets lists fixed storage targets for the cost service.
type costTargets []*models.StorageTarget

func (c costTargets) List(context.Context) ([]*models.StorageTarget, error) { return c, nil }

// lazyCost forwards to a cost service that is built once the fixture's store exists.
type lazyCost struct{ svc **cost.Service }

func (l lazyCost) Report(ctx context.Context, days int) (*cost.Report, error) {
	return (*l.svc).Report(ctx, days)
}

func TestGetStorageUsageTool(t *testing.T) {
	// Without a cost service the tool says so.
	bare := newFixture(t, nil)
	if res := call(t, bare.session(t, principal(auth.ScopeRead)), ToolGetStorageUsage, nil); !res.IsError || !strings.Contains(resultText(res), "not available") {
		t.Fatalf("without the service: %v %s", res.IsError, resultText(res))
	}

	var svc *cost.Service
	f := newFixture(t, func(c *Config) { c.Cost = lazyCost{&svc} })
	svc = cost.New(f.store, costTargets{{ID: "stg_a", Name: "Main", Type: models.StorageS3, IsDefault: true,
		Pricing: &models.StoragePricing{Currency: "EUR", StoragePerGiBMonth: 2}}})
	if err := f.store.SaveBackupRecord(context.Background(), &models.BackupRecord{ID: "b1", JobID: testJobID, Database: "shop",
		Status: models.StatusCompleted, SizeBytes: models.BytesPerGiB, StorageTargetID: "stg_a", StorageKey: "shop/1"}); err != nil {
		t.Fatal(err)
	}
	read := f.session(t, principal(auth.ScopeRead))

	var rep cost.Report
	structured(t, ToolGetStorageUsage, call(t, read, ToolGetStorageUsage, map[string]any{"days": 7}), &rep)
	if rep.Days != 7 || len(rep.Targets) != 1 || rep.Targets[0].StoredBytes != models.BytesPerGiB ||
		rep.Targets[0].MonthlyCost == nil || *rep.Targets[0].MonthlyCost != 2 || rep.Basis == "" {
		t.Errorf("report = %+v", rep)
	}
	if res := call(t, read, ToolGetStorageUsage, map[string]any{"days": 401}); !res.IsError {
		t.Errorf("days=401 accepted: %s", resultText(res))
	}

	// A key limited to some connections never sees the totals of every connection.
	limited := principal(auth.ScopeRead)
	limited.Connections = auth.OnlyConnections(testConnID)
	if res := call(t, f.session(t, limited), ToolGetStorageUsage, nil); !res.IsError || !strings.Contains(resultText(res), "limited to some connections") {
		t.Errorf("limited key: %v %s; want a refusal", res.IsError, resultText(res))
	}
}

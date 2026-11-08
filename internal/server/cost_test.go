package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/cost"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// costTargets lists fixed storage targets for the cost service.
type costTargets []*models.StorageTarget

func (c costTargets) List(context.Context) ([]*models.StorageTarget, error) { return c, nil }

func TestStorageUsageEndpoint(t *testing.T) {
	base, _, _ := setupTestServer(t)
	st := storetest.New(t)
	svc := newTestAuth(t, st, "")
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour)
	for _, b := range []*models.BackupRecord{
		{ID: "b1", JobID: "job_a", Database: "shop", Status: models.StatusCompleted, StartedAt: at, SizeBytes: 2 * models.BytesPerGiB, StorageTargetID: "stg_a", StorageKey: "shop/1"},
		{ID: "b2", JobID: "job_a", Database: "shop", Status: models.StatusCompleted, StartedAt: at, SizeBytes: models.BytesPerGiB, StorageTargetID: "stg_a", StorageKey: "shop/2"},
	} {
		if err := st.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	targets := costTargets{{ID: "stg_a", Name: "Main", Type: models.StorageS3, IsDefault: true,
		Pricing: &models.StoragePricing{Currency: "USD", StoragePerGiBMonth: 0.5, EgressPerGiB: 1}}}
	costSvc := cost.New(st, targets)
	if _, err := costSvc.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := NewServer(bootConfig(), st, base.backupEngine, base.restoreEngine, base.storageDriver, base.scheduler, nil, nil,
		WithAuth(svc), withTestConnection(t, st, nil), WithSettings(newTestSettings(t, st, newTestConfig().Security)),
		WithMetricsHandler(ok), WithMCPHandler(ok), WithAudit(audit.NewService(st, nil)), withTestOIDC(t), withoutRateLimit(), WithCost(costSvc))
	h := srv.Handler()
	keys := map[auth.Scope]string{}
	for _, scope := range auth.Scopes() {
		_, plain, err := svc.CreateAPIKey(ctx, auth.SystemPrincipal(), string(scope)+" key", scope)
		if err != nil {
			t.Fatal(err)
		}
		keys[scope] = plain
	}

	if serve(h, "GET", "/api/v1/storage/usage", nil, nil).Code != http.StatusUnauthorized {
		t.Error("the report is served without credentials")
	}
	if got := requiredScope("GET /api/v1/storage/usage"); got != auth.ScopeRead {
		t.Fatalf("scope = %s; want read", got)
	}
	for _, scope := range auth.Scopes() {
		if rec := serve(h, "GET", "/api/v1/storage/usage", nil, map[string]string{"X-API-Key": keys[scope]}); rec.Code != http.StatusOK {
			t.Fatalf("%s: GET /api/v1/storage/usage = %d %s", scope, rec.Code, rec.Body.String())
		}
	}

	rec := serve(h, "GET", "/api/v1/storage/usage?days=7", nil, map[string]string{"X-API-Key": keys[auth.ScopeRead]})
	var rep cost.Report
	decodeData(t, rec, &rep)
	if rep.Days != 7 || len(rep.Targets) != 1 || rep.Basis == "" {
		t.Fatalf("report = %+v", rep)
	}
	u := rep.Targets[0]
	if u.StoredBytes != 3*models.BytesPerGiB || u.MonthlyCost == nil || *u.MonthlyCost != 1.5 || len(u.History) != 1 || len(rep.Jobs) != 1 || rep.Jobs[0].Bytes != u.StoredBytes {
		t.Errorf("target usage = %+v, jobs %+v", u, rep.Jobs)
	}

	for _, q := range []string{"days=0", "days=401", "days=-3", "days=abc"} {
		if rec = serve(h, "GET", "/api/v1/storage/usage?"+q, nil, map[string]string{"X-API-Key": keys[auth.ScopeRead]}); rec.Code != http.StatusBadRequest {
			t.Errorf("?%s = %d %s; want 400", q, rec.Code, rec.Body.String())
		}
	}
}

func TestStorageUsageIsUnavailableWithoutTheService(t *testing.T) {
	f := newScopeFixture(t)
	rec := serve(f.h, "GET", "/api/v1/storage/usage", nil, map[string]string{"X-API-Key": f.keys[auth.ScopeRead]})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("without the service = %d %s; want 503", rec.Code, rec.Body.String())
	}
}

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// toolNames lists the tools a session sees.
func toolNames(t *testing.T, cs *sdk.ClientSession) []string {
	t.Helper()
	var names []string
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func TestToolsHaveScopesAndAnnotations(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeAdmin))
	seen := 0
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		seen++
		scope, ok := ToolScopes[tool.Name]
		if !ok {
			t.Errorf("tool %s has no scope", tool.Name)
			continue
		}
		a := tool.Annotations
		if a == nil || a.Title == "" || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("tool %s: annotations %+v; want a title, destructiveHint=false and openWorldHint=false", tool.Name, a)
			continue
		}
		switch scope {
		case auth.ScopeRead:
			if !a.ReadOnlyHint || !a.IdempotentHint {
				t.Errorf("read tool %s must be readOnly and idempotent: %+v", tool.Name, a)
			}
		case auth.ScopeOperator:
			if a.ReadOnlyHint || a.IdempotentHint {
				t.Errorf("action tool %s must not claim readOnly or idempotent: %+v", tool.Name, a)
			}
		default:
			t.Errorf("tool %s needs %q; MCP tools need read or operator", tool.Name, scope)
		}
		if tool.OutputSchema == nil || tool.Description == "" {
			t.Errorf("tool %s lacks a description or an output schema", tool.Name)
		}
		// Portable parameters: single-type properties only (no ["null", ...] arrays).
		if raw, _ := json.Marshal(tool.InputSchema); strings.Contains(string(raw), `"type":[`) {
			t.Errorf("tool %s has a type array in its input schema: %s", tool.Name, raw)
		}
	}
	if seen != len(ToolScopes) {
		t.Fatalf("admin sees %d tools; ToolScopes has %d", seen, len(ToolScopes))
	}
	for name := range ToolScopes {
		for _, banned := range []string{"delete", "drop", "remove", "setting", "user", "key", "in_place"} {
			if strings.Contains(name, banned) {
				t.Errorf("tool %s must not exist: MCP never deletes or reconfigures", name)
			}
		}
	}
}

func TestToolListIsFilteredByScope(t *testing.T) {
	f := newFixture(t, nil)
	read := toolNames(t, f.session(t, principal(auth.ScopeRead)))
	op := toolNames(t, f.session(t, principal(auth.ScopeOperator)))
	if len(read) != 11 || slices.Contains(read, ToolStartBackup) || slices.Contains(read, ToolRestoreSafeClone) || slices.Contains(read, ToolRunJob) {
		t.Fatalf("read key sees %v; want only the 11 read tools", read)
	}
	if len(op) != len(ToolScopes) || !slices.Contains(op, ToolStartBackup) {
		t.Fatalf("operator key sees %v; want all %d tools", op, len(ToolScopes))
	}
}

func TestReadScopeCannotStartOperations(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeRead))
	for tool, args := range map[string]map[string]any{
		ToolStartBackup:      {"connection_id": testConnID, "database": "shop"},
		ToolRunJob:           {"job_id": testJobID},
		ToolRestoreSafeClone: {"backup_id": "bkp_x"},
	} {
		res := call(t, cs, tool, args)
		if !res.IsError || !strings.Contains(resultText(res), `"operator" scope`) {
			t.Fatalf("read key calling %s: %+v; want a forbidden tool error", tool, res)
		}
	}
	if list, _ := f.store.ListBackupRecords(context.Background(), ""); len(list) != 0 {
		t.Fatalf("a refused call must not start anything, found %d backups", len(list))
	}
	entries, _ := f.audit.List(context.Background(), 10)
	if len(entries) != 3 || entries[0].Result != audit.ResultDenied || entries[0].APIKeyID != "key_read" {
		t.Fatalf("denied calls must be audited: %+v", entries)
	}
	if f.observed[ToolStartBackup+"/"+audit.ResultDenied] != 1 {
		t.Fatalf("denied call not counted: %v", f.observed)
	}
}

func TestBackupAndSafeCloneRestoreFlow(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeOperator))

	var started backupStarted
	structured(t, ToolStartBackup, call(t, cs, ToolStartBackup, map[string]any{"connection_id": testConnID, "database": "shop"}), &started)
	if started.Backup == nil || started.Backup.Status != models.StatusInProgress || !strings.Contains(started.NextStep, "get_backup") {
		t.Fatalf("start_backup = %+v; want an in-progress record and a polling hint", started)
	}
	done := awaitBackup(t, cs, started.Backup.ID)
	if done.Status != models.StatusCompleted || done.SHA256 == "" {
		t.Fatalf("backup = %+v; want completed", done)
	}

	var rst restoreStarted
	structured(t, ToolRestoreSafeClone, call(t, cs, ToolRestoreSafeClone, map[string]any{"backup_id": done.ID, "verify": true}), &rst)
	if rst.Restore == nil || !strings.HasPrefix(rst.Restore.TargetDatabase, "shop_rescue_") {
		t.Fatalf("restore_to_safe_clone = %+v; want a shop_rescue_ clone", rst)
	}
	final := awaitRestore(t, cs, rst.Restore.ID)
	if final.Status != models.RestoreStatusCompleted || !final.Verified || !strings.HasPrefix(final.TargetDatabase, "shop_rescue_") {
		t.Fatalf("restore = %+v; want a completed, verified clone", final)
	}

	var job backupStarted
	structured(t, ToolRunJob, call(t, cs, ToolRunJob, map[string]any{"job_id": testJobID}), &job)
	if job.Backup == nil || job.Backup.JobID != testJobID {
		t.Fatalf("run_job = %+v", job)
	}
	if b := awaitBackup(t, cs, job.Backup.ID); b.Status != models.StatusCompleted {
		t.Fatalf("job backup = %+v", b)
	}

	entries, _ := f.audit.List(context.Background(), 100)
	for _, e := range entries {
		assertNoSecret(t, "audit entry", e)
		if e.APIKeyName != "operator key" || e.Transport != audit.TransportHTTP {
			t.Fatalf("audit entry lacks principal or transport: %+v", e)
		}
	}
	if f.observed[ToolStartBackup+"/"+audit.ResultOK] != 1 || f.observed[ToolRestoreSafeClone+"/"+audit.ResultOK] != 1 {
		t.Fatalf("calls not counted: %v", f.observed)
	}
}

func TestRestoreToolCannotRestoreInPlace(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeAdmin))
	var started backupStarted
	structured(t, ToolStartBackup, call(t, cs, ToolStartBackup, map[string]any{"connection_id": testConnID, "database": "shop"}), &started)
	done := awaitBackup(t, cs, started.Backup.ID)
	// In-place parameters of the REST API do not exist on the tool and are rejected.
	for _, extra := range []map[string]any{
		{"safe_clone": false, "confirm_in_place": true},
		{"target_database": "shop"},
		{"drop_target": true},
	} {
		args := map[string]any{"backup_id": done.ID}
		for k, v := range extra {
			args[k] = v
		}
		res := call(t, cs, ToolRestoreSafeClone, args)
		if !res.IsError {
			t.Fatalf("restore_to_safe_clone accepted %v", extra)
		}
	}
	if list, _ := f.store.ListRestoreRecords(context.Background()); len(list) != 0 {
		t.Fatalf("rejected restores must not run: %+v", list)
	}
}

func TestReadTools(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeOperator))
	var started backupStarted
	structured(t, ToolStartBackup, call(t, cs, ToolStartBackup, map[string]any{"connection_id": testConnID, "database": "shop"}), &started)
	b := awaitBackup(t, cs, started.Backup.ID)
	var rst restoreStarted
	structured(t, ToolRestoreSafeClone, call(t, cs, ToolRestoreSafeClone, map[string]any{"backup_id": b.ID}), &rst)
	awaitRestore(t, cs, rst.Restore.ID)

	read := f.session(t, principal(auth.ScopeRead))
	var conns connectionList
	res := call(t, read, ToolListConnections, nil)
	structured(t, ToolListConnections, res, &conns)
	assertNoSecret(t, "list_connections", res)
	if len(conns.Connections) != 1 || conns.Connections[0].Hosts != "db.internal:27017" || strings.Contains(resultText(res), "admin:") {
		t.Fatalf("list_connections = %+v", conns)
	}
	var dbs databaseList
	structured(t, ToolListDatabases, call(t, read, ToolListDatabases, map[string]any{"connection_id": testConnID}), &dbs)
	if len(dbs.Databases) != 1 || dbs.Databases[0].Name != "shop" {
		t.Fatalf("list_databases must hide system databases: %+v", dbs)
	}
	var cols collectionList
	structured(t, ToolListCollections, call(t, read, ToolListCollections, map[string]any{"connection_id": testConnID, "database": "shop"}), &cols)
	if len(cols.Collections) != 1 {
		t.Fatalf("list_collections = %+v", cols)
	}
	var jobs jobList
	structured(t, ToolListJobs, call(t, read, ToolListJobs, nil), &jobs)
	if jobs.Total != 1 || jobs.Jobs[0].ID != testJobID {
		t.Fatalf("list_jobs = %+v", jobs)
	}
	var job models.Job
	structured(t, ToolGetJob, call(t, read, ToolGetJob, map[string]any{"id": testJobID}), &job)
	if job.Name != "nightly shop" {
		t.Fatalf("get_job = %+v", job)
	}
	var backups backupList
	structured(t, ToolListBackups, call(t, read, ToolListBackups, map[string]any{"database": "shop", "status": "completed"}), &backups)
	if backups.Total != 1 || backups.Backups[0].ID != b.ID {
		t.Fatalf("list_backups = %+v", backups)
	}
	var restores restoreList
	structured(t, ToolListRestores, call(t, read, ToolListRestores, map[string]any{"limit": 5}), &restores)
	if restores.Total != 1 {
		t.Fatalf("list_restores = %+v", restores)
	}
	var targets targetList
	structured(t, ToolListStorageTargets, call(t, read, ToolListStorageTargets, nil), &targets)
	var status struct {
		Health    string `json:"health"`
		Version   string `json:"version"`
		JobStatus []struct {
			ID string `json:"id"`
		} `json:"job_status"`
	}
	res = call(t, read, ToolGetStatus, nil)
	structured(t, ToolGetStatus, res, &status)
	if status.Health != "healthy" || status.Version != "test" || len(status.JobStatus) != 1 {
		t.Fatalf("get_status = %+v", status)
	}
	if len(res.Content) != 2 || !strings.Contains(resultText(res), "MongoRescue test is healthy") {
		t.Fatalf("results carry a text summary and the JSON: %+v", res.Content)
	}

	// Missing records are tool errors, not protocol errors.
	for tool, args := range map[string]map[string]any{
		ToolGetBackup:  {"id": "bkp_missing"},
		ToolGetRestore: {"id": "rst_missing"},
		ToolGetJob:     {"id": "job_missing"},
	} {
		if res := call(t, read, tool, args); !res.IsError || !strings.Contains(resultText(res), "not found") {
			t.Fatalf("%s of a missing record = %+v", tool, res)
		}
	}
}

func TestPagination(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	for i := range 5 {
		if err := f.store.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bkp_" + string(rune('a'+i)), Database: "shop", Status: models.StatusCompleted}); err != nil {
			t.Fatal(err)
		}
	}
	cs := f.session(t, principal(auth.ScopeRead))
	var seen []string
	cursor := ""
	for range 5 {
		args := map[string]any{"limit": 2}
		if cursor != "" {
			args["cursor"] = cursor
		}
		var page backupList
		structured(t, ToolListBackups, call(t, cs, ToolListBackups, args), &page)
		for _, b := range page.Backups {
			seen = append(seen, b.ID)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paging returned %v; want all 5 backups once", seen)
	}
}

func TestInputValidation(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeOperator))
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{ToolStartBackup, map[string]any{"connection_id": testConnID}, "database"},
		{ToolStartBackup, map[string]any{"connection_id": "conn_missing", "database": "shop"}, "unknown connection_id"},
		{ToolStartBackup, map[string]any{"connection_id": testConnID, "database": "shop", "storage_target_id": "stg_x", "extra": 1}, ""},
		{ToolListBackups, map[string]any{"limit": 1000}, "limit"},
		{ToolListBackups, map[string]any{"cursor": "bogus"}, "cursor"},
		{ToolListBackups, map[string]any{"status": "exploded"}, "status"},
		{ToolGetBackup, map[string]any{"id": strings.Repeat("x", 300)}, "id"},
		{ToolRunJob, map[string]any{"job_id": "job_missing"}, "job not found"},
		{ToolRestoreSafeClone, map[string]any{"backup_id": "bkp_missing"}, "source backup not found"},
	} {
		res := call(t, cs, tc.tool, tc.args)
		if !res.IsError || (tc.want != "" && !strings.Contains(resultText(res), tc.want)) {
			t.Errorf("%s %v = %q; want a tool error mentioning %q", tc.tool, tc.args, resultText(res), tc.want)
		}
		assertNoSecret(t, tc.tool, res)
	}
}

func TestUnknownToolIsCountedAsUnknown(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeAdmin))
	if _, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "drop_database", Arguments: map[string]any{}}); err == nil {
		t.Fatal("an unknown tool must fail")
	}
	if f.observed[unknownTool+"/"+audit.ResultError] != 1 {
		t.Fatalf("unknown tool calls must share one metric label: %v", f.observed)
	}
}

func TestRateLimitPerAPIKey(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.RateLimit = RateLimit{PerMinute: 1, Burst: 2} })
	cs := f.session(t, principal(auth.ScopeRead))
	for range 2 {
		call(t, cs, ToolGetStatus, nil)
	}
	_, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolGetStatus, Arguments: map[string]any{}})
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) || wire.Code != CodeRateLimited || !strings.Contains(wire.Message, "retry in") {
		t.Fatalf("third call = %v; want a rate limit error", err)
	}
	// Another key has its own budget.
	other := f.session(t, principal(auth.ScopeOperator))
	call(t, other, ToolGetStatus, nil)
	entries, _ := f.audit.List(context.Background(), 10)
	if !slices.ContainsFunc(entries, func(e *audit.Entry) bool { return e.Result == audit.ResultRateLimited }) {
		t.Fatalf("rate limited calls must be audited: %+v", entries)
	}
}

// TestRefusedCallsAreRateLimitedAndCoalesced proves that calls refused for their
// scope count against the rate limit and cannot flush the audit log.
func TestRefusedCallsAreRateLimitedAndCoalesced(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.RateLimit = RateLimit{PerMinute: 1, Burst: 5} })
	cs := f.session(t, principal(auth.ScopeRead))
	args := map[string]any{"connection_id": testConnID, "database": "shop"}
	denied, limited := 0, 0
	for range 12 {
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolStartBackup, Arguments: args})
		var wire *jsonrpc.Error
		switch {
		case errors.As(err, &wire) && wire.Code == CodeRateLimited:
			limited++
		case err == nil && res.IsError:
			denied++
		default:
			t.Fatalf("call = %+v, %v", res, err)
		}
	}
	if denied != 5 || limited != 7 {
		t.Fatalf("denied=%d rate limited=%d; want 5/7 (the limit applies before the scope check)", denied, limited)
	}
	entries, _ := f.audit.List(context.Background(), 100)
	got := map[string]int{}
	for _, e := range entries {
		got[e.Result] += e.Count
	}
	if len(entries) != 2 || got[audit.ResultDenied] != 5 || got[audit.ResultRateLimited] != 7 {
		t.Fatalf("audit = %d entries %v; want one denied (count 5) and one rate_limited (count 7)", len(entries), got)
	}
}

func TestUnauthenticatedSessionIsRefused(t *testing.T) {
	f := newFixture(t, nil)
	clientT, serverT := sdk.NewInMemoryTransports()
	ss, err := f.srv.Connect(context.Background(), serverT)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	_, err = sdk.NewClient(&sdk.Implementation{Name: "anonymous", Version: "v0"}, nil).Connect(context.Background(), clientT, nil)
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) || wire.Code != CodeUnauthenticated {
		t.Fatalf("initialize without a principal = %v; want an unauthenticated error", err)
	}
}

func TestResources(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeOperator))
	var started backupStarted
	structured(t, ToolStartBackup, call(t, cs, ToolStartBackup, map[string]any{"connection_id": testConnID, "database": "shop"}), &started)
	b := awaitBackup(t, cs, started.Backup.ID)
	var rst restoreStarted
	structured(t, ToolRestoreSafeClone, call(t, cs, ToolRestoreSafeClone, map[string]any{"backup_id": b.ID}), &rst)
	awaitRestore(t, cs, rst.Restore.ID)

	var templates []string
	for tpl, err := range cs.ResourceTemplates(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		templates = append(templates, tpl.URITemplate)
	}
	slices.Sort(templates)
	if want := []string{BackupURITemplate, JobURITemplate, RestoreURITemplate}; !slices.Equal(templates, want) {
		t.Fatalf("templates = %v; want %v", templates, want)
	}

	for uri, want := range map[string]string{
		StatusURI:                                  `"health": "healthy"`,
		"mongorescue://backups/" + b.ID:            `"id": "` + b.ID + `"`,
		"mongorescue://jobs/" + testJobID:          `"id": "` + testJobID + `"`,
		"mongorescue://restores/" + rst.Restore.ID: `"backup_id": "` + b.ID + `"`,
	} {
		res, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: uri})
		if err != nil || len(res.Contents) != 1 || !strings.Contains(res.Contents[0].Text, want) || res.Contents[0].MIMEType != jsonMIME {
			t.Fatalf("read %s = %+v, %v", uri, res, err)
		}
		assertNoSecret(t, uri, res)
	}
	for _, uri := range []string{"mongorescue://backups/bkp_missing", "mongorescue://jobs/a/b"} {
		if _, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: uri}); err == nil {
			t.Fatalf("read %s must fail", uri)
		}
	}
}

// TestResourceReadsAndPromptsAreAudited proves resources/read and prompts/get leave
// audit entries like tool calls.
func TestResourceReadsAndPromptsAreAudited(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.RateLimit = RateLimit{PerMinute: 1, Burst: 3} })
	cs := f.session(t, principal(auth.ScopeRead))
	if _, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: StatusURI}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: "mongorescue://backups/bkp_missing"}); err == nil {
		t.Fatal("reading a missing backup must fail")
	}
	if _, err := cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: PromptDiagnoseFailedBackup, Arguments: map[string]string{"backup_id": "bkp_1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: PromptVerifyRecentBackups}); err == nil {
		t.Fatal("a fourth request must be rate limited")
	}
	entries, _ := f.audit.List(context.Background(), 10)
	var got []string
	for _, e := range entries {
		if e.APIKeyID != "key_read" || e.Transport != audit.TransportHTTP {
			t.Fatalf("entry lacks principal or transport: %+v", e)
		}
		got = append(got, e.Tool+" "+e.Result+" "+string(e.Arguments))
	}
	want := []string{
		`prompts/get rate_limited {"arguments":null,"prompt":"verify_recent_backups"}`,
		`prompts/get ok {"arguments":{"backup_id":"bkp_1"},"prompt":"diagnose_failed_backup"}`,
		`resources/read error {"resource":"mongorescue://backups/bkp_missing"}`,
		`resources/read ok {"resource":"mongorescue://status"}`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("audit = %q\nwant %q", got, want)
	}
}

func TestPrompts(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeRead))
	var names []string
	for p, err := range cs.Prompts(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, p.Name)
	}
	slices.Sort(names)
	if want := []string{PromptDiagnoseFailedBackup, PromptDisasterRecoveryPlan, PromptVerifyRecentBackups}; !slices.Equal(names, want) {
		t.Fatalf("prompts = %v", names)
	}
	for name, tc := range map[string]struct {
		args map[string]string
		want []string
	}{
		PromptDiagnoseFailedBackup: {map[string]string{"backup_id": "bkp_1"}, []string{"get_backup", `"bkp_1"`}},
		PromptDisasterRecoveryPlan: {map[string]string{"database": "shop"}, []string{"restore_to_safe_clone", "shop_rescue_"}},
		PromptVerifyRecentBackups:  {map[string]string{"hours": "48"}, []string{"last 48 hours", "get_status"}},
	} {
		res, err := cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: name, Arguments: tc.args})
		if err != nil || len(res.Messages) != 1 {
			t.Fatalf("%s = %+v, %v", name, res, err)
		}
		text := res.Messages[0].Content.(*sdk.TextContent).Text
		for _, w := range tc.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s lacks %q: %s", name, w, text)
			}
		}
	}
	if _, err := cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: PromptDiagnoseFailedBackup}); err == nil {
		t.Fatal("a missing required argument must fail")
	}
	if _, err := cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: PromptVerifyRecentBackups, Arguments: map[string]string{"hours": "-1"}}); err == nil {
		t.Fatal("an invalid hours argument must fail")
	}
}

func TestAuditArgumentsAreRedacted(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeOperator))
	// A model pasting a connection string into a filter must not leak it into the log.
	call(t, cs, ToolListBackups, map[string]any{"database": testConnURI})
	entries, _ := f.audit.List(context.Background(), 1)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	assertNoSecret(t, "audit arguments", entries[0])
	var args map[string]any
	if err := json.Unmarshal(entries[0].Arguments, &args); err != nil || args["database"] == nil {
		t.Fatalf("arguments = %s, %v", entries[0].Arguments, err)
	}
}

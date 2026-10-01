package operations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Bulk operation limits.
const (
	// MaxBulkItems is the most items one bulk operation may select, by IDs or by
	// filter.
	MaxBulkItems = 10000
	// BulkConfirmThreshold is the number of affected items above which a bulk
	// operation needs confirm_count (the actionable count of its dry run).
	BulkConfirmThreshold = 10
)

// Bulk sentinel errors.
var (
	// ErrBulkTooLarge is returned when a bulk operation selects more than
	// MaxBulkItems items.
	ErrBulkTooLarge = errors.New("operations: too many items for one bulk operation")
	// ErrBulkConfirm is returned when confirm_count is missing although more than
	// BulkConfirmThreshold items are affected, or differs from the number of items the
	// action applies to now (the selection changed since the dry run).
	ErrBulkConfirm = errors.New("operations: bulk confirmation does not match")
)

// BulkResource names the records a bulk operation acts on.
type BulkResource string

// Bulk resources.
const (
	// BulkBackups acts on backup records (POST /api/v1/backups/bulk).
	BulkBackups BulkResource = "backups"
	// BulkRestores acts on restore records (POST /api/v1/restores/bulk).
	BulkRestores BulkResource = "restores"
	// BulkJobs acts on scheduled jobs (POST /api/v1/jobs/bulk).
	BulkJobs BulkResource = "jobs"
)

// Bulk action names.
const (
	// BulkDelete deletes backups (with their archives), restore history records or jobs.
	BulkDelete = "delete"
	// BulkEnable schedules jobs.
	BulkEnable = "enable"
	// BulkDisable pauses jobs.
	BulkDisable = "disable"
	// BulkRunNow runs jobs now.
	BulkRunNow = "run_now"
)

// Skip reasons of BulkSkip.Reason.
const (
	// SkipNotFound marks an ID that names no (readable) record.
	SkipNotFound = "not_found"
	// SkipInProgress marks a backup or restore that is still pending or running.
	SkipInProgress = "in_progress"
	// SkipLastGoodBackup marks the newest completed backup of a job, which bulk
	// deletes never remove (Detail names the job).
	SkipLastGoodBackup = "last_good_backup"
	// SkipAlreadyEnabled marks a job that is already scheduled.
	SkipAlreadyEnabled = "already_enabled"
	// SkipAlreadyDisabled marks a job that is already paused.
	SkipAlreadyDisabled = "already_disabled"
)

// BulkFilter selects every record matching the list filters, with the names and
// formats of the list endpoints' query parameters. Fields that do not apply to the
// resource must stay empty (an ErrInvalid error otherwise), so a misplaced filter can
// never widen the selection silently. Paging and sorting do not apply.
type BulkFilter struct {
	// Status keeps records in this state (backups and restores).
	Status string `json:"status,omitempty"`
	// Database keeps backups of, restores into, or jobs of exactly this database.
	Database string `json:"database,omitempty"`
	// ConnectionID keeps backups or jobs of this connection.
	ConnectionID string `json:"connection_id,omitempty"`
	// JobID keeps backups of this job.
	JobID string `json:"job_id,omitempty"`
	// Trigger keeps backups started this way.
	Trigger string `json:"trigger,omitempty"`
	// RetryOf keeps the retries of this backup.
	RetryOf string `json:"retry_of,omitempty"`
	// BackupID keeps restores of this backup.
	BackupID string `json:"backup_id,omitempty"`
	// From keeps records started at or after this RFC 3339 time (backups and restores).
	From string `json:"from,omitempty"`
	// To keeps records started before this RFC 3339 time (backups and restores).
	To string `json:"to,omitempty"`
	// Q keeps records whose ID or database (jobs: also name) contains this text,
	// ignoring ASCII case.
	Q string `json:"q,omitempty"`
	// Enabled keeps scheduled (true) or paused (false) jobs.
	Enabled *bool `json:"enabled,omitempty"`
}

// BulkRequest is the body of POST /api/v1/{backups,restores,jobs}/bulk. Exactly one of
// IDs and Filter selects the items.
type BulkRequest struct {
	// Action is the bulk action (see BulkActions).
	Action string `json:"action"`
	// IDs selects these records (at most MaxBulkItems; duplicates count once).
	IDs []string `json:"ids,omitempty"`
	// Filter selects every record matching it (at most MaxBulkItems).
	Filter *BulkFilter `json:"filter,omitempty"`
	// DryRun only reports what the action would do.
	DryRun bool `json:"dry_run"`
	// ConfirmCount must equal the actionable count when more than
	// BulkConfirmThreshold items are affected, and must match whenever it is set.
	ConfirmCount *int `json:"confirm_count,omitempty"`
}

// BulkSkip is an item the action does not apply to.
type BulkSkip struct {
	// ID is the record.
	ID string `json:"id"`
	// Reason is one of the Skip constants.
	Reason string `json:"reason"`
	// Detail explains the reason, such as the protected backup's job.
	Detail string `json:"detail,omitempty"`
}

// BulkItemResult is the outcome of the action on one item.
type BulkItemResult struct {
	// ID is the record.
	ID string `json:"id"`
	// OK reports success.
	OK bool `json:"ok"`
	// Error is why the action failed on the item.
	Error string `json:"error,omitempty"`
	// Warning is a problem that did not stop the action (an archive left in storage).
	Warning string `json:"warning,omitempty"`
	// Detail adds information, such as the backup a run started or why an archive
	// was kept.
	Detail string `json:"detail,omitempty"`
}

// BulkResult is the response of a bulk operation.
type BulkResult struct {
	// Resource and Action echo the request.
	Resource BulkResource `json:"resource"`
	// Action is the bulk action.
	Action string `json:"action"`
	// DryRun reports that nothing was changed.
	DryRun bool `json:"dry_run"`
	// Matched counts the selected items (unique IDs, or filter matches).
	Matched int `json:"matched"`
	// Actionable counts the items the action applies to (Matched minus Skipped).
	Actionable int `json:"actionable"`
	// TotalSizeBytes sums the archive sizes of the actionable backups.
	TotalSizeBytes int64 `json:"total_size_bytes"`
	// Skipped lists the items the action does not apply to, with the reason.
	Skipped []BulkSkip `json:"skipped"`
	// ActionableIDs lists the actionable items (dry runs only), so a client can run
	// exactly what it showed, in batches.
	ActionableIDs []string `json:"actionable_ids,omitempty"`
	// Succeeded and Failed count the outcomes of a real run.
	Succeeded int `json:"succeeded"`
	// Failed counts the items the action failed on.
	Failed int `json:"failed"`
	// Results lists the outcome of every actionable item of a real run, in order.
	Results []BulkItemResult `json:"results,omitempty"`
}

// BulkItem is one selected record. Exactly one of Backup, Restore and Job is set
// (by resource), or none when the ID names no record.
type BulkItem struct {
	// ID is the selected ID.
	ID string
	// Backup is the backup record (BulkBackups).
	Backup *models.BackupRecord
	// Restore is the restore record (BulkRestores).
	Restore *models.RestoreRecord
	// Job is the job (BulkJobs).
	Job *models.Job
}

// found reports whether the item names a record.
func (it BulkItem) found() bool {
	return it.Backup != nil || it.Restore != nil || it.Job != nil
}

// BulkRun is the state one bulk operation shares between its items (such as the
// protected backups or the archive reference counts), filled by the actions.
type BulkRun struct {
	// lastGood maps job IDs to their newest completed backup.
	lastGood map[string]*models.BackupRecord
	// jobNames maps job IDs to their names.
	jobNames map[string]string
	// refs counts the records of every archive (loaded on the first delete).
	refs archiveRefs
}

// BulkAction is an action of a bulk endpoint. New actions are added to the registry
// in registerBulkActions (or with RegisterBulkAction); an action whose Available
// reports false is neither listed nor accepted.
type BulkAction struct {
	// Resource is the endpoint the action belongs to.
	Resource BulkResource
	// Name is the action's name in requests.
	Name string
	// Scope is what the caller needs, also for dry runs; it mirrors the scope of
	// the single-item route.
	Scope auth.Scope
	// Destructive marks actions that cannot be undone (the dashboard asks to type
	// the count).
	Destructive bool
	// Available reports whether the configured dependencies support the action;
	// nil means always.
	Available func(s *Service) bool
	// Prepare loads state shared by the items before Check; nil when not needed.
	Prepare func(ctx context.Context, s *Service, run *BulkRun) error
	// Check returns why the action does not apply to a found item, or nil; nil
	// means it applies to every found item.
	Check func(ctx context.Context, s *Service, run *BulkRun, it BulkItem) *BulkSkip
	// Apply runs the action on an actionable item.
	Apply func(ctx context.Context, s *Service, run *BulkRun, it BulkItem) BulkItemResult
}

// BulkActionInfo describes an available bulk action (GET /api/v1/bulk/actions).
type BulkActionInfo struct {
	// Resource is the endpoint.
	Resource BulkResource `json:"resource"`
	// Name is the action.
	Name string `json:"name"`
	// Scope is the scope it needs.
	Scope auth.Scope `json:"scope"`
	// Destructive marks actions that cannot be undone.
	Destructive bool `json:"destructive"`
	// Allowed reports whether the caller has the scope.
	Allowed bool `json:"allowed"`
}

// RegisterBulkAction adds a to the registry, replacing an action of the same resource
// and name. It is not safe to call concurrently with bulk operations; register
// actions while wiring the service.
func (s *Service) RegisterBulkAction(a BulkAction) {
	if s.bulk == nil {
		s.bulk = map[BulkResource]map[string]BulkAction{}
	}
	if s.bulk[a.Resource] == nil {
		s.bulk[a.Resource] = map[string]BulkAction{}
	}
	s.bulk[a.Resource][a.Name] = a
}

// available reports whether a is available with s's dependencies.
func (a BulkAction) available(s *Service) bool {
	return a.Apply != nil && (a.Available == nil || a.Available(s))
}

// BulkActions lists the available bulk actions, sorted by resource and name, and
// whether the caller in ctx may run each.
func (s *Service) BulkActions(ctx context.Context) []BulkActionInfo {
	p := auth.PrincipalFrom(ctx)
	out := make([]BulkActionInfo, 0)
	for _, actions := range s.bulk {
		for _, a := range actions {
			if a.available(s) {
				out = append(out, BulkActionInfo{Resource: a.Resource, Name: a.Name, Scope: a.Scope, Destructive: a.Destructive, Allowed: p.Allows(a.Scope)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Resource != out[j].Resource {
			return out[i].Resource < out[j].Resource
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Bulk runs req.Action on the records of resource that req selects. A dry run reports
// what would happen; a real run re-plans the selection, refuses (ErrBulkConfirm) when
// the plan does not match req.ConfirmCount, then applies the action to the
// actionable items one at a time, through the same use cases as the single-item
// routes, and reports every outcome. Items are never processed after ctx ends (they
// are reported as failed). One events.BulkCompleted event summarises a real run.
// Expected failures: ErrInvalid, ErrBulkTooLarge, ErrBulkConfirm and auth.ErrForbidden.
func (s *Service) Bulk(ctx context.Context, resource BulkResource, req BulkRequest) (*BulkResult, error) {
	action, ok := s.bulk[resource][req.Action]
	if !ok || !action.available(s) {
		return nil, public(fmt.Sprintf("unknown action %q for %s; available: %s", req.Action, resource, s.bulkActionNames(resource)), ErrInvalid)
	}
	if err := auth.RequireScope(ctx, action.Scope); err != nil {
		return nil, fmt.Errorf("bulk %s of %s: %w", action.Name, resource, err)
	}
	items, matched, err := s.bulkItems(ctx, resource, req)
	if err != nil {
		return nil, err
	}
	run := &BulkRun{}
	if action.Prepare != nil {
		if err = action.Prepare(ctx, s, run); err != nil {
			return nil, err
		}
	}

	res := &BulkResult{Resource: resource, Action: action.Name, DryRun: req.DryRun, Matched: matched, Skipped: []BulkSkip{}}
	actionable := make([]BulkItem, 0, len(items))
	for _, it := range items {
		switch {
		case !it.found():
			res.Skipped = append(res.Skipped, BulkSkip{ID: it.ID, Reason: SkipNotFound})
		case action.Check != nil:
			if skip := action.Check(ctx, s, run, it); skip != nil {
				skip.ID = it.ID
				res.Skipped = append(res.Skipped, *skip)
				continue
			}
			fallthrough
		default:
			actionable = append(actionable, it)
			if it.Backup != nil {
				res.TotalSizeBytes += it.Backup.SizeBytes
			}
		}
	}
	res.Actionable = len(actionable)
	if req.DryRun {
		res.ActionableIDs = make([]string, len(actionable))
		for i, it := range actionable {
			res.ActionableIDs[i] = it.ID
		}
		return res, nil
	}
	if err = checkConfirm(req.ConfirmCount, res.Actionable); err != nil {
		return nil, err
	}

	res.Results = make([]BulkItemResult, 0, len(actionable))
	for _, it := range actionable {
		var r BulkItemResult
		if ctxErr := ctx.Err(); ctxErr != nil {
			r = BulkItemResult{ID: it.ID, Error: "not processed: the request ended first"}
		} else {
			r = action.Apply(ctx, s, run, it)
			r.ID = it.ID
		}
		if r.OK {
			res.Succeeded++
		} else {
			res.Failed++
		}
		res.Results = append(res.Results, r)
	}
	s.finishBulk(ctx, res)
	return res, nil
}

// checkConfirm enforces BulkRequest.ConfirmCount against the actionable count.
func checkConfirm(confirm *int, actionable int) error {
	switch {
	case confirm != nil && *confirm != actionable:
		return public(fmt.Sprintf("confirm_count is %d but the action now applies to %d items; run a dry run again", *confirm, actionable), ErrBulkConfirm)
	case confirm == nil && actionable > BulkConfirmThreshold:
		return public(fmt.Sprintf("confirm_count is required when more than %d items are affected: send the actionable count (%d) of a dry run", BulkConfirmThreshold, actionable), ErrBulkConfirm)
	}
	return nil
}

// finishBulk logs and publishes the summary of a real run.
func (s *Service) finishBulk(ctx context.Context, res *BulkResult) {
	actor := actorName(ctx)
	s.logger.Info("bulk operation finished",
		slog.String("resource", string(res.Resource)),
		slog.String("action", res.Action),
		slog.Int("matched", res.Matched),
		slog.Int("succeeded", res.Succeeded),
		slog.Int("skipped", len(res.Skipped)),
		slog.Int("failed", res.Failed),
		slog.String("actor", actor),
	)
	s.publish(context.WithoutCancel(ctx), events.Event{
		Type: events.BulkCompleted,
		Time: s.now().UTC(),
		Bulk: &events.BulkSummary{
			Resource: string(res.Resource), Action: res.Action, Matched: res.Matched,
			Succeeded: res.Succeeded, Skipped: len(res.Skipped), Failed: res.Failed, Actor: actor,
		},
	})
}

// bulkActionNames lists the available actions of resource for error messages.
func (s *Service) bulkActionNames(resource BulkResource) string {
	var names []string
	for name, a := range s.bulk[resource] {
		if a.available(s) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// bulkItems loads the items req selects and the number of selected items.
func (s *Service) bulkItems(ctx context.Context, resource BulkResource, req BulkRequest) ([]BulkItem, int, error) {
	if (len(req.IDs) > 0) == (req.Filter != nil) {
		return nil, 0, public("select the items with either ids or filter", ErrInvalid)
	}
	if req.Filter != nil {
		if err := checkBulkFilter(resource, req.Filter); err != nil {
			return nil, 0, err
		}
		var items []BulkItem
		var err error
		switch resource {
		case BulkBackups:
			items, err = s.filterBackups(ctx, req.Filter)
		case BulkRestores:
			items, err = s.filterRestores(ctx, req.Filter)
		default:
			items, err = s.filterJobs(ctx, req.Filter)
		}
		return items, len(items), err
	}

	ids, err := uniqueIDs(req.IDs)
	if err != nil {
		return nil, 0, err
	}
	var items []BulkItem
	switch resource {
	case BulkBackups:
		items, err = s.backupsByID(ctx, ids)
	case BulkRestores:
		items, err = s.restoresByID(ctx, ids)
	default:
		items, err = s.jobsByID(ctx, ids)
	}
	return items, len(ids), err
}

// uniqueIDs checks the IDs of a bulk request and drops duplicates, keeping the order.
func uniqueIDs(in []string) ([]string, error) {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, id := range in {
		if id == "" || len(id) > maxFilterText {
			return nil, public(fmt.Sprintf("ids must be non-empty and at most %d characters", maxFilterText), ErrInvalid)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) > MaxBulkItems {
		return nil, tooLarge(len(out))
	}
	return out, nil
}

// tooLarge is the ErrBulkTooLarge error for n selected items.
func tooLarge(n int) error {
	return public(fmt.Sprintf("the selection has %d items; one bulk operation takes at most %d, narrow the filter", n, MaxBulkItems), ErrBulkTooLarge)
}

// bulkFilterFields lists, per resource, the BulkFilter fields that apply to it.
var bulkFilterFields = map[BulkResource][]string{
	BulkBackups:  {"status", "database", "connection_id", "job_id", "trigger", "retry_of", "from", "to", "q"},
	BulkRestores: {"status", "database", "backup_id", "from", "to", "q"},
	BulkJobs:     {"database", "connection_id", "q", "enabled"},
}

// checkBulkFilter refuses filter fields that do not apply to resource.
func checkBulkFilter(resource BulkResource, f *BulkFilter) error {
	set := map[string]bool{
		"status": f.Status != "", "database": f.Database != "", "connection_id": f.ConnectionID != "",
		"job_id": f.JobID != "", "trigger": f.Trigger != "", "retry_of": f.RetryOf != "", "backup_id": f.BackupID != "",
		"from": f.From != "", "to": f.To != "", "q": f.Q != "", "enabled": f.Enabled != nil,
	}
	allowed := bulkFilterFields[resource]
	var bad []string
	for name, isSet := range set {
		if isSet && !slices.Contains(allowed, name) {
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		slices.Sort(bad)
		return public(fmt.Sprintf("filter fields %s do not apply to %s (accepted: %s)", strings.Join(bad, ", "), resource, strings.Join(allowed, ", ")), ErrInvalid)
	}
	return nil
}

// filterTime parses an RFC 3339 filter time; "" is the zero time.
func filterTime(name, v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, public(fmt.Sprintf("filter.%s must be an RFC 3339 time such as 2026-10-01T00:00:00Z", name), ErrInvalid)
	}
	return t, nil
}

// filterRange parses the from and to fields of f.
func filterRange(f *BulkFilter) (from, to time.Time, err error) {
	if from, err = filterTime("from", f.From); err != nil {
		return from, to, err
	}
	to, err = filterTime("to", f.To)
	return from, to, err
}

// filterBackups loads every backup matching f, validated like QueryBackups.
func (s *Service) filterBackups(ctx context.Context, f *BulkFilter) ([]BulkItem, error) {
	from, to, err := filterRange(f)
	if err != nil {
		return nil, err
	}
	q := BackupFilter{
		Status: models.BackupStatus(f.Status), Database: f.Database, ConnectionID: f.ConnectionID, JobID: f.JobID,
		Trigger: models.BackupTrigger(f.Trigger), RetryOf: f.RetryOf, From: from, To: to, Search: f.Q, Limit: 1,
	}
	// Count first, so an over-broad filter is refused without loading every record.
	count, err := s.QueryBackups(ctx, q)
	if err != nil {
		return nil, err
	}
	if count.Total > MaxBulkItems {
		return nil, tooLarge(count.Total)
	}
	q.Limit = 0
	page, err := s.QueryBackups(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(page.Items) > MaxBulkItems {
		return nil, tooLarge(len(page.Items))
	}
	items := make([]BulkItem, len(page.Items))
	for i, it := range page.Items {
		items[i] = BulkItem{ID: it.ID, Backup: it.BackupRecord}
	}
	return items, nil
}

// filterRestores loads every restore matching f, validated like QueryRestores.
func (s *Service) filterRestores(ctx context.Context, f *BulkFilter) ([]BulkItem, error) {
	from, to, err := filterRange(f)
	if err != nil {
		return nil, err
	}
	q := RestoreFilter{
		Status: models.RestoreStatus(f.Status), BackupID: f.BackupID, TargetDatabase: f.Database,
		From: from, To: to, Search: f.Q, Limit: 1,
	}
	count, err := s.QueryRestores(ctx, q)
	if err != nil {
		return nil, err
	}
	if count.Total > MaxBulkItems {
		return nil, tooLarge(count.Total)
	}
	q.Limit = 0
	page, err := s.QueryRestores(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(page.Items) > MaxBulkItems {
		return nil, tooLarge(len(page.Items))
	}
	items := make([]BulkItem, len(page.Items))
	for i, r := range page.Items {
		items[i] = BulkItem{ID: r.ID, Restore: r}
	}
	return items, nil
}

// filterJobs returns every job matching f, sorted by name.
func (s *Service) filterJobs(ctx context.Context, f *BulkFilter) ([]BulkItem, error) {
	if err := checkText(map[string]string{"database": f.Database, "connection_id": f.ConnectionID, "q": f.Q}); err != nil {
		return nil, err
	}
	jobs, err := s.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(f.Q)
	var items []BulkItem
	for _, j := range jobs {
		switch {
		case f.Database != "" && j.Database != f.Database,
			f.ConnectionID != "" && j.ConnectionID != f.ConnectionID,
			f.Enabled != nil && j.Enabled != *f.Enabled,
			q != "" && !strings.Contains(strings.ToLower(j.ID), q) && !strings.Contains(strings.ToLower(j.Name), q) &&
				!strings.Contains(strings.ToLower(j.Database), q):
			continue
		}
		items = append(items, BulkItem{ID: j.ID, Job: j})
	}
	if len(items) > MaxBulkItems {
		return nil, tooLarge(len(items))
	}
	return items, nil
}

// backupsByID loads the backups ids name, in order; missing ones have no record.
func (s *Service) backupsByID(ctx context.Context, ids []string) ([]BulkItem, error) {
	found := make(map[string]*models.BackupRecord, len(ids))
	for start := 0; start < len(ids); start += store.MaxFilterIDs {
		chunk := ids[start:min(start+store.MaxFilterIDs, len(ids))]
		page, err := s.cfg.Store.QueryBackupRecords(ctx, store.BackupFilter{IDs: chunk})
		if err != nil {
			return nil, fmt.Errorf("load backups: %w", err)
		}
		for _, row := range page.Rows {
			found[row.Record.ID] = row.Record
		}
	}
	items := make([]BulkItem, len(ids))
	for i, id := range ids {
		items[i] = BulkItem{ID: id, Backup: found[id]}
	}
	return items, nil
}

// restoresByID loads the restores ids name, in order; missing ones have no record.
func (s *Service) restoresByID(ctx context.Context, ids []string) ([]BulkItem, error) {
	items := make([]BulkItem, len(ids))
	for i, id := range ids {
		items[i] = BulkItem{ID: id}
		rec, err := s.cfg.Store.GetRestoreRecord(ctx, id)
		switch {
		case err == nil:
			items[i].Restore = rec
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrCorruptRecord):
		default:
			return nil, fmt.Errorf("load restore: %w", err)
		}
	}
	return items, nil
}

// jobsByID loads the jobs ids name, in order; missing ones have no job.
func (s *Service) jobsByID(ctx context.Context, ids []string) ([]BulkItem, error) {
	jobs, err := s.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*models.Job, len(jobs))
	for _, j := range jobs {
		byID[j.ID] = j
	}
	items := make([]BulkItem, len(ids))
	for i, id := range ids {
		items[i] = BulkItem{ID: id, Job: byID[id]}
	}
	return items, nil
}

// failed is the BulkItemResult of err; messages of expected failures are shown, any
// other error is logged and reported generically.
func (s *Service) failed(err error) BulkItemResult {
	var pe *publicError
	if errors.As(err, &pe) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrBusy) ||
		errors.Is(err, ErrShuttingDown) || errors.Is(err, ErrConnectionRequired) || errors.Is(err, ErrUnknownConnection) ||
		errors.Is(err, ErrUnknownStorageTarget) || errors.Is(err, auth.ErrForbidden) {
		return BulkItemResult{Error: err.Error()}
	}
	s.logger.Error("bulk item failed", slog.Any("error", err))
	return BulkItemResult{Error: "internal error (see the server log)"}
}

// registerBulkActions registers the built-in bulk actions. Scopes mirror the
// single-item routes (see internal/server/scopes.go).
func (s *Service) registerBulkActions() {
	// Backups: delete, like DELETE /api/v1/backups/{id} (admin). Running backups and
	// the newest completed backup of every job are never deleted in bulk.
	s.RegisterBulkAction(BulkAction{
		Resource: BulkBackups, Name: BulkDelete, Scope: auth.ScopeAdmin, Destructive: true,
		Prepare: func(ctx context.Context, s *Service, run *BulkRun) error {
			latest, err := s.cfg.Store.LatestJobBackups(ctx, models.StatusCompleted)
			if err != nil {
				return fmt.Errorf("load the last good backups: %w", err)
			}
			run.lastGood = latest
			run.jobNames = map[string]string{}
			jobs, err := s.ListJobs(ctx)
			if err != nil {
				return err
			}
			for _, j := range jobs {
				run.jobNames[j.ID] = j.Name
			}
			return nil
		},
		Check: func(_ context.Context, _ *Service, run *BulkRun, it BulkItem) *BulkSkip {
			b := it.Backup
			if b.Status == models.StatusInProgress || b.Status == models.StatusPending {
				return &BulkSkip{Reason: SkipInProgress, Detail: "the backup is still " + string(b.Status)}
			}
			if last := run.lastGood[b.JobID]; b.JobID != "" && last != nil && last.ID == b.ID {
				name := run.jobNames[b.JobID]
				if name == "" {
					name = b.JobID
				}
				return &BulkSkip{Reason: SkipLastGoodBackup, Detail: "last successful backup of job " + name}
			}
			return nil
		},
		Apply: func(ctx context.Context, s *Service, run *BulkRun, it BulkItem) BulkItemResult {
			if run.refs == nil {
				refs, err := s.allArchiveRefs(ctx)
				if err != nil {
					return s.failed(err)
				}
				run.refs = refs
			}
			res, err := s.deleteBackup(ctx, it.Backup, run.refs)
			if err != nil {
				return s.failed(err)
			}
			return BulkItemResult{OK: true, Warning: res.ArchiveError, Detail: res.ArchiveKept}
		},
	})

	// Restores: delete history records (admin, like deleting a backup). Running
	// restores are skipped; restored databases are never touched.
	s.RegisterBulkAction(BulkAction{
		Resource: BulkRestores, Name: BulkDelete, Scope: auth.ScopeAdmin, Destructive: true,
		Check: func(_ context.Context, _ *Service, _ *BulkRun, it BulkItem) *BulkSkip {
			if st := it.Restore.Status; st == models.RestoreStatusInProgress || st == models.RestoreStatusPending {
				return &BulkSkip{Reason: SkipInProgress, Detail: "the restore is still " + string(st)}
			}
			return nil
		},
		Apply: func(ctx context.Context, s *Service, _ *BulkRun, it BulkItem) BulkItemResult {
			if err := s.deleteRestore(ctx, it.Restore); err != nil {
				return s.failed(err)
			}
			return BulkItemResult{OK: true}
		},
	})

	// Jobs: enable and disable like PUT /api/v1/jobs/{id} (admin), run_now like
	// POST /api/v1/jobs/{id}/run (operator), delete like DELETE /api/v1/jobs/{id} (admin).
	for _, enable := range []bool{true, false} {
		name, reason := BulkDisable, SkipAlreadyDisabled
		if enable {
			name, reason = BulkEnable, SkipAlreadyEnabled
		}
		s.RegisterBulkAction(BulkAction{
			Resource: BulkJobs, Name: name, Scope: auth.ScopeAdmin,
			Check: func(_ context.Context, _ *Service, _ *BulkRun, it BulkItem) *BulkSkip {
				if it.Job.Enabled == enable {
					return &BulkSkip{Reason: reason}
				}
				return nil
			},
			Apply: func(ctx context.Context, s *Service, _ *BulkRun, it BulkItem) BulkItemResult {
				if _, err := s.SetJobEnabled(ctx, it.ID, enable); err != nil {
					return s.failed(err)
				}
				return BulkItemResult{OK: true}
			},
		})
	}
	s.RegisterBulkAction(BulkAction{
		Resource: BulkJobs, Name: BulkRunNow, Scope: auth.ScopeOperator,
		Available: func(s *Service) bool { return s.cfg.Jobs != nil },
		Apply: func(ctx context.Context, s *Service, _ *BulkRun, it BulkItem) BulkItemResult {
			rec, err := s.RunJob(ctx, it.ID, models.TriggerOnDemand)
			if err != nil {
				return s.failed(err)
			}
			return BulkItemResult{OK: true, Detail: rec.ID}
		},
	})
	s.RegisterBulkAction(BulkAction{
		Resource: BulkJobs, Name: BulkDelete, Scope: auth.ScopeAdmin, Destructive: true,
		Apply: func(ctx context.Context, s *Service, _ *BulkRun, it BulkItem) BulkItemResult {
			if err := s.DeleteJob(ctx, it.ID); err != nil {
				return s.failed(err)
			}
			return BulkItemResult{OK: true}
		},
	})
}

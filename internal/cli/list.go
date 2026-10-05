package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/apiclient"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

const listUsage = `Usage: mongorescue list backups|restores|jobs|connections|targets [flags]

Lists records of the server, newest first for backups and restores (50 by default:
use --limit and --offset, --limit 0 for all). --json prints the server's array as is.
Filters (each applies to the resources in brackets):
  --id ID,...          [backups, restores] exactly these IDs
  --status S           [backups, restores] pending, in_progress, completed, failed, cancelled (backups: pruned, missing)
  --database NAME      [backups, restores, jobs] the backed-up (restores: target) database
  --connection ID      [backups, jobs] the connection
  --job ID             [backups] the job
  --run ID             [backups] the backups of one run (a multi-database job run or backup)
  --trigger T          [backups] scheduled, on_demand, manual or mcp
  --backup ID          [restores] restores of this backup
  --from, --to TIME    [backups, restores] started_at range (RFC 3339)
  --search TEXT        [backups, restores, jobs] ID or name contains TEXT
  --sort COLUMN        [backups, restores] e.g. -size, started_at (see docs/api.md)
  --enabled BOOL       [jobs] scheduled (true) or paused (false) jobs
  --schedule S         [jobs] hourly, daily, weekly, monthly or other
  --last-status S      [jobs] completed, failed, partial, in_progress, cancelled or never
`

// listResources maps each resource to the filter flags it takes.
var listResources = map[string][]string{
	"backups":     {"id", "status", "database", "connection", "job", "run", "trigger", "from", "to", "search", "sort", "limit", "offset"},
	"restores":    {"id", "status", "database", "backup", "from", "to", "search", "sort", "limit", "offset"},
	"jobs":        {"search", "enabled", "connection", "database", "schedule", "last-status"},
	"connections": {},
	"targets":     {},
}

// listQueryParam maps filter flags to the query parameters of the API.
var listQueryParam = map[string]string{
	"id": "id", "status": "status", "database": "database", "connection": "connection_id", "job": "job_id", "run": "run_id",
	"trigger": "trigger", "backup": "backup_id", "from": "from", "to": "to", "search": "q", "sort": "sort",
	"limit": "limit", "offset": "offset", "enabled": "enabled", "schedule": "schedule", "last-status": "last_status",
}

// defaultListLimit is the page size of list backups and list restores.
const defaultListLimit = 50

// runList implements "mongorescue list".
func runList(ctx context.Context, s *session, args []string) error {
	fs := s.newFlagSet("list", listUsage, false)
	values := map[string]*string{}
	for _, name := range []string{"id", "status", "database", "connection", "job", "run", "trigger", "backup", "from", "to", "search", "sort", "enabled", "schedule", "last-status"} {
		values[name] = fs.String(name, "", "Filter (see above)")
	}
	limit := fs.Int("limit", defaultListLimit, "[backups, restores] page size, 1 to 200; 0 lists every match")
	offset := fs.Int("offset", 0, "[backups, restores] matches to skip")
	pos, err := s.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageErrorf("missing what to list: backups, restores, jobs, connections or targets")
	}
	if len(pos) > 1 {
		return usageErrorf("unexpected arguments: %s", strings.Join(pos[1:], " "))
	}
	resource := pos[0]
	if resource == "storage-targets" {
		resource = "targets"
	}
	allowed, ok := listResources[resource]
	if !ok {
		return usageErrorf("cannot list %q: want backups, restores, jobs, connections or targets", pos[0])
	}
	for name := range s.set {
		if _, isFilter := listQueryParam[name]; isFilter && !slices.Contains(allowed, name) {
			return usageErrorf("--%s does not apply to list %s", name, resource)
		}
	}
	if *limit < 0 || *offset < 0 {
		return usageErrorf("--limit and --offset must not be negative")
	}
	q := url.Values{}
	for name, v := range values {
		if s.set[name] && strings.TrimSpace(*v) != "" {
			q.Set(listQueryParam[name], strings.TrimSpace(*v))
		}
	}
	if slices.Contains(allowed, "limit") {
		if *limit > 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		if *offset > 0 {
			if *limit == 0 {
				return usageErrorf("--offset needs a --limit")
			}
			q.Set("offset", strconv.Itoa(*offset))
		}
	}
	client, err := s.connect()
	if err != nil {
		return err
	}
	switch resource {
	case "backups":
		res, err := client.ListBackups(ctx, q)
		if err != nil {
			return err
		}
		return printList(s, res, "backups", backupsTable)
	case "restores":
		res, err := client.ListRestores(ctx, q)
		if err != nil {
			return err
		}
		return printList(s, res, "restores", restoresTable)
	case "jobs":
		res, err := client.ListJobs(ctx, q)
		if err != nil {
			return err
		}
		return printList(s, res, "jobs", jobsTable)
	case "connections":
		res, err := client.ListConnections(ctx)
		if err != nil {
			return err
		}
		return printList(s, res, "connections", connectionsTable)
	default:
		res, err := client.ListStorageTargets(ctx)
		if err != nil {
			return err
		}
		return printList(s, res, "storage targets", targetsTable)
	}
}

// printList prints a list: the server's JSON, the IDs, or a table and the page.
func printList[T any](s *session, res *apiclient.Result[[]T], what string, render func(*table, T)) error {
	if s.opts.json {
		raw := res.Raw
		if len(raw) == 0 || string(raw) == "null" {
			raw = json.RawMessage("[]")
		}
		return writeJSON(s.stdout, raw)
	}
	if s.opts.quiet {
		var ids []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(res.Raw, &ids); err != nil && len(res.Raw) > 0 && string(res.Raw) != "null" {
			return fmt.Errorf("%w: decode the IDs: %w", apiclient.ErrUnexpectedResponse, err)
		}
		for _, it := range ids {
			fmt.Fprintln(s.stdout, it.ID)
		}
		return nil
	}
	if len(res.Value) == 0 {
		if m := res.Meta; m != nil && m.Total > 0 {
			s.infof("No %s on this page (%d match; --offset %d is past the end).", what, m.Total, m.Offset)
			return nil
		}
		s.infof("No %s.", what)
		return nil
	}
	var t *table
	for i, it := range res.Value {
		if i == 0 {
			t = newTableFor(s, what)
		}
		render(t, it)
	}
	if err := t.flush(); err != nil {
		return err
	}
	if m := res.Meta; m != nil && m.Total > len(res.Value) {
		s.infof("Showing %d-%d of %d; use --limit and --offset for more.", m.Offset+1, m.Offset+len(res.Value), m.Total)
	}
	return nil
}

// newTableFor starts the table of a list.
func newTableFor(s *session, what string) *table {
	switch what {
	case "backups":
		return newTable(s.stdout, "ID", "DATABASE", "STATUS", "TRIGGER", "STARTED", "DURATION", "SIZE")
	case "restores":
		return newTable(s.stdout, "ID", "BACKUP", "SOURCE", "TARGET", "STATUS", "STARTED", "DURATION")
	case "jobs":
		return newTable(s.stdout, "ID", "NAME", "DATABASE", "SCHEDULE", "ENABLED", "NEXT RUN")
	case "connections":
		return newTable(s.stdout, "ID", "NAME", "URI", "LAST TEST")
	}
	return newTable(s.stdout, "ID", "NAME", "TYPE", "DEFAULT", "LOCATION")
}

// backupsTable renders one backup.
func backupsTable(t *table, b models.BackupRecord) {
	t.row(b.ID, b.Database, string(b.Status), string(b.EffectiveTrigger()), fmtTime(b.StartedAt), fmtSeconds(b.DurationSeconds), fmtBytes(b.SizeBytes))
}

// restoresTable renders one restore.
func restoresTable(t *table, r models.RestoreRecord) {
	t.row(r.ID, r.BackupID, r.SourceDatabase, r.TargetDatabase, string(r.Status), fmtTime(r.StartedAt), fmtSeconds(r.DurationSeconds))
}

// jobsTable renders one job.
func jobsTable(t *table, j models.Job) {
	db := j.Database
	switch sel := j.Selection(); sel.Mode {
	case models.SelectionList:
		db = strings.Join(sel.Databases, ",")
	case models.SelectionAll:
		db = "(all)"
	case models.SelectionPattern:
		db = strings.Join(sel.Include, ",")
	}
	t.row(j.ID, j.Name, db, j.CronExpression, strconv.FormatBool(j.Enabled), fmtTimePtr(j.NextRun))
}

// connectionsTable renders one connection; the URI is always redacted.
func connectionsTable(t *table, c models.Connection) {
	last := "never"
	if c.LastTestAt != nil {
		last = "ok"
		if !c.LastTestOK {
			last = "failed"
		}
		last += " " + fmtTime(*c.LastTestAt)
	}
	t.row(c.ID, c.Name, redact.URI(c.URI), last)
}

// targetsTable renders one storage target.
func targetsTable(t *table, st models.StorageTarget) {
	loc := ""
	switch {
	case st.Local != nil:
		loc = st.Local.Path
	case st.S3 != nil:
		loc = "s3://" + st.S3.Bucket
		if st.S3.Prefix != "" {
			loc += "/" + strings.TrimPrefix(st.S3.Prefix, "/")
		}
		if st.S3.Endpoint != "" {
			loc += " (" + redact.URI(st.S3.Endpoint) + ")"
		}
	}
	t.row(st.ID, st.Name, string(st.Type), strconv.FormatBool(st.IsDefault), loc)
}

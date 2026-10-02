package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/apiclient"
	"github.com/yigitcittan/mongorescue/internal/models"
)

const statusUsage = `Usage: mongorescue status [flags]

Shows the server's health and version, who the API key belongs to and its
effective scope, backup and restore counts and the runs in progress. --readiness
adds the recovery readiness of every database a job backs up and exits 1 when a
database fails it. Any API key scope may run it.
`

// statusJSON is the --json output of status: the server's answers as they are.
type statusJSON struct {
	URL        string          `json:"url"`
	Health     json.RawMessage `json:"health"`
	Me         json.RawMessage `json:"me"`
	Stats      json.RawMessage `json:"stats"`
	ActiveRuns json.RawMessage `json:"active_runs"`
	Readiness  json.RawMessage `json:"readiness,omitempty"`
}

// runStatus implements "mongorescue status".
func runStatus(ctx context.Context, s *session, args []string) error {
	fs := s.newFlagSet("status", statusUsage, false)
	readiness := fs.Bool("readiness", false, "Add the recovery readiness report; exit 1 when a database fails it")
	pos, err := s.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErrorf("status takes no arguments, got %s", strings.Join(pos, " "))
	}
	client, err := s.connect()
	if err != nil {
		return err
	}
	health, err := client.Health(ctx)
	if err != nil {
		return err
	}
	me, err := client.Me(ctx)
	if err != nil {
		return err
	}
	if me.Value.Auth == "" {
		return fmt.Errorf("%w: the server did not accept the API key", apiclient.ErrUnauthorized)
	}
	stats, err := client.Stats(ctx)
	if err != nil {
		return err
	}
	runs, err := client.ActiveRuns(ctx)
	if err != nil {
		return err
	}
	var ready *apiclient.Result[apiclient.Readiness]
	if *readiness {
		if ready, err = client.Readiness(ctx); err != nil {
			return err
		}
	}
	failing := ready != nil && ready.Value.Summary.Fail > 0
	switch {
	case s.opts.json:
		out := statusJSON{URL: s.baseURL, Health: health.Raw, Me: me.Raw, Stats: stats.Raw, ActiveRuns: runs.Raw}
		if ready != nil {
			out.Readiness = ready.Raw
		}
		b, err := json.Marshal(out)
		if err != nil {
			return fmt.Errorf("encode status: %w", err)
		}
		if err := writeJSON(s.stdout, b); err != nil {
			return err
		}
	case s.opts.quiet:
	default:
		if err := writeStatus(s.stdout, s.baseURL, health.Value, me.Value, stats.Value, runs.Value, ready); err != nil {
			return err
		}
	}
	if failing {
		return s.reportedInText(failedErrorf("recovery readiness: %d of %d databases fail", ready.Value.Summary.Fail, len(ready.Value.Rows)))
	}
	return nil
}

// writeStatus writes the text output of status.
func writeStatus(w io.Writer, url string, h apiclient.Health, me apiclient.Me, st apiclient.Stats, runs []models.RunProgress, ready *apiclient.Result[apiclient.Readiness]) error {
	who := "an API key"
	if me.User != nil && me.User.Username != "" {
		who = me.User.Username
	}
	scope := me.Scope
	if scope == "" {
		scope = "not reported by this server"
	}
	version := h.Version
	if version == "" {
		version = "unknown"
	}
	t := newTable(w, "Server:", url)
	t.row("Version:", version+" ("+h.Status+")")
	t.row("Signed in as:", who+" ("+me.Auth+")")
	t.row("Scope:", scope)
	t.row("Backups:", fmt.Sprintf("%d (%d completed, %d failed, %d failed in the last 24 h, %d running), %s stored",
		st.TotalBackups, st.CompletedBackups, st.FailedBackups, st.FailedBackups24h, st.ActiveBackups, fmtBytes(st.TotalBytes)))
	t.row("Restores:", fmt.Sprintf("%d (%d running)", st.TotalRestores, st.ActiveRestores))
	t.row("Jobs:", fmt.Sprintf("%d enabled", st.ActiveJobs))
	if b := st.LastBackup; b != nil {
		t.row("Last backup:", fmt.Sprintf("%s of %s %s at %s", b.ID, b.Database, b.Status, fmtTime(b.StartedAt)))
	}
	if st.Degraded {
		t.row("Degraded:", clean(st.DegradedReason))
	}
	t.row("Active runs:", fmt.Sprint(len(runs)))
	if ready != nil {
		sum := ready.Value.Summary
		t.row("Readiness:", fmt.Sprintf("%d ok, %d warn, %d fail", sum.OK, sum.Warn, sum.Fail))
	}
	if err := t.flush(); err != nil {
		return err
	}
	if len(runs) > 0 {
		fmt.Fprintln(w)
		rt := newTable(w, "KIND", "ID", "DATABASE", "PHASE", "PROGRESS", "STARTED")
		for _, r := range runs {
			pct := ""
			if r.Percent != nil {
				pct = fmt.Sprintf("%.0f%%", *r.Percent)
			}
			if r.Bytes > 0 {
				pct = strings.TrimSpace(pct + " " + fmtBytes(r.Bytes))
			}
			rt.row(string(r.Kind), r.ID, r.Database, r.Phase, pct, fmtTime(r.StartedAt))
		}
		if err := rt.flush(); err != nil {
			return err
		}
	}
	if ready != nil && len(ready.Value.Rows) > 0 {
		fmt.Fprintln(w)
		rt := newTable(w, "CONNECTION", "DATABASE", "STATUS", "RPO AGE / TARGET", "LAST GOOD BACKUP", "REASONS")
		for _, r := range ready.Value.Rows {
			conn := r.ConnectionName
			if conn == "" {
				conn = r.ConnectionID
			}
			rpo := fmtSeconds(r.RPO.AgeSeconds) + " / " + fmtSeconds(r.RPO.TargetSeconds)
			last := ""
			if r.LastGoodBackup != nil {
				last = r.LastGoodBackup.ID
			}
			rt.row(conn, r.Database, r.Status, rpo, last, strings.Join(r.Reasons, ","))
		}
		return rt.flush()
	}
	return nil
}

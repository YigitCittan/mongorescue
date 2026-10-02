package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/apiclient"
	"github.com/yigitcittan/mongorescue/internal/models"
)

const backupUsage = `Usage: mongorescue backup --job ID [flags]
       mongorescue backup --connection ID --database NAME [flags]

Starts a backup on the server: the run of a scheduled job (--job), or an on-demand
backup of one database of a managed connection. Without --wait it prints the new
backup (or job run) and returns at once; with --wait it polls until it finishes and
exits 1 when it failed. Needs an operator or admin API key.
`

// runBackup implements "mongorescue backup".
func runBackup(ctx context.Context, s *session, args []string) error {
	fs := s.newFlagSet("backup", backupUsage, true)
	job := fs.String("job", "", "Run this job now")
	conn := fs.String("connection", "", "Connection ID to back up (with --database)")
	database := fs.String("database", "", "Database to back up (with --connection)")
	collections := fs.String("collections", "", "Only these collections, comma-separated")
	exclude := fs.String("exclude-collections", "", "Every collection except these, comma-separated")
	target := fs.String("storage-target", "", "Storage target ID (default: the default target)")
	var gzip optBool
	fs.Var(&gzip, "gzip", "Compress the archive (default: the server's setting; --gzip=false turns it off)")
	usersAndRoles := fs.Bool("users-and-roles", false, "Also dump the database's users and roles")
	pos, err := s.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErrorf("backup takes no arguments, got %s (use --job or --connection and --database)", strings.Join(pos, " "))
	}
	*job, *conn, *database = strings.TrimSpace(*job), strings.TrimSpace(*conn), strings.TrimSpace(*database)
	if *job != "" {
		for _, name := range []string{"connection", "database", "collections", "exclude-collections", "storage-target", "gzip", "users-and-roles"} {
			if s.set[name] {
				return usageErrorf("--%s cannot be combined with --job: the job's settings apply", name)
			}
		}
	} else if *conn == "" || *database == "" {
		return usageErrorf("give --job, or --connection and --database")
	}
	if *collections != "" && *exclude != "" {
		return usageErrorf("--collections and --exclude-collections cannot be combined")
	}
	client, err := s.connect()
	if err != nil {
		return err
	}
	if *job != "" {
		return s.runJob(ctx, client, *job)
	}
	res, err := client.StartBackup(ctx, apiclient.BackupRequest{
		ConnectionID: *conn, Database: *database, Collections: csv(*collections), ExcludeCollections: csv(*exclude),
		StorageTargetID: strings.TrimSpace(*target), Gzip: gzip.ptr(), IncludeUsersAndRoles: *usersAndRoles,
	})
	if err != nil {
		return err
	}
	return s.finishBackup(ctx, client, res)
}

// runJob starts job id and reports (or waits for) its backup or run.
func (s *session) runJob(ctx context.Context, client *apiclient.Client, id string) error {
	res, err := client.RunJob(ctx, id)
	if err != nil {
		return err
	}
	if b := res.Value.Backup; b != nil {
		return s.finishBackup(ctx, client, &apiclient.Result[models.BackupRecord]{Value: *b, Raw: res.Raw})
	}
	run := res.Value.Run
	final := &apiclient.Result[models.JobRun]{Value: *run, Raw: res.Raw}
	if !s.opts.wait {
		return s.printRun(final, true)
	}
	s.infof("Started run %s of job %s; waiting for it to finish.", run.ID, id)
	err = s.wait(ctx, waitTarget{kind: "job run", id: run.ID, hint: "mongorescue list backups --job " + id}, func(ctx context.Context) (bool, string, error) {
		got, pollErr := client.GetJobRun(ctx, id, run.ID)
		if pollErr != nil {
			return false, "", pollErr
		}
		final = got
		done := 0
		for _, d := range got.Value.Databases {
			if d.Status != models.StatusInProgress && d.Status != models.StatusPending {
				done++
			}
		}
		return got.Value.Status != models.JobRunRunning, fmt.Sprintf("%s (%d of %d databases done)", got.Value.Status, done, len(got.Value.Databases)), nil
	})
	if err != nil {
		return err
	}
	if err := s.printRun(final, false); err != nil {
		return err
	}
	if final.Value.Status != models.JobRunOK {
		return s.reportedInText(failedErrorf("job run %s %s%s", run.ID, final.Value.Status, suffix(final.Value.Error)))
	}
	return nil
}

// finishBackup prints a started backup, or waits for it with --wait.
func (s *session) finishBackup(ctx context.Context, client *apiclient.Client, res *apiclient.Result[models.BackupRecord]) error {
	if !s.opts.wait {
		return s.printBackup(res, true)
	}
	b := res.Value
	s.infof("Started backup %s of %s; waiting for it to finish.", b.ID, b.Database)
	final := res
	err := s.wait(ctx, waitTarget{kind: "backup", id: b.ID, hint: "mongorescue list backups --id " + b.ID}, func(ctx context.Context) (bool, string, error) {
		got, pollErr := client.GetBackup(ctx, b.ID)
		if pollErr != nil {
			return false, "", pollErr
		}
		final = got
		st := got.Value.Status
		return st != models.StatusPending && st != models.StatusInProgress, progressText(string(st), got.Value.Progress), nil
	})
	if err != nil {
		return err
	}
	if err := s.printBackup(final, false); err != nil {
		return err
	}
	if final.Value.Status != models.StatusCompleted {
		return s.reportedInText(failedErrorf("backup %s %s%s", b.ID, final.Value.Status, suffix(final.Value.ErrorMessage)))
	}
	return nil
}

// printBackup prints a backup record: the JSON, the ID or a summary.
func (s *session) printBackup(res *apiclient.Result[models.BackupRecord], started bool) error {
	b := res.Value
	switch {
	case s.opts.json:
		return writeJSON(s.stdout, res.Raw)
	case s.opts.quiet:
		_, err := fmt.Fprintln(s.stdout, b.ID)
		return err
	case started:
		_, err := fmt.Fprintf(s.stdout, "Started backup %s of %s (%s).\nFollow it with: mongorescue list backups --id %s\n", b.ID, b.Database, b.Status, b.ID)
		return err
	}
	line := fmt.Sprintf("Backup %s of %s %s", b.ID, b.Database, b.Status)
	if b.Status == models.StatusCompleted {
		line += fmt.Sprintf(": %s in %s", fmtBytes(b.SizeBytes), fmtSeconds(b.DurationSeconds))
	} else if b.ErrorMessage != "" {
		line += ": " + clean(b.ErrorMessage)
	}
	_, err := fmt.Fprintln(s.stdout, line+".")
	return err
}

// printRun prints a job run: the JSON, the ID or its databases.
func (s *session) printRun(res *apiclient.Result[models.JobRun], started bool) error {
	r := res.Value
	switch {
	case s.opts.json:
		return writeJSON(s.stdout, res.Raw)
	case s.opts.quiet:
		_, err := fmt.Fprintln(s.stdout, r.ID)
		return err
	case started:
		_, err := fmt.Fprintf(s.stdout, "Started run %s of job %s (%s).\nFollow it with: mongorescue list backups --job %s\n", r.ID, r.JobID, r.Status, r.JobID)
		return err
	}
	fmt.Fprintf(s.stdout, "Run %s of job %s %s in %s.\n", r.ID, r.JobID, r.Status, fmtSeconds(r.DurationSeconds))
	if len(r.Databases) == 0 {
		return nil
	}
	t := newTable(s.stdout, "DATABASE", "BACKUP", "STATUS", "ERROR")
	for _, d := range r.Databases {
		t.row(d.Database, d.BackupID, string(d.Status), clean(d.Error))
	}
	return t.flush()
}

// suffix returns ": msg" for a non-empty message, cleaned.
func suffix(msg string) string {
	if msg = clean(msg); msg == "" {
		return ""
	}
	return ": " + msg
}

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/apiclient"
	"github.com/yigitcittan/mongorescue/internal/models"
)

const backupUsage = `Usage: mongorescue backup --job ID [flags]
       mongorescue backup --connection ID --database NAME [--database NAME...] [flags]
       mongorescue backup --connection ID --databases NAME,NAME [flags]
       mongorescue backup --connection ID --databases-file FILE [flags]

Starts a backup on the server: the run of a scheduled job (--job), or an on-demand
backup of databases of a managed connection. Several databases (--database
repeated, --databases or --databases-file) are backed up in one run, each into its
own backup; a database another backup is running is skipped. A --database value
may carry a collection filter of its own: NAME:collections=A,B backs up only
those collections of NAME, NAME:exclude=A,B every collection but those.
--databases-file reads a JSON array in the API's form, such as
["a", {"name": "b", "exclude_collections": ["logs"]}]. Without --wait it prints the new
backup (or run) and returns at once; with --wait it polls until it finishes and
exits 1 when it failed (for a run: unless every database succeeded, with a summary
per database). Needs an operator or admin API key.
`

// runPageSize is the page size polled for the backups of a run: the server's
// largest page, as many as a run may have backups.
const runPageSize = 200

// stringsFlag is a flag that may be repeated; each value is kept.
type stringsFlag []string

// String implements flag.Value.
func (f *stringsFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

// Set implements flag.Value.
func (f *stringsFlag) Set(v string) error {
	*f = append(*f, strings.TrimSpace(v))
	return nil
}

// runBackup implements "mongorescue backup".
func runBackup(ctx context.Context, s *session, args []string) error {
	fs := s.newFlagSet("backup", backupUsage, true)
	job := fs.String("job", "", "Run this job now")
	conn := fs.String("connection", "", "Connection ID to back up (with --database)")
	var databases stringsFlag
	fs.Var(&databases, "database", "Database to back up (with --connection); repeat it to back up several in one run")
	databaseList := fs.String("databases", "", "Databases to back up in one run, comma-separated (with --connection)")
	databasesFile := fs.String("databases-file", "", "JSON file with the databases to back up in one run: names or {name, collections or exclude_collections}")
	parallelism := fs.Int("parallelism", 0, "With several databases: how many are backed up at once (1 to 4; default 1)")
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
	*job, *conn = strings.TrimSpace(*job), strings.TrimSpace(*conn)
	names, err := backupDatabases(slicesWithout(databases, ""), csv(*databaseList), *databasesFile)
	if err != nil {
		return err
	}
	// --databases and --databases-file always start a run, even of one database.
	multi := len(names) > 1 || s.set["databases"] || s.set["databases-file"]
	if *job != "" {
		for _, name := range []string{"connection", "database", "databases", "databases-file", "parallelism", "collections", "exclude-collections", "storage-target", "gzip", "users-and-roles"} {
			if s.set[name] {
				return usageErrorf("--%s cannot be combined with --job: the job's settings apply", name)
			}
		}
	} else if *conn == "" || len(names) == 0 {
		return usageErrorf("give --job, or --connection and --database (or --databases or --databases-file)")
	}
	if *collections != "" && *exclude != "" {
		return usageErrorf("--collections and --exclude-collections cannot be combined")
	}
	if multi && (*collections != "" || *exclude != "") {
		return usageErrorf("--collections and --exclude-collections apply to a backup of one database; give each database its own filter (--database NAME:exclude=A,B)")
	}
	if s.set["parallelism"] && !multi {
		return usageErrorf("--parallelism needs several databases")
	}
	client, err := s.connect()
	if err != nil {
		return err
	}
	if *job != "" {
		return s.runJob(ctx, client, *job)
	}
	req := apiclient.BackupRequest{
		ConnectionID: *conn, Collections: csv(*collections), ExcludeCollections: csv(*exclude),
		StorageTargetID: strings.TrimSpace(*target), Gzip: gzip.ptr(), IncludeUsersAndRoles: *usersAndRoles,
	}
	if multi {
		req.Databases = names
		if s.set["parallelism"] {
			req.Parallelism = parallelism
		}
		run, startErr := client.StartBackups(ctx, req)
		if startErr != nil {
			return startError(startErr, "backups")
		}
		return s.finishBackupRun(ctx, client, run)
	}
	req.Database = names[0].Name
	if names[0].Filtered() {
		if *collections != "" || *exclude != "" {
			return usageErrorf("give the collection filter of %s either after its name or with --collections/--exclude-collections, not both", req.Database)
		}
		req.Collections, req.ExcludeCollections = names[0].Collections, names[0].ExcludeCollections
	}
	res, err := client.StartBackup(ctx, req)
	if err != nil {
		return startError(err, "backups")
	}
	return s.finishBackup(ctx, client, res)
}

// Collection filter suffixes of a --database value.
const (
	dbCollectionsSuffix = ":collections="
	dbExcludeSuffix     = ":exclude="
)

// parseDatabaseFlag reads a --database value: NAME, NAME:collections=A,B or
// NAME:exclude=A,B.
func parseDatabaseFlag(v string) (models.DatabaseFilter, error) {
	for _, suffix := range []string{dbCollectionsSuffix, dbExcludeSuffix} {
		name, list, ok := strings.Cut(v, suffix)
		if !ok {
			continue
		}
		f := models.DatabaseFilter{Name: strings.TrimSpace(name)}
		if strings.Contains(list, dbCollectionsSuffix) || strings.Contains(list, dbExcludeSuffix) {
			return f, usageErrorf("--database %s: give either :collections= or :exclude=, once", f.Name)
		}
		names := csv(list)
		if f.Name == "" || len(names) == 0 {
			return f, usageErrorf("--database %q: want NAME%sA,B or NAME%sA,B", v, dbCollectionsSuffix, dbExcludeSuffix)
		}
		if suffix == dbCollectionsSuffix {
			f.Collections = names
		} else {
			f.ExcludeCollections = names
		}
		return f, nil
	}
	return models.DatabaseFilter{Name: strings.TrimSpace(v)}, nil
}

// backupDatabases returns the databases of --database (with their filters),
// --databases and --databases-file, in that order.
func backupDatabases(flags, list []string, file string) ([]models.DatabaseFilter, error) {
	out := make([]models.DatabaseFilter, 0, len(flags)+len(list))
	for _, v := range flags {
		f, err := parseDatabaseFlag(v)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	out = append(out, models.DatabaseNames(list...)...)
	if file = strings.TrimSpace(file); file != "" {
		raw, err := os.ReadFile(file) //nolint:gosec // G304: the file the user named.
		if err != nil {
			return nil, usageErrorf("--databases-file: %v", err)
		}
		var entries []models.DatabaseFilter
		if err = json.Unmarshal(raw, &entries); err != nil {
			return nil, usageErrorf("--databases-file %s: want a JSON array of names or {\"name\", \"collections\" or \"exclude_collections\"} objects: %v", file, err)
		}
		if len(entries) == 0 {
			return nil, usageErrorf("--databases-file %s names no database", file)
		}
		for _, entry := range entries {
			if len(entry.Collections) > 0 && len(entry.ExcludeCollections) > 0 {
				return nil, usageErrorf("--databases-file %s: database %s: give collections or exclude_collections, not both", file, entry.Name)
			}
		}
		out = append(out, entries...)
	}
	return out, nil
}

// slicesWithout returns the values of list other than drop.
func slicesWithout(list []string, drop string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != drop {
			out = append(out, v)
		}
	}
	return out
}

// finishBackupRun prints a started run of several backups, or waits for all of them
// with --wait: it exits 0 only when every database was backed up.
func (s *session) finishBackupRun(ctx context.Context, client *apiclient.Client, res *apiclient.Result[apiclient.BackupRun]) error {
	run := res.Value
	for _, b := range run.Busy {
		s.warnf("skipped %s: %s", b.Database, clean(b.Error))
	}
	hint := "mongorescue list backups --run " + run.RunID
	if !s.opts.wait {
		switch {
		case s.opts.json:
			return writeJSON(s.stdout, res.Raw)
		case s.opts.quiet:
			_, err := fmt.Fprintln(s.stdout, run.RunID)
			return err
		}
		fmt.Fprintf(s.stdout, "Started run %s: %d backups.\nFollow it with: %s\n", run.RunID, len(run.Backups), hint)
		return s.printRunBackups(run)
	}
	s.infof("Started run %s of %d backups; waiting for them to finish.", run.RunID, len(run.Backups))
	final := run
	query := url.Values{"run_id": {run.RunID}, "limit": {strconv.Itoa(runPageSize)}}
	err := s.wait(ctx, waitTarget{kind: "run", id: run.RunID, hint: hint}, func(ctx context.Context) (bool, string, error) {
		got, pollErr := client.ListBackups(ctx, query)
		if pollErr != nil {
			return false, "", pollErr
		}
		byID := make(map[string]models.BackupRecord, len(got.Value))
		for _, b := range got.Value {
			byID[b.ID] = b
		}
		done := 0
		for i, b := range run.Backups {
			if latest, ok := byID[b.ID]; ok {
				final.Backups[i] = latest
			}
			if st := final.Backups[i].Status; st != models.StatusPending && st != models.StatusInProgress {
				done++
			}
		}
		return done == len(run.Backups), fmt.Sprintf("%d of %d databases done", done, len(run.Backups)), nil
	})
	if err != nil {
		return err
	}
	failed := len(final.Busy)
	for _, b := range final.Backups {
		if b.Status != models.StatusCompleted {
			failed++
		}
	}
	switch {
	case s.opts.json:
		raw, marshalErr := json.Marshal(final)
		if marshalErr != nil {
			return fmt.Errorf("%w: encode the run: %w", ErrFailed, marshalErr)
		}
		if err = writeJSON(s.stdout, raw); err != nil {
			return err
		}
	case s.opts.quiet:
		if _, err = fmt.Fprintln(s.stdout, final.RunID); err != nil {
			return err
		}
	default:
		fmt.Fprintf(s.stdout, "Run %s: %d of %d databases backed up.\n", final.RunID, len(final.Backups)+len(final.Busy)-failed, len(final.Backups)+len(final.Busy))
		if err = s.printRunBackups(final); err != nil {
			return err
		}
	}
	if failed > 0 {
		return s.reportedInText(failedErrorf("run %s: %d of %d databases not backed up", final.RunID, failed, len(final.Backups)+len(final.Busy)))
	}
	return nil
}

// printRunBackups prints the databases of a run of backups as a table.
func (s *session) printRunBackups(run apiclient.BackupRun) error {
	t := newTable(s.stdout, "DATABASE", "BACKUP", "STATUS", "SIZE", "ERROR")
	for _, b := range run.Backups {
		size := ""
		if b.Status == models.StatusCompleted {
			size = fmtBytes(b.SizeBytes)
		}
		t.row(b.Database, b.ID, string(b.Status), size, clean(b.ErrorMessage))
	}
	for _, b := range run.Busy {
		t.row(b.Database, "", "skipped (already running)", "", clean(b.Error))
	}
	return t.flush()
}

// runJob starts job id and reports (or waits for) its backup or run.
func (s *session) runJob(ctx context.Context, client *apiclient.Client, id string) error {
	res, err := client.RunJob(ctx, id)
	if err != nil {
		return startError(err, "backups --job "+id)
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

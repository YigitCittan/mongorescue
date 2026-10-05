package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/apiclient"
	"github.com/yigitcittan/mongorescue/internal/models"
)

const restoreUsage = `Usage: mongorescue restore BACKUP_ID [flags]
       mongorescue restore --pitr STREAM_OR_CONNECTION --at RFC3339 [--database DB] [flags]

Restores a backup. By default it restores into a new safe clone database
(<db>_rescue_<timestamp>) and never touches existing data. Restoring into the
backup's own database (or --target-database) needs both --in-place and --confirm;
the CLI never prompts. The restore preflight runs first: a failed check prints the
checks and exits 1 unless --force is given. Safe clones need an operator key;
in-place and cross-connection restores need an admin key.

With --pitr (experimental, admin key) it restores a replica set to a point in
time from its PITR stream (a stream or connection ID): every database, or those
given with --database, goes into a new <db>_rescue_<timestamp> database, and
writes up to and including the second --at are replayed. Point-in-time restores
are never in place.
`

// runRestore implements "mongorescue restore".
func runRestore(ctx context.Context, s *session, args []string) error {
	fs := s.newFlagSet("restore", restoreUsage, true)
	inPlace := fs.Bool("in-place", false, "Restore into the backup's database (or --target-database) instead of a safe clone; needs --confirm")
	confirm := fs.Bool("confirm", false, "Confirm an --in-place restore, which writes into an existing database")
	targetDB := fs.String("target-database", "", "With --in-place: restore into this database instead of the backup's")
	drop := fs.Bool("drop", false, "With --in-place: drop each restored collection in the target first")
	targetConn := fs.String("target-connection", "", "Restore into this connection instead of the backup's (admin)")
	collections := fs.String("collections", "", "Restore only these collections, comma-separated")
	dryRun := fs.Bool("dry-run", false, "Check the archive and the target without writing anything")
	verifyArchive := fs.Bool("verify-archive", false, "Verify the archive's checksum before restoring (default: the server's policy)")
	noVerifyArchive := fs.Bool("no-verify-archive", false, "Do not verify the archive first (safe clones only)")
	verifyRestore := fs.Bool("verify-restore", false, "Compare the restored database with the backup's manifest afterwards")
	skipPreflight := fs.Bool("skip-preflight", false, "Do not run the preflight first (the server still checks; --force overrides it)")
	force := fs.Bool("force", false, "Restore although a preflight check failed")
	stream := fs.String("pitr", "", "Restore to a point in time from this PITR stream or connection (admin; experimental)")
	at := fs.String("at", "", "With --pitr: the RFC 3339 time to restore to, e.g. 2026-10-05T14:30:00Z")
	databases := fs.String("database", "", "With --pitr: restore only these databases, comma-separated (default: all but admin, config and local)")
	pos, err := s.parse(fs, args)
	if err != nil {
		return err
	}
	if s.set["pitr"] || s.set["at"] || s.set["database"] {
		req, pitrErr := s.pitrRequest(pos, *stream, *at, *databases)
		if pitrErr != nil {
			return pitrErr
		}
		req.TargetConnectionID = strings.TrimSpace(*targetConn)
		return s.startRestore(ctx, req, *skipPreflight, *force, "restores")
	}
	id, err := onlyOneID(pos, "backup")
	if err != nil {
		return err
	}
	switch {
	case *inPlace && !*confirm:
		return usageErrorf("--in-place overwrites data in an existing database; add --confirm to restore in place (there is no prompt)")
	case *confirm && !*inPlace:
		return usageErrorf("--confirm only applies to --in-place")
	case s.set["target-database"] && !*inPlace:
		return usageErrorf("--target-database needs --in-place --confirm (a safe clone picks its own name)")
	case *drop && !*inPlace:
		return usageErrorf("--drop needs --in-place --confirm (a safe clone is a new database)")
	case *verifyArchive && *noVerifyArchive:
		return usageErrorf("--verify-archive and --no-verify-archive cannot be combined")
	}
	req := models.RestoreRequest{
		BackupID: id, SelectedCollections: csv(*collections), DryRun: *dryRun,
		TargetConnectionID: strings.TrimSpace(*targetConn), VerifyRestore: *verifyRestore,
	}
	if *verifyArchive || *noVerifyArchive {
		v := *verifyArchive
		req.Verify = &v
	}
	if *inPlace {
		safeClone := false
		req.SafeClone, req.ConfirmInPlace = &safeClone, true
		req.TargetDatabase, req.DropTarget = strings.TrimSpace(*targetDB), *drop
	}
	return s.startRestore(ctx, req, *skipPreflight, *force, "restores --backup "+id)
}

// pitrOnlyFlags are the restore flags that do not apply to --pitr.
var pitrOnlyFlags = []string{"in-place", "confirm", "target-database", "drop", "collections", "dry-run", "verify-archive", "no-verify-archive", "verify-restore"}

// pitrRequest builds the request of "restore --pitr".
func (s *session) pitrRequest(pos []string, stream, at, databases string) (models.RestoreRequest, error) {
	switch {
	case len(pos) > 0:
		return models.RestoreRequest{}, usageErrorf("--pitr chooses its base backup itself; drop the backup ID %s", pos[0])
	case strings.TrimSpace(stream) == "":
		return models.RestoreRequest{}, usageErrorf("--at and --database need --pitr STREAM_OR_CONNECTION")
	case strings.TrimSpace(at) == "":
		return models.RestoreRequest{}, usageErrorf("--pitr needs --at, the RFC 3339 time to restore to")
	}
	for _, f := range pitrOnlyFlags {
		if s.set[f] {
			return models.RestoreRequest{}, usageErrorf("--%s does not apply to --pitr (point-in-time restores always go into new databases)", f)
		}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(at))
	if err != nil {
		return models.RestoreRequest{}, usageErrorf("--at %q is not an RFC 3339 time such as 2026-10-05T14:30:00Z", at)
	}
	return models.RestoreRequest{PITR: &models.PITRTarget{StreamID: strings.TrimSpace(stream), At: &t}, Databases: csv(databases)}, nil
}

// startRestore runs the preflight (unless skipped) and starts req.
func (s *session) startRestore(ctx context.Context, req models.RestoreRequest, skipPreflight, force bool, listHint string) error {
	client, err := s.connect()
	if err != nil {
		return err
	}
	if !skipPreflight {
		pre, preErr := client.Preflight(ctx, req)
		if preErr != nil {
			return preErr
		}
		if preErr = s.checkPreflight(pre.Value, pre.Raw, force); preErr != nil {
			return preErr
		}
	}
	req.Force = force
	res, err := client.StartRestore(ctx, req)
	if err != nil {
		if apiErr, ok := apiclient.AsAPIError(err); ok {
			if p, ok := apiErr.Preflight(); ok {
				return s.preflightFailed(p, apiErr.Data)
			}
		}
		return startError(err, listHint)
	}
	return s.finishRestore(ctx, client, res)
}

// checkPreflight reports the warnings of a preflight and refuses a failed one unless
// force is set.
func (s *session) checkPreflight(p models.PreflightResult, raw []byte, force bool) error {
	if p.OK {
		for _, c := range p.Checks {
			if c.Status == models.PreflightWarn {
				s.infof("preflight warning: %s: %s", c.ID, clean(c.Message))
			}
		}
		return nil
	}
	if !force {
		return s.preflightFailed(&p, raw)
	}
	s.warnf("the restore preflight failed; restoring anyway because of --force")
	if !s.opts.quiet {
		return writeChecks(s.stderr, &p)
	}
	return nil
}

// preflightFailed prints the checks of a failed preflight (the JSON on stdout with
// --json, else a table on stderr) and returns an ErrFailed error.
func (s *session) preflightFailed(p *models.PreflightResult, raw []byte) error {
	var failed []string
	for _, c := range p.Checks {
		if c.Status == models.PreflightFail {
			failed = append(failed, c.ID)
		}
	}
	err := failedErrorf("the restore preflight failed (%s); fix the target or add --force to restore anyway", strings.Join(failed, ", "))
	if s.opts.json {
		if werr := writeJSON(s.stdout, raw); werr != nil {
			return werr
		}
		return err
	}
	if werr := writeChecks(s.stderr, p); werr != nil {
		return werr
	}
	return err
}

// finishRestore prints a started restore, or waits for it with --wait.
func (s *session) finishRestore(ctx context.Context, client *apiclient.Client, res *apiclient.Result[models.RestoreRecord]) error {
	if !s.opts.wait {
		return s.printRestore(res, true)
	}
	r := res.Value
	s.infof("Started restore %s of backup %s into %s; waiting for it to finish.", r.ID, r.BackupID, r.TargetDatabase)
	final := res
	err := s.wait(ctx, waitTarget{kind: "restore", id: r.ID, hint: "mongorescue list restores --id " + r.ID}, func(ctx context.Context) (bool, string, error) {
		got, pollErr := client.GetRestore(ctx, r.ID)
		if pollErr != nil {
			return false, "", pollErr
		}
		final = got
		st := got.Value.Status
		return st != models.RestoreStatusPending && st != models.RestoreStatusInProgress, progressText(string(st), got.Value.Progress), nil
	})
	if err != nil {
		return err
	}
	if err := s.printRestore(final, false); err != nil {
		return err
	}
	f := final.Value
	switch {
	case f.Status != models.RestoreStatusCompleted:
		return s.reportedInText(failedErrorf("restore %s %s%s", f.ID, f.Status, suffix(f.ErrorMessage)))
	case f.Verification != nil && f.Verification.Status == models.RestoreVerificationFailed:
		return s.reportedInText(failedErrorf("restore %s completed, but its verification failed: %s", f.ID, clean(strings.Join(f.Verification.Mismatches, "; "))))
	}
	return nil
}

// printRestore prints a restore record: the JSON, the ID or a summary.
func (s *session) printRestore(res *apiclient.Result[models.RestoreRecord], started bool) error {
	r := res.Value
	switch {
	case s.opts.json:
		return writeJSON(s.stdout, res.Raw)
	case s.opts.quiet:
		_, err := fmt.Fprintln(s.stdout, r.ID)
		return err
	}
	mode := "safe clone"
	if r.InPlace {
		mode = "in place"
	}
	if r.PITR != nil {
		mode = "safe clones, point in time " + r.PITR.TargetTime.Format(time.RFC3339)
	}
	if r.DryRun {
		mode += ", dry run"
	}
	if started {
		_, err := fmt.Fprintf(s.stdout, "Started restore %s of backup %s: %s -> %s (%s).\nFollow it with: mongorescue list restores --id %s\n",
			r.ID, r.BackupID, r.SourceDatabase, r.TargetDatabase, mode, r.ID)
		return err
	}
	line := fmt.Sprintf("Restore %s of backup %s %s: %s -> %s (%s)", r.ID, r.BackupID, r.Status, r.SourceDatabase, r.TargetDatabase, mode)
	if r.Status == models.RestoreStatusCompleted {
		line += " in " + fmtSeconds(r.DurationSeconds)
	} else if r.ErrorMessage != "" {
		line += ": " + clean(r.ErrorMessage)
	}
	if v := r.Verification; v != nil {
		line += ".\nVerification: " + string(v.Status)
		if len(v.Mismatches) > 0 {
			line += ": " + clean(strings.Join(v.Mismatches, "; "))
		}
	}
	if r.Warning != "" {
		line += ".\nWarning: " + clean(r.Warning)
	}
	_, err := fmt.Fprintln(s.stdout, line+".")
	return err
}

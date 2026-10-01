package integrity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Restore test limits.
const (
	// dropTimeout bounds dropping the temporary database, which runs detached from
	// the test's cancellation.
	dropTimeout = 2 * time.Minute
	// persistTimeout bounds recording a finished test.
	persistTimeout = 10 * time.Second
	// dueSlack lets a time-based restore test run although the previous one is a
	// little less than its interval ago (cron runs are not exactly periodic).
	dueSlack = time.Hour
	// MaxRestoreTestList caps ListRestoreTests.
	MaxRestoreTestList = 100
)

// restoreTestsAvailable reports whether the dependencies of restore tests are set.
func (s *Service) restoreTestsAvailable() bool {
	return s.cfg.Restore != nil && s.cfg.Admin != nil && s.cfg.Connections != nil
}

// latestBackup returns the newest completed backup of job.
func (s *Service) latestBackup(ctx context.Context, job *models.Job) (*models.BackupRecord, error) {
	records, err := s.cfg.Store.ListBackupRecords(ctx, job.Database)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	for _, r := range records { // newest first
		if r.JobID == job.ID && r.Status == models.StatusCompleted && r.StorageKey != "" {
			return r, nil
		}
	}
	return nil, fmt.Errorf("%w: job %s", ErrNoBackup, job.ID)
}

// StartRestoreTest checks that job jobID has a backup to test and runs a restore
// test of it in the background (whether or not the job enables scheduled tests).
// It returns the backup that is tested. Expected failures: ErrNotFound, ErrNoBackup,
// ErrUnavailable, ErrBusy and runs.ErrShuttingDown.
func (s *Service) StartRestoreTest(ctx context.Context, jobID string) (*models.BackupRecord, error) {
	if !s.restoreTestsAvailable() {
		return nil, fmt.Errorf("%w: restore tests need the restore engine and a MongoDB connection", ErrUnavailable)
	}
	job, err := s.cfg.Store.GetJob(ctx, jobID)
	if err != nil {
		return nil, notFound(err, "job not found")
	}
	backup, err := s.latestBackup(ctx, job)
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Runs.Go(keyPrefixRestoreTest+jobID, func(runCtx context.Context) {
		s.runRestoreTest(runCtx, job, backup, TriggerManual)
	}); err != nil {
		return nil, err
	}
	return backup, nil
}

// AfterBackup is the scheduler hook (scheduler.AfterBackupFunc): after a successful
// scheduled backup of job it runs the job's restore test of that backup when the
// policy enables one and it is due. It returns once the test finished.
func (s *Service) AfterBackup(ctx context.Context, job *models.Job, record *models.BackupRecord) {
	if !s.restoreTestsAvailable() || job == nil || record == nil || record.Status != models.StatusCompleted {
		return
	}
	current, err := s.cfg.Store.GetJob(ctx, job.ID)
	if err != nil {
		return
	}
	if !s.restoreTestDue(ctx, current) {
		return
	}
	release, err := s.cfg.Runs.Acquire(keyPrefixRestoreTest + job.ID)
	if err != nil {
		s.logger.Info("skipping the scheduled restore test: one is already running", slog.String("job_id", job.ID))
		return
	}
	defer release()
	s.runRestoreTest(ctx, current, record, TriggerScheduled)
}

// restoreTestDue reports whether job's restore test policy asks for a test now.
func (s *Service) restoreTestDue(ctx context.Context, job *models.Job) bool {
	p := job.RestoreTest
	if p == nil || !p.Enabled {
		return false
	}
	last := job.LastRestoreTest
	if p.Frequency == models.RestoreTestEveryN {
		records, err := s.cfg.Store.ListBackupRecords(ctx, job.Database)
		if err != nil {
			return false
		}
		n := 0
		for _, r := range records {
			if r.JobID != job.ID || r.Status != models.StatusCompleted {
				continue
			}
			if last == nil || (r.CompletedAt != nil && r.CompletedAt.After(last.At)) {
				n++
			}
		}
		return n >= max(p.EveryN, 1)
	}
	return last == nil || s.now().Sub(last.At) >= p.Interval()-dueSlack
}

// runRestoreTest tests backup of job and records the result; it never returns an
// error (failures are the result) and never panics.
func (s *Service) runRestoreTest(ctx context.Context, job *models.Job, backup *models.BackupRecord, trigger string) (res *models.RestoreTestResult) {
	res = &models.RestoreTestResult{
		ID: newRestoreTestID(backup.Database, s.now()), JobID: job.ID, BackupID: backup.ID, Database: backup.Database,
		Trigger: trigger, Status: models.RestoreTestError, StartedAt: s.now(),
	}
	s.logger.Info("restore test started", slog.String("job_id", job.ID), slog.String("backup_id", backup.ID), slog.String("trigger", trigger))
	defer s.finishRestoreTest(ctx, job, res)
	defer func() {
		if p := recover(); p != nil {
			s.logger.Error("restore test panicked", slog.Any("panic", p), slog.String("stack", string(debug.Stack())))
			res.Status, res.Error = models.RestoreTestError, "internal error during the restore test"
		}
	}()
	if err := s.restoreTest(ctx, job, backup, res); err != nil {
		res.Status, res.Error = models.RestoreTestError, redact.Text(err.Error())
	}
	return res
}

// restoreTest performs a restore test, filling res. It returns the reason of a test
// that could not restore or compare.
func (s *Service) restoreTest(ctx context.Context, job *models.Job, backup *models.BackupRecord, res *models.RestoreTestResult) error {
	connID := backup.ConnectionID
	if job.RestoreTest != nil && job.RestoreTest.ConnectionID != "" {
		connID = job.RestoreTest.ConnectionID
	}
	if connID == "" {
		connID = job.ConnectionID
	}
	if connID == "" {
		return errors.New("no connection to restore into: the backup records none and the job has no test connection")
	}
	conn, err := s.cfg.Connections.Resolve(ctx, connID)
	if err != nil {
		return fmt.Errorf("resolve connection %s: %w", connID, err)
	}
	res.ConnectionID, res.ConnectionName = conn.ID, conn.Name
	encrypted := backup.Encrypted || strings.HasSuffix(backup.StorageKey, ".age")
	if encrypted && !s.cfg.Restore.CanDecrypt() {
		return errors.New("the backup is encrypted and no decryption key is configured under Settings → Encryption")
	}

	// The random suffix makes the name unique; the lock on it and the existence
	// check below are guards on top.
	suffix, err := models.NewRescueVerifySuffix()
	if err != nil {
		return err
	}
	temp, err := models.RescueVerifyDatabaseName(backup.Database, res.StartedAt, suffix)
	if err != nil {
		return err
	}
	if temp == backup.Database || !models.IsRescueVerifyDatabaseName(temp) {
		return fmt.Errorf("refusing temporary database name %s", temp)
	}
	releaseTemp, err := s.cfg.Runs.Acquire(keyPrefixRestoreTestDB + conn.ID + "/" + temp)
	if err != nil {
		return fmt.Errorf("%w: %s is in use by another restore test", ErrTempDatabaseExists, temp)
	}
	defer releaseTemp()
	missing, err := s.cfg.Admin.RestoreTestPrivileges(ctx, conn.URI, temp)
	if err != nil {
		return fmt.Errorf("check the privileges of connection %s: %w", conn.Name, err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: connection %s may not %s on %s; grant e.g. readWriteAnyDatabase (or restore plus dbAdminAnyDatabase), or choose a test connection with such a user",
			ErrInsufficientPrivileges, conn.Name, strings.Join(missing, ", "), temp)
	}
	exists, err := s.cfg.Admin.DatabaseExists(ctx, conn.URI, temp)
	if err != nil {
		return fmt.Errorf("check the temporary database %s: %w", temp, err)
	}
	if exists {
		return fmt.Errorf("%w: %s; it is left untouched", ErrTempDatabaseExists, temp)
	}

	// From here on the temporary database belongs to this test: it is dropped in
	// every case (success, failure, cancellation, panic), and only this exact name.
	res.TempDatabase = temp
	defer s.dropTemp(ctx, conn.URI, backup.Database, temp, res)

	noVerifyPass := false
	req := models.RestoreRequest{
		BackupID: backup.ID, MongoURI: conn.URI, TargetConnectionID: conn.ID, TargetConnectionName: conn.Name,
		CloneDatabase: temp, Verify: &noVerifyPass, // the streamed bytes are still checked against the checksum
	}
	record, err := s.cfg.Restore.Prepare(req, backup)
	if err != nil {
		return fmt.Errorf("prepare restore: %w", err)
	}
	if record.TargetDatabase != temp {
		return fmt.Errorf("refusing restore into %s (expected %s)", record.TargetDatabase, temp)
	}
	if record, err = s.cfg.Restore.Execute(ctx, req, backup, record); err != nil {
		msg := err.Error()
		if record != nil && record.ErrorMessage != "" {
			msg = record.ErrorMessage
		}
		return fmt.Errorf("restore failed: %s", msg)
	}

	actual, err := s.cfg.Admin.Manifest(ctx, conn.URI, temp)
	if err != nil {
		return fmt.Errorf("inspect the restored copy: %w", err)
	}
	res.Collections, res.Documents = len(actual.Collections), actual.Documents()
	expected, err := s.cfg.Store.GetManifest(ctx, backup.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		res.Notes = append(res.Notes, "no manifest was captured for this backup: the restore succeeded but counts and indexes were not compared")
	case err != nil:
		return fmt.Errorf("load the backup manifest: %w", err)
	default:
		res.Mismatches, res.Notes = models.CompareManifests(expected, actual)
	}
	res.Status = models.RestoreTestOK
	if len(res.Mismatches) > 0 {
		res.Status = models.RestoreTestMismatch
	}
	return nil
}

// dropTemp drops temp, the temporary database this test created, detached from
// ctx's cancellation. The name is passed by value at creation time, so nothing that
// changes res afterwards can redirect the drop. It never drops the source database
// or a name that is not a restore test's.
func (s *Service) dropTemp(ctx context.Context, uri, source, temp string, res *models.RestoreTestResult) {
	if temp == "" || temp == source || !models.IsRescueVerifyDatabaseName(temp) {
		return
	}
	dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dropTimeout)
	defer cancel()
	if err := s.cfg.Admin.DropDatabase(dropCtx, uri, temp); err != nil {
		res.DropError = redact.Text(err.Error())
		s.logger.Error("failed to drop the restore test database; drop it manually",
			slog.String("database", temp), slog.String("error", res.DropError))
		return
	}
	res.Dropped = true
}

// finishRestoreTest records a finished test: its result, the summary on the job and
// on the backup, and a restore_test event.
func (s *Service) finishRestoreTest(ctx context.Context, job *models.Job, res *models.RestoreTestResult) {
	done := s.now()
	res.CompletedAt = &done
	res.DurationSeconds = done.Sub(res.StartedAt).Seconds()
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	if err := s.cfg.Store.SaveRestoreTest(writeCtx, res); err != nil {
		s.logger.Error("failed to record the restore test", slog.String("job_id", job.ID), slog.Any("error", err))
	}
	summary := res.Summary()
	if err := s.cfg.Store.UpdateJobRestoreTest(writeCtx, job.ID, summary); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("failed to record the restore test on the job", slog.String("job_id", job.ID), slog.Any("error", err))
	}
	if _, err := s.cfg.Store.UpdateBackupRecord(writeCtx, res.BackupID, func(r *models.BackupRecord) error {
		r.LastRestoreTest = summary
		return nil
	}); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("failed to record the restore test on the backup", slog.String("backup_id", res.BackupID), slog.Any("error", err))
	}
	s.publish(writeCtx, events.RestoreTestEvent(res))
	attrs := []any{slog.String("job_id", job.ID), slog.String("backup_id", res.BackupID), slog.String("status", string(res.Status)),
		slog.Float64("duration_sec", res.DurationSeconds), slog.Bool("dropped", res.Dropped)}
	if res.Status == models.RestoreTestOK {
		s.logger.Info("restore test passed", attrs...)
	} else {
		s.logger.Warn("restore test failed", append(attrs, slog.String("error", res.Error), slog.Int("mismatches", len(res.Mismatches)))...)
	}
}

// ListRestoreTests returns up to limit (at most MaxRestoreTestList) restore tests of
// job jobID, newest first.
func (s *Service) ListRestoreTests(ctx context.Context, jobID string, limit int) ([]*models.RestoreTestResult, error) {
	if _, err := s.cfg.Store.GetJob(ctx, jobID); err != nil {
		return nil, notFound(err, "job not found")
	}
	if limit <= 0 || limit > MaxRestoreTestList {
		limit = MaxRestoreTestList
	}
	return s.cfg.Store.ListRestoreTests(ctx, jobID, limit)
}

// newRestoreTestID returns "rt_<db>_<timestamp>_<suffix>".
func newRestoreTestID(database string, at time.Time) string {
	const overhead = len("rt___") + len("20060102_150405") + models.IDSuffixLength
	suffix, err := models.NewIDSuffix()
	if err != nil {
		suffix = fmt.Sprintf("%0*d", models.IDSuffixLength, at.Nanosecond()%1_000_000)
	}
	return fmt.Sprintf("rt_%s_%s_%s", models.SanitizeIDComponent(database, models.MaxIDLength-overhead), at.UTC().Format("20060102_150405"), suffix)
}

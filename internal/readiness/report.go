package readiness

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Status is the overall readiness of a database.
type Status string

// Readiness statuses.
const (
	// StatusOK means every check passed.
	StatusOK Status = "ok"
	// StatusWarn means a check could not prove readiness (no restore test, no
	// verified backup, paused jobs, keys not escrowed).
	StatusWarn Status = "warn"
	// StatusFail means the database is not recoverable within its objectives: an RPO
	// is missed, or the newest restore test or verification failed.
	StatusFail Status = "fail"
)

// Reasons explain a Row's status. Fail reasons come first.
const (
	// ReasonRPOMissed: the newest successful backup is older than a job's RPO.
	ReasonRPOMissed = "rpo_missed"
	// ReasonRestoreTestFailed: the newest restore test failed or found differences.
	ReasonRestoreTestFailed = "restore_test_failed"
	// ReasonVerificationFailed: the newest successful backup failed verification.
	ReasonVerificationFailed = "verification_failed"
	// ReasonNoBackup: there is no successful backup yet (the RPO is still met).
	ReasonNoBackup = "no_backup"
	// ReasonPaused: every job covering the database is paused.
	ReasonPaused = "paused"
	// ReasonNotVerified: no backup of the database passed verification.
	ReasonNotVerified = "not_verified"
	// ReasonNoRestoreTest: the database was never restore-tested.
	ReasonNoRestoreTest = "no_restore_test"
	// ReasonKeysNotEscrowed: the newest backup is encrypted and no recovery kit holds
	// the current keys.
	ReasonKeysNotEscrowed = "keys_not_escrowed"
)

// RTO sources.
const (
	// RTOSourceRestoreTest: the duration of the newest successful restore test.
	RTOSourceRestoreTest = "restore_test"
	// RTOSourceRestore: the duration of the newest completed real restore.
	RTOSourceRestore = "restore"
)

// Report is the readiness of every database a job backs up.
type Report struct {
	// GeneratedAt is when the report was computed (UTC).
	GeneratedAt time.Time `json:"generated_at"`
	// KeysEscrowed reports whether a recovery kit holds the current secret.key,
	// encryption keys and storage targets.
	KeysEscrowed bool `json:"keys_escrowed"`
	// Summary counts the rows by status.
	Summary Summary `json:"summary"`
	// Rows has one entry per connection and database, ordered by status (fail
	// first), then connection and database.
	Rows []Row `json:"rows"`
}

// Summary counts rows by status.
type Summary struct {
	// OK, Warn and Fail count the rows of each status.
	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

// Row is the readiness of one database of one connection.
type Row struct {
	// ConnectionID and ConnectionName name the server the database lives on.
	ConnectionID   string `json:"connection_id"`
	ConnectionName string `json:"connection_name,omitempty"`
	// Database is the database.
	Database string `json:"database"`
	// Jobs are the jobs that back it up, with their RPO.
	Jobs []JobRPO `json:"jobs"`
	// LastGoodBackup is the newest completed backup by any of them.
	LastGoodBackup *BackupRef `json:"last_good_backup,omitempty"`
	// LastVerifiedBackup is the newest completed backup whose archive passed
	// verification.
	LastVerifiedBackup *BackupRef `json:"last_verified_backup,omitempty"`
	// LastRestoreTest is the newest restore test of the database.
	LastRestoreTest *RestoreTestRef `json:"last_restore_test,omitempty"`
	// RPO summarises the jobs' objectives.
	RPO RPOStatus `json:"rpo"`
	// RTO estimates how long a restore takes; nil when nothing was ever restored.
	RTO *RTOEstimate `json:"rto,omitempty"`
	// KeysEscrowed repeats Report.KeysEscrowed; Encrypted reports whether the newest
	// good backup is encrypted (it then needs the escrowed keys).
	KeysEscrowed bool `json:"keys_escrowed"`
	Encrypted    bool `json:"encrypted"`
	// Status is the overall readiness; Reasons explain it (see the Reason
	// constants), fail reasons first.
	Status  Status   `json:"status"`
	Reasons []string `json:"reasons"`
}

// JobRPO is the recovery point objective of one job for one database.
type JobRPO struct {
	// ID and Name identify the job; Enabled is false for a paused job.
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// TargetSeconds is the objective; Default reports that it comes from the
	// schedule (the job sets no rpo_minutes).
	TargetSeconds float64 `json:"target_seconds"`
	Default       bool    `json:"default"`
	// AgeSeconds is the age of the job's newest successful backup of the database,
	// or of the job itself when there is none (NoBackup).
	AgeSeconds float64 `json:"age_seconds"`
	NoBackup   bool    `json:"no_backup,omitempty"`
	// Met reports whether the age is within the objective (paused jobs are reported
	// but never fail the row).
	Met bool `json:"met"`
	// BreachedSince is when the checker reported the breach, while it lasts.
	BreachedSince *time.Time `json:"breached_since,omitempty"`
}

// BackupRef points to a backup.
type BackupRef struct {
	// ID is the backup ID.
	ID string `json:"id"`
	// At is when it finished.
	At time.Time `json:"at"`
	// JobID is the job that took it.
	JobID string `json:"job_id,omitempty"`
	// Filtered reports that the backup holds only some collections of the database:
	// only Collections, or all but ExcludedCollections. It still counts towards the
	// RPO; the collections it left out are not covered by it.
	Filtered            bool     `json:"filtered,omitempty"`
	Collections         []string `json:"collections,omitempty"`
	ExcludedCollections []string `json:"exclude_collections,omitempty"`
}

// RestoreTestRef is the newest restore test of a database.
type RestoreTestRef struct {
	// ID identifies the test; JobID is the job it belongs to.
	ID    string `json:"id"`
	JobID string `json:"job_id"`
	// At is when it finished.
	At time.Time `json:"at"`
	// Status is ok, mismatch or error.
	Status models.RestoreTestStatus `json:"status"`
	// DurationSeconds is how long it took.
	DurationSeconds float64 `json:"duration_seconds"`
}

// RPOStatus summarises the objectives of a row's enabled jobs.
type RPOStatus struct {
	// TargetSeconds is the strictest objective of the enabled jobs (0 without one).
	TargetSeconds float64 `json:"target_seconds"`
	// AgeSeconds is the age of the newest successful backup by any job; nil without
	// one.
	AgeSeconds *float64 `json:"age_seconds,omitempty"`
	// Met reports whether every enabled job meets its objective; nil when no
	// enabled job covers the database.
	Met *bool `json:"met,omitempty"`
}

// RTOEstimate is the estimated recovery time of a database.
type RTOEstimate struct {
	// Seconds is the estimate.
	Seconds float64 `json:"seconds"`
	// Source is where it comes from: RTOSourceRestoreTest or RTOSourceRestore.
	Source string `json:"source"`
	// ID is the restore test or restore it was measured on, MeasuredAt when that
	// finished.
	ID         string    `json:"id"`
	MeasuredAt time.Time `json:"measured_at"`
}

// rowKey identifies a row.
type rowKey struct{ connection, database string }

// rowAcc collects a row while the report is built.
type rowAcc struct {
	row      Row
	enabled  int
	missed   bool
	lastOK   *RestoreTestRef
	verifyKO bool
}

// Report computes the readiness of every database a job backs up, enabled or not.
// It fails with an ErrUnavailable error when the metadata cannot be read.
func (s *Service) Report(ctx context.Context) (*Report, error) {
	now := s.now()
	jobs, err := s.cfg.Store.ListJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: list jobs: %w", ErrUnavailable, err)
	}
	points, err := s.points(ctx, jobs, now)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	breaches, err := s.cfg.Store.ListRPOBreaches(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: list rpo breaches: %w", ErrUnavailable, err)
	}
	since := make(map[key]time.Time, len(breaches))
	for _, b := range breaches {
		since[key{b.JobID, b.Database}] = b.Since
	}
	escrowed := s.cfg.KeysEscrowed != nil && s.cfg.KeysEscrowed()
	names := s.connectionNames(ctx)

	// One query per kind of evidence, whatever the number of jobs.
	verified, err := s.cfg.Store.LatestJobDatabaseBackupsAll(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("%w: verified backups: %w", ErrUnavailable, err)
	}
	tests, err := s.cfg.Store.LatestRestoreTestsAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: restore tests: %w", ErrUnavailable, err)
	}

	rows := map[rowKey]*rowAcc{}
	for _, p := range points {
		rk := rowKey{p.job.ConnectionID, p.database}
		acc := rows[rk]
		if acc == nil {
			acc = &rowAcc{row: Row{
				ConnectionID: rk.connection, ConnectionName: names[rk.connection], Database: rk.database,
				Jobs: []JobRPO{}, KeysEscrowed: escrowed,
			}}
			rows[rk] = acc
		}
		s.addJob(acc, p, now, since)
		addEvidence(acc, p, verified[p.job.ID], tests[p.job.ID])
	}
	restores, err := s.cfg.Store.LatestCompletedRestores(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: latest restores: %w", ErrUnavailable, err)
	}
	byDB := make(map[rowKey]*models.RestoreRecord, len(restores))
	for _, r := range restores {
		byDB[rowKey{r.SourceConnectionID, r.SourceDatabase}] = r
	}

	report := &Report{GeneratedAt: now, KeysEscrowed: escrowed, Rows: make([]Row, 0, len(rows))}
	for rk, acc := range rows {
		finishRow(acc, byDB[rk], now)
		switch acc.row.Status {
		case StatusOK:
			report.Summary.OK++
		case StatusWarn:
			report.Summary.Warn++
		default:
			report.Summary.Fail++
		}
		report.Rows = append(report.Rows, acc.row)
	}
	rank := map[Status]int{StatusFail: 0, StatusWarn: 1, StatusOK: 2}
	slices.SortFunc(report.Rows, func(a, b Row) int {
		return cmp.Or(cmp.Compare(rank[a.Status], rank[b.Status]),
			cmp.Compare(a.ConnectionName, b.ConnectionName), cmp.Compare(a.ConnectionID, b.ConnectionID),
			cmp.Compare(a.Database, b.Database))
	})
	return report, nil
}

// addJob adds p's job and recovery point to acc.
func (s *Service) addJob(acc *rowAcc, p point, now time.Time, since map[key]time.Time) {
	met := p.met(now)
	jr := JobRPO{
		ID: p.job.ID, Name: p.job.Name, Enabled: p.job.Enabled,
		TargetSeconds: p.target.Seconds(), Default: p.isDefault,
		AgeSeconds: p.age(now).Seconds(), NoBackup: p.last == nil, Met: met,
	}
	if at, ok := since[key{p.job.ID, p.database}]; ok && p.job.Enabled {
		jr.BreachedSince = &at
	}
	acc.row.Jobs = append(acc.row.Jobs, jr)
	if p.job.Enabled {
		acc.enabled++
		if !met {
			acc.missed = true
		}
		if acc.row.RPO.TargetSeconds == 0 || jr.TargetSeconds < acc.row.RPO.TargetSeconds {
			acc.row.RPO.TargetSeconds = jr.TargetSeconds
		}
	}
	if p.last == nil {
		return
	}
	at := finishedAt(p.last)
	if cur := acc.row.LastGoodBackup; cur == nil || at.After(cur.At) {
		acc.row.LastGoodBackup = &BackupRef{ID: p.last.ID, At: at, JobID: p.job.ID,
			Filtered: p.last.Filtered, Collections: slices.Clone(p.last.Collections), ExcludedCollections: slices.Clone(p.last.ExcludedCollections)}
		acc.row.Encrypted = p.last.Encrypted
		acc.verifyKO = p.last.Verification == models.VerificationMismatch || p.last.Verification == models.VerificationError
	}
}

// addEvidence adds the newest verified backups (by database) and the restore tests
// (newest first) of p's job for p's database to acc.
func addEvidence(acc *rowAcc, p point, verified map[string]*models.BackupRecord, list []*models.RestoreTestResult) {
	if b := verified[p.database]; b != nil {
		at := finishedAt(b)
		if cur := acc.row.LastVerifiedBackup; cur == nil || at.After(cur.At) {
			acc.row.LastVerifiedBackup = &BackupRef{ID: b.ID, At: at, JobID: p.job.ID}
		}
	}
	for _, t := range list { // newest first
		db := t.Database
		if db == "" && !p.job.MultiDatabase() {
			db = p.job.Database // tests recorded before databases were named
		}
		if db != p.database {
			continue
		}
		ref := &RestoreTestRef{ID: t.ID, JobID: p.job.ID, At: t.StartedAt.UTC(), Status: t.Status, DurationSeconds: t.DurationSeconds}
		if t.CompletedAt != nil {
			ref.At = t.CompletedAt.UTC()
		}
		if cur := acc.row.LastRestoreTest; cur == nil || ref.At.After(cur.At) {
			acc.row.LastRestoreTest = ref
		}
		if t.Status == models.RestoreTestOK {
			if cur := acc.lastOK; cur == nil || ref.At.After(cur.At) {
				acc.lastOK = ref
			}
			break // older successes cannot be newer than this one
		}
	}
}

// finishRow sets acc's RPO summary, RTO estimate (restore is the newest completed
// real restore of the database, or nil) and status.
func finishRow(acc *rowAcc, restore *models.RestoreRecord, now time.Time) {
	r := &acc.row
	if b := r.LastGoodBackup; b != nil {
		age := max(now.Sub(b.At), 0).Seconds()
		r.RPO.AgeSeconds = &age
	}
	if acc.enabled > 0 {
		met := !acc.missed
		r.RPO.Met = &met
	}
	switch {
	case acc.lastOK != nil:
		r.RTO = &RTOEstimate{Seconds: acc.lastOK.DurationSeconds, Source: RTOSourceRestoreTest, ID: acc.lastOK.ID, MeasuredAt: acc.lastOK.At}
	case restore != nil:
		at := restore.StartedAt.UTC()
		if restore.CompletedAt != nil {
			at = restore.CompletedAt.UTC()
		}
		r.RTO = &RTOEstimate{Seconds: restore.DurationSeconds, Source: RTOSourceRestore, ID: restore.ID, MeasuredAt: at}
	}

	var fail, warn []string
	if acc.missed {
		fail = append(fail, ReasonRPOMissed)
	}
	if t := r.LastRestoreTest; t != nil && t.Status != models.RestoreTestOK {
		fail = append(fail, ReasonRestoreTestFailed)
	}
	if acc.verifyKO {
		fail = append(fail, ReasonVerificationFailed)
	}
	if r.LastGoodBackup == nil && !acc.missed {
		warn = append(warn, ReasonNoBackup)
	}
	if acc.enabled == 0 {
		warn = append(warn, ReasonPaused)
	}
	if r.LastGoodBackup != nil && r.LastVerifiedBackup == nil {
		warn = append(warn, ReasonNotVerified)
	}
	if r.LastRestoreTest == nil {
		warn = append(warn, ReasonNoRestoreTest)
	}
	if r.Encrypted && !r.KeysEscrowed {
		warn = append(warn, ReasonKeysNotEscrowed)
	}
	r.Reasons = append(fail, warn...)
	if r.Reasons == nil {
		r.Reasons = []string{}
	}
	switch {
	case len(fail) > 0:
		r.Status = StatusFail
	case len(warn) > 0:
		r.Status = StatusWarn
	default:
		r.Status = StatusOK
	}
}

// connectionNames maps connection IDs to names; a failed lookup leaves names empty.
func (s *Service) connectionNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	if s.cfg.Connections == nil {
		return out
	}
	list, err := s.cfg.Connections.List(ctx)
	if err != nil {
		s.logger.Debug("connection names are unavailable for the readiness report")
		return out
	}
	for _, c := range list {
		out[c.ID] = c.Name
	}
	return out
}

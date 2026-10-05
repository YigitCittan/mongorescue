// Package readiness answers "could we recover, and how fast?" for every database a
// job backs up: the age of its newest successful backup against the job's recovery
// point objective (RPO), its newest verified backup, its newest restore test, an
// estimated recovery time (RTO) and whether the keys are escrowed in a recovery kit.
//
// A background checker evaluates the RPO of every enabled job every few minutes (and
// right after a backup finishes), publishes job.rpo_missed once per breach and
// job.rpo_recovered when it heals, and feeds the job_rpo_* gauges. Breaches are
// persisted, so a restart never alerts again for one already reported.
//
// The package is a business package: it depends on ports (the metadata store, the
// connections, the event publisher), never on the HTTP server.
package readiness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
)

// ErrUnavailable is returned when the readiness report cannot be read from the
// metadata store.
var ErrUnavailable = errors.New("readiness: metadata unavailable")

// Defaults of Config.
const (
	// DefaultCheckInterval is how often the checker evaluates the RPOs.
	DefaultCheckInterval = 5 * time.Minute
	// DefaultStartDelay delays the first check after start-up.
	DefaultStartDelay = 30 * time.Second
)

// Store is the persistence port (implemented by *store.SQLiteStore).
type Store interface {
	// ListJobs returns every job.
	ListJobs(ctx context.Context) ([]*models.Job, error)
	// LatestJobDatabaseBackupsAll returns the newest completed (with verified: and
	// verified) backup of every job and database, keyed by job and database.
	LatestJobDatabaseBackupsAll(ctx context.Context, verified bool) (map[string]map[string]*models.BackupRecord, error)
	// LatestJobDatabaseBackupsAllIn is LatestJobDatabaseBackupsAll over the
	// backups taken from the connections in set (every backup when nil).
	LatestJobDatabaseBackupsAllIn(ctx context.Context, verified bool, set auth.ConnectionSet) (map[string]map[string]*models.BackupRecord, error)
	// LatestRestoreTestsAll returns per job the newest restore test of each
	// database and the newest passed one, newest first.
	LatestRestoreTestsAll(ctx context.Context) (map[string][]*models.RestoreTestResult, error)
	// LatestCompletedRestores returns the newest completed full restore per source
	// connection and database.
	LatestCompletedRestores(ctx context.Context) ([]*models.RestoreRecord, error)
	// JobDatabaseJoins returns, per job and database, when a database joined the
	// job after its first run.
	JobDatabaseJoins(ctx context.Context) (map[string]map[string]time.Time, error)
	// ListRPOBreaches returns the recorded RPO breaches.
	ListRPOBreaches(ctx context.Context) ([]models.RPOBreach, error)
	// AddRPOBreach records a breach unless one is recorded, reporting whether it was
	// added.
	AddRPOBreach(ctx context.Context, b models.RPOBreach) (bool, error)
	// DeleteRPOBreach removes a breach, reporting whether one was recorded.
	DeleteRPOBreach(ctx context.Context, jobID, database string) (bool, error)
}

// Connections lists the managed connections, for their names (implemented by
// *connections.Service).
type Connections interface {
	// List returns every managed connection.
	List(ctx context.Context) ([]*models.Connection, error)
}

// Sample is the recovery point of one database of an enabled job (see
// Config.Observe).
type Sample struct {
	// JobID and Database identify it.
	JobID    string
	Database string
	// Since is when the newest successful backup finished, or when the database
	// joined the job when it has none.
	Since time.Time
	// Target is the job's recovery point objective.
	Target time.Duration
}

// Config holds the dependencies of a Service. Store is required.
type Config struct {
	// Store reads jobs, backups, restore tests and restores and keeps the breaches.
	Store Store
	// Connections names the connections in the report; nil leaves names empty.
	Connections Connections
	// KeysEscrowed reports whether a recovery kit was downloaded for the current
	// secret.key, encryption keys and storage targets; nil means false.
	KeysEscrowed func() bool
	// Streams lists the PITR streams: they appear in the report, and a stream's
	// durable lag is the PITR RPO of the rows of its connection; nil means none.
	Streams StreamLister
	// Publisher receives job.rpo_missed and job.rpo_recovered.
	Publisher events.Publisher
	// Observe receives the samples of every check (metrics) with the time the check
	// started, so series of jobs deleted meanwhile can be dropped; nil means none.
	Observe func(started time.Time, samples []Sample)
	// Logger receives operational logs; nil means slog.Default().
	Logger *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// CheckInterval is how often the checker runs (default DefaultCheckInterval);
	// StartDelay delays its first check (default DefaultStartDelay).
	CheckInterval time.Duration
	StartDelay    time.Duration
}

// Service computes readiness reports and runs the RPO checker. It is safe for
// concurrent use.
type Service struct {
	cfg    Config
	logger *slog.Logger

	// checkMu makes checks single-flight.
	checkMu sync.Mutex
	// kick asks the background loop for an early check.
	kick chan struct{}

	lifeMu  sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// New returns a Service. It panics without a Store, which is a wiring bug.
func New(cfg Config) *Service {
	if cfg.Store == nil {
		panic("readiness: Store is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = DefaultCheckInterval
	}
	if cfg.StartDelay <= 0 {
		cfg.StartDelay = DefaultStartDelay
	}
	return &Service{cfg: cfg, logger: cfg.Logger, kick: make(chan struct{}, 1)}
}

// now returns the current time in UTC.
func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// Start runs the background checker until Stop or ctx ends. It is a no-op when
// already started.
func (s *Service) Start(ctx context.Context) {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.started {
		return
	}
	s.started = true
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(loopCtx)
	}()
}

// Stop cancels the background checker and waits for it. It is safe to call more
// than once and before Start.
func (s *Service) Stop() {
	s.lifeMu.Lock()
	cancel := s.cancel
	s.lifeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// loop checks after StartDelay, then every CheckInterval and whenever kicked.
func (s *Service) loop(ctx context.Context) {
	timer := time.NewTimer(s.cfg.StartDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-s.kick:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		if err := s.Check(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("the RPO check is incomplete", logsafe.Error(err))
		}
		timer.Reset(s.cfg.CheckInterval)
	}
}

// HandleEvent is an events.Handler: a finished backup of a job asks the checker for
// an early check, so a breach heals (and the gauges follow) without waiting for the
// next interval. It never blocks.
func (s *Service) HandleEvent(_ context.Context, e events.Event) {
	switch e.Type {
	case events.BackupSucceeded, events.BackupFailed:
		if e.JobID != "" {
			s.Kick()
		}
	}
}

// Kick asks the background checker for an early check, coalescing requests; it
// never blocks. Job writes (an edit of the RPO or the schedule, a pause, a resume,
// a deletion) call it, so their effect shows within moments.
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// point is the recovery point of one database of one job.
type point struct {
	job      *models.Job
	database string
	// last is the newest completed backup of the database by the job, or nil.
	last *models.BackupRecord
	// since is when last finished, or without one when the database joined the job
	// (see joinedAt).
	since     time.Time
	target    time.Duration
	isDefault bool
}

// age returns how old the recovery point is at now.
func (p point) age(now time.Time) time.Duration { return max(now.Sub(p.since), 0) }

// met reports whether the recovery point is within the objective at now.
func (p point) met(now time.Time) bool { return p.age(now) <= p.target }

// metWith reports whether the recovery point is within the objective at now,
// counting the PITR stream of the job's connection: its durable lag is the
// database's recovery point while its window is open and its collector healthy
// (see pitrAge). A broken, failing or lagging stream leaves the job's own RPO.
func (p point) metWith(now time.Time, stream *StreamInfo) bool {
	if p.met(now) {
		return true
	}
	age, ok := pitrAge(stream)
	return ok && time.Duration(age*float64(time.Second)) <= p.target
}

// streamsByConnection lists the PITR streams by connection. A failed listing is
// logged and treated as no stream, so the job RPO applies.
func (s *Service) streamsByConnection(ctx context.Context) map[string]*StreamInfo {
	out := map[string]*StreamInfo{}
	if s.cfg.Streams == nil {
		return out
	}
	streams, err := s.cfg.Streams(ctx)
	if err != nil {
		s.logger.Warn("PITR streams are unavailable to the RPO check; the job RPOs apply", logsafe.Error(err))
		return out
	}
	for i := range streams {
		out[streams[i].ConnectionID] = &streams[i]
	}
	return out
}

// key identifies a job's database in the breach table.
type key struct{ job, database string }

// points returns the recovery points of every database of jobs. The newest
// completed backups and the database join times are read in one query each,
// whatever the number of jobs.
func (s *Service) points(ctx context.Context, jobs []*models.Job, now time.Time) ([]point, error) {
	if len(jobs) == 0 {
		return nil, nil
	}
	latest, err := s.latestBackups(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("latest backups: %w", err)
	}
	joins, err := s.cfg.Store.JobDatabaseJoins(ctx)
	if err != nil {
		return nil, fmt.Errorf("database joins: %w", err)
	}
	var out []point
	for _, j := range jobs {
		target, isDefault := scheduler.EffectiveRPO(j, now)
		for _, db := range Databases(j, latest[j.ID]) {
			p := point{job: j, database: db, last: latest[j.ID][db], target: target, isDefault: isDefault}
			if p.last != nil {
				p.since = finishedAt(p.last)
			} else {
				p.since = joinedAt(j, joins[j.ID][db])
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// joinedAt is when a database without a successful backup became part of job j:
// joined, when its known databases recorded it joining later (auto-included by a
// run), else the job's last update (a database added by editing the job) or
// creation, whichever is later.
func joinedAt(j *models.Job, joined time.Time) time.Time {
	if !joined.IsZero() {
		return joined.UTC()
	}
	at := j.CreatedAt
	if j.UpdatedAt.After(at) {
		at = j.UpdatedAt
	}
	return at.UTC()
}

// Databases returns the databases job j is expected to back up: a single-database
// job's database; for a job with several, the listed ones, else its known and named
// ones, else those it has backups of (latest, keyed by database).
func Databases(j *models.Job, latest map[string]*models.BackupRecord) []string {
	sel := j.Selection()
	if !sel.Multi() {
		if len(sel.Databases) == 0 {
			return nil
		}
		return []string{sel.Databases[0]}
	}
	names := slices.Clone(sel.Databases)
	if sel.Mode != models.SelectionList {
		names = append(names, j.KnownDatabases...)
		if j.KnownDatabases == nil {
			for db := range latest {
				names = append(names, db)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// finishedAt is when b finished (its start for records without a completion time).
func finishedAt(b *models.BackupRecord) time.Time {
	if b.CompletedAt != nil {
		return b.CompletedAt.UTC()
	}
	return b.StartedAt.UTC()
}

// enabledJobs returns the enabled jobs of list.
func enabledJobs(list []*models.Job) []*models.Job {
	out := make([]*models.Job, 0, len(list))
	for _, j := range list {
		if j.Enabled {
			out = append(out, j)
		}
	}
	return out
}

// Check evaluates the RPO of every database of every enabled job once: a new breach
// is recorded and published as job.rpo_missed, a healed one removed and published as
// job.rpo_recovered, and breaches of jobs or databases no longer checked (deleted,
// paused, no longer selected) are removed without an event. The samples go to
// Config.Observe. Checks never run concurrently.
func (s *Service) Check(ctx context.Context) error {
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	now := s.now()
	jobs, err := s.cfg.Store.ListJobs(ctx)
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	points, err := s.points(ctx, enabledJobs(jobs), now)
	if err != nil {
		return err
	}
	stored, err := s.cfg.Store.ListRPOBreaches(ctx)
	if err != nil {
		return fmt.Errorf("list rpo breaches: %w", err)
	}
	streams := s.streamsByConnection(ctx)
	breachedSince := make(map[key]time.Time, len(stored))
	for _, b := range stored {
		breachedSince[key{b.JobID, b.Database}] = b.Since
	}

	var errs []error
	seen := make(map[key]bool, len(points))
	samples := make([]Sample, 0, len(points))
	for _, p := range points {
		k := key{p.job.ID, p.database}
		seen[k] = true
		samples = append(samples, Sample{JobID: p.job.ID, Database: p.database, Since: p.since, Target: p.target})
		since, breached := breachedSince[k]
		switch met := p.metWith(now, streams[p.job.ConnectionID]); {
		case !met && !breached:
			if err := s.reportMissed(ctx, p, now); err != nil {
				errs = append(errs, err)
			}
		case met && breached:
			if err := s.reportRecovered(ctx, p, since, now); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, b := range stored {
		if k := (key{b.JobID, b.Database}); !seen[k] {
			if _, delErr := s.cfg.Store.DeleteRPOBreach(ctx, k.job, k.database); delErr != nil {
				errs = append(errs, fmt.Errorf("drop the rpo breach of job %s: %w", k.job, delErr))
			}
		}
	}
	if s.cfg.Observe != nil {
		s.cfg.Observe(now, samples)
	}
	return errors.Join(errs...)
}

// reportMissed records the new breach of p and publishes job.rpo_missed. The
// breach is recorded first, so a crash in between loses one alert instead of
// repeating it on every restart; an event the bus refused (dropped) takes the
// record back, so the next check tries again.
func (s *Service) reportMissed(ctx context.Context, p point, now time.Time) error {
	added, err := s.cfg.Store.AddRPOBreach(ctx, models.RPOBreach{JobID: p.job.ID, Database: p.database, Since: now})
	if err != nil {
		return fmt.Errorf("record the rpo breach of job %s: %w", p.job.ID, err)
	}
	if !added {
		return nil
	}
	if !s.publish(ctx, rpoEvent(events.JobRPOMissed, p, time.Time{}, now)) {
		if _, err = s.cfg.Store.DeleteRPOBreach(ctx, p.job.ID, p.database); err != nil {
			return fmt.Errorf("take back the unpublished rpo breach of job %s: %w", p.job.ID, err)
		}
		s.logger.Warn("the rpo_missed event was dropped; the next check retries",
			logsafe.Attr("job_id", p.job.ID), logsafe.Attr("database", p.database))
		return nil
	}
	s.logger.Warn("recovery point objective missed",
		logsafe.Attr("job_id", p.job.ID), logsafe.Attr("database", p.database),
		slog.Duration("age", p.age(now).Round(time.Second)), slog.Duration("rpo", p.target))
	return nil
}

// reportRecovered clears the breach of p (recorded at since) and publishes
// job.rpo_recovered; a refused event restores the breach, so the next check tries
// again.
func (s *Service) reportRecovered(ctx context.Context, p point, since, now time.Time) error {
	deleted, err := s.cfg.Store.DeleteRPOBreach(ctx, p.job.ID, p.database)
	if err != nil {
		return fmt.Errorf("clear the rpo breach of job %s: %w", p.job.ID, err)
	}
	if !deleted {
		return nil
	}
	if !s.publish(ctx, rpoEvent(events.JobRPORecovered, p, since, now)) {
		if _, err = s.cfg.Store.AddRPOBreach(ctx, models.RPOBreach{JobID: p.job.ID, Database: p.database, Since: since}); err != nil {
			return fmt.Errorf("restore the rpo breach of job %s: %w", p.job.ID, err)
		}
		s.logger.Warn("the rpo_recovered event was dropped; the next check retries",
			logsafe.Attr("job_id", p.job.ID), logsafe.Attr("database", p.database))
		return nil
	}
	s.logger.Info("recovery point objective met again",
		logsafe.Attr("job_id", p.job.ID), logsafe.Attr("database", p.database))
	return nil
}

// publish emits e and reports whether the publisher accepted it; without a
// publisher there is nobody to tell, which counts as accepted.
func (s *Service) publish(ctx context.Context, e events.Event) bool {
	if s.cfg.Publisher == nil {
		return true
	}
	return s.cfg.Publisher.Publish(ctx, e)
}

// rpoEvent builds the job.rpo_missed or job.rpo_recovered event of p at now. since
// is when the healed breach began (job.rpo_recovered).
func rpoEvent(t events.EventType, p point, since, now time.Time) events.Event {
	e := events.Event{Type: t, Time: now, JobID: p.job.ID, Database: p.database, Status: "missed"}
	age, target := FormatDuration(p.age(now)), FormatDuration(p.target)
	switch {
	case t == events.JobRPORecovered && (p.last == nil || !finishedAt(p.last).After(since)):
		// No backup since the breach began: the objective was raised (or the
		// database joined anew), not the data refreshed.
		e.Status = "recovered"
		e.Detail = fmt.Sprintf("the objective changed to %s; the newest successful backup is %s old", target, age)
		if p.last == nil {
			e.Detail = fmt.Sprintf("the objective changed to %s; no successful backup yet", target)
		}
	case t == events.JobRPORecovered:
		e.Status = "recovered"
		e.Detail = fmt.Sprintf("the newest successful backup is %s old; objective %s", age, target)
	case p.last == nil:
		e.Detail = fmt.Sprintf("no successful backup since the database joined the job %s ago; objective %s", age, target)
	default:
		e.Detail = fmt.Sprintf("no successful backup for %s; objective %s", age, target)
	}
	if p.last != nil {
		e.BackupID = p.last.ID
	}
	return e
}

// FormatDuration renders d compactly in days, hours and minutes ("2d 3h", "7h 12m",
// "45m"), rounded down to the minute.
func FormatDuration(d time.Duration) string {
	d = d.Truncate(time.Minute)
	days := d / (24 * time.Hour)
	hours := (d % (24 * time.Hour)) / time.Hour
	minutes := (d % time.Hour) / time.Minute
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 && days == 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if len(parts) == 0 {
		return "0m"
	}
	return strings.Join(parts, " ")
}

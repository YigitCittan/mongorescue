package copies

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// Defaults of Config.
const (
	// DefaultCheckInterval is how often the queue looks for due copies.
	DefaultCheckInterval = time.Minute
	// DefaultStartDelay delays the first look after start-up.
	DefaultStartDelay = 30 * time.Second
	// DefaultMaxAttempts is how many times the queue tries a copy before it gives
	// up (a later sweep or a new backup does not retry it; retrying the backup
	// does).
	DefaultMaxAttempts = 10
	// DefaultBaseBackoff is the wait after the first failed attempt; it doubles
	// with every further attempt up to DefaultMaxBackoff.
	DefaultBaseBackoff = time.Minute
	// DefaultMaxBackoff caps the wait between attempts.
	DefaultMaxBackoff = 6 * time.Hour
	// SyncAttempts is how many times a synchronous copy is tried before the
	// backup fails.
	SyncAttempts = 3
)

// Store is the persistence port of the queue (implemented by *store.SQLiteStore).
type Store interface {
	// PendingCopyRecords returns the completed backups with a pending or failed
	// copy.
	PendingCopyRecords(ctx context.Context) ([]*models.BackupRecord, error)
	// UpdateBackupRecord applies fn to the stored record id in one transaction.
	UpdateBackupRecord(ctx context.Context, id string, fn func(*models.BackupRecord) error) (*models.BackupRecord, error)
}

// Config holds the dependencies of a Service. Store and Storages are required.
type Config struct {
	// Store reads the queue and records the outcome of every attempt.
	Store Store
	// Storages returns the driver of a storage target.
	Storages StorageFunc
	// UploadMbps returns the upload cap of a backup's copies in megabits per
	// second (its job's max_upload_mbps, else general.max_upload_mbps; 0 =
	// unlimited); nil means unlimited.
	UploadMbps func(ctx context.Context, rec *models.BackupRecord) float64
	// Publisher receives backup.copy_failed and backup.copy_recovered.
	Publisher events.Publisher
	// Observe receives the result of every attempt (metrics.CopyOK,
	// metrics.CopyMismatch or metrics.CopyError).
	Observe func(result string)
	// Logger receives operational logs; nil means slog.Default().
	Logger *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// CheckInterval, StartDelay, MaxAttempts, BaseBackoff and MaxBackoff tune the
	// queue (zero values take the defaults above).
	CheckInterval time.Duration
	StartDelay    time.Duration
	MaxAttempts   int
	BaseBackoff   time.Duration
	MaxBackoff    time.Duration
	// SyncBackoff is the wait between the attempts of a synchronous copy (default
	// two seconds, doubled per attempt).
	SyncBackoff time.Duration
}

// Results of an attempt, as passed to Config.Observe (equal to the metrics
// package's labels).
const (
	resultOK       = "ok"
	resultMismatch = "mismatch"
	resultError    = "error"
)

// Service copies backups to their copy targets: synchronously for CopySync backups
// (CopyAll, called by the backup engine) and through the persistent copy queue for
// CopyAsync ones. The queue is the backup records themselves: every pending copy,
// and every failed copy with a next attempt, is due once its NextAttemptAt has
// passed, so it survives restarts. Copies run outside backup windows too: they read
// storage, not MongoDB. Service is safe for concurrent use.
type Service struct {
	cfg    Config
	logger *slog.Logger

	// runMu makes RunDue single-flight.
	runMu sync.Mutex
	depth atomic.Int64
	wake  chan struct{}

	lifeMu  sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// New returns a copy service for cfg.
func New(cfg Config) *Service {
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = DefaultCheckInterval
	}
	if cfg.StartDelay <= 0 {
		cfg.StartDelay = DefaultStartDelay
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = DefaultBaseBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	if cfg.SyncBackoff <= 0 {
		cfg.SyncBackoff = 2 * time.Second
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{cfg: cfg, logger: logger, wake: make(chan struct{}, 1)}
}

// now returns the service's clock.
func (s *Service) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// Start runs the queue in the background until Stop or ctx ends. It is a no-op when
// already started.
func (s *Service) Start(ctx context.Context) {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.started {
		return
	}
	s.started = true
	var loopCtx context.Context
	loopCtx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(loopCtx)
	}()
}

// Stop cancels the queue and a running copy and waits for them. It is safe to call
// more than once and before Start.
func (s *Service) Stop() {
	s.lifeMu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.lifeMu.Unlock()
	s.wg.Wait()
}

// Notify wakes the queue (a backup with copies completed). It never blocks.
func (s *Service) Notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// HandleEvent wakes the queue for every succeeded backup (an events.Handler).
func (s *Service) HandleEvent(_ context.Context, e events.Event) {
	if e.Type == events.BackupSucceeded {
		s.Notify()
	}
}

// QueueDepth returns the number of copies the last look found waiting.
func (s *Service) QueueDepth() int { return int(s.depth.Load()) }

// loop runs RunDue after StartDelay, then every CheckInterval and on Notify.
func (s *Service) loop(ctx context.Context) {
	timer := time.NewTimer(s.cfg.StartDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-s.wake:
		}
		if err := s.RunDue(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("copy queue run incomplete", logsafe.Error(err))
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(s.cfg.CheckInterval)
	}
}

// due reports whether copy c of a completed backup is waiting and due at now.
func due(c *models.BackupCopy, now time.Time) bool {
	switch c.Status {
	case models.CopyPending:
		return c.NextAttemptAt == nil || !now.Before(*c.NextAttemptAt)
	case models.CopyFailed:
		return c.NextAttemptAt != nil && !now.Before(*c.NextAttemptAt)
	default:
		return false
	}
}

// waiting reports whether copy c is in the queue (due now or later).
func waiting(c *models.BackupCopy) bool {
	return c.Status == models.CopyPending || (c.Status == models.CopyFailed && c.NextAttemptAt != nil)
}

// RunDue tries every due copy of the queue once, oldest backup first. The
// background loop calls it; tests call it directly.
func (s *Service) RunDue(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	list, err := s.cfg.Store.PendingCopyRecords(ctx)
	if err != nil {
		return fmt.Errorf("list the copy queue: %w", err)
	}
	var errs []error
	for _, rec := range list {
		if rec.Verification == models.VerificationMismatch {
			// A damaged primary is never copied; the integrity sweep reports it.
			continue
		}
		for i := range rec.Copies {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !due(&rec.Copies[i], s.now()) {
				continue
			}
			if err := s.attempt(ctx, rec, rec.Copies[i].TargetID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	s.updateDepth(ctx)
	return errors.Join(errs...)
}

// updateDepth recounts the copies waiting in the queue.
func (s *Service) updateDepth(ctx context.Context) {
	list, err := s.cfg.Store.PendingCopyRecords(ctx)
	if err != nil {
		return
	}
	n := 0
	for _, rec := range list {
		for i := range rec.Copies {
			if waiting(&rec.Copies[i]) {
				n++
			}
		}
	}
	s.depth.Store(int64(n))
}

// backoff returns the wait after the attempts-th failed attempt.
func (s *Service) backoff(attempts int) time.Duration {
	d := s.cfg.BaseBackoff
	for i := 1; i < attempts && d < s.cfg.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, s.cfg.MaxBackoff)
}

// mbps returns the upload cap of rec's copies.
func (s *Service) mbps(ctx context.Context, rec *models.BackupRecord) float64 {
	if s.cfg.UploadMbps == nil {
		return 0
	}
	return s.cfg.UploadMbps(ctx, rec)
}

// copyOne copies rec to copy target targetID once and returns the stored object.
func (s *Service) copyOne(ctx context.Context, rec *models.BackupRecord, cp *models.BackupCopy, mbps float64) (*models.StorageObject, error) {
	src, err := s.cfg.Storages(ctx, rec.StorageTargetID)
	if err != nil {
		return nil, fmt.Errorf("open the primary storage target: %w", err)
	}
	dst, err := s.cfg.Storages(ctx, cp.TargetID)
	if err != nil {
		return nil, fmt.Errorf("open the copy target: %w", err)
	}
	key := cp.StorageKey
	if key == "" {
		key = rec.StorageKey
	}
	obj, err := Copy(ctx, src, rec, dst, key, models.UploadBytesPerSecond(mbps))
	s.observe(err)
	return obj, err
}

// observe reports the result of an attempt.
func (s *Service) observe(err error) {
	if s.cfg.Observe == nil {
		return
	}
	switch {
	case err == nil:
		s.cfg.Observe(resultOK)
	case errors.Is(err, ErrChecksumMismatch):
		s.cfg.Observe(resultMismatch)
	default:
		s.cfg.Observe(resultError)
	}
}

// succeed records the stored object obj on cp.
func succeed(cp *models.BackupCopy, obj *models.StorageObject, at time.Time) {
	cp.Status, cp.SHA256OK, cp.Error, cp.NextAttemptAt = models.CopyDone, true, "", nil
	cp.CopiedAt = &at
	cp.SetStorageObject(obj)
}

// attempt tries queued copy targetID of rec once and records the outcome.
func (s *Service) attempt(ctx context.Context, rec *models.BackupRecord, targetID string) error {
	cp := rec.Copy(targetID)
	obj, copyErr := s.copyOne(ctx, rec, cp, s.mbps(ctx, rec))
	if ctx.Err() != nil {
		// Stopped: the copy stays pending and is tried again after the restart.
		return ctx.Err()
	}
	at := s.now().UTC()
	var prev models.CopyStatus
	var updated models.BackupCopy
	_, err := s.cfg.Store.UpdateBackupRecord(ctx, rec.ID, func(r *models.BackupRecord) error {
		c := r.Copy(targetID)
		if c == nil || !waiting(c) {
			return errSkip
		}
		prev = c.Status
		c.Attempts++
		if copyErr == nil {
			succeed(c, obj, at)
		} else {
			c.Status, c.SHA256OK = models.CopyFailed, false
			c.Error = redact.Text(copyErr.Error())
			c.NextAttemptAt = nil
			if c.Attempts < s.cfg.MaxAttempts {
				next := at.Add(s.backoff(c.Attempts))
				c.NextAttemptAt = &next
			}
		}
		updated = *c
		return nil
	})
	switch {
	case errors.Is(err, errSkip):
		return nil
	case err != nil:
		return fmt.Errorf("record the copy of %s to %s: %w", rec.ID, targetID, err)
	}
	s.report(ctx, rec, &updated, prev, copyErr)
	if copyErr != nil {
		return fmt.Errorf("copy %s to %s: %w", rec.ID, targetID, copyErr)
	}
	return nil
}

// errSkip aborts the update of a copy that left the queue meanwhile.
var errSkip = errors.New("copies: the copy left the queue")

// report logs an attempt and publishes backup.copy_failed (when a copy starts
// failing) and backup.copy_recovered (when a failing copy succeeds).
func (s *Service) report(ctx context.Context, rec *models.BackupRecord, cp *models.BackupCopy, prev models.CopyStatus, copyErr error) {
	attrs := []any{logsafe.Attr("backup_id", rec.ID), logsafe.Attr("storage_target_id", cp.TargetID), slog.Int("attempts", cp.Attempts)}
	var typ events.EventType
	if copyErr == nil {
		s.logger.Info("backup copied", attrs...)
		if prev == models.CopyFailed {
			typ = events.BackupCopyRecovered
		}
	} else {
		s.logger.Warn("backup copy failed", append(attrs, slog.String("error", cp.Error))...)
		if prev != models.CopyFailed {
			typ = events.BackupCopyFailed
		}
	}
	if typ == "" || s.cfg.Publisher == nil {
		return
	}
	s.cfg.Publisher.Publish(ctx, CopyEvent(typ, rec, cp, s.now()))
}

// CopyEvent returns event typ about copy cp of backup rec.
func CopyEvent(typ events.EventType, rec *models.BackupRecord, cp *models.BackupCopy, at time.Time) events.Event {
	e := events.Event{Type: typ, Time: at.UTC(), JobID: rec.JobID, BackupID: rec.ID, Database: rec.Database,
		Status: string(cp.Status), TargetID: cp.TargetID, TargetName: cp.TargetName, RunID: rec.RunID}
	if typ == events.BackupCopyFailed {
		e.Error = cp.Error
		if cp.NextAttemptAt != nil {
			e.Detail = "retried at " + cp.NextAttemptAt.UTC().Format(time.RFC3339)
		} else {
			e.Detail = "no further automatic attempts"
		}
	}
	return e
}

// CopyAll copies record to every pending copy target synchronously (CopySync),
// trying each SyncAttempts times, and records the outcome on record.Copies. It
// returns an error naming every copy that failed. mbps caps the upload (0 =
// unlimited). It implements backup.CopyFunc.
func (s *Service) CopyAll(ctx context.Context, record *models.BackupRecord, mbps float64) error {
	var errs []error
	for i := range record.Copies {
		cp := &record.Copies[i]
		if cp.Status != models.CopyPending {
			continue
		}
		var lastErr error
		for attempt := 1; attempt <= SyncAttempts; attempt++ {
			cp.Attempts++
			obj, err := s.copyOne(ctx, record, cp, mbps)
			if err == nil {
				succeed(cp, obj, s.now().UTC())
				lastErr = nil
				break
			}
			lastErr = err
			if errors.Is(err, ErrChecksumMismatch) || ctx.Err() != nil || attempt == SyncAttempts {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(s.cfg.SyncBackoff << (attempt - 1)):
			}
		}
		if lastErr != nil {
			cp.Status, cp.SHA256OK, cp.NextAttemptAt = models.CopyFailed, false, nil
			cp.Error = redact.Text(lastErr.Error())
			name := cp.TargetName
			if name == "" {
				name = cp.TargetID
			}
			errs = append(errs, fmt.Errorf("copy to storage target %s: %w", name, lastErr))
		}
	}
	return errors.Join(errs...)
}

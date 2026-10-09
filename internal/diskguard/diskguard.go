// Package diskguard protects runs against a full data directory: the filesystem of
// the metadata database (mongorescue.db) and of the run logs.
//
// A Guard refuses new backups and restores (see Admit) while the data directory
// has less free space than its minimum, or after a metadata write failed because
// the disk is full, until space is available again. A write that fails that way
// publishes one critical events.SystemDiskFull event per episode and raises a
// settings warning. The final record of a run that could not be saved is retried
// once writes work again (see Defer), and at the latest settled by the startup
// recovery of interrupted runs.
package diskguard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yigitcittan/mongorescue/internal/diskspace"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// Defaults.
const (
	// DefaultMinFree is the free space the data directory needs for a run to start.
	DefaultMinFree = 100 << 20
	// DefaultPollInterval is how often a full data directory is checked for space.
	DefaultPollInterval = 30 * time.Second
	// recoverMinFree is the free space that ends a disk-full episode when the
	// pre-run check is off (MinFree 0).
	recoverMinFree = 1 << 20
	// maxDeferred bounds the saves waiting for space.
	maxDeferred = 1000
	// sqliteFull is SQLITE_FULL, the primary result code of "database or disk is
	// full" (extended codes keep it in the low byte).
	sqliteFull = 13
)

// saveRetryDelays are the waits between the attempts of SaveFinal.
var saveRetryDelays = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}

// ErrLowSpace is returned by Check and Admit while the data directory has not
// enough free space for a run, or after a metadata write failed because the disk
// was full.
var ErrLowSpace = errors.New("not enough free space in the data directory")

// ErrNotSaved is returned by FinishBackup when the final record of a backup could
// not be saved; the backup is then reported as failed.
var ErrNotSaved = errors.New("the final record could not be saved")

// IsFull reports whether err is a write that failed because the disk is full:
// SQLITE_FULL or ENOSPC.
func IsFull(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) {
		return true
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) && coded.Code()&0xff == sqliteFull {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database or disk is full") || strings.Contains(msg, "no space left on device")
}

// Config configures a Guard.
type Config struct {
	// Dir is the data directory.
	Dir string
	// MinFree is the free space (bytes) Dir needs for a run to start; 0 turns the
	// pre-run check off (a failed write still stops new runs).
	MinFree uint64
	// Free returns the free bytes of the filesystem holding a path
	// (diskspace.Free when nil).
	Free func(path string) (uint64, error)
	// Publisher receives the events.SystemDiskFull events (none when nil).
	Publisher events.Publisher
	// Warn shows (active) or hides the settings warning about a full data
	// directory (none when nil).
	Warn func(active bool)
	// PollInterval is how often Run checks a full data directory for space
	// (DefaultPollInterval when <= 0).
	PollInterval time.Duration
	// SaveRetryDelays are the waits between the attempts of SaveFinal (250ms,
	// 500ms, 1s and 2s when nil; empty for a single attempt).
	SaveRetryDelays []time.Duration
	// Logger receives the guard's messages (slog.Default() when nil).
	Logger *slog.Logger
}

// deferredSave is a save waiting to be retried at next, with its current backoff.
type deferredSave struct {
	what    string
	save    func(ctx context.Context) error
	next    time.Time
	backoff time.Duration
}

// Backoff of the deferred saves.
const (
	deferredBackoff    = time.Second
	deferredMaxBackoff = time.Minute
)

// Guard tracks the space of the data directory. A nil Guard admits every run and
// only retries saves (see SaveFinal). It is safe for concurrent use.
type Guard struct {
	cfg  Config
	wake chan struct{}

	mu       sync.Mutex
	full     bool
	fullErr  string
	deferred []deferredSave
}

// New returns a Guard. Run its Run method to retry deferred saves and end a
// disk-full episode without waiting for the next run.
func New(cfg Config) *Guard {
	if cfg.Free == nil {
		cfg.Free = diskspace.Free
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Guard{cfg: cfg, wake: make(chan struct{}, 1)}
}

// Full reports whether a metadata write failed because the disk was full and
// space has not been available since.
func (g *Guard) Full() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.full
}

// Admit is the admission check of runs.Manager (see runs.Manager.SetAdmission):
// it is Check.
func (g *Guard) Admit() error {
	return g.Check()
}

// Check reports whether a run may start: it returns an error wrapping ErrLowSpace
// while the data directory has less free space than the minimum, or after a
// metadata write failed because the disk was full until space is available again
// (which ends the episode). A data directory whose free space cannot be read is
// accepted unless a write failed.
func (g *Guard) Check() error {
	if g == nil {
		return nil
	}
	free, probeErr := g.cfg.Free(g.cfg.Dir)
	known := probeErr == nil
	if known && g.Full() && free >= max(g.cfg.MinFree, recoverMinFree) {
		g.recovered(free)
	}
	if g.Full() {
		return fmt.Errorf("%w: a write to the metadata database failed because the disk is full (%s); new backups and restores start again once space is freed",
			ErrLowSpace, g.cfg.Dir)
	}
	if known && g.cfg.MinFree > 0 && free < g.cfg.MinFree {
		return fmt.Errorf("%w: %s free in %s, below the minimum of %s; free space (or lower MONGORESCUE_MIN_FREE_SPACE_MB) to start backups and restores",
			ErrLowSpace, mib(free), g.cfg.Dir, mib(g.cfg.MinFree))
	}
	return nil
}

// Observe reports err, the error of a metadata write: a write that failed because
// the disk is full (IsFull) starts a disk-full episode, which publishes one
// events.SystemDiskFull event, raises the settings warning and stops new runs
// until space is available. It reports whether err was such a failure.
func (g *Guard) Observe(ctx context.Context, err error) bool {
	if g == nil || !IsFull(err) {
		return false
	}
	g.mu.Lock()
	started := !g.full
	g.full = true
	g.fullErr = redact.Text(err.Error())
	g.mu.Unlock()
	if !started {
		return true
	}
	g.cfg.Logger.Error("the data directory is full: metadata writes fail; new backups and restores are refused until space is freed",
		slog.String("data_dir", g.cfg.Dir), logsafe.Error(err))
	if g.cfg.Warn != nil {
		g.cfg.Warn(true)
	}
	if g.cfg.Publisher != nil {
		g.cfg.Publisher.Publish(ctx, events.Event{
			Type: events.SystemDiskFull, Time: time.Now().UTC(), Status: "full",
			Error:  redact.Text(err.Error()),
			Detail: "the metadata database in " + g.cfg.Dir + " cannot be written; new backups and restores are refused until space is freed",
		})
	}
	g.signal()
	return true
}

// recovered ends a disk-full episode.
func (g *Guard) recovered(free uint64) {
	g.mu.Lock()
	was := g.full
	g.full, g.fullErr = false, ""
	if was {
		// The saves that waited for space are due now.
		now := time.Now()
		for i := range g.deferred {
			g.deferred[i].next = now
		}
	}
	g.mu.Unlock()
	if !was {
		return
	}
	g.cfg.Logger.Info("the data directory has free space again; backups and restores start again",
		slog.String("data_dir", g.cfg.Dir), slog.String("free", mib(free)))
	if g.cfg.Warn != nil {
		g.cfg.Warn(false)
	}
	g.signal()
}

// Defer queues save, which failed, to be retried by Run until it succeeds; what
// names it in the log. The queue is bounded: the oldest save is dropped (and
// logged) when it is full, leaving its record to the startup recovery.
func (g *Guard) Defer(what string, save func(ctx context.Context) error) {
	if g == nil || save == nil {
		return
	}
	g.mu.Lock()
	if len(g.deferred) >= maxDeferred {
		g.cfg.Logger.Warn("too many saves wait for space; the oldest is left to the startup recovery",
			slog.String("dropped", g.deferred[0].what))
		g.deferred = g.deferred[1:]
	}
	g.deferred = append(g.deferred, deferredSave{what: what, save: save, next: time.Now().Add(deferredBackoff), backoff: deferredBackoff})
	g.mu.Unlock()
	g.signal()
}

// Pending returns the number of deferred saves.
func (g *Guard) Pending() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.deferred)
}

// signal wakes Run.
func (g *Guard) signal() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// Run checks a full data directory for space every poll interval (ending the
// episode once there is enough) and retries the deferred saves as they fall due,
// until ctx ends.
func (g *Guard) Run(ctx context.Context) {
	if g == nil {
		return
	}
	timer := time.NewTimer(g.cfg.PollInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-g.wake:
		}
		if g.Full() {
			// Check ends the episode once space is available.
			_ = g.Check()
		}
		g.retryDeferred(ctx, false)
		wait := g.cfg.PollInterval
		if next, ok := g.nextDeferred(); ok {
			wait = max(min(wait, time.Until(next)), time.Millisecond)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
	}
}

// nextDeferred returns when the next deferred save is due.
func (g *Guard) nextDeferred() (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var next time.Time
	for _, d := range g.deferred {
		if next.IsZero() || d.next.Before(next) {
			next = d.next
		}
	}
	return next, !next.IsZero()
}

// RetryDeferred retries every deferred save once, due or not (Run retries them as
// they fall due).
func (g *Guard) RetryDeferred(ctx context.Context) {
	if g == nil {
		return
	}
	g.retryDeferred(ctx, true)
}

// retryDeferred runs the due (or, with all, every) deferred saves in order and
// keeps those that fail, with their backoff doubled; while the disk is full it
// leaves them for later.
func (g *Guard) retryDeferred(ctx context.Context, all bool) {
	g.mu.Lock()
	list := g.deferred
	g.deferred = nil
	g.mu.Unlock()
	var keep []deferredSave
	now := time.Now()
	for i, d := range list {
		if ctx.Err() != nil || g.Full() {
			keep = append(keep, list[i:]...)
			break
		}
		if !all && d.next.After(now) {
			keep = append(keep, d)
			continue
		}
		if err := d.save(ctx); err != nil {
			g.Observe(ctx, err)
			d.backoff = min(2*d.backoff, deferredMaxBackoff)
			d.next = time.Now().Add(d.backoff)
			keep = append(keep, d)
			continue
		}
		g.cfg.Logger.Info("saved a record that could not be saved before", slog.String("record", d.what))
	}
	if len(keep) == 0 {
		return
	}
	g.mu.Lock()
	g.deferred = append(keep, g.deferred...)
	g.mu.Unlock()
}

// SaveFinal saves the final record of a run with save, retrying a few times with
// backoff within ctx (the disk may free up). Every failure is reported to Observe.
// It returns the last error.
func (g *Guard) SaveFinal(ctx context.Context, save func(context.Context) error) error {
	delays := saveRetryDelays
	if g != nil && g.cfg.SaveRetryDelays != nil {
		delays = g.cfg.SaveRetryDelays
	}
	var err error
	for attempt := 0; ; attempt++ {
		if err = save(ctx); err == nil {
			return nil
		}
		g.Observe(ctx, err)
		if attempt >= len(delays) || ctx.Err() != nil {
			return err
		}
		timer := time.NewTimer(delays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

// FinishBackup saves rec, the final record of a backup run, with save (see
// SaveFinal). When it cannot be saved because the disk is full (IsFull), the run
// must not report success: rec is marked failed (models.BackupRecord.FailUnsaved,
// so its archive goes to the purge), the failure is logged to logger and to run's
// log, and saving rec is deferred until writes work again; it then returns an
// error wrapping ErrNotSaved and the caller publishes the event of rec as it is
// now (failed). Any other failure (a busy or locked database, a timeout) is
// transient: rec is kept as it is, its save is retried in the background with
// backoff until it succeeds (see Defer), and FinishBackup returns nil, so a good
// archive is never deleted over a lock.
func (g *Guard) FinishBackup(ctx context.Context, rec *models.BackupRecord, save func(context.Context, *models.BackupRecord) error,
	logger *slog.Logger, run *runs.Run) error {
	if rec == nil {
		return nil
	}
	err := g.SaveFinal(ctx, func(ctx context.Context) error { return save(ctx, rec) })
	if err == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if !IsFull(err) {
		logger.Warn("the final record of a backup could not be saved yet; it is saved in the background",
			logsafe.Attr("backup_id", rec.ID), slog.String("status", string(rec.Status)), logsafe.Error(err))
		run.Printf("WARNING: the backup record could not be saved yet (%s); it is retried in the background", redact.Text(err.Error()))
		g.Defer("backup "+rec.ID, saveCopy(rec, save))
		return nil
	}
	outcome := rec.Status
	rec.FailUnsaved(redact.Text(err.Error()), time.Now())
	logger.Error("the final record of a backup could not be saved; the backup is reported as failed",
		logsafe.Attr("backup_id", rec.ID), slog.String("outcome", string(outcome)),
		slog.Bool("archive_cleanup_pending", rec.ArchiveCleanupPending), logsafe.Error(err))
	run.Printf("ERROR: the backup record could not be saved (%s); the backup is reported as failed and its archive is deleted by the next purge",
		redact.Text(err.Error()))
	g.Defer("backup "+rec.ID, saveCopy(rec, save))
	return fmt.Errorf("%w: backup %s: %w", ErrNotSaved, rec.ID, err)
}

// saveCopy returns a save of a copy of rec as it is now, which later changes of
// rec do not affect.
func saveCopy(rec *models.BackupRecord, save func(context.Context, *models.BackupRecord) error) func(context.Context) error {
	c := *rec
	c.Copies = append([]models.BackupCopy(nil), rec.Copies...)
	return func(ctx context.Context) error { return save(ctx, &c) }
}

// mib formats bytes in MiB.
func mib(b uint64) string {
	return fmt.Sprintf("%d MiB", b>>20)
}

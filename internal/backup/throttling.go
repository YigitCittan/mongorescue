package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// ErrNoEligibleMember indicates that a backup with the read preference "secondary"
// found no secondary to read from (a standalone server, a replica set without a
// reachable secondary, or a directConnection=true URI that reaches the primary).
var ErrNoEligibleMember = errors.New("backup: no member matches the read preference")

// ErrInterrupted indicates that a backup ended before mongodump started because
// MongoRescue shut down (or the run's context ended) while it waited for a slot
// of its connection. Such a backup is recorded as cancelled by the system, never
// as failed.
var ErrInterrupted = errors.New("backup: interrupted before it started")

// interruptedReason is the cancellation reason of a backup that waited for a slot
// when MongoRescue shut down.
const interruptedReason = "interrupted: MongoRescue shut down while the backup waited for a slot of its connection"

// memberTimeout bounds the hello query that finds the member a backup reads from.
const memberTimeout = 45 * time.Second

// Slots limits concurrent backups per key (see runs.Manager.AcquireSlot): it
// returns once fewer than limit holders share key, calling waiting first when the
// caller has to wait, and fails with ctx's error when ctx ends while waiting.
type Slots interface {
	AcquireSlot(ctx context.Context, key string, limit int, waiting func()) (release func(), err error)
}

// WithConnectionSlots makes every backup with BackupOptions.MaxConcurrentBackups
// wait for a slot of its connection before mongodump starts.
func WithConnectionSlots(s Slots) Option {
	return func(e *Engine) { e.slots = s }
}

// MemberFunc returns the member a read with the connection string's read
// preference selects (hello on that member). It must not quote the URI in errors.
type MemberFunc func(ctx context.Context, uri string) (models.SourceMember, error)

// WithMemberProbe records on every backup the member that served it, and lets a
// backup with the read preference "secondary" fail with ErrNoEligibleMember
// before mongodump starts when no secondary is available.
func WithMemberProbe(fn MemberFunc) Option {
	return func(e *Engine) { e.member = fn }
}

// waitForSlot takes a slot of the backup's connection when it has a limit,
// showing the run as models.PhaseWaiting while it waits. The returned release is
// never nil.
func (e *Engine) waitForSlot(ctx context.Context, opts models.BackupOptions, record *models.BackupRecord) (func(), error) {
	if e.slots == nil || opts.MaxConcurrentBackups <= 0 || opts.ConnectionID == "" {
		return func() {}, nil
	}
	tracker := runs.FromContext(ctx)
	waiting := func() {
		tracker.Phase(models.PhaseWaiting, record.Phases)
		tracker.Printf("waiting: connection %s already runs %d backup(s), its max_concurrent_backups",
			opts.ConnectionName, opts.MaxConcurrentBackups)
		e.logger.Info("backup waits for a slot of its connection",
			logsafe.Attr("backup_id", record.ID), slog.String("connection_id", opts.ConnectionID),
			slog.Int("max_concurrent_backups", opts.MaxConcurrentBackups))
	}
	release, err := e.slots.AcquireSlot(ctx, runs.ConnectionKey(opts.ConnectionID), opts.MaxConcurrentBackups, waiting)
	switch {
	case err == nil:
		return release, nil
	case runs.CancellationOf(ctx) != nil:
		return nil, fmt.Errorf("backup %w while waiting for a slot of its connection", runs.CancellationOf(ctx))
	default:
		// The runs manager shut down, or the run's context ended (the scheduler
		// stopping): the backup never started, so it was interrupted, not failed.
		return nil, fmt.Errorf("%w: %w", ErrInterrupted, err)
	}
}

// interrupt records a backup that ended before mongodump started because
// MongoRescue shut down as cancelled by the system, so it emits backup.cancelled
// and no heartbeat failure.
func (e *Engine) interrupt(ctx context.Context, record *models.BackupRecord, err error) (*models.BackupRecord, error) {
	record, err = e.fail(ctx, record, err)
	record.Status = models.StatusCancelled
	record.CancelledBy, record.CancelledAt = runs.SystemActor, models.Stamp(time.Now())
	record.ErrorMessage = interruptedReason
	runs.FromContext(ctx).Printf("backup cancelled: %s", interruptedReason)
	e.logger.Info("backup interrupted while waiting for a slot of its connection", logsafe.Attr("backup_id", record.ID))
	return record, err
}

// probeMember asks which member a read with opts' read preference selects and
// records it on record. Only the read preference "secondary" makes a failure or a
// member that is not a secondary fatal (ErrNoEligibleMember); otherwise the
// backup goes on without a recorded member, and mongodump selects as it would.
func (e *Engine) probeMember(ctx context.Context, uri string, opts models.BackupOptions, record *models.BackupRecord) error {
	rp := opts.ReadPreference
	record.ReadPreference = rp.String()
	if e.member == nil {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, memberTimeout)
	defer cancel()
	member, err := e.member(pctx, uri)
	strict := rp.Mode == models.ReadSecondary
	tracker := runs.FromContext(ctx)
	if err != nil {
		msg := redact.Text(err.Error())
		if strict && ctx.Err() == nil {
			return fmt.Errorf("%w: read preference %s: %s", ErrNoEligibleMember, rp, msg)
		}
		e.logger.Warn("could not determine the member the backup reads from",
			logsafe.Attr("backup_id", record.ID), slog.String("error", msg))
		return nil
	}
	if strict && member.State != models.MemberSecondary && member.State != models.MemberMongos {
		return fmt.Errorf("%w: read preference %s needs a replica set secondary, but the connection reaches only %s"+
			" (a single-member replica set, a standalone server or a directConnection=true URI)", ErrNoEligibleMember, rp, member)
	}
	record.SourceMember = &member
	if rp.Mode != "" {
		tracker.Printf("reading from %s (read preference %s)", member, rp)
	} else {
		tracker.Printf("reading from %s", member)
	}
	return nil
}

// uploadMbps returns the upload cap of a run with opts: the job's, else the
// engine's default.
func (e *Engine) uploadMbps(opts models.BackupOptions) float64 {
	if opts.MaxUploadMbps > 0 {
		return opts.MaxUploadMbps
	}
	return e.maxUploadMbps
}

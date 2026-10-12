package ha

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// Bounds of the lease timing.
const (
	// MinTTL and MaxTTL bound the lease TTL.
	MinTTL = 5 * time.Second
	MaxTTL = 10 * time.Minute
	// MinRenew is the shortest renew interval.
	MinRenew = time.Second
	// DefaultTTL and DefaultRenew are the defaults of the configuration.
	DefaultTTL   = 30 * time.Second
	DefaultRenew = 10 * time.Second
)

// ValidateTiming checks a lease TTL and renew interval: the TTL within [MinTTL,
// MaxTTL] and the renew interval at least MinRenew and below TTL/2, so the active
// instance gets at least one more attempt before it must step down.
func ValidateTiming(ttl, renew time.Duration) error {
	switch {
	case ttl < MinTTL || ttl > MaxTTL:
		return fmt.Errorf("%w: the lease TTL must be %s to %s", ErrInvalidConfig, MinTTL, MaxTTL)
	case renew < MinRenew:
		return fmt.Errorf("%w: the renew interval must be at least %s", ErrInvalidConfig, MinRenew)
	case 2*renew >= ttl:
		return fmt.Errorf("%w: the renew interval (%s) must be below half the lease TTL (%s)", ErrInvalidConfig, renew, ttl)
	}
	return nil
}

// Config holds the dependencies and settings of a Manager. Store, Cluster,
// InstanceID, TTL and Renew are required.
type Config struct {
	// Store holds the lease (storage.LeaseStore of the HA target).
	Store storage.LeaseStore
	// Cluster is the cluster name (see ValidateCluster).
	Cluster string
	// InstanceID identifies this instance; InstanceName labels it for people.
	InstanceID   string
	InstanceName string
	// KeyID identifies this instance's secret key; a lease with another KeyID is
	// never taken over (Status.KeyMismatch).
	KeyID string
	// TTL is how long a lease lives without renewal; Renew how often the active
	// instance renews it and a standby reads it (see ValidateTiming).
	TTL   time.Duration
	Renew time.Duration
	// Now is the monotonic clock of every decision; nil means time.Now.
	Now func() time.Time
	// Wall is the wall clock of the lease's renewed_at (for people only); nil
	// means time.Now.
	Wall func() time.Time
	// Logger receives role changes and lease errors; nil means slog.Default().
	Logger *slog.Logger
	// EpochFloor, when set, returns the highest epoch known outside the lease: of
	// the metadata snapshots on the HA target and of this instance's own state. An
	// acquired lease always gets a higher epoch, so a lease deleted or corrupted by
	// hand never restarts the epochs below snapshots that already exist. An error
	// defers the acquisition to the next step.
	EpochFloor func(ctx context.Context) (uint64, error)
}

// Status is a snapshot of a Manager's state.
type Status struct {
	// Role is the current role.
	Role Role `json:"role"`
	// Cluster is the cluster name.
	Cluster string `json:"cluster"`
	// InstanceID identifies this instance.
	InstanceID string `json:"instance_id"`
	// Epoch is the epoch this instance holds (active), or the highest it saw.
	Epoch uint64 `json:"epoch"`
	// Holder and HolderName identify the holder of the lease as last seen ("" when
	// released or unknown).
	Holder     string `json:"holder,omitempty"`
	HolderName string `json:"holder_name,omitempty"`
	// LeaseHealthy reports that the last read or write of the lease succeeded.
	LeaseHealthy bool `json:"lease_healthy"`
	// LastError is the last lease error (redacted), "" after a success.
	LastError string `json:"last_error,omitempty"`
	// KeyMismatch reports a lease written with another secret key.
	KeyMismatch bool `json:"key_mismatch,omitempty"`
	// LeaseErrors counts failed reads and writes of the lease; Takeovers counts the
	// leases this instance acquired.
	LeaseErrors uint64 `json:"lease_errors"`
	Takeovers   uint64 `json:"takeovers"`
}

// Manager acquires, renews and gives up the lease of one instance. Run drives it;
// Check is the fencing check of runs. It is safe for concurrent use.
type Manager struct {
	cfg    Config
	key    string
	logger *slog.Logger

	// opMu serialises lease operations (steps of Run, Release, StepDown).
	opMu sync.Mutex

	// mu guards the state below; it is never held during storage I/O.
	mu   sync.RWMutex
	role Role
	// lease and version are the lease this instance holds (active) and its
	// version; sentAt is when the last successful write of it was sent, on the
	// monotonic clock, and sentWall the same moment on the wall clock.
	lease    Lease
	version  string
	sentAt   time.Time
	sentWall time.Time
	// seen, seenVersion and seenAt are the lease as last read, its version and
	// when that version was first observed; seenEpoch is the highest epoch seen or
	// held.
	seen        Lease
	seenVersion string
	seenAt      time.Time
	seenEpoch   uint64
	// cooldownUntil keeps a standby from acquiring after it gave the lease up.
	cooldownUntil time.Time
	healthy       bool
	lastErr       string
	keyMismatch   bool
	leaseErrors   uint64
	takeovers     uint64
	// fresh reports that the held lease was created: no lease object existed
	// when this instance acquired it.
	fresh bool

	changes chan struct{}
}

// New returns a standby Manager.
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil || cfg.InstanceID == "" {
		return nil, fmt.Errorf("%w: a lease store and an instance ID are required", ErrInvalidConfig)
	}
	if err := ValidateCluster(cfg.Cluster); err != nil {
		return nil, err
	}
	if err := ValidateTiming(cfg.TTL, cfg.Renew); err != nil {
		return nil, err
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Wall == nil {
		cfg.Wall = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Manager{
		cfg: cfg, key: LeaseKey(cfg.Cluster), logger: cfg.Logger.With(slog.String("ha_cluster", cfg.Cluster)),
		role: RoleStandby, changes: make(chan struct{}, 1),
	}, nil
}

// Changes returns a channel that receives a value after every role change (it
// holds at most one pending value). Read Status for the new role.
func (m *Manager) Changes() <-chan struct{} { return m.changes }

// Status returns the current state.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := Status{
		Role: m.role, Cluster: m.cfg.Cluster, InstanceID: m.cfg.InstanceID, Epoch: m.seenEpoch,
		Holder: m.seen.Holder, HolderName: m.seen.HolderName, LeaseHealthy: m.healthy, LastError: m.lastErr,
		KeyMismatch: m.keyMismatch, LeaseErrors: m.leaseErrors, Takeovers: m.takeovers,
	}
	if m.role == RoleActive {
		st.Epoch, st.Holder, st.HolderName = m.lease.Epoch, m.lease.Holder, m.lease.HolderName
	}
	return st
}

// Role returns the current role.
func (m *Manager) Role() Role {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.role
}

// deadlineLocked is when the active instance must have stepped down without a
// successful renewal: ttl - renew after the last successful write was sent.
func (m *Manager) deadlineLocked() time.Time {
	return m.sentAt.Add(m.cfg.TTL - m.cfg.Renew)
}

// wallNow returns the wall clock without its monotonic reading.
func (m *Manager) wallNow() time.Time { return m.cfg.Wall().Round(0) }

// expiredLocked reports whether the step-down deadline has passed on either clock:
// the monotonic one, or the wall clock, which keeps running while the machine is
// suspended (a laptop lid, a paused VM) when the monotonic clock may not.
func (m *Manager) expiredLocked(now, wall time.Time) bool {
	return !now.Before(m.deadlineLocked()) || wall.Sub(m.sentWall) >= m.cfg.TTL-m.cfg.Renew
}

// Check returns nil while this instance holds the lease, and ErrNotActive
// otherwise. It reads the clocks itself, so it fails at the step-down deadline even
// when Run is stuck in a storage call, and after a suspend that only the wall clock
// saw. Runs call it before they start and before they are marked completed.
func (m *Manager) Check() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.role != RoleActive || m.expiredLocked(m.cfg.Now(), m.wallNow()) {
		return ErrNotActive
	}
	return nil
}

// Epoch returns the epoch this instance holds, or 0 when it is not active.
func (m *Manager) Epoch() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.role != RoleActive {
		return 0
	}
	return m.lease.Epoch
}

// Run renews (active) or reads (standby) the lease now and then every renew
// interval until ctx ends, and wakes at the active instance's step-down deadline
// when that comes first, so a step-down after a failed renewal is never late. It
// does not release the lease: call Release after the work it guards has stopped.
func (m *Manager) Run(ctx context.Context) {
	for {
		m.Step(ctx)
		timer := time.NewTimer(m.untilNextStep())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// untilNextStep returns how long Run waits for the next step: the renew interval,
// or the time left to the step-down deadline of an active instance when that is
// shorter.
func (m *Manager) untilNextStep() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	wait := m.cfg.Renew
	if m.role == RoleActive {
		wait = min(wait, max(m.deadlineLocked().Sub(m.cfg.Now()), 0),
			max(m.sentWall.Add(m.cfg.TTL-m.cfg.Renew).Sub(m.wallNow()), 0))
	}
	return wait
}

// Step runs one lease operation: a renewal when active, a read (and takeover when
// the lease expired) when standby. Run calls it every renew interval.
func (m *Manager) Step(ctx context.Context) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if m.Role() == RoleActive {
		m.renew(ctx)
		return
	}
	m.observe(ctx)
}

// renew renews the held lease, or steps down when it cannot be sure it still holds
// it.
func (m *Manager) renew(ctx context.Context) {
	now, wall := m.cfg.Now(), m.wallNow()
	m.mu.RLock()
	deadline, next, version := m.deadlineLocked(), m.lease, m.version
	expired := m.expiredLocked(now, wall)
	m.mu.RUnlock()
	if expired {
		m.stepDown("the lease was not renewed in time", true)
		return
	}
	next.Seq++
	next.RenewedAt = m.cfg.Wall().UTC()
	data, err := next.encode()
	if err != nil {
		m.recordError(err)
		return
	}
	// The request may not outlive the deadline: the step-down must not wait for it.
	rctx, cancel := context.WithTimeout(ctx, min(m.cfg.Renew, deadline.Sub(now)))
	newVersion, err := m.cfg.Store.ReplaceIfMatch(rctx, m.key, data, version)
	cancel()
	switch {
	case err == nil:
		m.mu.Lock()
		m.lease, m.version, m.sentAt, m.sentWall = next, newVersion, now, wall
		m.healthy, m.lastErr = true, ""
		m.mu.Unlock()
	case errors.Is(err, storage.ErrPreconditionFailed):
		m.recordError(err)
		m.stepDown("another instance wrote the lease", true)
	default:
		m.recordError(err)
		m.mu.RLock()
		expired = m.expiredLocked(m.cfg.Now(), m.wallNow())
		m.mu.RUnlock()
		if expired {
			m.stepDown("the lease storage did not answer in time", true)
		}
	}
}

// observe reads the lease as a standby and takes it over when it is free or
// expired.
func (m *Manager) observe(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, m.cfg.Renew)
	data, version, err := m.cfg.Store.ReadVersioned(rctx, m.key)
	cancel()
	// The observation is stamped when the read returns, never when it was sent: a
	// renewal may have committed while the read was in flight, and the expiry of
	// what was read must not start before the read could have seen it.
	now := m.cfg.Now()
	if errors.Is(err, storage.ErrNotFound) {
		m.mu.Lock()
		m.healthy, m.lastErr = true, ""
		m.seen, m.seenVersion = Lease{}, ""
		cooling := now.Before(m.cooldownUntil)
		epoch := m.seenEpoch + 1
		m.mu.Unlock()
		if !cooling {
			m.acquire(ctx, epoch, 1, nil, "")
		}
		return
	}
	if err != nil {
		m.recordError(err)
		return
	}
	lease, decodeErr := DecodeLease(data)
	m.mu.Lock()
	m.healthy, m.lastErr = true, ""
	if decodeErr != nil {
		// A lease nobody can read expires like any other; its epoch is unknown, so
		// the next one continues from the highest seen.
		m.lastErr = redact.Text(decodeErr.Error())
		lease = Lease{}
	}
	if lease.KeyID != "" && m.cfg.KeyID != "" && lease.KeyID != m.cfg.KeyID {
		m.keyMismatch = true
		m.lastErr = ErrKeyMismatch.Error()
		m.mu.Unlock()
		return
	}
	m.keyMismatch = false
	if version != m.seenVersion {
		m.seen, m.seenVersion, m.seenAt = lease, version, now
	}
	if lease.Epoch > m.seenEpoch {
		m.seenEpoch = lease.Epoch
	}
	cooling := now.Before(m.cooldownUntil)
	released := decodeErr == nil && lease.Released()
	expired := released || now.Sub(m.seenAt) >= max(m.cfg.TTL, lease.ttl())
	epoch, seq := m.seenEpoch+1, lease.Seq+1
	m.mu.Unlock()
	if cooling || !expired {
		return
	}
	if !released {
		m.logger.Warn("the HA lease expired; taking over",
			logsafe.Attr("previous_holder", lease.HolderName), slog.Uint64("previous_epoch", lease.Epoch))
	}
	m.acquire(ctx, epoch, seq, data, version)
}

// AcquiredFresh reports whether the held lease was created because no lease object
// existed (the cluster's first activation, or a lease removed by hand), rather than
// taken over from an expired, released or unreadable one.
func (m *Manager) AcquiredFresh() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.role == RoleActive && m.fresh
}

// acquire writes a lease held by this instance with epoch (raised above
// Config.EpochFloor): created when version is "" (prev is nil), else replacing
// version.
func (m *Manager) acquire(ctx context.Context, epoch, seq uint64, prev []byte, version string) {
	if m.cfg.EpochFloor != nil {
		fctx, fcancel := context.WithTimeout(ctx, m.cfg.Renew)
		floor, err := m.cfg.EpochFloor(fctx)
		fcancel()
		if err != nil {
			m.recordError(fmt.Errorf("ha: the highest epoch on record is unknown, the lease is not acquired: %w", err))
			return
		}
		epoch = max(epoch, floor+1)
	}
	next := Lease{
		Cluster: m.cfg.Cluster, Holder: m.cfg.InstanceID, HolderName: m.cfg.InstanceName, Epoch: epoch, Seq: seq,
		RenewedAt: m.cfg.Wall().UTC(), TTLSeconds: int64(m.cfg.TTL / time.Second), KeyID: m.cfg.KeyID,
	}
	data, err := next.encode()
	if err != nil {
		m.recordError(err)
		return
	}
	sent, sentWall := m.cfg.Now(), m.wallNow()
	rctx, cancel := context.WithTimeout(ctx, m.cfg.Renew)
	var newVersion string
	if prev == nil {
		newVersion, err = m.cfg.Store.CreateIfAbsent(rctx, m.key, data)
	} else {
		newVersion, err = m.cfg.Store.ReplaceIfMatch(rctx, m.key, data, version)
	}
	cancel()
	if errors.Is(err, storage.ErrPreconditionFailed) {
		// Another instance was faster (or the holder renewed): read again next time.
		return
	}
	if err != nil {
		m.recordError(err)
		return
	}
	m.mu.Lock()
	m.role, m.lease, m.version, m.sentAt, m.sentWall = RoleActive, next, newVersion, sent, sentWall
	m.seen, m.seenVersion, m.seenAt, m.seenEpoch = next, newVersion, sent, epoch
	m.healthy, m.lastErr = true, ""
	m.fresh = prev == nil
	m.takeovers++
	m.mu.Unlock()
	m.logger.Info("acquired the HA lease: this instance is now active", slog.Uint64("epoch", epoch))
	m.notify()
}

// stepDown makes this instance a standby without touching the lease. With
// cooldown it does not try to acquire the lease again for a TTL.
func (m *Manager) stepDown(reason string, cooldown bool) {
	m.mu.Lock()
	if m.role != RoleActive {
		m.mu.Unlock()
		return
	}
	now := m.cfg.Now()
	m.role = RoleStandby
	epoch := m.lease.Epoch
	// The lease as this instance last wrote it: a standby measures its expiry from
	// the moment the write was sent.
	m.seen, m.seenVersion, m.seenAt = m.lease, m.version, m.sentAt
	if cooldown {
		m.cooldownUntil = now.Add(m.cfg.TTL)
	}
	m.mu.Unlock()
	m.logger.Warn("stepped down to standby: "+reason, slog.Uint64("epoch", epoch))
	m.notify()
}

// StepDown makes an active instance a standby that waits a TTL before it tries to
// acquire the lease again, and releases the lease when release is set (so another
// standby takes over at its next read). It is used when this instance cannot work
// as active, such as when no metadata snapshot can be used.
func (m *Manager) StepDown(ctx context.Context, reason string, release bool) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	var err error
	if release {
		err = m.releaseLocked(ctx)
	}
	m.stepDown(reason, true)
	return err
}

// Release gives the lease up after a graceful shutdown of the work it guards: the
// lease is written without a holder, so a standby takes it over at its next read
// instead of after the TTL. The instance becomes a standby that waits a TTL before
// it tries to acquire again. It is a no-op for a standby.
func (m *Manager) Release(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	err := m.releaseLocked(ctx)
	m.stepDown("released the lease", true)
	return err
}

// releaseLocked writes the held lease without a holder; m.opMu must be held.
func (m *Manager) releaseLocked(ctx context.Context) error {
	m.mu.RLock()
	active, next, version := m.role == RoleActive, m.lease, m.version
	m.mu.RUnlock()
	if !active {
		return nil
	}
	next.Holder, next.HolderName = "", ""
	next.Seq++
	next.RenewedAt = m.cfg.Wall().UTC()
	data, err := next.encode()
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, m.cfg.Renew)
	defer cancel()
	newVersion, err := m.cfg.Store.ReplaceIfMatch(rctx, m.key, data, version)
	if err != nil {
		m.recordError(err)
		return fmt.Errorf("ha: release the lease: %w", err)
	}
	m.mu.Lock()
	m.lease, m.version, m.sentAt, m.sentWall = next, newVersion, m.cfg.Now(), m.wallNow()
	m.mu.Unlock()
	return nil
}

// Observe reads the lease once without acting on it (used at startup to refuse a
// lease of another secret key). It returns storage.ErrNotFound when there is none.
func (m *Manager) Observe(ctx context.Context) (Lease, error) {
	rctx, cancel := context.WithTimeout(ctx, m.cfg.Renew)
	defer cancel()
	data, _, err := m.cfg.Store.ReadVersioned(rctx, m.key)
	if err != nil {
		return Lease{}, err
	}
	lease, err := DecodeLease(data)
	if err != nil {
		return Lease{}, err
	}
	if lease.KeyID != "" && m.cfg.KeyID != "" && lease.KeyID != m.cfg.KeyID {
		return lease, ErrKeyMismatch
	}
	return lease, nil
}

// recordError counts a lease error.
func (m *Manager) recordError(err error) {
	msg := redact.Text(err.Error())
	m.mu.Lock()
	m.leaseErrors++
	m.healthy, m.lastErr = false, msg
	m.mu.Unlock()
	m.logger.Warn("HA lease operation failed", slog.String("error", msg))
}

// notify signals a role change without blocking.
func (m *Manager) notify() {
	select {
	case m.changes <- struct{}{}:
	default:
	}
}

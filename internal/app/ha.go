package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/ha"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/metrics"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/server"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// LostLeaseReason is the reason recorded on the backups and restores an active
// instance cancels when it loses the HA lease.
const LostLeaseReason = "interrupted: this instance lost the HA lease, another instance took over"

// HA timing of the role changes.
const (
	// haStandbyDrain bounds the HTTP drain of a standby that takes over: it serves
	// reads only, and the takeover should not wait for them.
	haStandbyDrain = 2 * time.Second
	// haProbeTimeout bounds the conditional-write probe at startup.
	haProbeTimeout = 30 * time.Second
)

// ErrHANeedsSharedKey is returned by RunHA when neither MONGORESCUE_SECRET_KEY nor
// a secret.key file is present: an instance would generate a key of its own, and
// could not read the other instances' snapshots.
var ErrHANeedsSharedKey = errors.New("app: active-passive HA needs the secret key shared by every instance: set " +
	config.EnvSecretKey + " (or mount the same secret.key in every data directory)")

// haRole is the role an App is opened in.
type haRole struct {
	rt     *haRuntime
	active bool
}

// fence returns the fencing check of the App's runs (nil outside HA mode).
func (o options) fence() models.Fence {
	if o.ha == nil {
		return nil
	}
	return o.ha.rt
}

// haKey returns the secret key an HA App keeps for its snapshots (nil outside HA).
func haKey(role *haRole, key []byte) []byte {
	if role == nil {
		return nil
	}
	return slices.Clone(key)
}

// haRuntime is the state of an HA member that outlives the Apps of its roles: the
// lease manager, the storage of the lease and the counters of the HA metrics.
type haRuntime struct {
	cfg    config.HAConfig
	target config.HATarget
	state  ha.State
	name   string
	logger *slog.Logger

	mu      sync.RWMutex
	manager *ha.Manager
	store   storage.Storage
	// key is the secret key (the snapshots' passphrase derives from it).
	key []byte
	// since is the creation time of the snapshot the last takeover restored or
	// kept; zero when there was none. Archives written after it are imported.
	since time.Time

	// lastTakeover holds the duration of the last takeover in seconds (float64
	// bits); takeoverFailures counts the takeovers given up.
	lastTakeover     atomic.Uint64
	takeoverFailures atomic.Uint64
	// stepDowns counts the times this instance lost the lease while active.
	stepDowns atomic.Uint64
}

// mgr returns the lease manager (nil before the bootstrap).
func (rt *haRuntime) mgr() *ha.Manager {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.manager
}

// storage returns the driver of the HA target.
func (rt *haRuntime) storage() storage.Storage {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.store
}

// takeoverSince returns the creation time of the snapshot of the last takeover.
func (rt *haRuntime) takeoverSince() time.Time {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.since
}

// Check implements models.Fence: nil while this instance holds the lease.
func (rt *haRuntime) Check() error {
	m := rt.mgr()
	if m == nil {
		return ha.ErrNotActive
	}
	return m.Check()
}

// Renew implements models.FenceRenewer: one lease step now, then Check.
func (rt *haRuntime) Renew(ctx context.Context) error {
	if m := rt.mgr(); m != nil {
		m.Step(ctx)
	}
	return rt.Check()
}

// Epoch implements models.Fence.
func (rt *haRuntime) Epoch() uint64 {
	if m := rt.mgr(); m != nil {
		return m.Epoch()
	}
	return 0
}

// status returns the manager's status (a standby without a lease before the
// bootstrap).
func (rt *haRuntime) status() ha.Status {
	if m := rt.mgr(); m != nil {
		return m.Status()
	}
	return ha.Status{Role: ha.RoleStandby, Cluster: rt.cfg.Cluster, InstanceID: rt.state.InstanceID}
}

// serverStatus is the HA status of GET /api/v1/health.
func (rt *haRuntime) serverStatus() server.HAStatus {
	st := rt.status()
	role := server.HARoleStandby
	if st.Role == ha.RoleActive && rt.Check() == nil {
		role = server.HARoleActive
	}
	holder := st.HolderName
	if holder == "" {
		holder = st.Holder
	}
	return server.HAStatus{Role: role, Cluster: st.Cluster, Epoch: st.Epoch, Holder: holder,
		LeaseHealthy: st.LeaseHealthy, RetryAfterSeconds: int(math.Ceil(rt.cfg.RenewInterval.Seconds()))}
}

// metricsState is the source of the HA metrics.
func (rt *haRuntime) metricsState() metrics.HAState {
	st := rt.serverStatus()
	ms := rt.status()
	return metrics.HAState{Cluster: st.Cluster, Role: st.Role, Epoch: st.Epoch, LeaseHealthy: st.LeaseHealthy, LeaseErrors: ms.LeaseErrors,
		Takeovers: ms.Takeovers, LastTakeoverSeconds: math.Float64frombits(rt.lastTakeover.Load()),
		TakeoverFailures: rt.takeoverFailures.Load(), StepDowns: rt.stepDowns.Load()}
}

// RunHA runs MongoRescue as a member of the active-passive HA cluster of cfg.HA
// (docs/design/ha.md) until SIGINT or SIGTERM. The instance starts as a standby: it
// serves the dashboard, the read-only API, the health check and the metrics, and
// runs no background work. When it acquires the lease it reopens as the active
// instance and starts the scheduler, the PITR collector and the other background
// work; when it loses the lease it cancels its runs and reopens as a standby. A
// graceful shutdown of the active instance releases the lease.
func RunHA(cfg *config.Config, logger *slog.Logger, opts ...Option) error {
	if logger == nil {
		logger = slog.Default()
	}
	if !cfg.HA.Enabled() {
		return fmt.Errorf("app: RunHA needs %s=%s", config.EnvHAMode, config.HAModeActivePassive)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	if cfg.SecretKey == "" {
		if _, err := os.Stat(filepath.Join(cfg.DataDir, secretbox.KeyFileName)); err != nil {
			return ErrHANeedsSharedKey
		}
	}
	target, err := config.ParseHATarget(cfg.HA.Target)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	state, err := ha.LoadState(cfg.DataDir)
	if err != nil {
		return err
	}
	name, _ := os.Hostname()
	if name == "" {
		name = state.InstanceID
	}
	rt := &haRuntime{cfg: cfg.HA, target: target, state: state, name: name,
		logger: logger.With(slog.String("ha_cluster", cfg.HA.Cluster))}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sup := &haSupervisor{cfg: cfg, logger: logger, opts: opts, rt: rt}
	return sup.run(ctx)
}

// haSupervisor opens the App of each role in turn.
type haSupervisor struct {
	cfg    *config.Config
	logger *slog.Logger
	opts   []Option
	rt     *haRuntime
	// onActive, when set, is called with every App that started as active (tests).
	onActive func(*App)
}

// open builds the App of a role.
func (s *haSupervisor) open(active bool) (*App, error) {
	opts := append(slices.Clone(s.opts), func(o *options) { o.ha = &haRole{rt: s.rt, active: active} })
	return New(s.cfg, s.logger, opts...)
}

// run is the role loop of RunHA.
func (s *haSupervisor) run(ctx context.Context) error {
	app, err := s.open(false)
	if err != nil {
		return err
	}
	if err = s.bootstrap(ctx, app); err != nil {
		_ = app.Close()
		return err
	}
	managerCtx, cancelManager := context.WithCancel(context.Background())
	var managerWG sync.WaitGroup
	managerWG.Go(func() { s.rt.mgr().Run(managerCtx) })
	defer func() {
		cancelManager()
		managerWG.Wait()
	}()

	for {
		becameActive, err := s.serveStandby(ctx, app)
		if err != nil || !becameActive {
			return err
		}
		start := time.Now()
		if app, err = s.takeOver(ctx); err != nil {
			return err
		}
		if !app.ha.active {
			continue
		}
		lost, err := s.serveActive(ctx, app, start)
		if err != nil || !lost {
			return err
		}
		if app, err = s.open(false); err != nil {
			return fmt.Errorf("reopen as HA standby: %w", err)
		}
	}
}

// bootstrap opens the storage of the lease, checks that it enforces conditional
// writes and that the lease was written with this instance's secret key, and builds
// the lease manager.
func (s *haSupervisor) bootstrap(ctx context.Context, app *App) error {
	st, err := s.haStorage(ctx, app)
	if err != nil {
		return fmt.Errorf("HA target %s: %w", config.HATargetLabel(s.cfg.HA.Target), err)
	}
	ls, err := storage.AsLeaseStore(st)
	if err != nil {
		return fmt.Errorf("HA target %s: %w", config.HATargetLabel(s.cfg.HA.Target), err)
	}
	probeKey := ha.ProbeKey(s.cfg.HA.Cluster, s.rt.state.InstanceID)
	probeCtx, cancel := context.WithTimeout(ctx, haProbeTimeout)
	defer cancel()
	// A probe that a crash interrupted left its object behind.
	if delErr := st.Delete(probeCtx, probeKey); delErr != nil && !errors.Is(delErr, storage.ErrNotFound) {
		s.logger.Debug("could not remove an earlier HA probe object", logsafe.Error(delErr))
	}
	if err = storage.ProbeLeaseStore(probeCtx, ls, probeKey, st.Delete); err != nil {
		if errors.Is(err, storage.ErrConditionalWritesUnsupported) {
			return fmt.Errorf("the HA target does not enforce conditional writes (If-None-Match/If-Match), so the lease would not be safe; use AWS S3, MinIO or another storage that does: %w", err)
		}
		return fmt.Errorf("HA target %s: %w", config.HATargetLabel(s.cfg.HA.Target), err)
	}
	keyID, err := metabackup.InstallID(app.haKey)
	if err != nil {
		return err
	}
	mgr, err := ha.New(ha.Config{
		Store: ls, Cluster: s.cfg.HA.Cluster, InstanceID: s.rt.state.InstanceID, InstanceName: s.rt.name, KeyID: keyID,
		TTL: s.cfg.HA.LeaseTTL, Renew: s.cfg.HA.RenewInterval, Logger: s.logger,
	})
	if err != nil {
		return err
	}
	lease, err := mgr.Observe(ctx)
	switch {
	case errors.Is(err, ha.ErrKeyMismatch):
		return err
	case errors.Is(err, storage.ErrNotFound):
		s.logger.Info("no HA lease yet: this instance creates it", slog.String("ha_cluster", s.cfg.HA.Cluster))
	case err != nil:
		s.logger.Warn("could not read the HA lease at startup; retrying in the background", logsafe.Error(err))
	default:
		s.logger.Info("joined the HA cluster as standby", slog.String("ha_cluster", s.cfg.HA.Cluster),
			logsafe.Attr("holder", lease.HolderName), slog.Uint64("epoch", lease.Epoch))
	}
	if err = s.checkSnapshotKey(ctx, st, app.haKey); err != nil {
		return err
	}
	s.rt.mu.Lock()
	s.rt.manager, s.rt.store, s.rt.key = mgr, st, slices.Clone(app.haKey)
	s.rt.mu.Unlock()
	return nil
}

// haStorage opens the storage of the lease: a location of the bootstrap
// configuration, or a storage target of the database (resolved once).
func (s *haSupervisor) haStorage(ctx context.Context, app *App) (storage.Storage, error) {
	t := s.rt.target
	switch t.Scheme {
	case "s3":
		return storage.NewS3Storage(ctx, storage.S3Config{Endpoint: t.Endpoint, Bucket: t.Bucket, Region: t.Region,
			Prefix: t.Prefix, UsePathStyle: t.PathStyle, Logger: s.logger})
	case "file":
		return storage.NewLocalStorage(t.Path)
	}
	list, err := app.targets.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, tg := range list {
		if tg.ID != t.Ref && tg.Name != t.Ref {
			continue
		}
		if tg.ObjectLocked() {
			return nil, errors.New("the storage target has S3 Object Lock, and every lease renewal would leave a locked version behind; use a target without Object Lock")
		}
		return app.targets.Storage(ctx, tg.ID)
	}
	return nil, errors.New("no storage target has this ID or name in this instance's database (a fresh standby has none yet): use a location URL such as s3://bucket/prefix?endpoint=… instead")
}

// serve starts the HTTP server of app; the channel receives its error when it stops.
func serve(app *App) <-chan error {
	errCh := make(chan error, 1)
	go func() { errCh <- app.server.Start() }()
	return errCh
}

// closeHTTP stops the HTTP server of app, waiting at most drain for requests.
func closeHTTP(app *App, drain time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	_ = app.server.Shutdown(ctx)
	_ = app.server.Close()
}

// serveStandby runs app as a standby until the lease is acquired (true) or ctx
// ends (false). app is closed when it returns.
func (s *haSupervisor) serveStandby(ctx context.Context, app *App) (bool, error) {
	mgr := s.rt.mgr()
	if mgr.Role() == ha.RoleActive {
		return true, app.Close()
	}
	if err := app.Start(ctx); err != nil {
		_ = app.Close()
		return false, err
	}
	errCh := serve(app)
	s.logger.Info("mongorescue ready as HA standby (read-only)", slog.String("ha_cluster", s.cfg.HA.Cluster),
		slog.Int("port", s.cfg.Port))
	for {
		select {
		case <-ctx.Done():
			closeHTTP(app, httpDrainTimeout)
			app.Stop()
			return false, app.Close()
		case err := <-errCh:
			app.Stop()
			_ = app.Close()
			return false, err
		case <-mgr.Changes():
			if mgr.Role() != ha.RoleActive {
				continue
			}
			s.logger.Info("this instance acquired the HA lease; taking over", slog.Uint64("epoch", mgr.Epoch()))
			closeHTTP(app, haStandbyDrain)
			app.Stop()
			return true, app.Close()
		}
	}
}

// takeOver prepares the data directory of the new active instance and opens its
// App. When the metadata cannot be taken over, it releases the lease and returns a
// standby App.
func (s *haSupervisor) takeOver(ctx context.Context) (*App, error) {
	mgr := s.rt.mgr()
	if err := s.prepareTakeover(ctx); err != nil {
		s.rt.takeoverFailures.Add(1)
		s.logger.Error("cannot take over: giving the HA lease up and staying standby", logsafe.Error(err))
		if relErr := mgr.StepDown(ctx, "the takeover failed", true); relErr != nil {
			s.logger.Warn("could not release the HA lease", logsafe.Error(relErr))
		}
		return s.open(false)
	}
	if mgr.Role() != ha.RoleActive {
		return s.open(false)
	}
	app, err := s.open(true)
	if err != nil {
		if relErr := mgr.StepDown(ctx, "the active instance could not open", true); relErr != nil {
			s.logger.Warn("could not release the HA lease", logsafe.Error(relErr))
		}
		return nil, fmt.Errorf("open as HA active: %w", err)
	}
	return app, nil
}

// serveActive runs app as the active instance until the lease is lost (true) or ctx
// ends (false, after a graceful shutdown that releases the lease). app is closed
// when it returns.
func (s *haSupervisor) serveActive(ctx context.Context, app *App, start time.Time) (bool, error) {
	mgr := s.rt.mgr()
	if err := app.Start(ctx); err != nil {
		app.Stop()
		_ = app.Close()
		_ = mgr.StepDown(context.Background(), "the active instance could not start", true)
		return false, err
	}
	errCh := serve(app)
	if s.onActive != nil {
		s.onActive(app)
	}
	epoch := mgr.Epoch()
	s.rt.lastTakeover.Store(math.Float64bits(time.Since(start).Seconds()))
	s.recordActiveEpoch(epoch)
	app.publishHA(ctx, events.HATookOver, fmt.Sprintf("instance %s (%s) is now the active instance of HA cluster %s (epoch %d)",
		s.rt.name, s.rt.state.InstanceID, s.cfg.HA.Cluster, epoch))
	s.logger.Info("mongorescue ready as HA active", slog.String("ha_cluster", s.cfg.HA.Cluster), slog.Uint64("epoch", epoch),
		slog.Duration("takeover", time.Since(start)), slog.Int("port", s.cfg.Port))
	if code := app.auth.SetupCode(); code != "" {
		s.logger.Warn("Setup required: create the first user with setup code "+code, slog.String("setup_code", code))
	}
	for {
		select {
		case <-ctx.Done():
			return false, s.shutdownActive(app)
		case err := <-errCh:
			app.ForceStop(ShutdownReason)
			_ = mgr.Release(context.Background())
			_ = app.Close()
			return false, err
		case <-mgr.Changes():
			if mgr.Role() == ha.RoleActive {
				continue
			}
			return true, s.loseLease(app, epoch)
		}
	}
}

// shutdownActive is the graceful shutdown of the active instance: it drains the
// runs like a single instance, then releases the lease so a standby takes over at
// its next read.
func (s *haSupervisor) shutdownActive(app *App) error {
	s.logger.Info("shutdown signal received, initiating graceful exit of the HA active instance...")
	again, stopAgain := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := app.shutdown(again)
	stopAgain()
	s.finalSnapshot(app)
	if relErr := s.rt.mgr().Release(context.Background()); relErr != nil {
		s.logger.Warn("could not release the HA lease; a standby takes over once it expires", logsafe.Error(relErr))
	} else {
		s.logger.Info("released the HA lease")
	}
	return errors.Join(err, app.Close())
}

// loseLease stops the active instance that lost the lease: it cancels its runs and
// closes its App. When runs do not stop in time, it returns an error so the process
// exits and its restart policy starts a fresh standby.
func (s *haSupervisor) loseLease(app *App, epoch uint64) error {
	st := s.rt.status()
	s.rt.stepDowns.Add(1)
	s.logger.Error("this instance lost the HA lease: cancelling its runs and stepping down to standby",
		slog.Uint64("epoch", epoch), slog.String("error", st.LastError))
	// Work stops first; the alert follows before the App closes.
	_ = app.server.Close()
	app.ForceStop(LostLeaseReason)
	s.notifyStepDown(app, epoch)
	if app.runsAbandoned.Load() {
		return errors.New("runs did not stop after the HA lease was lost; exiting so that the restart policy starts a fresh standby")
	}
	return app.Close()
}

// haStepDownNotifyTimeout bounds the direct delivery of the step-down alert.
const haStepDownNotifyTimeout = 5 * time.Second

// notifyStepDown sends the ha.stepped_down alert to every enabled channel at once,
// best effort: an instance that lost the lease runs no notification outbox any
// more, so the alert does not go through it.
func (s *haSupervisor) notifyStepDown(app *App, epoch uint64) {
	if app.notifications == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), haStepDownNotifyTimeout)
	defer cancel()
	sent := app.notifications.BroadcastNow(ctx, events.Event{Type: events.HASteppedDown, Time: time.Now().UTC(), Status: string(events.HASteppedDown),
		Detail: fmt.Sprintf("instance %s (%s) lost the lease of HA cluster %s (epoch %d) and stepped down to standby; its running backups and restores were cancelled",
			s.rt.name, s.rt.state.InstanceID, s.cfg.HA.Cluster, epoch)})
	s.logger.Info("sent the HA step-down alert", slog.Int("channels", sent))
}

// recordActiveEpoch remembers the epoch under which this instance's database is
// the cluster's.
func (s *haSupervisor) recordActiveEpoch(epoch uint64) {
	s.rt.state.Cluster, s.rt.state.LastActiveEpoch = s.cfg.HA.Cluster, epoch
	if err := ha.SaveState(s.cfg.DataDir, s.rt.state); err != nil {
		s.logger.Warn("could not record the HA epoch in the data directory", logsafe.Error(err))
	}
}

// publishHA publishes an HA event on the App's bus.
func (a *App) publishHA(ctx context.Context, t events.EventType, detail string) {
	if a.bus == nil {
		return
	}
	a.bus.Publish(ctx, events.Event{Type: t, Time: time.Now().UTC(), Status: string(t), Detail: detail})
}

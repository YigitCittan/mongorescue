// Package app wires dependencies, manages application lifecycle, and coordinates graceful shutdowns.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/mcp"
	"github.com/yigitcittan/mongorescue/internal/metrics"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/server"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/targets"
	"github.com/yigitcittan/mongorescue/web"
)

// staleToolsConfigAge is the minimum age of a leftover tools config file removed at startup.
const staleToolsConfigAge = time.Minute

// Shutdown budget. In-flight backups and restores are cancelled (their mongo tools get
// SIGTERM, then SIGKILL after mongotools.KillGracePeriod) and awaited for at most
// shutdownTimeout; anything still running afterwards is killed outright.
const (
	shutdownTimeout  = 30 * time.Second
	httpDrainTimeout = 15 * time.Second
	forceKillGrace   = 5 * time.Second
)

// ErrStarted is returned by Start when the App was already started (or stopped): its
// background work runs at most once.
var ErrStarted = errors.New("app: already started")

// App manages the lifecycle of all MongoRescue core systems.
type App struct {
	cfg           *config.Config
	logger        *slog.Logger
	server        *server.Server
	scheduler     *scheduler.Scheduler
	bus           *events.Bus
	notifications *notify.Service
	metrics       *metrics.Metrics
	runs          *runs.Manager
	metaStore     store.Store
	auth          *auth.Service
	settings      *settings.Service
	targets       *targets.Service

	// storeCloser releases the metadata database and dirLock the data directory;
	// Close releases both once.
	storeCloser io.Closer
	dirLock     *store.DirLock
	closeOnce   sync.Once
	closeErr    error

	// runsAbandoned is set when shutdown gave up waiting for background runs; the
	// metadata database is then left open for the process exit to release, so no
	// runner writes to a closed database.
	runsAbandoned atomic.Bool

	// lifeMu guards the Start/Stop state below.
	lifeMu         sync.Mutex
	started        bool
	stopped        bool
	stopBackground func()
}

// options holds optional App settings.
type options struct {
	desktop bool
	version string
	commit  string
	getenv  func(string) string
}

// Option customises App construction.
type Option func(*options)

// WithGetenv replaces os.Getenv for reading deprecated environment variables (tests).
func WithGetenv(getenv func(string) string) Option {
	return func(o *options) { o.getenv = getenv }
}

// WithDesktop marks the App as served to the desktop app's webview, whose page
// origin needs a wider Content-Security-Policy (see server.WithDesktopCSP).
func WithDesktop() Option {
	return func(o *options) { o.desktop = true }
}

// WithBuildInfo sets the version and commit reported by mongorescue_build_info.
func WithBuildInfo(version, commit string) Option {
	return func(o *options) {
		o.version, o.commit = version, commit
	}
}

// New creates and wires all MongoRescue services. It opens the metadata database,
// imports a legacy state.json and the deprecated environment variables and
// <data_dir>/config.json once, creates the default "Local disk" storage target on
// first start and loads the dashboard-managed settings. The caller owns the returned
// App and must call Run or Close.
func New(cfg *config.Config, logger *slog.Logger, opts ...Option) (_ *App, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	o := options{version: "dev", commit: "unknown", getenv: os.Getenv}
	for _, opt := range opts {
		opt(&o)
	}
	if err = cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	ctx := context.Background()

	legacy, err := config.LoadLegacy(cfg.DataDir, o.getenv)
	if err != nil {
		return nil, fmt.Errorf("read legacy configuration: %w", err)
	}
	if err = checkLegacyDBPath(cfg, legacy.DBPath); err != nil {
		return nil, err
	}
	secretKey := cfg.SecretKey
	if secretKey == "" && legacy.SecretKey != "" {
		secretKey = legacy.SecretKey
		logger.Warn("using secret_key from the legacy " + config.LegacyFileName + "; set " + config.EnvSecretKey + " instead and remove the file")
	}

	// 1. Claim the data directory (a single instance runs schedules), open the
	// persistent metadata store and import a legacy state.json once.
	dirLock, err := store.LockDataDir(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("lock data directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = dirLock.Release()
		}
	}()
	keyFile := filepath.Join(cfg.DataDir, secretbox.KeyFileName)
	key, err := secretbox.LoadKey(secretbox.KeySource{Env: secretKey, File: keyFile}, logger)
	if err != nil {
		return nil, fmt.Errorf("load secret key: %w", err)
	}
	box, err := secretbox.New(key.Key)
	if err != nil {
		return nil, fmt.Errorf("load secret key: %w", err)
	}
	metaStore, err := store.OpenSQLite(ctx, cfg.MetadataDBPath(), logger, store.WithSecretBox(box))
	if err != nil {
		if key.Created && errors.Is(err, secretbox.ErrSecretKeyMismatch) {
			// Do not leave a useless new key next to a database it cannot open.
			_ = os.Remove(keyFile)
		}
		return nil, fmt.Errorf("initialize metadata store: %w", err)
	}
	defer func() {
		if err != nil {
			_ = metaStore.Close()
		}
	}()
	if _, err = metaStore.MigrateLegacyState(ctx, filepath.Join(cfg.DataDir, store.LegacyStateFileName)); err != nil {
		return nil, fmt.Errorf("migrate legacy metadata: %w", err)
	}

	// 2. Dashboard-managed settings, storage targets, connections and authentication.
	settingsSvc, err := settings.NewService(ctx, metaStore, settings.WithLogger(logger))
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	targetSvc := targets.NewService(metaStore, storage.NewForTarget, cfg.DataDir, targets.WithLogger(logger))
	connSvc := connections.NewService(metaStore, mongoconn.New(), connections.WithLogger(logger))
	authSvc, err := auth.NewService(metaStore, auth.WithLogger(logger),
		auth.WithSessionPolicy(func() (time.Duration, time.Duration) {
			sec := settingsSvc.Current().Security
			return sec.SessionIdleTimeout.Std(), sec.SessionAbsoluteTimeout.Std()
		}))
	if err != nil {
		return nil, fmt.Errorf("initialize authentication: %w", err)
	}

	imp := &legacyImport{logger: logger, legacy: legacy, settings: settingsSvc, targets: targetSvc,
		connections: connSvc, auth: authSvc, store: metaStore}
	if err = imp.run(ctx); err != nil {
		return nil, fmt.Errorf("import deprecated configuration: %w", err)
	}
	defaultDir, err := cfg.DefaultBackupsDir()
	if err != nil {
		return nil, err
	}
	defaultTarget, created, err := targetSvc.EnsureDefault(ctx, defaultDir)
	if err != nil {
		return nil, fmt.Errorf("create the default storage target: %w", err)
	}
	if created {
		logger.Info("created the default storage target", slog.String("name", defaultTarget.Name), slog.String("path", defaultTarget.Location()))
	}
	if jobs, backups, aerr := metaStore.AssignStorageTarget(ctx, defaultTarget); aerr != nil {
		return nil, fmt.Errorf("assign storage target to existing jobs and backups: %w", aerr)
	} else if jobs > 0 || backups > 0 {
		logger.Info("assigned existing jobs and backups to the default storage target",
			slog.String("storage_target_id", defaultTarget.ID), slog.Int("jobs", jobs), slog.Int("backups", backups))
	}
	if _, err = authSvc.Init(ctx); err != nil {
		return nil, fmt.Errorf("initialize authentication: %w", err)
	}
	if enc := settingsSvc.Encryptor(); enc != nil {
		logger.Info("client-side backup encryption enabled",
			slog.String("mode", string(enc.Mode())),
			slog.Bool("restore_key_configured", settingsSvc.Decryptor() != nil),
		)
	}

	// 3. Engines read their settings and storage target for every run, so changes in
	// the dashboard apply without a restart.
	logToolPaths(logger, cfg.ToolsDir)
	backupEngine := backup.NewEngine(nil, "",
		backup.WithLogger(logger),
		backup.WithToolsDir(cfg.ToolsDir),
		backup.WithStorageResolver(targetSvc.Storage),
		backup.WithRunConfig(func() backup.RunConfig {
			g := settingsSvc.Current().General
			return backup.RunConfig{Encryptor: settingsSvc.Encryptor(), Timeout: g.BackupTimeout.Std(), StallTimeout: g.BackupStallTimeout.Std()}
		}),
	)
	restoreEngine := restore.NewEngine(nil, "",
		restore.WithLogger(logger),
		restore.WithToolsDir(cfg.ToolsDir),
		restore.WithStorageResolver(targetSvc.Storage),
		restore.WithRunConfig(func() restore.RunConfig {
			g := settingsSvc.Current().General
			return restore.RunConfig{Decryptor: settingsSvc.Decryptor(), VerifyPolicy: g.RestoreVerifyPolicy, Timeout: g.RestoreTimeout.Std()}
		}),
	)

	// Background operations started through the API live under the application
	// lifecycle, and share per-database concurrency keys with scheduled runs.
	runManager := runs.NewManager(logger)

	// 4. Initialize observability & notifications: metrics registry, event bus, and the
	// notification service (the metadata store doubles as its repository).
	metricSet := metrics.New(metrics.BuildInfo{Version: o.version, Commit: o.commit, GoVersion: runtime.Version()})
	bus := events.NewBus(events.WithLogger(logger), events.WithDropHook(metricSet.IncEventsDropped))
	notifySvc := notify.NewService(metaStore,
		notify.WithLogger(logger),
		notify.WithObserver(func(t notify.ChannelType, outcome string) {
			metricSet.ObserveNotification(string(t), outcome)
		}),
	)
	bus.Subscribe(metricSet.ObserveEvent)
	bus.Subscribe(notifySvc.HandleEvent)
	if err = checkEncryptionAfterUpgrade(ctx, logger, legacy, settingsSvc, bus); err != nil {
		return nil, fmt.Errorf("check encryption after upgrade: %w", err)
	}

	// 5. Initialize scheduler
	sched := scheduler.NewScheduler(metaStore, backupEngine, nil, logger,
		scheduler.WithPublisher(bus),
		scheduler.WithConnectionResolver(connSvc),
		scheduler.WithStorageTargets(targetSvc),
		scheduler.WithBackupGuard(func(connectionID, database string) (func(), error) {
			return runManager.Acquire(runs.BackupKey(connectionID, database))
		}),
	)
	metricSet.SetScheduledJobsSource(sched.ActiveJobCount)

	// 6. Initialize the embedded web dashboard (only with cfg.Dashboard) & HTTP API.
	// Without it the server has no "/" route and serves only the API, MCP and metrics.
	var subFS fs.FS
	if cfg.Dashboard {
		sub, err := web.GetSubFS()
		if err != nil {
			logger.Warn("failed to isolate embedded static web fs", slog.Any("error", err))
		} else {
			subFS = sub
		}
	}

	// 7. The backup, job and restore use cases are shared by the REST API and the MCP
	// server, so both adapters apply exactly the same rules.
	ops := operations.New(operations.Config{
		Store:       metaStore,
		Backup:      backupEngine,
		Restore:     restoreEngine,
		Jobs:        sched,
		Scheduler:   sched,
		Runs:        runManager,
		Connections: connSvc,
		Targets:     targetSvc,
		Settings:    settingsSvc.Current,
		Publisher:   bus,
		Logger:      logger,
		Version:     o.version,
	})
	auditSvc := audit.NewService(metaStore, logger)
	mcpSrv := mcp.New(mcp.Config{
		Operations:  ops,
		Connections: connSvc,
		Targets:     targetSvc,
		Audit:       auditSvc,
		ObserveCall: metricSet.ObserveMCPCall,
		Version:     o.version,
		Logger:      logger,
	})

	serverOpts := []server.Option{
		server.WithOperations(ops),
		server.WithMCPHandler(mcpSrv.Handler()),
		server.WithAudit(auditSvc),
		server.WithEventPublisher(bus),
		server.WithNotifications(notifySvc),
		server.WithMetricsHandler(metricSet.Handler()),
		server.WithJobDeletedHook(metricSet.ForgetJob),
		server.WithRunManager(runManager),
		server.WithAuth(authSvc),
		server.WithVersion(o.version),
		server.WithConnections(connSvc),
		server.WithSettings(settingsSvc),
		server.WithStorageTargets(targetSvc),
	}
	if o.desktop {
		serverOpts = append(serverOpts, server.WithDesktopCSP())
	}
	srv := server.NewServer(cfg, metaStore, backupEngine, restoreEngine, nil, sched, subFS, logger, serverOpts...)

	return &App{
		cfg:           cfg,
		logger:        logger,
		server:        srv,
		scheduler:     sched,
		bus:           bus,
		notifications: notifySvc,
		metrics:       metricSet,
		runs:          runManager,
		metaStore:     metaStore,
		auth:          authSvc,
		settings:      settingsSvc,
		targets:       targetSvc,
		storeCloser:   metaStore,
		dirLock:       dirLock,
	}, nil
}

// checkLegacyDBPath refuses to start when the removed MONGORESCUE_DB_PATH (or
// database_path) points to an existing database elsewhere: starting with an empty
// database next to it would silently hide every job and backup.
func checkLegacyDBPath(cfg *config.Config, legacyPath string) error {
	if legacyPath == "" {
		return nil
	}
	want, err := filepath.Abs(cfg.MetadataDBPath())
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	got, err := filepath.Abs(legacyPath)
	if err != nil || got == want {
		return nil
	}
	if _, statErr := os.Stat(got); statErr != nil {
		return nil
	}
	return fmt.Errorf("%s is no longer supported and %s exists: move it (with its -wal and -shm files) to %s, then remove the variable",
		config.EnvDBPath, got, want)
}

// Close releases the metadata database and the data directory lock. Run calls it on
// exit after every run and notification has drained; callers that never Run the App
// (or use Start and Stop) must call it themselves, after Stop. When Stop gave up
// waiting for background runs, Close leaves the database open for the process exit
// to release, so no runner writes to a closed database. Close is idempotent.
func (a *App) Close() error {
	if a.runsAbandoned.Load() {
		a.logger.Warn("leaving the metadata database open for abandoned runs; the process exit releases it")
		return nil
	}
	a.closeOnce.Do(func() {
		var errs []error
		if a.storeCloser != nil {
			errs = append(errs, a.storeCloser.Close())
		}
		if a.dirLock != nil {
			errs = append(errs, a.dirLock.Release())
		}
		a.closeErr = errors.Join(errs...)
	})
	return a.closeErr
}

// startBackground runs the event bus and the notification worker pool, each in a
// goroutine owned by the returned stop function. Stop must be called after every
// event producer (HTTP server, scheduler) has stopped: it first drains the bus into
// the notification queue and then drains the notification queue, each bounded by its
// own drain timeout.
func (a *App) startBackground() (stop func()) {
	busCtx, cancelBus := context.WithCancel(context.Background())
	notifyCtx, cancelNotify := context.WithCancel(context.Background())

	var busWG, notifyWG sync.WaitGroup
	notifyWG.Add(1)
	go func() {
		defer notifyWG.Done()
		if err := a.notifications.Run(notifyCtx); err != nil {
			a.logger.Error("notification service stopped with error", slog.Any("error", err))
		}
	}()
	busWG.Add(1)
	go func() {
		defer busWG.Done()
		if err := a.bus.Run(busCtx); err != nil {
			a.logger.Error("event bus stopped with error", slog.Any("error", err))
		}
	}()

	return func() {
		cancelBus()
		busWG.Wait()
		cancelNotify()
		notifyWG.Wait()
		a.logger.Info("event bus and notification workers stopped")
	}
}

// Handler returns the composed HTTP handler (REST API, MCP, metrics and, with
// config.Config.Dashboard, the embedded dashboard) without binding a listener. The
// desktop app serves it in-process to its webview.
func (a *App) Handler() http.Handler {
	return a.server.Handler()
}

// Busy reports whether a backup or restore runs (API-started or scheduled), so an
// embedded host can hold back a restart, such as the desktop app's update.
func (a *App) Busy() bool {
	return len(a.runs.Active()) > 0
}

// SetupCode returns the one-time setup code while no user exists, or "" otherwise.
// Run logs it; embedded hosts (the desktop app) show it to the operator.
func (a *App) SetupCode() string {
	return a.auth.SetupCode()
}

// ActiveRuns returns the concurrency keys (runs.BackupKey, runs.RestoreKey) of the
// backups and restores in progress, sorted: manual, job-triggered and scheduled
// runs alike.
func (a *App) ActiveRuns() []string {
	return a.runs.Active()
}

// PauseRuns keeps new backups and restores from starting until ResumeRuns: the
// scheduler skips its triggers, and runs started from the dashboard, the API or
// MCP are refused with runs.ErrShuttingDown (operations.ErrShuttingDown, "MongoRescue
// is shutting down", 503). Runs in progress continue. The desktop app pauses runs
// while it waits for the running ones to finish before it quits.
func (a *App) PauseRuns() {
	a.runs.Refuse()
	a.scheduler.Pause()
}

// ResumeRuns lets backups and restores start again after PauseRuns.
func (a *App) ResumeRuns() {
	a.scheduler.Resume()
	a.runs.Accept()
}

// forceStopPersistTimeout bounds the writes ForceStop makes after the runs stopped.
const forceStopPersistTimeout = 5 * time.Second

// ForceStop is Stop for a forced quit: the backups and restores still in progress
// are cancelled like in Stop, and each one is then recorded as failed with reason
// in front of the engine's error message, as in "reason (backup cancelled: context
// canceled)", and logged, so the history says why it did not finish.
// Like Stop it is idempotent and a no-op before Start; call Close afterwards.
func (a *App) ForceStop(reason string) {
	a.lifeMu.Lock()
	defer a.lifeMu.Unlock()
	if !a.started || a.stopped {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), forceStopPersistTimeout)
	defer cancel()
	backups, restores := a.inProgressRuns(ctx)
	a.stopLocked()
	if len(backups) == 0 && len(restores) == 0 {
		return
	}
	if a.runsAbandoned.Load() {
		a.logger.Warn("runs did not stop in time; the next start records them as interrupted",
			slog.Int("backups", len(backups)), slog.Int("restores", len(restores)))
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), forceStopPersistTimeout)
	defer cancel()
	for _, id := range backups {
		b, err := a.metaStore.GetBackupRecord(ctx, id)
		if err != nil || (b.Status != models.StatusFailed && b.Status != models.StatusInProgress) {
			continue
		}
		b.Status, b.ErrorMessage = models.StatusFailed, withReason(reason, b.ErrorMessage)
		if err = a.metaStore.SaveBackupRecord(ctx, b); err != nil {
			a.logger.Warn("failed to record the cancelled backup", slog.String("backup_id", id), slog.Any("error", err))
			continue
		}
		a.logger.Warn("backup cancelled", slog.String("backup_id", id), slog.String("database", b.Database), slog.String("reason", reason))
	}
	for _, id := range restores {
		r, err := a.metaStore.GetRestoreRecord(ctx, id)
		if err != nil || (r.Status != models.RestoreStatusFailed && r.Status != models.RestoreStatusInProgress) {
			continue
		}
		r.Status, r.ErrorMessage = models.RestoreStatusFailed, withReason(reason, r.ErrorMessage)
		if err = a.metaStore.SaveRestoreRecord(ctx, r); err != nil {
			a.logger.Warn("failed to record the cancelled restore", slog.String("restore_id", id), slog.Any("error", err))
			continue
		}
		a.logger.Warn("restore cancelled", slog.String("restore_id", id), slog.String("target_db", r.TargetDatabase), slog.String("reason", reason))
	}
}

// withReason puts reason in front of a run's error message: "reason (detail)".
func withReason(reason, detail string) string {
	switch {
	case detail == "" || detail == reason:
		return reason
	case strings.HasPrefix(detail, reason+" ("):
		return detail
	}
	return reason + " (" + detail + ")"
}

// inProgressRuns returns the IDs of the backup and restore records in progress.
func (a *App) inProgressRuns(ctx context.Context) (backups, restores []string) {
	if list, err := a.metaStore.ListBackupRecords(ctx, ""); err == nil {
		for _, b := range list {
			if b.Status == models.StatusInProgress {
				backups = append(backups, b.ID)
			}
		}
	}
	if list, err := a.metaStore.ListRestoreRecords(ctx); err == nil {
		for _, r := range list {
			if r.Status == models.RestoreStatusInProgress {
				restores = append(restores, r.ID)
			}
		}
	}
	return backups, restores
}

// Start runs the background work of the App without an HTTP listener: it removes
// stale mongo tools config files, marks runs interrupted by a previous process as
// failed, starts the event bus and notification workers and starts the scheduler
// with ctx. Run calls it before starting the HTTP server; embedded hosts call it
// directly and serve Handler themselves. Every goroutine started here is stopped by
// Stop. Start may be called once; it returns ErrStarted afterwards.
func (a *App) Start(ctx context.Context) error {
	a.lifeMu.Lock()
	defer a.lifeMu.Unlock()
	if a.started || a.stopped {
		return ErrStarted
	}
	a.started = true

	// Purge credential-bearing tools config files left behind by a previous crash.
	if removed, err := mongotools.CleanupStale("", staleToolsConfigAge); err != nil {
		a.logger.Warn("failed to clean up stale mongo tools config files",
			slog.Int("removed", removed),
			slog.Any("error", err),
		)
	} else {
		a.logger.Info("stale mongo tools config cleanup completed", slog.Int("removed", removed))
	}

	a.failInterruptedRuns(ctx)

	// Start the event bus and notification workers. Stop drains them after the
	// producers (scheduler, runs) have stopped.
	a.stopBackground = a.startBackground()

	// Start background cron scheduler. It is stopped by shutdownRuns.
	if err := a.scheduler.Start(ctx); err != nil {
		a.stopLocked()
		return fmt.Errorf("start scheduler: %w", err)
	}
	return nil
}

// Stop stops what Start started: it cancels and awaits in-flight backups and restores
// and the scheduler (force-killing mongo tools after the shutdown deadline), then
// drains the event bus and the notification queue. It does not stop an HTTP server
// and does not release the database: call Close afterwards. Stop is idempotent and a
// no-op before Start.
func (a *App) Stop() {
	a.lifeMu.Lock()
	defer a.lifeMu.Unlock()
	a.stopLocked()
}

// stopLocked implements Stop; a.lifeMu must be held.
func (a *App) stopLocked() {
	if !a.started || a.stopped {
		return
	}
	a.stopped = true
	// Producers stop before the event-bus drain: cancel and await backups and
	// restores so no mongodump/mongorestore outlives the process.
	a.shutdownRuns()
	if a.stopBackground != nil {
		a.stopBackground()
		a.stopBackground = nil
	}
}

// Run starts the background work (see Start), boots the HTTP server, and waits for
// termination signals.
func (a *App) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Deferred first so it runs last: after runs are awaited and the notification
	// queue (which records delivery status) has drained.
	defer func() {
		if err := a.Close(); err != nil {
			a.logger.Error("failed to close metadata store", slog.Any("error", err))
		}
	}()

	if sec := a.settings.Current().Security; !isLoopbackHost(a.cfg.Host) && sec.SecureCookies != settings.CookiesAlways && !sec.TrustProxyHeaders {
		a.logger.Warn("listening on a non-loopback address over plain HTTP; put a TLS-terminating reverse proxy in front",
			slog.String("host", a.cfg.Host),
		)
	}

	if err := a.Start(ctx); err != nil {
		return err
	}
	// Stops runs, the scheduler and then the background workers, after the HTTP
	// server has stopped producing events.
	defer a.Stop()

	// Start HTTP server in a managed goroutine
	serverErrChan := make(chan error, 1)
	go func() {
		serverErrChan <- a.server.Start()
	}()

	if code := a.auth.SetupCode(); code != "" {
		if a.cfg.Dashboard {
			url := a.server.SetupURL()
			a.logger.Warn("Setup required: open "+url+" and enter setup code "+code,
				slog.String("url", url), slog.String("setup_code", code))
		} else {
			a.logger.Warn("Setup required: create the first user with POST /api/v1/setup and setup code "+code,
				slog.String("setup_code", code))
		}
	}

	baseURL := fmt.Sprintf("http://localhost:%d", a.cfg.Port)
	readyAttrs := []any{slog.String("api_url", baseURL)}
	if a.cfg.Dashboard {
		readyAttrs = []any{slog.String("dashboard_url", baseURL)}
	}
	if def, err := a.targets.Resolve(ctx, ""); err == nil {
		readyAttrs = append(readyAttrs, slog.String("default_storage_target", def.Name), slog.String("storage_type", string(def.Type)))
	}
	a.logger.Info("mongorescue ready and accepting connections", readyAttrs...)

	// Block until signal or server error
	var runErr error
	select {
	case <-ctx.Done():
		a.logger.Info("shutdown signal received, initiating graceful exit...")
		drainCtx, cancel := context.WithTimeout(context.Background(), httpDrainTimeout)
		runErr = a.server.Shutdown(drainCtx)
		cancel()
	case runErr = <-serverErrChan:
	}
	return runErr
}

// shutdownRuns cancels every in-flight operation (API-started runs and scheduled
// jobs) and waits for them within shutdownTimeout. Tool processes still alive after
// the deadline are killed with their process groups.
func (a *App) shutdownRuns() {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := a.runs.Shutdown(context.Background()); err != nil {
				a.logger.Warn("background operations did not stop cleanly", slog.Any("error", err))
			}
		}()
		go func() {
			defer wg.Done()
			a.scheduler.Stop()
		}()
		wg.Wait()
	}()

	select {
	case <-done:
		a.logger.Info("in-flight backups and restores stopped")
		return
	case <-ctx.Done():
	}

	killed := mongotools.KillAll()
	a.logger.Warn("shutdown deadline exceeded, force-killed remaining mongo tool processes",
		slog.Int("processes", killed),
		slog.Duration("deadline", shutdownTimeout),
	)
	select {
	case <-done:
	case <-time.After(forceKillGrace):
		a.runsAbandoned.Store(true)
		a.logger.Error("background operations still running after force kill; exiting anyway",
			slog.Any("abandoned_runs", a.runs.Active()),
			slog.Duration("grace", forceKillGrace),
		)
	}
}

// failInterruptedRuns marks backup and restore records left in progress by a previous
// process (crash, SIGKILL) as failed, since nothing will ever complete them.
func (a *App) failInterruptedRuns(ctx context.Context) {
	const msg = "interrupted: the server stopped before this run finished"
	if backups, err := a.metaStore.ListBackupRecords(ctx, ""); err == nil {
		for _, b := range backups {
			if b.Status == models.StatusInProgress {
				b.Status, b.ErrorMessage, b.SizeBytes, b.SHA256 = models.StatusFailed, msg, 0, ""
				if err := a.metaStore.SaveBackupRecord(ctx, b); err != nil {
					a.logger.Warn("failed to mark interrupted backup", slog.String("backup_id", b.ID), slog.Any("error", err))
				}
			}
		}
	}
	if restores, err := a.metaStore.ListRestoreRecords(ctx); err == nil {
		for _, r := range restores {
			if r.Status == models.RestoreStatusInProgress {
				r.Status, r.ErrorMessage = models.RestoreStatusFailed, msg
				if err := a.metaStore.SaveRestoreRecord(ctx, r); err != nil {
					a.logger.Warn("failed to mark interrupted restore", slog.String("restore_id", r.ID), slog.Any("error", err))
				}
			}
		}
	}
}

// isLoopbackHost reports whether host binds only to the local loopback interface.
// An empty host and unspecified addresses (0.0.0.0, ::) bind to all interfaces and
// are therefore treated as non-loopback.
func isLoopbackHost(host string) bool {
	h := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// logToolPaths logs where mongodump and mongorestore were found, or warns with the
// searched locations when one is missing, so a missing install is visible at startup
// rather than at the first backup.
func logToolPaths(logger *slog.Logger, toolsDir string) {
	resolver := mongotools.NewResolver(toolsDir)
	for _, tool := range []string{"mongodump", "mongorestore"} {
		path, err := resolver.Resolve(tool)
		if err != nil {
			mongotools.LogNotFound(context.Background(), logger, err)
			continue
		}
		logger.Info("MongoDB Database Tools binary found", slog.String("tool", tool), slog.String("path", path))
	}
}

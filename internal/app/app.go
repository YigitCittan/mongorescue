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
	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/auth/oidc"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/heartbeat"
	"github.com/yigitcittan/mongorescue/internal/integrity"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/mcp"
	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/metrics"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/recoverykit"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runlog"
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

// RunLogDirName is the directory under the data directory that holds the log of
// every backup and restore run (<run-id>.log).
const RunLogDirName = "logs"

// runLogPruneInterval is how often run logs older than general.log_retention_days
// are deleted.
const runLogPruneInterval = time.Hour

// auditPruneInterval is how often audit log entries older than audit.retention_days
// are deleted.
const auditPruneInterval = time.Hour

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
	registry      *runs.Registry
	ops           *operations.Service
	metaStore     store.Store
	auth          *auth.Service
	settings      *settings.Service
	targets       *targets.Service
	integrity     *integrity.Service
	metaBackup    *metabackup.Service
	readiness     *readiness.Service
	auditLog      *auditlog.Service
	auditForward  *auditlog.Forwarder
	heartbeat     *heartbeat.Service

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
	prober := mongoconn.New()
	connSvc := connections.NewService(metaStore, prober, connections.WithLogger(logger))
	importedKeySecret, err := secretbox.DeriveSubkey(key.Key, auth.ImportedKeySubkeyPurpose)
	if err != nil {
		return nil, fmt.Errorf("initialize authentication: %w", err)
	}
	authSvc, err := auth.NewService(metaStore, auth.WithLogger(logger),
		auth.WithImportedKeySecret(importedKeySecret),
		auth.WithSessionPolicy(func() (time.Duration, time.Duration) {
			sec := settingsSvc.Current().Security
			return sec.SessionIdleTimeout.Std(), sec.SessionAbsoluteTimeout.Std()
		}),
		auth.WithOIDCPolicy(func() auth.OIDCPolicy { return oidcPolicy(settingsSvc.Current().OIDC, o.desktop) }))
	if err != nil {
		return nil, fmt.Errorf("initialize authentication: %w", err)
	}
	// Single sign-on: every provider request goes through the guarded notification
	// client (timeout, no redirects, no link-local or metadata addresses), and the
	// flow cookie is sealed with its own subkey of secret.key. The desktop app has no
	// single sign-on.
	oidcClient := oidc.NewClient(notify.NewHTTPClient(), oidc.WithClock(authSvc.Now))
	oidcFlowKey, err := secretbox.DeriveSubkey(key.Key, oidc.FlowSubkeyPurpose)
	if err != nil {
		return nil, fmt.Errorf("initialize single sign-on: %w", err)
	}
	oidcFlowBox, err := secretbox.New(oidcFlowKey)
	if err != nil {
		return nil, fmt.Errorf("initialize single sign-on: %w", err)
	}
	settingsSvc.SetOIDCGuard(&oidcGuard{auth: authSvc, client: oidcClient, desktop: o.desktop})

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
		backup.WithCollectionLister(collectionLister(prober)),
		backup.WithManifestCapturer(prober.Manifest),
		backup.WithRunConfig(func() backup.RunConfig {
			cur := settingsSvc.Current()
			g := cur.General
			cfg := backup.RunConfig{Encryptor: settingsSvc.Encryptor(), Timeout: g.BackupTimeout.Std(), StallTimeout: g.BackupStallTimeout.Std(),
				Verify: cur.Integrity.VerifyAfterBackup}
			if cur.Integrity.VerifyDecrypt {
				cfg.VerifyDecryptor = settingsSvc.Decryptor()
			}
			return cfg
		}),
	)
	restoreEngine := restore.NewEngine(nil, "",
		restore.WithLogger(logger),
		restore.WithToolsDir(cfg.ToolsDir),
		restore.WithStorageResolver(targetSvc.Storage),
		restore.WithValidationBypassCheck(prober.CanBypassDocumentValidation),
		restore.WithDatabaseAdmin(prober),
		restore.WithRunConfig(func() restore.RunConfig {
			g := settingsSvc.Current().General
			return restore.RunConfig{Decryptor: settingsSvc.Decryptor(), VerifyPolicy: g.RestoreVerifyPolicy, Timeout: g.RestoreTimeout.Std()}
		}),
	)

	// Background operations started through the API live under the application
	// lifecycle, and share per-database concurrency keys with scheduled runs.
	runManager := runs.NewManager(logger)
	// Every run (API, MCP or cron) is tracked for cancellation, live progress and its
	// log file under <datadir>/logs.
	registry := runs.NewRegistry(
		runs.WithLogs(runlog.NewDir(filepath.Join(cfg.DataDir, RunLogDirName))),
		runs.WithRegistryLogger(logger),
	)

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
	metricSet.SetActiveRunsSource(func(kind string) int { return registry.Count(models.RunKind(kind)) })
	bus.Subscribe(metricSet.ObserveEvent)
	bus.Subscribe(notifySvc.HandleEvent)
	bus.Subscribe(watchEncryptionOffAlert(logger, settingsSvc))
	if err = checkEncryptionAfterUpgrade(ctx, logger, legacy, settingsSvc, bus); err != nil {
		return nil, fmt.Errorf("check encryption after upgrade: %w", err)
	}

	// Integrity: on-demand and swept archive verification, restore tests after
	// scheduled backups and weekly storage scans.
	// The audit log of every action (hash-chained, pruned by audit.retention_days,
	// optionally forwarded to a webhook). MCP tool calls and system actions recorded
	// in the API key activity log are mirrored into it.
	auditForwarder := auditlog.NewForwarder(auditlog.ForwarderConfig{
		Endpoint: func() auditlog.Endpoint {
			a := settingsSvc.Current().Audit
			return auditlog.Endpoint{URL: a.WebhookURL, Secret: a.WebhookSecret}
		},
		Observe: metricSet.ObserveAuditForward,
		Logger:  logger,
	})
	auditLog := auditlog.New(auditlog.Config{
		Repo:           metaStore,
		RetentionDays:  func() int { return settingsSvc.Current().Audit.RetentionDays },
		Forwarder:      auditForwarder,
		PruneInterval:  auditPruneInterval,
		OnWriteFailure: metricSet.IncAuditWriteFailures,
		OnSyncWrite:    metricSet.IncAuditSyncWrites,
		Logger:         logger,
	})
	metricSet.SetAuditQueueSource(auditLog.QueueDepth)
	auditSvc := audit.NewService(metaStore, logger, audit.WithObserver(auditLog.Mirror()))
	integritySvc := integrity.New(integrity.Config{
		Store:       metaStore,
		Targets:     targetSvc,
		Runs:        runManager,
		Restore:     restoreEngine,
		Admin:       prober,
		Connections: connSvc,
		Settings:    settingsSvc.Current,
		Decryptor:   settingsSvc.Decryptor,
		Publisher:   bus,
		ObserveScan: metricSet.ObserveStorageScan,
		Logger:      logger,
	})

	// Scheduled snapshots of the metadata database and the recovery kit, which
	// carries secret.key and points to the latest snapshot.
	installID, err := metabackup.InstallID(key.Key)
	if err != nil {
		return nil, fmt.Errorf("initialize metadata backups: %w", err)
	}
	metaBackupSvc := metabackup.New(metabackup.Config{
		InstallID: installID,
		Store:     metaStore,
		Targets:   targetSvc,
		DataDir:   cfg.DataDir,
		Settings:  settingsSvc.Current,
		Encryptor: settingsSvc.Encryptor,
		Publisher: bus,
		Observe:   metricSet.ObserveMetadataBackup,
		Logger:    logger,
	})
	kitSvc, err := recoverykit.New(recoverykit.Config{
		SecretKey:        key.Key,
		SecretKeyFromEnv: key.FromEnv,
		Settings:         settingsSvc,
		Targets:          targetSvc,
		LatestSnapshot:   metaBackupSvc.Latest,
		MetadataPrefix:   metaBackupSvc.Prefix(),
		Version:          o.version,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize recovery kit: %w", err)
	}
	if err = kitSvc.Refresh(ctx); err != nil {
		logger.Warn("could not check whether the recovery kit is current", logsafe.Error(err))
	}

	// Recovery readiness: the RPO checker (job.rpo_missed / job.rpo_recovered, the
	// job_rpo_* gauges) and the per-database readiness report. A finished backup
	// asks for an early check.
	readinessSvc := readiness.New(readiness.Config{
		Store:        metaStore,
		Connections:  connSvc,
		KeysEscrowed: func() bool { return settingsSvc.RecoveryKitStatus().UpToDate },
		Publisher:    bus,
		Observe: func(started time.Time, samples []readiness.Sample) {
			out := make([]metrics.RPOSample, len(samples))
			for i, s := range samples {
				out[i] = metrics.RPOSample{JobID: s.JobID, Database: s.Database, Since: s.Since, Target: s.Target}
			}
			metricSet.SetRPOSamples(started, out)
		},
		Logger: logger,
	})
	bus.Subscribe(readinessSvc.HandleEvent)

	// The outbound heartbeat: the global ping while the scheduler is healthy and the
	// per-job start/success/fail pings. sched is assigned below, before Start runs
	// the service.
	var sched *scheduler.Scheduler
	heartbeatSvc := heartbeat.New(heartbeat.Config{
		Global: func() (string, time.Duration) {
			m := settingsSvc.Current().Monitoring
			return m.HeartbeatURL, m.HeartbeatInterval.Std()
		},
		Healthy: func() bool { return !sched.Stale() },
		OnDrop:  metricSet.IncHeartbeatDropped,
		Logger:  logger,
	})

	// 5. Initialize scheduler
	sched = scheduler.NewScheduler(metaStore, backupEngine, nil, logger,
		scheduler.WithPublisher(bus),
		scheduler.WithRunObserver(heartbeatSvc),
		scheduler.WithConnectionResolver(connSvc),
		scheduler.WithStorageTargets(targetSvc),
		scheduler.WithRunRegistry(registry),
		scheduler.WithBackupGuard(func(connectionID, database string) (func(), error) {
			return runManager.Acquire(runs.BackupKey(connectionID, database))
		}),
		scheduler.WithRetentionLog(metaStore),
		scheduler.WithAuditor(auditSvc),
		scheduler.WithAfterBackup(integritySvc.AfterBackup),
		scheduler.WithAfterRun(integritySvc.AfterRun),
		// Job edits, pauses and resumes re-evaluate the RPOs at once.
		scheduler.WithJobChanged(func(string) { readinessSvc.Kick() }),
		// Multi-database jobs resolve their selection against the connection's
		// databases (system ones included; selections exclude them themselves).
		scheduler.WithDatabaseLister(func(ctx context.Context, connectionID string) ([]string, error) {
			dbs, err := connSvc.Databases(ctx, connectionID, true)
			if err != nil {
				return nil, err
			}
			names := make([]string, len(dbs))
			for i, d := range dbs {
				names[i] = d.Name
			}
			return names, nil
		}),
	)
	metricSet.SetScheduledJobsSource(sched.ActiveJobCount)
	metricSet.SetSchedulerTickSource(sched.LastTick)
	metricSet.SetSettingsWarningsSource(func() int { return len(settingsSvc.Warnings()) })

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
		Registry:    registry,
		Connections: connSvc,
		Targets:     targetSvc,
		Storage:     targetSvc.Storage,
		Settings:    settingsSvc.Current,
		Publisher:   bus,
		Verifier:    integritySvc,
		Inspector:   prober,
		Audit:       auditSvc,
		Logger:      logger,
		Version:     o.version,
		// Deleted jobs (single or bulk) drop their metric series and are no longer
		// checked.
		OnJobDeleted: func(jobID string) {
			metricSet.ForgetJob(jobID)
			readinessSvc.Kick()
		},
	})
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
		server.WithAuditLog(auditLog),
		server.WithEventPublisher(bus),
		server.WithNotifications(notifySvc),
		server.WithMetricsHandler(metricSet.Handler()),
		server.WithRunManager(runManager),
		server.WithRunRegistry(registry),
		server.WithAuth(authSvc),
		server.WithVersion(o.version),
		server.WithConnections(connSvc),
		server.WithSettings(settingsSvc),
		server.WithStorageTargets(targetSvc),
		server.WithIntegrity(integritySvc),
		server.WithMetadataBackup(metaBackupSvc),
		server.WithRecoveryKit(kitSvc),
		server.WithReadiness(readinessSvc),
		server.WithHeartbeat(heartbeatSvc),
	}
	if o.desktop {
		serverOpts = append(serverOpts, server.WithDesktopCSP())
	} else {
		serverOpts = append(serverOpts, server.WithOIDC(oidcClient, oidcFlowBox))
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
		registry:      registry,
		ops:           ops,
		metaStore:     metaStore,
		auth:          authSvc,
		settings:      settingsSvc,
		targets:       targetSvc,
		integrity:     integritySvc,
		metaBackup:    metaBackupSvc,
		readiness:     readinessSvc,
		auditLog:      auditLog,
		auditForward:  auditForwarder,
		heartbeat:     heartbeatSvc,
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

	// Run logs older than general.log_retention_days are pruned at start and hourly.
	pruneCtx, cancelPrune := context.WithCancel(context.Background())
	var pruneWG sync.WaitGroup
	pruneWG.Add(1)
	go func() {
		defer pruneWG.Done()
		a.pruneRunLogs(pruneCtx, runLogPruneInterval)
	}()
	// The audit log writer (coalescing flushes and retention included), and the
	// worker forwarding stored entries to the webhook. The writer stops first, so
	// that the entries it drains are still forwarded.
	auditCtx, cancelAudit := context.WithCancel(context.Background())
	forwardCtx, cancelForward := context.WithCancel(context.Background())
	var auditWG, forwardWG sync.WaitGroup
	auditWG.Go(func() { a.auditLog.Run(auditCtx) })
	forwardWG.Go(func() { a.auditForward.Run(forwardCtx) })
	// The heartbeat stops after the runs (Stop calls this after shutdownRuns), so the
	// /fail pings of runs cancelled by the shutdown are still sent.
	heartbeatCtx, cancelHeartbeat := context.WithCancel(context.Background())
	var heartbeatWG sync.WaitGroup
	if a.heartbeat != nil {
		heartbeatWG.Go(func() { a.heartbeat.Run(heartbeatCtx) })
	}

	return func() {
		cancelHeartbeat()
		heartbeatWG.Wait()
		cancelPrune()
		pruneWG.Wait()
		cancelAudit()
		auditWG.Wait()
		cancelForward()
		forwardWG.Wait()
		cancelBus()
		busWG.Wait()
		cancelNotify()
		notifyWG.Wait()
		a.logger.Info("event bus and notification workers stopped")
	}
}

// pruneRunLogs deletes expired run logs now and then every interval until ctx ends.
func (a *App) pruneRunLogs(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if a.ops != nil {
			if n, err := a.ops.PruneRunLogs(); err != nil {
				a.logger.Warn("failed to prune run logs", slog.Int("removed", n), slog.Any("error", err))
			} else if n > 0 {
				a.logger.Info("expired run logs removed", slog.Int("removed", n))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
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
// are cancelled through the run registry with reason (by runs.SystemActor), and each
// run this cancellation stopped is recorded as cancelled, with reason in front of the
// engine's message, as in "reason (backup cancelled: reason)", and logged, so the
// history says why it did not finish. Runs that ended on their own (failed or
// completed), that were cancelled before or that were already finishing keep their
// outcome. Like Stop it is idempotent and a no-op before Start; call Close afterwards.
func (a *App) ForceStop(reason string) {
	a.lifeMu.Lock()
	defer a.lifeMu.Unlock()
	if !a.started || a.stopped {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), forceStopPersistTimeout)
	defer cancel()
	backups, restores := a.inProgressRuns(ctx)
	cancelledAt := time.Now().UTC()
	stopped := make(map[string]bool)
	for _, id := range a.registry.CancelAll(runs.Cancellation{By: runs.SystemActor, Kind: runs.ActorSystem, Reason: reason, At: cancelledAt}) {
		stopped[id] = true
	}
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
		if !stopped[id] {
			continue
		}
		b, err := a.metaStore.GetBackupRecord(ctx, id)
		if err != nil || (b.Status != models.StatusInProgress && b.Status != models.StatusCancelled) {
			continue
		}
		b.Status, b.ErrorMessage = models.StatusCancelled, withReason(reason, b.ErrorMessage)
		if b.CancelledBy == "" {
			b.CancelledBy, b.CancelledAt = runs.SystemActor, models.Stamp(cancelledAt)
		}
		b.SizeBytes, b.SHA256 = 0, ""
		if err = a.metaStore.SaveBackupRecord(ctx, b); err != nil {
			a.logger.Warn("failed to record the cancelled backup", slog.String("backup_id", id), slog.Any("error", err))
			continue
		}
		a.logger.Warn("backup cancelled", slog.String("backup_id", id), slog.String("database", b.Database), slog.String("reason", reason))
	}
	for _, id := range restores {
		if !stopped[id] {
			continue
		}
		r, err := a.metaStore.GetRestoreRecord(ctx, id)
		if err != nil || (r.Status != models.RestoreStatusInProgress && r.Status != models.RestoreStatusCancelled) {
			continue
		}
		r.Status, r.ErrorMessage = models.RestoreStatusCancelled, withReason(reason, r.ErrorMessage)
		if r.CancelledBy == "" {
			r.CancelledBy, r.CancelledAt = runs.SystemActor, models.Stamp(cancelledAt)
		}
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
	if page, err := a.metaStore.QueryBackupRecords(ctx, store.BackupFilter{Status: models.StatusInProgress}); err == nil {
		for _, row := range page.Rows {
			backups = append(backups, row.Record.ID)
		}
	}
	if page, err := a.metaStore.QueryRestoreRecords(ctx, store.RestoreFilter{Status: models.RestoreStatusInProgress}); err == nil {
		for _, r := range page.Records {
			restores = append(restores, r.ID)
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
	// Background integrity sweeps and storage scans; stopped by shutdownRuns.
	if a.integrity != nil {
		a.integrity.Start(context.WithoutCancel(ctx))
	}
	// Scheduled snapshots of the metadata database; stopped by shutdownRuns.
	if a.metaBackup != nil {
		a.metaBackup.Start(context.WithoutCancel(ctx))
	}
	// The RPO checker; stopped by shutdownRuns.
	if a.readiness != nil {
		a.readiness.Start(context.WithoutCancel(ctx))
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
		wg.Add(5)
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
		go func() {
			defer wg.Done()
			if a.integrity != nil {
				a.integrity.Stop()
			}
		}()
		go func() {
			defer wg.Done()
			if a.metaBackup != nil {
				a.metaBackup.Stop()
			}
		}()
		go func() {
			defer wg.Done()
			if a.readiness != nil {
				a.readiness.Stop()
			}
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
					a.logger.Warn("failed to mark interrupted backup", slog.String("backup_id", b.ID), logsafe.Error(err))
				}
			}
		}
	}
	if restores, err := a.metaStore.ListRestoreRecords(ctx); err == nil {
		for _, r := range restores {
			if r.Status == models.RestoreStatusInProgress {
				r.Status, r.ErrorMessage = models.RestoreStatusFailed, msg
				if err := a.metaStore.SaveRestoreRecord(ctx, r); err != nil {
					a.logger.Warn("failed to mark interrupted restore", slog.String("restore_id", r.ID), logsafe.Error(err))
				}
			}
		}
	}
	// Job runs end with their databases. Each database's outcome is rebuilt from the
	// backup records of the run (marked above): completed ones stay completed, those
	// still waiting or running failed with the interruption.
	if jobRuns, err := a.metaStore.ListRunningJobRuns(ctx); err == nil {
		for _, run := range jobRuns {
			records := map[string]*models.BackupRecord{}
			if page, qerr := a.metaStore.QueryBackupRecords(ctx, store.BackupFilter{RunID: run.ID}); qerr == nil {
				for _, row := range page.Rows {
					records[row.Record.ID] = row.Record
				}
			}
			for i := range run.Databases {
				d := &run.Databases[i]
				if rec := records[d.BackupID]; rec != nil && d.BackupID != "" {
					d.Status, d.Error = rec.Status, ""
					if rec.Status != models.StatusCompleted {
						d.Error = rec.ErrorMessage
					}
				}
				if d.Status == models.StatusInProgress || d.Status == models.StatusPending {
					d.Status, d.Error = models.StatusFailed, msg
				}
			}
			run.Error = msg
			run.Finish(time.Now())
			if err := a.metaStore.SaveJobRun(ctx, run); err != nil {
				a.logger.Warn("failed to mark interrupted job run", slog.String("run_id", run.ID), logsafe.Error(err))
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

// collectionLister adapts the driver adapter to backup.CollectionLister, so backups
// of several collections can be expressed as exclusions.
func collectionLister(prober connections.Prober) backup.CollectionLister {
	return func(ctx context.Context, uri, database string) ([]string, error) {
		cols, err := prober.ListCollections(ctx, uri, database)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(cols))
		for _, c := range cols {
			names = append(names, c.Name)
		}
		return names, nil
	}
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

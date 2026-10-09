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
	"github.com/yigitcittan/mongorescue/internal/copies"
	"github.com/yigitcittan/mongorescue/internal/diskguard"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/heartbeat"
	"github.com/yigitcittan/mongorescue/internal/integrity"
	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/mcp"
	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/metrics"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/recoverykit"
	"github.com/yigitcittan/mongorescue/internal/reencrypt"
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

// ToolsTempDirName is the private (0700) directory under the data directory that
// holds the short-lived files passing connection URIs and TLS material to
// mongodump and mongorestore (see mongotools.WriteConfig) when neither
// MONGORESCUE_TMP_DIR nor a writable system temporary directory is available.
const ToolsTempDirName = "tmp"

// toolsTempDirPerm restricts ToolsTempDirName to the current user.
const toolsTempDirPerm = 0o700

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
	cfg *config.Config
	// toolsTemp is the directory of the Database Tools' temporary files
	// (toolsTempDir).
	toolsTemp     string
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
	copies        *copies.Service
	reencrypt     *reencrypt.Service
	pitr          *collector.Service
	// cleanupPITR drops the recorded clones of an interrupted point-in-time restore
	// or chain test (operations.Service.CleanupInterruptedPITR).
	cleanupPITR  func(ctx context.Context, rec *models.RestoreRecord) string
	readiness    *readiness.Service
	auditLog     *auditlog.Service
	auditForward *auditlog.Forwarder
	heartbeat    *heartbeat.Service
	diskGuard    *diskguard.Guard

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
	// keyrotation.Open settles a secret key rotation that a crash interrupted; the
	// key it returns is the one the database is sealed with (secret.key again).
	keyFiles := keyrotation.FilesIn(cfg.DataDir)
	opened, err := keyrotation.Open(ctx, keyFiles, key.Key, key.FromEnv,
		func(ctx context.Context, box *secretbox.Box) (*store.SQLiteStore, error) {
			return store.OpenSQLite(ctx, cfg.MetadataDBPath(), logger, store.WithSecretBox(box))
		}, logger)
	var metaStore *store.SQLiteStore
	if opened != nil {
		metaStore, key.Key = opened.Store, opened.Key
	}
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
	// Every driver reads the live upload stall timeout at the start of each upload.
	storageFactory := storage.TargetFactory(func() time.Duration { return settingsSvc.Current().General.StorageStallTimeout.Std() })
	targetSvc := targets.NewService(metaStore, storageFactory, cfg.DataDir, targets.WithLogger(logger))
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
	oidcFlowRef := secretbox.NewRef(oidcFlowBox)
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
	toolsTemp, err := toolsTempDir(cfg)
	if err != nil {
		return nil, err
	}
	logger.Info("temporary files for the database tools", slog.String("dir", toolsTemp))
	// Background operations started through the API live under the application
	// lifecycle, and share per-database concurrency keys with scheduled runs; its
	// slots hold every connection's max_concurrent_backups.
	runManager := runs.NewManager(logger)
	sizeEstimator := archiveSizeEstimator(metaStore, prober.DatabaseSize)
	// The copy queue is built below (it needs the event bus); synchronous copies
	// only run once the app has started.
	var copySvc *copies.Service
	backupEngine := backup.NewEngine(nil, "",
		backup.WithLogger(logger),
		backup.WithCopier(func(ctx context.Context, rec *models.BackupRecord, mbps float64) error {
			return copySvc.CopyAll(ctx, rec, mbps)
		}),
		backup.WithToolsDir(cfg.ToolsDir),
		backup.WithConfigDir(toolsTemp),
		backup.WithStorageResolver(targetSvc.Storage),
		backup.WithCollectionLister(collectionLister(prober)),
		backup.WithManifestCapturer(prober.Manifest),
		backup.WithDatabaseLister(backup.DatabaseListFunc(databaseNames(prober))),
		backup.WithMemberProbe(prober.ServingMember),
		backup.WithConnectionSlots(runManager),
		backup.WithOpTimeReader(prober.WriteOpTimes),
		backup.WithSizeEstimator(sizeEstimator),
		backup.WithRunConfig(func() backup.RunConfig {
			cur := settingsSvc.Current()
			g := cur.General
			cfg := backup.RunConfig{Encryptor: settingsSvc.Encryptor(), Timeout: g.BackupTimeout.Std(), StallTimeout: g.BackupStallTimeout.Std(),
				Verify: cur.Integrity.VerifyAfterBackup, MaxUploadMbps: g.MaxUploadMbps}
			if cur.Integrity.VerifyDecrypt {
				cfg.VerifyDecryptor = settingsSvc.Decryptor()
			}
			return cfg
		}),
	)
	// The audit log (built in step 4) records every post-restore command.
	var auditLog *auditlog.Service
	restoreEngine := restore.NewEngine(nil, "",
		restore.WithLogger(logger),
		restore.WithToolsDir(cfg.ToolsDir),
		restore.WithConfigDir(toolsTemp),
		restore.WithStorageResolver(targetSvc.Storage),
		restore.WithValidationBypassCheck(prober.CanBypassDocumentValidation),
		restore.WithDatabaseAdmin(prober),
		restore.WithDatabaseLister(databaseNames(prober)),
		restore.WithServerVersion(func(ctx context.Context, uri string) (string, error) {
			info, pingErr := prober.Ping(ctx, uri)
			return info.Version, pingErr
		}),
		restore.WithRunConfig(func() restore.RunConfig {
			g := settingsSvc.Current().General
			return restore.RunConfig{Decryptor: settingsSvc.Decryptor(), VerifyPolicy: g.RestoreVerifyPolicy, Timeout: g.RestoreTimeout.Std(),
				CommandTimeout: g.PostRestoreCommandTimeout.Std()}
		}),
		restore.WithCommandRunner(commandRunner(prober)),
		// auditLog is built below; restores only run once the app has started.
		restore.WithCommandAudit(func(ctx context.Context, rec *models.RestoreRecord, res models.PostRestoreResult) {
			auditLog.Record(ctx, postRestoreAuditEvent(rec, res))
		}),
	)

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
	// Deliveries are stored in the metadata database before they are sent
	// (notification_outbox), so a crash or a kill does not lose them.
	notifySvc := notify.NewService(metaStore,
		notify.WithOutbox(metaStore),
		notify.WithChannelWarning(settingsSvc.SetChannelUnreadable),
		notify.WithLogger(logger),
		notify.WithObserver(func(t notify.ChannelType, outcome string) {
			metricSet.ObserveNotification(string(t), outcome)
		}),
	)
	metricSet.SetActiveRunsSource(func(kind string) int { return registry.Count(models.RunKind(kind)) })
	bus.Subscribe(metricSet.ObserveEvent)
	bus.Subscribe(notifySvc.HandleEvent)
	bus.Subscribe(watchEncryptionOffAlert(logger, settingsSvc))
	// Copies of backups to their copy targets (3-2-1): the persistent queue wakes up
	// for every succeeded backup.
	copySvc = copies.New(copies.Config{
		Store:    metaStore,
		Storages: copies.StorageFunc(targetSvc.Storage),
		UploadMbps: func(ctx context.Context, rec *models.BackupRecord) float64 {
			if rec.JobID != "" {
				if job, jobErr := metaStore.GetJob(ctx, rec.JobID); jobErr == nil && job.MaxUploadMbps > 0 {
					return job.MaxUploadMbps
				}
			}
			return settingsSvc.Current().General.MaxUploadMbps
		},
		Publisher: bus,
		Observe:   metricSet.ObserveCopy,
		Logger:    logger,
	})
	metricSet.SetCopyQueueSource(copySvc.QueueDepth)
	bus.Subscribe(copySvc.HandleEvent)
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
	auditLog = auditlog.New(auditlog.Config{
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
	// The PITR collector is built below; the sweep's chunk item calls it.
	// chainTestFailed reports failed PITR chain tests to readiness once the
	// operations service is built.
	var chainTestFailed func(ctx context.Context, streamID string) bool
	var pitrSvc *collector.Service
	integritySvc := integrity.New(integrity.Config{
		ChunkKeys: metaStore.ChunkKeys,
		CopyKeys:  metaStore.CopyKeys,
		VerifyChunks: func(ctx context.Context) (integrity.ChunkSweep, error) {
			if pitrSvc == nil {
				return integrity.ChunkSweep{}, nil
			}
			r, verifyErr := pitrSvc.VerifyChunks(ctx)
			return integrity.ChunkSweep{Verified: r.Verified, Failed: r.Failed, Breaks: r.Breaks}, verifyErr
		},
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
	var keyRotator *keyrotation.Rotator
	metaBackupSvc := metabackup.New(metabackup.Config{
		OnRetiredPruned: previousKeyCheck(settingsSvc, func() *keyrotation.Rotator { return keyRotator }, logger),
		InstallID:       installID,
		Store:           metaStore,
		Targets:         targetSvc,
		DataDir:         cfg.DataDir,
		Settings:        settingsSvc.Current,
		Encryptor:       settingsSvc.Encryptor,
		Publisher:       bus,
		Observe:         metricSet.ObserveMetadataBackup,
		Logger:          logger,
	})
	kitSvc, err := recoverykit.New(recoverykit.Config{
		SecretKey:        key.Key,
		SecretKeyFromEnv: key.FromEnv,
		Settings:         settingsSvc,
		Targets:          targetSvc,
		LatestSnapshot:   metaBackupSvc.Latest,
		MetadataPrefix:   metaBackupSvc.Prefix(),
		PreviousKeyFile:  keyFiles.Previous,
		Version:          o.version,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize recovery kit: %w", err)
	}
	if err = kitSvc.Refresh(ctx); err != nil {
		logger.Warn("could not check whether the recovery kit is current", logsafe.Error(err))
	}
	// secret.key rotation re-seals the store, then hands the new key to every
	// component holding it or one of its subkeys.
	holders := &keyHolders{auth: authSvc, oidcFlow: oidcFlowRef, metaBackup: metaBackupSvc, kit: kitSvc, logger: logger}
	keyRotator = keyrotation.New(keyrotation.Config{
		Files: keyFiles, Store: metaStore, Key: key.Key, FromEnv: key.FromEnv,
		RetiredMAC:       func(old []byte) ([]byte, error) { return secretbox.DeriveSubkey(old, auth.ImportedKeySubkeyPurpose) },
		RetiredInstallID: metabackup.InstallID,
		OnCommit:         holders.onCommit,
		Apply:            holders.apply,
		Logger:           logger,
	})
	// Re-encryption of existing backups after an encryption key rotation.
	reencryptSvc := reencrypt.New(reencrypt.Config{
		Store: metaStore, Storage: targetSvc.Storage,
		Encryptor: settingsSvc.Encryptor, Decryptor: settingsSvc.Decryptor,
		Grace:  func() time.Duration { return settingsSvc.Current().Security.DeleteGrace() },
		Logger: logger,
	})

	// Recovery readiness: the RPO checker (job.rpo_missed / job.rpo_recovered, the
	// job_rpo_* gauges) and the per-database readiness report. A finished backup
	// asks for an early check.
	readinessSvc := readiness.New(readiness.Config{
		Store:        metaStore,
		Connections:  connSvc,
		KeysEscrowed: func() bool { return settingsSvc.RecoveryKitStatus().UpToDate },
		Streams:      pitrStreams(&pitrSvc, &chainTestFailed),
		Targets:      targetSvc.List,
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

	// The data directory's space guard: backups and restores start only with
	// cfg.MinFreeSpaceMB free in the data directory, and not at all after a
	// metadata write failed because the disk was full (system.disk_full), until
	// space is freed. A backup whose final record cannot be saved is reported as
	// failed, never as succeeded.
	diskGuard := diskguard.New(diskguard.Config{
		Dir:       cfg.DataDir,
		MinFree:   cfg.MinFreeSpaceBytes(),
		Publisher: bus,
		Warn:      settingsSvc.SetDiskFullWarning,
		Logger:    logger,
	})
	runManager.SetAdmission(diskGuard.Admit)

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

	// 5. Initialize scheduler. Its maintenance (every ten minutes) purges deleted
	// backups once their grace period ends and applies the due pending changes of the
	// operations service, which is built below.
	var ops *operations.Service
	sched = scheduler.NewScheduler(metaStore, backupEngine, nil, logger,
		scheduler.WithDeleteGrace(func() time.Duration { return settingsSvc.Current().Security.DeleteGrace() }),
		scheduler.WithMaintenance(func(ctx context.Context) {
			if ops != nil {
				ops.ApplyDueChanges(ctx)
			}
		}),
		scheduler.WithPublisher(bus),
		scheduler.WithRunObserver(heartbeatSvc),
		scheduler.WithDiskGuard(diskGuard),
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
	ops = operations.New(operations.Config{
		Store:       metaStore,
		WakeCopies:  copySvc.Notify,
		Backup:      backupEngine,
		Restore:     restoreEngine,
		Jobs:        sched,
		Scheduler:   sched,
		Runs:        runManager,
		Registry:    registry,
		Connections: connSvc,
		Targets:     targetSvc,
		Storage:     targetSvc.Storage,
		ArchiveSize: sizeEstimator,
		Settings:    settingsSvc.Current,
		// The delete protection: delayed and approved settings changes, and the
		// two-person rule needing two administrators.
		SettingsUpdater:     settingsSvc,
		SecondApproverCheck: authSvc.CheckSecondApproverPossible,
		Users:               authSvc,
		KeyRotator:          keyRotator,
		Reencrypter:         reencryptSvc,
		Publisher:           bus,
		DiskGuard:           diskGuard,
		Verifier:            integritySvc,
		Inspector:           prober,
		Dropper:             prober,
		Audit:               auditSvc,
		PITR:                metaStore,
		PITRRestore:         restoreEngine,
		PITRBases:           metaStore.ListBaseBackups,
		ToolsVersion: func(ctx context.Context) (string, error) {
			return mongotools.NewResolver(cfg.ToolsDir).ToolVersion(ctx, "mongorestore")
		},
		Logger:  logger,
		Version: o.version,
		// Deleted jobs (single or bulk) drop their metric series and are no longer
		// checked.
		OnJobDeleted: func(jobID string) {
			metricSet.ForgetJob(jobID)
			readinessSvc.Kick()
		},
	})
	// A secret key rotation that the startup recovery completed is announced now
	// (the bus queues the event until it runs).
	if opened.Completed != nil {
		ops.KeyRotationCompleted(ctx, opened.Completed)
	}
	// The PITR oplog collector: one goroutine and session per enabled stream, off
	// while no stream is enabled. Base backups go through the operations service.
	pitrSvc = collector.New(collector.Config{
		Repo: metaStore,
		OnWriteError: func(ctx context.Context, err error) {
			diskGuard.Observe(ctx, err)
		},
		Open: func(ctx context.Context, st *pitr.Stream) (collector.Session, error) {
			conn, err := connSvc.Resolve(ctx, st.ConnectionID)
			if err != nil {
				return nil, err
			}
			return openOplogSession(mongotls.NewContext(ctx, conn.TLS()), prober, conn.URI, st.ReadPreference)
		},
		Storage:   targetSvc.Storage,
		Encryptor: settingsSvc.Encryptor,
		StartBase: ops.StartBaseBackup,
		// Scheduled chain tests run as the application itself (admin).
		StartChainTest: func(ctx context.Context, id string) error {
			_, chainErr := ops.StartChainTest(auth.WithPrincipal(ctx, auth.SystemPrincipal()), id)
			return chainErr
		},
		LastChainTest: ops.LastChainTestStart,
		Bases:         metaStore.ListBaseBackups,
		NextRun: func(expr string, from time.Time) (time.Time, bool) {
			next := scheduler.NextRuns(expr, from, 1)
			if len(next) == 0 {
				return time.Time{}, false
			}
			return next[0], true
		},
		Inspect: func(ctx context.Context, connectionID string) (collector.Inspection, error) {
			conn, err := connSvc.Resolve(ctx, connectionID)
			if err != nil {
				return collector.Inspection{}, err
			}
			ctx = mongotls.NewContext(ctx, conn.TLS())
			// The oplog's ends are read only once the user may read it.
			if ok, accessErr := prober.CanReadOplog(ctx, conn.URI); accessErr != nil || !ok {
				return collector.Inspection{}, accessErr
			}
			win, err := prober.OplogWindow(ctx, conn.URI)
			return collector.Inspection{Window: win, CanReadOplog: err == nil}, err
		},
		ResolveTarget: func(ctx context.Context, id string) (string, error) {
			t, err := targetSvc.Resolve(ctx, id)
			if err != nil {
				return "", err
			}
			return t.ID, nil
		},
		DeleteGrace: func() time.Duration { return settingsSvc.Current().Security.DeleteGrace() },
		UpdateBase:  metaStore.UpdateBackupRecord,
		Decryptor:   settingsSvc.Decryptor,
		Publisher:   bus,
		Observer:    metricSet,
		Logger:      logger,
	})

	// With the two-person rule, admin users, promotions and admin API keys wait for a
	// second administrator too.
	authSvc.SetAdminGrantGate(ops)
	chainTestFailed = func(ctx context.Context, streamID string) bool {
		r := ops.LastChainTest(ctx, streamID)
		return r != nil && r.Failed
	}
	mcpSrv := mcp.New(mcp.Config{
		Operations:  ops,
		Connections: connSvc,
		Targets:     targetSvc,
		PITR:        pitrSvc,
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
		server.WithKeyRotation(keyRotator),
		server.WithReadiness(readinessSvc),
		server.WithPITR(pitrSvc),
		server.WithHeartbeat(heartbeatSvc),
	}
	if o.desktop {
		serverOpts = append(serverOpts, server.WithDesktopCSP())
	} else {
		serverOpts = append(serverOpts, server.WithOIDCRef(oidcClient, oidcFlowRef))
	}
	srv := server.NewServer(cfg, metaStore, backupEngine, restoreEngine, nil, sched, subFS, logger, serverOpts...)

	return &App{
		cfg:           cfg,
		toolsTemp:     toolsTemp,
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
		copies:        copySvc,
		reencrypt:     reencryptSvc,
		pitr:          pitrSvc,
		cleanupPITR:   ops.CleanupInterruptedPITR,
		readiness:     readinessSvc,
		auditLog:      auditLog,
		auditForward:  auditForwarder,
		heartbeat:     heartbeatSvc,
		diskGuard:     diskGuard,
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
	// The space guard retries the saves a full data directory failed; it stops
	// after the runs too, before the metadata database closes.
	guardCtx, cancelGuard := context.WithCancel(context.Background())
	var guardWG sync.WaitGroup
	guardWG.Go(func() { a.diskGuard.Run(guardCtx) })

	return func() {
		cancelHeartbeat()
		heartbeatWG.Wait()
		cancelGuard()
		guardWG.Wait()
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
// embedded host can hold back a restart, such as the desktop app's update. A run of
// a multi-database job counts between its databases too.
func (a *App) Busy() bool {
	return !a.runs.Idle()
}

// drainPoll is how often Drain checks whether the runs ended, and
// drainProgressInterval how often it logs the runs it still waits for.
const (
	drainPoll             = 250 * time.Millisecond
	drainProgressInterval = 30 * time.Second
)

// Drain prepares a graceful shutdown: it refuses new backups and restores (see
// PauseRuns: the API answers 503, the scheduler skips its triggers) and waits until
// the running ones end, grace passes or ctx ends. It reports whether no run is left.
// It does not stop anything itself: Stop or ForceStop (for the runs left) follow.
func (a *App) Drain(ctx context.Context, grace time.Duration) bool {
	a.PauseRuns()
	if !a.Busy() {
		return true
	}
	if grace <= 0 {
		return false
	}
	a.logger.Info("waiting for running backups and restores to finish before shutting down; new runs are refused",
		slog.Any("active_runs", a.runs.Active()), slog.Duration("grace", grace))
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	poll := time.NewTicker(drainPoll)
	defer poll.Stop()
	lastLog := time.Now()
	for {
		select {
		case <-ctx.Done():
			return !a.Busy()
		case <-deadline.C:
			return !a.Busy()
		case <-poll.C:
			if !a.Busy() {
				a.logger.Info("running backups and restores finished; shutting down")
				return true
			}
			if time.Since(lastLog) >= drainProgressInterval {
				lastLog = time.Now()
				a.logger.Info("still waiting for running backups and restores", slog.Any("active_runs", a.runs.Active()))
			}
		}
	}
}

// ShutdownReason is the reason recorded on the backups and restores a shutdown
// cancels because they did not finish within the shutdown grace period.
const ShutdownReason = "interrupted: MongoRescue shut down before the run finished"

// shutdown drains the runs for at most a.cfg.ShutdownGrace (a second signal on
// ctx stops the wait), stops the HTTP server and then stops the App: with
// ForceStop(ShutdownReason) when runs are left, so they are recorded as cancelled with
// their partial archives removed, and with Stop otherwise.
func (a *App) shutdown(ctx context.Context) error {
	idle := a.Drain(ctx, a.cfg.ShutdownGrace)
	drainCtx, cancel := context.WithTimeout(context.Background(), httpDrainTimeout)
	err := a.server.Shutdown(drainCtx)
	cancel()
	if !idle && a.Busy() {
		a.logger.Warn("cancelling the backups and restores still running",
			slog.Any("active_runs", a.runs.Active()), slog.Duration("grace", a.cfg.ShutdownGrace))
		a.ForceStop(ShutdownReason)
		return err
	}
	a.Stop()
	return err
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

	// Purge credential-bearing tools config files and TLS directories left behind by
	// a previous crash: in the directory in use, and in <data dir>/tmp, the
	// fallback an earlier start may have used.
	dirs := []string{a.toolsTemp}
	if fallback := filepath.Join(a.cfg.DataDir, ToolsTempDirName); fallback != a.toolsTemp {
		if info, statErr := os.Stat(fallback); statErr == nil && info.IsDir() {
			dirs = append(dirs, fallback)
		}
	}
	for _, dir := range dirs {
		if removed, err := mongotools.CleanupStale(dir, staleToolsConfigAge); err != nil {
			a.logger.Warn("failed to clean up stale mongo tools config files",
				slog.Int("removed", removed),
				slog.Any("error", err),
			)
		} else {
			a.logger.Info("stale mongo tools config cleanup completed", slog.Int("removed", removed))
		}
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
	// The copy queue; stopped by shutdownRuns.
	if a.copies != nil {
		a.copies.Start(context.WithoutCancel(ctx))
	}
	// Re-encryption of backups after a key rotation; resumes an interrupted job and
	// is stopped by shutdownRuns.
	if a.reencrypt != nil {
		a.reencrypt.Start(context.WithoutCancel(ctx))
	}
	// The PITR oplog collector; stopped by shutdownRuns.
	if a.pitr != nil {
		a.pitr.Start(context.WithoutCancel(ctx))
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
		// A second signal ends the wait for running backups and restores.
		again, stopAgain := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		runErr = a.shutdown(again)
		stopAgain()
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
		wg.Add(6)
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
			if a.copies != nil {
				a.copies.Stop()
			}
			if a.reencrypt != nil {
				a.reencrypt.Stop()
			}
		}()
		go func() {
			defer wg.Done()
			if a.pitr != nil {
				a.pitr.Stop()
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
// process (crash, SIGKILL) as failed, since nothing will ever complete them, and
// publishes their backup.failed and restore.failed events: the run that died with
// the process never did, so without them a crash would go unnoticed by the alerts.
// The bus queues the events until Start runs it.
func (a *App) failInterruptedRuns(ctx context.Context) {
	const msg = "interrupted: the server stopped before this run finished"
	var failed []*models.BackupRecord
	if backups, err := a.metaStore.ListBackupRecords(ctx, ""); err == nil {
		for _, b := range backups {
			if b.Status == models.StatusInProgress {
				b.Status, b.ErrorMessage, b.SizeBytes, b.SHA256 = models.StatusFailed, msg, 0, ""
				// Copies it never made leave the queue; copies a synchronous run made
				// already go to the purge (archive_cleanup_pending).
				b.AbandonCopies()
				// So does an archive it may have uploaded, such as the complete one of
				// a run whose final record could not be saved (a full data
				// directory): the purge deletes it instead of leaving an orphan.
				if b.StorageKey != "" {
					b.ArchiveCleanupPending = true
				}
				if err := a.metaStore.SaveBackupRecord(ctx, b); err != nil {
					a.logger.Warn("failed to mark interrupted backup", slog.String("backup_id", b.ID), logsafe.Error(err))
					continue
				}
				failed = append(failed, b)
			}
		}
	}
	if restores, err := a.metaStore.ListRestoreRecords(ctx); err == nil {
		for _, r := range restores {
			if r.Status == models.RestoreStatusInProgress {
				// An interrupted point-in-time restore or chain test drops the clones
				// it recorded, and says so.
				note := ""
				if a.cleanupPITR != nil && r.PITR != nil {
					note = a.cleanupPITR(ctx, r)
				}
				r.Status, r.ErrorMessage = models.RestoreStatusFailed, msg+note
				if err := a.metaStore.SaveRestoreRecord(ctx, r); err != nil {
					a.logger.Warn("failed to mark interrupted restore", slog.String("restore_id", r.ID), logsafe.Error(err))
					continue
				}
				a.publishInterrupted(ctx, events.RestoreEvent(r, nil, r.BackupID))
			}
		}
	}
	var multiRuns []*models.JobRun
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
				continue
			}
			// A run of several databases is reported once, by its summary, as when
			// it finishes normally; its databases' events only feed the metrics.
			if len(run.Databases) > 1 {
				multiRuns = append(multiRuns, run)
			}
		}
	}
	inMulti := map[string]bool{}
	for _, run := range multiRuns {
		inMulti[run.ID] = true
	}
	for _, b := range failed {
		e := events.BackupEvent(b, nil, b.JobID, b.Database)
		e.RunID, e.InRun = b.RunID, inMulti[b.RunID]
		a.publishInterrupted(ctx, e)
	}
	for _, run := range multiRuns {
		a.publishInterrupted(ctx, events.JobRunEvent(run))
	}
}

// publishInterrupted publishes an event of a run failInterruptedRuns ended; it is a
// no-op without an event bus.
func (a *App) publishInterrupted(ctx context.Context, e events.Event) {
	if a.bus != nil {
		a.bus.Publish(ctx, e)
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

// toolsTempDir returns the directory of the short-lived files that pass connection
// strings and TLS material to the Database Tools: cfg.TmpDir (created 0700 when
// missing), else the system temporary directory, else, when that is not
// writable, <data dir>/tmp (created 0700).
func toolsTempDir(cfg *config.Config) (string, error) {
	if cfg.TmpDir != "" {
		if err := os.MkdirAll(cfg.TmpDir, toolsTempDirPerm); err != nil {
			return "", fmt.Errorf("create %s: %w", config.EnvTmpDir, err)
		}
		if err := writableDir(cfg.TmpDir); err != nil {
			return "", fmt.Errorf("%s is not writable: %w", config.EnvTmpDir, err)
		}
		return cfg.TmpDir, nil
	}
	if dir := os.TempDir(); writableDir(dir) == nil {
		return dir, nil
	}
	dir := filepath.Join(cfg.DataDir, ToolsTempDirName)
	if err := os.MkdirAll(dir, toolsTempDirPerm); err != nil {
		return "", fmt.Errorf("create tools temporary directory: %w", err)
	}
	if err := os.Chmod(dir, toolsTempDirPerm); err != nil {
		return "", fmt.Errorf("restrict tools temporary directory: %w", err)
	}
	return dir, nil
}

// writableDir reports whether a private directory can be created in dir.
func writableDir(dir string) error {
	probe, err := os.MkdirTemp(dir, "mongorescue-probe-*")
	if err != nil {
		return err
	}
	return os.Remove(probe)
}

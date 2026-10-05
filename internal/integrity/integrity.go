// Package integrity gives evidence that backups are restorable and that the backup
// records match what is stored:
//
//   - archive verification on demand and in a scheduled integrity sweep, which
//     re-reads every completed backup (least recently verified first, one at a time,
//     optionally rate limited) and compares it with its recorded checksum;
//   - automated restore tests, which restore a job's latest backup into a temporary
//     <db>_rescue_verify_<timestamp> database, compare it with the manifest captured
//     at backup time and always drop it again;
//   - storage scans, which compare a storage target's objects with the backup
//     records and report orphan archives (which can be imported) and missing ones.
//
// Nothing here ever deletes an archive or touches a backup's source database.
// The package is a business package: it depends on ports (the metadata store, the
// restore engine, the MongoDB adapter, the storage targets), never on the HTTP
// server.
package integrity

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// Sentinel errors. Their messages are safe to show to clients.
var (
	// ErrNotFound is returned for an unknown backup, job, storage target or object.
	ErrNotFound = errors.New("integrity: not found")
	// ErrBusy is returned when the same verification, sweep, restore test, scan or
	// import is already running. It aliases runs.ErrBusy.
	ErrBusy = runs.ErrBusy
	// ErrNotVerifiable is returned for a backup that is not completed or has no
	// checksum to verify against.
	ErrNotVerifiable = errors.New("integrity: only completed backups with a checksum can be verified")
	// ErrNoBackup is returned when a job has no completed backup to restore-test.
	ErrNoBackup = errors.New("integrity: the job has no completed backup to test")
	// ErrInsufficientPrivileges is returned when the restore test's user may not
	// create, fill and drop the temporary database.
	ErrInsufficientPrivileges = errors.New("integrity: the connection's user lacks the privileges a restore test needs")
	// ErrTempDatabaseExists is returned when the restore test's temporary database
	// already exists; the test refuses to touch it.
	ErrTempDatabaseExists = errors.New("integrity: the temporary restore test database already exists")
	// ErrNotOrphan is returned when importing an object that is not an orphan
	// archive (it belongs to a backup record, or is not an archive at all).
	ErrNotOrphan = errors.New("integrity: the object is not an orphan archive")
	// ErrInvalidImport is returned when an orphan's key names no valid database
	// (MongoDB's naming rules, and no '*' or '\', which namespace patterns treat as
	// wildcards and escapes).
	ErrInvalidImport = errors.New("integrity: the archive cannot be imported")
	// ErrUnavailable is returned when a dependency the operation needs is not
	// configured.
	ErrUnavailable = errors.New("integrity: not available")
	// ErrEmptyListing is recorded on a storage scan that listed no archive at all
	// although completed backups are recorded on the target; no record is marked
	// missing then.
	ErrEmptyListing = errors.New("integrity: storage target lists no archives")
)

// Store is the persistence port (implemented by *store.SQLiteStore).
type Store interface {
	// GetJob returns a job or an error wrapping store.ErrNotFound.
	GetJob(ctx context.Context, id string) (*models.Job, error)
	// GetBackupRecord returns a backup record or an error wrapping store.ErrNotFound.
	GetBackupRecord(ctx context.Context, id string) (*models.BackupRecord, error)
	// ListBackupRecords returns backup records, newest first ("" = all databases).
	ListBackupRecords(ctx context.Context, database string) ([]*models.BackupRecord, error)
	// SaveBackupRecord creates or replaces a backup record.
	SaveBackupRecord(ctx context.Context, record *models.BackupRecord) error
	// UpdateBackupRecord applies fn to the stored record id in one transaction.
	UpdateBackupRecord(ctx context.Context, id string, fn func(*models.BackupRecord) error) (*models.BackupRecord, error)
	// ArchiveKeyIndex maps every storage key a backup row on targetID names (also
	// rows that cannot be decoded) to the IDs of those rows.
	ArchiveKeyIndex(ctx context.Context, targetID string) (map[string][]string, error)
	// GetManifest returns the manifest of a backup or an error wrapping
	// store.ErrNotFound.
	GetManifest(ctx context.Context, id string) (*models.Manifest, error)
	// SaveRestoreTest stores a restore test result.
	SaveRestoreTest(ctx context.Context, r *models.RestoreTestResult) error
	// ListRestoreTests returns a job's restore tests, newest first.
	ListRestoreTests(ctx context.Context, jobID string, limit int) ([]*models.RestoreTestResult, error)
	// UpdateJobRestoreTest stores a job's latest restore test summary.
	UpdateJobRestoreTest(ctx context.Context, id string, summary *models.RestoreTestSummary) error
	// LoadIntegrityState decodes state document key into v and reports whether it
	// exists.
	LoadIntegrityState(ctx context.Context, key string, v any) (bool, error)
	// SaveIntegrityState stores v as state document key.
	SaveIntegrityState(ctx context.Context, key string, v any) error
}

// DatabaseAdmin is the MongoDB port (implemented by *mongoconn.Prober).
type DatabaseAdmin interface {
	// DatabaseExists reports whether database exists on the server at uri.
	DatabaseExists(ctx context.Context, uri, database string) (bool, error)
	// DropDatabase drops database on the server at uri.
	DropDatabase(ctx context.Context, uri, database string) error
	// Manifest returns the manifest of database on the server at uri.
	Manifest(ctx context.Context, uri, database string) (*models.Manifest, error)
	// RestoreTestPrivileges returns the actions the user lacks to restore-test into
	// database, or none.
	RestoreTestPrivileges(ctx context.Context, uri, database string) ([]string, error)
}

// Restorer runs restores (implemented by *restore.Engine).
type Restorer interface {
	// CanDecrypt reports whether a decryption key is configured.
	CanDecrypt() bool
	// Prepare validates req and returns the in-progress record.
	Prepare(req models.RestoreRequest, source *models.BackupRecord) (*models.RestoreRecord, error)
	// Execute runs the restore described by req and record.
	Execute(ctx context.Context, req models.RestoreRequest, source *models.BackupRecord, record *models.RestoreRecord) (*models.RestoreRecord, error)
}

// Connections resolves managed connections (implemented by *connections.Service).
type Connections interface {
	// Resolve returns the connection with its full URI.
	Resolve(ctx context.Context, id string) (*models.Connection, error)
}

// Targets resolves storage targets and their drivers (implemented by
// *targets.Service).
type Targets interface {
	// List returns every storage target.
	List(ctx context.Context) ([]*models.StorageTarget, error)
	// Resolve returns target id (the default target for "").
	Resolve(ctx context.Context, id string) (*models.StorageTarget, error)
	// Storage returns the driver of target id.
	Storage(ctx context.Context, id string) (storage.Storage, error)
}

// Config holds the dependencies of a Service. Store, Targets and Runs are required;
// without Restore, Admin and Connections restore tests are unavailable.
type Config struct {
	// Store persists records, manifests, restore tests and the integrity state.
	Store Store
	// Targets resolves storage targets and their drivers.
	Targets Targets
	// Runs owns on-demand work started through the API.
	Runs *runs.Manager
	// Restore restores backups for restore tests.
	Restore Restorer
	// Admin checks, inspects and drops restore test databases.
	Admin DatabaseAdmin
	// Connections resolves the connections restore tests restore into.
	Connections Connections
	// Settings returns the live settings; nil means the defaults.
	Settings func() settings.Settings
	// Decryptor returns the configured decryption keys (nil when none).
	Decryptor func() *encryption.Decryptor
	// Publisher receives verification, restore test and drift events.
	Publisher events.Publisher
	// VerifyChunks is the chunk item of sweeps: it verifies the PITR oplog chunks
	// (implemented by collector.Service.VerifyChunks); nil skips them.
	VerifyChunks func(ctx context.Context) (ChunkSweep, error)
	// ChunkKeys returns the storage keys of the PITR oplog chunks recorded on a
	// target (implemented by store.SQLiteStore.ChunkKeys); nil means none, and
	// chunk objects are then never reported as orphans.
	ChunkKeys func(ctx context.Context, targetID string) (map[string]bool, error)
	// ObserveScan receives the outcome of every storage scan (metrics).
	ObserveScan func(targetID string, orphans, missing int, at time.Time)
	// Logger receives operational logs; nil means slog.Default().
	Logger *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// CheckInterval is how often the background loop looks for due sweeps and
	// scans (default 15 minutes); StartDelay delays its first check (default 2
	// minutes) so that start-up is not slowed down.
	CheckInterval time.Duration
	StartDelay    time.Duration
}

// Service implements verification, sweeps, restore tests and storage scans. It is
// safe for concurrent use.
type Service struct {
	cfg    Config
	logger *slog.Logger

	// sweepMu makes sweeps single-flight; sweep is the live status of the running
	// sweep (guarded by mu).
	sweepMu sync.Mutex
	mu      sync.Mutex
	sweep   *SweepStatus

	// lifecycle of the background loop
	lifeMu  sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// New returns a Service. It panics when a required dependency is missing, which is a
// wiring bug.
func New(cfg Config) *Service {
	if cfg.Store == nil || cfg.Targets == nil || cfg.Runs == nil {
		panic("integrity: Store, Targets and Runs are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 15 * time.Minute
	}
	if cfg.StartDelay <= 0 {
		cfg.StartDelay = 2 * time.Minute
	}
	return &Service{cfg: cfg, logger: cfg.Logger}
}

// settings returns the live settings or the defaults.
func (s *Service) settings() settings.Settings {
	if s.cfg.Settings == nil {
		return settings.Defaults()
	}
	return s.cfg.Settings()
}

// now returns the current time in UTC.
func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// decryptor returns the decryption keys verification uses: only with the
// integrity.verify_decrypt setting.
func (s *Service) verifyDecryptor() *encryption.Decryptor {
	if s.cfg.Decryptor == nil || !s.settings().Integrity.VerifyDecrypt {
		return nil
	}
	return s.cfg.Decryptor()
}

// publish emits e if a publisher is configured.
func (s *Service) publish(ctx context.Context, e events.Event) {
	if s.cfg.Publisher != nil {
		s.cfg.Publisher.Publish(ctx, e)
	}
}

// Start runs the background loop that starts due integrity sweeps and weekly
// storage scans, until Stop or ctx ends. It is a no-op when already started.
func (s *Service) Start(ctx context.Context) {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.started {
		return
	}
	s.started = true
	s.recoverInterrupted(ctx)
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(loopCtx)
	}()
}

// Stop cancels the background loop (and a sweep or scan it runs) and waits for it.
// It is safe to call more than once and before Start.
func (s *Service) Stop() {
	s.lifeMu.Lock()
	cancel := s.cancel
	s.lifeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// loop checks for due work after StartDelay and then every CheckInterval.
func (s *Service) loop(ctx context.Context) {
	timer := time.NewTimer(s.cfg.StartDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.RunDue(ctx)
		timer.Reset(s.cfg.CheckInterval)
	}
}

// RunDue starts the integrity sweep and the storage scans when they are due, one
// after the other, and returns when they finished. The background loop calls it.
func (s *Service) RunDue(ctx context.Context) {
	now := s.now()
	if due := s.nextSweep(ctx); due != nil && !now.Before(*due) {
		if _, err := s.Sweep(ctx, TriggerScheduled); err != nil && !errors.Is(err, ErrBusy) && ctx.Err() == nil {
			s.logger.Warn("scheduled integrity sweep failed", slog.Any("error", err))
		}
	}
	if due := s.nextScan(ctx); due != nil && !now.Before(*due) {
		if err := s.ScanAll(ctx, TriggerScheduled); err != nil && ctx.Err() == nil {
			s.logger.Warn("scheduled storage scan failed", slog.Any("error", err))
		}
	}
}

// Triggers of sweeps, scans and restore tests.
const (
	// TriggerScheduled is work started by a schedule.
	TriggerScheduled = "scheduled"
	// TriggerManual is work started through the API.
	TriggerManual = "manual"
)

// Overview is the integrity status shown in the dashboard.
type Overview struct {
	// Sweep is the status of the integrity sweep.
	Sweep SweepStatus `json:"sweep"`
	// Scans is the latest storage scan of every storage target (targets never
	// scanned are omitted).
	Scans []*DriftReport `json:"scans"`
	// NextScanAt is when the weekly storage scan runs next (nil when off).
	NextScanAt *time.Time `json:"next_scan_at,omitempty"`
	// Running lists the integrity work in progress (verify:<backup>,
	// restore-test:<job>, scan:<target>, import:<target>, sweep).
	Running []string `json:"running"`
}

// Status returns the integrity overview.
func (s *Service) Status(ctx context.Context) (*Overview, error) {
	out := &Overview{Sweep: s.SweepStatus(ctx), Scans: []*DriftReport{}, Running: []string{}, NextScanAt: s.nextScan(ctx)}
	targets, err := s.cfg.Targets.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		if r, ok := s.LastScan(ctx, t.ID); ok {
			out.Scans = append(out.Scans, r)
		}
	}
	for _, k := range s.cfg.Runs.Active() {
		if isIntegrityKey(k) {
			out.Running = append(out.Running, k)
		}
	}
	return out, nil
}

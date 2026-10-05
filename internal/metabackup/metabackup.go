// Package metabackup backs up MongoRescue's own metadata database (jobs, backup
// records, users, settings, storage targets) on a schedule: an online snapshot is
// written with SQLite's VACUUM INTO to a temporary file in the data directory,
// streamed through age encryption (the backup encryption settings) to a storage
// target under the _mongorescue/metadata/ prefix, and removed again. Only the
// newest snapshots are kept.
//
// The snapshot is the one temporary copy MongoRescue writes: VACUUM INTO needs a
// file, and it holds metadata, never a dump. Credentials inside it stay sealed with
// secret.key, so restoring a snapshot needs that key (see the recovery kit).
//
// The package is a business package: it depends on ports (the metadata store, the
// storage targets), never on the HTTP server.
package metabackup

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// Prefix is the storage key prefix of metadata snapshots. Each installation writes
// below its own sub-prefix, Prefix + install ID + "/" (see InstallID).
const Prefix = "_mongorescue/metadata/"

// installIDPurpose derives the install ID from secret.key.
const installIDPurpose = "metadata-backup-install-id"

// installIDPattern is the shape of an install ID (see InstallID).
var installIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// snapshotName matches the file name of a snapshot written by this package;
// retention only ever deletes such keys, and only under the install's prefix.
var snapshotName = regexp.MustCompile(`^mongorescue-\d{8}T\d{9}Z\.db(\.age)?$`)

// InstallID returns the stable identifier of the installation that owns
// secretKey: 16 hex characters of an HKDF subkey of secret.key. It is the same on a
// host restored with the same key, and different for installations with different
// keys, so installations that share a bucket and prefix keep their snapshots apart.
// It reveals nothing about the key.
func InstallID(secretKey []byte) (string, error) {
	sub, err := secretbox.DeriveSubkey(secretKey, installIDPurpose)
	if err != nil {
		return "", fmt.Errorf("metabackup: derive install ID: %w", err)
	}
	return hex.EncodeToString(sub[:8]), nil
}

// tempDirPattern names the temporary directories snapshots are written to.
const tempDirPattern = ".metabackup-*"

// stateKey is the integrity state document holding the status.
const stateKey = "metadata_backup.status"

// retryAfterFailure bounds how long the schedule waits after a failed snapshot.
const retryAfterFailure = time.Hour

// Triggers of a snapshot.
const (
	// TriggerScheduled is a snapshot started by the schedule.
	TriggerScheduled = "scheduled"
	// TriggerManual is a snapshot started through the API.
	TriggerManual = "manual"
)

// Sentinel errors. Their messages are safe to show to clients.
var (
	// ErrBusy is returned when a snapshot is already running.
	ErrBusy = errors.New("metabackup: a metadata snapshot is already running")
	// ErrUnavailable is returned when the service is not running (before Start or
	// after Stop).
	ErrUnavailable = errors.New("metabackup: the metadata backup service is not running")
	// ErrNoTarget is returned when the configured storage target does not exist.
	ErrNoTarget = errors.New("metabackup: the storage target of metadata backups does not exist")
)

// Store is the persistence port (implemented by *store.SQLiteStore).
type Store interface {
	// VacuumInto writes a consistent snapshot of the database to the new file path.
	VacuumInto(ctx context.Context, path string) error
	// LoadIntegrityState decodes state document key into v and reports whether it
	// exists.
	LoadIntegrityState(ctx context.Context, key string, v any) (bool, error)
	// SaveIntegrityState stores v as state document key.
	SaveIntegrityState(ctx context.Context, key string, v any) error
}

// Targets resolves storage targets and their drivers (implemented by
// *targets.Service).
type Targets interface {
	// Resolve returns target id (the default target for "").
	Resolve(ctx context.Context, id string) (*models.StorageTarget, error)
	// Storage returns the driver of target id.
	Storage(ctx context.Context, id string) (storage.Storage, error)
}

// Config holds the dependencies of a Service. Store, Targets, DataDir and InstallID
// are required.
type Config struct {
	// InstallID scopes the snapshots of this installation (see InstallID).
	InstallID string
	// Store snapshots the database and persists the status.
	Store Store
	// Targets resolves the storage target snapshots are written to.
	Targets Targets
	// DataDir is the data directory; snapshots are written to a temporary
	// directory inside it.
	DataDir string
	// Settings returns the live settings; nil means the defaults.
	Settings func() settings.Settings
	// Encryptor returns the backup encryptor (nil when encryption is off).
	Encryptor func() *encryption.Encryptor
	// Publisher receives a MetadataBackupFailed event for every failed snapshot.
	Publisher events.Publisher
	// Observe receives the outcome of every snapshot (metrics).
	Observe func(ok bool, sizeBytes int64, at time.Time)
	// Logger receives operational logs; nil means slog.Default().
	Logger *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// CheckInterval is how often the background loop looks for a due snapshot
	// (default 5 minutes); StartDelay delays its first check (default 1 minute).
	CheckInterval time.Duration
	StartDelay    time.Duration
}

// Snapshot describes a stored metadata snapshot.
type Snapshot struct {
	// TargetID and TargetName name the storage target.
	TargetID   string `json:"target_id"`
	TargetName string `json:"target_name"`
	// InstallID is the installation that wrote the snapshot; its snapshots are
	// under Prefix + InstallID + "/".
	InstallID string `json:"install_id"`
	// Key is the storage key.
	Key string `json:"key"`
	// CreatedAt is when the snapshot was taken.
	CreatedAt time.Time `json:"created_at"`
	// SizeBytes is the stored size (after encryption).
	SizeBytes int64 `json:"size_bytes"`
	// Encrypted reports whether the snapshot is age-encrypted.
	Encrypted bool `json:"encrypted"`
}

// Status is the state of the metadata backup shown in the dashboard.
type Status struct {
	// Enabled reports whether scheduled snapshots are on.
	Enabled bool `json:"enabled"`
	// Prefix is the storage key prefix of this installation's snapshots.
	Prefix string `json:"prefix"`
	// Running reports a snapshot in progress.
	Running bool `json:"running"`
	// LastRunAt is when the last snapshot (successful or not) started.
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
	// LastTrigger is TriggerScheduled or TriggerManual.
	LastTrigger string `json:"last_trigger,omitempty"`
	// LastError is the failure of the last snapshot ("" when it succeeded).
	LastError string `json:"last_error,omitempty"`
	// RetentionError reports the last failure to delete an old snapshot.
	RetentionError string `json:"retention_error,omitempty"`
	// Last is the last stored snapshot.
	Last *Snapshot `json:"last,omitempty"`
	// NextRunAt is when the next scheduled snapshot is due (nil when off).
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
}

// Service runs metadata snapshots on a schedule and on demand. It is safe for
// concurrent use.
type Service struct {
	cfg    Config
	logger *slog.Logger

	// installID is the install ID in use (a secret key rotation replaces it).
	idMu      sync.RWMutex
	installID string

	// runMu makes snapshots single-flight; running mirrors it for Status.
	runMu   sync.Mutex
	mu      sync.Mutex
	running bool

	// lifecycle of the background loop and of snapshots started by Trigger
	lifeMu  sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// New returns a Service. It panics when a required dependency is missing, which is a
// wiring bug.
func New(cfg Config) *Service {
	if cfg.Store == nil || cfg.Targets == nil || cfg.DataDir == "" || !installIDPattern.MatchString(cfg.InstallID) {
		panic("metabackup: Store, Targets, DataDir and a valid InstallID are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 5 * time.Minute
	}
	if cfg.StartDelay <= 0 {
		cfg.StartDelay = time.Minute
	}
	return &Service{cfg: cfg, logger: cfg.Logger, installID: cfg.InstallID}
}

// Prefix returns the storage key prefix of this installation's snapshots.
func (s *Service) Prefix() string { return Prefix + s.InstallID() + "/" }

// InstallID returns the install ID snapshots are written under.
func (s *Service) InstallID() string {
	s.idMu.RLock()
	defer s.idMu.RUnlock()
	return s.installID
}

// SetInstallID switches to the install ID of a rotated secret.key (see InstallID):
// later snapshots, sealed with the new key, go below the new prefix, and the
// snapshots sealed with the old key stay below the old one, out of retention.
func (s *Service) SetInstallID(id string) error {
	if !installIDPattern.MatchString(id) {
		return errors.New("metabackup: invalid install ID")
	}
	s.idMu.Lock()
	defer s.idMu.Unlock()
	s.installID = id
	return nil
}

// settings returns the live metadata backup settings or the defaults.
func (s *Service) settings() settings.MetadataBackup {
	if s.cfg.Settings == nil {
		return settings.Defaults().MetadataBackup
	}
	return s.cfg.Settings().MetadataBackup
}

// now returns the current time in UTC.
func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// deleteGrace returns the delete grace period in force (security.delete_grace_days):
// retention never deletes a snapshot younger than it.
func (s *Service) deleteGrace() time.Duration {
	if s.cfg.Settings == nil {
		return settings.Defaults().Security.DeleteGrace()
	}
	return s.cfg.Settings().Security.DeleteGrace()
}

// snapshotTime returns when the snapshot named key (under the prefix) was taken,
// from its name; ok is false when the name holds no time.
func snapshotTime(name string) (time.Time, bool) {
	stamp, _, found := strings.Cut(strings.TrimPrefix(name, "mongorescue-"), "Z.db")
	if !found || len(stamp) != len("20060102T150405")+3 {
		return time.Time{}, false
	}
	at, err := time.ParseInLocation("20060102T150405", stamp[:len(stamp)-3], time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	ms, err := strconv.Atoi(stamp[len(stamp)-3:])
	if err != nil {
		return time.Time{}, false
	}
	return at.Add(time.Duration(ms) * time.Millisecond), true
}

// Start removes temporary snapshots left by a crash and runs the background loop
// that takes due snapshots, until Stop or ctx ends. It is a no-op when already
// started.
func (s *Service) Start(ctx context.Context) {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.started {
		return
	}
	s.started = true
	s.removeStaleTemp()
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(s.ctx)
	}()
}

// Stop cancels the background loop and a running snapshot and waits for them. It is
// safe to call more than once and before Start.
func (s *Service) Stop() {
	// Cancelling under lifeMu orders it with Trigger, which only adds to wg while
	// the context is live.
	s.lifeMu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.lifeMu.Unlock()
	s.wg.Wait()
}

// loop checks for a due snapshot after StartDelay and then every CheckInterval.
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

// RunDue takes a snapshot when one is due. The background loop calls it.
func (s *Service) RunDue(ctx context.Context) {
	st := s.load(ctx)
	next := s.nextRun(st)
	if next == nil || s.now().Before(*next) {
		return
	}
	if _, err := s.Run(ctx, TriggerScheduled); err != nil && !errors.Is(err, ErrBusy) && ctx.Err() == nil {
		s.logger.Warn("scheduled metadata backup failed", logsafe.Error(err))
	}
}

// nextRun returns when the next scheduled snapshot is due, or nil when scheduled
// snapshots are off. A failed snapshot is retried after at most an hour.
func (s *Service) nextRun(st storedStatus) *time.Time {
	cfg := s.settings()
	if !cfg.Enabled {
		return nil
	}
	if st.LastRunAt == nil {
		at := s.now()
		return &at
	}
	wait := cfg.Interval.Std()
	if st.LastError != "" && wait > retryAfterFailure {
		wait = retryAfterFailure
	}
	at := st.LastRunAt.Add(wait).UTC()
	return &at
}

// Trigger starts a snapshot in the background, bound to the service's lifetime. It
// returns ErrBusy when one is running and ErrUnavailable when the service is not
// running.
func (s *Service) Trigger() error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if !s.started || s.ctx == nil || s.ctx.Err() != nil {
		return ErrUnavailable
	}
	if !s.runMu.TryLock() {
		return ErrBusy
	}
	ctx := s.ctx
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.runMu.Unlock()
		if _, err := s.run(ctx, TriggerManual); err != nil && ctx.Err() == nil {
			s.logger.Warn("metadata backup failed", logsafe.Error(err))
		}
	}()
	return nil
}

// Run takes a snapshot now and returns it once stored. It returns ErrBusy when one
// is already running.
func (s *Service) Run(ctx context.Context, trigger string) (*Snapshot, error) {
	if !s.runMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.runMu.Unlock()
	return s.run(ctx, trigger)
}

// run takes a snapshot; the caller holds runMu.
func (s *Service) run(ctx context.Context, trigger string) (*Snapshot, error) {
	s.setRunning(true)
	defer s.setRunning(false)
	started := s.now()
	st := s.load(ctx)
	st.LastRunAt, st.LastTrigger = &started, trigger

	res, err := s.snapshot(ctx, started)
	if err != nil {
		st.LastError = redact.Text(err.Error())
		s.save(ctx, st)
		if s.cfg.Observe != nil {
			s.cfg.Observe(false, 0, started)
		}
		e := events.Event{Type: events.MetadataBackupFailed, Time: s.now(), Status: "failed", Source: trigger, Error: st.LastError}
		if res.target != nil {
			e.TargetID, e.TargetName = res.target.ID, res.target.Name
		}
		if s.cfg.Publisher != nil && ctx.Err() == nil {
			s.cfg.Publisher.Publish(ctx, e)
		}
		return nil, err
	}
	snap := res.snap
	st.LastError, st.Last = "", snap
	st.RetentionError = ""
	if res.retentionErr != nil {
		st.RetentionError = redact.Text(res.retentionErr.Error())
		s.logger.Warn("metadata backup retention failed", logsafe.Error(res.retentionErr))
	}
	s.save(ctx, st)
	if s.cfg.Observe != nil {
		s.cfg.Observe(true, snap.SizeBytes, snap.CreatedAt)
	}
	s.logger.Info("metadata backup stored",
		logsafe.Attr("target", snap.TargetID), logsafe.Attr("object", snap.Key),
		slog.Int64("size_bytes", snap.SizeBytes), slog.Bool("encrypted", snap.Encrypted))
	return snap, nil
}

// result is the outcome of snapshot.
type result struct {
	// snap is the stored snapshot.
	snap *Snapshot
	// target is the storage target, also on failure once it was resolved.
	target *models.StorageTarget
	// retentionErr is a retention failure, which does not fail the snapshot.
	retentionErr error
}

// snapshot writes the database to a temporary file, uploads it and applies
// retention.
func (s *Service) snapshot(ctx context.Context, at time.Time) (result, error) {
	var res result
	cfg := s.settings()
	target, err := s.cfg.Targets.Resolve(ctx, cfg.TargetID)
	if err != nil {
		if ctx.Err() == nil {
			return res, fmt.Errorf("%w: %w", ErrNoTarget, err)
		}
		return res, err
	}
	res.target = target
	driver, err := s.cfg.Targets.Storage(ctx, target.ID)
	if err != nil {
		return res, fmt.Errorf("metabackup: open storage target: %w", err)
	}

	dir, err := os.MkdirTemp(s.cfg.DataDir, tempDirPattern)
	if err != nil {
		return res, fmt.Errorf("metabackup: create temporary directory: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			s.logger.Warn("could not remove the temporary metadata snapshot", logsafe.Error(rmErr))
		}
	}()
	file := filepath.Join(dir, "mongorescue.db")
	if err = s.cfg.Store.VacuumInto(ctx, file); err != nil {
		return res, err
	}

	var enc *encryption.Encryptor
	if s.cfg.Encryptor != nil {
		enc = s.cfg.Encryptor()
	}
	installID := s.InstallID()
	key := Prefix + installID + "/" + snapshotFile(at, enc != nil)
	obj, err := upload(ctx, driver, key, file, enc)
	if err != nil {
		return res, err
	}
	res.snap = &Snapshot{TargetID: target.ID, TargetName: target.Name, InstallID: installID, Key: key, CreatedAt: at, SizeBytes: obj.SizeBytes, Encrypted: enc != nil}
	res.retentionErr = s.prune(ctx, driver, key)
	return res, nil
}

// snapshotFile returns the file name of a snapshot taken at (sortable by time).
func snapshotFile(at time.Time, encrypted bool) string {
	at = at.UTC()
	key := fmt.Sprintf("mongorescue-%s%03dZ.db", at.Format("20060102T150405"), at.Nanosecond()/int(time.Millisecond))
	if encrypted {
		key += encryption.FileExtension
	}
	return key
}

// upload streams file to key on driver, through enc when it is set.
func upload(ctx context.Context, driver storage.Storage, key, file string, enc *encryption.Encryptor) (*models.StorageObject, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("metabackup: open snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()
	if enc == nil {
		obj, saveErr := driver.Save(ctx, key, f)
		if saveErr != nil {
			return nil, fmt.Errorf("metabackup: store snapshot: %w", saveErr)
		}
		return obj, nil
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- encryptTo(pw, f, enc)
	}()
	obj, saveErr := driver.Save(ctx, key, pr)
	// Unblock the encrypting goroutine if Save stopped reading early.
	_ = pr.CloseWithError(errors.New("metabackup: upload ended"))
	encErr := <-done
	if saveErr != nil {
		return nil, fmt.Errorf("metabackup: store snapshot: %w", saveErr)
	}
	if encErr != nil {
		_ = driver.Delete(context.WithoutCancel(ctx), key)
		return nil, encErr
	}
	return obj, nil
}

// encryptTo encrypts src into pw and closes pw with the outcome.
func encryptTo(pw *io.PipeWriter, src io.Reader, enc *encryption.Encryptor) error {
	w, err := enc.Encrypt(pw)
	if err == nil {
		if _, err = io.Copy(w, src); err == nil {
			err = w.Close()
		}
	}
	if err != nil {
		err = fmt.Errorf("metabackup: encrypt snapshot: %w", err)
		_ = pw.CloseWithError(err)
		return err
	}
	return pw.Close()
}

// prune deletes all but the newest retention-count snapshots on driver; keep is the
// snapshot just stored, which is never deleted, and neither is a snapshot younger
// than the delete grace period (or one whose age is unknown): like a deleted
// backup, a metadata snapshot stays recoverable for at least the grace period.
func (s *Service) prune(ctx context.Context, driver storage.Storage, keep string) error {
	cutoff := s.now().Add(-s.deleteGrace())
	limit := s.settings().RetentionCount
	if limit < 1 {
		limit = 1
	}
	prefix := s.Prefix()
	objects, err := driver.List(ctx, prefix)
	if err != nil {
		return fmt.Errorf("metabackup: list snapshots: %w", err)
	}
	keys := make([]string, 0, len(objects))
	for _, o := range objects {
		// Only this installation's own snapshots, directly under its prefix.
		if o == nil || !strings.HasPrefix(o.Key, prefix) {
			continue
		}
		if name := strings.TrimPrefix(o.Key, prefix); snapshotName.MatchString(name) {
			keys = append(keys, o.Key)
		}
	}
	// Keys sort by time; ".db" and ".db.age" of the same instant cannot both exist.
	slices.SortFunc(keys, func(a, b string) int { return strings.Compare(b, a) })
	var errs []error
	kept := 0
	for _, k := range keys {
		if k == keep || kept < limit-1 {
			if k != keep {
				kept++
			}
			continue
		}
		if at, ok := snapshotTime(strings.TrimPrefix(k, prefix)); !ok || at.After(cutoff) {
			continue
		}
		if err := driver.Delete(ctx, k); err != nil && !errors.Is(err, storage.ErrNotFound) {
			errs = append(errs, fmt.Errorf("delete %s: %w", k, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("metabackup: retention: %w", errors.Join(errs...))
	}
	return nil
}

// removeStaleTemp removes temporary snapshot directories left by a crash.
func (s *Service) removeStaleTemp() {
	matches, err := filepath.Glob(filepath.Join(s.cfg.DataDir, tempDirPattern))
	if err != nil {
		return
	}
	for _, m := range matches {
		if err := os.RemoveAll(m); err != nil {
			s.logger.Warn("could not remove a stale temporary metadata snapshot", logsafe.Error(err))
		}
	}
}

// storedStatus is the persisted part of Status.
type storedStatus struct {
	LastRunAt      *time.Time `json:"last_run_at,omitempty"`
	LastTrigger    string     `json:"last_trigger,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	RetentionError string     `json:"retention_error,omitempty"`
	Last           *Snapshot  `json:"last,omitempty"`
}

// load returns the stored status (zero when none or unreadable).
func (s *Service) load(ctx context.Context) storedStatus {
	var st storedStatus
	if _, err := s.cfg.Store.LoadIntegrityState(ctx, stateKey, &st); err != nil {
		s.logger.Warn("could not load the metadata backup status", logsafe.Error(err))
		return storedStatus{}
	}
	return st
}

// save persists st; a failure is logged.
func (s *Service) save(ctx context.Context, st storedStatus) {
	if err := s.cfg.Store.SaveIntegrityState(context.WithoutCancel(ctx), stateKey, st); err != nil {
		s.logger.Error("could not save the metadata backup status", logsafe.Error(err))
	}
}

// setRunning records whether a snapshot is running.
func (s *Service) setRunning(v bool) {
	s.mu.Lock()
	s.running = v
	s.mu.Unlock()
}

// Status returns the current status.
func (s *Service) Status(ctx context.Context) Status {
	st := s.load(ctx)
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	return Status{
		Enabled: s.settings().Enabled, Prefix: s.Prefix(), Running: running, LastRunAt: st.LastRunAt, LastTrigger: st.LastTrigger,
		LastError: st.LastError, RetentionError: st.RetentionError, Last: st.Last, NextRunAt: s.nextRun(st),
	}
}

// Latest returns the last stored snapshot, or nil when none was stored.
func (s *Service) Latest(ctx context.Context) *Snapshot {
	return s.load(ctx).Last
}

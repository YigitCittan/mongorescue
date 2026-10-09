// Package targets manages storage targets: the local directories and S3-compatible
// buckets backups are written to. It validates targets, keeps stored secrets on
// masked updates, tests targets with a small probe object and hands out one storage
// driver per target (cached, rebuilt when the target changes).
//
// The package is the domain core; persistence (Repository) and driver construction
// (Factory) are ports implemented by internal/store and internal/storage.
package targets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// Sentinel errors.
var (
	// ErrNotFound is returned for an unknown target ID.
	ErrNotFound = errors.New("targets: storage target not found")
	// ErrNoDefault is returned when no default target exists.
	ErrNoDefault = errors.New("targets: no default storage target")
	// ErrInUse is returned when deleting a target that jobs or restorable backups use.
	ErrInUse = errors.New("targets: storage target is in use")
	// ErrIsDefault is returned when deleting the default target.
	ErrIsDefault = errors.New("targets: the default storage target cannot be deleted; make another target the default first")
	// ErrInvalid is returned for invalid target input.
	ErrInvalid = errors.New("targets: invalid storage target")
	// ErrMaskedSecret is returned when the masked secret key is sent back but it cannot
	// stand for the stored one (new target, or endpoint, bucket or access key changed).
	ErrMaskedSecret = errors.New("targets: secret access key is masked; enter it again")
	// ErrConflict is returned when a target changed between reading and updating it.
	ErrConflict = errors.New("targets: the storage target was changed meanwhile; reload it and try again")
	// ErrLocationInUse is returned when changing where a target stores archives (type,
	// local path, S3 endpoint, bucket or prefix) while backups are stored on it: their
	// records would point to a place that does not hold them.
	ErrLocationInUse = errors.New("targets: the location of a storage target that holds backups cannot change")
	// ErrLocationOverlap is returned when a target would store archives where another
	// target does: the same or a nested local directory, or the same S3 endpoint and
	// bucket with an equal or nested prefix. Two targets naming one object would let a
	// purge on one delete an archive the other still holds.
	ErrLocationOverlap = errors.New("targets: the location equals or overlaps the location of another storage target")
	// ErrUnverifiedChange is returned when new credentials (or region or path style)
	// of a target that holds backups fail a connection test: saving them would cut
	// those backups off.
	ErrUnverifiedChange = errors.New("targets: the changed settings of a storage target that holds backups failed a connection test")
)

// usageCounter counts the backup records that keep a target in use, deleted ones
// waiting for their purge included (implemented by *store.SQLiteStore).
type usageCounter interface {
	CountStorageTargetBackups(ctx context.Context, id string) (int, error)
}

// failedRecords lists the failed and cancelled records of a target that name a
// storage key, and deletes a target without counting the ones listed (implemented
// by *store.SQLiteStore).
type failedRecords interface {
	FailedBackupKeys(ctx context.Context, id string) (map[string]string, error)
	DeleteStorageTargetIgnoring(ctx context.Context, id string, ignore []string) error
}

// DefaultTestTimeout bounds storage target tests.
const DefaultTestTimeout = 15 * time.Second

// ProbePrefix is the key prefix of the small objects written by tests. Probe objects
// live directly in the target's root (or S3 prefix), so a test creates no directory.
const ProbePrefix = ".mongorescue-probe-"

// DefaultLocalName is the name of the target created on first start.
const DefaultLocalName = "Local disk"

// Limits for user-supplied fields.
const (
	maxNameLength   = 100
	maxFieldLength  = 1024
	minBucketLength = 3
	maxBucketLength = 63
)

// Repository is the persistence port for storage targets. Implementations store the
// S3 secret key encrypted and return copies callers may mutate.
type Repository interface {
	// ListStorageTargets returns all targets sorted by name.
	ListStorageTargets(ctx context.Context) ([]*models.StorageTarget, error)
	// GetStorageTarget returns a target or ErrNotFound.
	GetStorageTarget(ctx context.Context, id string) (*models.StorageTarget, error)
	// CreateStorageTarget inserts a new target that is not the default (see
	// SetDefaultStorageTarget). It is the only way a target row is created.
	CreateStorageTarget(ctx context.Context, t *models.StorageTarget) error
	// UpdateStorageTarget replaces t (keeping its default flag) only if its stored
	// UpdatedAt still equals expected; it returns ErrNotFound for a deleted target and
	// ErrConflict for one changed meanwhile. With locationChanged it refuses, in the
	// same transaction, with ErrLocationInUse while completed or running backups are
	// stored on the target.
	UpdateStorageTarget(ctx context.Context, t *models.StorageTarget, expected time.Time, locationChanged bool) error
	// RecordStorageTargetTest stores a test outcome only if the target still has the
	// UpdatedAt that was tested, and reports whether it did.
	RecordStorageTargetTest(ctx context.Context, id string, tested, at time.Time, ok bool, testErr string) (bool, error)
	// SetDefaultStorageTarget makes id the only default target, atomically.
	SetDefaultStorageTarget(ctx context.Context, id string) error
	// DeleteStorageTarget removes a target, returning ErrNotFound or, atomically,
	// ErrIsDefault for the default target and ErrInUse while a job or a completed or
	// running backup references it.
	DeleteStorageTarget(ctx context.Context, id string) error
}

// Factory builds the storage driver of a target. localPath is the resolved absolute
// directory of a local target.
type Factory func(ctx context.Context, t *models.StorageTarget, localPath string) (storage.Storage, error)

// Input is the client-editable part of a target.
type Input struct {
	// Name is the display name (required).
	Name string `json:"name"`
	// Type is "local" or "s3".
	Type models.StorageType `json:"type"`
	// Local configures a local target.
	Local *models.LocalTarget `json:"local,omitempty"`
	// S3 configures an S3 target; SecretAccessKey follows the keep-secret rule.
	S3 *models.S3Target `json:"s3,omitempty"`
	// IsDefault makes the new target the default (create only).
	IsDefault bool `json:"is_default,omitempty"`
	// Region labels the region holding the target's data (models.StorageTarget.Region);
	// nil keeps the stored one on an update. An S3 target without a region (here or
	// in s3.region) takes its bucket's location when storage tells it.
	Region *string `json:"region,omitempty"`
}

// TestResult is the outcome of a target test.
type TestResult struct {
	// OK reports whether the probe object was written, read back and deleted.
	OK bool `json:"ok"`
	// LatencyMS is the duration of the probe in milliseconds.
	LatencyMS int64 `json:"latency_ms"`
	// Error is the failure reason (never containing credentials).
	Error string `json:"error,omitempty"`
}

// cached is a driver built for a target version.
type cached struct {
	updatedAt time.Time
	driver    storage.Storage
}

// Service implements the storage target use cases. It is safe for concurrent use.
type Service struct {
	repo    Repository
	factory Factory
	// dataDir is the data directory (never a target location); baseDir, its parent,
	// resolves relative paths stored by earlier builds.
	dataDir     string
	baseDir     string
	logger      *slog.Logger
	now         func() time.Time
	testTimeout time.Duration

	// mu serialises writes (default switching, cache invalidation) and guards drivers.
	mu      sync.Mutex
	drivers map[string]cached
}

// Option customises a Service.
type Option func(*Service)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.logger = l } }

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithTestTimeout overrides DefaultTestTimeout.
func WithTestTimeout(d time.Duration) Option { return func(s *Service) { s.testTimeout = d } }

// NewService returns a Service. dataDir is MongoRescue's data directory: local
// targets must not be it or lie inside it.
func NewService(repo Repository, factory Factory, dataDir string, opts ...Option) *Service {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = filepath.Clean(dataDir)
	}
	s := &Service{
		repo: repo, factory: factory, dataDir: abs, baseDir: filepath.Dir(abs),
		logger: slog.Default(), now: time.Now, testTimeout: DefaultTestTimeout,
		drivers: make(map[string]cached),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// List returns all targets with masked secrets.
func (s *Service) List(ctx context.Context) ([]*models.StorageTarget, error) {
	list, err := s.repo.ListStorageTargets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*models.StorageTarget, 0, len(list))
	for _, t := range list {
		out = append(out, t.Redacted())
	}
	return out, nil
}

// Get returns one target with a masked secret.
func (s *Service) Get(ctx context.Context, id string) (*models.StorageTarget, error) {
	t, err := s.repo.GetStorageTarget(ctx, id)
	if err != nil {
		return nil, err
	}
	return t.Redacted(), nil
}

// Resolve returns target id with its secret, or the default target for an empty id.
// The result must never be serialised to clients or logged.
func (s *Service) Resolve(ctx context.Context, id string) (*models.StorageTarget, error) {
	if id != "" {
		return s.repo.GetStorageTarget(ctx, id)
	}
	list, err := s.repo.ListStorageTargets(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range list {
		if t.IsDefault {
			return t, nil
		}
	}
	return nil, ErrNoDefault
}

// Storage returns the driver of target id (the default target for an empty id). The
// driver is cached per target and rebuilt after the target changes.
func (s *Service) Storage(ctx context.Context, id string) (storage.Storage, error) {
	t, err := s.Resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	c, ok := s.drivers[t.ID]
	s.mu.Unlock()
	if ok && c.updatedAt.Equal(t.UpdatedAt) {
		return c.driver, nil
	}
	driver, err := s.build(ctx, t)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.drivers[t.ID] = cached{updatedAt: t.UpdatedAt, driver: driver}
	s.mu.Unlock()
	return driver, nil
}

// build constructs the driver of t.
func (s *Service) build(ctx context.Context, t *models.StorageTarget) (storage.Storage, error) {
	if s.factory == nil {
		return nil, fmt.Errorf("%w: no storage driver factory", ErrInvalid)
	}
	path := ""
	if t.Type == models.StorageLocal && t.Local != nil {
		path = s.LocalPath(t.Local.Path)
	}
	driver, err := s.factory(ctx, t, path)
	if err != nil {
		return nil, fmt.Errorf("storage target %q: %w", t.Name, err)
	}
	return driver, nil
}

// LocalPath resolves a local target path: absolute paths are kept; relative ones,
// stored only by earlier builds, are joined to the parent of the data directory.
func (s *Service) LocalPath(p string) string {
	p = filepath.Clean(p)
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(s.baseDir, p)
}

// Create validates in and stores a new target, making it the default when requested
// or when it is the first one. It returns the masked result.
func (s *Service) Create(ctx context.Context, in Input) (*models.StorageTarget, error) {
	t, err := s.fromInput(in, nil)
	if err != nil {
		return nil, err
	}
	// A first check keeps the write probe out of another target's place; the one
	// under the lock below decides.
	if err = s.checkOverlap(ctx, t, ""); err != nil {
		return nil, err
	}
	if t.Type == models.StorageLocal {
		if err = s.checkWritable(ctx, t); err != nil {
			return nil, err
		}
	}
	if err = s.checkObjectLock(ctx, t); err != nil {
		return nil, err
	}
	s.detectRegion(ctx, t)
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	t.ID, t.CreatedAt, t.UpdatedAt = id, now, now

	// The overlap check and the write share the lock, so two concurrent creates
	// (or a create and a move) cannot both pass the check for one place.
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.checkOverlap(ctx, t, ""); err != nil {
		return nil, err
	}
	list, err := s.repo.ListStorageTargets(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.repo.CreateStorageTarget(ctx, t); err != nil {
		return nil, err
	}
	if in.IsDefault || len(list) == 0 {
		if err := s.repo.SetDefaultStorageTarget(ctx, t.ID); err != nil {
			return nil, err
		}
		t.IsDefault = true
	}
	s.logger.Info("storage target created", slog.String("storage_target_id", t.ID),
		logsafe.Attr("type", string(t.Type)), logsafe.Attr("location", t.Location()))
	return t.Redacted(), nil
}

// Update replaces the editable fields of target id. It is a real update guarded by
// the target's UpdatedAt: a target deleted or changed by a concurrent request is not
// recreated or overwritten (ErrNotFound, ErrConflict). While backups are stored on
// the target its location (type, local path, S3 endpoint, bucket and prefix) cannot
// change (ErrLocationInUse); the name, credentials, region and path style can. A
// changed configuration clears the last test result and the cached driver.
func (s *Service) Update(ctx context.Context, id string, in Input) (*models.StorageTarget, error) {
	existing, err := s.repo.GetStorageTarget(ctx, id)
	if err != nil {
		return nil, err
	}
	t, err := s.fromInput(in, existing)
	if err != nil {
		return nil, err
	}
	moved := !sameLocation(existing, t)
	if moved {
		if err = s.checkOverlap(ctx, t, existing.ID); err != nil {
			return nil, err
		}
	}
	if t.Type == models.StorageLocal && moved {
		if err = s.checkWritable(ctx, t); err != nil {
			return nil, err
		}
	}
	if !moved && !sameConfig(existing, t) {
		if err = s.verifyInUseChange(ctx, existing, t); err != nil {
			return nil, err
		}
	}
	if err = s.checkObjectLock(ctx, t); err != nil {
		return nil, err
	}
	s.detectRegion(ctx, t)
	t.ID, t.CreatedAt, t.IsDefault = existing.ID, existing.CreatedAt, existing.IsDefault
	t.UpdatedAt = s.now().UTC()
	if !t.UpdatedAt.After(existing.UpdatedAt) {
		t.UpdatedAt = existing.UpdatedAt.Add(time.Microsecond)
	}
	if sameConfig(existing, t) {
		t.LastTestAt, t.LastTestOK, t.LastTestError = existing.LastTestAt, existing.LastTestOK, existing.LastTestError
	}

	// Like in Create, the overlap check and the write share the lock.
	s.mu.Lock()
	defer s.mu.Unlock()
	if moved {
		if err = s.checkOverlap(ctx, t, existing.ID); err != nil {
			return nil, err
		}
	}
	if err = s.repo.UpdateStorageTarget(ctx, t, existing.UpdatedAt, moved); err != nil {
		return nil, err
	}
	delete(s.drivers, t.ID)
	return t.Redacted(), nil
}

// LowerObjectLock applies a held lowering of the S3 Object Lock of target id (see
// models.SplitLockChange): each part of the lock becomes the weaker of the lock in
// force and lowered (models.LowerLock). createdAt, when set, binds the change to the
// target it was requested for: a target created since under the same ID is left
// alone. It reports whether the lock changed. Objects already uploaded keep their
// lock; only later uploads use the lowered one.
func (s *Service) LowerObjectLock(ctx context.Context, id string, lowered models.ObjectLockSettings, createdAt *time.Time) (bool, error) {
	existing, err := s.repo.GetStorageTarget(ctx, id)
	if err != nil {
		return false, err
	}
	if existing.Type != models.StorageS3 || existing.S3 == nil || (createdAt != nil && !existing.CreatedAt.Equal(*createdAt)) {
		return false, nil
	}
	current := existing.S3.LockSettings()
	next := models.LowerLock(current, lowered)
	if next == current {
		return false, nil
	}
	t := existing.Clone()
	t.S3.SetLockSettings(next)
	t.UpdatedAt = s.now().UTC()
	if !t.UpdatedAt.After(existing.UpdatedAt) {
		t.UpdatedAt = existing.UpdatedAt.Add(time.Microsecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.repo.UpdateStorageTarget(ctx, t, existing.UpdatedAt, false); err != nil {
		return false, err
	}
	delete(s.drivers, t.ID)
	s.logger.Info("storage target object lock lowered", slog.String("storage_target_id", t.ID), slog.String("object_lock", next.String()))
	return true, nil
}

// baseLocation is where target t stores its objects, in a form that compares equal
// for every way of naming the same place: "local:" and the resolved directory with a
// trailing separator, or "s3:" and the endpoint, bucket and prefix.
func (s *Service) baseLocation(t *models.StorageTarget) string {
	switch {
	case t.Type == models.StorageLocal && t.Local != nil:
		dir := realPath(s.LocalPath(t.Local.Path))
		return "local:" + foldLocal(strings.TrimSuffix(filepath.ToSlash(dir), "/")+"/")
	case t.Type == models.StorageS3 && t.S3 != nil:
		return "s3:" + endpointKey(t.S3.Endpoint) + "|" + strings.ToLower(t.S3.Bucket) + "|" + storage.NormalizePrefix(t.S3.Prefix)
	}
	return "unknown:" + t.ID
}

// awsEndpoint is the endpoint key of every Amazon S3 endpoint (see endpointKey).
const awsEndpoint = "aws"

// endpointKey returns the S3 endpoint in a form that compares equal for every
// spelling of the same service: without the scheme, with a lower-case host without
// trailing dots, without the default ports 80 and 443 and without trailing slashes.
// The empty endpoint, "aws" and every *.amazonaws.com host (regional, global,
// dual-stack or virtual-host "bucket.s3…") are Amazon S3, where a bucket name is
// unique, so they all map to awsEndpoint and only the bucket and prefix tell
// locations apart.
func endpointKey(endpoint string) string {
	e := strings.TrimSpace(endpoint)
	if e == "" || strings.EqualFold(e, awsEndpoint) {
		return awsEndpoint
	}
	if !strings.Contains(e, "://") {
		e = "//" + e
	}
	u, err := url.Parse(e)
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimRight(strings.TrimSpace(endpoint), "/"))
	}
	host := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	if host == "amazonaws.com" || strings.HasSuffix(host, ".amazonaws.com") {
		return awsEndpoint
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		host += ":" + port
	}
	return host + strings.TrimRight(u.EscapedPath(), "/")
}

// checkOverlap refuses t when its location equals or overlaps that of another target
// (any but selfID): one location is a prefix of the other.
func (s *Service) checkOverlap(ctx context.Context, t *models.StorageTarget, selfID string) error {
	list, err := s.repo.ListStorageTargets(ctx)
	if err != nil {
		return err
	}
	mine := s.baseLocation(t)
	for _, o := range list {
		if o.ID == selfID {
			continue
		}
		theirs := s.baseLocation(o)
		if strings.HasPrefix(mine, theirs) || strings.HasPrefix(theirs, mine) {
			return fmt.Errorf("%w: %s stores archives in the same place or one inside the other; choose a separate directory, bucket or prefix",
				ErrLocationOverlap, o.Name)
		}
	}
	return nil
}

// ObjectLocation returns where key on target id is stored, so two records on
// different targets that name the same physical object compare equal (the purge
// checks every target before it deletes an archive). It returns ErrNotFound for an
// unknown target.
func (s *Service) ObjectLocation(ctx context.Context, id, key string) (string, error) {
	t, err := s.Resolve(ctx, id)
	if err != nil {
		return "", err
	}
	return s.objectKey(t, key), nil
}

// objectKey is the location of key on target t (see ObjectLocation).
func (s *Service) objectKey(t *models.StorageTarget, key string) string {
	rel := strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(key)), "/")
	if t.Type == models.StorageLocal {
		rel = foldLocal(rel)
	}
	return s.baseLocation(t) + rel
}

// foldLocalCase reports whether local paths are compared case-insensitively: the
// default file systems of macOS (APFS, HFS+) and Windows (NTFS) ignore case, so
// /Backups and /backups are one directory there. On a case-sensitive volume of
// those systems this only refuses more, never less.
var foldLocalCase = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// foldLocal returns p lower-cased when foldLocalCase is set.
func foldLocal(p string) string {
	if foldLocalCase {
		return strings.ToLower(p)
	}
	return p
}

// verifyInUseChange tests t, the new credentials, region or path style of existing,
// when backup records reference existing: a change that fails the test is refused
// (ErrUnverifiedChange), so wrong credentials can never cut off stored backups. An
// unused target is not tested.
func (s *Service) verifyInUseChange(ctx context.Context, existing, t *models.StorageTarget) error {
	if c, ok := s.repo.(usageCounter); ok {
		n, err := c.CountStorageTargetBackups(ctx, existing.ID)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
	probe := *t
	probe.ID = existing.ID
	if res := s.probe(ctx, &probe); !res.OK {
		return fmt.Errorf("%w (%s); the target keeps its current settings", ErrUnverifiedChange, res.Error)
	}
	return nil
}

// SetDefault makes id the default target.
func (s *Service) SetDefault(ctx context.Context, id string) (*models.StorageTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.repo.SetDefaultStorageTarget(ctx, id); err != nil {
		return nil, err
	}
	t, err := s.repo.GetStorageTarget(ctx, id)
	if err != nil {
		return nil, err
	}
	s.logger.Info("default storage target changed", slog.String("storage_target_id", id))
	return t.Redacted(), nil
}

// Delete removes a target. The default target and targets still used by jobs or by
// backup records (any but purged ones, so deleted backups waiting for their purge
// too) cannot be deleted.
//
// A failed or cancelled backup only blocks the deletion while its object may exist:
// one that never named an object does not count, and one whose object the target
// reports missing (Stat) is not counted either. An object that cannot be checked
// (an unreachable target) keeps blocking.
func (s *Service) Delete(ctx context.Context, id string) error {
	ignore := s.missingFailedObjects(ctx, id)
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if fr, ok := s.repo.(failedRecords); ok {
		err = fr.DeleteStorageTargetIgnoring(ctx, id, ignore)
	} else {
		err = s.repo.DeleteStorageTarget(ctx, id)
	}
	if err != nil {
		return err
	}
	delete(s.drivers, id)
	s.logger.Info("storage target deleted", slog.String("storage_target_id", id))
	return nil
}

// missingFailedObjects returns the failed and cancelled records of target id whose
// object the target reports missing; on any doubt a record is left out (so it keeps
// blocking).
func (s *Service) missingFailedObjects(ctx context.Context, id string) []string {
	fr, ok := s.repo.(failedRecords)
	if !ok || id == "" {
		return nil
	}
	keys, err := fr.FailedBackupKeys(ctx, id)
	if err != nil || len(keys) == 0 {
		return nil
	}
	driver, err := s.Storage(ctx, id)
	if err != nil {
		return nil
	}
	var out []string
	for rid, key := range keys {
		if _, statErr := driver.Stat(ctx, key); errors.Is(statErr, storage.ErrNotFound) {
			out = append(out, rid)
		}
	}
	return out
}

// Test probes stored target id and records the outcome on it.
func (s *Service) Test(ctx context.Context, id string) (TestResult, error) {
	t, err := s.repo.GetStorageTarget(ctx, id)
	if err != nil {
		return TestResult{}, err
	}
	res := s.probe(ctx, t)
	// Record the result even if the client went away meanwhile, but only on the
	// version that was tested: a target deleted or edited during the probe is left
	// alone (never recreated or reverted).
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	stored, err := s.repo.RecordStorageTargetTest(saveCtx, id, t.UpdatedAt, s.now().UTC(), res.OK, res.Error)
	switch {
	case err != nil:
		s.logger.Warn("failed to record storage target test result", slog.String("storage_target_id", id), slog.Any("error", err))
	case !stored:
		s.logger.Debug("storage target changed during its test; result not recorded", slog.String("storage_target_id", id))
	}
	return res, nil
}

// TestInput probes an unsaved target (from the edit form). With a non-empty id, the
// masked secret key stands for the stored one under the keep-secret rule.
func (s *Service) TestInput(ctx context.Context, in Input, id string) (TestResult, error) {
	var existing *models.StorageTarget
	if id != "" {
		var err error
		if existing, err = s.repo.GetStorageTarget(ctx, id); err != nil {
			return TestResult{}, err
		}
	}
	t, err := s.fromInput(in, existing)
	if err != nil {
		return TestResult{}, err
	}
	t.ID = "test"
	// Testing an unsaved form must not create directories: a missing local path is
	// reported instead (saving the target creates it).
	if t.Type == models.StorageLocal {
		path, err := checkedPath(s.LocalPath(t.Local.Path))
		if err != nil {
			return TestResult{}, err
		}
		info, statErr := os.Stat(path)
		switch {
		case errors.Is(statErr, fs.ErrNotExist):
			return TestResult{Error: fmt.Sprintf("path %s does not exist (saving the target creates it)", path)}, nil
		case statErr != nil:
			return TestResult{Error: scrub(statErr, t)}, nil
		case !info.IsDir():
			return TestResult{Error: fmt.Sprintf("path %s is not a directory", path)}, nil
		}
	}
	return s.probe(ctx, t), nil
}

// EnsureDefault creates a local target named DefaultLocalName at path as the
// default when no target exists. It reports whether one was created.
func (s *Service) EnsureDefault(ctx context.Context, path string) (*models.StorageTarget, bool, error) {
	list, err := s.repo.ListStorageTargets(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, t := range list {
		if t.IsDefault {
			return t, false, nil
		}
	}
	if len(list) > 0 {
		// Targets exist but none is the default (should not happen): promote the first.
		if _, err = s.SetDefault(ctx, list[0].ID); err != nil {
			return nil, false, err
		}
		promoted, getErr := s.repo.GetStorageTarget(ctx, list[0].ID)
		return promoted, false, getErr
	}
	created, err := s.Create(ctx, Input{Name: DefaultLocalName, Type: models.StorageLocal,
		Local: &models.LocalTarget{Path: path}, IsDefault: true})
	if err != nil {
		return nil, false, err
	}
	t, err := s.repo.GetStorageTarget(ctx, created.ID)
	return t, true, err
}

// probe writes, reads back and deletes a small object on t within the test timeout.
func (s *Service) probe(ctx context.Context, t *models.StorageTarget) TestResult {
	ctx, cancel := context.WithTimeout(ctx, s.testTimeout)
	defer cancel()
	start := s.now()
	err := s.runProbe(ctx, t)
	res := TestResult{OK: err == nil, LatencyMS: s.now().Sub(start).Milliseconds()}
	if err != nil {
		res.Error = scrub(err, t)
	}
	return res
}

// A target with S3 Object Lock is probed without a lock (a locked probe object could
// not be deleted for days or years), every version of the probe is deleted through
// the locked driver (deleting the key would only add a delete marker), and the
// bucket's Object Lock and versioning are checked afterwards.
func (s *Service) runProbe(ctx context.Context, t *models.StorageTarget) error {
	unlocked := t
	if t.ObjectLocked() {
		unlocked = t.Clone()
		unlocked.S3.ObjectLock, unlocked.S3.RetentionDays, unlocked.S3.LegalHoldOnPin = "", 0, false
	}
	driver, err := s.build(ctx, unlocked)
	if err != nil {
		return err
	}
	cleaner := driver
	if t.ObjectLocked() {
		if cleaner, err = s.build(ctx, t); err != nil {
			return err
		}
	}
	suffix, err := randomHex(8)
	if err != nil {
		return err
	}
	key := ProbePrefix + suffix
	payload := []byte("mongorescue storage probe " + suffix)
	if _, err = driver.Save(ctx, key, bytes.NewReader(payload)); err != nil {
		return fmt.Errorf("write test object: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		until, delErr := storage.Purge(cleanupCtx, cleaner, key, "", s.now())
		switch {
		case delErr != nil && !errors.Is(delErr, storage.ErrNotFound):
			s.logger.Warn("failed to delete storage probe object", slog.String("key", key), logsafe.Error(delErr))
		case until != nil:
			s.logger.Warn("the storage probe object is locked by the bucket's default retention and stays until it ends",
				slog.String("key", key), slog.Time("retain_until", *until))
		}
	}()
	rc, err := driver.Retrieve(ctx, key)
	if err != nil {
		return fmt.Errorf("read test object: %w", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(io.LimitReader(rc, int64(len(payload))+1))
	if err != nil {
		return fmt.Errorf("read test object: %w", err)
	}
	if !bytes.Equal(got, payload) {
		return errors.New("read test object: content differs from what was written")
	}
	if t.ObjectLocked() {
		return lockCheck(ctx, cleaner)
	}
	return nil
}

// lockCheck verifies that the bucket of driver has Object Lock and versioning
// enabled.
func lockCheck(ctx context.Context, driver storage.Storage) error {
	locker, ok := driver.(storage.ObjectLocker)
	if !ok {
		return fmt.Errorf("%w: the storage driver does not support object lock", storage.ErrObjectLockUnavailable)
	}
	return locker.CheckObjectLock(ctx)
}

// checkObjectLock refuses to save an S3 target with an object lock mode whose bucket
// does not have Object Lock and versioning enabled. MongoRescue never enables them
// itself.
func (s *Service) checkObjectLock(ctx context.Context, t *models.StorageTarget) error {
	if !t.ObjectLocked() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.testTimeout)
	defer cancel()
	driver, err := s.build(ctx, t)
	if err == nil {
		err = lockCheck(ctx, driver)
	}
	if err != nil {
		return fmt.Errorf("%w: object lock: %s", ErrInvalid, scrub(err, t))
	}
	return nil
}

// sameRegionSource reports whether a and b are S3 targets on the same endpoint and
// bucket with the same S3 region, so a region detected for a holds for b.
func sameRegionSource(a, b *models.StorageTarget) bool {
	return a.S3 != nil && b.S3 != nil && a.S3.Endpoint == b.S3.Endpoint && a.S3.Bucket == b.S3.Bucket &&
		strings.EqualFold(strings.TrimSpace(a.S3.Region), strings.TrimSpace(b.S3.Region))
}

// detectRegion sets the Region of an AWS S3 target whose region is unknown (no
// label, and its S3 region empty or "auto") to the location of its bucket, when
// storage tells it, and marks it detected. Other endpoints are left alone: only an
// operator's label tells where their data is (see models.StorageTarget.DRRegion).
// It is best effort: a failure leaves the region unknown, which the readiness
// report warns about.
func (s *Service) detectRegion(ctx context.Context, t *models.StorageTarget) {
	if !t.IsAWSS3() || t.DRRegion() != "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, s.testTimeout)
	defer cancel()
	driver, err := s.build(ctx, t)
	if err != nil {
		return
	}
	locator, ok := driver.(storage.RegionLocator)
	if !ok {
		return
	}
	region, err := locator.BucketRegion(ctx)
	if err != nil {
		s.logger.Debug("the bucket region of a storage target is unknown", logsafe.Attr("location", t.Location()), logsafe.Error(err))
		return
	}
	if region != "" && len(region) <= models.MaxRegionLength && !strings.ContainsFunc(region, isControl) {
		t.Region, t.RegionDetected = region, true
	}
}

// checkWritable verifies that a local target's directory can be created and written.
func (s *Service) checkWritable(ctx context.Context, t *models.StorageTarget) error {
	if err := s.runProbe(ctx, t); err != nil {
		return fmt.Errorf("%w: local.path %q is not writable: %s", ErrInvalid, t.Local.Path, scrub(err, t))
	}
	return nil
}

// fromInput validates in and builds a target. existing (may be nil) supplies the
// stored secret for the keep-secret rule.
func (s *Service) fromInput(in Input, existing *models.StorageTarget) (*models.StorageTarget, error) {
	name := strings.TrimSpace(in.Name)
	switch {
	case name == "":
		return nil, fmt.Errorf("%w: name is required", ErrInvalid)
	case len(name) > maxNameLength:
		return nil, fmt.Errorf("%w: name must be at most %d characters", ErrInvalid, maxNameLength)
	case strings.ContainsFunc(name, isControl):
		return nil, fmt.Errorf("%w: name must not contain control characters", ErrInvalid)
	}
	t := &models.StorageTarget{Name: name, Type: in.Type}
	switch {
	case in.Region != nil:
		region := strings.TrimSpace(*in.Region)
		if len(region) > models.MaxRegionLength || strings.ContainsFunc(region, isControl) {
			return nil, fmt.Errorf("%w: region must be printable and at most %d characters", ErrInvalid, models.MaxRegionLength)
		}
		t.Region = region
	}
	switch in.Type {
	case models.StorageLocal:
		if in.Local == nil {
			return nil, fmt.Errorf("%w: local.path is required", ErrInvalid)
		}
		p, err := cleanLocalPath(in.Local.Path)
		if err != nil {
			return nil, err
		}
		// Relative paths were stored by earlier builds; they stay valid as they are.
		unchanged := existing != nil && existing.Local != nil && filepath.Clean(existing.Local.Path) == p
		if !filepath.IsAbs(p) && !unchanged {
			return nil, fmt.Errorf("%w: local.path must be an absolute path such as /backups", ErrInvalid)
		}
		if err := s.checkLocalLocation(p); err != nil {
			return nil, err
		}
		t.Local = &models.LocalTarget{Path: p}
	case models.StorageS3:
		if in.S3 == nil {
			return nil, fmt.Errorf("%w: s3 settings are required", ErrInvalid)
		}
		s3, err := cleanS3(*in.S3)
		if err != nil {
			return nil, err
		}
		if s3.SecretAccessKey == models.SecretMask {
			if existing == nil || existing.S3 == nil || existing.S3.SecretAccessKey == "" ||
				existing.S3.Endpoint != s3.Endpoint || existing.S3.Bucket != s3.Bucket || existing.S3.AccessKeyID != s3.AccessKeyID {
				return nil, ErrMaskedSecret
			}
			s3.SecretAccessKey = existing.S3.SecretAccessKey
		}
		if (s3.AccessKeyID == "") != (s3.SecretAccessKey == "") {
			return nil, fmt.Errorf("%w: s3.access_key_id and s3.secret_access_key must be set together (or both left empty for the AWS default credentials)", ErrInvalid)
		}
		t.S3 = &s3
	default:
		return nil, fmt.Errorf("%w: type must be local or s3", ErrInvalid)
	}
	if in.Region == nil && existing != nil {
		t.Region, t.RegionDetected = existing.Region, existing.RegionDetected
		// A detected region belongs to the bucket it was read from.
		if t.RegionDetected && !sameRegionSource(existing, t) {
			t.Region, t.RegionDetected = "", false
		}
	}
	return t, nil
}

// cleanLocalPath trims and checks a local path: non-empty, no ".." component, no NUL.
func cleanLocalPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		return "", fmt.Errorf("%w: local.path is required", ErrInvalid)
	case len(p) > maxFieldLength || strings.ContainsFunc(p, isControl):
		return "", fmt.Errorf("%w: local.path is not a valid path", ErrInvalid)
	}
	if dotDotElement.MatchString(p) {
		return "", errDotDot
	}
	return filepath.Clean(p), nil
}

// dotDotElement matches a path with a ".." element (separated by / or \); names that
// merely contain two dots, such as "v1..v2", are fine.
var dotDotElement = regexp.MustCompile(`(?:^|[/\\])\.\.(?:[/\\]|$)`)

// errDotDot rejects a local path with a ".." element.
var errDotDot = fmt.Errorf("%w: local.path must not contain \"..\"", ErrInvalid)

// checkedPath is the last check of a resolved local target path before it reaches
// the file system. The path is admin-supplied by design (any directory the server
// may write to), so there is no allowed-roots list; the location policy itself lives
// in checkLocalLocation. checkedPath makes sure the value handed to the file system is
// what validation saw: no NUL or other control character, no ".." element, absolute,
// and cleaned.
func checkedPath(p string) (string, error) {
	if strings.ContainsFunc(p, isControl) {
		return "", fmt.Errorf("%w: local.path is not a valid path", ErrInvalid)
	}
	if dotDotElement.MatchString(p) {
		return "", errDotDot
	}
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%w: local.path must be an absolute path such as /backups", ErrInvalid)
	}
	return p, nil
}

// checkLocalLocation refuses the file system root, the data directory and anything
// inside it, comparing real paths (symbolic links of the nearest existing ancestor
// resolved).
func (s *Service) checkLocalLocation(p string) error {
	resolved, err := checkedPath(s.LocalPath(p))
	if err != nil {
		return err
	}
	target := realPath(resolved)
	if filepath.Dir(target) == target {
		return fmt.Errorf("%w: local.path must not be the root directory", ErrInvalid)
	}
	data := realPath(s.dataDir)
	if rel, err := filepath.Rel(data, target); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: local.path must not be MongoRescue's data directory or inside it", ErrInvalid)
	}
	return nil
}

// realPath returns the absolute p with the symbolic links of its nearest existing
// ancestor resolved; the part below that ancestor is kept as is.
func realPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	for cur := abs; ; cur = filepath.Dir(cur) {
		if _, err := os.Lstat(cur); err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return abs
			}
			rest, err := filepath.Rel(cur, abs)
			if err != nil {
				return abs
			}
			return filepath.Join(resolved, rest)
		}
		if filepath.Dir(cur) == cur {
			return abs
		}
	}
}

// sameLocation reports whether a and b store archives in the same place: the same
// type and local path, or the same S3 endpoint, bucket and prefix.
func sameLocation(a, b *models.StorageTarget) bool {
	if a.Type != b.Type {
		return false
	}
	switch a.Type {
	case models.StorageLocal:
		return a.Local != nil && b.Local != nil && filepath.Clean(a.Local.Path) == filepath.Clean(b.Local.Path)
	case models.StorageS3:
		return a.S3 != nil && b.S3 != nil && a.S3.Endpoint == b.S3.Endpoint && a.S3.Bucket == b.S3.Bucket &&
			storage.NormalizePrefix(a.S3.Prefix) == storage.NormalizePrefix(b.S3.Prefix)
	}
	return false
}

// cleanS3 trims and checks S3 settings.
func cleanS3(in models.S3Target) (models.S3Target, error) {
	out := models.S3Target{
		Endpoint:        strings.TrimRight(strings.TrimSpace(in.Endpoint), "/"),
		Region:          strings.TrimSpace(in.Region),
		Bucket:          strings.TrimSpace(in.Bucket),
		Prefix:          storage.NormalizePrefix(in.Prefix),
		AccessKeyID:     strings.TrimSpace(in.AccessKeyID),
		SecretAccessKey: strings.TrimSpace(in.SecretAccessKey),
		UsePathStyle:    in.UsePathStyle,
		PartSizeMB:      in.PartSizeMB,
		ObjectLock:      models.ObjectLockMode(strings.ToLower(strings.TrimSpace(string(in.ObjectLock)))),
		RetentionDays:   in.RetentionDays,
		LegalHoldOnPin:  in.LegalHoldOnPin,
	}
	switch out.ObjectLock {
	case "", models.ObjectLockNone:
		if out.LegalHoldOnPin {
			return out, fmt.Errorf("%w: s3.legal_hold_on_pin needs s3.object_lock governance or compliance", ErrInvalid)
		}
		out.ObjectLock, out.RetentionDays = "", 0
	case models.ObjectLockGovernance, models.ObjectLockCompliance:
		if out.RetentionDays < models.MinObjectLockRetentionDays || out.RetentionDays > models.MaxObjectLockRetentionDays {
			return out, fmt.Errorf("%w: s3.retention_days must be %d to %d with an object lock", ErrInvalid,
				models.MinObjectLockRetentionDays, models.MaxObjectLockRetentionDays)
		}
	default:
		return out, fmt.Errorf("%w: s3.object_lock must be none, governance or compliance", ErrInvalid)
	}
	if out.PartSizeMB == 0 {
		out.PartSizeMB = models.DefaultS3PartSizeMB
	}
	if out.PartSizeMB < models.MinS3PartSizeMB || out.PartSizeMB > models.MaxS3PartSizeMB {
		return out, fmt.Errorf("%w: s3.part_size_mb must be %d to %d (MiB)", ErrInvalid, models.MinS3PartSizeMB, models.MaxS3PartSizeMB)
	}
	for _, v := range []string{out.Endpoint, out.Region, out.Bucket, out.Prefix, out.AccessKeyID, out.SecretAccessKey} {
		if len(v) > maxFieldLength || strings.ContainsFunc(v, isControl) {
			return out, fmt.Errorf("%w: s3 fields must be printable and at most %d characters", ErrInvalid, maxFieldLength)
		}
	}
	if out.Endpoint != "" {
		u, err := url.Parse(out.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || strings.ContainsAny(out.Endpoint, "<>{}") {
			return out, fmt.Errorf("%w: s3.endpoint must be an http(s) URL such as https://s3.example.com", ErrInvalid)
		}
	}
	if len(out.Bucket) < minBucketLength || len(out.Bucket) > maxBucketLength || strings.ContainsAny(out.Bucket, " /\\") {
		return out, fmt.Errorf("%w: s3.bucket must be a bucket name (%d-%d characters, no slashes)", ErrInvalid, minBucketLength, maxBucketLength)
	}
	for _, part := range strings.Split(out.Prefix, "/") {
		if part == ".." {
			return out, fmt.Errorf("%w: s3.prefix must not contain \"..\"", ErrInvalid)
		}
	}
	return out, nil
}

// sameConfig reports whether a and b store to the same place with the same
// credentials. The S3 part size is ignored: it changes neither.
func sameConfig(a, b *models.StorageTarget) bool {
	if a.Type != b.Type {
		return false
	}
	switch a.Type {
	case models.StorageLocal:
		return a.Local != nil && b.Local != nil && *a.Local == *b.Local
	case models.StorageS3:
		if a.S3 == nil || b.S3 == nil {
			return false
		}
		x, y := *a.S3, *b.S3
		x.PartSizeMB, y.PartSizeMB = 0, 0
		return x == y
	}
	return false
}

// scrub renders err without the target's secret key.
func scrub(err error, t *models.StorageTarget) string {
	msg := err.Error()
	if t.S3 != nil && t.S3.SecretAccessKey != "" {
		msg = strings.ReplaceAll(msg, t.S3.SecretAccessKey, redact.Mask)
	}
	return redact.Text(msg)
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("targets: random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// NewID returns a new random target ID ("stg_" + 16 hex characters).
func NewID() (string, error) {
	h, err := randomHex(8)
	if err != nil {
		return "", err
	}
	return "stg_" + h, nil
}

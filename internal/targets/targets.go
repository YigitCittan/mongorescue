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
	"path/filepath"
	"strings"
	"sync"
	"time"

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
)

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
	if t.Type == models.StorageLocal {
		if err = s.checkWritable(ctx, t); err != nil {
			return nil, err
		}
	}
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	t.ID, t.CreatedAt, t.UpdatedAt = id, now, now

	s.mu.Lock()
	defer s.mu.Unlock()
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
		slog.String("type", string(t.Type)), slog.String("location", t.Location()))
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
	if t.Type == models.StorageLocal && moved {
		if err = s.checkWritable(ctx, t); err != nil {
			return nil, err
		}
	}
	t.ID, t.CreatedAt, t.IsDefault = existing.ID, existing.CreatedAt, existing.IsDefault
	t.UpdatedAt = s.now().UTC()
	if !t.UpdatedAt.After(existing.UpdatedAt) {
		t.UpdatedAt = existing.UpdatedAt.Add(time.Microsecond)
	}
	if sameConfig(existing, t) {
		t.LastTestAt, t.LastTestOK, t.LastTestError = existing.LastTestAt, existing.LastTestOK, existing.LastTestError
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.repo.UpdateStorageTarget(ctx, t, existing.UpdatedAt, moved); err != nil {
		return nil, err
	}
	delete(s.drivers, t.ID)
	return t.Redacted(), nil
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
// restorable backups cannot be deleted.
func (s *Service) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.repo.DeleteStorageTarget(ctx, id); err != nil {
		return err
	}
	delete(s.drivers, id)
	s.logger.Info("storage target deleted", slog.String("storage_target_id", id))
	return nil
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
		path := s.LocalPath(t.Local.Path)
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

func (s *Service) runProbe(ctx context.Context, t *models.StorageTarget) error {
	driver, err := s.build(ctx, t)
	if err != nil {
		return err
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
		if err = driver.Delete(cleanupCtx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			s.logger.Warn("failed to delete storage probe object", slog.String("key", key), slog.Any("error", err))
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
	return nil
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
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return "", fmt.Errorf("%w: local.path must not contain \"..\"", ErrInvalid)
		}
	}
	return filepath.Clean(p), nil
}

// checkLocalLocation refuses the file system root, the data directory and anything
// inside it, comparing real paths (symbolic links of the nearest existing ancestor
// resolved).
func (s *Service) checkLocalLocation(p string) error {
	target := realPath(s.LocalPath(p))
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

// sameConfig reports whether a and b store to the same place with the same credentials.
func sameConfig(a, b *models.StorageTarget) bool {
	if a.Type != b.Type {
		return false
	}
	switch a.Type {
	case models.StorageLocal:
		return a.Local != nil && b.Local != nil && *a.Local == *b.Local
	case models.StorageS3:
		return a.S3 != nil && b.S3 != nil && *a.S3 == *b.S3
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

package integrity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Storage scan limits and state.
const (
	// MaxReportedOrphans caps the orphans listed in a DriftReport (all are counted).
	MaxReportedOrphans = 500
	// MaxReportedMissing caps the missing archives listed in a DriftReport.
	MaxReportedMissing = 500
	// ScanInterval is the period of the scheduled storage scans.
	ScanInterval = 7 * 24 * time.Hour
	// stateScanPrefix prefixes the state document of a target's latest scan;
	// stateScanAll holds when all targets were last scanned on schedule.
	stateScanPrefix = "scan:"
	stateScanAll    = "scan_all"
)

// Orphan is an archive on a storage target without a backup record.
type Orphan struct {
	// Key is the object key (relative to the target's root or prefix).
	Key string `json:"key"`
	// SizeBytes and ModTime describe the object.
	SizeBytes int64     `json:"size_bytes"`
	ModTime   time.Time `json:"mod_time"`
	// Database is the database the key layout names, if any.
	Database string `json:"database,omitempty"`
	// RecordID and RecordStatus name a failed or pruned record that still points at
	// the object (a leftover that retention or a failed run could not delete).
	RecordID     string `json:"record_id,omitempty"`
	RecordStatus string `json:"record_status,omitempty"`
}

// MissingArchive is a backup record whose archive a scan did not find.
type MissingArchive struct {
	// BackupID, Database and StorageKey identify the backup.
	BackupID   string `json:"backup_id"`
	Database   string `json:"database"`
	StorageKey string `json:"storage_key"`
	// StartedAt is when the backup was taken.
	StartedAt time.Time `json:"started_at"`
}

// DriftReport is the outcome of a storage scan of one target.
type DriftReport struct {
	// TargetID and TargetName identify the storage target.
	TargetID   string `json:"target_id"`
	TargetName string `json:"target_name"`
	// Source is "scheduled" or "manual".
	Source string `json:"source"`
	// ScannedAt is when the scan finished; DurationSeconds how long it took.
	ScannedAt       time.Time `json:"scanned_at"`
	DurationSeconds float64   `json:"duration_seconds"`
	// Objects counts the archives found; Ignored the other objects (probe files,
	// unrelated data).
	Objects int `json:"objects"`
	Ignored int `json:"ignored"`
	// OrphanCount and Orphans are the archives without a record (the list is capped
	// at MaxReportedOrphans).
	OrphanCount int      `json:"orphan_count"`
	Orphans     []Orphan `json:"orphans"`
	// MissingCount and Missing are the records whose archive is gone (now marked
	// missing).
	MissingCount int              `json:"missing_count"`
	Missing      []MissingArchive `json:"missing"`
	// Recovered lists backups marked missing earlier whose archive is back.
	Recovered []string `json:"recovered"`
	// Error is set when the scan failed (the counts are then incomplete).
	Error string `json:"error,omitempty"`
}

// Drift reports whether the scan found orphan or missing archives.
func (r *DriftReport) Drift() bool { return r.OrphanCount > 0 || r.MissingCount > 0 }

// archiveKey matches the keys of backup archives: "<anything>.archive", optionally
// gzipped and age-encrypted.
var archiveKey = regexp.MustCompile(`\.archive(\.gz)?(\.age)?$`)

// defaultLayout matches the default key layout "<db>/<YYYY>/<MM>/<backup id>.archive…".
var defaultLayout = regexp.MustCompile(`^([^/]+)/(\d{4})/(\d{2})/(bkp_[A-Za-z0-9_-]+)\.archive(\.gz)?(\.age)?$`)

// idTimestamp finds the "_YYYYMMDD_HHMMSS" timestamp in a backup ID.
var idTimestamp = regexp.MustCompile(`_(\d{8}_\d{6})(?:_|$)`)

// IsArchiveKey reports whether key names a backup archive.
func IsArchiveKey(key string) bool { return archiveKey.MatchString(key) }

// liveStatus reports whether a record with status st owns its storage key: its
// object is expected to exist (or to be written right now).
func liveStatus(st models.BackupStatus) bool {
	switch st {
	case models.StatusCompleted, models.StatusMissing, models.StatusInProgress, models.StatusPending:
		return true
	default:
		return false
	}
}

// targetRecords returns the backup records stored on target id, keyed by storage
// key (a live record wins over a failed or pruned one with the same key).
func (s *Service) targetRecords(ctx context.Context, targetID string) ([]*models.BackupRecord, map[string]*models.BackupRecord, error) {
	all, err := s.cfg.Store.ListBackupRecords(ctx, "")
	if err != nil {
		return nil, nil, fmt.Errorf("list backups: %w", err)
	}
	var list []*models.BackupRecord
	byKey := make(map[string]*models.BackupRecord)
	for _, r := range all {
		if r.StorageTargetID != targetID || r.StorageKey == "" {
			continue
		}
		list = append(list, r)
		if prev, ok := byKey[r.StorageKey]; !ok || (!liveStatus(prev.Status) && liveStatus(r.Status)) {
			byKey[r.StorageKey] = r
		}
	}
	return list, byKey, nil
}

// ScanTarget lists the archives on storage target targetID, compares them with the
// backup records and returns (and stores) the drift report: orphan archives and
// records whose archive is gone, which are marked models.StatusMissing (and marked
// completed again once their archive is back). Nothing is deleted. source is
// TriggerScheduled or TriggerManual. Expected failures: ErrNotFound and ErrBusy.
func (s *Service) ScanTarget(ctx context.Context, targetID, source string) (*DriftReport, error) {
	target, err := s.cfg.Targets.Resolve(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("%w: storage target %s", ErrNotFound, targetID)
	}
	release, err := s.cfg.Runs.Acquire(keyPrefixScan + target.ID)
	if err != nil {
		return nil, err
	}
	defer release()

	start := s.now()
	report := &DriftReport{TargetID: target.ID, TargetName: target.Name, Source: source,
		Orphans: []Orphan{}, Missing: []MissingArchive{}, Recovered: []string{}}
	if err := s.scan(ctx, target, start, report); err != nil {
		report.Error = redact.Text(err.Error())
		s.logger.Warn("storage scan failed", slog.String("storage_target_id", target.ID), slog.String("error", report.Error))
	}
	report.ScannedAt = s.now()
	report.DurationSeconds = report.ScannedAt.Sub(start).Seconds()

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	if err := s.cfg.Store.SaveIntegrityState(writeCtx, stateScanPrefix+target.ID, report); err != nil {
		s.logger.Warn("failed to save the storage scan", slog.Any("error", err))
	}
	if report.Error == "" {
		if s.cfg.ObserveScan != nil {
			s.cfg.ObserveScan(target.ID, report.OrphanCount, report.MissingCount, report.ScannedAt)
		}
		if report.Drift() {
			s.publish(writeCtx, events.DriftEvent(target.ID, target.Name, report.OrphanCount, report.MissingCount, source, report.ScannedAt))
		}
	}
	s.logger.Info("storage scan finished", slog.String("storage_target_id", target.ID), slog.Int("archives", report.Objects),
		slog.Int("orphans", report.OrphanCount), slog.Int("missing", report.MissingCount), slog.Int("recovered", len(report.Recovered)))
	return report, nil
}

// scan fills report for target. Objects are listed before the records are read, so
// every listed archive of a running backup already has its record; records are
// only reported missing when they completed before the listing started.
func (s *Service) scan(ctx context.Context, target *models.StorageTarget, listStart time.Time, report *DriftReport) error {
	driver, err := s.cfg.Targets.Storage(ctx, target.ID)
	if err != nil {
		return fmt.Errorf("open storage target: %w", err)
	}
	objects, err := driver.List(ctx, "")
	if err != nil {
		return fmt.Errorf("list objects: %w", err)
	}
	records, byKey, err := s.targetRecords(ctx, target.ID)
	if err != nil {
		return err
	}

	present := make(map[string]bool, len(objects))
	for _, obj := range objects {
		if obj == nil || !IsArchiveKey(obj.Key) {
			report.Ignored++
			continue
		}
		report.Objects++
		present[obj.Key] = true
		rec := byKey[obj.Key]
		if rec != nil && liveStatus(rec.Status) {
			continue
		}
		report.OrphanCount++
		if len(report.Orphans) < MaxReportedOrphans {
			o := Orphan{Key: obj.Key, SizeBytes: obj.SizeBytes, ModTime: obj.ModTime, Database: parseKey(obj.Key).database}
			if rec != nil {
				o.RecordID, o.RecordStatus = rec.ID, string(rec.Status)
			}
			report.Orphans = append(report.Orphans, o)
		}
	}

	// A listing without a single archive while backups are recorded on the target
	// means a wrong prefix or an unmounted directory far more often than lost data:
	// nothing is marked missing then.
	if report.Objects == 0 {
		recorded := 0
		for _, rec := range records {
			if rec.Status == models.StatusCompleted {
				recorded++
			}
		}
		if recorded > 0 {
			return fmt.Errorf("%w: the target lists no archive at all although %d completed backup(s) are recorded on it; check its location (nothing was marked missing)", ErrEmptyListing, recorded)
		}
	}

	for _, rec := range records {
		switch {
		case rec.Status == models.StatusMissing && present[rec.StorageKey]:
			if s.markFound(ctx, rec.ID, rec.StorageKey) {
				report.Recovered = append(report.Recovered, rec.ID)
			}
		case rec.Status == models.StatusMissing:
			s.addMissing(report, rec)
		case rec.Status == models.StatusCompleted && !present[rec.StorageKey] &&
			rec.CompletedAt != nil && rec.CompletedAt.Before(listStart):
			if s.markMissing(ctx, rec.ID, rec.StorageKey) {
				s.addMissing(report, rec)
			}
		}
	}
	return nil
}

// addMissing counts rec as missing in report.
func (s *Service) addMissing(report *DriftReport, rec *models.BackupRecord) {
	report.MissingCount++
	if len(report.Missing) < MaxReportedMissing {
		report.Missing = append(report.Missing, MissingArchive{BackupID: rec.ID, Database: rec.Database, StorageKey: rec.StorageKey, StartedAt: rec.StartedAt})
	}
}

// markMissing marks completed record id missing; it reports whether it did.
func (s *Service) markMissing(ctx context.Context, id, key string) bool {
	now := s.now()
	_, err := s.cfg.Store.UpdateBackupRecord(context.WithoutCancel(ctx), id, func(r *models.BackupRecord) error {
		if r.Status != models.StatusCompleted || r.StorageKey != key {
			return errRecordChanged
		}
		r.Status, r.MissingSince = models.StatusMissing, &now
		return nil
	})
	if err != nil {
		if !errors.Is(err, errRecordChanged) && !errors.Is(err, store.ErrNotFound) {
			s.logger.Warn("failed to mark a backup missing", slog.String("backup_id", id), slog.Any("error", err))
		}
		return false
	}
	s.logger.Warn("backup archive is missing from its storage target", slog.String("backup_id", id), slog.String("storage_key", key))
	return true
}

// markFound marks missing record id completed again; it reports whether it did.
func (s *Service) markFound(ctx context.Context, id, key string) bool {
	_, err := s.cfg.Store.UpdateBackupRecord(context.WithoutCancel(ctx), id, func(r *models.BackupRecord) error {
		if r.Status != models.StatusMissing || r.StorageKey != key {
			return errRecordChanged
		}
		r.Status, r.MissingSince = models.StatusCompleted, nil
		return nil
	})
	return err == nil
}

// ScanAll scans every storage target, one after the other, and records when it ran.
func (s *Service) ScanAll(ctx context.Context, source string) error {
	targets, err := s.cfg.Targets.List(ctx)
	if err != nil {
		return fmt.Errorf("list storage targets: %w", err)
	}
	var errs []error
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		if _, err := s.ScanTarget(ctx, t.ID, source); err != nil && !errors.Is(err, ErrBusy) {
			errs = append(errs, fmt.Errorf("scan %s: %w", t.ID, err))
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	if err := s.cfg.Store.SaveIntegrityState(writeCtx, stateScanAll, scanAllState{LastRunAt: s.now()}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// scanAllState records when every target was last scanned on schedule.
type scanAllState struct {
	LastRunAt time.Time `json:"last_run_at"`
}

// nextScan returns when the scheduled storage scan is due, or nil when it is off.
func (s *Service) nextScan(ctx context.Context) *time.Time {
	if !s.settings().Integrity.StorageScan {
		return nil
	}
	next := s.now()
	var st scanAllState
	if ok, err := s.cfg.Store.LoadIntegrityState(ctx, stateScanAll, &st); err == nil && ok {
		if due := st.LastRunAt.Add(ScanInterval); due.After(next) {
			next = due
		}
	}
	return &next
}

// LastScan returns the latest scan of target targetID.
func (s *Service) LastScan(ctx context.Context, targetID string) (*DriftReport, bool) {
	var r DriftReport
	ok, err := s.cfg.Store.LoadIntegrityState(ctx, stateScanPrefix+targetID, &r)
	if err != nil || !ok {
		return nil, false
	}
	return &r, true
}

// keyInfo is what a storage key reveals about a backup.
type keyInfo struct {
	database  string
	id        string
	startedAt time.Time
	gzip      bool
	encrypted bool
}

// parseKey reads the database, backup ID, start time, compression and encryption
// from a key of the default layout "<db>/<YYYY>/<MM>/<id>.archive[.gz][.age]";
// other archive keys yield the first path element as database (if any) and the
// suffixes.
func parseKey(key string) keyInfo {
	info := keyInfo{
		gzip:      strings.Contains(path.Base(key), ".archive.gz"),
		encrypted: strings.HasSuffix(key, encryption.FileExtension),
	}
	if m := defaultLayout.FindStringSubmatch(key); m != nil {
		info.database, info.id = m[1], m[4]
		if ts := idTimestamp.FindStringSubmatch(m[4]); ts != nil {
			if t, err := time.Parse("20060102_150405", ts[1]); err == nil {
				info.startedAt = t.UTC()
			}
		}
		return info
	}
	if dir, _, ok := strings.Cut(key, "/"); ok && models.ValidateDatabaseName(dir) == nil {
		info.database = dir
	}
	return info
}

// StartImport creates a backup record for the orphan archive key on storage target
// targetID and hashes the archive in the background: the record is pending until
// its size and SHA-256 are known, then completed, imported and unverified. A failed
// import marks the record failed and detaches it from the key, so the archive stays
// an orphan and is never deleted through the record. Expected failures:
// ErrNotFound, ErrNotOrphan, ErrBusy and runs.ErrShuttingDown.
func (s *Service) StartImport(ctx context.Context, targetID, key string) (*models.BackupRecord, error) {
	key = strings.TrimSpace(key)
	if !IsArchiveKey(key) {
		return nil, fmt.Errorf("%w: %q is not a backup archive key", ErrNotOrphan, key)
	}
	target, err := s.cfg.Targets.Resolve(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("%w: storage target %s", ErrNotFound, targetID)
	}
	driver, err := s.cfg.Targets.Storage(ctx, target.ID)
	if err != nil {
		return nil, fmt.Errorf("open storage target: %w", err)
	}
	obj, err := driver.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrInvalidKey) || errors.Is(err, storage.ErrPathTraversal) {
			return nil, fmt.Errorf("%w: object %q on storage target %s", ErrNotFound, key, target.Name)
		}
		return nil, fmt.Errorf("stat object: %w", err)
	}
	_, byKey, err := s.targetRecords(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	if rec := byKey[key]; rec != nil && liveStatus(rec.Status) {
		return nil, fmt.Errorf("%w: it belongs to backup %s", ErrNotOrphan, rec.ID)
	}

	rec, err := s.importRecord(ctx, target, key, obj)
	if err != nil {
		return nil, err
	}
	release, err := s.cfg.Runs.Acquire(keyPrefixImport + target.ID + "/" + key)
	if err != nil {
		return nil, err
	}
	snapshot := *rec
	if err := s.cfg.Store.SaveBackupRecord(ctx, rec); err != nil {
		release()
		return nil, fmt.Errorf("save imported record: %w", err)
	}
	if err := s.cfg.Runs.Go("", func(runCtx context.Context) {
		defer release()
		s.finishImport(runCtx, driver, rec)
	}); err != nil {
		release()
		s.failImport(ctx, rec, err)
		return nil, err
	}
	return &snapshot, nil
}

// importRecord builds the pending record of an imported archive.
func (s *Service) importRecord(ctx context.Context, target *models.StorageTarget, key string, obj *models.StorageObject) (*models.BackupRecord, error) {
	info := parseKey(key)
	now := s.now()
	started := info.startedAt
	if started.IsZero() {
		started = obj.ModTime.UTC()
	}
	if started.IsZero() {
		started = now
	}
	database := info.database
	if database == "" {
		database = "imported"
	}
	id := info.id
	if id != "" {
		if models.ValidateID(id) != nil {
			id = ""
		} else if _, err := s.cfg.Store.GetBackupRecord(ctx, id); err == nil {
			id = "" // the ID is taken (e.g. by the pruned record of the same archive)
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("check backup id: %w", err)
		}
	}
	if id == "" {
		suffix, err := models.NewIDSuffix()
		if err != nil {
			return nil, err
		}
		const overhead = len("bkp___") + len("20060102_150405") + models.IDSuffixLength
		id = fmt.Sprintf("bkp_%s_%s_%s", models.SanitizeIDComponent(database, models.MaxIDLength-overhead), started.Format("20060102_150405"), suffix)
	}
	rec := &models.BackupRecord{
		ID: id, Trigger: models.TriggerManual, Database: database, Status: models.StatusPending,
		StorageType: target.Type, StorageTargetID: target.ID, StorageTargetName: target.Name, StorageKey: key,
		Encrypted: info.encrypted, StartedAt: started, Imported: true, ImportedAt: &now,
	}
	return rec, nil
}

// finishImport hashes the imported archive and completes (or fails) its record.
func (s *Service) finishImport(ctx context.Context, driver storage.Storage, rec *models.BackupRecord) {
	sum, size, err := hashObject(ctx, driver, rec.StorageKey)
	if err != nil {
		s.failImport(ctx, rec, err)
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	_, err = s.cfg.Store.UpdateBackupRecord(writeCtx, rec.ID, func(r *models.BackupRecord) error {
		if r.Status != models.StatusPending {
			return errRecordChanged
		}
		done := s.now()
		r.Status, r.SHA256, r.SizeBytes, r.CompletedAt = models.StatusCompleted, sum, size, &done
		return nil
	})
	if err != nil {
		s.logger.Warn("failed to complete an imported backup", slog.String("backup_id", rec.ID), slog.Any("error", err))
		return
	}
	s.logger.Info("orphan archive imported", slog.String("backup_id", rec.ID), slog.String("storage_key", rec.StorageKey), slog.Int64("size_bytes", size))
}

// failImport marks an imported record failed and detaches it from its archive.
func (s *Service) failImport(ctx context.Context, rec *models.BackupRecord, cause error) {
	msg := redact.Text(fmt.Sprintf("import of %s failed: %v; the archive was left in place", rec.StorageKey, cause))
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	if _, err := s.cfg.Store.UpdateBackupRecord(writeCtx, rec.ID, func(r *models.BackupRecord) error {
		r.Status, r.ErrorMessage, r.StorageKey = models.StatusFailed, msg, ""
		return nil
	}); err != nil {
		s.logger.Warn("failed to record a failed import", slog.String("backup_id", rec.ID), slog.Any("error", err))
	}
	s.logger.Warn("orphan archive import failed", slog.String("backup_id", rec.ID), slog.String("error", msg))
}

// hashObject streams object key through SHA-256 and returns the hex digest and the
// size.
func hashObject(ctx context.Context, driver storage.Storage, key string) (string, int64, error) {
	rc, err := driver.Retrieve(ctx, key)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, &ctxReader{ctx: ctx, r: rc})
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ctxReader fails reads once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

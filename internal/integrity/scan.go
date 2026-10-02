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
	"github.com/yigitcittan/mongorescue/internal/logsafe"
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
	// Unreadable counts the archives named by a backup row that cannot be read
	// (see store.CorruptRecords): neither orphans nor missing, never touched.
	Unreadable int `json:"unreadable,omitempty"`
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

// unreadableOwner returns the ID of a backup row that names key but could not be
// decoded (it is not among the readable records), or "". Such a row owns the
// archive: it is never an orphan and is never imported over.
func unreadableOwner(index map[string][]string, readable map[string]bool, key string) string {
	for _, id := range index[key] {
		if !readable[id] {
			return id
		}
	}
	return ""
}

// readableIDs returns the IDs of records.
func readableIDs(records []*models.BackupRecord) map[string]bool {
	out := make(map[string]bool, len(records))
	for _, r := range records {
		out[r.ID] = true
	}
	return out
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
	// Rows that cannot be decoded are skipped by the record list; their keys come
	// from SQL, so their archives are never reported as orphans.
	index, err := s.cfg.Store.ArchiveKeyIndex(ctx, target.ID)
	if err != nil {
		return err
	}
	readable := readableIDs(records)

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
		if unreadableOwner(index, readable, obj.Key) != "" {
			report.Unreadable++
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

// StartImport makes the orphan archive key on storage target targetID a backup
// again and hashes it in the background: the record is pending until its size and
// SHA-256 are known, then completed, imported and unverified. A key that a failed
// or pruned record still names revives that record (keeping its ID and history) so
// that no two records ever share an archive; a key a live record owns is refused.
// The database is taken from the key and must be a valid name. Imports of one key
// are serialised, and the record check happens under that lock. A failed import
// marks the record failed and detaches it from the key, so the archive stays an
// orphan and is never deleted through the record. Expected failures: ErrNotFound,
// ErrNotOrphan, ErrInvalidImport, ErrBusy and runs.ErrShuttingDown.
func (s *Service) StartImport(ctx context.Context, targetID, key string) (*models.BackupRecord, error) {
	key = strings.TrimSpace(key)
	if !IsArchiveKey(key) {
		return nil, fmt.Errorf("%w: %q is not a backup archive key", ErrNotOrphan, key)
	}
	database, err := importDatabase(key)
	if err != nil {
		return nil, err
	}
	target, err := s.cfg.Targets.Resolve(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("%w: storage target %s", ErrNotFound, targetID)
	}
	// The lock comes first: the record check and the save below cannot race with
	// another import of the same key.
	release, err := s.cfg.Runs.Acquire(keyPrefixImport + target.ID + "/" + key)
	if err != nil {
		return nil, err
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

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
	records, byKey, err := s.targetRecords(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	index, err := s.cfg.Store.ArchiveKeyIndex(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	if id := unreadableOwner(index, readableIDs(records), key); id != "" {
		return nil, fmt.Errorf("%w: backup %s, which cannot be read, names it; repair that row first", ErrNotOrphan, id)
	}
	var rec *models.BackupRecord
	priorSHA := ""
	switch existing := byKey[key]; {
	case existing != nil && liveStatus(existing.Status):
		return nil, fmt.Errorf("%w: it belongs to backup %s", ErrNotOrphan, existing.ID)
	case existing != nil:
		priorSHA = existing.SHA256
		if rec, err = s.reviveRecord(ctx, existing.ID, key); err != nil {
			return nil, err
		}
	default:
		if rec, err = s.importRecord(ctx, target, key, database, obj); err != nil {
			return nil, err
		}
		if err := s.cfg.Store.SaveBackupRecord(ctx, rec); err != nil {
			return nil, fmt.Errorf("save imported record: %w", err)
		}
	}
	snapshot := *rec
	if err := s.cfg.Runs.Go("", func(runCtx context.Context) {
		defer release()
		s.finishImport(runCtx, driver, rec, priorSHA)
	}); err != nil {
		s.failImport(ctx, rec, err)
		return nil, err
	}
	released = true
	return &snapshot, nil
}

// importDatabase returns the database an imported archive key names. It must be a
// valid MongoDB database name without '*' or '\' (which namespace patterns would
// read as wildcards or escapes); keys that name none are refused.
func importDatabase(key string) (string, error) {
	db := ""
	if m := defaultLayout.FindStringSubmatch(key); m != nil {
		db = m[1]
	} else if dir, _, ok := strings.Cut(key, "/"); ok {
		db = dir
	}
	switch {
	case db == "":
		return "", fmt.Errorf("%w: the key %q names no database (expected <db>/<YYYY>/<MM>/<backup id>.archive)", ErrInvalidImport, key)
	case strings.ContainsAny(db, "*\\"):
		return "", fmt.Errorf("%w: the database %q in the key contains '*' or '\\'", ErrInvalidImport, db)
	}
	if err := models.ValidateDatabaseName(db); err != nil {
		return "", fmt.Errorf("%w: the database in the key: %w", ErrInvalidImport, err)
	}
	return db, nil
}

// reviveRecord turns the failed or pruned record id that still names key into a
// pending import: same ID and history, but manual (retention never prunes it
// again) and unverified until it is hashed.
func (s *Service) reviveRecord(ctx context.Context, id, key string) (*models.BackupRecord, error) {
	now := s.now()
	rec, err := s.cfg.Store.UpdateBackupRecord(ctx, id, func(r *models.BackupRecord) error {
		if liveStatus(r.Status) || r.StorageKey != key {
			return fmt.Errorf("%w: backup %s changed meanwhile", ErrNotOrphan, r.ID)
		}
		r.Status, r.Trigger, r.Imported, r.ImportedAt, r.ErrorMessage = models.StatusPending, models.TriggerManual, true, &now, ""
		r.CompletedAt, r.MissingSince, r.VerifiedAt, r.Verification, r.VerificationError = nil, nil, nil, "", ""
		return nil
	})
	if err != nil {
		return nil, notFound(err, "backup not found")
	}
	return rec, nil
}

// importRecord builds the pending record of an imported archive of database.
func (s *Service) importRecord(ctx context.Context, target *models.StorageTarget, key, database string, obj *models.StorageObject) (*models.BackupRecord, error) {
	info := parseKey(key)
	now := s.now()
	started := info.startedAt
	if started.IsZero() {
		started = obj.ModTime.UTC()
	}
	if started.IsZero() {
		started = now
	}
	id := info.id
	if id != "" {
		if models.ValidateID(id) != nil {
			id = ""
		} else if _, err := s.cfg.Store.GetBackupRecord(ctx, id); err == nil {
			id = "" // the ID is taken by a record of another archive
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

// finishImport hashes the imported archive and completes (or fails) its record. A
// revived record whose archive no longer matches the checksum recorded when it was
// written (priorSHA) is completed with the new checksum but flagged as a mismatch.
func (s *Service) finishImport(ctx context.Context, driver storage.Storage, rec *models.BackupRecord, priorSHA string) {
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
		if priorSHA != "" && !strings.EqualFold(priorSHA, sum) {
			r.Verification, r.VerifiedAt = models.VerificationMismatch, &done
			r.VerificationError = fmt.Sprintf("the archive no longer matches the checksum recorded when it was written (recorded %s, imported %s)", priorSHA, sum)
		}
		return nil
	})
	if err != nil {
		s.logger.Warn("failed to complete an imported backup", logsafe.Attr("backup_id", rec.ID), slog.Any("error", err))
		return
	}
	s.logger.Info("orphan archive imported", logsafe.Attr("backup_id", rec.ID), logsafe.Attr("storage_key", rec.StorageKey), slog.Int64("size_bytes", size))
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
		s.logger.Warn("failed to record a failed import", logsafe.Attr("backup_id", rec.ID), slog.Any("error", err))
	}
	s.logger.Warn("orphan archive import failed", logsafe.Attr("backup_id", rec.ID), logsafe.Attr("error", msg))
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

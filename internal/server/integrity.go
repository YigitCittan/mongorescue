package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/integrity"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// maxIntegrityBody bounds the JSON bodies of the integrity endpoints.
const maxIntegrityBody = 8 << 10

// WithIntegrity enables archive verification, the integrity sweep, restore tests and
// storage scans (the /api/v1/integrity, .../verify, .../restore-test and .../scan
// endpoints).
func WithIntegrity(svc *integrity.Service) Option {
	return func(s *Server) { s.integrity = svc }
}

// registerIntegrityRoutes adds the verification, pin, retention, restore test and
// storage scan endpoints.
func (s *Server) registerIntegrityRoutes(mux *router) {
	mux.HandleFunc("POST /api/v1/backups/{id}/verify", s.handleVerifyBackup)
	mux.HandleFunc("POST /api/v1/backups/{id}/pin", s.handlePinBackup)
	mux.HandleFunc("POST /api/v1/backups/{id}/unpin", s.handleUnpinBackup)
	mux.HandleFunc("GET /api/v1/jobs/{id}/retention/preview", s.handleRetentionPreview)
	mux.HandleFunc("GET /api/v1/jobs/{id}/retention/log", s.handleRetentionLog)
	mux.HandleFunc("POST /api/v1/jobs/{id}/restore-test", s.handleStartRestoreTest)
	mux.HandleFunc("GET /api/v1/jobs/{id}/restore-tests", s.handleListRestoreTests)
	mux.HandleFunc("GET /api/v1/integrity", s.handleIntegrityStatus)
	mux.HandleFunc("POST /api/v1/integrity/sweep", s.handleStartSweep)
	mux.HandleFunc("GET /api/v1/storage-targets/{id}/scan", s.handleLastScan)
	mux.HandleFunc("POST /api/v1/storage-targets/{id}/scan", s.handleScanTarget)
	mux.HandleFunc("POST /api/v1/storage-targets/{id}/import", s.handleImportOrphan)
}

// requireIntegrity returns the integrity service, answering 503 when absent.
func (s *Server) requireIntegrity(w http.ResponseWriter) (*integrity.Service, bool) {
	if s.integrity == nil {
		writeError(w, http.StatusServiceUnavailable, "integrity checks are not configured")
		return nil, false
	}
	return s.integrity, true
}

// writeIntegrityError maps integrity and trust errors to HTTP responses; anything
// unexpected is logged and answered with a generic 500.
func (s *Server) writeIntegrityError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, integrity.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, integrity.ErrInvalidImport):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, integrity.ErrNotVerifiable), errors.Is(err, integrity.ErrNoBackup),
		errors.Is(err, integrity.ErrNotOrphan), errors.Is(err, integrity.ErrBusy), errors.Is(err, operations.ErrPinned):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, integrity.ErrUnavailable), errors.Is(err, operations.ErrUnavailable), errors.Is(err, runs.ErrShuttingDown):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, operations.ErrInvalid), errors.Is(err, operations.ErrNotFound), errors.Is(err, auth.ErrForbidden),
		errors.Is(err, operations.ErrApprovalRequired), errors.Is(err, operations.ErrBackupDeleted):
		s.writeOperationError(w, err)
	default:
		s.logger.Error("integrity operation failed", logsafe.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// handleVerifyBackup re-reads a backup's archive in the background and compares it
// with its checksum; poll the backup for verified_at and verification.
func (s *Server) handleVerifyBackup(w http.ResponseWriter, r *http.Request) {
	rec, err := s.ops.VerifyBackup(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeIntegrityError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, rec)
}

// pinRequest is the optional body of POST /api/v1/backups/{id}/pin.
type pinRequest struct {
	// Note is the reason of the legal hold.
	Note string `json:"note"`
}

// handlePinBackup pins a backup (legal hold) with an optional note.
func (s *Server) handlePinBackup(w http.ResponseWriter, r *http.Request) {
	var req pinRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxIntegrityBody)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request json")
		return
	}
	rec, err := s.ops.PinBackup(r.Context(), r.PathValue("id"), req.Note)
	if err != nil {
		s.writeIntegrityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handleUnpinBackup lifts a backup's pin.
func (s *Server) handleUnpinBackup(w http.ResponseWriter, r *http.Request) {
	rec, err := s.ops.UnpinBackup(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeIntegrityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// optionalInt parses query parameter name; ok is false (with a 400 written) when it
// is present but not an integer.
func optionalInt(w http.ResponseWriter, r *http.Request, name string) (*int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, name+" must be an integer")
		return nil, false
	}
	return &v, true
}

// handleRetentionPreview lists the backups a job's retention would delete now (or
// with the retention_days / retention_count query values), without deleting.
func (s *Server) handleRetentionPreview(w http.ResponseWriter, r *http.Request) {
	days, ok := optionalInt(w, r, "retention_days")
	if !ok {
		return
	}
	count, ok := optionalInt(w, r, "retention_count")
	if !ok {
		return
	}
	preview, err := s.ops.RetentionPreview(r.Context(), r.PathValue("id"), days, count)
	if err != nil {
		s.writeIntegrityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

// handleRetentionLog lists the backups a job's retention deleted.
func (s *Server) handleRetentionLog(w http.ResponseWriter, r *http.Request) {
	limit, ok := optionalInt(w, r, "limit")
	if !ok {
		return
	}
	log, err := s.ops.RetentionLog(r.Context(), r.PathValue("id"), derefOr(limit, 0))
	if err != nil {
		s.writeIntegrityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, log)
}

// handleStartRestoreTest runs a job's restore test now, in the background.
func (s *Server) handleStartRestoreTest(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireIntegrity(w)
	if !ok {
		return
	}
	backup, err := svc.StartRestoreTest(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeIntegrityError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": r.PathValue("id"), "backup_id": backup.ID})
}

// handleListRestoreTests lists a job's restore tests, newest first.
func (s *Server) handleListRestoreTests(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireIntegrity(w)
	if !ok {
		return
	}
	limit, ok := optionalInt(w, r, "limit")
	if !ok {
		return
	}
	list, err := svc.ListRestoreTests(r.Context(), r.PathValue("id"), derefOr(limit, 0))
	if err != nil {
		s.writeIntegrityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleIntegrityStatus returns the sweep status and the latest storage scans.
func (s *Server) handleIntegrityStatus(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireIntegrity(w)
	if !ok {
		return
	}
	ov, err := svc.Status(r.Context())
	if err != nil {
		s.writeIntegrityError(w, err)
		return
	}
	// The sweep and the storage scans cover every connection's backups: a caller
	// limited to some connections gets neither.
	if auth.ConnectionFilter(r.Context()).Limited() {
		ov = &integrity.Overview{Scans: []*integrity.DriftReport{}, Running: []string{}}
	}
	writeJSON(w, http.StatusOK, ov)
}

// handleStartSweep starts an integrity sweep now, in the background.
func (s *Server) handleStartSweep(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireIntegrity(w)
	if !ok {
		return
	}
	st, err := svc.StartSweep(r.Context())
	if err != nil {
		s.writeIntegrityError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

// handleLastScan returns the latest storage scan of a target.
func (s *Server) handleLastScan(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireIntegrity(w)
	if !ok {
		return
	}
	// A scan lists every archive on the target, whichever connection it came from.
	if auth.ConnectionFilter(r.Context()).Limited() {
		writeError(w, http.StatusNotFound, "storage target not found")
		return
	}
	report, found := svc.LastScan(r.Context(), r.PathValue("id"))
	if !found {
		writeError(w, http.StatusNotFound, "this storage target was not scanned yet")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleScanTarget scans a storage target now and returns its drift report.
func (s *Server) handleScanTarget(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireIntegrity(w)
	if !ok {
		return
	}
	report, err := svc.ScanTarget(r.Context(), r.PathValue("id"), integrity.TriggerManual)
	if err != nil {
		s.writeIntegrityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// importRequest is the body of POST /api/v1/storage-targets/{id}/import.
type importRequest struct {
	// Key is the orphan archive's object key, as listed by the scan.
	Key string `json:"key"`
}

// handleImportOrphan creates a backup record for an orphan archive; the record is
// pending until the archive is hashed in the background.
func (s *Server) handleImportOrphan(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireIntegrity(w)
	if !ok {
		return
	}
	var req importRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxIntegrityBody)).Decode(&req); err != nil || req.Key == "" {
		writeError(w, http.StatusBadRequest, "a JSON body with the orphan's key is required")
		return
	}
	rec, err := svc.StartImport(r.Context(), r.PathValue("id"), req.Key)
	if err != nil {
		s.writeIntegrityError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, rec)
}

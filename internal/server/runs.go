package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runlog"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// WithRunRegistry sets the registry of active runs (cancellation, live progress and
// run logs) used when the Server builds its own operations service. With
// WithOperations, the operations service's registry applies instead.
func WithRunRegistry(reg *runs.Registry) Option {
	return func(s *Server) { s.registry = reg }
}

// registerRunRoutes registers the run control endpoints: cancelling backups and
// restores, their logs and the live progress of active runs.
func (s *Server) registerRunRoutes(mux *router) {
	mux.HandleFunc("POST /api/v1/backups/{id}/cancel", s.handleCancelBackup)
	mux.HandleFunc("POST /api/v1/restores/{id}/cancel", s.handleCancelRestore)
	mux.HandleFunc("GET /api/v1/backups/{id}/log", s.handleBackupLog)
	mux.HandleFunc("GET /api/v1/restores/{id}/log", s.handleRestoreLog)
	mux.HandleFunc("GET /api/v1/runs/active", s.handleActiveRuns)
}

// handleCancelBackup cancels a running backup. It answers 200 with the final
// cancelled record once the run stopped (usually within the request), 202 with the
// in-progress record while it is still stopping, 404 for an unknown backup and 409
// when it is not running.
func (s *Server) handleCancelBackup(w http.ResponseWriter, r *http.Request) {
	rec, err := s.ops.CancelBackup(r.Context(), r.PathValue("id"), "")
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, cancelStatus(rec.Status == models.StatusInProgress), rec)
}

// cancelStatus is 202 while a cancelled run is still stopping, else 200.
func cancelStatus(stopping bool) int {
	if stopping {
		return http.StatusAccepted
	}
	return http.StatusOK
}

// handleCancelRestore cancels a running restore (see handleCancelBackup). Cancelling
// an in-place restore needs admin, which the operations service enforces.
func (s *Server) handleCancelRestore(w http.ResponseWriter, r *http.Request) {
	rec, err := s.ops.CancelRestore(r.Context(), r.PathValue("id"), "")
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, cancelStatus(rec.Status == models.RestoreStatusInProgress), rec)
}

// handleActiveRuns returns the live progress of every running backup and restore.
func (s *Server) handleActiveRuns(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.ops.ActiveRuns())
}

// handleBackupLog serves the log of a backup (see serveRunLog).
func (s *Server) handleBackupLog(w http.ResponseWriter, r *http.Request) {
	s.serveRunLog(w, r, s.ops.OpenBackupLog)
}

// handleRestoreLog serves the log of a restore (see serveRunLog).
func (s *Server) handleRestoreLog(w http.ResponseWriter, r *http.Request) {
	s.serveRunLog(w, r, s.ops.OpenRestoreLog)
}

// maxTailParam bounds the tail query parameter.
const maxTailParam = runlog.MaxTailLines

// serveRunLog writes a run log as text/plain: the last N lines with ?tail=N (for
// following a running log), otherwise the whole file as a download.
func (s *Server) serveRunLog(w http.ResponseWriter, r *http.Request, open func(ctx context.Context, id string) (*runlog.Reader, error)) {
	id := r.PathValue("id")
	tail := 0
	if raw := r.URL.Query().Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxTailParam {
			writeError(w, http.StatusBadRequest, "tail must be a number of lines between 1 and "+strconv.Itoa(maxTailParam))
			return
		}
		tail = n
	}
	rd, err := open(r.Context(), id)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	defer rd.Close()

	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	if tail > 0 {
		body, err := rd.Tail(tail)
		if err != nil {
			s.writeOperationError(w, err)
			return
		}
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	h.Set("Content-Disposition", `attachment; filename="`+logFileName(id)+`"`)
	h.Set("Content-Length", strconv.FormatInt(rd.Size(), 10))
	w.WriteHeader(http.StatusOK)
	if _, err := rd.WriteTo(w); err != nil {
		s.logger.Debug("run log download interrupted", "error", err)
	}
}

// logFileName returns the download name of the log of run id, with every character
// outside [A-Za-z0-9._-] replaced, so the header cannot be broken by a legacy ID.
func logFileName(id string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, id)
	if clean == "" {
		clean = "run"
	}
	return clean + ".log"
}

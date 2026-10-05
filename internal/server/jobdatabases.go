package server

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// registerJobDatabaseRoutes registers the endpoints of multi-database jobs: the
// preview of a database selection, a job's run history grouped by run, and stopping
// a job's current run.
func (s *Server) registerJobDatabaseRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/jobs/{id}/databases/preview", s.handleJobDatabasesPreview)
	mux.HandleFunc("GET /api/v1/jobs/databases/preview", s.handleJobDatabasesPreview)
	mux.HandleFunc("GET /api/v1/jobs/{id}/runs", s.handleListJobRuns)
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.handleCancelJobRun)
}

// previewRequest reads the optional selection of a preview from the query: mode,
// repeated databases, include and exclude parameters, auto_include_new,
// connection_id and storage_target_id. Without mode the job's stored selection is
// previewed.
func previewRequest(q url.Values) (operations.DatabasePreviewRequest, error) {
	req := operations.DatabasePreviewRequest{ConnectionID: q.Get("connection_id"), StorageTargetID: q.Get("storage_target_id")}
	if mode := q.Get("mode"); mode != "" {
		sel := &models.DatabaseSelection{
			Mode: models.SelectionMode(mode), Databases: q["databases"], Include: q["include"], Exclude: q["exclude"],
		}
		if v := q.Get("auto_include_new"); v != "" {
			auto, err := strconv.ParseBool(v)
			if err != nil {
				return req, err
			}
			sel.AutoIncludeNew = auto
		}
		req.Selection = sel
	}
	return req, nil
}

// handleJobDatabasesPreview resolves a job's database selection (or the one in the
// query, for a job being edited or created) against the live server, exactly as the
// next run would: {included, excluded: [{name, reason}], new_since_last_run}.
func (s *Server) handleJobDatabasesPreview(w http.ResponseWriter, r *http.Request) {
	req, err := previewRequest(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "auto_include_new must be true or false")
		return
	}
	preview, err := s.ops.PreviewJobDatabases(r.Context(), r.PathValue("id"), req)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

// handleListJobRuns lists a job's runs, newest first, each with the outcome of every
// database (?limit=, at most operations.MaxJobRunList).
func (s *Server) handleListJobRuns(w http.ResponseWriter, r *http.Request) {
	limit, ok := optionalInt(w, r, "limit")
	if !ok {
		return
	}
	list, err := s.ops.ListJobRuns(r.Context(), r.PathValue("id"), derefOr(limit, 0))
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCancelJobRun stops a job's current run: the running database and every
// database still waiting. It answers 200 once the backups stopped, 202 while some
// are still stopping, 404 for an unknown job and 409 when no run of it is active.
func (s *Server) handleCancelJobRun(w http.ResponseWriter, r *http.Request) {
	res, err := s.ops.CancelJobRun(r.Context(), r.PathValue("id"), "")
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	status := http.StatusOK
	for _, b := range res.Backups {
		if b.Status == models.StatusInProgress {
			status = http.StatusAccepted
			break
		}
	}
	writeJSON(w, status, res)
}

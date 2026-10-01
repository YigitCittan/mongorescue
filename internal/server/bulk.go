package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/operations"
)

// maxBulkBody bounds the body of a bulk request: MaxBulkItems IDs of
// MaxBulkIDLength bytes, each byte escaped (\" or \\) at worst, plus the quotes,
// commas and the rest of the body.
const maxBulkBody = operations.MaxBulkItems*(2*operations.MaxBulkIDLength+3) + 64<<10

// bulkWriteTimeout is the write deadline of a bulk run: deleting thousands of
// archives one at a time can take longer than writeTimeout.
const bulkWriteTimeout = 15 * time.Minute

// registerBulkRoutes registers the bulk endpoints. Their route scope is the lowest
// scope of their actions; the operations service checks the scope of the requested
// action (delete needs admin, like the single-item routes).
func (s *Server) registerBulkRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/bulk/actions", s.handleBulkActions)
	mux.HandleFunc("POST /api/v1/backups/bulk", s.handleBulk(operations.BulkBackups))
	mux.HandleFunc("POST /api/v1/restores/bulk", s.handleBulk(operations.BulkRestores))
	mux.HandleFunc("POST /api/v1/jobs/bulk", s.handleBulk(operations.BulkJobs))
}

// handleBulkActions lists the available bulk actions and whether the caller may run
// each, so clients only offer what exists.
func (s *Server) handleBulkActions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.ops.BulkActions(r.Context()))
}

// handleBulk serves POST /api/v1/{resource}/bulk. Unknown fields are refused, so a
// misspelt filter can never widen the selection.
func (s *Server) handleBulk(resource operations.BulkResource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBulkBody))
		dec.DisallowUnknownFields()
		var req operations.BulkRequest
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, bulkDecodeError(err))
			return
		}
		if dec.More() {
			writeError(w, http.StatusBadRequest, "invalid request json: one JSON object expected")
			return
		}
		if !req.DryRun {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(bulkWriteTimeout))
		}
		res, err := s.ops.Bulk(r.Context(), resource, req)
		if err != nil {
			s.writeOperationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

// bulkDecodeError describes a body that does not decode into a BulkRequest. Only the
// decoder's own descriptions of unknown fields are echoed.
func bulkDecodeError(err error) string {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return "request body too large"
	case errors.Is(err, io.EOF):
		return "invalid request json: empty body"
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return "invalid request json: " + strings.TrimPrefix(err.Error(), "json: ")
	default:
		return "invalid request json"
	}
}

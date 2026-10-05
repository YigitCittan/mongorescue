package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
)

// PITR stream routes (experimental).
const (
	pitrStreamsRoute      = "GET /api/v1/pitr/streams"
	pitrCreateStreamRoute = "POST /api/v1/pitr/streams"
	pitrStreamRoute       = "GET /api/v1/pitr/streams/{id}"
	pitrUpdateStreamRoute = "PATCH /api/v1/pitr/streams/{id}"
	pitrDeleteStreamRoute = "DELETE /api/v1/pitr/streams/{id}"
	pitrChunksRoute       = "GET /api/v1/pitr/streams/{id}/chunks"
	pitrBaseRoute         = "POST /api/v1/pitr/streams/{id}/base"
)

// Audit log targets of the stream routes.
const (
	targetPITRStream     = "stream"
	targetPITRConnection = "connection"
	targetPITREnabled    = "enabled"
	targetPITRBackup     = "backup"
)

// maxPITRBody bounds the JSON body of the stream routes.
const maxPITRBody = 16 << 10

// WithPITR enables the PITR stream endpoints (/api/v1/pitr/streams).
func WithPITR(svc *collector.Service) Option {
	return func(s *Server) { s.pitr = svc }
}

// registerPITRRoutes adds the PITR stream endpoints.
func (s *Server) registerPITRRoutes(mux *router) {
	mux.HandleFunc(pitrStreamsRoute, s.handleListPITRStreams)
	mux.HandleFunc(pitrCreateStreamRoute, s.handleCreatePITRStream)
	mux.HandleFunc(pitrStreamRoute, s.handleGetPITRStream)
	mux.HandleFunc(pitrUpdateStreamRoute, s.handleUpdatePITRStream)
	mux.HandleFunc(pitrDeleteStreamRoute, s.handleDeletePITRStream)
	mux.HandleFunc(pitrChunksRoute, s.handleListPITRChunks)
	mux.HandleFunc(pitrBaseRoute, s.handlePITRBase)
}

// pitrReady answers 503 without the collector.
func (s *Server) pitrReady(w http.ResponseWriter) bool {
	if s.pitr == nil {
		writeError(w, http.StatusServiceUnavailable, "point-in-time recovery is not available")
		return false
	}
	return true
}

// writePITRError maps a stream use case error to a response.
func (s *Server) writePITRError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, collector.ErrInvalid), errors.Is(err, collector.ErrNotReplicaSet),
		errors.Is(err, collector.ErrNoOplogAccess), errors.Is(err, collector.ErrEncryptionRequired):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, pitr.ErrNotFound):
		writeError(w, http.StatusNotFound, "PITR stream not found")
	case errors.Is(err, pitr.ErrConnectionTaken), errors.Is(err, collector.ErrStillEnabled),
		errors.Is(err, collector.ErrChunksPending), errors.Is(err, pitr.ErrInUse):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, collector.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		s.writeOperationError(w, err)
	}
}

// decodePITRRequest decodes a bounded stream request.
func decodePITRRequest(w http.ResponseWriter, r *http.Request) (collector.StreamRequest, bool) {
	var req collector.StreamRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxPITRBody)
	if !decodeBody(w, r, &req) {
		return req, false
	}
	return req, true
}

// handleListPITRStreams returns the status of every stream.
func (s *Server) handleListPITRStreams(w http.ResponseWriter, r *http.Request) {
	if !s.pitrReady(w) {
		return
	}
	list, err := s.pitr.ListStatuses(r.Context())
	if err != nil {
		s.writePITRError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCreatePITRStream creates the stream of a replica set connection.
func (s *Server) handleCreatePITRStream(w http.ResponseWriter, r *http.Request) {
	if !s.pitrReady(w) {
		return
	}
	req, ok := decodePITRRequest(w, r)
	if !ok {
		return
	}
	if req.ConnectionID != nil {
		auditlog.Annotate(r.Context(), targetPITRConnection, truncateUTF8(*req.ConnectionID, maxAuditPathValue))
	}
	st, err := s.pitr.CreateStream(r.Context(), req)
	if err != nil {
		s.writePITRError(w, err)
		return
	}
	auditlog.Annotate(r.Context(), targetPITRStream, st.ID)
	auditlog.Annotate(r.Context(), targetPITREnabled, strconv.FormatBool(st.Enabled))
	s.logger.Info("PITR stream created", logsafe.Attr("stream_id", st.ID), logsafe.Attr("connection_id", st.ConnectionID))
	writeJSON(w, http.StatusCreated, st)
}

// handleGetPITRStream returns a stream's state, windows, lag and headroom.
func (s *Server) handleGetPITRStream(w http.ResponseWriter, r *http.Request) {
	if !s.pitrReady(w) {
		return
	}
	st, err := s.pitr.Status(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writePITRError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleUpdatePITRStream changes a stream (enable, disable, schedule, retention).
func (s *Server) handleUpdatePITRStream(w http.ResponseWriter, r *http.Request) {
	if !s.pitrReady(w) {
		return
	}
	req, ok := decodePITRRequest(w, r)
	if !ok {
		return
	}
	if req.Enabled != nil {
		auditlog.Annotate(r.Context(), targetPITREnabled, strconv.FormatBool(*req.Enabled))
	}
	st, err := s.pitr.UpdateStream(r.Context(), r.PathValue("id"), req)
	if err != nil {
		s.writePITRError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleDeletePITRStream deletes a disabled stream; while its chunks wait for the
// end of the delete grace period it answers 409.
func (s *Server) handleDeletePITRStream(w http.ResponseWriter, r *http.Request) {
	if !s.pitrReady(w) {
		return
	}
	if err := s.pitr.DeleteStream(r.Context(), r.PathValue("id")); err != nil {
		s.writePITRError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": r.PathValue("id")})
}

// handleListPITRChunks returns a page of a stream's chunks (?limit=, ?offset=),
// metadata only.
func (s *Server) handleListPITRChunks(w http.ResponseWriter, r *http.Request) {
	if !s.pitrReady(w) {
		return
	}
	q := r.URL.Query()
	limit, offset := 0, 0
	var err error
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
	}
	if v := q.Get("offset"); v != "" {
		if offset, err = strconv.Atoi(v); err != nil || offset < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
	}
	page, err := s.pitr.ListChunks(r.Context(), r.PathValue("id"), limit, offset)
	if err != nil {
		s.writePITRError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handlePITRBase takes a base backup of a stream now.
func (s *Server) handlePITRBase(w http.ResponseWriter, r *http.Request) {
	if !s.pitrReady(w) {
		return
	}
	rec, err := s.pitr.TakeBase(r.Context(), r.PathValue("id"), models.TriggerManual)
	if err != nil {
		s.writePITRError(w, err)
		return
	}
	auditlog.Annotate(r.Context(), targetPITRBackup, rec.ID)
	writeJSON(w, http.StatusAccepted, rec)
}

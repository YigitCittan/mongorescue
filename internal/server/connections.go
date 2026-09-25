package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/mongouri"
)

// WithConnections enables the /api/v1/connections endpoints and connection-based
// backups, jobs and restores.
func WithConnections(svc *connections.Service) Option {
	return func(s *Server) { s.connections = svc }
}

// registerConnectionRoutes adds the managed connection endpoints.
func (s *Server) registerConnectionRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/connections", s.handleListConnections)
	mux.HandleFunc("POST /api/v1/connections", s.handleCreateConnection)
	mux.HandleFunc("POST /api/v1/connections/test", s.handleTestConnectionURI)
	mux.HandleFunc("GET /api/v1/connections/{id}", s.handleGetConnection)
	mux.HandleFunc("PUT /api/v1/connections/{id}", s.handleUpdateConnection)
	mux.HandleFunc("DELETE /api/v1/connections/{id}", s.handleDeleteConnection)
	mux.HandleFunc("POST /api/v1/connections/{id}/test", s.handleTestConnection)
	mux.HandleFunc("GET /api/v1/connections/{id}/databases", s.handleListDatabases)
	mux.HandleFunc("GET /api/v1/connections/{id}/databases/{db}/collections", s.handleListCollections)
}

// requireConnections returns the connections service, answering 503 when absent.
func (s *Server) requireConnections(w http.ResponseWriter) (*connections.Service, bool) {
	if s.connections == nil {
		writeError(w, http.StatusServiceUnavailable, "connections are not configured")
		return nil, false
	}
	return s.connections, true
}

// writeConnectionError maps connection errors to HTTP responses. Messages never
// contain connection strings.
func (s *Server) writeConnectionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, connections.ErrNotFound):
		writeError(w, http.StatusNotFound, "connection not found")
	case errors.Is(err, connections.ErrInUse):
		writeError(w, http.StatusConflict, err.Error()+"; delete or reassign those jobs first")
	case errors.Is(err, connections.ErrInvalid), errors.Is(err, connections.ErrMaskedURI),
		errors.Is(err, mongouri.ErrInvalidMongoURI):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, connections.ErrUnavailable):
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		s.logger.Error("connection request failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireConnections(w)
	if !ok {
		return
	}
	list, err := svc.List(r.Context())
	if err != nil {
		s.writeConnectionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetConnection(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireConnections(w)
	if !ok {
		return
	}
	c, err := svc.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeConnectionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleCreateConnection(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireConnections(w)
	if !ok {
		return
	}
	var in connections.Input
	if !decodeBody(w, r, &in) {
		return
	}
	c, err := svc.Create(r.Context(), in)
	if err != nil {
		s.writeConnectionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleUpdateConnection(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireConnections(w)
	if !ok {
		return
	}
	var in connections.Input
	if !decodeBody(w, r, &in) {
		return
	}
	c, err := svc.Update(r.Context(), r.PathValue("id"), in)
	if err != nil {
		s.writeConnectionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireConnections(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := svc.Delete(r.Context(), id); err != nil {
		s.writeConnectionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted_id": id})
}

func (s *Server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireConnections(w)
	if !ok {
		return
	}
	res, err := svc.Test(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeConnectionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleTestConnectionURI(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireConnections(w)
	if !ok {
		return
	}
	var req struct {
		URI string `json:"uri"`
		// ConnectionID lets an edit form test the stored credentials through the
		// redacted URI it was given.
		ConnectionID string `json:"connection_id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := svc.TestURI(r.Context(), req.URI, req.ConnectionID)
	if err != nil {
		s.writeConnectionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleListDatabases(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireConnections(w)
	if !ok {
		return
	}
	system, _ := strconv.ParseBool(r.URL.Query().Get("system"))
	dbs, err := svc.Databases(r.Context(), r.PathValue("id"), system)
	if err != nil {
		s.writeConnectionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dbs)
}

func (s *Server) handleListCollections(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireConnections(w)
	if !ok {
		return
	}
	cols, err := svc.Collections(r.Context(), r.PathValue("id"), r.PathValue("db"))
	if err != nil {
		s.writeConnectionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cols)
}

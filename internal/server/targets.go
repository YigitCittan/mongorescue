package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// ErrUnknownStorageTarget is returned (as HTTP 400) when a job or backup names a
// storage target that does not exist. It aliases operations.ErrUnknownStorageTarget.
var ErrUnknownStorageTarget = operations.ErrUnknownStorageTarget

// WithStorageTargets enables the /api/v1/storage-targets endpoints and makes jobs,
// backups, deletions and restores use per-target storage drivers.
func WithStorageTargets(svc *targets.Service) Option {
	return func(s *Server) { s.targets = svc }
}

// registerStorageTargetRoutes adds the storage target endpoints.
func (s *Server) registerStorageTargetRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/storage-targets", s.handleListTargets)
	mux.HandleFunc("POST /api/v1/storage-targets", s.handleCreateTarget)
	mux.HandleFunc("POST /api/v1/storage-targets/test", s.handleTestTargetInput)
	mux.HandleFunc("GET /api/v1/storage-targets/{id}", s.handleGetTarget)
	mux.HandleFunc("PUT /api/v1/storage-targets/{id}", s.handleUpdateTarget)
	mux.HandleFunc("DELETE /api/v1/storage-targets/{id}", s.handleDeleteTarget)
	mux.HandleFunc("POST /api/v1/storage-targets/{id}/test", s.handleTestTarget)
	mux.HandleFunc("POST /api/v1/storage-targets/{id}/default", s.handleSetDefaultTarget)
}

// requireTargets returns the storage target service, answering 503 when absent.
func (s *Server) requireTargets(w http.ResponseWriter) (*targets.Service, bool) {
	if s.targets == nil {
		writeError(w, http.StatusServiceUnavailable, "storage targets are not configured")
		return nil, false
	}
	return s.targets, true
}

// storageFor returns the driver of target id, or the fixed driver without targets.
func (s *Server) storageFor(ctx context.Context, id string) (storage.Storage, error) {
	if s.targets == nil {
		if s.storageDriver == nil {
			return nil, errors.New("no storage configured")
		}
		return s.storageDriver, nil
	}
	return s.targets.Storage(ctx, id)
}

// writeTargetError maps storage target errors to HTTP responses. Messages never
// contain secret keys.
func (s *Server) writeTargetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnknownStorageTarget):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, targets.ErrNotFound):
		writeError(w, http.StatusNotFound, "storage target not found")
	case errors.Is(err, targets.ErrInUse), errors.Is(err, targets.ErrIsDefault), errors.Is(err, targets.ErrNoDefault),
		errors.Is(err, targets.ErrConflict), errors.Is(err, targets.ErrLocationInUse):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, targets.ErrInvalid), errors.Is(err, targets.ErrMaskedSecret):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Error("storage target request failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) handleListTargets(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireTargets(w)
	if !ok {
		return
	}
	list, err := svc.List(r.Context())
	if err != nil {
		s.writeTargetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetTarget(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireTargets(w)
	if !ok {
		return
	}
	t, err := svc.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeTargetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireTargets(w)
	if !ok {
		return
	}
	var in targets.Input
	if !decodeBody(w, r, &in) {
		return
	}
	t, err := svc.Create(r.Context(), in)
	if err != nil {
		s.writeTargetError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireTargets(w)
	if !ok {
		return
	}
	var in targets.Input
	if !decodeBody(w, r, &in) {
		return
	}
	t, err := svc.Update(r.Context(), r.PathValue("id"), in)
	if err != nil {
		s.writeTargetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireTargets(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := svc.Delete(r.Context(), id); err != nil {
		s.writeTargetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted_id": id})
}

func (s *Server) handleSetDefaultTarget(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireTargets(w)
	if !ok {
		return
	}
	t, err := svc.SetDefault(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeTargetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleTestTarget(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireTargets(w)
	if !ok {
		return
	}
	res, err := svc.Test(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeTargetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// testTargetRequest is the body of POST /api/v1/storage-targets/test: an unsaved
// target, optionally with the ID of the target being edited (for its masked secret).
type testTargetRequest struct {
	targets.Input
	ID string `json:"id,omitempty"`
}

func (s *Server) handleTestTargetInput(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireTargets(w)
	if !ok {
		return
	}
	var req testTargetRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := svc.TestInput(r.Context(), req.Input, req.ID)
	if err != nil {
		s.writeTargetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

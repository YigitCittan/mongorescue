package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
)

// maxNotificationBody bounds notification configuration request bodies.
const maxNotificationBody = 64 << 10

// registerNotificationRoutes wires the notification channel and rule endpoints.
//
// Deleting a channel that is referenced by rules succeeds and removes the channel ID
// from those rules (rules left without channels stay stored but deliver nothing).
func (s *Server) registerNotificationRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/notifications/channels", s.handleListChannels)
	mux.HandleFunc("POST /api/v1/notifications/channels", s.handleCreateChannel)
	mux.HandleFunc("PUT /api/v1/notifications/channels/{id}", s.handleUpdateChannel)
	mux.HandleFunc("DELETE /api/v1/notifications/channels/{id}", s.handleDeleteChannel)
	mux.HandleFunc("POST /api/v1/notifications/channels/{id}/test", s.handleTestChannel)

	mux.HandleFunc("GET /api/v1/notifications/rules", s.handleListRules)
	mux.HandleFunc("POST /api/v1/notifications/rules", s.handleCreateRule)
	mux.HandleFunc("PUT /api/v1/notifications/rules/{id}", s.handleUpdateRule)
	mux.HandleFunc("DELETE /api/v1/notifications/rules/{id}", s.handleDeleteRule)
}

// notificationsReady writes 503 and returns false when notifications are not wired.
func (s *Server) notificationsReady(w http.ResponseWriter) bool {
	if s.notifications == nil {
		writeError(w, http.StatusServiceUnavailable, "notifications not initialized")
		return false
	}
	return true
}

// notifyStatus maps notification sentinel errors to HTTP status codes.
func notifyStatus(err error) int {
	switch {
	case errors.Is(err, notify.ErrChannelNotFound), errors.Is(err, notify.ErrRuleNotFound):
		return http.StatusNotFound
	case errors.Is(err, notify.ErrAlreadyExists):
		return http.StatusConflict
	case errors.Is(err, notify.ErrInvalidChannelConfig),
		errors.Is(err, notify.ErrInvalidRule),
		errors.Is(err, notify.ErrMaskedSecret),
		errors.Is(err, notify.ErrHeaderInjection),
		errors.Is(err, models.ErrInvalidID):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// writeNotifyError writes err with the mapped status. Internal errors are logged and
// replaced by a generic message.
func (s *Server) writeNotifyError(w http.ResponseWriter, err error) {
	status := notifyStatus(err)
	if status == http.StatusInternalServerError {
		s.logger.Error("notification api error", slog.Any("error", err))
		writeError(w, status, "internal error")
		return
	}
	writeError(w, status, err.Error())
}

// decodeJSON decodes a bounded JSON body into v, writing 400 on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxNotificationBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request json")
		return false
	}
	return true
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	if !s.notificationsReady(w) {
		return
	}
	list, err := s.notifications.ListChannels(r.Context())
	if err != nil {
		s.writeNotifyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	if !s.notificationsReady(w) {
		return
	}
	var ch notify.Channel
	if !decodeJSON(w, r, &ch) {
		return
	}
	created, err := s.notifications.CreateChannel(r.Context(), &ch)
	if err != nil {
		s.writeNotifyError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	if !s.notificationsReady(w) {
		return
	}
	var ch notify.Channel
	if !decodeJSON(w, r, &ch) {
		return
	}
	updated, err := s.notifications.UpdateChannel(r.Context(), r.PathValue("id"), &ch)
	if err != nil {
		s.writeNotifyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	if !s.notificationsReady(w) {
		return
	}
	id := r.PathValue("id")
	if err := s.notifications.DeleteChannel(r.Context(), id); err != nil {
		s.writeNotifyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted_id": id})
}

func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	if !s.notificationsReady(w) {
		return
	}
	status, err := s.notifications.TestChannel(r.Context(), r.PathValue("id"))
	if err != nil {
		if status == nil {
			s.writeNotifyError(w, err)
			return
		}
		// The provider rejected or could not be reached: report the recorded outcome.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(apiResponse{Success: false, Data: status, Error: status.Error})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	if !s.notificationsReady(w) {
		return
	}
	list, err := s.notifications.ListRules(r.Context())
	if err != nil {
		s.writeNotifyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	if !s.notificationsReady(w) {
		return
	}
	var rule notify.Rule
	if !decodeJSON(w, r, &rule) {
		return
	}
	created, err := s.notifications.CreateRule(r.Context(), &rule)
	if err != nil {
		s.writeNotifyError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	if !s.notificationsReady(w) {
		return
	}
	var rule notify.Rule
	if !decodeJSON(w, r, &rule) {
		return
	}
	updated, err := s.notifications.UpdateRule(r.Context(), r.PathValue("id"), &rule)
	if err != nil {
		s.writeNotifyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	if !s.notificationsReady(w) {
		return
	}
	id := r.PathValue("id")
	if err := s.notifications.DeleteRule(r.Context(), id); err != nil {
		s.writeNotifyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted_id": id})
}

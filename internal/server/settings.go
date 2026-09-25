package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/yigitcittan/mongorescue/internal/settings"
)

// WithSettings enables GET/PUT /api/v1/settings and makes the server read CORS,
// cookie, proxy, metrics and job defaults from svc on every request.
func WithSettings(svc *settings.Service) Option {
	return func(s *Server) { s.settings = svc }
}

// registerSettingsRoutes adds the settings endpoints.
func (s *Server) registerSettingsRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/v1/settings", s.handleUpdateSettings)
	mux.HandleFunc("POST /api/v1/settings/encryption/generate-key", s.handleGenerateKey)
}

// settingsResponse is returned by GET and PUT /api/v1/settings. Secrets are masked.
// RestartRequired lists settings that only apply after a restart (currently none:
// every setting applies to the next operation or request).
type settingsResponse struct {
	settings.Settings
	RestartRequired []string `json:"restart_required"`
}

// requireSettings returns the settings service, answering 503 when absent.
func (s *Server) requireSettings(w http.ResponseWriter) (*settings.Service, bool) {
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings are not configured")
		return nil, false
	}
	return s.settings, true
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	svc, ok := s.requireSettings(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, settingsResponse{Settings: svc.Masked(), RestartRequired: []string{}})
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireSettings(w)
	if !ok {
		return
	}
	var patch settings.Patch
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAuthBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&patch); err != nil {
		if errors.Is(err, settings.ErrInvalid) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request json: "+jsonProblem(err))
		return
	}
	updated, err := svc.Update(r.Context(), patch)
	if err != nil {
		s.writeSettingsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsResponse{Settings: updated, RestartRequired: []string{}})
}

func (s *Server) handleGenerateKey(w http.ResponseWriter, _ *http.Request) {
	key, err := settings.GenerateKey()
	if err != nil {
		s.writeSettingsError(w, err)
		return
	}
	// The identity is shown once; it must not linger in caches.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, key)
}

// writeSettingsError maps settings errors to HTTP responses without key material.
func (s *Server) writeSettingsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, settings.ErrInvalid), errors.Is(err, settings.ErrMaskedSecret):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Error("settings request failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// jsonProblem describes a JSON decoding error without echoing request content.
func jsonProblem(err error) string {
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return "malformed JSON"
	case errors.As(err, &typeErr):
		return "wrong type for " + typeErr.Field
	}
	msg := err.Error()
	const unknown = "json: unknown field "
	if len(msg) > len(unknown) && msg[:len(unknown)] == unknown {
		return "unknown field " + msg[len(unknown):]
	}
	return "could not decode the body"
}

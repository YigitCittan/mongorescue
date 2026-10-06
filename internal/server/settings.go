package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
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
	mux.HandleFunc("POST /api/v1/settings/warnings/{id}/dismiss", s.handleDismissWarning)
}

// settingsResponse is returned by GET and PUT /api/v1/settings. Secrets are masked.
// RestartRequired lists settings that only apply after a restart (currently none:
// every setting applies to the next operation or request). Warnings lists active
// persistent warnings for the dashboard banner (see settings.Service.Warnings).
type settingsResponse struct {
	settings.Settings
	RestartRequired []string           `json:"restart_required"`
	Warnings        []settings.Warning `json:"warnings"`
	// PendingChanges lists the lowered protections (a shorter grace period or job
	// retention) that take effect later.
	PendingChanges []*models.PendingChange `json:"pending_changes"`
	// ApprovalsRequested lists the requests a PUT created for a second
	// administrator (turning the two-person rule off, or a lower grace period with it
	// on).
	ApprovalsRequested []*models.Approval `json:"approvals_requested,omitempty"`
}

// pendingChanges returns the pending changes for a settings response ([] when they
// cannot be listed, which is logged).
func (s *Server) pendingChanges(r *http.Request) []*models.PendingChange {
	if s.ops == nil {
		return []*models.PendingChange{}
	}
	list, err := s.ops.PendingChanges(r.Context())
	if err != nil {
		s.logger.Warn("cannot list pending protection changes", logsafe.Error(err))
		return []*models.PendingChange{}
	}
	return list
}

// requireSettings returns the settings service, answering 503 when absent.
func (s *Server) requireSettings(w http.ResponseWriter) (*settings.Service, bool) {
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings are not configured")
		return nil, false
	}
	return s.settings, true
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireSettings(w)
	if !ok {
		return
	}
	s.refreshRecoveryKit(r.Context())
	masked := svc.Masked()
	// The connections of the group mappings name connections: only administrators
	// see them; everyone else gets the groups and roles.
	if !auth.PrincipalFrom(r.Context()).Allows(auth.ScopeAdmin) {
		for i, m := range masked.OIDC.RoleMappings {
			masked.OIDC.RoleMappings[i] = m.Redacted()
		}
	}
	writeJSON(w, http.StatusOK, settingsResponse{Settings: masked, RestartRequired: []string{}, Warnings: s.warnings(r.Context(), svc),
		PendingChanges: s.pendingChanges(r)})
}

// handleDismissWarning dismisses a persistent warning for good.
func (s *Server) handleDismissWarning(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireSettings(w)
	if !ok {
		return
	}
	s.refreshRecoveryKit(r.Context())
	if err := svc.DismissWarning(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, settings.ErrInvalid) {
			writeError(w, http.StatusNotFound, "unknown warning")
			return
		}
		s.writeSettingsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"warnings": s.warnings(r.Context(), svc)})
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
	// Through the operations service: lowering a delete protection is delayed or
	// waits for a second administrator.
	var res *operations.SettingsUpdate
	if s.ops != nil && s.ops.ManagesSettings() {
		var err error
		if res, err = s.ops.UpdateSettings(r.Context(), patch); err != nil {
			if errors.Is(err, operations.ErrTooFewAdmins) || errors.Is(err, operations.ErrUnavailable) ||
				errors.Is(err, operations.ErrRequesterUnknown) || errors.Is(err, operations.ErrInvalid) {
				s.writeOperationError(w, err)
				return
			}
			s.writeSettingsError(w, err)
			return
		}
	} else {
		// Without the operations service nothing can count the administrators, so
		// the two-person rule cannot be turned on; lowering a protection is refused
		// by the settings service itself.
		if patch.Security != nil && patch.Security.RequireSecondApprover != nil && *patch.Security.RequireSecondApprover {
			writeError(w, http.StatusServiceUnavailable, "the two-person rule cannot be turned on in this configuration")
			return
		}
		updated, changed, err := svc.UpdateChanged(r.Context(), patch)
		if err != nil {
			s.writeSettingsError(w, err)
			return
		}
		res = &operations.SettingsUpdate{Settings: updated, Changed: changed}
	}
	annotateSettings(r.Context(), res.Changed)
	s.refreshRecoveryKit(r.Context())
	writeJSON(w, http.StatusOK, settingsResponse{Settings: res.Settings, RestartRequired: []string{}, Warnings: s.warnings(r.Context(), svc),
		PendingChanges: s.pendingChanges(r), ApprovalsRequested: res.Approvals})
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
	case errors.Is(err, settings.ErrInvalid), errors.Is(err, settings.ErrMaskedSecret), errors.Is(err, settings.ErrSecretReentry):
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

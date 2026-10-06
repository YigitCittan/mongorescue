package server

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/recoverykit"
)

// maxRecoveryKitBody bounds the JSON body of POST /api/v1/recovery-kit.
const maxRecoveryKitBody = 8 << 10

// recoveryKitRoute is the download route, also its audit label.
const recoveryKitRoute = "POST /api/v1/recovery-kit"

// WithMetadataBackup enables the metadata backup endpoints (GET
// /api/v1/metadata-backup and POST /api/v1/metadata-backup/run).
func WithMetadataBackup(svc *metabackup.Service) Option {
	return func(s *Server) { s.metaBackup = svc }
}

// WithRecoveryKit enables the recovery kit endpoints and keeps the recovery kit
// reminder in the settings warnings current.
func WithRecoveryKit(svc *recoverykit.Service) Option {
	return func(s *Server) { s.recoveryKit = svc }
}

// registerRecoveryRoutes adds the metadata backup and recovery kit endpoints.
func (s *Server) registerRecoveryRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/metadata-backup", s.handleMetadataBackupStatus)
	mux.HandleFunc("POST /api/v1/metadata-backup/run", s.handleRunMetadataBackup)
	mux.HandleFunc("GET /api/v1/recovery-kit", s.handleRecoveryKitStatus)
	mux.HandleFunc(recoveryKitRoute, s.handleDownloadRecoveryKit)
}

// handleMetadataBackupStatus returns the metadata backup status.
func (s *Server) handleMetadataBackupStatus(w http.ResponseWriter, r *http.Request) {
	if s.metaBackup == nil {
		writeError(w, http.StatusServiceUnavailable, "metadata backups are not configured")
		return
	}
	writeJSON(w, http.StatusOK, s.metaBackup.Status(r.Context()))
}

// handleRunMetadataBackup starts a metadata snapshot in the background; poll GET
// /api/v1/metadata-backup for its outcome.
func (s *Server) handleRunMetadataBackup(w http.ResponseWriter, r *http.Request) {
	if s.metaBackup == nil {
		writeError(w, http.StatusServiceUnavailable, "metadata backups are not configured")
		return
	}
	switch err := s.metaBackup.Trigger(); {
	case errors.Is(err, metabackup.ErrBusy):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, metabackup.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		s.logger.Error("metadata backup could not start", logsafe.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	st := s.metaBackup.Status(r.Context())
	st.Running = true
	writeJSON(w, http.StatusAccepted, st)
}

// recoveryKitStatus is returned by GET /api/v1/recovery-kit.
type recoveryKitStatus struct {
	// DownloadedAt is when the last kit was downloaded (absent when never).
	DownloadedAt *time.Time `json:"downloaded_at,omitempty"`
	// UpToDate reports whether the last kit still carries the current material.
	UpToDate bool `json:"up_to_date"`
	// MinPassphraseLength is the minimum passphrase length.
	MinPassphraseLength int `json:"min_passphrase_length"`
}

// handleRecoveryKitStatus reports when the last recovery kit was downloaded.
func (s *Server) handleRecoveryKitStatus(w http.ResponseWriter, r *http.Request) {
	if s.recoveryKit == nil || s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "the recovery kit is not configured")
		return
	}
	s.refreshRecoveryKit(r.Context())
	st := s.settings.RecoveryKitStatus()
	writeJSON(w, http.StatusOK, recoveryKitStatus{DownloadedAt: st.DownloadedAt, UpToDate: st.UpToDate, MinPassphraseLength: recoverykit.MinPassphraseLength})
}

// recoveryKitRequest is the body of POST /api/v1/recovery-kit. Neither field is ever
// logged or audited.
type recoveryKitRequest struct {
	// Passphrase seals the kit (age scrypt).
	Passphrase string `json:"passphrase"`
	// CurrentPassword re-authenticates the signed-in user.
	CurrentPassword string `json:"current_password"`
	// IncludePreviousKey adds secret.key.previous, the key the last secret key
	// rotation replaced (metadata snapshots taken before it need it).
	IncludePreviousKey bool `json:"include_previous_key,omitempty"`
}

// handleDownloadRecoveryKit streams the recovery kit, sealed with the passphrase
// from the body, to a signed-in admin who confirmed their password. API keys are
// refused. This is the one response that carries secrets (secret.key, private keys,
// storage credentials) in a form the client can open: it is never cached, logged or
// audited beyond the fact of the download.
func (s *Server) handleDownloadRecoveryKit(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	authSvc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	if s.recoveryKit == nil {
		writeError(w, http.StatusServiceUnavailable, "the recovery kit is not configured")
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req recoveryKitRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRecoveryKitBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request json: "+jsonProblem(err))
		return
	}
	if err := authSvc.ConfirmPassword(r.Context(), p, req.CurrentPassword); err != nil {
		// The audit entry carries the status the client gets (403, 429 when
		// throttled, ...). API key requests are audited by the auth middleware.
		rec := &statusRecorder{ResponseWriter: w}
		s.writeAuthError(rec, err)
		if p.Method == auth.MethodSession {
			result, msg := audit.ResultDenied, "password not confirmed"
			switch {
			case rec.status == http.StatusTooManyRequests:
				result, msg = audit.ResultRateLimited, "too many failed attempts"
			case rec.status >= http.StatusInternalServerError:
				result, msg = audit.ResultError, "password check failed"
			}
			s.auditRecoveryKit(r.Context(), p, start, result, rec.status, msg)
		}
		return
	}
	if err := recoverykit.ValidatePassphrase(req.Passphrase); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	kit, err := s.recoveryKit.PrepareWith(r.Context(), recoverykit.PrepareOptions{IncludePreviousKey: req.IncludePreviousKey})
	if errors.Is(err, recoverykit.ErrNoPreviousKey) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.logger.Error("recovery kit could not be prepared", logsafe.Error(err))
		s.auditRecoveryKit(r.Context(), p, start, audit.ResultError, http.StatusInternalServerError, "kit could not be prepared")
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": recoverykit.FileName(kit.CreatedAt())}))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err = s.recoveryKit.Seal(w, kit, req.Passphrase); err != nil {
		// The status is sent; the client sees a truncated stream that does not decrypt.
		s.logger.Error("recovery kit download failed", logsafe.Error(err))
		s.auditRecoveryKit(r.Context(), p, start, audit.ResultError, http.StatusOK, "download interrupted")
		return
	}
	if err = s.recoveryKit.MarkDownloaded(r.Context(), kit); err != nil {
		s.logger.Error("could not record the recovery kit download", logsafe.Error(err))
	}
	s.auditRecoveryKit(r.Context(), p, start, audit.ResultOK, http.StatusOK, "")
	s.logger.Info("recovery kit downloaded", logsafe.Attr("user_id", p.UserID()))
}

// auditRecoveryKit records a recovery kit download attempt of p: who and the
// outcome, never the passphrase, the password or the content.
func (s *Server) auditRecoveryKit(ctx context.Context, p *auth.Principal, start time.Time, result string, status int, msg string) {
	if s.audit == nil {
		return
	}
	username := ""
	if p.User != nil {
		username = p.User.Username
	}
	args, _ := json.Marshal(map[string]string{"actor": "user:" + p.UserID(), "username": username})
	s.audit.Record(context.WithoutCancel(ctx), audit.Entry{
		Time: start, APIKeyID: p.APIKeyID, APIKeyName: p.APIKeyName, Transport: audit.TransportREST, Tool: recoveryKitRoute,
		Arguments: args, Result: result, Error: msg, HTTPStatus: status, DurationMS: time.Since(start).Milliseconds(),
		Coalesce: result != audit.ResultOK,
	})
}

// refreshRecoveryKit updates the fingerprint behind the recovery kit reminder, so
// the warnings reflect changed storage targets and keys.
func (s *Server) refreshRecoveryKit(ctx context.Context) {
	if s.recoveryKit == nil {
		return
	}
	if err := s.recoveryKit.Refresh(ctx); err != nil && ctx.Err() == nil {
		s.logger.Warn("could not check whether the recovery kit is current", logsafe.Error(err))
	}
}

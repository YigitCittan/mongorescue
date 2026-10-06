package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// maxRotateBody bounds the JSON body of the key rotation endpoints.
const maxRotateBody = 8 << 10

// KeyRotationInfo describes the secret key for the key rotation status
// (implemented by *keyrotation.Rotator).
type KeyRotationInfo interface {
	// FromEnv reports a key from MONGORESCUE_SECRET_KEY.
	FromEnv() bool
	// Fingerprint identifies the current key.
	Fingerprint() string
	// PreviousKeyKept reports whether secret.key.previous exists.
	PreviousKeyKept() bool
}

// WithKeyRotation enables the key rotation endpoints.
func WithKeyRotation(info KeyRotationInfo) Option {
	return func(s *Server) { s.keyRotation = info }
}

// registerKeyRotationRoutes adds the key rotation endpoints.
func (s *Server) registerKeyRotationRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/security/key-rotation", s.handleKeyRotationStatus)
	mux.HandleFunc("POST /api/v1/security/rotate-secret-key", s.handleRotateSecretKey)
	mux.HandleFunc("POST /api/v1/encryption/rotate", s.handleRotateEncryptionKey)
	mux.HandleFunc("GET /api/v1/encryption/reencryption", s.handleReencryptionStatus)
	mux.HandleFunc("POST /api/v1/storage-targets/{id}/rotate-credentials", s.handleRotateCredentials)
}

// handleRotateCredentials probes new S3 credentials of a storage target and swaps
// them in when every probe passes. A failed probe answers 422 with the steps.
func (s *Server) handleRotateCredentials(w http.ResponseWriter, r *http.Request) {
	var req targets.Credentials
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRotateBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request json: "+jsonProblem(err))
		return
	}
	res, err := s.ops.RotateStorageCredentials(r.Context(), r.PathValue("id"), req)
	switch {
	case errors.Is(err, targets.ErrProbeFailed):
		writeErrorData(w, http.StatusUnprocessableEntity, err.Error(), res)
		return
	case errors.Is(err, targets.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, targets.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.writeOperationError(w, err)
		return
	}
	s.refreshRecoveryKit(r.Context())
	writeJSON(w, http.StatusOK, res)
}

// handleRotateEncryptionKey rotates the backup encryption key (body:
// operations.EncryptionRotationRequest) and optionally starts the re-encryption of
// existing backups. The new passphrase is never logged or echoed.
func (s *Server) handleRotateEncryptionKey(w http.ResponseWriter, r *http.Request) {
	var req operations.EncryptionRotationRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRotateBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request json: "+jsonProblem(err))
		return
	}
	res, err := s.ops.RotateEncryptionKey(r.Context(), req)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	s.refreshRecoveryKit(r.Context())
	writeJSON(w, http.StatusOK, res)
}

// handleReencryptionStatus returns the latest re-encryption job (null when none).
func (s *Server) handleReencryptionStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.ops.ReencryptionStatus(r.Context())
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// secretKeyStatus is the secret key part of GET /api/v1/security/key-rotation.
type secretKeyStatus struct {
	// FromEnv reports a key from MONGORESCUE_SECRET_KEY (rotated by hand).
	FromEnv bool `json:"from_env"`
	// Fingerprint identifies the current key; it reveals nothing about it.
	Fingerprint string `json:"fingerprint"`
	// PreviousKeyKept reports that secret.key.previous exists (it can be added to
	// the next recovery kit).
	PreviousKeyKept bool `json:"previous_key_kept"`
}

// keyRotationStatus is returned by GET /api/v1/security/key-rotation.
type keyRotationStatus struct {
	SecretKey *secretKeyStatus `json:"secret_key"`
}

// handleKeyRotationStatus reports what the key rotation actions would act on.
func (s *Server) handleKeyRotationStatus(w http.ResponseWriter, _ *http.Request) {
	out := keyRotationStatus{}
	if s.keyRotation != nil {
		out.SecretKey = &secretKeyStatus{FromEnv: s.keyRotation.FromEnv(), Fingerprint: s.keyRotation.Fingerprint(),
			PreviousKeyKept: s.keyRotation.PreviousKeyKept()}
	}
	writeJSON(w, http.StatusOK, out)
}

// rotateSecretKeyRequest is the body of POST /api/v1/security/rotate-secret-key. It
// is never logged or audited.
type rotateSecretKeyRequest struct {
	// CurrentPassword re-authenticates the signed-in administrator.
	CurrentPassword string `json:"current_password"`
}

// rotateSecretKeyResponse is the answer of a finished rotation.
type rotateSecretKeyResponse struct {
	*keyrotation.Result
	// SignInAgain is always true: every session, this one included, has ended.
	SignInAgain bool `json:"sign_in_again"`
}

// handleRotateSecretKey rotates secret.key for a signed-in administrator who
// confirmed their password (API keys are refused). With the two-person rule it
// answers 202 with the approval request. Afterwards every session has ended.
func (s *Server) handleRotateSecretKey(w http.ResponseWriter, r *http.Request) {
	authSvc, ok := s.requireAuth(w)
	if !ok {
		return
	}
	if s.keyRotation == nil {
		writeError(w, http.StatusServiceUnavailable, "secret key rotation is not configured")
		return
	}
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req rotateSecretKeyRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRotateBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request json: "+jsonProblem(err))
		return
	}
	if err := authSvc.ConfirmPassword(r.Context(), p, req.CurrentPassword); err != nil {
		s.writeAuthError(w, err)
		return
	}
	res, err := s.ops.RotateSecretKey(r.Context())
	switch {
	case errors.Is(err, keyrotation.ErrEnvKey), errors.Is(err, keyrotation.ErrBusy), errors.Is(err, keyrotation.ErrPending):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, keyrotation.ErrIncomplete):
		s.logger.Error("secret key rotation incomplete", logsafe.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	case err != nil:
		s.writeOperationError(w, err)
		return
	}
	s.refreshRecoveryKit(r.Context())
	s.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, rotateSecretKeyResponse{Result: res, SignInAgain: true})
}

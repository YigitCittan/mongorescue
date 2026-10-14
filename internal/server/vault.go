package server

import (
	"errors"
	"net/http"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/vaultcreds"
)

// Routes of the credentials read from Vault.
const (
	vaultStatusRoute = "GET /api/v1/vault"
	vaultTestRoute   = "POST /api/v1/vault/test"
)

// WithVaultCredentials enables GET /api/v1/vault and POST /api/v1/vault/test
// with the resolver of credentials read from Vault.
func WithVaultCredentials(r *vaultcreds.Resolver) Option {
	return func(s *Server) { s.vault = r }
}

// registerVaultRoutes adds the Vault endpoints.
func (s *Server) registerVaultRoutes(mux *router) {
	mux.HandleFunc(vaultStatusRoute, s.handleVaultStatus)
	mux.HandleFunc(vaultTestRoute, s.handleVaultTest)
}

// handleVaultStatus reports whether credentials can be read from Vault (the
// dashboard offers the option only then). It never names the Vault address.
func (s *Server) handleVaultStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"configured": s.vault.Configured()})
}

// vaultTestRequest is the body of POST /api/v1/vault/test.
type vaultTestRequest struct {
	// Use is "connection" or "s3": what the credentials are for.
	Use models.VaultUse `json:"use"`
	// Vault is the reference to resolve.
	Vault models.VaultRef `json:"vault"`
}

// handleVaultTest resolves a Vault reference without returning any credential:
// kv-v2 fields are read and checked, dynamic credentials issued and revoked at
// once. The answer says what was found (field names, the lease TTL), never a
// value.
func (s *Server) handleVaultTest(w http.ResponseWriter, r *http.Request) {
	var req vaultTestRequest
	if !decodeBody(w, r, &req) {
		return
	}
	switch req.Use {
	case models.VaultForConnection, models.VaultForS3:
	default:
		writeError(w, http.StatusBadRequest, "use must be connection or s3")
		return
	}
	res, err := s.vault.Test(r.Context(), req.Use, req.Vault)
	if err != nil {
		if errors.Is(err, models.ErrInvalidVaultRef) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.logger.Error("vault test failed", logsafe.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

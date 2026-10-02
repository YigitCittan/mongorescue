package server

import (
	"net/http"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/readiness"
)

// WithReadiness serves GET /api/v1/readiness from svc. Without it the server builds
// a report-only service on its metadata store.
func WithReadiness(svc *readiness.Service) Option {
	return func(s *Server) { s.readiness = svc }
}

// defaultReadiness returns a readiness service on the server's metadata store (no
// checker, no events), or nil when the store does not provide what it needs.
func (s *Server) defaultReadiness() *readiness.Service {
	st, ok := s.metaStore.(readiness.Store)
	if !ok {
		return nil
	}
	cfg := readiness.Config{Store: st, Logger: s.logger}
	if s.connections != nil {
		cfg.Connections = s.connections
	}
	if s.settings != nil {
		cfg.KeysEscrowed = func() bool { return s.settings.RecoveryKitStatus().UpToDate }
	}
	return readiness.New(cfg)
}

// registerReadinessRoutes adds the recovery readiness report.
func (s *Server) registerReadinessRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/readiness", s.handleReadiness)
}

// handleReadiness returns one row per database a job backs up: its last good,
// verified and restore-tested backups, its RPO and estimated RTO, whether the keys
// are escrowed and an overall ok, warn or fail status.
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if s.readiness == nil {
		writeError(w, http.StatusServiceUnavailable, "the readiness report is not available")
		return
	}
	report, err := s.readiness.Report(r.Context())
	if err != nil {
		s.logger.Warn("could not compute the readiness report", logsafe.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

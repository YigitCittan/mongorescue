package server

import (
	"encoding/json"
	"net/http"
	"sync"

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
	body, err := s.readinessBodies.encode(report)
	if err != nil {
		s.logger.Warn("could not encode the readiness report", logsafe.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// maxEncodedReports bounds encodedReports: one per connection access asking at the
// same time is enough.
const maxEncodedReports = 8

// encodedReports keeps the encoded responses (writeJSON's envelope) of the newest
// readiness reports. A shared report (readiness.Config.ReportCacheTTL) is never
// changed, so its pointer identifies its encoding: dashboards asking for the same
// report get the same bytes instead of encoding megabytes each at 10,000 jobs.
type encodedReports struct {
	mu      sync.Mutex
	reports []*readiness.Report
	bodies  [][]byte
}

// encode returns the response body of report, encoding it unless it is one of the
// newest reports.
func (e *encodedReports) encode(report *readiness.Report) ([]byte, error) {
	e.mu.Lock()
	for i, r := range e.reports {
		if r == report {
			body := e.bodies[i]
			e.mu.Unlock()
			return body, nil
		}
	}
	e.mu.Unlock()
	body, err := json.Marshal(apiResponse{Success: true, Data: report})
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.reports) == maxEncodedReports {
		e.reports, e.bodies = e.reports[1:], e.bodies[1:]
	}
	e.reports, e.bodies = append(e.reports, report), append(e.bodies, body)
	return body, nil
}

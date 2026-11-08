package server

import (
	"errors"
	"net/http"

	"github.com/yigitcittan/mongorescue/internal/apierr"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/cost"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
)

// WithCost serves GET /api/v1/storage/usage from svc. Without it the endpoint
// answers 503.
func WithCost(svc *cost.Service) Option {
	return func(s *Server) { s.cost = svc }
}

// registerCostRoutes adds the storage usage and cost report.
func (s *Server) registerCostRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/storage/usage", s.handleStorageUsage)
}

// handleStorageUsage reports the stored size per storage target, job and database,
// its daily history, the estimated monthly cost from the price entered on each
// target and the estimated egress of verification, restore tests and copies
// (?days=30, 1 to 400). It spans every connection, so a key limited to some
// connections is refused.
func (s *Server) handleStorageUsage(w http.ResponseWriter, r *http.Request) {
	if auth.ConnectionFilter(r.Context()).Limited() {
		writeErrorCode(w, http.StatusForbidden, apierr.Forbidden, "forbidden: a key limited to some connections cannot read the storage usage of every connection")
		return
	}
	if s.cost == nil {
		writeError(w, http.StatusServiceUnavailable, "the storage usage report is not available")
		return
	}
	days, ok := optionalInt(w, r, "days")
	if !ok {
		return
	}
	n := 0
	if days != nil {
		if *days == 0 {
			// 0 would mean the default in the service; here it is out of range.
			n = -1
		} else {
			n = *days
		}
	}
	report, err := s.cost.Report(r.Context(), n)
	if err != nil {
		if errors.Is(err, cost.ErrInvalid) {
			writeErrorCode(w, http.StatusBadRequest, apierr.InvalidRequest, err.Error())
			return
		}
		s.logger.WarnContext(r.Context(), "could not compute the storage usage report", logsafe.Error(err), requestIDAttr(w))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

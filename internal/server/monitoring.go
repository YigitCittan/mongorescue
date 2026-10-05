package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/yigitcittan/mongorescue/internal/heartbeat"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
)

// heartbeatTestRoute sends a test ping to the global heartbeat URL.
const heartbeatTestRoute = "POST /api/v1/settings/monitoring/test"

// Scheduler states reported by GET /api/v1/health.
const (
	// schedulerOK is a scheduler that ticked within scheduler.StaleAfter.
	schedulerOK = "ok"
	// schedulerStale is a scheduler whose last tick is older than
	// scheduler.StaleAfter: hung, deadlocked or stopped. The health check fails.
	schedulerStale = "stale"
	// schedulerNotStarted is a scheduler that has not started yet.
	schedulerNotStarted = "not_started"
)

// WithHeartbeat enables POST /api/v1/settings/monitoring/test, which sends a test
// ping through svc.
func WithHeartbeat(svc *heartbeat.Service) Option {
	return func(s *Server) { s.heartbeat = svc }
}

// registerMonitoringRoutes adds the monitoring endpoints.
func (s *Server) registerMonitoringRoutes(mux *router) {
	mux.HandleFunc(heartbeatTestRoute, s.handleTestHeartbeat)
}

// schedulerLiveness is the part of the scheduler the health check reads
// (implemented by *scheduler.Scheduler).
type schedulerLiveness interface {
	// LastTick returns the time of the last liveness tick (zero before Start).
	LastTick() time.Time
	// Stale reports whether the last tick is older than scheduler.StaleAfter.
	Stale(now time.Time) bool
}

// liveness returns the scheduler liveness source, or nil without a scheduler.
func (s *Server) liveness() schedulerLiveness {
	switch {
	case s.livenessSource != nil:
		return s.livenessSource
	case s.scheduler != nil:
		return s.scheduler
	}
	return nil
}

// handleHealth reports liveness. It answers 503 with status "unhealthy" and
// scheduler "stale" when the scheduler's last tick is older than
// scheduler.StaleAfter, so a hung scheduler fails the health check; otherwise 200
// with status "healthy". scheduler_last_tick is the time of that tick.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().UTC()
	body := map[string]any{
		"status":  "healthy",
		"version": s.version,
		"time":    now,
	}
	live := s.liveness()
	if live == nil {
		writeJSON(w, http.StatusOK, body)
		return
	}
	last := live.LastTick()
	if !last.IsZero() {
		body["scheduler_last_tick"] = last.UTC()
	}
	switch {
	case last.IsZero():
		body["scheduler"] = schedulerNotStarted
	case live.Stale(now):
		body["scheduler"] = schedulerStale
		body["status"] = "unhealthy"
		writeErrorData(w, http.StatusServiceUnavailable, "the scheduler is stale: its last tick is older than "+scheduler.StaleAfter.String(), body)
		return
	default:
		body["scheduler"] = schedulerOK
	}
	writeJSON(w, http.StatusOK, body)
}

// heartbeatTestRequest is the body of POST /api/v1/settings/monitoring/test.
// HeartbeatURL is the URL to ping; empty, SecretMask or the masked stored URL ping
// the stored monitoring.heartbeat_url.
type heartbeatTestRequest struct {
	HeartbeatURL string `json:"heartbeat_url"`
}

// handleTestHeartbeat sends one success ping and reports the outcome. Failures are
// 502 with a message naming the URL's host only.
func (s *Server) handleTestHeartbeat(w http.ResponseWriter, r *http.Request) {
	if s.heartbeat == nil {
		writeError(w, http.StatusServiceUnavailable, "the heartbeat is not configured")
		return
	}
	svc, ok := s.requireSettings(w)
	if !ok {
		return
	}
	var req heartbeatTestRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAuthBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request json: "+jsonProblem(err))
		return
	}
	stored := svc.Current().Monitoring.HeartbeatURL
	target := stored
	if req.HeartbeatURL != "" {
		v, err := models.ResolveHeartbeatURL(req.HeartbeatURL, stored)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		target = v
	}
	if target == "" {
		writeError(w, http.StatusBadRequest, "no heartbeat URL is configured; enter one first")
		return
	}
	if err := models.ValidateHeartbeatURL(target); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.heartbeat.Test(r.Context(), target); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	host := ""
	if u, err := url.Parse(target); err == nil {
		host = u.Hostname()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "host": host})
}

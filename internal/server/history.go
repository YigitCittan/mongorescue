package server

import (
	"fmt"
	"net/http"

	"github.com/yigitcittan/mongorescue/internal/operations"
)

// schedulePreviewRuns is how many activations GET /api/v1/schedule/preview returns
// without ?n.
const schedulePreviewRuns = 3

// registerHistoryRoutes adds the dashboard overview's history and the cron preview.
func (s *Server) registerHistoryRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/stats/history", s.handleStatsHistory)
	mux.HandleFunc("GET /api/v1/schedule/preview", s.handleSchedulePreview)
}

// handleStatsHistory serves the outcomes and stored sizes per day, the jobs' recent
// runs, the next day's scheduled runs and the failed verifications
// (?days=30&tz=<IANA zone>&tz_offset=<minutes east of UTC, used when tz is
// missing or unknown>).
func (s *Server) handleStatsHistory(w http.ResponseWriter, r *http.Request) {
	days, ok := optionalInt(w, r, "days")
	if !ok {
		return
	}
	offset, ok := optionalInt(w, r, "tz_offset")
	if !ok {
		return
	}
	req := operations.HistoryRequest{TimeZone: r.URL.Query().Get("tz")}
	if days != nil {
		if *days == 0 {
			// 0 would mean the default in the request; here it is out of range.
			writeError(w, http.StatusBadRequest, fmt.Sprintf("days must be between 1 and %d", operations.MaxHistoryDays))
			return
		}
		req.Days = *days
	}
	if offset != nil {
		req.OffsetMinutes = *offset
	}
	h, err := s.ops.History(r.Context(), req)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// handleSchedulePreview reports whether ?cron= is a valid schedule and its next
// activations as the scheduler computes them (?n=3).
func (s *Server) handleSchedulePreview(w http.ResponseWriter, r *http.Request) {
	n, ok := optionalInt(w, r, "n")
	if !ok {
		return
	}
	count := schedulePreviewRuns
	if n != nil {
		count = *n
	}
	p, err := s.ops.PreviewSchedule(r.URL.Query().Get("cron"), count)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

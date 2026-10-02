package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
)

// Audit log routes.
const (
	auditListRoute   = "GET /api/v1/audit"
	auditExportRoute = "GET /api/v1/audit/export"
	auditVerifyRoute = "GET /api/v1/audit/verify"
)

// exportWriteTimeout bounds the whole audit log export (the server's write timeout
// is meant for JSON answers).
const exportWriteTimeout = 15 * time.Minute

// auditedReads are the GET routes the audit log records although they change
// nothing: downloads that copy data out of MongoRescue.
var auditedReads = map[string]bool{
	auditExportRoute: true,
}

// WithAuditLog enables the audit log of every action: the auth middleware records
// mutating requests, sign-ins and setup into svc, and GET /api/v1/audit,
// /api/v1/audit/export and /api/v1/audit/verify serve it.
func WithAuditLog(svc *auditlog.Service) Option {
	return func(s *Server) { s.auditLog = svc }
}

// registerAuditLogRoutes adds the audit log endpoints.
func (s *Server) registerAuditLogRoutes(mux *router) {
	mux.HandleFunc(auditListRoute, s.handleListAuditLog)
	mux.HandleFunc(auditExportRoute, s.handleExportAuditLog)
	mux.HandleFunc(auditVerifyRoute, s.handleVerifyAuditLog)
}

// auditsAction reports whether a request is recorded in the audit log: every
// request to the REST API that may change something (not GET, HEAD or OPTIONS) and
// the audited downloads. MCP records its own tool calls; /metrics is never audited.
func auditsAction(method, path, pattern string) bool {
	if !strings.HasPrefix(path, "/api/") {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return auditedReads[pattern]
	case http.MethodOptions:
		return false
	}
	return true
}

// recordAction records a request of p in the audit log: the route pattern (never the
// raw path), its path parameters, the response status and the principal.
func (s *Server) recordAction(r *http.Request, p *auth.Principal, pattern string, rec *statusRecorder) {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	e := auditlog.Event{
		Action: routeLabel(r.Method, pattern), Targets: pathTargets(r, pattern), Status: status,
		ClientIP: s.clientIP(r), UserAgent: r.UserAgent(),
	}
	setActor(&e, p)
	s.auditLog.Record(r.Context(), e)
}

// setActor fills the actor fields of e from p.
func setActor(e *auditlog.Event, p *auth.Principal) {
	if p == nil {
		e.ActorKind = auditlog.ActorAnonymous
		return
	}
	if p.User != nil {
		e.ActorUserID, e.ActorName = p.User.ID, p.User.Username
	}
	switch p.Method {
	case auth.MethodAPIKey:
		e.ActorKind, e.ActorKeyID, e.ActorKeyName = auditlog.ActorAPIKey, p.APIKeyID, p.APIKeyName
	case auth.MethodSession:
		e.ActorKind = auditlog.ActorUser
	default:
		e.ActorKind = auditlog.ActorSystem
	}
}

// recordSignIn records a sign-in or setup attempt: on success the user, otherwise
// the name that was tried (never the password) as an anonymous actor.
func (s *Server) recordSignIn(r *http.Request, action string, rec *statusRecorder, user *auth.User, tried string) {
	if s.auditLog == nil {
		return
	}
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	e := auditlog.Event{
		ActorKind: auditlog.ActorAnonymous, ActorName: tried, Action: action, Status: status,
		ClientIP: s.clientIP(r), UserAgent: r.UserAgent(),
	}
	if user != nil {
		e.ActorKind, e.ActorUserID, e.ActorName = auditlog.ActorUser, user.ID, user.Username
	}
	s.auditLog.Record(r.Context(), e)
}

// pathTargets returns the path parameters of the matched route.
func pathTargets(r *http.Request, pattern string) map[string]string {
	out := map[string]string{}
	for _, m := range wildcardPattern.FindAllStringSubmatch(pattern, -1) {
		if v := r.PathValue(m[1]); v != "" {
			out[m[1]] = truncateUTF8(v, maxAuditPathValue)
		}
	}
	return out
}

// requireAuditLog returns the audit log, answering 503 when it is not configured.
func (s *Server) requireAuditLog(w http.ResponseWriter) (*auditlog.Service, bool) {
	if s.auditLog == nil {
		writeError(w, http.StatusServiceUnavailable, "the audit log is not configured")
		return nil, false
	}
	return s.auditLog, true
}

// auditFilter parses the filters of the audit log endpoints: actor, actor_kind,
// action, result, since and until (RFC 3339), and with paging before_id and limit.
func auditFilter(r *http.Request, paging bool) (auditlog.Filter, error) {
	q := r.URL.Query()
	f := auditlog.Filter{
		Actor: strings.TrimSpace(q.Get("actor")), ActorKind: strings.TrimSpace(q.Get("actor_kind")),
		Action: strings.TrimSpace(q.Get("action")), Outcome: strings.TrimSpace(q.Get("result")),
	}
	switch f.ActorKind {
	case "", auditlog.ActorUser, auditlog.ActorAPIKey, auditlog.ActorSystem, auditlog.ActorAnonymous:
	default:
		return f, errors.New("actor_kind must be user, api_key, system or anonymous")
	}
	switch f.Outcome {
	case "", auditlog.OutcomeOK, auditlog.OutcomeError, auditlog.OutcomeDenied, auditlog.OutcomeRateLimited:
	default:
		return f, errors.New("result must be ok, error, denied or rate_limited")
	}
	for _, p := range []struct {
		name string
		dst  *time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if v := q.Get(p.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return f, errors.New(p.name + " must be an RFC 3339 time such as 2026-01-02T15:04:05Z")
			}
			*p.dst = t
		}
	}
	if !paging {
		return f, nil
	}
	if v := q.Get("before_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return f, errors.New("before_id must be a positive entry ID")
		}
		f.BeforeID = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > auditlog.MaxListLimit {
			return f, errors.New("limit must be between 1 and " + strconv.Itoa(auditlog.MaxListLimit))
		}
		f.Limit = n
	}
	return f, nil
}

// auditListResponse is the answer of GET /api/v1/audit.
type auditListResponse struct {
	auditlog.Page
	// Forwarding reports the audit webhook's counters.
	Forwarding auditlog.ForwardStatus `json:"forwarding"`
}

// handleListAuditLog returns one page of the audit log, newest first.
func (s *Server) handleListAuditLog(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuditLog(w)
	if !ok {
		return
	}
	f, err := auditFilter(r, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := svc.List(r.Context(), f)
	if err != nil {
		s.writeAuditLogError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, auditListResponse{Page: page, Forwarding: svc.Forwarding()})
}

// handleExportAuditLog streams the entries matching the filters as JSON Lines,
// oldest first, one entry (with its hash) per line.
func (s *Server) handleExportAuditLog(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuditLog(w)
	if !ok {
		return
	}
	f, err := auditFilter(r, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(exportWriteTimeout))
	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": "mongorescue-audit-" + time.Now().UTC().Format("20060102T150405Z") + ".jsonl",
	}))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	bw := bufio.NewWriterSize(w, 32<<10)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	err = svc.Export(r.Context(), f, func(e *auditlog.Event) error { return enc.Encode(e) })
	if err == nil {
		err = bw.Flush()
	}
	if err != nil && r.Context().Err() == nil {
		// The status is sent; the client sees a truncated file.
		s.logger.Error("audit log export failed", logsafe.Error(err))
	}
}

// handleVerifyAuditLog walks the hash chain and reports the first broken entry.
func (s *Server) handleVerifyAuditLog(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.requireAuditLog(w)
	if !ok {
		return
	}
	v, err := svc.Verify(r.Context())
	if err != nil {
		s.writeAuditLogError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, v)
}

// writeAuditLogError maps audit log errors to responses.
func (s *Server) writeAuditLogError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auditlog.ErrInvalidFilter):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, auditlog.ErrNoRepository):
		writeError(w, http.StatusServiceUnavailable, "the audit log is not configured")
	default:
		s.logger.Error("audit log request failed", logsafe.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

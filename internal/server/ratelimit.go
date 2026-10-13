package server

import (
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/ratelimit"
)

// Callers and budgets of the REST API rate limit (the labels of
// mongorescue_api_rate_limited_total).
const (
	rateCallerAPIKey    = "api_key"
	rateCallerSession   = "session"
	rateCallerAnonymous = "anonymous"
	rateBudgetGeneral   = "general"
	rateBudgetWork      = "work"
)

// RateLimitCallers and RateLimitBudgets list the label values of the rate limit
// observer (see WithRateLimitObserver), so its series can start at zero.
var (
	RateLimitCallers = []string{rateCallerAPIKey, rateCallerSession, rateCallerAnonymous}
	RateLimitBudgets = []string{rateBudgetGeneral, rateBudgetWork}
)

// anonymousRateLimit is the budget of unauthenticated requests per client address
// (an IPv6 address counts by its /64): the public routes (sign-in, setup, single
// sign-on) and requests refused for missing or wrong credentials. Login and setup
// failures are throttled further by internal/auth.
var anonymousRateLimit = ratelimit.Limit{PerMinute: 300, Burst: 100}

// rateLimitLogInterval is how often a refused caller is logged and, when its
// refusal is not recorded anyway, audited.
const rateLimitLogInterval = time.Minute

// healthPath is the health check, which is never rate limited.
const healthPath = "/api/v1/health"

// codeRateLimited is the code of the 429 of a request over its rate limit.
const codeRateLimited = "rate_limited"

// reasonRateLimited is the audit reason of a rate-limited request.
const reasonRateLimited = "rate_limited"

// workRoutes are the routes that start work (backups, restores, job runs,
// verifications, restore tests, scans, imports, bulk actions, metadata
// snapshots) or reach another system (connection, storage and notification
// tests). They spend the tighter work budget (security.api_work_rate_limit_*) on
// top of the general one. A test checks that every entry is a registered route and
// that every operator route that starts work is listed.
var workRoutes = map[string]bool{
	"POST /api/v1/backups":                          true,
	"POST /api/v1/backups/{id}/retry":               true,
	"POST /api/v1/backups/{id}/verify":              true,
	"POST /api/v1/backups/{id}/copies/retry":        true,
	"POST /api/v1/backups/bulk":                     true,
	"POST /api/v1/restores/bulk":                    true,
	"POST /api/v1/jobs/bulk":                        true,
	"POST /api/v1/jobs/{id}/run":                    true,
	"POST /api/v1/jobs/{id}/restore-test":           true,
	"POST /api/v1/restore":                          true,
	"POST /api/v1/restores/preflight":               true,
	pitrBaseRoute:                                   true,
	pitrChainTestRoute:                              true,
	"POST /api/v1/integrity/sweep":                  true,
	"POST /api/v1/storage-targets/{id}/scan":        true,
	"POST /api/v1/storage-targets/{id}/import":      true,
	"POST /api/v1/metadata-backup/run":              true,
	"POST /api/v1/connections/test":                 true,
	"POST /api/v1/connections/{id}/test":            true,
	"POST /api/v1/storage-targets/test":             true,
	"POST /api/v1/storage-targets/{id}/test":        true,
	"POST /api/v1/notifications/channels/{id}/test": true,
}

// WithRateLimitObserver calls fn for every request refused by the REST API rate
// limit, with the caller (api_key, session or anonymous) and the budget (general
// or work), for mongorescue_api_rate_limited_total.
func WithRateLimitObserver(fn func(caller, budget string)) Option {
	return func(s *Server) { s.rateObserve = fn }
}

// apiRateLimits returns the general and work budgets of the security settings. A
// general budget of 0 per minute turns the whole limit off.
func (s *Server) apiRateLimits() (general, work ratelimit.Limit) {
	sec := s.security()
	general = ratelimit.Limit{PerMinute: sec.APIRateLimitPerMinute, Burst: sec.APIRateLimitBurst}
	if !general.Enabled() {
		return general, ratelimit.Limit{}
	}
	return general, ratelimit.Limit{PerMinute: sec.APIWorkRateLimitPerMinute, Burst: sec.APIWorkRateLimitBurst}
}

// rateKey returns the bucket key of a request: the API key, the session or, for
// p nil, the client address.
func (s *Server) rateKey(r *http.Request, p *auth.Principal) (key, caller string) {
	switch {
	case p != nil && p.Method == auth.MethodAPIKey:
		return "key:" + p.APIKeyID, rateCallerAPIKey
	case p != nil && p.Method == auth.MethodSession:
		return "session:" + p.SessionHash, rateCallerSession
	}
	return "ip:" + addressKey(s.clientIP(r)), rateCallerAnonymous
}

// addressKey returns the rate limit key of a client address: an IPv6 address by
// its /64 (one host usually holds a whole /64), anything else as is.
func addressKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if prefix, err := addr.Prefix(64); err == nil {
			return prefix.String()
		}
	}
	return addr.String()
}

// allowRate charges the request of p (nil: unauthenticated) to its budgets: the
// general one per API key or session (anonymousRateLimit per client address
// without a caller) and, for workRoutes, the work one. Over a budget it answers
// 429 with Retry-After and returns false. recorded says whether the audit log
// records the request anyway.
func (s *Server) allowRate(w http.ResponseWriter, r *http.Request, p *auth.Principal, pattern string, recorded bool) bool {
	general, work := s.apiRateLimits()
	if !general.Enabled() || s.rateBuckets == nil {
		return true
	}
	key, caller := s.rateKey(r, p)
	limit := general
	if caller == rateCallerAnonymous {
		limit = anonymousRateLimit
	}
	budget := rateBudgetGeneral
	ok, wait := s.rateBuckets.Allow(key, limit)
	if ok && caller != rateCallerAnonymous && work.Enabled() && workRoutes[pattern] {
		budget, limit = rateBudgetWork, work
		ok, wait = s.rateBuckets.Allow("work:"+key, work)
	}
	if ok {
		return true
	}
	s.refuseRate(w, r, p, pattern, caller, budget, limit, wait, key, recorded)
	return false
}

// refuseRate answers 429 with Retry-After, counts the refusal and, at most once a
// minute per caller, logs it and records it in the audit log unless it is
// recorded anyway.
func (s *Server) refuseRate(w http.ResponseWriter, r *http.Request, p *auth.Principal, pattern, caller, budget string, limit ratelimit.Limit,
	wait time.Duration, key string, recorded bool) {
	secs := max(int(math.Ceil(wait.Seconds())), 1)
	if s.rateObserve != nil {
		s.rateObserve(caller, budget)
	}
	if s.rateLog.Pass(key) {
		attrs := []any{slog.String("caller", caller), slog.String("budget", budget), slog.String("route", routeLabel(r.Method, pattern)),
			slog.Int("per_minute", limit.PerMinute), slog.Int("burst", limit.Burst)}
		switch {
		case p != nil && p.Method == auth.MethodAPIKey:
			attrs = append(attrs, slog.String("api_key_id", p.APIKeyID))
		case p != nil:
			attrs = append(attrs, logsafe.Attr("user_id", p.UserID()))
		default:
			attrs = append(attrs, logsafe.Attr("client_ip", s.clientIP(r)))
		}
		s.logger.Warn("API rate limit exceeded; further refusals of this caller are not logged for a minute", attrs...)
		if s.auditLog != nil && !recorded {
			e := auditlog.Event{
				Action: routeLabel(r.Method, pattern), Targets: map[string]string{targetReason: reasonRateLimited, "budget": budget},
				Status: http.StatusTooManyRequests, Outcome: auditlog.OutcomeRateLimited,
				ClientIP: s.clientIP(r), UserAgent: r.UserAgent(),
			}
			setActor(&e, p)
			s.auditLog.Record(r.Context(), e)
		}
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeErrorCode(w, http.StatusTooManyRequests, codeRateLimited,
		"too many requests: over the "+budget+" rate limit of "+strconv.Itoa(limit.PerMinute)+" per minute; retry in "+strconv.Itoa(secs)+"s")
}

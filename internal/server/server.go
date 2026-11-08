// Package server provides Go 1.22+ ServeMux HTTP routing, REST API endpoints,
// and static file serving for the embedded web dashboard.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/apierr"
	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/auth/oidc"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/cost"
	"github.com/yigitcittan/mongorescue/internal/diskguard"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/heartbeat"
	"github.com/yigitcittan/mongorescue/internal/integrity"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongouri"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
	"github.com/yigitcittan/mongorescue/internal/ratelimit"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/recoverykit"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/targets"
	"github.com/yigitcittan/mongorescue/internal/vaultcreds"
)

// Sentinel errors surfaced by the HTTP API.
var (
	// ErrInvalidID is returned (as HTTP 400) when a client-supplied identifier for a new
	// resource does not match ^[a-zA-Z0-9_-]{1,64}$. It aliases models.ErrInvalidID.
	ErrInvalidID = models.ErrInvalidID

	// ErrConnectionRequired is returned (as HTTP 400) when a job, backup or restore
	// does not name a connection. It aliases operations.ErrConnectionRequired.
	ErrConnectionRequired = operations.ErrConnectionRequired

	// ErrUnknownConnection is returned (as HTTP 400) when a request names a connection
	// that does not exist. It aliases operations.ErrUnknownConnection.
	ErrUnknownConnection = operations.ErrUnknownConnection
)

// Server coordinates HTTP API routing and dashboard delivery.
type Server struct {
	cfg           *config.Config
	metaStore     operations.Store
	backupEngine  *backup.Engine
	restoreEngine *restore.Engine
	storageDriver storage.Storage
	scheduler     *scheduler.Scheduler
	logger        *slog.Logger
	staticFS      fs.FS
	httpServer    *http.Server

	publisher      events.Publisher
	notifications  *notify.Service
	metricsHandler http.Handler
	onJobDeleted   func(jobID string)

	// runs owns manual backups, job runs and restores started through the API, which
	// outlive the HTTP request that triggered them.
	runs *runs.Manager
	// registry tracks active runs for an operations service built by the Server.
	registry *runs.Registry

	// ops implements the backup, job and restore use cases the handlers delegate to
	// (shared with the MCP server).
	ops *operations.Service

	// auth authenticates requests; connections manages MongoDB servers.
	auth        *auth.Service
	connections *connections.Service
	// vault resolves credentials read from Vault (its test and status).
	vault *vaultcreds.Resolver

	// settings holds the dashboard-managed configuration and targets the storage
	// targets; without them, defaults and the fixed storageDriver apply.
	settings *settings.Service
	targets  *targets.Service

	// integrity verifies archives, runs restore tests and scans storage targets.
	integrity *integrity.Service

	// metaBackup snapshots the metadata database; recoveryKit builds recovery kits.
	metaBackup  *metabackup.Service
	recoveryKit *recoverykit.Service
	// keyRotation describes secret.key for the key rotation endpoints (nil: none).
	keyRotation KeyRotationInfo

	// readiness reports the RPO, RTO and evidence of every database a job backs up.
	readiness *readiness.Service
	// cost reports the stored size and estimated cost per storage target.
	cost *cost.Service
	// readinessBodies holds the encoded responses of the newest shared reports.
	readinessBodies encodedReports

	// pitr runs the PITR oplog collectors and serves the stream API.
	pitr *collector.Service

	// heartbeat sends the test pings of POST /api/v1/settings/monitoring/test.
	heartbeat *heartbeat.Service
	// livenessSource overrides the scheduler as the health check's liveness source
	// (tests).
	livenessSource schedulerLiveness
	// heartbeatThrottle limits the test pings of each caller (see
	// handleTestHeartbeat).
	heartbeatThrottle *auth.Throttle

	// version is reported by the health endpoint.
	version string
	// diskGuard is consulted by the readiness check (see WithDiskGuard).
	diskGuard diskChecker

	// mux is the router built by buildRoutes; the auth middleware asks it which route
	// a request matches to enforce that route's scope. patterns lists every
	// registered route pattern.
	mux      *http.ServeMux
	patterns []string

	// mcpHandler serves /mcp; audit backs GET /api/v1/audit, the API key activity
	// log, and auditLog the audit log of every action (GET /api/v1/audit/events).
	mcpHandler http.Handler
	audit      *audit.Service
	auditLog   *auditlog.Service

	// desktopCSP selects desktopContentSecurityPolicy (see WithDesktopCSP).
	desktopCSP bool

	// haStatus reports the role in an active-passive HA cluster (see WithHA); nil
	// outside HA mode; standbyMetrics answers GET /metrics on a standby.
	haStatus func() HAStatus
	// fence must pass before a request that may change something is served (see
	// WithFence); nil outside HA mode.
	fence          models.Fence
	standbyMetrics http.Handler

	// oidc talks to the single sign-on provider, oidcBox seals the flow cookie and
	// usedStates remembers consumed states (see WithOIDC); nil in the desktop app.
	oidc       *oidc.Client
	oidcBox    *secretbox.Ref
	usedStates *stateSet

	// rateBuckets holds the REST API request budgets per API key, session and
	// client address, rateLog limits their log lines and rateObserve counts the
	// refusals (see ratelimit.go).
	rateBuckets *ratelimit.Limiter
	rateLog     *ratelimit.Gate
	rateObserve func(caller, budget string)
	// proxyWarning logs the untrusted forwarding headers once (warnUntrustedProxy).
	proxyWarning sync.Once
	// accessLog logs one line per request; requestObserver counts them (observe.go).
	accessLog       bool
	requestObserver func(route, method string, code int, d time.Duration)
	// untrustedPeerWarning logs a header-sending peer outside trusted_proxies once.
	untrustedPeerWarning sync.Once
}

// WithDesktopCSP makes the server send desktopContentSecurityPolicy, which also
// allows the origins of the desktop app's webview (wails: on macOS and Linux,
// http(s)://wails.localhost on Windows). WebKit may treat the custom wails:// origin
// as opaque, so 'self' alone would block the dashboard there. Servers reachable over
// the network keep the strict policy.
func WithDesktopCSP() Option {
	return func(s *Server) { s.desktopCSP = true }
}

// WithVersion sets the build version reported by GET /api/v1/health.
func WithVersion(v string) Option {
	return func(s *Server) { s.version = v }
}

// WithRunManager sets the Manager that owns operations started through the API. The
// application shuts it down (cancelling and awaiting in-flight runs) on exit. Without
// it the Server creates a private Manager.
func WithRunManager(m *runs.Manager) Option {
	return func(s *Server) { s.runs = m }
}

// WithOperations sets the operations service the backup, job and restore handlers
// delegate to, so that the REST API and other adapters share one instance. Without it
// the Server builds one from its own dependencies.
func WithOperations(ops *operations.Service) Option {
	return func(s *Server) { s.ops = ops }
}

// HTTP server timeouts.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	// idleTimeout is how long an idle keep-alive connection stays open. It must stay
	// above the idle timeout of every proxy in front (ALB 60 s, nginx keepalive_timeout
	// 60 s by default), or the proxy reuses a connection the server just closed and
	// answers 502.
	idleTimeout = 120 * time.Second
)

// WithJobDeletedHook registers fn to be called after a job has been deleted, e.g. to
// drop the job's metric series. It applies to the operations service the Server
// builds itself; with WithOperations, set operations.Config.OnJobDeleted instead.
func WithJobDeletedHook(fn func(jobID string)) Option {
	return func(s *Server) { s.onJobDeleted = fn }
}

// Option customises a Server.
type Option func(*Server)

// WithEventPublisher sets the port used to emit backup/restore events for manual
// operations triggered through the API.
func WithEventPublisher(p events.Publisher) Option {
	return func(s *Server) { s.publisher = p }
}

// WithNotifications enables the /api/v1/notifications endpoints backed by svc.
func WithNotifications(svc *notify.Service) Option {
	return func(s *Server) { s.notifications = svc }
}

// WithMetricsHandler serves h on GET /metrics. The endpoint requires an API key unless
// the security.metrics_public setting is on.
func WithMetricsHandler(h http.Handler) Option {
	return func(s *Server) { s.metricsHandler = h }
}

// NewServer initializes an HTTP Server with all delivery dependencies.
func NewServer(
	cfg *config.Config,
	metaStore operations.Store,
	backupEngine *backup.Engine,
	restoreEngine *restore.Engine,
	storageDriver storage.Storage,
	sched *scheduler.Scheduler,
	staticFS fs.FS,
	logger *slog.Logger,
	opts ...Option,
) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		cfg:           cfg,
		metaStore:     metaStore,
		backupEngine:  backupEngine,
		restoreEngine: restoreEngine,
		storageDriver: storageDriver,
		scheduler:     sched,
		logger:        logger,
		staticFS:      staticFS,

		heartbeatThrottle: auth.NewThrottle(nil),
		rateBuckets:       ratelimit.New(),
		rateLog:           ratelimit.NewGate(rateLimitLogInterval, nil),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.runs == nil {
		s.runs = runs.NewManager(logger)
	}
	if s.ops == nil {
		s.ops = operations.New(s.operationsConfig())
		// The two-person rule holds back admin grants of the auth service too.
		if s.auth != nil {
			s.auth.SetAdminGrantGate(s.ops)
		}
	}
	if s.readiness == nil {
		s.readiness = s.defaultReadiness()
	}

	mux := s.buildRoutes()

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	csp := contentSecurityPolicy
	if s.desktopCSP {
		csp = desktopContentSecurityPolicy
	}
	handler := s.observeMiddleware(s.loggingMiddleware(s.securityHeadersMiddleware(csp, s.corsMiddleware(s.standbyGate(s.fenceGate(s.authMiddleware(mux)))))))
	s.httpServer = NewHTTPServer(addr, handler)

	return s
}

// NewHTTPServer returns an HTTP server for addr serving h with the timeouts of the
// API server. An active-passive HA member serves its listener with it across the
// Apps of its roles.
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// Handler returns the fully composed HTTP handler (logging, CORS, authentication and
// routing middleware). It is the same handler served by Start and is exposed so the
// complete request chain can be exercised in tests or mounted by an embedding server.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// Start begins listening for incoming HTTP requests.
func (s *Server) Start() error {
	s.logger.Info("starting mongorescue http web dashboard & api",
		slog.String("addr", s.httpServer.Addr),
	)

	if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server failed: %w", err)
	}

	return nil
}

// Shutdown gracefully stops the HTTP server within the provided context.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down http server")
	return s.httpServer.Shutdown(ctx)
}

// Close closes the listener and every connection at once, including long-lived
// ones (event streams) that Shutdown would wait for.
func (s *Server) Close() error {
	return s.httpServer.Close()
}

// buildRoutes registers all API endpoints using Go 1.22 ServeMux method routing. The
// returned mux is also the one the auth middleware consults to find the matched route
// (and so its required scope); the registered patterns are recorded in s.patterns.
func (s *Server) buildRoutes() *http.ServeMux {
	mux := &router{mux: http.NewServeMux()}
	s.mux = mux.mux
	defer func() { s.patterns = mux.patterns }()

	// Setup, sessions, users and API keys
	s.registerAuthRoutes(mux)

	// Single sign-on: the sign-in methods, the provider test and the OIDC flow
	s.registerOIDCRoutes(mux)

	// Managed MongoDB connections
	s.registerConnectionRoutes(mux)
	s.registerVaultRoutes(mux)

	// Settings and storage targets
	s.registerSettingsRoutes(mux)
	s.registerMonitoringRoutes(mux)
	s.registerStorageTargetRoutes(mux)

	// API System & Stats
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET "+readyPath, s.handleReady)
	mux.HandleFunc("GET "+openAPIPath, s.handleOpenAPI)
	mux.HandleFunc("GET /api/v1/stats", s.handleStats)

	// API Jobs (Cron & Retention)
	mux.HandleFunc("GET /api/v1/jobs", s.handleListJobs)
	mux.HandleFunc("POST /api/v1/jobs", s.handleSaveJob)
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.handleGetJob)
	mux.HandleFunc("PUT /api/v1/jobs/{id}", s.handleUpdateJob)
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", s.handleDeleteJob)
	mux.HandleFunc("POST /api/v1/jobs/{id}/run", s.handleTriggerJob)
	s.registerJobDatabaseRoutes(mux)

	// API Backups
	mux.HandleFunc("GET /api/v1/backups", s.handleListBackups)
	mux.HandleFunc("GET /api/v1/backups/databases", s.handleBackupDatabases)
	mux.HandleFunc("POST /api/v1/backups", s.handleCreateBackup)
	mux.HandleFunc("DELETE /api/v1/backups/{id}", s.handleDeleteBackup)
	mux.HandleFunc("POST /api/v1/backups/{id}/retry", s.handleRetryBackup)
	mux.HandleFunc("GET /api/v1/backups/{id}/collections", s.handleBackupCollections)

	// API Disaster Recovery / Restores
	mux.HandleFunc("GET /api/v1/restores", s.handleListRestores)
	mux.HandleFunc("GET /api/v1/restores/databases", s.handleRestoreDatabases)
	mux.HandleFunc("POST /api/v1/restore", s.handleRunRestore)
	mux.HandleFunc("POST /api/v1/restores/preflight", s.handleRestorePreflight)

	// Run control: cancel, logs and live progress
	s.registerRunRoutes(mux)

	// Verification, pins, retention previews and logs, restore tests, storage scans
	s.registerIntegrityRoutes(mux)
	s.registerHistoryRoutes(mux)

	// Metadata backups and the recovery kit
	s.registerRecoveryRoutes(mux)
	s.registerKeyRotationRoutes(mux)

	// Recovery readiness: RPO, RTO and evidence per database
	s.registerReadinessRoutes(mux)
	s.registerCostRoutes(mux)
	s.registerPITRRoutes(mux)

	// Bulk actions on backups, restores and jobs
	s.registerBulkRoutes(mux)

	// Delete protection: undelete, approvals and pending changes
	s.registerProtectionRoutes(mux)

	// API Notifications (channels & rule workflows)
	s.registerNotificationRoutes(mux)

	// MCP endpoint and the audit log of API/MCP activity
	s.registerMCPRoutes(mux)

	// The audit log of every action: list, export and chain verification
	s.registerAuditLogRoutes(mux)

	// Prometheus metrics
	if s.metricsHandler != nil {
		mux.Handle("GET /metrics", s.unlimitedOnly(s.metricsHandler))
	}

	// Unknown API paths and methods answer the JSON error envelope (with its code),
	// not the plain text of the mux. They are not recorded in s.patterns.
	for _, m := range apiFallbackMethods {
		mux.mux.Handle(m+" "+apiFallbackPath, http.HandlerFunc(s.handleAPIFallback))
	}

	// Embedded Static Frontend
	if s.staticFS != nil {
		fileServer := http.FileServer(http.FS(s.staticFS))
		mux.Handle("GET /", fileServer)
	}

	return mux.mux
}

// JSON API Response Envelope
type apiResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
	// Code is a stable, machine-readable reason of some refusals (such as
	// apierr.APIKeyExpired or apierr.RateLimited); clients match it instead of the English error text.
	Code string `json:"code,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiResponse{
		Success: status < 400,
		Data:    data,
	})
}

// writeError writes an error response with the default code of its status.
func writeError(w http.ResponseWriter, status int, message string) {
	writeEnvelopeError(w, status, "", message, nil)
}

// writeErrorCode writes an error response with a machine-readable code (apierr).
func writeErrorCode(w http.ResponseWriter, status int, code, message string) {
	writeEnvelopeError(w, status, code, message, nil)
}

// writeErrorData writes an error response that also carries data (e.g. the checks of
// a preflight that refused a restore), with the default code of its status.
func writeErrorData(w http.ResponseWriter, status int, message string, data any) {
	writeEnvelopeError(w, status, "", message, data)
}

// writeEnvelopeError is the one writer of error responses: an empty code becomes the
// default of the status (apierr.ForStatus), so every error carries a code.
func writeEnvelopeError(w http.ResponseWriter, status int, code, message string, data any) {
	if code == "" {
		code = apierr.ForStatus(status)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiResponse{Success: false, Data: data, Error: message, Code: code})
}

// The catch-all of the API: apiFallbackPath for each of apiFallbackMethods.
const apiFallbackPath = "/api/"

var apiFallbackMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions}

// isAPIFallback reports whether a ServeMux pattern is one of the catch-alls.
func isAPIFallback(pattern string) bool {
	return strings.HasSuffix(pattern, " "+apiFallbackPath)
}

// handleAPIFallback answers a request that matches no API route: 405 with Allow when
// another method serves the path, otherwise 404.
func (s *Server) handleAPIFallback(w http.ResponseWriter, r *http.Request) {
	var allowed []string
	for _, m := range apiFallbackMethods {
		probe := r.Clone(r.Context())
		probe.Method = m
		if _, pattern := s.mux.Handler(probe); pattern != "" && !isAPIFallback(pattern) {
			allowed = append(allowed, m)
		}
	}
	if len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeError(w, http.StatusNotFound, "not found")
}

// Handlers

// handleStats serves the dashboard KPIs. Administrators also get the stored rows that
// cannot be read (corrupt_records), for the dashboard's warning banner.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st := s.ops.Stats(r.Context())
	if auth.PrincipalFrom(r.Context()).Allows(auth.ScopeAdmin) {
		bad, err := s.ops.CorruptRecords(r.Context())
		if err != nil {
			reason := redact.Text(err.Error())
			if st.Degraded {
				reason = st.DegradedReason + "; " + reason
			}
			st.Degraded, st.DegradedReason = true, reason
			s.logger.WarnContext(r.Context(), "could not check stored records for damage", slog.String("error", redact.Text(err.Error())))
		}
		st.CorruptRecords = bad
	}
	writeJSON(w, http.StatusOK, st)
}

// handleListJobs lists one page of the jobs in the order of sort (name, next_run,
// last_status or created_at; name by default) and order (asc or desc), ties by ID:
// limit jobs (100 by default, at most 500) after the cursor after, with the total number of matches
// and the cursor of the next page in meta. The optional filters q, enabled,
// connection_id, database, schedule and last_status (see jobListFilter) keep only
// the matching jobs, with the matcher of the bulk job actions.
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, _, err := jobListFilter(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req := operations.JobListRequest{Filter: f, After: q.Get("after"), Sort: q.Get("sort")}
	switch q.Get("order") {
	case "", "asc":
	case "desc":
		req.Desc = true
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%v: order must be \"asc\" or \"desc\"", errBadParam))
		return
	}
	if v := q.Get("limit"); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 1 || n > operations.MaxJobPageSize {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%v: limit must be an integer between 1 and %d", errBadParam, operations.MaxJobPageSize))
			return
		}
		req.Limit = n
	}
	page, err := s.ops.ListJobsPage(r.Context(), req)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeCursorList(w, cursorMeta{Total: page.Total, Limit: page.Limit, Next: page.Next}, page.Items)
}

// handleSaveJob creates a job (POST /api/v1/jobs). A job id that is taken answers 409
// (conflict); an existing job is edited with PUT /api/v1/jobs/{id}.
func (s *Server) handleSaveJob(w http.ResponseWriter, r *http.Request) {
	var req operations.JobCreate
	if !decodeJSONLimit(w, r, &req, maxAPIBody) {
		return
	}
	res, err := s.ops.CreateJob(r.Context(), req)
	if err != nil {
		s.writeJobError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, operations.JobSaveResult{Job: res.Redacted(), JobProtection: res.JobProtection, Warnings: res.Warnings})
}

// handleGetJob returns a job with its next activations.
func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	details, err := s.ops.GetJobDetails(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, details)
}

// handleUpdateJob replaces the editable fields of a job, validated like a new job, and
// reschedules it at once. The id, creation time and run history are kept.
func (s *Server) handleUpdateJob(w http.ResponseWriter, r *http.Request) {
	var req operations.JobUpdate
	if !decodeJSONLimit(w, r, &req, maxAPIBody) {
		return
	}
	res, err := s.ops.UpdateJob(r.Context(), r.PathValue("id"), req)
	if err != nil {
		s.writeJobError(w, err)
		return
	}
	// The redacted job with what the edit deferred (a shorter retention).
	writeJSON(w, http.StatusOK, operations.JobSaveResult{Job: res.Redacted(), JobProtection: res.JobProtection, Warnings: res.Warnings})
}

// writeJobError maps job validation and save errors to HTTP responses: storage target
// and connection lookup failures keep the statuses of their own endpoints; anything
// else (such as a store failure) is logged and answered with a generic 500.
func (s *Server) writeJobError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, operations.ErrJobChanged):
		writeErrorCode(w, http.StatusConflict, apierr.JobChanged, err.Error())
	case errors.Is(err, targets.ErrNotFound), errors.Is(err, targets.ErrNoDefault), errors.Is(err, targets.ErrInvalid):
		s.writeTargetError(w, err)
	case errors.Is(err, connections.ErrInvalid), errors.Is(err, connections.ErrMaskedURI),
		errors.Is(err, mongouri.ErrInvalidMongoURI), errors.Is(err, connections.ErrUnavailable):
		s.writeConnectionError(w, err)
	default:
		// Validation, not-found and connection/target sentinels map to 4xx; the rest to 500.
		s.writeOperationError(w, err)
	}
}

// handleDeleteJob deletes a job (its backups are kept).
func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "job id required")
		return
	}
	if err := s.ops.DeleteJob(r.Context(), id); err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted_id": id})
}

func (s *Server) handleTriggerJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "job id required")
		return
	}
	started, err := s.ops.RunJob(r.Context(), id, models.TriggerOnDemand)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	// The backup of a single-database job, or the run of a multi-database job.
	writeJSON(w, http.StatusAccepted, started.Body())
}

func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	var req operations.BackupRequest
	if !decodeJSONLimit(w, r, &req, maxAPIBody) {
		return
	}
	// databases backs up several databases in one run, answered with the run.
	if req.Databases != nil {
		run, err := s.ops.StartBackups(r.Context(), req)
		if err != nil {
			s.writeOperationError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, run)
		return
	}
	record, err := s.ops.StartBackup(r.Context(), req)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, record)
}

// handleRetryBackup starts a new manual backup with the parameters of a failed one.
// The failed record is kept as it is; the new record's retry_of names it.
func (s *Server) handleRetryBackup(w http.ResponseWriter, r *http.Request) {
	record, err := s.ops.RetryBackup(r.Context(), r.PathValue("id"), models.TriggerManual)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, record)
}

// handleBackupCollections lists the collections stored in a backup's archive (read
// from the archive prelude only) for a selective restore.
func (s *Server) handleBackupCollections(w http.ResponseWriter, r *http.Request) {
	// Reading the prelude may take up to operations.ArchivePreviewTimeout; the response
	// must still be written after it, past the server's default write timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(operations.ArchivePreviewTimeout + writeTimeout))
	list, err := s.ops.ListBackupCollections(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// writeOperationError maps operations errors to HTTP responses. Messages of expected
// failures are shown as they are (they never carry credentials); anything else is
// logged and answered with a generic 500.
func (s *Server) writeOperationError(w http.ResponseWriter, err error) {
	// A refusing preflight answers 409 with its checks, so clients can show them.
	var preflight *operations.PreflightError
	if errors.As(err, &preflight) {
		writeEnvelopeError(w, http.StatusConflict, apierr.PreflightFailed, err.Error(), preflight.Result)
		return
	}
	// A run refused for space (503) may be retried once the guard looks again.
	if errors.Is(err, diskguard.ErrLowSpace) {
		w.Header().Set("Retry-After", strconv.Itoa(int(diskguard.DefaultPollInterval.Seconds())))
	}
	// A destructive action that waits for a second administrator answers 202.
	if writeApprovalPending(w, err) {
		return
	}
	switch {
	// Retry errors come first: they also wrap the connection and target sentinels.
	case errors.Is(err, models.ErrShardedUnsupported):
		writeErrorCode(w, http.StatusConflict, apierr.ShardedUnsupported, err.Error())
	case errors.Is(err, operations.ErrPinned):
		writeErrorCode(w, http.StatusConflict, apierr.Protected, err.Error())
	case errors.Is(err, operations.ErrBackupRunning), errors.Is(err, operations.ErrBusy):
		writeErrorCode(w, http.StatusConflict, apierr.Busy, err.Error())
	case errors.Is(err, operations.ErrNotRetryable), errors.Is(err, operations.ErrBulkConfirm),
		errors.Is(err, operations.ErrAlreadyDeleted), errors.Is(err, operations.ErrNotDeleted), errors.Is(err, operations.ErrBackupDeleted),
		errors.Is(err, operations.ErrApprovalClosed), errors.Is(err, operations.ErrTooFewAdmins),
		errors.Is(err, operations.ErrInPlaceHAMode), errors.Is(err, operations.ErrNotRunning):
		writeErrorCode(w, http.StatusConflict, apierr.Conflict, err.Error())
	case errors.Is(err, operations.ErrApprovalNeedsSession), errors.Is(err, operations.ErrSelfApproval),
		errors.Is(err, auth.ErrApproverTooRecent), errors.Is(err, operations.ErrRequesterUnknown):
		writeErrorCode(w, http.StatusForbidden, apierr.ApprovalRefused, err.Error())
	case errors.Is(err, operations.ErrAlreadyExists):
		writeErrorCode(w, http.StatusConflict, apierr.Conflict, err.Error())
	case errors.Is(err, operations.ErrJobRecreated), errors.Is(err, operations.ErrJobChanged):
		writeErrorCode(w, http.StatusConflict, apierr.JobChanged, err.Error())
	case errors.Is(err, operations.ErrUnavailable), errors.Is(err, operations.ErrPITRUnavailable):
		writeErrorCode(w, http.StatusServiceUnavailable, apierr.Unavailable, err.Error())
	case errors.Is(err, settings.ErrInvalid), errors.Is(err, settings.ErrMaskedSecret), errors.Is(err, settings.ErrSecretReentry):
		writeErrorCode(w, http.StatusBadRequest, apierr.InvalidRequest, err.Error())
	case errors.Is(err, targets.ErrNotFound), errors.Is(err, targets.ErrInUse), errors.Is(err, targets.ErrIsDefault):
		s.writeTargetError(w, err)
	case errors.Is(err, operations.ErrBulkTooLarge), errors.Is(err, operations.ErrRetryUnavailable),
		errors.Is(err, operations.ErrPITRNotRestorable), errors.Is(err, operations.ErrKeyRequired):
		writeErrorCode(w, http.StatusUnprocessableEntity, apierr.ValidationFailed, err.Error())
	case errors.Is(err, operations.ErrInvalid), errors.Is(err, operations.ErrConnectionRequired),
		errors.Is(err, operations.ErrUnknownConnection), errors.Is(err, operations.ErrUnknownStorageTarget):
		writeErrorCode(w, http.StatusBadRequest, apierr.InvalidRequest, err.Error())
	case errors.Is(err, operations.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, apierr.NotFound, err.Error())
	case errors.Is(err, operations.ErrNotActive):
		s.writeFenced(w)
	case errors.Is(err, operations.ErrShuttingDown), errors.Is(err, operations.ErrSchedulerUnavailable):
		writeErrorCode(w, http.StatusServiceUnavailable, apierr.Unavailable, err.Error())
	case errors.Is(err, operations.ErrDatabaseListing), errors.Is(err, operations.ErrLegalHold):
		writeErrorCode(w, http.StatusBadGateway, apierr.BadGateway, redact.Text(err.Error()))
	case errors.Is(err, auth.ErrForbidden):
		writeErrorCode(w, http.StatusForbidden, apierr.Forbidden, "forbidden: "+err.Error())
	default:
		s.logger.Error("operation failed", logsafe.Error(err), requestIDAttr(w))
		writeErrorCode(w, http.StatusInternalServerError, apierr.InternalError, "internal error")
	}
}

// handleDeleteBackup deletes a backup softly, with an optional ?reason=: the archive
// stays until the grace period ends (the answer's purge_after) and the deletion can
// be undone until then; see operations.DeleteBackup. With the two-person rule it
// answers 202 with the approval request.
func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "backup id required")
		return
	}
	res, err := s.ops.DeleteBackup(r.Context(), id, r.URL.Query().Get("reason"))
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRunRestore(w http.ResponseWriter, r *http.Request) {
	var req models.RestoreRequest
	if !decodeJSONLimit(w, r, &req, maxAPIBody) {
		return
	}
	// The restore runs under the application lifecycle; clients poll GET /api/v1/restores.
	record, err := s.ops.StartRestore(r.Context(), req)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, record)
}

// handleRestorePreflight runs the checks of a restore request (the body of POST
// /api/v1/restore) without starting anything and answers the go/no-go summary.
func (s *Server) handleRestorePreflight(w http.ResponseWriter, r *http.Request) {
	var req models.RestoreRequest
	if !decodeJSONLimit(w, r, &req, maxAPIBody) {
		return
	}
	res, err := s.ops.PreflightRestore(r.Context(), req)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// currentSettings returns the live settings, or the defaults without a settings service.
func (s *Server) currentSettings() settings.Settings {
	if s.settings == nil {
		return settings.Defaults()
	}
	return s.settings.Current()
}

// security returns the live security settings.
func (s *Server) security() settings.Security {
	return s.currentSettings().Security
}

// derefOr returns *p, or def when p is nil.
func derefOr[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

// operationsConfig wires the operations service from the server's dependencies. Nil
// optional dependencies stay nil interfaces.
func (s *Server) operationsConfig() operations.Config {
	cfg := operations.Config{
		Store:     s.metaStore,
		Backup:    s.backupEngine,
		Restore:   s.restoreEngine,
		Runs:      s.runs,
		Registry:  s.registry,
		Publisher: s.publisher,
		Settings:  s.currentSettings,
		Logger:    s.logger,
		Version:   s.version,
		Fence:     s.fence,

		Storage:      s.storageFor,
		OnJobDeleted: s.onJobDeleted,
	}
	if s.audit != nil {
		cfg.Audit = s.audit
	}
	if s.scheduler != nil {
		cfg.Jobs = s.scheduler
		cfg.Scheduler = s.scheduler
	}
	if s.connections != nil {
		cfg.Connections = s.connections
	}
	if s.targets != nil {
		cfg.Targets = s.targets
	}
	if s.integrity != nil {
		cfg.Verifier = s.integrity
	}
	if s.settings != nil {
		cfg.SettingsUpdater = s.settings
	}
	if s.auth != nil {
		cfg.SecondApproverCheck = s.auth.CheckSecondApproverPossible
		cfg.Users = s.auth
	}
	if r, ok := s.keyRotation.(operations.SecretKeyRotator); ok {
		cfg.KeyRotator = r
	}
	return cfg
}

// loggingMiddleware logs HTTP request details.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.logger.DebugContext(r.Context(), "http request completed",
			slog.String("method", r.Method),
			logsafe.Attr("path", r.URL.Path),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

// contentSecurityPolicy is the Content-Security-Policy of every response. The
// dashboard loads only its own scripts, styles and images (no inline code, no
// third-party origins), talks only to its own API and may not be framed.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// desktopOrigins are the page origins of the desktop app's webview.
const desktopOrigins = "wails: http://wails.localhost https://wails.localhost"

// desktopContentSecurityPolicy is contentSecurityPolicy with desktopOrigins added to
// the fetch directives the dashboard uses (see WithDesktopCSP).
const desktopContentSecurityPolicy = "default-src 'self' " + desktopOrigins + "; script-src 'self' " + desktopOrigins +
	"; style-src 'self' " + desktopOrigins + "; img-src 'self' data: " + desktopOrigins + "; connect-src 'self' " + desktopOrigins +
	"; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// securityHeadersMiddleware sets the browser security headers on every response: the
// Content-Security-Policy csp, X-Content-Type-Options: nosniff, X-Frame-Options: DENY
// and Referrer-Policy: no-referrer. API, MCP and metrics responses carry
// Cache-Control: no-store, so neither browsers nor proxies keep copies of them.
// Strict-Transport-Security (security.hsts_max_age, 0 turns it off) is sent only when
// the response is known to travel over TLS (tlsRequest), never on plain HTTP.
func (s *Server) securityHeadersMiddleware(csp string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if age := s.security().HSTSMaxAge; age > 0 && s.tlsRequest(r) {
			h.Set("Strict-Transport-Security", "max-age="+strconv.Itoa(age))
		}
		if p := r.URL.Path; strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, oidcFlowCookiePath) || p == MCPPath || p == "/metrics" {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware applies an explicit, default-off CORS policy.
//
// When the security.cors_origins setting is empty no CORS headers are emitted and OPTIONS requests
// are passed through untouched, so browsers enforce the same-origin policy. When origins
// are configured, only a request whose Origin header exactly matches an allow-list entry
// has that origin reflected (never "*"), and its preflight is answered with 204.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := s.security().CORSOrigins
		if len(allowed) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		// The response depends on Origin whenever a policy is configured.
		w.Header().Add("Vary", "Origin")

		origin := r.Header.Get("Origin")
		if origin == "" || !slices.Contains(allowed, origin) {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, "+CSRFHeader+", Mcp-Protocol-Version, Mcp-Session-Id")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

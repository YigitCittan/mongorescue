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
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/auth/oidc"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/heartbeat"
	"github.com/yigitcittan/mongorescue/internal/integrity"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongouri"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/recoverykit"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/targets"
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

// jobIDOverhead is the fixed length of the generated "job_<db>_<unix>_<random>"
// wrapper (prefix, separators, a 10-digit Unix timestamp and the random suffix).
const jobIDOverhead = len("job___") + 10 + models.IDSuffixLength

// Server coordinates HTTP API routing and dashboard delivery.
type Server struct {
	cfg           *config.Config
	metaStore     store.Store
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

	// settings holds the dashboard-managed configuration and targets the storage
	// targets; without them, defaults and the fixed storageDriver apply.
	settings *settings.Service
	targets  *targets.Service

	// integrity verifies archives, runs restore tests and scans storage targets.
	integrity *integrity.Service

	// metaBackup snapshots the metadata database; recoveryKit builds recovery kits.
	metaBackup  *metabackup.Service
	recoveryKit *recoverykit.Service

	// readiness reports the RPO, RTO and evidence of every database a job backs up.
	readiness *readiness.Service

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

	// oidc talks to the single sign-on provider, oidcBox seals the flow cookie and
	// usedStates remembers consumed states (see WithOIDC); nil in the desktop app.
	oidc       *oidc.Client
	oidcBox    *secretbox.Box
	usedStates *stateSet
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
	metaStore store.Store,
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
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.runs == nil {
		s.runs = runs.NewManager(logger)
	}
	if s.ops == nil {
		s.ops = operations.New(s.operationsConfig())
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
	handler := s.loggingMiddleware(securityHeadersMiddleware(csp, s.corsMiddleware(s.authMiddleware(mux))))
	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
	}

	return s
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

	// Settings and storage targets
	s.registerSettingsRoutes(mux)
	s.registerMonitoringRoutes(mux)
	s.registerStorageTargetRoutes(mux)

	// API System & Stats
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
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

	// Recovery readiness: RPO, RTO and evidence per database
	s.registerReadinessRoutes(mux)

	// Bulk actions on backups, restores and jobs
	s.registerBulkRoutes(mux)

	// API Notifications (channels & rule workflows)
	s.registerNotificationRoutes(mux)

	// MCP endpoint and the audit log of API/MCP activity
	s.registerMCPRoutes(mux)

	// The audit log of every action: list, export and chain verification
	s.registerAuditLogRoutes(mux)

	// Prometheus metrics
	if s.metricsHandler != nil {
		mux.Handle("GET /metrics", s.metricsHandler)
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
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiResponse{
		Success: status < 400,
		Data:    data,
	})
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeErrorData(w, status, message, nil)
}

// writeErrorData writes an error response that also carries data (e.g. the checks of
// a preflight that refused a restore).
func writeErrorData(w http.ResponseWriter, status int, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiResponse{
		Success: false,
		Data:    data,
		Error:   message,
	})
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
			s.logger.Warn("could not check stored records for damage", slog.String("error", redact.Text(err.Error())))
		}
		st.CorruptRecords = bad
	}
	writeJSON(w, http.StatusOK, st)
}

// handleListJobs lists the jobs, sorted by name. The optional filters q, enabled,
// connection_id, database, schedule and last_status (see jobListFilter) keep only the
// matching jobs, with the matcher of the bulk job actions.
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	f, filtered, err := jobListFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var jobs []*models.Job
	if filtered {
		jobs, err = s.ops.FilterJobs(r.Context(), f)
	} else {
		jobs, err = s.ops.ListJobs(r.Context())
	}
	if err != nil {
		s.writeOperationError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, jobs)
}

// jobRequest is the body of POST /api/v1/jobs. Omitted retention and gzip fields take
// the defaults from the general settings.
type jobRequest struct {
	models.Job
	RetentionDays  *int  `json:"retention_days"`
	RetentionCount *int  `json:"retention_count"`
	Gzip           *bool `json:"gzip"`
}

func (s *Server) handleSaveJob(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request json")
		return
	}
	job := req.Job
	general := s.currentSettings().General
	job.RetentionDays = derefOr(req.RetentionDays, general.DefaultRetentionDays)
	job.RetentionCount = derefOr(req.RetentionCount, general.DefaultRetentionCount)
	job.Gzip = derefOr(req.Gzip, general.DefaultGzip)

	// Existing records are updated regardless of ID format (legacy IDs may contain raw
	// database-name characters); only new IDs must satisfy the strict pattern.
	var existing *models.Job
	if job.ID == "" {
		suffix, err := models.NewIDSuffix()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		idName := job.Database
		if sel := job.DatabaseSelection; idName == "" && sel.Multi() {
			// A multi-database job is named after its mode ("job_all_…", "job_pattern_…").
			idName = string(sel.Mode)
		}
		cleanName := models.SanitizeIDComponent(strings.ToLower(idName), models.MaxIDLength-jobIDOverhead)
		job.ID = fmt.Sprintf("job_%s_%d_%s", cleanName, time.Now().Unix(), suffix)
	} else {
		found, err := s.metaStore.GetJob(r.Context(), job.ID)
		switch {
		case err == nil:
			existing = found
		case !errors.Is(err, store.ErrNotFound):
			s.writeOperationError(w, err) // 500, logged
			return
		}
	}
	if existing == nil {
		if err := models.ValidateID(job.ID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// The heartbeat URL is a secret: its masked form keeps the stored URL.
	storedHeartbeat := ""
	if existing != nil {
		storedHeartbeat = existing.HeartbeatURL
	}
	heartbeatURL, hbErr := models.ResolveHeartbeatURL(job.HeartbeatURL, storedHeartbeat)
	if hbErr != nil {
		writeError(w, http.StatusBadRequest, hbErr.Error())
		return
	}
	job.HeartbeatURL = heartbeatURL

	if !job.Enabled {
		if err := s.ops.ValidatePausedUntil(job.PausedUntil); err != nil {
			s.writeJobError(w, err)
			return
		}
	}
	// Known databases are server-managed: never taken from the client.
	operations.CarryKnownDatabases(&job, existing)
	if err := s.ops.ValidateJob(r.Context(), &job); err != nil {
		s.writeJobError(w, err)
		return
	}
	// A new job is inserted and never overwrites another; an existing one is updated
	// and never recreated after a concurrent delete. Run history is owned by the
	// scheduler, not by clients: it is re-read right before the write.
	persist := func() error {
		if existing == nil {
			job.LastRestoreTest = nil // server-managed, never taken from clients
			return s.metaStore.CreateJob(r.Context(), &job)
		}
		current, err := s.metaStore.GetJob(r.Context(), job.ID)
		if err != nil {
			return err
		}
		job.LastRun, job.NextRun, job.CreatedAt = current.LastRun, current.NextRun, current.CreatedAt
		job.LastRestoreTest = current.LastRestoreTest
		// Known databases a run recorded since existing was read are kept.
		operations.RefreshKnownDatabases(&job, current)
		return s.metaStore.UpdateJob(r.Context(), &job)
	}
	var saveErr error
	if s.scheduler != nil {
		// Stores and schedules the job under the scheduler's lock.
		saveErr = s.scheduler.ApplyJobUpdate(&job, persist)
	} else {
		saveErr = persist()
	}
	switch {
	case errors.Is(saveErr, store.ErrAlreadyExists):
		writeError(w, http.StatusConflict, "a job with this id already exists")
		return
	case errors.Is(saveErr, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "job not found (deleted meanwhile)")
		return
	case saveErr != nil:
		s.writeOperationError(w, saveErr) // 500, logged
		return
	}

	writeJSON(w, http.StatusCreated, job.Redacted())
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request json")
		return
	}
	job, err := s.ops.UpdateJob(r.Context(), r.PathValue("id"), req)
	if err != nil {
		s.writeJobError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job.Redacted())
}

// writeJobError maps job validation and save errors to HTTP responses: storage target
// and connection lookup failures keep the statuses of their own endpoints; anything
// else (such as a store failure) is logged and answered with a generic 500.
func (s *Server) writeJobError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, operations.ErrJobChanged):
		writeError(w, http.StatusConflict, err.Error())
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request json")
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
		writeErrorData(w, http.StatusConflict, err.Error(), preflight.Result)
		return
	}
	switch {
	// Retry errors come first: they also wrap the connection and target sentinels.
	case errors.Is(err, operations.ErrNotRetryable), errors.Is(err, operations.ErrBulkConfirm), errors.Is(err, operations.ErrPinned):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, operations.ErrBulkTooLarge):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, operations.ErrRetryUnavailable):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, operations.ErrInvalid), errors.Is(err, operations.ErrConnectionRequired),
		errors.Is(err, operations.ErrUnknownConnection), errors.Is(err, operations.ErrUnknownStorageTarget):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, operations.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, operations.ErrBusy), errors.Is(err, operations.ErrNotRunning):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, operations.ErrShuttingDown), errors.Is(err, operations.ErrSchedulerUnavailable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, operations.ErrDatabaseListing):
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
	case errors.Is(err, operations.ErrKeyRequired):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, auth.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden: "+err.Error())
	default:
		s.logger.Error("operation failed", logsafe.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// handleDeleteBackup deletes a backup's archive (unless another record shares it) and
// its record; see operations.DeleteBackup.
func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "backup id required")
		return
	}
	res, err := s.ops.DeleteBackup(r.Context(), id)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRunRestore(w http.ResponseWriter, r *http.Request) {
	var req models.RestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request json")
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request json")
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
	return cfg
}

// loggingMiddleware logs HTTP request details.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Debug("http request completed",
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
func securityHeadersMiddleware(csp string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
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

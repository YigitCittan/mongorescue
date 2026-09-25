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
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
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

	// version is reported by the health endpoint.
	version string

	// mux is the router built by buildRoutes; the auth middleware asks it which route
	// a request matches to enforce that route's scope. patterns lists every
	// registered route pattern.
	mux      *http.ServeMux
	patterns []string

	// mcpHandler serves /mcp; audit backs GET /api/v1/audit.
	mcpHandler http.Handler
	audit      *audit.Service
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
// drop the job's metric series.
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

	mux := s.buildRoutes()

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	handler := s.loggingMiddleware(s.corsMiddleware(s.authMiddleware(mux)))
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

	// Managed MongoDB connections
	s.registerConnectionRoutes(mux)

	// Settings and storage targets
	s.registerSettingsRoutes(mux)
	s.registerStorageTargetRoutes(mux)

	// API System & Stats
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/stats", s.handleStats)

	// API Jobs (Cron & Retention)
	mux.HandleFunc("GET /api/v1/jobs", s.handleListJobs)
	mux.HandleFunc("POST /api/v1/jobs", s.handleSaveJob)
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", s.handleDeleteJob)
	mux.HandleFunc("POST /api/v1/jobs/{id}/run", s.handleTriggerJob)

	// API Backups
	mux.HandleFunc("GET /api/v1/backups", s.handleListBackups)
	mux.HandleFunc("POST /api/v1/backups", s.handleCreateBackup)
	mux.HandleFunc("DELETE /api/v1/backups/{id}", s.handleDeleteBackup)

	// API Disaster Recovery / Restores
	mux.HandleFunc("GET /api/v1/restores", s.handleListRestores)
	mux.HandleFunc("POST /api/v1/restore", s.handleRunRestore)

	// API Notifications (channels & rule workflows)
	s.registerNotificationRoutes(mux)

	// MCP endpoint and the audit log of API/MCP activity
	s.registerMCPRoutes(mux)

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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiResponse{
		Success: false,
		Error:   message,
	})
}

// Handlers

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "healthy",
		"version": s.version,
		"time":    time.Now().UTC(),
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.ops.Stats(r.Context()))
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.ops.ListJobs(r.Context())
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
		cleanName := models.SanitizeIDComponent(strings.ToLower(job.Database), models.MaxIDLength-jobIDOverhead)
		job.ID = fmt.Sprintf("job_%s_%d_%s", cleanName, time.Now().Unix(), suffix)
	} else {
		found, err := s.metaStore.GetJob(r.Context(), job.ID)
		switch {
		case err == nil:
			existing = found
		case !errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if existing == nil {
		if err := models.ValidateID(job.ID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if _, err := s.ops.ResolveConnection(r.Context(), job.ConnectionID); err != nil {
		s.writeResolveError(w, err)
		return
	}
	target, err := s.ops.ResolveTarget(r.Context(), job.StorageTargetID)
	if err != nil {
		s.writeTargetError(w, err)
		return
	}
	job.StorageTargetID, job.StorageType = target.ID, target.Type
	if existing != nil {
		// Run history is owned by the scheduler, not by clients.
		job.LastRun, job.NextRun, job.CreatedAt = existing.LastRun, existing.NextRun, existing.CreatedAt
	}
	if job.CronExpression == "" {
		job.CronExpression = "@daily"
	}

	// A new job is inserted and never overwrites another; an existing one is updated
	// and never recreated after a concurrent delete.
	var saveErr error
	if existing == nil {
		saveErr = s.metaStore.CreateJob(r.Context(), &job)
	} else {
		saveErr = s.metaStore.UpdateJob(r.Context(), &job)
	}
	switch {
	case errors.Is(saveErr, store.ErrAlreadyExists):
		writeError(w, http.StatusConflict, "a job with this id already exists")
		return
	case errors.Is(saveErr, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "job not found (deleted meanwhile)")
		return
	case saveErr != nil:
		writeError(w, http.StatusInternalServerError, saveErr.Error())
		return
	}

	if s.scheduler != nil {
		if err := s.scheduler.RegisterJob(&job); err != nil {
			s.logger.Error("failed to register job with scheduler",
				slog.String("job_id", job.ID),
				slog.Any("error", err),
			)
		}
	}

	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "job id required")
		return
	}

	if s.scheduler != nil {
		s.scheduler.UnregisterJob(id)
	}

	if err := s.metaStore.DeleteJob(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.onJobDeleted != nil {
		s.onJobDeleted(id)
	}

	writeJSON(w, http.StatusOK, map[string]string{"deleted_id": id})
}

func (s *Server) handleTriggerJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "job id required")
		return
	}
	record, err := s.ops.RunJob(r.Context(), id, models.TriggerOnDemand)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, record)
}

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	backups, err := s.ops.ListBackups(r.Context(), operations.BackupFilter{Database: r.URL.Query().Get("database")})
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backups)
}

func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	var req operations.BackupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request json")
		return
	}
	record, err := s.ops.StartBackup(r.Context(), req)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, record)
}

// writeOperationError maps operations errors to HTTP responses. Messages of expected
// failures are shown as they are (they never carry credentials); anything else is
// logged and answered with a generic 500.
func (s *Server) writeOperationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, operations.ErrInvalid), errors.Is(err, operations.ErrConnectionRequired),
		errors.Is(err, operations.ErrUnknownConnection), errors.Is(err, operations.ErrUnknownStorageTarget):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, operations.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, operations.ErrBusy):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, operations.ErrShuttingDown), errors.Is(err, operations.ErrSchedulerUnavailable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, operations.ErrKeyRequired):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, auth.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden: "+err.Error())
	default:
		s.logger.Error("operation failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "backup id required")
		return
	}

	rec, err := s.metaStore.GetBackupRecord(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "backup not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Delete from the storage target the backup was written to (not the current default)
	if rec.StorageKey != "" {
		driver, delErr := s.storageFor(r.Context(), rec.StorageTargetID)
		if delErr == nil {
			delErr = driver.Delete(r.Context(), rec.StorageKey)
		}
		if delErr != nil && !errors.Is(delErr, storage.ErrNotFound) {
			s.logger.Error("failed to delete backup artifact from storage",
				slog.String("storage_key", rec.StorageKey),
				slog.String("storage_target_id", rec.StorageTargetID),
				slog.Any("error", delErr),
			)
		}
	}

	// Delete from metadata store
	if err := s.metaStore.DeleteBackupRecord(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"deleted_id": id})
}

func (s *Server) handleListRestores(w http.ResponseWriter, r *http.Request) {
	restores, err := s.ops.ListRestores(r.Context())
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, restores)
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

// writeResolveError maps connection resolution errors to HTTP responses.
func (s *Server) writeResolveError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrConnectionRequired) || errors.Is(err, ErrUnknownConnection) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeConnectionError(w, err)
}

// operationsConfig wires the operations service from the server's dependencies. Nil
// optional dependencies stay nil interfaces.
func (s *Server) operationsConfig() operations.Config {
	cfg := operations.Config{
		Store:     s.metaStore,
		Backup:    s.backupEngine,
		Restore:   s.restoreEngine,
		Runs:      s.runs,
		Publisher: s.publisher,
		Settings:  s.currentSettings,
		Logger:    s.logger,
		Version:   s.version,
	}
	if s.scheduler != nil {
		cfg.Jobs = s.scheduler
	}
	if s.connections != nil {
		cfg.Connections = s.connections
	}
	if s.targets != nil {
		cfg.Targets = s.targets
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
			slog.String("path", r.URL.Path),
			slog.Duration("duration", time.Since(start)),
		)
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

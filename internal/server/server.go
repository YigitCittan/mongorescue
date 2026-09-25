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

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/redact"
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
	// does not name a connection.
	ErrConnectionRequired = errors.New("connection_id is required")

	// ErrUnknownConnection is returned (as HTTP 400) when a request names a connection
	// that does not exist.
	ErrUnknownConnection = errors.New("unknown connection_id")
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

// persistTimeout bounds metadata writes of background runs after they finish.
const persistTimeout = 5 * time.Second

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
	ctx := r.Context()
	backups, _ := s.metaStore.ListBackupRecords(ctx, "")
	jobs, _ := s.metaStore.ListJobs(ctx)
	activeJobs := 0
	for _, j := range jobs {
		if j.Enabled {
			activeJobs++
		}
	}

	var totalBytes int64
	var completedCount int
	var failedCount int

	for _, b := range backups {
		switch b.Status {
		case models.StatusCompleted:
			completedCount++
			totalBytes += b.SizeBytes
		case models.StatusFailed:
			failedCount++
		}
	}

	stats := map[string]any{
		"total_backups":     len(backups),
		"completed_backups": completedCount,
		"failed_backups":    failedCount,
		"total_bytes":       totalBytes,
		"active_jobs":       activeJobs,
	}
	if s.targets != nil {
		if def, err := s.targets.Resolve(ctx, ""); err == nil {
			stats["storage_type"] = def.Type
			stats["default_storage_target"] = map[string]any{"id": def.ID, "name": def.Name, "type": def.Type}
		}
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.metaStore.ListJobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
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

	if _, err := s.resolveConnection(r.Context(), job.ConnectionID); err != nil {
		s.writeResolveError(w, err)
		return
	}
	target, err := s.resolveTarget(r.Context(), job.StorageTargetID)
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

	if s.scheduler == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler not initialized")
		return
	}

	// Lookup-based: any stored job (including legacy-format IDs) may be triggered.
	job, record, err := s.scheduler.PrepareJobRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The scheduler persists the final record and publishes the outcome event.
	s.startBackup(w, r, record, func(ctx context.Context) {
		_, _ = s.scheduler.ExecuteJobRun(ctx, job, record)
	})
}

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	dbFilter := r.URL.Query().Get("database")
	backups, err := s.metaStore.ListBackupRecords(r.Context(), dbFilter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, backups)
}

// backupRequest is the body of POST /api/v1/backups; an omitted gzip takes the
// general.default_gzip setting.
type backupRequest struct {
	models.BackupOptions
	Gzip *bool `json:"gzip"`
}

func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	var req backupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request json")
		return
	}
	opts := req.BackupOptions

	conn, err := s.resolveConnection(r.Context(), opts.ConnectionID)
	if err != nil {
		s.writeResolveError(w, err)
		return
	}
	opts.MongoURI, opts.ConnectionName = conn.URI, conn.Name
	target, err := s.resolveTarget(r.Context(), opts.StorageTargetID)
	if err != nil {
		s.writeTargetError(w, err)
		return
	}
	opts.StorageTargetID, opts.StorageTargetName, opts.StorageType = target.ID, target.Name, target.Type
	opts.Gzip = derefOr(req.Gzip, s.currentSettings().General.DefaultGzip)

	record, err := s.backupEngine.Prepare(opts)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	jobID := s.knownJobID(r.Context(), opts.JobID)

	s.startBackup(w, r, record, func(ctx context.Context) {
		final, runErr := s.backupEngine.Execute(ctx, opts, record)
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
		defer cancel()
		if saveErr := s.metaStore.SaveBackupRecord(persistCtx, final); saveErr != nil {
			s.logger.Error("failed to persist backup metadata record",
				slog.String("backup_id", final.ID),
				slog.Any("error", saveErr),
			)
		}
		s.publish(persistCtx, events.BackupEvent(final, runErr, jobID, opts.Database))
	})
}

// startBackup reserves the database, persists the in-progress record, runs execute in
// the background under the application lifecycle (never the request context) and
// answers 202 Accepted with a snapshot of the record. A backup of the same database
// already running yields 409 Conflict.
func (s *Server) startBackup(w http.ResponseWriter, r *http.Request, record *models.BackupRecord, execute func(ctx context.Context)) {
	release, err := s.runs.Acquire(runs.BackupKey(record.ConnectionID, record.Database))
	if err != nil {
		writeRunError(w, err, "a backup of database "+record.Database+" is already running")
		return
	}
	snapshot := *record
	if err := s.metaStore.SaveBackupRecord(r.Context(), &snapshot); err != nil {
		release()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.runs.Go("", func(ctx context.Context) {
		defer release()
		execute(ctx)
	}); err != nil {
		release()
		s.abandonBackup(r.Context(), &snapshot, err)
		writeRunError(w, err, "")
		return
	}
	writeJSON(w, http.StatusAccepted, snapshot)
}

// abandonBackup marks a persisted in-progress record failed when its run never started.
func (s *Server) abandonBackup(ctx context.Context, record *models.BackupRecord, cause error) {
	record.Status = models.StatusFailed
	record.ErrorMessage = "backup not started: " + cause.Error()
	if err := s.metaStore.SaveBackupRecord(context.WithoutCancel(ctx), record); err != nil {
		s.logger.Error("failed to persist abandoned backup record", slog.String("backup_id", record.ID), slog.Any("error", err))
	}
}

// writeRunError maps runs.Manager errors to HTTP responses (409 busy, 503 shutting down).
func writeRunError(w http.ResponseWriter, err error, busyMessage string) {
	switch {
	case errors.Is(err, runs.ErrBusy):
		writeError(w, http.StatusConflict, busyMessage)
	case errors.Is(err, runs.ErrShuttingDown):
		writeError(w, http.StatusServiceUnavailable, "server is shutting down")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
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
	restores, err := s.metaStore.ListRestoreRecords(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
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
	if req.BackupID == "" {
		writeError(w, http.StatusBadRequest, "backup_id required")
		return
	}
	if err := req.ValidateTarget(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The route needs operator; overwriting existing data in place needs admin.
	if req.InPlace() {
		if err := auth.RequireScope(r.Context(), auth.ScopeAdmin); err != nil {
			writeError(w, http.StatusForbidden, "forbidden: in-place restores need an admin API key or a session ("+scopeMessage(err)+")")
			return
		}
	}

	// Lookup-based: any stored backup (including legacy-format IDs) may be restored.
	sourceRecord, err := s.metaStore.GetBackupRecord(r.Context(), req.BackupID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "source backup not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// The target defaults to the server the backup was taken from; another connection
	// restores across servers.
	targetID := req.TargetConnectionID
	if targetID == "" {
		targetID = sourceRecord.ConnectionID
	}
	target, err := s.resolveConnection(r.Context(), targetID)
	if err != nil {
		if errors.Is(err, ErrConnectionRequired) {
			err = fmt.Errorf("%w: the backup has no connection, choose a target_connection_id", ErrConnectionRequired)
		}
		s.writeResolveError(w, err)
		return
	}
	req.TargetConnectionID, req.TargetConnectionName, req.MongoURI = target.ID, target.Name, target.URI

	// Key material is checked synchronously so the client learns about it immediately.
	if sourceRecord.Encrypted && !s.restoreEngine.CanDecrypt() {
		writeError(w, http.StatusUnprocessableEntity, encryption.ErrEncryptionKeyRequired.Error())
		return
	}

	// Request errors (e.g. models.ErrInPlaceNotConfirmed) are reported synchronously.
	record, err := s.restoreEngine.Prepare(req, sourceRecord)
	if err != nil {
		writeError(w, http.StatusBadRequest, redact.Text(err.Error()))
		return
	}
	record.SourceConnectionID, record.SourceConnectionName = sourceRecord.ConnectionID, sourceRecord.ConnectionName
	if sourceRecord.ConnectionID != "" && s.connections != nil {
		if src, getErr := s.connections.Get(r.Context(), sourceRecord.ConnectionID); getErr == nil {
			record.SourceConnectionName = src.Name
		}
	}

	release, err := s.runs.Acquire(runs.RestoreKey(record.TargetConnectionID, record.TargetDatabase))
	if err != nil {
		writeRunError(w, err, "a restore into database "+record.TargetDatabase+" is already running")
		return
	}
	snapshot := *record
	if err := s.metaStore.SaveRestoreRecord(r.Context(), &snapshot); err != nil {
		release()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// The restore runs under the application lifecycle; clients poll GET /api/v1/restores.
	if err := s.runs.Go("", func(ctx context.Context) {
		defer release()
		final, runErr := s.restoreEngine.Execute(ctx, req, sourceRecord, record)
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
		defer cancel()
		if saveErr := s.metaStore.SaveRestoreRecord(persistCtx, final); saveErr != nil {
			s.logger.Error("failed to persist restore metadata record",
				slog.String("restore_id", final.ID),
				slog.Any("error", saveErr),
			)
		}
		s.publish(persistCtx, events.RestoreEvent(final, runErr, req.BackupID))
	}); err != nil {
		release()
		snapshot.Status = models.RestoreStatusFailed
		snapshot.ErrorMessage = "restore not started: " + err.Error()
		_ = s.metaStore.SaveRestoreRecord(context.WithoutCancel(r.Context()), &snapshot)
		writeRunError(w, err, "")
		return
	}

	writeJSON(w, http.StatusAccepted, snapshot)
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

// resolveConnection returns the connection id with its full URI. It returns
// ErrConnectionRequired for an empty id and ErrUnknownConnection for a missing one.
func (s *Server) resolveConnection(ctx context.Context, id string) (*models.Connection, error) {
	if id == "" {
		return nil, ErrConnectionRequired
	}
	if s.connections == nil {
		return nil, fmt.Errorf("%w: connections are not configured", ErrUnknownConnection)
	}
	c, err := s.connections.Resolve(ctx, id)
	if errors.Is(err, connections.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownConnection, id)
	}
	return c, err
}

// writeResolveError maps resolveConnection errors to HTTP responses.
func (s *Server) writeResolveError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrConnectionRequired) || errors.Is(err, ErrUnknownConnection) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeConnectionError(w, err)
}

// publish emits e if a publisher is configured. It never blocks.
func (s *Server) publish(ctx context.Context, e events.Event) {
	if s.publisher != nil {
		s.publisher.Publish(ctx, e)
	}
}

// knownJobID returns id when it names a stored job and "" otherwise, so arbitrary
// client-supplied job IDs on manual backups cannot inflate metric label cardinality
// or spoof rule matching.
func (s *Server) knownJobID(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	if _, err := s.metaStore.GetJob(ctx, id); err != nil {
		return ""
	}
	return id
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
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, "+CSRFHeader)

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

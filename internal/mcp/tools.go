package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// Output size caps.
const (
	// DefaultPageSize is the page size of list tools when limit is omitted.
	DefaultPageSize = 20
	// MaxPageSize caps the limit of list tools.
	MaxPageSize = 100
	// maxIDLength bounds identifiers accepted from clients.
	maxIDLength = 256
	// maxNameLength bounds database and collection names accepted from clients.
	maxNameLength = 255
	// maxCollections bounds collection filters.
	maxCollections = 1000
	// maxSummaryName bounds an untrusted name quoted in a text summary, in runes.
	maxSummaryName = 64
)

// errInvalidInput marks tool input rejected before any service is called.
var errInvalidInput = errors.New("invalid input")

// toolSpec is what the middleware needs to know about a tool.
type toolSpec struct {
	// scope is the API key scope the tool requires.
	scope auth.Scope
}

// Tool names, with the scope each requires.
const (
	ToolListConnections    = "list_connections"
	ToolListDatabases      = "list_databases"
	ToolListCollections    = "list_collections"
	ToolListJobs           = "list_jobs"
	ToolGetJob             = "get_job"
	ToolListBackups        = "list_backups"
	ToolGetBackup          = "get_backup"
	ToolListRestores       = "list_restores"
	ToolGetRestore         = "get_restore"
	ToolListStorageTargets = "list_storage_targets"
	ToolGetStatus          = "get_status"
	ToolStartBackup        = "start_backup"
	ToolRunJob             = "run_job"
	ToolRestoreSafeClone   = "restore_to_safe_clone"
)

// ToolScopes maps every tool to the API key scope it requires. It is the single
// source of truth the middleware enforces and tools/list filters by.
var ToolScopes = map[string]auth.Scope{
	ToolListConnections:    auth.ScopeRead,
	ToolListDatabases:      auth.ScopeRead,
	ToolListCollections:    auth.ScopeRead,
	ToolListJobs:           auth.ScopeRead,
	ToolGetJob:             auth.ScopeRead,
	ToolListBackups:        auth.ScopeRead,
	ToolGetBackup:          auth.ScopeRead,
	ToolListRestores:       auth.ScopeRead,
	ToolGetRestore:         auth.ScopeRead,
	ToolListStorageTargets: auth.ScopeRead,
	ToolGetStatus:          auth.ScopeRead,
	ToolStartBackup:        auth.ScopeOperator,
	ToolRunJob:             auth.ScopeOperator,
	ToolRestoreSafeClone:   auth.ScopeOperator,
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// readOnly annotates a tool that only reads MongoRescue's own state.
func readOnly(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}
}

// additive annotates a tool that starts an operation which only adds data (a new
// backup artifact or a new clone database) and never overwrites or deletes any.
func additive(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: false, IdempotentHint: false, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}
}

// addTool registers a tool whose handler returns a value (the structured output)
// and a one-line summary. Summaries are prose the model reads, so they carry only
// IDs, counts, statuses and timestamps; untrusted strings (names chosen by users or
// read from MongoDB, error messages) stay in the structured content, and the few
// names a summary needs are passed through quoted. The result carries the summary and the JSON text of the
// value, so clients without structured content support see the data too. Errors
// become tool errors with a client-safe message.
func addTool[In, Out any](s *Server, t *sdk.Tool, h func(ctx context.Context, in In) (Out, string, error)) {
	scope, ok := ToolScopes[t.Name]
	if !ok {
		panic("mcp: tool " + t.Name + " has no scope")
	}
	s.tools[t.Name] = toolSpec{scope: scope}
	sdk.AddTool(s.sdk, t, func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
		out, summary, err := h(ctx, in)
		if err != nil {
			var zero Out
			return nil, zero, s.toolError(t.Name, err)
		}
		raw, err := json.Marshal(out)
		if err != nil {
			var zero Out
			return nil, zero, s.toolError(t.Name, fmt.Errorf("encode output: %w", err))
		}
		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.TextContent{Text: summary},
			&sdk.TextContent{Text: string(raw)},
		}}, out, nil
	})
}

// toolError maps a service error to a message that is safe to show to the model:
// expected failures keep their (credential-free) message, anything else is logged
// and reported as an internal error.
func (s *Server) toolError(tool string, err error) error {
	switch {
	case errors.Is(err, errInvalidInput),
		errors.Is(err, operations.ErrInvalid), errors.Is(err, operations.ErrNotFound),
		errors.Is(err, operations.ErrConnectionRequired), errors.Is(err, operations.ErrUnknownConnection),
		errors.Is(err, operations.ErrUnknownStorageTarget), errors.Is(err, operations.ErrBusy),
		errors.Is(err, operations.ErrShuttingDown), errors.Is(err, operations.ErrSchedulerUnavailable),
		errors.Is(err, operations.ErrKeyRequired), errors.Is(err, auth.ErrForbidden),
		errors.Is(err, connections.ErrInvalid), errors.Is(err, connections.ErrUnavailable):
		return errors.New(err.Error())
	case errors.Is(err, connections.ErrNotFound):
		return errors.New("connection not found")
	default:
		s.logger.Error("mcp tool failed", slog.String("tool", tool), slog.Any("error", err))
		return errors.New("internal error")
	}
}

// Inputs.

type noInput struct{}

type connectionInput struct {
	ConnectionID string `json:"connection_id" jsonschema:"ID of a managed connection (see list_connections)"`
}

type collectionsInput struct {
	ConnectionID string `json:"connection_id" jsonschema:"ID of a managed connection (see list_connections)"`
	Database     string `json:"database" jsonschema:"database name (see list_databases)"`
}

type pageInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"page size, 1-100 (default 20)"`
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page"`
}

type idInput struct {
	ID string `json:"id" jsonschema:"the record ID"`
}

type listBackupsInput struct {
	Database     string `json:"database,omitempty" jsonschema:"only backups of this database"`
	ConnectionID string `json:"connection_id,omitempty" jsonschema:"only backups taken from this connection"`
	Status       string `json:"status,omitempty" jsonschema:"only backups in this state"`
	Limit        int    `json:"limit,omitempty" jsonschema:"page size, 1-100 (default 20)"`
	Cursor       string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page"`
}

type startBackupInput struct {
	ConnectionID       string   `json:"connection_id" jsonschema:"ID of the connection to back up from (see list_connections)"`
	Database           string   `json:"database" jsonschema:"database to back up (see list_databases)"`
	StorageTargetID    string   `json:"storage_target_id,omitempty" jsonschema:"storage target to write to (default: the default target)"`
	Collections        []string `json:"collections,omitempty" jsonschema:"only these collections"`
	ExcludeCollections []string `json:"exclude_collections,omitempty" jsonschema:"skip these collections"`
	Gzip               *bool    `json:"gzip,omitempty" jsonschema:"compress the archive (default: the server setting)"`
}

type runJobInput struct {
	JobID string `json:"job_id" jsonschema:"ID of the scheduled job (see list_jobs)"`
}

type restoreInput struct {
	BackupID           string   `json:"backup_id" jsonschema:"ID of a completed backup (see list_backups)"`
	TargetConnectionID string   `json:"target_connection_id,omitempty" jsonschema:"admin API keys only: another connection to restore into (default, and the only choice for operator keys: the backup's own connection)"`
	Collections        []string `json:"collections,omitempty" jsonschema:"only restore these collections"`
	Verify             *bool    `json:"verify,omitempty" jsonschema:"verify the archive checksum (and decryption) before restoring (default: the server's verify policy)"`
}

// Outputs.

// ConnectionView is a connection as shown to assistants: no URI, only its hosts.
type ConnectionView struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Hosts         string     `json:"hosts"`
	Description   string     `json:"description,omitempty"`
	ServerVersion string     `json:"server_version,omitempty"`
	LastTestOK    bool       `json:"last_test_ok"`
	LastTestAt    *time.Time `json:"last_test_at,omitempty"`
}

type connectionList struct {
	Connections []ConnectionView `json:"connections"`
}

type databaseList struct {
	ConnectionID string                 `json:"connection_id"`
	Databases    []connections.Database `json:"databases"`
}

type collectionList struct {
	ConnectionID string                   `json:"connection_id"`
	Database     string                   `json:"database"`
	Collections  []connections.Collection `json:"collections"`
}

type jobList struct {
	Jobs       []*models.Job `json:"jobs"`
	Total      int           `json:"total"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type backupList struct {
	Backups    []*models.BackupRecord `json:"backups"`
	Total      int                    `json:"total"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

type restoreList struct {
	Restores   []*models.RestoreRecord `json:"restores"`
	Total      int                     `json:"total"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

// TargetView is a storage target as shown to assistants: no credentials.
type TargetView struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Type       models.StorageType `json:"type"`
	IsDefault  bool               `json:"is_default"`
	Location   string             `json:"location"`
	LastTestOK bool               `json:"last_test_ok"`
}

type targetList struct {
	StorageTargets []TargetView `json:"storage_targets"`
}

type backupStarted struct {
	Backup   *models.BackupRecord `json:"backup"`
	NextStep string               `json:"next_step"`
}

type restoreStarted struct {
	Restore  *models.RestoreRecord `json:"restore"`
	NextStep string                `json:"next_step"`
}

// schemaFor infers the input schema of T and lets tweak add constraints. Optional
// slice and pointer fields are inferred as ["null", <type>]; they are narrowed to
// the single type, because clients that map tool parameters onto single-type
// dialects (such as Gemini function declarations) reject type arrays, and an optional
// argument is omitted rather than sent as null.
func schemaFor[T any](tweak func(props map[string]*jsonschema.Schema)) *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("mcp: input schema of %T: %v", *new(T), err))
	}
	for _, p := range s.Properties {
		if len(p.Types) == 2 && slices.Contains(p.Types, "null") {
			p.Type = p.Types[0]
			if p.Type == "null" {
				p.Type = p.Types[1]
			}
			p.Types = nil
		}
	}
	if tweak != nil {
		tweak(s.Properties)
	}
	return s
}

// limitIDs bounds the ID-like string properties named.
func limitIDs(props map[string]*jsonschema.Schema, names ...string) {
	for _, n := range names {
		if p := props[n]; p != nil {
			p.MinLength, p.MaxLength = ptr(1), ptr(maxIDLength)
		}
	}
}

// pageProps constrains limit and cursor.
func pageProps(props map[string]*jsonschema.Schema) {
	if p := props["limit"]; p != nil {
		p.Minimum, p.Maximum = ptr(1.0), ptr(float64(MaxPageSize))
	}
	if p := props["cursor"]; p != nil {
		p.MaxLength = ptr(64)
	}
}

// collectionProps constrains collection filters.
func collectionProps(props map[string]*jsonschema.Schema, names ...string) {
	for _, n := range names {
		if p := props[n]; p != nil {
			p.MaxItems = ptr(maxCollections)
			if p.Items != nil {
				p.Items.MinLength, p.Items.MaxLength = ptr(1), ptr(maxNameLength)
			}
		}
	}
}

func (s *Server) registerTools() {
	addTool(s, &sdk.Tool{
		Name:        ToolListConnections,
		Description: "List the MongoDB servers (connections) MongoRescue backs up from and restores into. Connection strings are never shown, only hosts.",
		Annotations: readOnly("List connections"),
	}, s.listConnections)
	addTool(s, &sdk.Tool{
		Name:        ToolListDatabases,
		Description: "List the databases of a connection (admin, config and local are hidden). Connects to the MongoDB server.",
		Annotations: readOnly("List databases"),
		InputSchema: schemaFor[connectionInput](func(p map[string]*jsonschema.Schema) { limitIDs(p, "connection_id") }),
	}, s.listDatabases)
	addTool(s, &sdk.Tool{
		Name:        ToolListCollections,
		Description: "List the collections and views of a database on a connection. Connects to the MongoDB server.",
		Annotations: readOnly("List collections"),
		InputSchema: schemaFor[collectionsInput](func(p map[string]*jsonschema.Schema) {
			limitIDs(p, "connection_id")
			p["database"].MinLength, p["database"].MaxLength = ptr(1), ptr(maxNameLength)
		}),
	}, s.listCollections)
	addTool(s, &sdk.Tool{
		Name:        ToolListJobs,
		Description: "List scheduled backup jobs (cron schedule, database, retention, last and next run).",
		Annotations: readOnly("List jobs"),
		InputSchema: schemaFor[pageInput](pageProps),
	}, s.listJobs)
	addTool(s, &sdk.Tool{
		Name:        ToolGetJob,
		Description: "Get one scheduled backup job by ID.",
		Annotations: readOnly("Get job"),
		InputSchema: schemaFor[idInput](func(p map[string]*jsonschema.Schema) { limitIDs(p, "id") }),
	}, s.getJob)
	addTool(s, &sdk.Tool{
		Name:        ToolListBackups,
		Description: "List backups, newest first, optionally filtered by database, connection and status. Paginated: pass next_cursor to get the next page.",
		Annotations: readOnly("List backups"),
		InputSchema: schemaFor[listBackupsInput](func(p map[string]*jsonschema.Schema) {
			pageProps(p)
			limitIDs(p, "connection_id")
			p["database"].MaxLength = ptr(maxNameLength)
			p["status"].Enum = []any{string(models.StatusInProgress), string(models.StatusCompleted), string(models.StatusFailed), string(models.StatusPruned)}
		}),
	}, s.listBackups)
	addTool(s, &sdk.Tool{
		Name:        ToolGetBackup,
		Description: "Get one backup record by ID: status (in_progress, completed, failed), size, SHA-256, storage target, error message.",
		Annotations: readOnly("Get backup"),
		InputSchema: schemaFor[idInput](func(p map[string]*jsonschema.Schema) { limitIDs(p, "id") }),
	}, s.getBackup)
	addTool(s, &sdk.Tool{
		Name:        ToolListRestores,
		Description: "List restores, newest first. Paginated: pass next_cursor to get the next page.",
		Annotations: readOnly("List restores"),
		InputSchema: schemaFor[pageInput](pageProps),
	}, s.listRestores)
	addTool(s, &sdk.Tool{
		Name:        ToolGetRestore,
		Description: "Get one restore record by ID: status (in_progress, completed, failed), target database, verification, error message.",
		Annotations: readOnly("Get restore"),
		InputSchema: schemaFor[idInput](func(p map[string]*jsonschema.Schema) { limitIDs(p, "id") }),
	}, s.getRestore)
	addTool(s, &sdk.Tool{
		Name:        ToolListStorageTargets,
		Description: "List the storage targets backups are written to (local disk or S3-compatible). Credentials are never shown.",
		Annotations: readOnly("List storage targets"),
	}, s.listStorageTargets)
	addTool(s, &sdk.Tool{
		Name:        ToolGetStatus,
		Description: "Operational overview: health, version, counts, running operations, the last successful backup of every job and the backups that failed in the last 24 hours.",
		Annotations: readOnly("Get status"),
	}, s.getStatus)
	addTool(s, &sdk.Tool{
		Name: ToolStartBackup,
		Description: "Start a backup of a database now. Returns immediately with the new backup record (status in_progress); " +
			"poll get_backup with its id until the status is completed or failed. Only one backup of a database runs at a time.",
		Annotations: additive("Start backup"),
		InputSchema: schemaFor[startBackupInput](func(p map[string]*jsonschema.Schema) {
			limitIDs(p, "connection_id", "storage_target_id")
			p["database"].MinLength, p["database"].MaxLength = ptr(1), ptr(maxNameLength)
			collectionProps(p, "collections", "exclude_collections")
		}),
	}, s.startBackup)
	addTool(s, &sdk.Tool{
		Name: ToolRunJob,
		Description: "Run a scheduled backup job now. Returns immediately with the new backup record (status in_progress); " +
			"poll get_backup with its id until the status is completed or failed. On-demand runs never delete older backups: " +
			"the job's retention policy is applied by its scheduled runs only.",
		Annotations: additive("Run job now"),
		InputSchema: schemaFor[runJobInput](func(p map[string]*jsonschema.Schema) { limitIDs(p, "job_id") }),
	}, s.runJob)
	addTool(s, &sdk.Tool{
		Name: ToolRestoreSafeClone,
		Description: "Restore a backup into a NEW database named <db>_rescue_<timestamp> (a safe clone); existing data is never overwritten. " +
			"Returns immediately with the restore record (status in_progress); poll get_restore until completed or failed. " +
			"In-place restores are not available through MCP. Operator keys restore into the backup's own connection only; " +
			"target_connection_id (another server) needs an admin key.",
		Annotations: additive("Restore to a safe clone"),
		InputSchema: schemaFor[restoreInput](func(p map[string]*jsonschema.Schema) {
			limitIDs(p, "backup_id", "target_connection_id")
			collectionProps(p, "collections")
		}),
	}, s.restoreSafeClone)
}

// page returns the window of n items selected by limit and cursor, and the cursor
// of the next page ("" on the last page).
func page(n, limit int, cursor string) (start, end int, next string, err error) {
	if limit == 0 {
		limit = DefaultPageSize
	}
	if limit < 1 || limit > MaxPageSize {
		return 0, 0, "", fmt.Errorf("%w: limit must be between 1 and %d", errInvalidInput, MaxPageSize)
	}
	if cursor != "" {
		raw, decErr := base64.RawURLEncoding.DecodeString(cursor)
		off, convErr := strconv.Atoi(strings.TrimPrefix(string(raw), "o:"))
		if decErr != nil || convErr != nil || !strings.HasPrefix(string(raw), "o:") || off < 0 {
			return 0, 0, "", fmt.Errorf("%w: cursor is not a next_cursor of this tool", errInvalidInput)
		}
		start = off
	}
	start = min(start, n)
	end = min(start+limit, n)
	if end < n {
		next = base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(end)))
	}
	return start, end, next, nil
}

// quoted renders an untrusted name (a database, for instance) for a text summary:
// cut to maxSummaryName runes and Go-quoted, so it reads as a value and cannot add
// lines, control characters or unbalanced quotes to the prose.
func quoted(v string) string {
	if utf8.RuneCountInString(v) > maxSummaryName {
		v = string([]rune(v)[:maxSummaryName]) + "…"
	}
	return strconv.Quote(v)
}

// idText renders a record ID for a text summary: plain when it only has ID characters
// (letters, digits, '_', '-', '.'), as new IDs do, else quoted like a name (legacy
// IDs may embed raw database names).
func idText(v string) string {
	plain := v != "" && len(v) <= maxIDLength && strings.IndexFunc(v, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.'
	}) < 0
	if plain {
		return v
	}
	return quoted(v)
}

// timestamp renders an optional time for a text summary.
func timestamp(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

// requireID validates a required identifier argument.
func requireID(name, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%w: %s is required", errInvalidInput, name)
	}
	if len(v) > maxIDLength {
		return fmt.Errorf("%w: %s is too long", errInvalidInput, name)
	}
	return nil
}

func (s *Server) listConnections(ctx context.Context, _ noInput) (connectionList, string, error) {
	out := connectionList{Connections: []ConnectionView{}}
	if s.cfg.Connections == nil {
		return out, "No connections are configured.", nil
	}
	list, err := s.cfg.Connections.List(ctx)
	if err != nil {
		return out, "", err
	}
	ids := make([]string, 0, len(list))
	for _, c := range list {
		out.Connections = append(out.Connections, ConnectionView{
			ID: c.ID, Name: c.Name, Hosts: connections.HostList(c.URI), Description: c.Description,
			ServerVersion: c.ServerVersion, LastTestOK: c.LastTestOK, LastTestAt: c.LastTestAt,
		})
		ids = append(ids, idText(c.ID))
	}
	return out, fmt.Sprintf("%d connection(s): %s.", len(list), strings.Join(ids, ", ")), nil
}

func (s *Server) listDatabases(ctx context.Context, in connectionInput) (databaseList, string, error) {
	out := databaseList{ConnectionID: in.ConnectionID, Databases: []connections.Database{}}
	if err := requireID("connection_id", in.ConnectionID); err != nil {
		return out, "", err
	}
	if s.cfg.Connections == nil {
		return out, "", connections.ErrNotFound
	}
	dbs, err := s.cfg.Connections.Databases(ctx, in.ConnectionID, false)
	if err != nil {
		return out, "", err
	}
	out.Databases = dbs
	return out, fmt.Sprintf("%d database(s) on connection %s.", len(dbs), idText(in.ConnectionID)), nil
}

func (s *Server) listCollections(ctx context.Context, in collectionsInput) (collectionList, string, error) {
	out := collectionList{ConnectionID: in.ConnectionID, Database: in.Database, Collections: []connections.Collection{}}
	if err := requireID("connection_id", in.ConnectionID); err != nil {
		return out, "", err
	}
	if s.cfg.Connections == nil {
		return out, "", connections.ErrNotFound
	}
	cols, err := s.cfg.Connections.Collections(ctx, in.ConnectionID, in.Database)
	if err != nil {
		return out, "", err
	}
	out.Collections = cols
	return out, fmt.Sprintf("%d collection(s) in database %s.", len(cols), quoted(in.Database)), nil
}

func (s *Server) listJobs(ctx context.Context, in pageInput) (jobList, string, error) {
	jobs, err := s.cfg.Operations.ListJobs(ctx)
	if err != nil {
		return jobList{}, "", err
	}
	start, end, next, err := page(len(jobs), in.Limit, in.Cursor)
	if err != nil {
		return jobList{}, "", err
	}
	out := jobList{Jobs: jobs[start:end], Total: len(jobs), NextCursor: next}
	return out, fmt.Sprintf("%d of %d job(s).", end-start, len(jobs)), nil
}

func (s *Server) getJob(ctx context.Context, in idInput) (*models.Job, string, error) {
	if err := requireID("id", in.ID); err != nil {
		return nil, "", err
	}
	job, err := s.cfg.Operations.GetJob(ctx, in.ID)
	if err != nil {
		return nil, "", err
	}
	return job, fmt.Sprintf("Job %s (enabled: %v): last run %s, next run %s.", idText(job.ID), job.Enabled, timestamp(job.LastRun), timestamp(job.NextRun)), nil
}

func (s *Server) listBackups(ctx context.Context, in listBackupsInput) (backupList, string, error) {
	list, err := s.cfg.Operations.ListBackups(ctx, operations.BackupFilter{
		Database: in.Database, ConnectionID: in.ConnectionID, Status: models.BackupStatus(in.Status),
	})
	if err != nil {
		return backupList{}, "", err
	}
	start, end, next, err := page(len(list), in.Limit, in.Cursor)
	if err != nil {
		return backupList{}, "", err
	}
	out := backupList{Backups: list[start:end], Total: len(list), NextCursor: next}
	return out, fmt.Sprintf("%d of %d backup(s), newest first.", end-start, len(list)), nil
}

func (s *Server) getBackup(ctx context.Context, in idInput) (*models.BackupRecord, string, error) {
	if err := requireID("id", in.ID); err != nil {
		return nil, "", err
	}
	b, err := s.cfg.Operations.GetBackup(ctx, in.ID)
	if err != nil {
		return nil, "", err
	}
	return b, backupSummary(b), nil
}

// backupSummary describes a backup record in one sentence (no error message, which
// is untrusted text; it is in the structured content).
func backupSummary(b *models.BackupRecord) string {
	switch b.Status {
	case models.StatusCompleted:
		return fmt.Sprintf("Backup %s of database %s is completed (%d bytes, sha256 %s).", idText(b.ID), quoted(b.Database), b.SizeBytes, b.SHA256)
	case models.StatusFailed:
		return fmt.Sprintf("Backup %s of database %s failed; error_message in the result has the details.", idText(b.ID), quoted(b.Database))
	default:
		return fmt.Sprintf("Backup %s of database %s is %s.", idText(b.ID), quoted(b.Database), b.Status)
	}
}

func (s *Server) listRestores(ctx context.Context, in pageInput) (restoreList, string, error) {
	list, err := s.cfg.Operations.ListRestores(ctx)
	if err != nil {
		return restoreList{}, "", err
	}
	start, end, next, err := page(len(list), in.Limit, in.Cursor)
	if err != nil {
		return restoreList{}, "", err
	}
	out := restoreList{Restores: list[start:end], Total: len(list), NextCursor: next}
	return out, fmt.Sprintf("%d of %d restore(s), newest first.", end-start, len(list)), nil
}

func (s *Server) getRestore(ctx context.Context, in idInput) (*models.RestoreRecord, string, error) {
	if err := requireID("id", in.ID); err != nil {
		return nil, "", err
	}
	r, err := s.cfg.Operations.GetRestore(ctx, in.ID)
	if err != nil {
		return nil, "", err
	}
	return r, restoreSummary(r), nil
}

// restoreSummary describes a restore record in one sentence (no error message, which
// is untrusted text; it is in the structured content).
func restoreSummary(r *models.RestoreRecord) string {
	switch r.Status {
	case models.RestoreStatusCompleted:
		return fmt.Sprintf("Restore %s of backup %s into database %s is completed (verified: %v).", idText(r.ID), idText(r.BackupID), quoted(r.TargetDatabase), r.Verified)
	case models.RestoreStatusFailed:
		return fmt.Sprintf("Restore %s of backup %s into database %s failed; error_message in the result has the details.", idText(r.ID), idText(r.BackupID), quoted(r.TargetDatabase))
	default:
		return fmt.Sprintf("Restore %s of backup %s into database %s is %s.", idText(r.ID), idText(r.BackupID), quoted(r.TargetDatabase), r.Status)
	}
}

func (s *Server) listStorageTargets(ctx context.Context, _ noInput) (targetList, string, error) {
	out := targetList{StorageTargets: []TargetView{}}
	if s.cfg.Targets == nil {
		return out, "No storage targets are configured.", nil
	}
	list, err := s.cfg.Targets.List(ctx)
	if err != nil {
		return out, "", err
	}
	def := "none"
	for _, t := range list {
		out.StorageTargets = append(out.StorageTargets, TargetView{
			ID: t.ID, Name: t.Name, Type: t.Type, IsDefault: t.IsDefault, Location: t.Location(), LastTestOK: t.LastTestOK,
		})
		if t.IsDefault {
			def = idText(t.ID)
		}
	}
	return out, fmt.Sprintf("%d storage target(s); default target: %s.", len(list), def), nil
}

func (s *Server) getStatus(ctx context.Context, _ noInput) (*operations.Status, string, error) {
	st, err := s.cfg.Operations.Status(ctx)
	if err != nil {
		return nil, "", err
	}
	return st, fmt.Sprintf("MongoRescue %s is %s: %d job(s), %d backup(s) (%d failed), %d running backup(s), %d running restore(s), %d failure(s) in the last 24h.",
		st.Version, st.Health, st.Jobs, st.Stats.TotalBackups, st.Stats.FailedBackups, st.RunningBackups, st.RunningRestores, len(st.FailedLast24h)), nil
}

func (s *Server) startBackup(ctx context.Context, in startBackupInput) (backupStarted, string, error) {
	if err := requireID("connection_id", in.ConnectionID); err != nil {
		return backupStarted{}, "", err
	}
	rec, err := s.cfg.Operations.StartBackup(ctx, operations.BackupRequest{
		BackupOptions: models.BackupOptions{
			ConnectionID: in.ConnectionID, Database: in.Database, StorageTargetID: in.StorageTargetID,
			Collections: in.Collections, ExcludeCollections: in.ExcludeCollections,
		},
		Gzip: in.Gzip,
	})
	if err != nil {
		return backupStarted{}, "", err
	}
	next := fmt.Sprintf("poll get_backup with id %q until status is completed or failed", rec.ID)
	return backupStarted{Backup: rec, NextStep: next}, fmt.Sprintf("Backup %s of database %s started; %s.", idText(rec.ID), quoted(rec.Database), next), nil
}

func (s *Server) runJob(ctx context.Context, in runJobInput) (backupStarted, string, error) {
	if err := requireID("job_id", in.JobID); err != nil {
		return backupStarted{}, "", err
	}
	rec, err := s.cfg.Operations.RunJob(ctx, in.JobID)
	if err != nil {
		return backupStarted{}, "", err
	}
	next := fmt.Sprintf("poll get_backup with id %q until status is completed or failed", rec.ID)
	return backupStarted{Backup: rec, NextStep: next}, fmt.Sprintf("Job %s started backup %s; %s.", idText(rec.JobID), idText(rec.ID), next), nil
}

func (s *Server) restoreSafeClone(ctx context.Context, in restoreInput) (restoreStarted, string, error) {
	if err := requireID("backup_id", in.BackupID); err != nil {
		return restoreStarted{}, "", err
	}
	// Always a safe clone: this tool has no way to name a target database or to
	// drop collections, so nothing that exists can be overwritten.
	rec, err := s.cfg.Operations.StartRestore(ctx, models.RestoreRequest{
		BackupID:            in.BackupID,
		SafeClone:           ptr(true),
		TargetConnectionID:  in.TargetConnectionID,
		SelectedCollections: in.Collections,
		Verify:              in.Verify,
	})
	if err != nil {
		return restoreStarted{}, "", err
	}
	next := fmt.Sprintf("poll get_restore with id %q until status is completed or failed", rec.ID)
	return restoreStarted{Restore: rec, NextStep: next},
		fmt.Sprintf("Restore %s of backup %s into the new database %s started; %s.", idText(rec.ID), idText(rec.BackupID), quoted(rec.TargetDatabase), next), nil
}

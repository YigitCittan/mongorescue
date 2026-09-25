// Package mcp is the Model Context Protocol adapter of MongoRescue: it lets AI
// assistants (Claude Desktop and Claude Code, VS Code, Cursor, custom agents) inspect
// and operate MongoRescue through tools, resources and prompts.
//
// Like internal/server it is a delivery adapter: every tool calls the same
// application services the REST API uses (operations, connections, targets), so
// validation and safety rules cannot drift. Two transports are offered: Streamable
// HTTP on the main listener (Server.Handler, mounted at /mcp) and a stdio bridge
// (RunBridge, the "mongorescue mcp" subcommand) that forwards to a running instance.
//
// Safety model: callers authenticate with an API key (never a browser session); each
// tool requires a scope (read or operator) and tools/list only shows the tools the
// key may call; nothing can be deleted and restores always go into a fresh
// <db>_rescue_<timestamp> clone; every tool call is audited, counted in
// mongorescue_mcp_calls_total and rate limited per API key.
package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// Sentinel errors.
var (
	// ErrUnauthenticated is returned when a request reaches the MCP server without an
	// authenticated API key principal.
	ErrUnauthenticated = errors.New("mcp: authentication required")
	// ErrRateLimited is returned when an API key exceeds its tool call budget.
	ErrRateLimited = errors.New("mcp: rate limit exceeded")
)

// ServerName is the implementation name the MCP server reports.
const ServerName = "mongorescue"

// instructions are sent to clients on initialization; assistants typically show them
// to the model.
const instructions = `MongoRescue backs up and restores MongoDB databases.
Use the read tools (list_*, get_*) to inspect connections, jobs, backups, restores and storage targets; get_status gives an overview.
start_backup, run_job and restore_to_safe_clone start asynchronous operations and return a record with status "in_progress": poll get_backup or get_restore until the status is "completed" or "failed".
Restores through MCP always go into a new database named <db>_rescue_<timestamp>; nothing is overwritten and nothing can be deleted through MCP. In-place restores, deletions and configuration changes are done by a person in the MongoRescue dashboard.`

// ConnectionService is the part of connections.Service the MCP tools use.
type ConnectionService interface {
	// List returns every connection with redacted URIs.
	List(ctx context.Context) ([]*models.Connection, error)
	// Databases lists the databases of connection id.
	Databases(ctx context.Context, id string, includeSystem bool) ([]connections.Database, error)
	// Collections lists the collections of database on connection id.
	Collections(ctx context.Context, id, database string) ([]connections.Collection, error)
}

// TargetService is the part of targets.Service the MCP tools use.
type TargetService interface {
	// List returns every storage target with masked secrets.
	List(ctx context.Context) ([]*models.StorageTarget, error)
}

// Config holds the dependencies of a Server. Operations is required.
type Config struct {
	// Operations implements the backup, job and restore use cases.
	Operations *operations.Service
	// Connections lists connections, databases and collections; nil means none.
	Connections ConnectionService
	// Targets lists storage targets; nil means none.
	Targets TargetService
	// Audit records every tool call; nil disables auditing.
	Audit *audit.Service
	// ObserveCall counts tool calls by tool and result (metrics); nil disables it.
	ObserveCall func(tool, result string)
	// RateLimit bounds tool calls per API key; the zero value means DefaultRateLimit.
	RateLimit RateLimit
	// Version is the build version reported to clients.
	Version string
	// Logger receives operational logs; nil means slog.Default().
	Logger *slog.Logger
}

// Server is the MongoRescue MCP server. It is safe for concurrent use.
type Server struct {
	cfg     Config
	logger  *slog.Logger
	sdk     *sdk.Server
	tools   map[string]toolSpec
	limiter *limiter
	now     func() time.Time
}

// New builds the MCP server with every tool, resource and prompt registered. It
// panics when Operations is nil, which is a wiring bug.
func New(cfg Config) *Server {
	if cfg.Operations == nil {
		panic("mcp: Operations is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		cfg:     cfg,
		logger:  logger,
		tools:   map[string]toolSpec{},
		limiter: newLimiter(cfg.RateLimit, time.Now),
		now:     time.Now,
	}
	s.sdk = sdk.NewServer(&sdk.Implementation{Name: ServerName, Title: "MongoRescue", Version: cfg.Version}, &sdk.ServerOptions{
		Instructions: instructions,
		Logger:       logger,
		// No logging capability: this server only offers tools, resources and prompts.
		Capabilities: &sdk.ServerCapabilities{},
	})
	s.registerTools()
	s.registerResources()
	s.registerPrompts()
	s.sdk.AddReceivingMiddleware(s.middleware)
	return s
}

// transportKey carries the transport name of a request in its context.
type transportKey struct{}

// withTransport returns ctx annotated with the transport name for audit records.
func withTransport(ctx context.Context, transport string) context.Context {
	return context.WithValue(ctx, transportKey{}, transport)
}

// transportFrom returns the transport stored by withTransport (default "http").
func transportFrom(ctx context.Context) string {
	if t, ok := ctx.Value(transportKey{}).(string); ok && t != "" {
		return t
	}
	return audit.TransportHTTP
}

// TransportHeader is sent by the stdio bridge so that audit records show the
// transport the assistant used. It is informational only: it grants nothing.
const TransportHeader = "X-MongoRescue-Transport"

// Handler returns the Streamable HTTP handler of the MCP endpoint. It runs stateless
// (every POST is self-contained; GET and DELETE answer 405) and answers with JSON
// rather than server-sent events, which keeps it simple behind proxies.
//
// The caller must authenticate requests first and attach the auth.Principal to the
// request context (internal/server does, accepting API keys only); requests without
// a principal are refused. Origin checks and the enable switch are the caller's too.
func (s *Server) Handler() http.Handler {
	streamable := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s.sdk }, &sdk.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		Logger:       s.logger,
		// The embedding server validates Origin and Host (with its proxy settings);
		// the SDK's fixed localhost rule would break reverse proxies on the same host.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        maxRequestBody,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principalOf(r.Context()) == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mongorescue"`)
			http.Error(w, "unauthorized: supply an API key", http.StatusUnauthorized)
			return
		}
		transport := audit.TransportHTTP
		if r.Header.Get(TransportHeader) == audit.TransportStdio {
			transport = audit.TransportStdio
		}
		streamable.ServeHTTP(w, r.WithContext(withTransport(r.Context(), transport)))
	})
}

// maxRequestBody bounds MCP request bodies (tool arguments are small).
const maxRequestBody = 1 << 20

// Connect serves one MCP session over t, for in-process clients and tests. ctx must
// carry the caller's auth.Principal.
func (s *Server) Connect(ctx context.Context, t sdk.Transport) (*sdk.ServerSession, error) {
	return s.sdk.Connect(ctx, t, nil)
}

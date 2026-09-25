package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Bridge errors.
var (
	// ErrBridgeConfig is returned for an invalid bridge configuration (URL, API key).
	ErrBridgeConfig = errors.New("mcp bridge: invalid configuration")
	// ErrBridgeAuth is returned when the MongoRescue server refuses the API key.
	ErrBridgeAuth = errors.New("mcp bridge: the server refused the API key")
	// ErrBridgeConnect is returned when the MongoRescue server cannot be reached or
	// does not speak MCP.
	ErrBridgeConnect = errors.New("mcp bridge: cannot connect to the MongoRescue server")
)

// Bridge defaults.
const (
	// DefaultBridgeURL is the MongoRescue instance the bridge connects to by default.
	DefaultBridgeURL = "http://127.0.0.1:8080"
	// bridgeRequestTimeout bounds one forwarded request.
	bridgeRequestTimeout = 2 * time.Minute
)

// BridgeConfig configures RunBridge.
type BridgeConfig struct {
	// URL is the base URL of the running MongoRescue instance (DefaultBridgeURL when
	// empty); "/mcp" is appended unless the URL already ends with it.
	URL string
	// APIKey authenticates to the instance. It is sent as a bearer token and never
	// logged.
	APIKey string
	// Version is the bridge's build version, sent as its client version.
	Version string
	// Logger receives diagnostics; it must not write to the protocol stream (stdout).
	Logger *slog.Logger
	// In and Out carry the MCP protocol; nil means stdin and stdout.
	In  io.Reader
	Out io.Writer
	// HTTPClient overrides the HTTP client (tests); its transport is wrapped to add
	// the credentials.
	HTTPClient *http.Client
}

// Endpoint returns the MCP endpoint URL of cfg.URL, or an ErrBridgeConfig error.
func (cfg BridgeConfig) Endpoint() (string, error) {
	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		raw = DefaultBridgeURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: --url must be an http(s) URL such as %s", ErrBridgeConfig, DefaultBridgeURL)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/mcp") {
		u.Path += "/mcp"
	}
	return u.String(), nil
}

// RunBridge serves MCP over stdio (or cfg.In/cfg.Out) and forwards every tool call,
// resource read and prompt to the Streamable HTTP endpoint of a running MongoRescue
// instance, authenticated with cfg.APIKey. It lists the remote tools, resources and
// prompts once at start (so the key's scope decides what the assistant sees) and runs
// until the client disconnects or ctx is cancelled. It never opens the metadata
// database or the data directory.
func RunBridge(ctx context.Context, cfg BridgeConfig) error {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	endpoint, err := cfg.Endpoint()
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return fmt.Errorf("%w: set MONGORESCUE_MCP_API_KEY to an API key created under Settings → API keys", ErrBridgeConfig)
	}
	if u, _ := url.Parse(endpoint); u.Scheme == "http" && !isLoopback(u.Hostname()) {
		logger.Warn("the API key is sent over plain HTTP to a non-local host; use https", slog.String("host", u.Host))
	}

	base := http.DefaultTransport
	client := &http.Client{Timeout: bridgeRequestTimeout}
	if cfg.HTTPClient != nil {
		*client = *cfg.HTTPClient
		if cfg.HTTPClient.Transport != nil {
			base = cfg.HTTPClient.Transport
		}
	}
	endpointURL, _ := url.Parse(endpoint)
	creds := &credentialTransport{
		base: base, key: strings.TrimSpace(cfg.APIKey), userAgent: "mongorescue-mcp-bridge/" + cfg.Version,
		scheme: endpointURL.Scheme, host: endpointURL.Host,
	}
	client.Transport = creds
	// Never follow redirects: a 3xx could send the next request (and, with a
	// forwarding transport, the API key) to another host. The 3xx response is
	// returned as is and fails the MCP request.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	remoteClient := sdk.NewClient(&sdk.Implementation{Name: "mongorescue-mcp-bridge", Version: cfg.Version}, nil)
	remote, err := remoteClient.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: client, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		return connectError(creds.lastStatus.Load(), endpoint, err)
	}
	defer func() { _ = remote.Close() }()

	local, err := mirror(ctx, remote)
	if err != nil {
		return connectError(creds.lastStatus.Load(), endpoint, err)
	}
	logger.Info("mcp bridge connected", slog.String("endpoint", endpoint))

	var transport sdk.Transport = &sdk.StdioTransport{}
	if cfg.In != nil || cfg.Out != nil {
		in, out := cfg.In, cfg.Out
		if in == nil {
			in = os.Stdin
		}
		if out == nil {
			out = os.Stdout
		}
		transport = &sdk.IOTransport{Reader: readCloser(in), Writer: writeCloser(out)}
	}
	if err := local.Run(ctx, transport); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		return fmt.Errorf("mcp bridge: %w", err)
	}
	return nil
}

// connectError explains a failed connection, distinguishing refused credentials.
func connectError(status int32, endpoint string, err error) error {
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w (401): check MONGORESCUE_MCP_API_KEY", ErrBridgeAuth)
	case http.StatusForbidden:
		return fmt.Errorf("%w (403): the MCP endpoint may be disabled (Settings → Security) or the request origin was refused", ErrBridgeAuth)
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return fmt.Errorf("%w at %s: the server answered with a redirect (%d), which the bridge does not follow; set --url to the final address", ErrBridgeConnect, endpoint, status)
	}
	return fmt.Errorf("%w at %s: %w", ErrBridgeConnect, endpoint, err)
}

// mirror returns a local server that exposes the remote session's tools, resources,
// resource templates and prompts and forwards every request to it.
func mirror(ctx context.Context, remote *sdk.ClientSession) (*sdk.Server, error) {
	init := remote.InitializeResult()
	impl := &sdk.Implementation{Name: ServerName, Version: "unknown"}
	var instr string
	if init != nil {
		instr = init.Instructions
		if init.ServerInfo != nil {
			impl = &sdk.Implementation{Name: init.ServerInfo.Name, Title: init.ServerInfo.Title, Version: init.ServerInfo.Version}
		}
	}
	local := sdk.NewServer(impl, &sdk.ServerOptions{Instructions: instr, Capabilities: &sdk.ServerCapabilities{}})

	for tool, err := range remote.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("list tools: %w", err)
		}
		local.AddTool(tool, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			res, err := remote.CallTool(ctx, &sdk.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
			return res, forwardError(err)
		})
	}
	readResource := func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		res, err := remote.ReadResource(ctx, &sdk.ReadResourceParams{URI: req.Params.URI})
		return res, forwardError(err)
	}
	if init != nil && init.Capabilities != nil && init.Capabilities.Resources != nil {
		for r, err := range remote.Resources(ctx, nil) {
			if err != nil {
				return nil, fmt.Errorf("list resources: %w", err)
			}
			local.AddResource(r, readResource)
		}
		for t, err := range remote.ResourceTemplates(ctx, nil) {
			if err != nil {
				return nil, fmt.Errorf("list resource templates: %w", err)
			}
			local.AddResourceTemplate(t, readResource)
		}
	}
	if init != nil && init.Capabilities != nil && init.Capabilities.Prompts != nil {
		for p, err := range remote.Prompts(ctx, nil) {
			if err != nil {
				return nil, fmt.Errorf("list prompts: %w", err)
			}
			local.AddPrompt(p, func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
				res, err := remote.GetPrompt(ctx, &sdk.GetPromptParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
				return res, forwardError(err)
			})
		}
	}
	return local, nil
}

// forwardError passes JSON-RPC errors of the remote server (rate limits, unknown
// resources) through unchanged and reports transport failures as internal errors.
func forwardError(err error) error {
	if err == nil {
		return nil
	}
	var wire *jsonrpc.Error
	if errors.As(err, &wire) {
		return wire
	}
	return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "mongorescue server unavailable: " + err.Error()}
}

// credentialTransport adds the API key and the bridge's identity to every request
// for the configured endpoint (scheme and host) and remembers the last HTTP status,
// to explain refused connections. Requests to any other origin never carry the key.
type credentialTransport struct {
	base       http.RoundTripper
	key        string
	userAgent  string
	scheme     string
	host       string
	lastStatus atomic.Int32
}

// RoundTrip implements http.RoundTripper.
func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Del("Authorization")
	if strings.EqualFold(r.URL.Scheme, t.scheme) && strings.EqualFold(r.URL.Host, t.host) {
		r.Header.Set("Authorization", "Bearer "+t.key)
	}
	r.Header.Set(TransportHeader, "stdio")
	r.Header.Set("User-Agent", t.userAgent)
	resp, err := t.base.RoundTrip(r)
	if resp != nil {
		t.lastStatus.Store(int32(resp.StatusCode)) //nolint:gosec // G115: HTTP status codes fit in int32.
	}
	return resp, err
}

// isLoopback reports whether host is localhost or a loopback address.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// nopReadCloser and nopWriteCloser adapt plain streams for IOTransport.
type nopReadCloser struct{ io.Reader }

func (nopReadCloser) Close() error { return nil }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func readCloser(r io.Reader) io.ReadCloser {
	if rc, ok := r.(io.ReadCloser); ok {
		return rc
	}
	return nopReadCloser{r}
}

func writeCloser(w io.Writer) io.WriteCloser {
	if wc, ok := w.(io.WriteCloser); ok {
		return wc
	}
	return nopWriteCloser{w}
}

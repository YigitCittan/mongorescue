// Package apiclient is a client of the MongoRescue REST API (/api/v1) for the CLI and
// other out-of-process tools. It authenticates with an API key that is only ever
// sent to the configured origin, never follows redirects, decodes the response
// envelope and turns refusals into *APIError values that match the sentinel errors
// of this package. It does not depend on the server package.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/redact"
)

// DefaultURL is the MongoRescue instance a client talks to when no URL is set.
const DefaultURL = "http://127.0.0.1:8080"

// Defaults of a Client.
const (
	// DefaultTimeout bounds one request, including reading its response.
	DefaultTimeout = time.Minute
	// maxResponseBytes bounds the response body a client reads.
	maxResponseBytes = 64 << 20
)

// Sentinel errors. An *APIError unwraps to the one of its HTTP status; transport
// failures wrap ErrUnreachable.
var (
	// ErrInvalidConfig is returned by New for an unusable URL or API key.
	ErrInvalidConfig = errors.New("apiclient: invalid configuration")
	// ErrUnreachable is returned when the server cannot be reached.
	ErrUnreachable = errors.New("apiclient: server unreachable")
	// ErrUnexpectedResponse is returned for a response that is not a MongoRescue API
	// envelope, and for redirects, which the client never follows.
	ErrUnexpectedResponse = errors.New("apiclient: unexpected response")
	// ErrBadRequest matches 400, 415 and 422 responses: the request was refused.
	ErrBadRequest = errors.New("apiclient: request refused")
	// ErrUnauthorized matches 401 responses: no valid API key.
	ErrUnauthorized = errors.New("apiclient: unauthorized")
	// ErrForbidden matches 403 responses: the key's scope does not allow the request.
	ErrForbidden = errors.New("apiclient: forbidden")
	// ErrNotFound matches 404 responses, and lookups by ID that found nothing.
	ErrNotFound = errors.New("apiclient: not found")
	// ErrConflict matches 409 responses: the request conflicts with the server's
	// state (already running, a failed restore preflight).
	ErrConflict = errors.New("apiclient: conflict")
	// ErrRateLimited matches 429 responses.
	ErrRateLimited = errors.New("apiclient: rate limited")
	// ErrServer matches 5xx responses.
	ErrServer = errors.New("apiclient: server error")
)

// APIError is a refused request: an HTTP status of 300 or more. It unwraps to the
// sentinel error of its status.
type APIError struct {
	// Method and Path name the request.
	Method, Path string
	// StatusCode is the HTTP status.
	StatusCode int
	// Message is the envelope's error (redacted), or the status text.
	Message string
	// Data is the envelope's data, which some refusals carry (the checks of a failed
	// restore preflight).
	Data json.RawMessage
	// Location is the redirect target of a 3xx response (redacted).
	Location string
	// RetryAfter is the delay a 429 or 503 response asked for (Retry-After), or 0.
	RetryAfter time.Duration
}

// Error implements error.
func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	if e.Location != "" {
		msg += " (redirect to " + e.Location + ", which is not followed; set the URL to the final address)"
	}
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.StatusCode, msg)
}

// Unwrap returns the sentinel error of the status.
func (e *APIError) Unwrap() error {
	switch {
	case e.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case e.StatusCode == http.StatusForbidden:
		return ErrForbidden
	case e.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case e.StatusCode == http.StatusConflict:
		return ErrConflict
	case e.StatusCode == http.StatusTooManyRequests:
		return ErrRateLimited
	case e.StatusCode >= 500:
		return ErrServer
	case e.StatusCode >= 400:
		return ErrBadRequest
	}
	return ErrUnexpectedResponse
}

// Meta describes the page of a paged list.
type Meta struct {
	// Total counts every match.
	Total int `json:"total"`
	// Limit and Offset repeat the page asked for.
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// Result is a decoded response: the typed value, the envelope's data exactly as the
// server sent it (for JSON output) and the page of a paged list.
type Result[T any] struct {
	// Value is the decoded data.
	Value T
	// Raw is the undecoded data.
	Raw json.RawMessage
	// Meta is set for paged lists.
	Meta *Meta
}

// Config configures New.
type Config struct {
	// URL is the base URL of the instance (DefaultURL when empty), with an optional
	// path prefix for a reverse proxy; "/api/v1/..." is appended to it.
	URL string
	// APIKey is sent as a bearer token to the URL's origin only.
	APIKey string
	// UserAgent is the client's User-Agent.
	UserAgent string
	// Transport is sent as TransportHeader ("cli" for the CLI).
	Transport string
	// HTTPClient overrides the HTTP client (tests); its transport is wrapped to add
	// the credentials and its redirect policy is replaced. Nil uses a client with
	// DefaultTimeout.
	HTTPClient *http.Client
}

// Client calls the REST API of one MongoRescue instance. It is safe for concurrent
// use.
type Client struct {
	base      *url.URL
	http      *http.Client
	transport *BearerTransport
}

// ParseURL validates a base URL: http or https, with a host, without credentials, a
// query or a fragment. An empty URL is DefaultURL. The trailing slash of its path is
// removed. Errors wrap ErrInvalidConfig and never repeat the URL.
func ParseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" {
		return nil, fmt.Errorf("%w: the URL must be an http(s) URL such as %s", ErrInvalidConfig, DefaultURL)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: the URL must not contain credentials; pass the API key separately", ErrInvalidConfig)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("%w: the URL must not have a query or a fragment", ErrInvalidConfig)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

// New returns a client for cfg. It returns an ErrInvalidConfig error for an invalid
// URL or a missing API key.
func New(cfg Config) (*Client, error) {
	base, err := ParseURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, fmt.Errorf("%w: no API key", ErrInvalidConfig)
	}
	if strings.ContainsFunc(key, func(r rune) bool { return r < 0x21 || r == 0x7f }) {
		return nil, fmt.Errorf("%w: the API key contains spaces or control characters", ErrInvalidConfig)
	}
	hc := &http.Client{Timeout: DefaultTimeout}
	var inner http.RoundTripper
	if cfg.HTTPClient != nil {
		*hc = *cfg.HTTPClient
		inner = cfg.HTTPClient.Transport
	}
	tr := NewBearerTransport(inner, base, key, cfg.UserAgent, cfg.Transport)
	hc.Transport = tr
	// Never follow redirects: the next request could go to another origin. The 3xx
	// response is returned as an *APIError.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: base, http: hc, transport: tr}, nil
}

// BaseURL returns the base URL the client talks to.
func (c *Client) BaseURL() *url.URL {
	u := *c.base
	return &u
}

// envelope is the response body of every API route.
type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
	Message string          `json:"message"`
	Meta    *Meta           `json:"meta"`
}

// do sends one request to path (under /api/v1) with the query and the JSON body (nil
// for none) and returns the decoded envelope of a 2xx response.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*envelope, error) {
	u := *c.base
	u.Path = c.base.Path + "/api/v1" + path
	u.RawQuery = query.Encode()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("apiclient: encode %s %s: %w", method, path, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %s", ErrInvalidConfig, method, path, redact.Text(err.Error()))
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%s %s: %w", method, path, ctxErr)
		}
		return nil, fmt.Errorf("%w: %s %s: %s", ErrUnreachable, method, path, transportMessage(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%s %s: %w", method, path, ctxErr)
		}
		return nil, fmt.Errorf("%w: %s %s: reading the response: %s", ErrUnreachable, method, path, transportMessage(err))
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("%w: %s %s: the response is larger than %d MiB", ErrUnexpectedResponse, method, path, maxResponseBytes>>20)
	}
	var env envelope
	decodeErr := json.Unmarshal(data, &env)
	if resp.StatusCode >= 300 {
		apiErr := &APIError{Method: method, Path: path, StatusCode: resp.StatusCode}
		if decodeErr == nil {
			apiErr.Message = redact.Text(strings.TrimSpace(env.Error))
			apiErr.Data = env.Data
		}
		if resp.StatusCode < 400 {
			apiErr.Location = redact.URI(resp.Header.Get("Location"))
		}
		apiErr.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), time.Now())

		return nil, apiErr
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("%w: %s %s answered %d with a body that is not a MongoRescue API response; check the URL", ErrUnexpectedResponse, method, path, resp.StatusCode)
	}
	return &env, nil
}

// retryAfter parses a Retry-After header: delay seconds or an HTTP date. It returns
// 0 for a missing, invalid or past value.
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// transportMessage describes a transport failure without the request URL, redacted.
func transportMessage(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	return redact.Text(err.Error())
}

// call sends a request and decodes its data into a Result.
func call[T any](ctx context.Context, c *Client, method, path string, query url.Values, body any) (*Result[T], error) {
	env, err := c.do(ctx, method, path, query, body)
	if err != nil {
		return nil, err
	}
	res := &Result[T]{Raw: env.Data, Meta: env.Meta}
	if len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, &res.Value); err != nil {
			return nil, fmt.Errorf("%w: %s %s: decode data: %w", ErrUnexpectedResponse, method, path, err)
		}
	}
	return res, nil
}

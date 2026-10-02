package auditlog

import (
	"context"
	"maps"
	"sync"
)

// Client describes the caller of a request: its address and user agent.
type Client struct {
	// IP is the client address (see Event.ClientIP).
	IP string
	// UserAgent is the User-Agent header.
	UserAgent string
}

type clientKey struct{}

// WithClient returns a context carrying c, which Record uses for entries that do
// not name their client themselves (MCP tool calls, for example).
func WithClient(ctx context.Context, c Client) context.Context {
	return context.WithValue(ctx, clientKey{}, c)
}

// ClientFrom returns the client stored by WithClient.
func ClientFrom(ctx context.Context) (Client, bool) {
	c, ok := ctx.Value(clientKey{}).(Client)
	return c, ok
}

// annotations are targets a handler adds to the entry of its own request.
type annotations struct {
	mu sync.Mutex
	m  map[string]string
}

type annotationsKey struct{}

// WithAnnotations returns a context in which Annotate collects targets for the
// entry of the current request (the auth middleware sets it up).
func WithAnnotations(ctx context.Context) context.Context {
	return context.WithValue(ctx, annotationsKey{}, &annotations{m: map[string]string{}})
}

// Annotate adds the target key=value to the entry of the request ctx belongs to,
// for example the names of the settings a request changed. Values must never be
// secrets or request bodies. Without WithAnnotations it does nothing.
func Annotate(ctx context.Context, key, value string) {
	a, ok := ctx.Value(annotationsKey{}).(*annotations)
	if !ok {
		return
	}
	a.mu.Lock()
	a.m[key] = value
	a.mu.Unlock()
}

// Annotations returns a copy of the targets added with Annotate.
func Annotations(ctx context.Context) map[string]string {
	a, ok := ctx.Value(annotationsKey{}).(*annotations)
	if !ok {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.m)
}

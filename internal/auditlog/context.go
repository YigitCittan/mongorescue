package auditlog

import "context"

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

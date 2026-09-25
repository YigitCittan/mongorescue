package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/redact"
)

// SendTimeout bounds a single delivery attempt.
const SendTimeout = 10 * time.Second

// maxResponseExcerpt bounds how much of a provider response is kept for diagnostics.
const maxResponseExcerpt = 512

// NewHTTPClient returns the HTTP client used by HTTP-based notifiers: it has an overall
// timeout and never follows redirects (a redirect would silently change the target and
// could replay signed payloads or credentials to another host).
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: SendTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// scrubbedError hides secrets from an error message while preserving the chain for
// errors.Is / errors.As.
type scrubbedError struct {
	msg string
	err error
}

// Error returns the scrubbed message.
func (e *scrubbedError) Error() string { return e.msg }

// Unwrap returns the original error.
func (e *scrubbedError) Unwrap() error { return e.err }

// scrub replaces every secret (and URL credentials) in err's message with redact.Mask.
func scrub(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := redact.Text(err.Error())
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, redact.Mask)
		}
	}
	if msg == err.Error() {
		return err
	}
	return &scrubbedError{msg: msg, err: err}
}

// sanitizeURLError rebuilds a *url.Error so that the request URL (which may carry a
// bot token or a webhook secret in its path or query) never enters the error text:
// only the origin is kept, via RedactEndpoint. The underlying cause stays wrapped so
// errors.Is(err, context.DeadlineExceeded) and similar checks keep working.
func sanitizeURLError(prefix string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %s %s: %w", prefix, ue.Op, RedactEndpoint(ue.URL), ue.Err)
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// permanentf returns an error wrapping ErrPermanent.
func permanentf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPermanent, fmt.Sprintf(format, args...))
}

// doHTTP executes req and maps the response status: 2xx succeeds, 408/429/5xx are
// retryable failures, any other status is permanent. The response body is always
// drained and closed; a short excerpt is returned in the error. On success the (bounded)
// body is returned for provider-specific checks.
func doHTTP(ctx context.Context, client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, sanitizeURLError("http request", err)
	}
	defer func() {
		// Drain a bounded amount so the connection can be reused; errors are irrelevant here.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseExcerpt))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}

	excerpt := singleLine(string(body))
	switch {
	case resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		return nil, fmt.Errorf("http status %d: %s", resp.StatusCode, excerpt)
	default:
		return nil, permanentf("http status %d: %s", resp.StatusCode, excerpt)
	}
}

// isPermanent reports whether err must not be retried.
func isPermanent(err error) bool {
	return errors.Is(err, ErrPermanent) || errors.Is(err, ErrHeaderInjection) ||
		errors.Is(err, ErrInvalidChannelConfig)
}

package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// DefaultTwilioBaseURL is the Twilio REST API root.
const DefaultTwilioBaseURL = "https://api.twilio.com"

// TwilioNotifier sends SMS through the Twilio Messages API. Other SMS providers can be
// reached through a webhook channel pointed at the provider's HTTP API or a relay.
//
// A notifier remembers which recipients already accepted a given text, so retrying a
// partially failed Send does not text the successful recipients twice.
type TwilioNotifier struct {
	cfg     TwilioConfig
	baseURL string
	client  *http.Client

	mu        sync.Mutex
	delivered map[string]struct{}
}

// NewTwilioNotifier returns a Twilio notifier. baseURL overrides the API root (for
// tests); empty uses DefaultTwilioBaseURL. A nil client uses NewHTTPClient.
func NewTwilioNotifier(cfg TwilioConfig, baseURL string, client *http.Client) *TwilioNotifier {
	if baseURL == "" {
		baseURL = DefaultTwilioBaseURL
	}
	if client == nil {
		client = NewHTTPClient()
	}
	return &TwilioNotifier{
		cfg:       cfg,
		baseURL:   strings.TrimRight(baseURL, "/"),
		client:    client,
		delivered: make(map[string]struct{}),
	}
}

// Type returns "twilio".
func (n *TwilioNotifier) Type() string { return string(ChannelTwilio) }

// Send posts one message per recipient to
// POST <base>/2010-04-01/Accounts/<sid>/Messages.json using HTTP basic auth. All
// recipients are attempted; the joined errors are returned.
func (n *TwilioNotifier) Send(ctx context.Context, msg Message) error {
	if err := validateTwilio(&n.cfg); err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json", n.baseURL, url.PathEscape(n.cfg.AccountSID))
	text := smsText(msg)

	var errs []error
	for _, to := range n.cfg.To {
		key := to + "\x00" + text
		n.mu.Lock()
		_, done := n.delivered[key]
		n.mu.Unlock()
		if done {
			continue
		}

		form := url.Values{}
		form.Set("To", to)
		form.Set("Body", text)
		if twilioMsgSvcPattern.MatchString(n.cfg.From) {
			form.Set("MessagingServiceSid", n.cfg.From)
		} else {
			form.Set("From", n.cfg.From)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			errs = append(errs, sanitizeURLError("build twilio request", err))
			continue
		}
		req.SetBasicAuth(n.cfg.AccountSID, n.cfg.AuthToken)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", userAgent)

		if _, err := doHTTP(ctx, n.client, req); err != nil {
			errs = append(errs, fmt.Errorf("twilio to %s: %w", to, err))
		} else {
			n.mu.Lock()
			n.delivered[key] = struct{}{}
			n.mu.Unlock()
		}
		if ctx.Err() != nil {
			break
		}
	}
	return scrub(errors.Join(errs...), n.cfg.AuthToken)
}

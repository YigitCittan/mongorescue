package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/redact"
)

const (
	slackURL   = "https://hooks.slack.com/services/T0000/B0000/XXslackSECRETXX"
	discordURL = "https://discord.com/api/webhooks/123456/discordTOKENvalue"
	tokenURL   = "https://relay.example.com:8443/notify?token=qTOKENq&sig=qSIGq" // #nosec G101 -- test fixture
)

// urlSecrets are fragments that must never be exposed.
var urlSecrets = []string{"XXslackSECRETXX", "discordTOKENvalue", "qTOKENq", "qSIGq", "services/T0000", "userPASS"}

func TestRedactEndpoint(t *testing.T) {
	tests := []struct{ in, want string }{
		{slackURL, "https://hooks.slack.com/******"},
		{discordURL, "https://discord.com/******"},
		{tokenURL, "https://relay.example.com:8443/******"},
		{"https://user:userPASS@h.example.com", "https://h.example.com/******"},
		{"https://h.example.com/#frag", "https://h.example.com/******"},
		{"https://h.example.com", "https://h.example.com"},
		{"https://h.example.com/", "https://h.example.com/"},
		{"http://10.0.0.1:9000", "http://10.0.0.1:9000"},
		{"::not a url", redact.Mask},
		{"", ""},
	}
	for _, tt := range tests {
		if got := RedactEndpoint(tt.in); got != tt.want {
			t.Errorf("RedactEndpoint(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestChannelRedactedHidesURLSecrets(t *testing.T) {
	for _, u := range []string{slackURL, discordURL, tokenURL, "https://user:userPASS@h.example.com/x"} {
		ch := webhookChannel("w")
		ch.Webhook.URL = u
		red := ch.Redacted()
		for _, s := range urlSecrets {
			if strings.Contains(red.Webhook.URL, s) {
				t.Errorf("Redacted(%q).URL = %q leaks %q", u, red.Webhook.URL, s)
			}
		}
	}
}

func TestSanitizeURLError(t *testing.T) {
	inner := context.DeadlineExceeded
	err := sanitizeURLError("http request", &url.Error{Op: "Post", URL: "https://api.telegram.org/bot" + testBotToken + "/sendMessage", Err: inner})
	if strings.Contains(err.Error(), testBotToken) || strings.Contains(err.Error(), "sendMessage") {
		t.Fatalf("url leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "Post https://api.telegram.org/******") {
		t.Errorf("unexpected message %q", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("cause must remain inspectable")
	}
	if got := sanitizeURLError("x", errors.New("plain")); got.Error() != "x: plain" {
		t.Errorf("non-url error = %q", got)
	}
}

func TestTransportErrorsNeverContainWebhookURL(t *testing.T) {
	// A closed server makes net/http return *url.Error carrying the full request URL.
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()

	for _, path := range []string{"/services/T0000/B0000/XXslackSECRETXX", "/api/webhooks/123456/discordTOKENvalue", "/n?token=qTOKENq&sig=qSIGq"} {
		ch := webhookChannel("w")
		ch.Webhook.URL = base + path
		n := NewWebhookNotifier(*ch.Webhook, nil)
		err := n.Send(context.Background(), testMessage())
		if err == nil {
			t.Fatal("expected transport error")
		}
		// Service.send additionally scrubs with secretsOf; the notifier alone must not leak.
		msg := err.Error() + scrub(err, secretsOf(ch)...).Error()
		for _, s := range urlSecrets {
			if strings.Contains(msg, s) {
				t.Errorf("error for %s leaks %q: %v", path, s, err)
			}
		}
	}
}

func TestSecretsOfIncludesWebhookURLParts(t *testing.T) {
	ch := webhookChannel("w")
	ch.Webhook.URL = "https://u:userPASS@relay.example.com/services/T0000/B0000/XXslackSECRETXX?token=qTOKENq"
	got := strings.Join(secretsOf(ch), "\n")
	for _, want := range []string{ch.Webhook.URL, "/services/T0000/B0000/XXslackSECRETXX", "qTOKENq", "userPASS"} {
		if !strings.Contains(got, want) {
			t.Errorf("secretsOf misses %q", want)
		}
	}
	msg := scrub(errors.New("failed at "+ch.Webhook.URL), secretsOf(ch)...).Error()
	for _, s := range urlSecrets {
		if strings.Contains(msg, s) {
			t.Errorf("scrub leaks %q in %q", s, msg)
		}
	}
}

func TestMaskedSecretsBoundToDestination(t *testing.T) {
	storedWebhook := func() *Channel {
		ch := webhookChannel("w")
		ch.Webhook.URL = slackURL
		return ch
	}
	storedEmail := func() *Channel {
		return &Channel{ID: "e", Type: ChannelEmail, Email: &EmailConfig{
			Host: "smtp.example.com", Port: 587, Username: "ops", Password: "smtp-pass", Security: SecuritySTARTTLS,
			From: "a@example.com", To: []string{"b@example.com"},
		}}
	}
	storedTwilio := func() *Channel {
		return &Channel{ID: "s", Type: ChannelTwilio, Twilio: &TwilioConfig{
			AccountSID: testTwilioSID, AuthToken: "tw-token", From: "+15551234567", To: []string{"+15551234568"},
		}}
	}

	tests := []struct {
		name    string
		stored  *Channel
		mutate  func(*Channel) // applied to the redacted round-trip copy
		wantErr bool
		check   func(*testing.T, *Channel)
	}{
		{
			name: "webhook unchanged keeps secrets", stored: storedWebhook(), mutate: func(*Channel) {},
			check: func(t *testing.T, c *Channel) {
				if c.Webhook.URL != slackURL || c.Webhook.Secret != "hmac-secret" || c.Webhook.Headers["Authorization"] != "Bearer live" {
					t.Errorf("secrets not kept: %+v", c.Webhook)
				}
			},
		},
		{
			name: "webhook header to attacker host", stored: storedWebhook(),
			mutate:  func(c *Channel) { c.Webhook.URL = "https://attacker.tld/collect"; c.Webhook.Secret = "new" },
			wantErr: true,
		},
		{
			name: "webhook hmac to attacker host", stored: storedWebhook(),
			mutate: func(c *Channel) {
				c.Webhook.URL = "https://attacker.tld/collect"
				c.Webhook.Headers["Authorization"] = "Bearer typed-again"
			},
			wantErr: true,
		},
		{
			name: "webhook port change", stored: storedWebhook(),
			mutate:  func(c *Channel) { c.Webhook.URL = "https://hooks.slack.com:8443/services/x" },
			wantErr: true,
		},
		{
			name: "webhook scheme downgrade", stored: storedWebhook(),
			mutate:  func(c *Channel) { c.Webhook.URL = "http://hooks.slack.com/services/x" },
			wantErr: true,
		},
		{
			name: "webhook same host different path with masked header", stored: storedWebhook(),
			mutate: func(c *Channel) {
				c.Webhook.URL = "https://hooks.slack.com/services/T1/B1/otherTenant"
				c.Webhook.Secret = "fresh"
			},
			wantErr: true,
		},
		{
			name: "webhook same host different path with masked hmac", stored: storedWebhook(),
			mutate: func(c *Channel) {
				c.Webhook.URL = "https://hooks.slack.com/services/T1/B1/otherTenant"
				c.Webhook.Headers["Authorization"] = "Bearer fresh"
			},
			wantErr: true,
		},
		{
			name: "webhook real unchanged url keeps secrets", stored: storedWebhook(),
			mutate: func(c *Channel) { c.Webhook.URL = slackURL },
			check: func(t *testing.T, c *Channel) {
				if c.Webhook.Secret != "hmac-secret" || c.Webhook.Headers["Authorization"] != "Bearer live" {
					t.Error("re-supplying the identical URL should keep the secrets")
				}
			},
		},
		{
			name: "webhook new path with fresh secrets", stored: storedWebhook(),
			mutate: func(c *Channel) {
				c.Webhook.URL = "https://hooks.slack.com/services/T1/B1/otherTenant"
				c.Webhook.Secret = "fresh"
				c.Webhook.Headers["Authorization"] = "Bearer fresh"
			},
		},
		{
			name: "webhook forged masked url", stored: storedWebhook(),
			mutate:  func(c *Channel) { c.Webhook.URL = "https://attacker.tld/******" },
			wantErr: true,
		},
		{
			name: "webhook destination change with fresh secrets", stored: storedWebhook(),
			mutate: func(c *Channel) {
				c.Webhook.URL = "https://new.example.com/hook"
				c.Webhook.Secret = "fresh"
				c.Webhook.Headers["Authorization"] = "Bearer fresh"
			},
		},
		{
			name: "email unchanged keeps password", stored: storedEmail(), mutate: func(*Channel) {},
			check: func(t *testing.T, c *Channel) {
				if c.Email.Password != "smtp-pass" {
					t.Error("password not kept")
				}
			},
		},
		{name: "email host change", stored: storedEmail(), mutate: func(c *Channel) { c.Email.Host = "smtp.attacker.tld" }, wantErr: true},
		{name: "email port change", stored: storedEmail(), mutate: func(c *Channel) { c.Email.Port = 25 }, wantErr: true},
		{name: "email username change", stored: storedEmail(), mutate: func(c *Channel) { c.Email.Username = "other" }, wantErr: true},
		{name: "email security downgrade", stored: storedEmail(), mutate: func(c *Channel) { c.Email.Security = SecurityNone }, wantErr: true},
		{name: "email recipients change keeps password", stored: storedEmail(), mutate: func(c *Channel) { c.Email.To = []string{"x@example.com"} }},
		{name: "twilio sid change", stored: storedTwilio(), mutate: func(c *Channel) { c.Twilio.AccountSID = "AC" + strings.Repeat("f", 32) }, wantErr: true},
		{name: "twilio recipients change keeps token", stored: storedTwilio(), mutate: func(c *Channel) { c.Twilio.To = []string{"+15559999999"} }},
		{
			name:   "telegram chat change keeps token",
			stored: &Channel{ID: "t", Type: ChannelTelegram, Telegram: &TelegramConfig{BotToken: testBotToken, ChatID: "1"}},
			mutate: func(c *Channel) { c.Telegram.ChatID = "2" },
			check: func(t *testing.T, c *Channel) {
				if c.Telegram.BotToken != testBotToken {
					t.Error("token not kept")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.stored.Redacted()
			tt.mutate(in)
			err := restoreSecrets(in, tt.stored)
			if tt.wantErr {
				if !errors.Is(err, ErrMaskedSecret) {
					t.Fatalf("restoreSecrets = %v; want ErrMaskedSecret", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("restoreSecrets: %v", err)
			}
			if tt.check != nil {
				tt.check(t, in)
			}
		})
	}
}

package notify

import "maps"

// Secret field names passed to TransformSecrets. Webhook header values use
// SecretFieldWebhookHeader followed by the header name.
//
//nolint:gosec // G101: these are field names, not credentials.
const (
	SecretFieldWebhookURL       = "webhook.url"
	SecretFieldWebhookSecret    = "webhook.secret"
	SecretFieldWebhookHeader    = "webhook.headers."
	SecretFieldTelegramBotToken = "telegram.bot_token"
	SecretFieldEmailPassword    = "email.password"
	SecretFieldTwilioAuthToken  = "twilio.auth_token"
)

// TransformSecrets returns a deep copy of c in which every credential-bearing field has
// been replaced by fn(field, value): the webhook URL (its path and query often embed a
// token), the webhook HMAC secret and every webhook header value, the Telegram bot
// token, the SMTP password and the Twilio auth token. field is one of the SecretField
// names (for headers SecretFieldWebhookHeader + the header name), so persistence
// adapters can bind each encrypted value to its location. Empty values are passed to
// fn as well, so fn decides how to treat them. The receiver is not modified.
func (c *Channel) TransformSecrets(fn func(field, value string) (string, error)) (*Channel, error) {
	out := c.Clone()
	if out == nil {
		return nil, nil
	}
	var err error
	apply := func(field string, p *string) {
		if err == nil {
			*p, err = fn(field, *p)
		}
	}
	if w := out.Webhook; w != nil {
		apply(SecretFieldWebhookURL, &w.URL)
		apply(SecretFieldWebhookSecret, &w.Secret)
		if w.Headers != nil {
			headers := maps.Clone(w.Headers)
			for k, v := range headers {
				apply(SecretFieldWebhookHeader+k, &v)
				headers[k] = v
			}
			w.Headers = headers
		}
	}
	if tg := out.Telegram; tg != nil {
		apply(SecretFieldTelegramBotToken, &tg.BotToken)
	}
	if e := out.Email; e != nil {
		apply(SecretFieldEmailPassword, &e.Password)
	}
	if tw := out.Twilio; tw != nil {
		apply(SecretFieldTwilioAuthToken, &tw.AuthToken)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

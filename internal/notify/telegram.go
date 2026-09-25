package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// DefaultTelegramBaseURL is the Telegram Bot API root.
const DefaultTelegramBaseURL = "https://api.telegram.org"

// telegramRequest is the sendMessage request body.
type telegramRequest struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode,omitempty"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

// telegramResponse is the relevant subset of a Bot API response.
type telegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

// TelegramNotifier sends messages through the Telegram Bot API.
type TelegramNotifier struct {
	cfg     TelegramConfig
	baseURL string
	client  *http.Client
}

// NewTelegramNotifier returns a Telegram notifier. baseURL overrides the API root (for
// tests); empty uses DefaultTelegramBaseURL. A nil client uses NewHTTPClient.
func NewTelegramNotifier(cfg TelegramConfig, baseURL string, client *http.Client) *TelegramNotifier {
	if baseURL == "" {
		baseURL = DefaultTelegramBaseURL
	}
	if client == nil {
		client = NewHTTPClient()
	}
	return &TelegramNotifier{cfg: cfg, baseURL: strings.TrimRight(baseURL, "/"), client: client}
}

// Type returns "telegram".
func (n *TelegramNotifier) Type() string { return string(ChannelTelegram) }

// Send posts msg to POST <base>/bot<token>/sendMessage.
func (n *TelegramNotifier) Send(ctx context.Context, msg Message) error {
	if err := validateTelegram(&n.cfg); err != nil {
		return err
	}
	body, err := json.Marshal(telegramRequest{
		ChatID:                n.cfg.ChatID,
		Text:                  truncate(telegramText(msg, n.cfg.ParseMode), 4000),
		ParseMode:             n.cfg.ParseMode,
		DisableWebPagePreview: true,
	})
	if err != nil {
		return fmt.Errorf("marshal telegram request: %w", err)
	}

	endpoint := n.baseURL + "/bot" + n.cfg.BotToken + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return scrub(sanitizeURLError("build telegram request", err), n.cfg.BotToken)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	respBody, err := doHTTP(ctx, n.client, req)
	if err != nil {
		return scrub(fmt.Errorf("telegram: %w", err), n.cfg.BotToken)
	}
	var tr telegramResponse
	if err := json.Unmarshal(respBody, &tr); err != nil || !tr.OK {
		return scrub(permanentf("telegram: api rejected message: %s", singleLine(tr.Description)), n.cfg.BotToken)
	}
	return nil
}

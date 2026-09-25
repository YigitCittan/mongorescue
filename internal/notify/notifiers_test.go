package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
)

const testBotToken = "123456:ABCdefGHIjklMNOpqrSTUvwxYZ012345"

// testTwilioSID is built at run time so the source never contains a literal
// that secret scanners mistake for a real Twilio account SID.
var testTwilioSID = "AC" + strings.Repeat("0f", 16)

func testMessage() Message {
	return Render(events.Event{
		Type:      events.BackupFailed,
		Time:      time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC),
		JobID:     "nightly-shop",
		BackupID:  "bkp_shop_1",
		Database:  "shop",
		Status:    "failed",
		Error:     "mongodump exited 1: mongodb://admin:pw@db/x",
		Duration:  1500 * time.Millisecond,
		SizeBytes: 2048,
	})
}

func TestRender(t *testing.T) {
	msg := testMessage()
	wantSubject := "❌ Backup failed: job nightly-shop (db shop)"
	if msg.Subject != wantSubject {
		t.Errorf("subject = %q; want %q", msg.Subject, wantSubject)
	}
	if !strings.HasPrefix(msg.Body, wantSubject+" — mongodump exited 1") || !strings.Contains(msg.Body, "— 2026-09-24T03:00Z") {
		t.Errorf("unexpected body %q", msg.Body)
	}
	if strings.Contains(msg.Body, ":pw@") {
		t.Errorf("body leaks credentials: %q", msg.Body)
	}

	manual := Render(events.Event{Type: events.BackupSucceeded, Database: "db\r\nBcc: x"})
	if strings.ContainsAny(manual.Subject, "\r\n") || !strings.Contains(manual.Subject, "job manual") {
		t.Errorf("subject must be single-line and mention the manual job: %q", manual.Subject)
	}
	if got := Render(events.Event{Type: events.RestoreSucceeded, BackupID: "b", Database: "t"}).Subject; got != "✅ Restore succeeded: backup b → db t" {
		t.Errorf("restore subject = %q", got)
	}
}

func TestEscapeMarkdownV2(t *testing.T) {
	in := `a_b*c[d]e(f)g~h` + "`" + `i>j#k+l-m=n|o{p}q.r!s\t`
	want := `a\_b\*c\[d\]e\(f\)g\~h` + "\\`" + `i\>j\#k\+l\-m\=n\|o\{p\}q\.r\!s\\t`
	if got := EscapeMarkdownV2(in); got != want {
		t.Errorf("EscapeMarkdownV2 = %q; want %q", got, want)
	}
}

func TestWebhookNotifier(t *testing.T) {
	const secret = "s3cr3t-hmac"
	var (
		mu  sync.Mutex
		got *http.Request
		raw []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, raw = r.Clone(context.Background()), body
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := NewWebhookNotifier(WebhookConfig{
		URL:     srv.URL + "/hook",
		Headers: map[string]string{"Authorization": "Bearer abc", "X-Custom": "1"},
		Secret:  secret,
	}, nil)
	if err := n.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got.Method != http.MethodPost || got.URL.Path != "/hook" {
		t.Errorf("unexpected request %s %s", got.Method, got.URL.Path)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if sig := got.Header.Get("X-MongoRescue-Signature"); sig != wantSig {
		t.Errorf("signature = %q; want %q", sig, wantSig)
	}
	if got.Header.Get("Authorization") != "Bearer abc" || got.Header.Get("X-Custom") != "1" {
		t.Error("custom headers not forwarded")
	}
	if got.Header.Get("X-MongoRescue-Event") != "backup.failed" || got.Header.Get("Content-Type") != "application/json" {
		t.Error("missing protocol headers")
	}

	var p WebhookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Version != 1 || p.Event != "backup.failed" || p.JobID != "nightly-shop" || p.Database != "shop" ||
		p.SizeBytes != 2048 || p.DurationSeconds != 1.5 || p.Subject == "" || p.Text == "" {
		t.Errorf("unexpected payload %+v", p)
	}
}

func TestWebhookStatusClassification(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantErr   bool
		permanent bool
	}{
		{"ok", http.StatusOK, false, false},
		{"server error retryable", http.StatusBadGateway, true, false},
		{"rate limited retryable", http.StatusTooManyRequests, true, false},
		{"client error permanent", http.StatusBadRequest, true, true},
		{"redirect not followed", http.StatusFound, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.status == http.StatusFound {
					w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte("nope"))
			}))
			defer srv.Close()

			err := NewWebhookNotifier(WebhookConfig{URL: srv.URL}, nil).Send(context.Background(), testMessage())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v; wantErr %v", err, tt.wantErr)
			}
			if err != nil && isPermanent(err) != tt.permanent {
				t.Fatalf("permanent = %v; want %v (%v)", isPermanent(err), tt.permanent, err)
			}
		})
	}
}

func TestTelegramNotifier(t *testing.T) {
	var (
		mu   sync.Mutex
		path string
		body telegramRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	tests := []struct {
		name      string
		parseMode string
		check     func(t *testing.T, text string)
	}{
		{"plain", ParseModePlain, func(t *testing.T, text string) {
			if !strings.HasPrefix(text, "❌ Backup failed: job nightly-shop (db shop)") {
				t.Errorf("unexpected text %q", text)
			}
		}},
		{"markdownv2", ParseModeMarkdownV2, func(t *testing.T, text string) {
			if !strings.HasPrefix(text, `*❌ Backup failed: job nightly\-shop \(db shop\)*`) || !strings.Contains(text, `2026\-09\-24T03:00Z`) {
				t.Errorf("unexpected text %q", text)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := NewTelegramNotifier(TelegramConfig{BotToken: testBotToken, ChatID: "-100123", ParseMode: tt.parseMode}, srv.URL, nil)
			if err := n.Send(context.Background(), testMessage()); err != nil {
				t.Fatalf("Send: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if path != "/bot"+testBotToken+"/sendMessage" {
				t.Errorf("path = %q", path)
			}
			if body.ChatID != "-100123" || body.ParseMode != tt.parseMode {
				t.Errorf("unexpected body %+v", body)
			}
			tt.check(t, body.Text)
		})
	}
}

func TestTelegramErrorsDoNotLeakToken(t *testing.T) {
	// API-level rejection.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"description":"chat not found"}`))
	}))
	err := NewTelegramNotifier(TelegramConfig{BotToken: testBotToken, ChatID: "1"}, srv.URL, nil).Send(context.Background(), testMessage())
	srv.Close()
	if err == nil || !errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("expected permanent API error, got %v", err)
	}

	// Transport error: net/http embeds the full URL (including the token) in the error.
	err = NewTelegramNotifier(TelegramConfig{BotToken: testBotToken, ChatID: "1"}, srv.URL, nil).Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("expected transport error against a closed server")
	}
	if strings.Contains(err.Error(), testBotToken) {
		t.Fatalf("error leaks bot token: %v", err)
	}
}

func TestTwilioNotifier(t *testing.T) {
	var (
		mu    sync.Mutex
		forms []url.Values
		calls atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != testTwilioSID || pass != "tw-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/2010-04-01/Accounts/"+testTwilioSID+"/Messages.json" ||
			r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = r.ParseForm()
		// The first request to the second recipient fails transiently.
		if r.PostForm.Get("To") == "+15550000002" && calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		mu.Lock()
		forms = append(forms, r.PostForm)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sid":"SM1"}`))
	}))
	defer srv.Close()

	n := NewTwilioNotifier(TwilioConfig{
		AccountSID: testTwilioSID, AuthToken: "tw-token", From: "+15551234567",
		To: []string{"+15550000001", "+15550000002"},
	}, srv.URL, nil)

	fast := RetryPolicy{Retries: 3, BaseBackoff: time.Millisecond, AttemptTimeout: time.Second}
	attempts, err := SendWithRetry(context.Background(), n, testMessage(), fast)
	if err != nil {
		t.Fatalf("SendWithRetry: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d; want 2", attempts)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(forms) != 2 {
		t.Fatalf("accepted messages = %d; want exactly 2 (no duplicate to the first recipient)", len(forms))
	}
	for _, f := range forms {
		if f.Get("From") != "+15551234567" || !strings.Contains(f.Get("Body"), "Backup failed") {
			t.Errorf("unexpected form %v", f)
		}
	}
}

func TestSendWithRetry(t *testing.T) {
	fast := RetryPolicy{Retries: 3, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, AttemptTimeout: time.Second}

	tests := []struct {
		name         string
		failures     int
		err          error
		wantAttempts int
		wantErr      bool
	}{
		{"first try", 0, nil, 1, false},
		{"succeeds on third", 2, errors.New("transient"), 3, false},
		{"exhausts budget", 10, errors.New("transient"), 4, true},
		{"permanent stops immediately", 10, permanentf("bad request"), 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := &fakeNotifier{failures: tt.failures, err: tt.err}
			attempts, err := SendWithRetry(context.Background(), n, Message{}, fast)
			if attempts != tt.wantAttempts || (err != nil) != tt.wantErr {
				t.Fatalf("attempts=%d err=%v; want %d, err=%v", attempts, err, tt.wantAttempts, tt.wantErr)
			}
		})
	}

	t.Run("context cancel interrupts backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		n := &fakeNotifier{failures: 10, err: errors.New("down"), onSend: cancel}
		slow := RetryPolicy{Retries: 3, BaseBackoff: time.Hour, AttemptTimeout: time.Second}
		start := time.Now()
		attempts, err := SendWithRetry(ctx, n, Message{}, slow)
		if !errors.Is(err, context.Canceled) || attempts != 1 {
			t.Fatalf("attempts=%d err=%v; want 1 attempt and context.Canceled", attempts, err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("backoff did not honour cancellation")
		}
	})

	t.Run("attempt timeout applied", func(t *testing.T) {
		n := &fakeNotifier{block: true}
		p := RetryPolicy{Retries: 0, AttemptTimeout: 20 * time.Millisecond}
		if _, err := SendWithRetry(context.Background(), n, Message{}, p); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v; want deadline exceeded", err)
		}
	})
}

// fakeNotifier fails the first `failures` sends with err.
type fakeNotifier struct {
	mu       sync.Mutex
	failures int
	err      error
	block    bool
	onSend   func()
	sent     []Message
	typ      string
}

func (f *fakeNotifier) Type() string {
	if f.typ == "" {
		return "fake"
	}
	return f.typ
}

func (f *fakeNotifier) Send(ctx context.Context, msg Message) error {
	if f.onSend != nil {
		f.onSend()
	}
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures > 0 {
		f.failures--
		return f.err
	}
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

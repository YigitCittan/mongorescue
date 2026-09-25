package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// EmailNotifier sends e-mail through an SMTP relay using net/smtp. It supports plain,
// STARTTLS and implicit-TLS connections and AUTH PLAIN (which net/smtp only permits
// over TLS or to localhost).
type EmailNotifier struct {
	cfg       EmailConfig
	tlsConfig *tls.Config
	now       func() time.Time
}

// NewEmailNotifier returns an e-mail notifier. tlsConfig may be nil (system roots,
// TLS 1.2+); when set it is cloned and ServerName is filled from the host if empty.
func NewEmailNotifier(cfg EmailConfig, tlsConfig *tls.Config) *EmailNotifier {
	var tc *tls.Config
	if tlsConfig != nil {
		tc = tlsConfig.Clone()
	} else {
		tc = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if tc.ServerName == "" {
		tc.ServerName = cfg.Host
	}
	return &EmailNotifier{cfg: cfg, tlsConfig: tc, now: time.Now}
}

// Type returns "email".
func (n *EmailNotifier) Type() string { return string(ChannelEmail) }

// Send delivers msg to every configured recipient in a single SMTP transaction.
func (n *EmailNotifier) Send(ctx context.Context, msg Message) error {
	if err := validateEmail(&n.cfg); err != nil {
		return err
	}
	raw, err := n.buildMessage(msg)
	if err != nil {
		return err
	}
	if err := n.deliver(ctx, raw); err != nil {
		return scrub(fmt.Errorf("smtp: %w", err), n.cfg.Password)
	}
	return nil
}

// buildMessage renders RFC 5322 headers and a quoted-printable UTF-8 body. Every
// header value is checked for CR/LF so event content cannot inject headers.
func (n *EmailNotifier) buildMessage(msg Message) ([]byte, error) {
	if hasCRLF(msg.Subject) {
		return nil, ErrHeaderInjection
	}
	from, err := parseAddress(n.cfg.From)
	if err != nil {
		return nil, fmt.Errorf("%w: from", ErrInvalidChannelConfig)
	}
	domain := "mongorescue.local"
	if at := strings.LastIndexByte(from, '@'); at != -1 && at < len(from)-1 {
		domain = from[at+1:]
	}

	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("generate message id: %w", err)
	}

	headers := [][2]string{
		{"From", n.cfg.From},
		{"To", strings.Join(n.cfg.To, ", ")},
		{"Subject", mime.QEncoding.Encode("utf-8", msg.Subject)},
		{"Date", n.now().UTC().Format(time.RFC1123Z)},
		{"Message-ID", "<" + hex.EncodeToString(id) + "@" + domain + ">"},
		{"MIME-Version", "1.0"},
		{"Content-Type", `text/plain; charset="utf-8"`},
		{"Content-Transfer-Encoding", "quoted-printable"},
		{"X-Mailer", userAgent},
	}

	var buf bytes.Buffer
	for _, h := range headers {
		if hasCRLF(h[1]) {
			return nil, fmt.Errorf("%w (%s)", ErrHeaderInjection, h[0])
		}
		buf.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	buf.WriteString("\r\n")

	qp := quotedprintable.NewWriter(&buf)
	body := strings.ReplaceAll(strings.ReplaceAll(msg.Body, "\r\n", "\n"), "\n", "\r\n")
	if _, err := qp.Write([]byte(body + "\r\n")); err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}
	return buf.Bytes(), nil
}

// deliver runs the SMTP dialogue, bounded by ctx: the connection deadline follows the
// context deadline and the connection is closed if ctx is cancelled mid-dialogue.
func (n *EmailNotifier) deliver(ctx context.Context, raw []byte) error {
	addr := net.JoinHostPort(n.cfg.Host, strconv.Itoa(n.cfg.Port))
	dialer := &net.Dialer{Timeout: SendTimeout}

	var (
		conn net.Conn
		err  error
	)
	if n.cfg.Security == SecurityTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: n.tlsConfig}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	deadline := time.Now().Add(SendTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline) // best effort; a failure surfaces on the next I/O call
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, n.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("handshake: %w", ctxErr(ctx, err))
	}
	defer func() {
		// Close is a no-op after a successful Quit.
		_ = c.Close()
	}()

	if n.cfg.Security == SecuritySTARTTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return permanentf("server does not support STARTTLS")
		}
		if err = c.StartTLS(n.tlsConfig); err != nil {
			return fmt.Errorf("starttls: %w", ctxErr(ctx, err))
		}
	}

	if n.cfg.Username != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return permanentf("server does not support AUTH")
		}
		auth := smtp.PlainAuth("", n.cfg.Username, n.cfg.Password, n.cfg.Host)
		if err = c.Auth(auth); err != nil {
			return permanentf("auth failed: %v", ctxErr(ctx, err))
		}
	}

	from, _ := parseAddress(n.cfg.From) // validated in Send
	if err = c.Mail(from); err != nil {
		return fmt.Errorf("mail from: %w", ctxErr(ctx, err))
	}
	for _, to := range n.cfg.To {
		rcpt, _ := parseAddress(to) // validated in Send
		if err = c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("rcpt to: %w", ctxErr(ctx, err))
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", ctxErr(ctx, err))
	}
	if _, err = w.Write(raw); err != nil {
		_ = w.Close()
		return fmt.Errorf("write body: %w", ctxErr(ctx, err))
	}
	if err = w.Close(); err != nil {
		return fmt.Errorf("finish data: %w", ctxErr(ctx, err))
	}
	if err = c.Quit(); err != nil {
		return fmt.Errorf("quit: %w", ctxErr(ctx, err))
	}
	return nil
}

// ctxErr prefers the context error when the context ended the dialogue.
func ctxErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
		return fmt.Errorf("%w: %w", cerr, err)
	}
	return err
}

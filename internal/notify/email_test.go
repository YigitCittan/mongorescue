package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// smtpMessage is a message captured by fakeSMTP.
type smtpMessage struct {
	from   string
	to     []string
	data   string
	auth   string
	secure bool
}

// fakeSMTP is a minimal in-process SMTP server supporting EHLO, STARTTLS, AUTH PLAIN,
// MAIL, RCPT, DATA and QUIT.
type fakeSMTP struct {
	ln          net.Listener
	serverTLS   *tls.Config
	implicitTLS bool
	offerTLS    bool

	wg   sync.WaitGroup
	mu   sync.Mutex
	msgs []smtpMessage
}

// testTLSConfigs returns a server certificate for 127.0.0.1 and a client config
// trusting it, borrowed from httptest's built-in test certificate.
func testTLSConfigs(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	defer ts.Close()
	server = &tls.Config{Certificates: ts.TLS.Certificates, MinVersion: tls.VersionTLS12}
	client = ts.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return server, client
}

func startFakeSMTP(t *testing.T, mode string) (*fakeSMTP, int, *tls.Config) {
	t.Helper()
	serverTLS, clientTLS := testTLSConfigs(t)
	f := &fakeSMTP{serverTLS: serverTLS, implicitTLS: mode == SecurityTLS, offerTLS: mode == SecuritySTARTTLS}

	var err error
	if f.implicitTLS {
		f.ln, err = tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	} else {
		f.ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f.wg.Add(1)
	go f.acceptLoop()
	t.Cleanup(func() {
		_ = f.ln.Close()
		f.wg.Wait()
	})
	return f, f.ln.Addr().(*net.TCPAddr).Port, clientTLS
}

func (f *fakeSMTP) acceptLoop() {
	defer f.wg.Done()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.serve(conn)
		}()
	}
}

func (f *fakeSMTP) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	tp := textproto.NewConn(conn)
	secure := f.implicitTLS
	var cur smtpMessage
	_ = tp.PrintfLine("220 fake.local ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			lines := []string{"fake.local"}
			if f.offerTLS && !secure {
				lines = append(lines, "STARTTLS")
			}
			lines = append(lines, "AUTH PLAIN", "8BITMIME")
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				_ = tp.PrintfLine("250%s%s", sep, l)
			}
		case upper == "STARTTLS":
			_ = tp.PrintfLine("220 ready")
			tconn := tls.Server(conn, f.serverTLS)
			if err := tconn.Handshake(); err != nil {
				return
			}
			conn = tconn
			tp = textproto.NewConn(tconn)
			secure = true
		case strings.HasPrefix(upper, "AUTH PLAIN "):
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len("AUTH PLAIN "):]))
			cur.auth = string(raw)
			_ = tp.PrintfLine("235 2.7.0 authenticated")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			cur.from = strings.Trim(line[len("MAIL FROM:"):], "<> ")
			if i := strings.Index(cur.from, ">"); i != -1 {
				cur.from = cur.from[:i]
			}
			_ = tp.PrintfLine("250 ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			cur.to = append(cur.to, strings.Trim(line[len("RCPT TO:"):], "<> "))
			_ = tp.PrintfLine("250 ok")
		case upper == "DATA":
			_ = tp.PrintfLine("354 go ahead")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			cur.data = string(data)
			cur.secure = secure
			f.mu.Lock()
			f.msgs = append(f.msgs, cur)
			f.mu.Unlock()
			cur = smtpMessage{}
			_ = tp.PrintfLine("250 queued")
		case upper == "QUIT":
			_ = tp.PrintfLine("221 bye")
			return
		case upper == "RSET", upper == "NOOP":
			_ = tp.PrintfLine("250 ok")
		default:
			_ = tp.PrintfLine("502 unsupported")
		}
	}
}

func (f *fakeSMTP) messages() []smtpMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]smtpMessage(nil), f.msgs...)
}

func TestEmailNotifier(t *testing.T) {
	tests := []struct {
		mode     string
		username string
	}{
		{SecurityNone, ""},
		{SecurityNone, "ops"},
		{SecuritySTARTTLS, "ops"},
		{SecurityTLS, "ops"},
	}
	for _, tt := range tests {
		t.Run(tt.mode+"/"+tt.username, func(t *testing.T) {
			srv, port, clientTLS := startFakeSMTP(t, tt.mode)
			cfg := EmailConfig{
				Host: "127.0.0.1", Port: port, Security: tt.mode,
				From: "MongoRescue <alerts@example.com>",
				To:   []string{"oncall@example.com", "dba@example.com"},
			}
			if tt.username != "" {
				cfg.Username, cfg.Password = tt.username, "smtp-pass"
			}
			n := NewEmailNotifier(cfg, clientTLS)
			if err := n.Send(context.Background(), testMessage()); err != nil {
				t.Fatalf("Send: %v", err)
			}

			msgs := srv.messages()
			if len(msgs) != 1 {
				t.Fatalf("messages = %d; want 1", len(msgs))
			}
			m := msgs[0]
			if m.from != "alerts@example.com" || strings.Join(m.to, ",") != "oncall@example.com,dba@example.com" {
				t.Errorf("unexpected envelope from=%q to=%v", m.from, m.to)
			}
			if wantSecure := tt.mode != SecurityNone; m.secure != wantSecure {
				t.Errorf("secure = %v; want %v", m.secure, wantSecure)
			}
			if tt.username != "" && m.auth != "\x00ops\x00smtp-pass" {
				t.Errorf("auth = %q", m.auth)
			}

			// textproto.ReadDotBytes normalises CRLF to LF.
			headers, qpBody, _ := strings.Cut(m.data, "\n\n")
			decoded, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(qpBody)))
			body := string(decoded)
			for _, h := range []string{"From: ", "To: ", "Subject: ", "Date: ", "Message-ID: <", "MIME-Version: 1.0", "Content-Transfer-Encoding: quoted-printable"} {
				if !strings.Contains(headers, h) {
					t.Errorf("missing header %q in %q", h, headers)
				}
			}
			if !strings.Contains(headers, "@example.com>") {
				t.Errorf("Message-ID should use the sender domain: %q", headers)
			}
			var subject string
			for _, l := range strings.Split(headers, "\n") {
				if strings.HasPrefix(l, "Subject: ") {
					subject, _ = new(mime.WordDecoder).DecodeHeader(strings.TrimPrefix(l, "Subject: "))
				}
			}
			if subject != "❌ Backup failed: job nightly-shop (db shop)" {
				t.Errorf("decoded subject = %q", subject)
			}
			if !strings.Contains(body, "Backup ID: bkp_shop_1") {
				t.Errorf("unexpected body %q", body)
			}
		})
	}
}

func TestEmailHeaderInjectionRejected(t *testing.T) {
	base := EmailConfig{Host: "127.0.0.1", Port: 25, Security: SecurityNone, From: "a@example.com", To: []string{"b@example.com"}}

	tests := []struct {
		name   string
		mutate func(*EmailConfig)
	}{
		{"from", func(c *EmailConfig) { c.From = "a@example.com\r\nBcc: victim@example.com" }},
		{"to", func(c *EmailConfig) { c.To = []string{"b@example.com\nBcc: victim@example.com"} }},
		{"host", func(c *EmailConfig) { c.Host = "smtp.example.com\r\nX: y" }},
		{"username", func(c *EmailConfig) { c.Username = "u\r\n" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.To = append([]string(nil), base.To...)
			tt.mutate(&cfg)
			err := validateEmail(&cfg)
			if !errors.Is(err, ErrHeaderInjection) || !errors.Is(err, ErrInvalidChannelConfig) {
				t.Fatalf("validateEmail = %v; want ErrHeaderInjection", err)
			}
			if err := NewEmailNotifier(cfg, nil).Send(context.Background(), testMessage()); !errors.Is(err, ErrHeaderInjection) {
				t.Fatalf("Send = %v; want ErrHeaderInjection", err)
			}
		})
	}

	t.Run("subject", func(t *testing.T) {
		n := NewEmailNotifier(base, nil)
		if _, err := n.buildMessage(Message{Subject: "hi\r\nBcc: x@example.com", Body: "b"}); !errors.Is(err, ErrHeaderInjection) {
			t.Fatalf("buildMessage = %v; want ErrHeaderInjection", err)
		}
	})
}

func TestEmailContextCancel(t *testing.T) {
	// A listener that accepts but never speaks: the dialogue must end with ctx.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 1)
		_, _ = conn.Read(buf) // blocks until the client closes
		_ = conn.Close()
	}()
	defer func() {
		_ = ln.Close()
		wg.Wait()
	}()

	cfg := EmailConfig{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Security: SecurityNone, From: "a@example.com", To: []string{"b@example.com"}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := NewEmailNotifier(cfg, nil).Send(ctx, testMessage()); err == nil {
		t.Fatal("expected error from silent server")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("send did not honour the context deadline")
	}
}

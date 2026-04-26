package notifier

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// EmailConfig holds SMTP connection and message settings for email notifications.
type EmailConfig struct {
	Host      string
	Port      int
	Username  string
	Password  string
	From      string
	To        []string
	UseTLS    bool // STARTTLS on an initially plain connection
	UseSSL    bool // implicit TLS (SMTPS), typically port 465
	EventMask []EventType
}

// EmailNotifier sends notifications via SMTP.
type EmailNotifier struct {
	cfg EmailConfig
}

// NewEmailNotifier constructs an EmailNotifier from cfg.
func NewEmailNotifier(cfg EmailConfig) *EmailNotifier {
	return &EmailNotifier{cfg: cfg}
}

// Name returns the notifier identifier.
func (e *EmailNotifier) Name() string { return "email" }

// Accepts reports whether this notifier is configured to handle t.
func (e *EmailNotifier) Accepts(t EventType) bool {
	return acceptsAny(e.cfg.EventMask, t)
}

// Send delivers an email for ev. The caller's context is respected for dial
// timeouts where the smtp package propagates it; TLS dial uses the deadline
// derived from the context if set.
func (e *EmailNotifier) Send(ctx context.Context, ev Event) error {
	msg := e.FormatMessage(ev)
	addr := fmt.Sprintf("%s:%d", e.cfg.Host, e.cfg.Port)

	var auth smtp.Auth
	if e.cfg.Username != "" {
		auth = smtp.PlainAuth("", e.cfg.Username, e.cfg.Password, e.cfg.Host)
	}

	if e.cfg.UseSSL {
		return e.sendImplicitTLS(ctx, addr, auth, msg)
	}
	return e.sendPlainOrSTARTTLS(ctx, addr, auth, msg)
}

func (e *EmailNotifier) sendImplicitTLS(ctx context.Context, addr string, auth smtp.Auth, msg []byte) error {
	//nolint:gosec // G402: InsecureSkipVerify intentionally false; no flag exposed yet
	tlsCfg := &tls.Config{ServerName: e.cfg.Host, InsecureSkipVerify: false}
	dialer := &tls.Dialer{NetDialer: &net.Dialer{}, Config: tlsCfg}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("email: tls dial %s: %w", addr, err)
	}
	c, err := smtp.NewClient(conn, e.cfg.Host)
	if err != nil {
		return fmt.Errorf("email: smtp client on tls conn: %w", err)
	}
	return e.finishSend(c, auth, msg)
}

func (e *EmailNotifier) sendPlainOrSTARTTLS(ctx context.Context, addr string, auth smtp.Auth, msg []byte) error {
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("email: smtp dial %s: %w", addr, err)
	}
	c, err := smtp.NewClient(conn, e.cfg.Host)
	if err != nil {
		return fmt.Errorf("email: smtp client on plain conn: %w", err)
	}
	if e.cfg.UseTLS {
		//nolint:gosec // G402: InsecureSkipVerify intentionally false; no flag exposed yet
		tlsCfg := &tls.Config{ServerName: e.cfg.Host, InsecureSkipVerify: false}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("email: starttls: %w", err)
		}
	}
	return e.finishSend(c, auth, msg)
}

func (e *EmailNotifier) finishSend(c *smtp.Client, auth smtp.Auth, msg []byte) error {
	defer func() {
		//nolint:errcheck // best-effort close; connection already done at this point
		_ = c.Quit()
	}()

	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("email: smtp auth: %w", err)
		}
	}
	if err := c.Mail(e.cfg.From); err != nil {
		return fmt.Errorf("email: MAIL FROM: %w", err)
	}
	for _, to := range e.cfg.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("email: RCPT TO %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("email: DATA command: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("email: write message body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email: close DATA writer: %w", err)
	}
	return nil
}

// FormatMessage builds a minimal RFC 822 message. Exported so tests can
// verify message structure without requiring a live SMTP server.
func (e *EmailNotifier) FormatMessage(ev Event) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", e.cfg.From)
	fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(e.cfg.To, ", "))
	fmt.Fprintf(&buf, "Subject: SABnzbd: %s\r\n", ev.Title)
	fmt.Fprintf(&buf, "Date: %s\r\n", ev.Timestamp.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&buf, "\r\n%s", ev.Body)
	return buf.Bytes()
}

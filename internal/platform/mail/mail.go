// Package mail provides email senders. It is platform infrastructure and does
// not import any domain: it defines its own neutral Message type. The auth
// domain declares its own Mailer interface; the composition root adapts a Sender
// here to that interface (keeping platform domain-free per BACKEND.md §2).
package mail

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Message is a plain-text email.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Sender delivers a Message.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// SMTPSender sends over SMTP. It works against Mailpit in dev (no auth, port
// 1025) and any real SMTP server in production. Credentials are optional: when
// Username is empty no SMTP AUTH is attempted (Mailpit's mode).
type SMTPSender struct {
	Addr     string // host:port
	Host     string // host alone, for AUTH
	Username string
	Password string
	From     string
}

// NewSMTPSender builds an SMTPSender.
func NewSMTPSender(host string, port int, username, password, from string) *SMTPSender {
	return &SMTPSender{
		Addr:     net.JoinHostPort(host, strconv.Itoa(port)),
		Host:     host,
		Username: username,
		Password: password,
		From:     from,
	}
}

// Send delivers msg. ctx is honored best-effort (net/smtp has no context hook):
// an already-cancelled context short-circuits before dialing.
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var auth smtp.Auth
	if s.Username != "" {
		auth = smtp.PlainAuth("", s.Username, s.Password, s.Host)
	}
	if err := smtp.SendMail(s.Addr, auth, s.From, []string{msg.To}, s.compose(msg)); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}

// compose renders a minimal RFC 5322 plain-text message.
func (s *SMTPSender) compose(msg Message) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", s.From)
	fmt.Fprintf(&b, "To: %s\r\n", msg.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", msg.Subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	// Normalize line endings to CRLF for the DATA section.
	b.WriteString(strings.ReplaceAll(msg.Body, "\n", "\r\n"))
	return []byte(b.String())
}

// LogSender does not send anything; it logs that a message WOULD have been
// sent — recipient and subject only, never the body. The body carries the
// verification/reset link, i.e. a live credential, and "no SMTP configured"
// is not a promise of a developer laptop: it is exactly the state of a first
// deploy. A developer who needs the link locally should point SMTPHost at a
// capture tool (mailpit et al.) instead of fishing credentials out of stdout.
type LogSender struct {
	log *slog.Logger
}

// NewLogSender builds a LogSender.
func NewLogSender(log *slog.Logger) *LogSender {
	return &LogSender{log: log}
}

// Send logs the message envelope. The body is deliberately withheld: it
// contains a live verification/reset token, and process logs outlive any
// intention anyone had for them.
func (s *LogSender) Send(ctx context.Context, msg Message) error {
	s.log.InfoContext(ctx, "email (not sent: no SMTP configured)",
		"to", msg.To, "subject", msg.Subject, "bodyBytes", len(msg.Body))
	return nil
}

package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// defaultSMTPTimeout bounds the whole SMTP conversation (dial through
// QUIT) when ctx carries no deadline of its own — without this, an
// unreachable or misconfigured host can hang net/smtp indefinitely, and
// RunDueDeliveries processes deliveries one at a time, so a single hung
// send stalls every other channel's delivery behind it.
const defaultSMTPTimeout = 30 * time.Second

// mailSendFunc is the seam EmailSender.SendMail takes so tests never dial
// a real SMTP server. It takes ctx (unlike smtp.SendMail's own signature)
// because bounding the conversation on ctx's deadline is the point of
// this type existing rather than calling smtp.SendMail directly.
//
// net/smtp already negotiates STARTTLS opportunistically whenever the
// server advertises it, before attempting AUTH, so no separate code path
// is needed for EmailConfig.EmailStartTLS; it exists on the config only
// as a UI-facing statement of intent (doc 03 §8.3) — the client behaves
// the same whether or not it is set, since it already refuses to send an
// AUTH command over a connection the server didn't just upgrade or that
// wasn't already encrypted.
type mailSendFunc func(ctx context.Context, addr string, auth smtp.Auth, from string, to []string, msg []byte) error

// EmailSender delivers a Message over SMTP (doc 03 §8.3's email channel).
type EmailSender struct {
	// SendMail defaults to defaultSendMail bound to Timeout. Tests
	// replace it with a fake that records the call instead of opening a
	// real connection (CLAUDE.md: a system-touching subsystem behind an
	// interface with a scriptable fake).
	SendMail mailSendFunc

	// Timeout bounds a real defaultSendMail conversation when ctx
	// carries no deadline of its own. Zero means defaultSMTPTimeout; a
	// test exercising the real dial path against an unresponsive peer
	// lowers it instead of waiting out the production default.
	Timeout time.Duration
}

var _ Sender = (*EmailSender)(nil)

func (e *EmailSender) sendMail() mailSendFunc {
	if e.SendMail != nil {
		return e.SendMail
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultSMTPTimeout
	}
	return func(ctx context.Context, addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
		return defaultSendMail(ctx, timeout, addr, auth, from, to, msg)
	}
}

func (e *EmailSender) Send(ctx context.Context, cfg ChannelConfig, secret string, msg Message) error {
	if cfg.EmailHost == "" {
		return fmt.Errorf("email channel has no host configured")
	}
	if cfg.EmailFrom == "" {
		return fmt.Errorf("email channel has no from address configured")
	}
	if len(cfg.EmailTo) == 0 {
		return fmt.Errorf("email channel has no recipients configured")
	}

	addr := fmt.Sprintf("%s:%d", cfg.EmailHost, cfg.EmailPort)
	var auth smtp.Auth
	if cfg.EmailUsername != "" {
		auth = smtp.PlainAuth("", cfg.EmailUsername, secret, cfg.EmailHost)
	}

	if err := e.sendMail()(ctx, addr, auth, cfg.EmailFrom, cfg.EmailTo, buildEmailMessage(cfg, msg)); err != nil {
		return fmt.Errorf("sending email via %s: %w", addr, err)
	}
	return nil
}

// defaultSendMail is smtp.SendMail's own conversation (dial, optional
// STARTTLS, optional AUTH, MAIL/RCPT/DATA, QUIT), rebuilt over a
// connection this function dials and deadlines itself: smtp.SendMail
// takes no context and imposes no deadline of its own, so a peer that
// accepts the connection and never responds hangs it forever.
func defaultSendMail(ctx context.Context, timeout time.Duration, addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	if err := validateSMTPLine(from); err != nil {
		return err
	}
	for _, recipient := range to {
		if err := validateSMTPLine(recipient); err != nil {
			return err
		}
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if auth != nil {
		if ok, _ := client.Extension("AUTH"); !ok {
			return errors.New("smtp: server doesn't support AUTH")
		}
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// validateSMTPLine rejects a from/to address carrying a CR or LF, the
// same header-injection guard smtp.SendMail applies before dialing.
func validateSMTPLine(line string) error {
	if strings.ContainsAny(line, "\r\n") {
		return errors.New("smtp: a line must not contain CR or LF")
	}
	return nil
}

// buildEmailMessage builds a minimal, valid RFC 5322 message: headers,
// a blank line, then the plain-text body.
func buildEmailMessage(cfg ChannelConfig, msg Message) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", cfg.EmailFrom)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(cfg.EmailTo, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", msg.Title)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	b.WriteString("\r\n")
	b.WriteString(msg.Body)
	return []byte(b.String())
}

package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

const smtpTimeout = 15 * time.Second

// SMTPConfig configures an SMTP sender.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	TLSMode  string
}

type smtpSession interface {
	Extension(string) (bool, string)
	StartTLS(*tls.Config) error
	Auth(smtp.Auth) error
	Mail(string) error
	Rcpt(string) error
	Data() (io.WriteCloser, error)
	Quit() error
	Close() error
}

type smtpDialFunc func(context.Context, string, string, bool, *tls.Config, time.Time) (smtpSession, error)

// SMTPSender delivers messages through one SMTP relay.
type SMTPSender struct {
	config    SMTPConfig
	dial      smtpDialFunc
	now       func() time.Time
	messageID func(string, time.Time) string
}

// NewSMTPSender validates config and returns an SMTP sender.
func NewSMTPSender(config SMTPConfig) (*SMTPSender, error) {
	config.Host = strings.TrimSpace(config.Host)
	config.Username = strings.TrimSpace(config.Username)
	config.TLSMode = strings.ToLower(strings.TrimSpace(config.TLSMode))
	if config.Host == "" {
		return nil, fmt.Errorf("SMTP host is required")
	}
	if strings.ContainsAny(config.Host, "\r\n") || strings.ContainsAny(config.Username, "\r\n") {
		return nil, fmt.Errorf("SMTP host and username must not contain newlines")
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, fmt.Errorf("SMTP port must be between 1 and 65535")
	}
	switch config.TLSMode {
	case "starttls", "tls", "none":
	default:
		return nil, fmt.Errorf("unsupported SMTP TLS mode %q", config.TLSMode)
	}
	if config.TLSMode == "none" && config.Username != "" {
		return nil, fmt.Errorf("SMTP authentication requires TLS")
	}
	if config.Username != "" && config.Password == "" {
		return nil, fmt.Errorf("SMTP password is required when a username is configured")
	}
	return &SMTPSender{
		config:    config,
		dial:      dialSMTP,
		now:       time.Now,
		messageID: newMessageID,
	}, nil
}

// Send delivers one message through the configured SMTP relay.
func (s *SMTPSender) Send(ctx context.Context, message Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if message.From.Address == "" {
		return fmt.Errorf("email sender address is required")
	}
	if len(message.To) == 0 {
		return fmt.Errorf("at least one email recipient is required")
	}

	now := s.now().UTC()
	data, err := encodeMessage(message, now, s.messageID(s.config.Host, now))
	if err != nil {
		return err
	}
	deadline := now.Add(smtpTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: s.config.Host}
	address := net.JoinHostPort(s.config.Host, fmt.Sprintf("%d", s.config.Port))
	client, err := s.dial(ctx, address, s.config.Host, s.config.TLSMode == "tls", tlsConfig, deadline)
	if err != nil {
		return fmt.Errorf("connecting to SMTP host %s: %w", s.config.Host, err)
	}
	defer client.Close()

	if s.config.TLSMode == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("SMTP host %s does not advertise STARTTLS", s.config.Host)
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("starting SMTP TLS with %s: %w", s.config.Host, err)
		}
	}
	if s.config.Username != "" {
		auth := smtp.PlainAuth("", s.config.Username, s.config.Password, s.config.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("authenticating to SMTP host %s: %w", s.config.Host, err)
		}
	}

	if err := client.Mail(message.From.Address); err != nil {
		return fmt.Errorf("setting SMTP sender on %s: %w", s.config.Host, err)
	}
	for _, recipient := range message.To {
		if recipient.Address == "" {
			return fmt.Errorf("email recipient address is required")
		}
		if err := client.Rcpt(recipient.Address); err != nil {
			return fmt.Errorf("setting SMTP recipient on %s: %w", s.config.Host, err)
		}
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("opening SMTP message on %s: %w", s.config.Host, err)
	}
	if _, err := writer.Write(data); err != nil {
		writer.Close()
		return fmt.Errorf("writing SMTP message to %s: %w", s.config.Host, err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("closing SMTP message on %s: %w", s.config.Host, err)
	}
	// DATA close already received the server's 250 acceptance response. QUIT is
	// best-effort so a teardown failure cannot cause an accepted email to retry.
	_ = client.Quit()
	return nil
}

func dialSMTP(ctx context.Context, address, host string, implicitTLS bool, tlsConfig *tls.Config, deadline time.Time) (smtpSession, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, err
	}
	if implicitTLS {
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tlsConn
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return client, nil
}

func encodeMessage(message Message, date time.Time, messageID string) ([]byte, error) {
	if strings.ContainsAny(message.Subject, "\r\n") {
		return nil, fmt.Errorf("email subject contains a newline")
	}
	for name, value := range map[string]string{
		"from":       message.From.String(),
		"message id": messageID,
	} {
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("email %s contains a newline", name)
		}
	}
	for _, recipient := range message.To {
		if strings.ContainsAny(recipient.String(), "\r\n") {
			return nil, fmt.Errorf("email recipient contains a newline")
		}
	}
	var b bytes.Buffer
	multipartWriter := multipart.NewWriter(&b)
	writeHeader := func(name, value string) {
		fmt.Fprintf(&b, "%s: %s\r\n", name, value)
	}
	writeHeader("Date", date.Format(time.RFC1123Z))
	writeHeader("Message-ID", messageID)
	writeHeader("From", message.From.String())
	recipients := make([]string, 0, len(message.To))
	for _, recipient := range message.To {
		recipients = append(recipients, recipient.String())
	}
	writeHeader("To", strings.Join(recipients, ", "))
	writeHeader("Subject", mime.QEncoding.Encode("utf-8", message.Subject))
	writeHeader("MIME-Version", "1.0")
	writeHeader("Content-Type", fmt.Sprintf("multipart/alternative; boundary=%q", multipartWriter.Boundary()))
	b.WriteString("\r\n")

	if err := writeQuotedPrintablePart(multipartWriter, "text/plain; charset=utf-8", message.TextBody); err != nil {
		return nil, err
	}
	if err := writeQuotedPrintablePart(multipartWriter, "text/html; charset=utf-8", message.HTMLBody); err != nil {
		return nil, err
	}
	if err := multipartWriter.Close(); err != nil {
		return nil, fmt.Errorf("closing MIME message: %w", err)
	}
	return b.Bytes(), nil
}

func writeQuotedPrintablePart(writer *multipart.Writer, contentType, content string) error {
	header := make(textproto.MIMEHeader)
	header.Set("Content-Type", contentType)
	header.Set("Content-Transfer-Encoding", "quoted-printable")
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("creating MIME part: %w", err)
	}
	qp := quotedprintable.NewWriter(part)
	if _, err := io.WriteString(qp, content); err != nil {
		qp.Close()
		return fmt.Errorf("writing MIME part: %w", err)
	}
	if err := qp.Close(); err != nil {
		return fmt.Errorf("closing MIME part: %w", err)
	}
	return nil
}

func newMessageID(host string, now time.Time) string {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Sprintf("<%d@%s>", now.UnixNano(), host)
	}
	return fmt.Sprintf("<%d.%s@%s>", now.UnixNano(), hex.EncodeToString(random[:]), host)
}

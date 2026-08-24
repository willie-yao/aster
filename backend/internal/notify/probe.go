package notify

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"net/mail"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/project"
)

var probeHTMLTemplate = template.Must(template.New("probe").Parse(`<!doctype html>
<html>
<body>
  <h2>Aster email delivery probe</h2>
  <p><strong>Project:</strong> {{.Project}}</p>
  <p>Sent at {{.Sent}}. The relay accepted this message, so alerts can reach this address.</p>
</body>
</html>`))

// ProbeOptions configures a one-off delivery check.
type ProbeOptions struct {
	// Config is the loaded consumer configuration.
	Config *project.Config
	// Password is the EMAIL_SMTP_PASSWORD value. Required when the relay
	// configures a username.
	Password string
	// To overrides the configured recipients so an operator can probe without
	// mailing the whole distribution list.
	To []string
}

// Probe sends one test email through the consumer's configured relay and
// reports each step to out. It exercises the same sender the fetcher uses, so a
// success proves the relay, credentials, TLS mode, and network path all work.
func Probe(ctx context.Context, opts ProbeOptions, out io.Writer) error {
	email, enabled := opts.Config.EffectiveEmailNotifications()
	if !enabled {
		return fmt.Errorf("notifications.email.enabled is false in project.yaml")
	}
	if email.SMTP.Username != "" && opts.Password == "" {
		return fmt.Errorf("EMAIL_SMTP_PASSWORD is unset but smtp.username is %q", email.SMTP.Username)
	}

	recipients := email.To
	if len(opts.To) > 0 {
		recipients = opts.To
	}
	from, to, err := ParseAddresses(email.From, recipients)
	if err != nil {
		return fmt.Errorf("email addresses: %w", err)
	}
	fmt.Fprintf(out, "relay:      %s:%d (tls=%s)\n", email.SMTP.Host, email.SMTP.Port, email.SMTP.TLS)
	fmt.Fprintf(out, "auth:       %s\n", authDescription(email.SMTP.Username))
	fmt.Fprintf(out, "from:       %s\n", from.String())
	fmt.Fprintf(out, "to:         %s\n", joinAddresses(to))

	sender, err := NewSMTPSender(SMTPConfig{
		Host:     email.SMTP.Host,
		Port:     email.SMTP.Port,
		Username: email.SMTP.Username,
		Password: opts.Password,
		TLSMode:  email.SMTP.TLS,
	})
	if err != nil {
		return fmt.Errorf("email config: %w", err)
	}

	sent := time.Now().UTC().Format(time.RFC3339)
	message, err := probeMessage(opts.Config.Name, from, to, sent)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "sending...")
	if err := sender.Send(ctx, message); err != nil {
		return fmt.Errorf("sending test email: %w", err)
	}
	fmt.Fprintf(out, "delivered:  the relay accepted the message at %s\n", sent)
	return nil
}

func probeMessage(projectName string, from mail.Address, to []mail.Address, sent string) (Message, error) {
	var html bytes.Buffer
	if err := probeHTMLTemplate.Execute(&html, struct{ Project, Sent string }{projectName, sent}); err != nil {
		return Message{}, fmt.Errorf("rendering test email: %w", err)
	}
	return Message{
		From:     from,
		To:       to,
		Subject:  notificationSubject(projectName, "Test", "email delivery probe"),
		TextBody: probeText(projectName, sent),
		HTMLBody: html.String(),
	}, nil
}

func probeText(projectName, sent string) string {
	return fmt.Sprintf("Aster email delivery probe\n\nProject: %s\nSent: %s\n\nThe relay accepted this message, so alerts can reach this address.\n", projectName, sent)
}

func authDescription(username string) string {
	if username == "" {
		return "none (unauthenticated relay)"
	}
	return username + " (password from EMAIL_SMTP_PASSWORD)"
}

func joinAddresses(addresses []mail.Address) string {
	rendered := make([]string, 0, len(addresses))
	for _, address := range addresses {
		rendered = append(rendered, address.String())
	}
	return strings.Join(rendered, ", ")
}

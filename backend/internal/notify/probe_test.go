package notify

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/project"
)

// probeConfig points at a closed local port so send attempts fail immediately
// without DNS or outbound network access.
func probeConfig(username string) *project.Config {
	return &project.Config{
		Name: "Example",
		Notifications: &project.Notifications{
			Email: &project.EmailNotifications{
				Enabled: true,
				From:    "Aster <aster@example.com>",
				To:      []string{"team@example.com"},
				SMTP:    project.EmailSMTP{Host: "127.0.0.1", Port: 1, Username: username, TLS: "starttls"},
			},
		},
	}
}

func TestProbeRequiresEnabledEmail(t *testing.T) {
	cfg := probeConfig("")
	cfg.Notifications.Email.Enabled = false
	err := Probe(context.Background(), ProbeOptions{Config: cfg}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "notifications.email.enabled") {
		t.Fatalf("err = %v", err)
	}
}

func TestProbeRequiresPasswordWhenAuthenticated(t *testing.T) {
	err := Probe(context.Background(), ProbeOptions{Config: probeConfig("relay@example.com")}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "EMAIL_SMTP_PASSWORD is unset") {
		t.Fatalf("err = %v", err)
	}
}

func TestProbeReportsRelayAndRecipientOverride(t *testing.T) {
	var out bytes.Buffer
	// The closed port makes the send fail after the summary is written, which
	// is what this asserts on.
	err := Probe(context.Background(), ProbeOptions{Config: probeConfig(""), To: []string{"oncall@example.com"}}, &out)
	if err == nil {
		t.Fatal("expected the send to fail against a closed relay port")
	}
	report := out.String()
	for _, want := range []string{"127.0.0.1:1", "tls=starttls", "none (unauthenticated relay)", "oncall@example.com"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q: %s", want, report)
		}
	}
	if strings.Contains(report, "team@example.com") {
		t.Errorf("override did not replace the configured recipients: %s", report)
	}
}

func TestProbeEscapesProjectNameInHTML(t *testing.T) {
	from, to, err := ParseAddresses("Aster <aster@example.com>", []string{"team@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := probeMessage("CAPZ <production> & staging", from, to, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(message.HTMLBody, "<production>") {
		t.Errorf("project name was not escaped: %s", message.HTMLBody)
	}
	if !strings.Contains(message.HTMLBody, "&lt;production&gt;") {
		t.Errorf("escaped project name missing: %s", message.HTMLBody)
	}
	if !strings.Contains(message.TextBody, "CAPZ <production> & staging") {
		t.Errorf("text body missing project name: %s", message.TextBody)
	}
}

func TestProbeRejectsInvalidAddress(t *testing.T) {
	err := Probe(context.Background(), ProbeOptions{Config: probeConfig(""), To: []string{"not-an-address"}}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "email addresses") {
		t.Fatalf("err = %v", err)
	}
}

func TestProbeStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Probe(ctx, ProbeOptions{Config: probeConfig("")}, &bytes.Buffer{})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

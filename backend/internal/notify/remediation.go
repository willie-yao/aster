package notify

import (
	"fmt"
	"html"
	"net/mail"
	"strings"
)

// RemediationUpdate describes one lifecycle transition email.
type RemediationUpdate struct {
	From         mail.Address
	To           []mail.Address
	ProjectName  string
	JobName      string
	Status       string
	Reason       string
	PullURL      string
	DashboardURL string
}

// RemediationUpdateMessage renders one remediation lifecycle transition.
func RemediationUpdateMessage(input RemediationUpdate) Message {
	label := remediationStatusLabel(input.Status)
	subject := notificationSubject(input.ProjectName, "Remediation "+label, input.JobName)
	var text strings.Builder
	fmt.Fprintf(&text, "%s\n\nProject: %s\nJob: %s\nStatus: %s\n", label, input.ProjectName, input.JobName, input.Status)
	if input.Reason != "" {
		fmt.Fprintf(&text, "Details: %s\n", input.Reason)
	}
	if input.PullURL != "" {
		fmt.Fprintf(&text, "Pull request: %s\n", input.PullURL)
	}
	if input.DashboardURL != "" {
		fmt.Fprintf(&text, "Dashboard: %s\n", input.DashboardURL)
	}
	htmlBody := fmt.Sprintf("<!doctype html><html><body><h2>%s</h2><p><strong>Project:</strong> %s</p><p><strong>Job:</strong> %s</p><p><strong>Status:</strong> %s</p>",
		html.EscapeString(label), html.EscapeString(input.ProjectName), html.EscapeString(input.JobName), html.EscapeString(input.Status))
	if input.Reason != "" {
		htmlBody += "<p><strong>Details:</strong> " + html.EscapeString(input.Reason) + "</p>"
	}
	if input.PullURL != "" {
		htmlBody += `<p><a href="` + html.EscapeString(input.PullURL) + `">View pull request</a></p>`
	}
	if input.DashboardURL != "" {
		htmlBody += `<p><a href="` + html.EscapeString(input.DashboardURL) + `">View dashboard</a></p>`
	}
	htmlBody += "</body></html>"
	return Message{From: input.From, To: append([]mail.Address(nil), input.To...), Subject: subject, TextBody: text.String(), HTMLBody: htmlBody}
}

func remediationStatusLabel(status string) string {
	switch status {
	case "awaiting_presubmit":
		return "awaiting presubmit"
	case "presubmit_running":
		return "presubmit running"
	case "premerge_verified":
		return "passed presubmit verification"
	case "presubmit_failed_same_cause":
		return "presubmit reproduced the same failure"
	case "presubmit_failed_different_cause":
		return "presubmit failed for a different reason"
	case "observing":
		return "observing post-merge runs"
	case "verified_fixed":
		return "verified fixed"
	case "still_failing_same_cause":
		return "same failure still present"
	case "failing_different_cause":
		return "different failure observed"
	case "inconclusive":
		return "verification inconclusive"
	default:
		return strings.ReplaceAll(status, "_", " ")
	}
}

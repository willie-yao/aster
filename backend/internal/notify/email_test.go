package notify

import (
	"net/mail"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestFailureMessageContent(t *testing.T) {
	n := &Notifier{
		from:             mail.Address{Name: "Dashboard", Address: "dashboard@example.com"},
		to:               []mail.Address{{Address: "team@example.com"}},
		projectName:      "Example Project",
		dashboardBaseURL: "https://dash.example.com",
		prowURLBase:      "https://prow.example.com/view/",
	}
	tf := persistentFailure("job-id", "periodic-example", "TestNetwork", "hash", 5)
	tf.LastFailure.FailureMessage = "connection refused"

	message := n.failureMessage(eventNewFailure, tf, "summary", "root cause")
	for _, want := range []string{"Example Project", "TestNetwork", "periodic-example", "5 consecutive", "connection refused", "root cause", "https://dash.example.com/job/periodic-example/test/TestNetwork", "https://prow.example.com/view/periodic-example/100"} {
		if !strings.Contains(message.TextBody, want) {
			t.Errorf("text body missing %q: %s", want, message.TextBody)
		}
	}
	if !strings.Contains(message.Subject, "Persistent failure") {
		t.Fatalf("subject = %q", message.Subject)
	}
	if message.From.Address != "dashboard@example.com" || len(message.To) != 1 {
		t.Fatalf("envelope = %+v %+v", message.From, message.To)
	}
}

func TestChangedFailureSubject(t *testing.T) {
	n := &Notifier{projectName: "Example", from: mail.Address{Address: "from@example.com"}, to: []mail.Address{{Address: "to@example.com"}}}
	message := n.failureMessage(eventChangedFailure, models.TestFlakiness{TestName: "TestChanged"}, "", "")
	if !strings.Contains(message.Subject, "Failure changed") {
		t.Fatalf("subject = %q", message.Subject)
	}
}

func TestRecoveryMessageContent(t *testing.T) {
	n := &Notifier{
		from:             mail.Address{Address: "from@example.com"},
		to:               []mail.Address{{Address: "to@example.com"}},
		projectName:      "Example",
		dashboardBaseURL: "https://dash.example.com",
	}
	message := n.recoveryMessage(NotifiedFailure{JobName: "job", TestName: "TestRecovered", ConsecutiveCount: 7})
	for _, want := range []string{"Recovered", "TestRecovered", "7 consecutive", "https://dash.example.com/job/job/test/TestRecovered"} {
		if !strings.Contains(message.Subject+message.TextBody, want) {
			t.Errorf("message missing %q: subject=%q body=%s", want, message.Subject, message.TextBody)
		}
	}
}

func TestHTMLMessageEscapesDynamicContent(t *testing.T) {
	n := &Notifier{from: mail.Address{Address: "from@example.com"}, to: []mail.Address{{Address: "to@example.com"}}, projectName: "<Project>", dashboardBaseURL: "https://dash.example.com"}
	tf := persistentFailure("job", "job<script>", "Test<script>", "hash", 3)
	tf.LastFailure.FailureMessage = `<img src=x onerror="alert(1)">`
	message := n.failureMessage(eventNewFailure, tf, "", `<script>alert(1)</script>`)
	if strings.Contains(message.HTMLBody, "<script>") || strings.Contains(message.HTMLBody, "<img") {
		t.Fatalf("HTML contains unescaped content: %s", message.HTMLBody)
	}
	for _, escaped := range []string{"&lt;Project&gt;", "Test&lt;script&gt;", "&lt;script&gt;alert(1)&lt;/script&gt;", "&lt;img"} {
		if !strings.Contains(message.HTMLBody, escaped) {
			t.Errorf("HTML missing escaped value %q: %s", escaped, message.HTMLBody)
		}
	}
}

func TestNotificationSubjectSanitizesHeaders(t *testing.T) {
	subject := notificationSubject("Project\r\nBcc: victim@example.com", "Persistent failure", "Test\nInjected")
	if strings.ContainsAny(subject, "\r\n") || strings.Contains(subject, "  ") {
		t.Fatalf("subject was not sanitized: %q", subject)
	}
}

func TestFailureMessageTruncatesLongFields(t *testing.T) {
	n := &Notifier{from: mail.Address{Address: "from@example.com"}, to: []mail.Address{{Address: "to@example.com"}}}
	tf := persistentFailure("job", "job", "test", "hash", 3)
	tf.LastFailure.FailureMessage = strings.Repeat("e", 400)
	message := n.failureMessage(eventNewFailure, tf, "", strings.Repeat("a", 800))
	if strings.Contains(message.TextBody, strings.Repeat("e", 201)) || strings.Contains(message.TextBody, strings.Repeat("a", 501)) {
		t.Fatal("message fields were not truncated")
	}
}

func TestPatternMessageEscapesContentAndBuildsInertLinks(t *testing.T) {
	n := &Notifier{
		from:             mail.Address{Address: "from@example.com"},
		to:               []mail.Address{{Address: "to@example.com"}},
		projectName:      "Example",
		dashboardBaseURL: "https://dash.example.com",
		actionLinks:      true,
	}
	pattern := systemicPattern("pattern-1", "periodic/job", "Job<script>")
	pattern.SharedRootCause = `<script>alert(1)</script>`
	message := n.patternMessage(pattern)
	if strings.Contains(message.HTMLBody, "<script>") || !strings.Contains(message.HTMLBody, "&lt;script&gt;") {
		t.Fatalf("pattern HTML was not escaped: %s", message.HTMLBody)
	}
	for _, want := range []string{"action=create-issue", "action=propose-fix", "failure=pattern-1", "Nothing is created"} {
		if !strings.Contains(message.HTMLBody+message.TextBody, want) {
			t.Errorf("pattern message missing %q", want)
		}
	}
}

func TestActionDraftReadyMessage(t *testing.T) {
	message := ActionDraftReadyMessage(ActionDraftReady{
		From: mail.Address{Address: "from@example.com"}, To: []mail.Address{{Address: "to@example.com"}},
		Project: "Example", Owner: "alice", Kind: "propose-fix", Title: "Fix timeout",
		ReviewURL: "https://dash.example.com/action-request/request-1",
	})
	for _, want := range []string{"Draft ready", "alice", "fix proposal", "Fix timeout", "request-1", "Nothing has been posted"} {
		if !strings.Contains(message.Subject+message.TextBody+message.HTMLBody, want) {
			t.Errorf("message missing %q", want)
		}
	}
}

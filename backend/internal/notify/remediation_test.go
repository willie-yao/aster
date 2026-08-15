package notify

import (
	"net/mail"
	"strings"
	"testing"
)

func TestRemediationUpdateMessage(t *testing.T) {
	message := RemediationUpdateMessage(RemediationUpdate{
		From: mail.Address{Address: "from@example.com"}, To: []mail.Address{{Address: "to@example.com"}},
		ProjectName: "Project", JobName: "periodic-job", Status: "verified_fixed",
		Reason: "2 clean runs", PullURL: "https://github.com/o/r/pull/7", DashboardURL: "https://dash/job",
	})
	for _, want := range []string{"verified fixed", "periodic-job", "2 clean runs", "pull/7"} {
		if !strings.Contains(message.TextBody, want) && !strings.Contains(message.Subject, want) {
			t.Errorf("message missing %q: %+v", want, message)
		}
	}
}

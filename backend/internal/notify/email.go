package notify

import (
	"bytes"
	"fmt"
	"html/template"
	"net/mail"
	"net/url"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

type eventKind int

const (
	eventNewFailure eventKind = iota
	eventChangedFailure
)

type emailView struct {
	Heading          string
	ProjectName      string
	TestName         string
	JobName          string
	ConsecutiveCount int
	FailureMessage   string
	AIText           string
	DashboardURL     string
	ProwURL          string
	Recovery         bool
}

var emailHTMLTemplate = template.Must(template.New("notification").Parse(`<!doctype html>
<html>
<body>
  <h2>{{.Heading}}</h2>
  <p><strong>Project:</strong> {{.ProjectName}}</p>
  <table>
    <tr><td><strong>Test</strong></td><td>{{.TestName}}</td></tr>
    <tr><td><strong>Job</strong></td><td>{{.JobName}}</td></tr>
    <tr><td><strong>Status</strong></td><td>{{if .Recovery}}Recovered after {{.ConsecutiveCount}} consecutive failures{{else}}Failed {{.ConsecutiveCount}} consecutive times{{end}}</td></tr>
  </table>
  {{if .FailureMessage}}<h3>Latest error</h3><pre>{{.FailureMessage}}</pre>{{end}}
  {{if .AIText}}<h3>AI analysis</h3><p>{{.AIText}}</p>{{end}}
  <p><a href="{{.DashboardURL}}">View on dashboard</a>{{if .ProwURL}} | <a href="{{.ProwURL}}">View in Prow</a>{{end}}</p>
</body>
</html>`))

func (n *Notifier) failureMessage(kind eventKind, tf models.TestFlakiness, aiSummary, aiRootCause string) Message {
	heading := "Persistent test failure"
	subjectLabel := "Persistent failure"
	if kind == eventChangedFailure {
		heading = "Persistent test failure changed"
		subjectLabel = "Failure changed"
	}

	failureMessage := ""
	prowURL := ""
	if tf.LastFailure != nil {
		failureMessage = textutil.Truncate(tf.LastFailure.FailureMessage, 200)
		if tf.LastFailure.BuildID != "" {
			prowURL = n.prowURLBase + tf.JobName + "/" + tf.LastFailure.BuildID
		}
	}
	aiText := aiRootCause
	if aiText == "" {
		aiText = aiSummary
	}
	if aiText == "" {
		aiText = "No AI analysis available"
	}
	aiText = textutil.Truncate(aiText, 500)

	view := emailView{
		Heading:          heading,
		ProjectName:      n.projectName,
		TestName:         tf.TestName,
		JobName:          tf.JobName,
		ConsecutiveCount: tf.ConsecutiveFailures,
		FailureMessage:   failureMessage,
		AIText:           aiText,
		DashboardURL:     n.dashboardURL(tf.JobName, tf.TestName),
		ProwURL:          prowURL,
	}
	return Message{
		From:     n.from,
		To:       append([]mail.Address(nil), n.to...),
		Subject:  notificationSubject(n.projectName, subjectLabel, tf.TestName),
		TextBody: failureText(view),
		HTMLBody: renderHTML(view),
	}
}

func (n *Notifier) recoveryMessage(nf NotifiedFailure) Message {
	view := emailView{
		Heading:          "Test recovery",
		ProjectName:      n.projectName,
		TestName:         nf.TestName,
		JobName:          nf.JobName,
		ConsecutiveCount: nf.ConsecutiveCount,
		DashboardURL:     n.dashboardURL(nf.JobName, nf.TestName),
		Recovery:         true,
	}
	return Message{
		From:     n.from,
		To:       append([]mail.Address(nil), n.to...),
		Subject:  notificationSubject(n.projectName, "Recovered", nf.TestName),
		TextBody: recoveryText(view),
		HTMLBody: renderHTML(view),
	}
}

func failureText(view emailView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nProject: %s\nTest: %s\nJob: %s\nStatus: Failed %d consecutive times\n",
		view.Heading, view.ProjectName, view.TestName, view.JobName, view.ConsecutiveCount)
	if view.FailureMessage != "" {
		fmt.Fprintf(&b, "\nLatest error:\n%s\n", view.FailureMessage)
	}
	fmt.Fprintf(&b, "\nAI analysis:\n%s\n\nDashboard: %s\n", view.AIText, view.DashboardURL)
	if view.ProwURL != "" {
		fmt.Fprintf(&b, "Prow: %s\n", view.ProwURL)
	}
	return b.String()
}

func recoveryText(view emailView) string {
	return fmt.Sprintf("%s\n\nProject: %s\nTest: %s\nJob: %s\nStatus: Recovered after %d consecutive failures\n\nDashboard: %s\n",
		view.Heading, view.ProjectName, view.TestName, view.JobName, view.ConsecutiveCount, view.DashboardURL)
}

func renderHTML(view emailView) string {
	var b bytes.Buffer
	if err := emailHTMLTemplate.Execute(&b, view); err != nil {
		return ""
	}
	return b.String()
}

func notificationSubject(projectName, label, testName string) string {
	subject := fmt.Sprintf("[%s] %s: %s", cleanHeader(projectName), label, cleanHeader(testName))
	return textutil.Truncate(subject, 180)
}

func cleanHeader(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")), " ")
}

func (n *Notifier) dashboardURL(jobName, testName string) string {
	return fmt.Sprintf("%s/job/%s/test/%s", n.dashboardBaseURL, url.PathEscape(jobName), url.PathEscape(testName))
}

// ParseAddresses parses sender and recipient configuration.
func ParseAddresses(from string, recipients []string) (mail.Address, []mail.Address, error) {
	parsedFrom, err := mail.ParseAddress(from)
	if err != nil {
		return mail.Address{}, nil, fmt.Errorf("parsing email sender: %w", err)
	}
	parsedRecipients := make([]mail.Address, 0, len(recipients))
	for i, recipient := range recipients {
		parsed, err := mail.ParseAddress(recipient)
		if err != nil {
			return mail.Address{}, nil, fmt.Errorf("parsing email recipient %d: %w", i, err)
		}
		parsedRecipients = append(parsedRecipients, *parsed)
	}
	return *parsedFrom, parsedRecipients, nil
}

type patternEmailView struct {
	Heading        string
	ProjectName    string
	Subject        string
	Confidence     string
	BuildsAnalyzed int
	RootCause      string
	PreviousCause  string
	SuggestedFix   string
	DashboardURL   string
	IssueURL       string
	FixURL         string
}

var patternHTMLTemplate = template.Must(template.New("pattern-notification").Parse(`<!doctype html>
<html>
<body>
  <h2>{{.Heading}}</h2>
  <p><strong>Project:</strong> {{.ProjectName}}</p>
  <table>
    <tr><td><strong>Job</strong></td><td>{{.Subject}}</td></tr>
    <tr><td><strong>Confidence</strong></td><td>{{.Confidence}}</td></tr>
    <tr><td><strong>Builds analyzed</strong></td><td>{{.BuildsAnalyzed}}</td></tr>
  </table>
  {{if .PreviousCause}}<h3>Previous shared root cause</h3><p>{{.PreviousCause}}</p>{{end}}
  {{if .RootCause}}<h3>Current shared root cause</h3><p>{{.RootCause}}</p>{{end}}
  {{if .SuggestedFix}}<h3>Suggested fix</h3><p>{{.SuggestedFix}}</p>{{end}}
  <p><a href="{{.DashboardURL}}">View recurring pattern</a></p>
  {{if .IssueURL}}
  <p>
    <a href="{{.IssueURL}}" style="display:inline-block;padding:10px 14px;background:#b45309;color:#ffffff;text-decoration:none;border-radius:6px;margin-right:8px">Review issue draft</a>
    <a href="{{.FixURL}}" style="display:inline-block;padding:10px 14px;background:#1d4ed8;color:#ffffff;text-decoration:none;border-radius:6px">Review fix proposal</a>
  </p>
  <p>These links open the authenticated dashboard. Nothing is created until a maintainer generates, reviews, and confirms the draft.</p>
  {{end}}
</body>
</html>`))

func (n *Notifier) patternMessage(pattern models.PatternAnalysis, previousRootCause string) Message {
	heading := "Systemic recurring failure"
	subjectLabel := heading
	if previousRootCause != "" {
		heading = "Systemic recurring failure changed"
		subjectLabel = "Recurring failure changed"
	}
	view := patternEmailView{
		Heading:        heading,
		ProjectName:    n.projectName,
		Subject:        pattern.Subject,
		Confidence:     pattern.Confidence,
		BuildsAnalyzed: pattern.BuildsAnalyzed,
		RootCause:      textutil.Truncate(pattern.SharedRootCause, 1000),
		PreviousCause:  textutil.Truncate(previousRootCause, 1000),
		SuggestedFix:   textutil.Truncate(pattern.SuggestedFix, 1000),
		DashboardURL:   n.patternURL(pattern),
	}
	if n.actionLinks && pattern.ID != "" {
		view.IssueURL = n.patternActionURL(pattern, "create-issue")
		view.FixURL = n.patternActionURL(pattern, "propose-fix")
	}
	return Message{
		From:     n.from,
		To:       append([]mail.Address(nil), n.to...),
		Subject:  notificationSubject(n.projectName, subjectLabel, pattern.Subject),
		TextBody: patternText(view),
		HTMLBody: renderPatternHTML(view),
	}
}

func patternText(view patternEmailView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nProject: %s\nJob: %s\nConfidence: %s\nBuilds analyzed: %d\n",
		view.Heading, view.ProjectName, view.Subject, view.Confidence, view.BuildsAnalyzed)
	if view.PreviousCause != "" {
		fmt.Fprintf(&b, "\nPrevious shared root cause:\n%s\n", view.PreviousCause)
	}
	if view.RootCause != "" {
		fmt.Fprintf(&b, "\nCurrent shared root cause:\n%s\n", view.RootCause)
	}
	if view.SuggestedFix != "" {
		fmt.Fprintf(&b, "\nSuggested fix:\n%s\n", view.SuggestedFix)
	}
	fmt.Fprintf(&b, "\nDashboard: %s\n", view.DashboardURL)
	if view.IssueURL != "" {
		fmt.Fprintf(&b, "Review issue draft: %s\nReview fix proposal: %s\n", view.IssueURL, view.FixURL)
		b.WriteString("\nNothing is created until a maintainer generates, reviews, and confirms the draft in the authenticated dashboard.\n")
	}
	return b.String()
}

func renderPatternHTML(view patternEmailView) string {
	var b bytes.Buffer
	if err := patternHTMLTemplate.Execute(&b, view); err != nil {
		return ""
	}
	return b.String()
}

func (n *Notifier) patternURL(pattern models.PatternAnalysis) string {
	return fmt.Sprintf("%s/job/%s#pattern-%s", n.dashboardBaseURL, url.PathEscape(patternJobID(pattern)), url.PathEscape(pattern.ID))
}

func (n *Notifier) patternActionURL(pattern models.PatternAnalysis, action string) string {
	values := url.Values{}
	values.Set("failure", pattern.ID)
	values.Set("action", action)
	return fmt.Sprintf("%s/job/%s?%s#pattern-%s", n.dashboardBaseURL, url.PathEscape(patternJobID(pattern)), values.Encode(), url.PathEscape(pattern.ID))
}

func patternJobID(pattern models.PatternAnalysis) string {
	if pattern.JobID != "" {
		return pattern.JobID
	}
	return pattern.Subject
}

// ActionDraftReady describes a persisted draft ready for authenticated review.
type ActionDraftReady struct {
	From      mail.Address
	To        []mail.Address
	Project   string
	Owner     string
	RequestID string
	Kind      string
	Title     string
	ReviewURL string
}

// ActionDraftReadyMessage renders the email sent after async generation.
func ActionDraftReadyMessage(input ActionDraftReady) Message {
	label := "issue draft"
	if input.Kind == "propose-fix" || input.Kind == "fix" {
		label = "fix proposal"
	}
	subject := notificationSubject(input.Project, "Draft ready", input.Title)
	text := fmt.Sprintf("Draft ready for review\n\nProject: %s\nRequested by: %s\nType: %s\nTitle: %s\n\nReview and confirm: %s\n\nNothing has been posted to GitHub. Sign in as the requesting maintainer to review and confirm the exact draft.\n",
		input.Project, input.Owner, label, input.Title, input.ReviewURL)
	view := struct {
		Project, Owner, Label, Title, ReviewURL string
	}{input.Project, input.Owner, label, input.Title, input.ReviewURL}
	var html bytes.Buffer
	_ = template.Must(template.New("draft-ready").Parse(`<!doctype html>
<html><body>
<h2>Draft ready for review</h2>
<p><strong>Project:</strong> {{.Project}}</p>
<p><strong>Requested by:</strong> {{.Owner}}</p>
<p><strong>Type:</strong> {{.Label}}</p>
<p><strong>Title:</strong> {{.Title}}</p>
<p><a href="{{.ReviewURL}}" style="display:inline-block;padding:10px 14px;background:#1d4ed8;color:#ffffff;text-decoration:none;border-radius:6px">Review and confirm</a></p>
<p>Nothing has been posted to GitHub. Sign in as the requesting maintainer to review and confirm the exact draft.</p>
</body></html>`)).Execute(&html, view)
	return Message{From: input.From, To: append([]mail.Address(nil), input.To...), Subject: subject, TextBody: text, HTMLBody: html.String()}
}

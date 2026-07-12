// Package notify sends Slack notifications for persistent test failures through
// incoming webhooks, with deduplication and recovery tracking.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/redact"
)

// NotificationState tracks which persistent failures have been notified.
type NotificationState struct {
	Notified map[string]NotifiedFailure `json:"notified"`
}

// NotifiedFailure tracks a single notified persistent failure.
type NotifiedFailure struct {
	FirstNotifiedAt  string `json:"first_notified_at"`
	ConsecutiveCount int    `json:"consecutive_count"`
	ErrorHash        string `json:"error_hash"`
	JobName          string `json:"job_name"`
	TestName         string `json:"test_name"`
}

// Notifier sends Slack notifications for persistent test failures.
type Notifier struct {
	webhookURL       string
	client           *http.Client
	state            *NotificationState
	stateFile        string
	dashboardBaseURL string
	prowURLBase      string
}

// Stats tracks notification counts for logging.
type Stats struct {
	NewAlerts  int
	Recoveries int
}

// NewNotifier creates a Notifier and loads existing state from stateFile.
// prowURLBase is the trailing-slashed Prow view prefix for artifact links.
func NewNotifier(webhookURL, stateFile, dashboardBaseURL, prowURLBase string) *Notifier {
	n := &Notifier{
		webhookURL:       webhookURL,
		client:           &http.Client{Timeout: 15 * time.Second},
		stateFile:        stateFile,
		dashboardBaseURL: strings.TrimRight(dashboardBaseURL, "/"),
		prowURLBase:      prowURLBase,
		state: &NotificationState{
			Notified: make(map[string]NotifiedFailure),
		},
	}
	n.loadState()
	return n
}

func (n *Notifier) loadState() {
	data, err := os.ReadFile(n.stateFile)
	if err != nil {
		return // no state file yet
	}
	var s NotificationState
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("Warning: failed to parse notification state: %v", err)
		return
	}
	if s.Notified != nil {
		n.state = &s
	}
}

// SaveState writes the current notification state to disk.
func (n *Notifier) SaveState() error {
	data, err := json.MarshalIndent(n.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling notification state: %w", err)
	}
	return os.WriteFile(n.stateFile, data, 0644)
}

// notificationKey returns the deduplication key for a test. It uses JobID so
// presubmits and periodics with the same job name do not collide.
func notificationKey(jobID, testName string) string {
	return jobID + "::" + testName
}

// ProcessFailures compares current persistent failures against state and sends
// Slack notifications for new failures, changed error hashes, and recoveries.
func (n *Notifier) ProcessFailures(ctx context.Context, report models.FlakinessReport, jobDetails []models.JobDetail) (Stats, error) {
	var stats Stats

	// Current persistent failures require at least 3 consecutive failures.
	current := make(map[string]models.TestFlakiness)
	for _, tf := range report.PersistentFailures {
		if tf.ConsecutiveFailures >= 3 {
			key := notificationKey(tf.JobID, tf.TestName)
			current[key] = tf
		}
	}

	aiLookup := buildAILookup(jobDetails)

	for key, tf := range current {
		existing, wasNotified := n.state.Notified[key]

		currentHash := ""
		if tf.LastFailure != nil {
			currentHash = tf.LastFailure.ErrorHash
		}

		if !wasNotified {
			summary, rootCause := lookupAI(aiLookup, tf.JobID, tf.TestName)
			if err := n.sendFailureAlert(ctx, tf, summary, rootCause); err != nil {
				log.Printf("  ⚠ Failed to send alert for %s: %v", key, err)
			} else {
				stats.NewAlerts++
			}
			n.state.Notified[key] = NotifiedFailure{
				FirstNotifiedAt:  time.Now().UTC().Format(time.RFC3339),
				ConsecutiveCount: tf.ConsecutiveFailures,
				ErrorHash:        currentHash,
				JobName:          tf.JobName,
				TestName:         tf.TestName,
			}
		} else if currentHash != existing.ErrorHash {
			// Notify again when the failure mode changes.
			summary, rootCause := lookupAI(aiLookup, tf.JobID, tf.TestName)
			if err := n.sendFailureAlert(ctx, tf, summary, rootCause); err != nil {
				log.Printf("  ⚠ Failed to send changed-alert for %s: %v", key, err)
			} else {
				stats.NewAlerts++
			}
			n.state.Notified[key] = NotifiedFailure{
				FirstNotifiedAt:  existing.FirstNotifiedAt,
				ConsecutiveCount: tf.ConsecutiveFailures,
				ErrorHash:        currentHash,
				JobName:          tf.JobName,
				TestName:         tf.TestName,
			}
		}
		// Same error hash still failing has already been notified.
	}

	// Recover state entries absent from current persistent failures.
	for key, nf := range n.state.Notified {
		if _, stillFailing := current[key]; !stillFailing {
			if err := n.sendRecoveryAlert(ctx, nf); err != nil {
				log.Printf("  ⚠ Failed to send recovery for %s: %v", key, err)
			} else {
				stats.Recoveries++
			}
			delete(n.state.Notified, key)
		}
	}

	return stats, nil
}

// aiEntry stores AI text used in notifications.
type aiEntry struct {
	Summary   string
	RootCause string
}

func buildAILookup(jobDetails []models.JobDetail) map[string]aiEntry {
	lookup := make(map[string]aiEntry)
	for _, jd := range jobDetails {
		for _, run := range jd.Runs {
			for _, tc := range run.TestCases {
				if tc.Status != "failed" {
					continue
				}
				key := notificationKey(jd.JobID, tc.Name)
				if _, exists := lookup[key]; exists {
					continue // keep the first entry, since runs are newest-first
				}
				var entry aiEntry
				if tc.AISummary != nil {
					entry.Summary = tc.AISummary.Summary
				}
				if tc.AIAnalysis != nil {
					entry.RootCause = tc.AIAnalysis.RootCause
				}
				if entry.Summary != "" || entry.RootCause != "" {
					lookup[key] = entry
				}
			}
		}
	}
	return lookup
}

func lookupAI(lookup map[string]aiEntry, jobID, testName string) (summary, rootCause string) {
	key := notificationKey(jobID, testName)
	if e, ok := lookup[key]; ok {
		return e.Summary, e.RootCause
	}
	return "", ""
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// sendFailureAlert posts a failure notification to the Slack webhook.
func (n *Notifier) sendFailureAlert(ctx context.Context, tf models.TestFlakiness, aiSummary, aiRootCause string) error {
	failureMsg := ""
	prowURL := ""
	if tf.LastFailure != nil {
		failureMsg = truncate(tf.LastFailure.FailureMessage, 200)
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

	dashboardURL := fmt.Sprintf("%s/job/%s/test/%s",
		n.dashboardBaseURL,
		url.PathEscape(tf.JobName),
		url.PathEscape(tf.TestName))

	blocks := []map[string]interface{}{
		{
			"type": "header",
			"text": map[string]string{"type": "plain_text", "text": "🔴 Persistent Test Failure"},
		},
		{
			"type": "section",
			"fields": []map[string]string{
				{"type": "mrkdwn", "text": fmt.Sprintf("*Test:*\n%s", tf.TestName)},
				{"type": "mrkdwn", "text": fmt.Sprintf("*Job:*\n%s", tf.JobName)},
				{"type": "mrkdwn", "text": fmt.Sprintf("*Status:*\nFailed %d consecutive times", tf.ConsecutiveFailures)},
			},
		},
	}

	if failureMsg != "" {
		blocks = append(blocks, map[string]interface{}{
			"type": "section",
			"text": map[string]string{"type": "mrkdwn", "text": fmt.Sprintf("*Error:*\n```%s```", failureMsg)},
		})
	}

	blocks = append(blocks, map[string]interface{}{
		"type": "section",
		"text": map[string]string{"type": "mrkdwn", "text": fmt.Sprintf("*🤖 AI Analysis:*\n%s", truncate(aiText, 500))},
	})

	linkParts := []string{fmt.Sprintf("<%s|View on Dashboard>", dashboardURL)}
	if prowURL != "" {
		linkParts = append(linkParts, fmt.Sprintf("<%s|View in Prow>", prowURL))
	}
	blocks = append(blocks, map[string]interface{}{
		"type":     "actions",
		"elements": slackButtons(linkParts),
	})

	payload := map[string]interface{}{"blocks": blocks}
	return n.postWebhook(ctx, payload)
}

// sendRecoveryAlert posts a recovery notification to the Slack webhook.
func (n *Notifier) sendRecoveryAlert(ctx context.Context, nf NotifiedFailure) error {
	blocks := []map[string]interface{}{
		{
			"type": "header",
			"text": map[string]string{"type": "plain_text", "text": "✅ Test Recovery"},
		},
		{
			"type": "section",
			"fields": []map[string]string{
				{"type": "mrkdwn", "text": fmt.Sprintf("*Test:*\n%s", nf.TestName)},
				{"type": "mrkdwn", "text": fmt.Sprintf("*Job:*\n%s", nf.JobName)},
			},
		},
		{
			"type": "section",
			"text": map[string]string{"type": "mrkdwn", "text": fmt.Sprintf("Previously failed %d consecutive times. Now passing.", nf.ConsecutiveCount)},
		},
	}

	payload := map[string]interface{}{"blocks": blocks}
	return n.postWebhook(ctx, payload)
}

func slackButtons(links []string) []map[string]interface{} {
	var elements []map[string]interface{}
	for _, link := range links {
		// Slack mrkdwn links use <url|text>.
		parts := strings.SplitN(strings.Trim(link, "<>"), "|", 2)
		if len(parts) == 2 {
			elements = append(elements, map[string]interface{}{
				"type": "button",
				"text": map[string]string{"type": "plain_text", "text": parts[1]},
				"url":  parts[0],
			})
		}
	}
	return elements
}

func (n *Notifier) postWebhook(ctx context.Context, payload interface{}) error {
	if n.webhookURL == "" {
		return nil
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("creating webhook request: %s", redact.URLs(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		// The webhook URL path is itself the secret; a transport error embeds it.
		return fmt.Errorf("posting to webhook: %s", redact.URLs(err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}

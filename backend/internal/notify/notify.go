// Package notify sends email notifications for persistent test failures with
// deduplication and recovery tracking.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

const notificationChannel = "email-v1"

// NotificationState tracks which persistent failures have been notified.
type NotificationState struct {
	Channel  string                     `json:"channel"`
	Notified map[string]NotifiedFailure `json:"notified"`
	Patterns map[string]NotifiedPattern `json:"patterns,omitempty"`
}

// NotifiedFailure tracks a single notified persistent failure.
type NotifiedFailure struct {
	FirstNotifiedAt  string `json:"first_notified_at"`
	ConsecutiveCount int    `json:"consecutive_count"`
	ErrorHash        string `json:"error_hash"`
	JobName          string `json:"job_name"`
	TestName         string `json:"test_name"`
}

// NotifiedPattern tracks one systemic recurring pattern email.
type NotifiedPattern struct {
	PatternID       string `json:"pattern_id"`
	JobID           string `json:"job_id"`
	Subject         string `json:"subject"`
	SharedRootCause string `json:"shared_root_cause"`
}

// Message is one rendered email notification.
type Message struct {
	From     mail.Address
	To       []mail.Address
	Subject  string
	TextBody string
	HTMLBody string
}

// Sender delivers one email message.
type Sender interface {
	Send(context.Context, Message) error
}

// Notifier sends email notifications for persistent test failures.
type Notifier struct {
	sender           Sender
	from             mail.Address
	to               []mail.Address
	state            *NotificationState
	stateFile        string
	projectName      string
	dashboardBaseURL string
	prowURLBase      string
	actionLinks      bool
}

// Stats tracks notification counts for logging.
type Stats struct {
	NewAlerts     int
	PatternAlerts int
	Recoveries    int
	Failed        int
}

// NewNotifier creates a Notifier and loads existing state from stateFile.
func NewNotifier(sender Sender, from mail.Address, to []mail.Address, stateFile, projectName, dashboardBaseURL, prowURLBase string, actionLinks bool) *Notifier {
	n := &Notifier{
		sender:           sender,
		from:             from,
		to:               append([]mail.Address(nil), to...),
		stateFile:        stateFile,
		projectName:      projectName,
		dashboardBaseURL: strings.TrimRight(dashboardBaseURL, "/"),
		prowURLBase:      prowURLBase,
		actionLinks:      actionLinks,
		state:            newNotificationState(),
	}
	n.loadState()
	return n
}

func newNotificationState() *NotificationState {
	return &NotificationState{
		Channel:  notificationChannel,
		Notified: make(map[string]NotifiedFailure),
		Patterns: make(map[string]NotifiedPattern),
	}
}

func (n *Notifier) loadState() {
	data, err := os.ReadFile(n.stateFile)
	if err != nil {
		return
	}
	var s NotificationState
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("Warning: failed to parse notification state: %v", err)
		return
	}
	if s.Channel != notificationChannel {
		log.Printf("Notifications: resetting state for channel %q", notificationChannel)
		return
	}
	if s.Notified == nil {
		s.Notified = make(map[string]NotifiedFailure)
	}
	if s.Patterns == nil {
		s.Patterns = make(map[string]NotifiedPattern)
	}
	n.state = &s
}

// SaveState writes the current notification state to disk.
func (n *Notifier) SaveState() error {
	if err := statefile.WriteJSON(n.stateFile, n.state); err != nil {
		return fmt.Errorf("saving notification state: %w", err)
	}
	return nil
}

// notificationKey returns the deduplication key for a test. It uses JobID so
// presubmits and periodics with the same job name do not collide.
func notificationKey(jobID, testName string) string {
	return jobID + "::" + testName
}

// ProcessFailures compares current persistent failures against state and sends
// email for new failures, changed error hashes, and recoveries.
func (n *Notifier) ProcessFailures(ctx context.Context, report models.FlakinessReport, jobDetails []models.JobDetail) (Stats, error) {
	var stats Stats
	var sendErrs []error

	current := make(map[string]models.TestFlakiness)
	for _, tf := range report.PersistentFailures {
		if tf.ConsecutiveFailures >= 3 {
			current[notificationKey(tf.JobID, tf.TestName)] = tf
		}
	}

	aiLookup := buildAILookup(jobDetails)
	currentKeys := sortedKeys(current)
	for _, key := range currentKeys {
		tf := current[key]
		existing, wasNotified := n.state.Notified[key]
		currentHash := ""
		if tf.LastFailure != nil {
			currentHash = tf.LastFailure.ErrorHash
		}

		kind := eventNewFailure
		send := !wasNotified
		if wasNotified && currentHash != existing.ErrorHash {
			kind = eventChangedFailure
			send = true
		}
		if send {
			summary, rootCause := lookupAI(aiLookup, tf.JobID, tf.TestName)
			if err := n.sender.Send(ctx, n.failureMessage(kind, tf, summary, rootCause)); err != nil {
				stats.Failed++
				sendErrs = append(sendErrs, fmt.Errorf("%s: %w", key, err))
				continue
			}
			stats.NewAlerts++
			firstNotifiedAt := time.Now().UTC().Format(time.RFC3339)
			if wasNotified {
				firstNotifiedAt = existing.FirstNotifiedAt
			}
			n.state.Notified[key] = NotifiedFailure{
				FirstNotifiedAt:  firstNotifiedAt,
				ConsecutiveCount: tf.ConsecutiveFailures,
				ErrorHash:        currentHash,
				JobName:          tf.JobName,
				TestName:         tf.TestName,
			}
			continue
		}

		existing.ConsecutiveCount = tf.ConsecutiveFailures
		existing.JobName = tf.JobName
		existing.TestName = tf.TestName
		n.state.Notified[key] = existing
	}

	stateKeys := sortedKeys(n.state.Notified)
	for _, key := range stateKeys {
		if _, stillFailing := current[key]; stillFailing {
			continue
		}
		nf := n.state.Notified[key]
		if err := n.sender.Send(ctx, n.recoveryMessage(nf)); err != nil {
			stats.Failed++
			sendErrs = append(sendErrs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		stats.Recoveries++
		delete(n.state.Notified, key)
	}

	if n.actionLinks {
		for _, pattern := range report.RecurringPatterns {
			if !pattern.Systemic {
				continue
			}
			if pattern.ID == "" {
				pattern.ID = models.PatternID(pattern)
			}
			if _, notified := n.state.Patterns[pattern.ID]; notified {
				continue
			}
			if err := n.sender.Send(ctx, n.patternMessage(pattern)); err != nil {
				stats.Failed++
				sendErrs = append(sendErrs, fmt.Errorf("pattern %s: %w", pattern.ID, err))
				continue
			}
			stats.PatternAlerts++
			n.state.Patterns[pattern.ID] = NotifiedPattern{
				PatternID:       pattern.ID,
				JobID:           patternJobID(pattern),
				Subject:         pattern.Subject,
				SharedRootCause: pattern.SharedRootCause,
			}
		}
		n.reconcilePatternState(report.RecurringPatterns, jobDetails)
	}

	return stats, errors.Join(sendErrs...)
}

func (n *Notifier) reconcilePatternState(current []models.PatternAnalysis, jobDetails []models.JobDetail) {
	if len(jobDetails) == 0 || len(n.state.Patterns) == 0 {
		return
	}
	currentIDs := make(map[string]bool, len(current))
	for _, pattern := range current {
		if !pattern.Systemic {
			continue
		}
		id := pattern.ID
		if id == "" {
			id = models.PatternID(pattern)
		}
		currentIDs[id] = true
	}

	presentJobs := make(map[string]bool, len(jobDetails))
	authoritativeJobs := make(map[string]bool, len(jobDetails))
	for _, detail := range jobDetails {
		jobID := detail.JobID
		if jobID == "" {
			jobID = detail.Name
		}
		presentJobs[jobID] = true
		if len(detail.PatternAnalyses) > 0 || completedFailedBuilds(detail) < 3 {
			authoritativeJobs[jobID] = true
		}
	}

	for id, notified := range n.state.Patterns {
		if currentIDs[id] {
			continue
		}
		if !presentJobs[notified.JobID] || authoritativeJobs[notified.JobID] {
			delete(n.state.Patterns, id)
		}
	}
}

func completedFailedBuilds(detail models.JobDetail) int {
	count := 0
	for _, run := range detail.Runs {
		if !run.Passed && run.Result != "PENDING" {
			count++
		}
	}
	return count
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
					continue
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
	if entry, ok := lookup[notificationKey(jobID, testName)]; ok {
		return entry.Summary, entry.RootCause
	}
	return "", ""
}

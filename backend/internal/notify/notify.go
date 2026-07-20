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
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

const notificationChannel = "email-v1"

const patternSimilarityFloor = 0.30

var patternTokenRegex = regexp.MustCompile(`[a-z0-9]+`)

var patternNumericSignalRegexes = []struct {
	name string
	re   *regexp.Regexp
}{
	{name: "http", re: regexp.MustCompile(`\bhttp(?:/[0-9.]+)?(?:\s+status(?:\s+code)?)?\s*[:=-]?\s*([1-5][0-9]{2})\b`)},
	{name: "http", re: regexp.MustCompile(`\bstatus\s+code\s+([1-5][0-9]{2})\b`)},
	{name: "port", re: regexp.MustCompile(`\bport\s+([0-9]{1,5})\b`)},
	{name: "address-port", re: regexp.MustCompile(`(?:\]|[a-z][a-z0-9.-]*|(?:[0-9]{1,3}\.){3}[0-9]{1,3}):([0-9]{2,5})\b`)},
	{name: "tls", re: regexp.MustCompile(`\btls(?:v|\s+version)?\s*([0-9]+(?:\.[0-9]+)?)\b`)},
}

var patternPolarityRegexes = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:not|no|without|never)\s+([a-z0-9]+)\b`),
	regexp.MustCompile(`\b(?:fail|fails|failed)\s+to\s+([a-z0-9]+)\b`),
	regexp.MustCompile(`\b(?:before|until)(?:\s+[a-z0-9-]+){0,8}\s+(?:is|was|becomes?)\s+([a-z0-9]+)\b`),
}

var patternNegativeTokens = map[string]string{
	"disabled":     "enabled",
	"unavailable":  "available",
	"unauthorized": "authorized",
	"unhealthy":    "healthy",
	"unreachable":  "reachable",
	"unready":      "ready",
	"unsupported":  "supported",
}

var patternPolarityReplacer = strings.NewReplacer(
	"isn't", "is not", "isn’t", "is not",
	"aren't", "are not", "aren’t", "are not",
	"wasn't", "was not", "wasn’t", "was not",
	"weren't", "were not", "weren’t", "were not",
	"doesn't", "does not", "doesn’t", "does not",
	"don't", "do not", "don’t", "do not",
	"didn't", "did not", "didn’t", "did not",
	"can't", "can not", "can’t", "can not",
	"cannot", "can not",
	"couldn't", "could not", "couldn’t", "could not",
	"won't", "will not", "won’t", "will not",
	"wouldn't", "would not", "wouldn’t", "would not",
	"shouldn't", "should not", "shouldn’t", "should not",
	"hasn't", "has not", "hasn’t", "has not",
	"haven't", "have not", "haven’t", "have not",
	"hadn't", "had not", "hadn’t", "had not",
	"no longer", "not",
)

var patternStopTokens = map[string]struct{}{
	"all": {}, "and": {}, "any": {}, "are": {}, "because": {},
	"been": {}, "being": {}, "but": {}, "can": {}, "context": {},
	"could": {}, "deadline": {}, "during": {}, "error": {}, "errors": {},
	"exceeded": {}, "failed": {}, "fails": {}, "failure": {}, "for": {},
	"from": {}, "has": {}, "have": {}, "into": {}, "its": {},
	"never": {}, "no": {}, "not": {}, "occurred": {}, "only": {}, "retry": {},
	"side": {}, "that": {}, "the": {},
	"their": {}, "then": {}, "this": {}, "through": {}, "was": {},
	"were": {}, "when": {}, "which": {}, "while": {}, "will": {}, "with": {}, "without": {},
}

var patternLowWeightTokens = map[string]struct{}{
	"api": {}, "azure": {}, "call": {}, "client": {}, "cluster": {},
	"component": {}, "controller": {}, "conversion": {}, "error": {}, "fail": {},
	"failure": {}, "job": {}, "node": {}, "operator": {}, "pod": {},
	"provider": {}, "resource": {}, "server": {}, "service": {}, "timeout": {},
	"webhook": {},
}

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

// NotifiedPattern tracks the latest systemic pattern emailed for one job.
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
			key := patternJobID(pattern)
			existing, notified, stateKeys := n.patternStateFor(pattern)
			changed := notified && patternsMateriallyDifferent(existing.SharedRootCause, pattern.SharedRootCause)
			if notified && !changed {
				n.replacePatternState(key, stateKeys, notifiedPattern(pattern))
				continue
			}
			previousRootCause := ""
			if changed {
				previousRootCause = existing.SharedRootCause
			}
			if err := n.sender.Send(ctx, n.patternMessage(pattern, previousRootCause)); err != nil {
				stats.Failed++
				sendErrs = append(sendErrs, fmt.Errorf("pattern %s: %w", key, err))
				continue
			}
			stats.PatternAlerts++
			n.replacePatternState(key, stateKeys, notifiedPattern(pattern))
		}
		n.reconcilePatternState(report.RecurringPatterns, jobDetails)
	}

	return stats, errors.Join(sendErrs...)
}

func (n *Notifier) reconcilePatternState(current []models.PatternAnalysis, jobDetails []models.JobDetail) {
	if len(jobDetails) == 0 || len(n.state.Patterns) == 0 {
		return
	}
	currentJobs := make(map[string]bool, len(current))
	for _, pattern := range current {
		if !pattern.Systemic {
			continue
		}
		currentJobs[patternJobID(pattern)] = true
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

	for stateKey, notified := range n.state.Patterns {
		jobID := strings.TrimSpace(notified.JobID)
		if jobID == "" {
			jobID = stateKey
		}
		if currentJobs[jobID] {
			continue
		}
		if !presentJobs[jobID] || authoritativeJobs[jobID] {
			delete(n.state.Patterns, stateKey)
		}
	}
}

func notifiedPattern(pattern models.PatternAnalysis) NotifiedPattern {
	return NotifiedPattern{
		PatternID:       pattern.ID,
		JobID:           patternJobID(pattern),
		Subject:         pattern.Subject,
		SharedRootCause: pattern.SharedRootCause,
	}
}

func (n *Notifier) patternStateFor(pattern models.PatternAnalysis) (NotifiedPattern, bool, []string) {
	jobID := patternJobID(pattern)
	var exact *NotifiedPattern
	var closest *NotifiedPattern
	closestSimilarity := -1.0
	closestIsJobScoped := false
	var stateKeys []string
	for _, key := range sortedKeys(n.state.Patterns) {
		candidate := n.state.Patterns[key]
		candidateJobID := strings.TrimSpace(candidate.JobID)
		if candidateJobID == "" && key == jobID {
			candidateJobID = key
		}
		if candidateJobID != jobID {
			continue
		}
		stateKeys = append(stateKeys, key)
		copy := candidate
		if candidate.PatternID == pattern.ID || key == pattern.ID {
			exact = &copy
		}
		similarity := patternSimilarity(candidate.SharedRootCause, pattern.SharedRootCause)
		if similarity > closestSimilarity || similarity == closestSimilarity && key == jobID && !closestIsJobScoped {
			closest = &copy
			closestSimilarity = similarity
			closestIsJobScoped = key == jobID
		}
	}
	if exact != nil {
		return *exact, true, stateKeys
	}
	if closest != nil {
		return *closest, true, stateKeys
	}
	return NotifiedPattern{}, false, nil
}

func (n *Notifier) replacePatternState(jobID string, oldKeys []string, pattern NotifiedPattern) {
	for _, key := range oldKeys {
		delete(n.state.Patterns, key)
	}
	n.state.Patterns[jobID] = pattern
}

func patternsMateriallyDifferent(previous, current string) bool {
	if patternPolarityReversed(previous, current) {
		return true
	}
	previousSignals := patternNumericSignals(previous)
	currentSignals := patternNumericSignals(current)
	if patternNumericSignalsConflict(previousSignals, currentSignals) {
		return true
	}
	return patternSimilarity(previous, current) < patternSimilarityFloor
}

func patternPolarityReversed(previous, current string) bool {
	previousPolarity := patternPolarityTargets(previous)
	currentPolarity := patternPolarityTargets(current)
	previousTokens := patternTokens(previous)
	currentTokens := patternTokens(current)
	for target := range previousPolarity {
		if _, stillNegative := currentPolarity[target]; stillNegative {
			continue
		}
		if _, nowPositive := currentTokens[target]; nowPositive {
			return true
		}
	}
	for target := range currentPolarity {
		if _, alreadyNegative := previousPolarity[target]; alreadyNegative {
			continue
		}
		if _, wasPositive := previousTokens[target]; wasPositive {
			return true
		}
	}
	return false
}

func patternPolarityTargets(value string) map[string]struct{} {
	targets := make(map[string]struct{})
	lower := normalizePatternPolarityText(value)
	for _, re := range patternPolarityRegexes {
		for _, match := range re.FindAllStringSubmatch(lower, -1) {
			target := singularPatternToken(match[1])
			if target != "" {
				targets[target] = struct{}{}
			}
		}
	}
	for _, token := range patternTokenRegex.FindAllString(lower, -1) {
		if target := patternNegativeTokens[token]; target != "" {
			targets[target] = struct{}{}
		}
	}
	return targets
}

func patternSimilarity(previous, current string) float64 {
	previousTokens := patternTokens(previous)
	currentTokens := patternTokens(current)
	if len(previousTokens) == 0 || len(currentTokens) == 0 {
		if strings.TrimSpace(strings.ToLower(previous)) == strings.TrimSpace(strings.ToLower(current)) {
			return 1
		}
		return 0
	}
	unionTokens := make(map[string]struct{}, len(previousTokens)+len(currentTokens))
	for token := range previousTokens {
		unionTokens[token] = struct{}{}
	}
	for token := range currentTokens {
		unionTokens[token] = struct{}{}
	}
	intersectionWeight := 0.0
	unionWeight := 0.0
	for token := range unionTokens {
		weight := 1.0
		if _, lowWeight := patternLowWeightTokens[token]; lowWeight {
			weight = 0.2
		}
		unionWeight += weight
		if _, previousHas := previousTokens[token]; previousHas {
			if _, currentHas := currentTokens[token]; currentHas {
				intersectionWeight += weight
			}
		}
	}
	return intersectionWeight / unionWeight
}

func patternTokens(value string) map[string]struct{} {
	tokens := make(map[string]struct{})
	value = normalizePatternPolarityText(value)
	for _, token := range patternTokenRegex.FindAllString(value, -1) {
		if isNumericToken(token) {
			if len(token) <= 5 {
				tokens[token] = struct{}{}
			}
			continue
		}
		if len(token) < 3 {
			continue
		}
		if target := patternNegativeTokens[token]; target != "" {
			token = target
		}
		token = singularPatternToken(token)
		if _, stop := patternStopTokens[token]; stop {
			continue
		}
		tokens[token] = struct{}{}
	}
	return tokens
}

func normalizePatternPolarityText(value string) string {
	return patternPolarityReplacer.Replace(strings.ToLower(value))
}

func patternNumericSignals(value string) map[string]struct{} {
	value = strings.ToLower(value)
	signals := make(map[string]struct{})
	for _, signal := range patternNumericSignalRegexes {
		for _, match := range signal.re.FindAllStringSubmatch(value, -1) {
			for _, number := range match[1:] {
				if number != "" {
					signals[signal.name+":"+number] = struct{}{}
				}
			}
		}
	}
	return signals
}

func patternNumericSignalsConflict(a, b map[string]struct{}) bool {
	byKind := func(signals map[string]struct{}) map[string]map[string]struct{} {
		out := make(map[string]map[string]struct{})
		for signal := range signals {
			kind, value, ok := strings.Cut(signal, ":")
			if !ok {
				continue
			}
			if out[kind] == nil {
				out[kind] = make(map[string]struct{})
			}
			out[kind][value] = struct{}{}
		}
		return out
	}
	aByKind, bByKind := byKind(a), byKind(b)
	for kind, aValues := range aByKind {
		bValues := bByKind[kind]
		if bValues == nil {
			continue
		}
		if len(aValues) != len(bValues) {
			return true
		}
		for value := range aValues {
			if _, ok := bValues[value]; !ok {
				return true
			}
		}
	}
	return false
}

func singularPatternToken(token string) string {
	if len(token) > 5 && strings.HasSuffix(token, "ies") {
		return token[:len(token)-3] + "y"
	}
	if len(token) > 4 && strings.HasSuffix(token, "s") && !strings.HasSuffix(token, "ss") {
		return token[:len(token)-1]
	}
	return token
}

func isNumericToken(token string) bool {
	for _, r := range token {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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

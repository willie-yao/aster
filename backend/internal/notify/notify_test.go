package notify

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type fakeSender struct {
	messages []Message
	failNext int
}

func (f *fakeSender) Send(_ context.Context, message Message) error {
	if f.failNext > 0 {
		f.failNext--
		return errors.New("delivery failed")
	}
	f.messages = append(f.messages, message)
	return nil
}

func newTestNotifier(t *testing.T, sender Sender, stateFile string) *Notifier {
	t.Helper()
	from, to, err := ParseAddresses("Prow Dashboard <prow@example.com>", []string{"team@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return NewNotifier(sender, from, to, stateFile, "Example", "https://dash.example.com", "https://prow.example.com/view/", true)
}

func makeReport(failures ...models.TestFlakiness) models.FlakinessReport {
	return models.FlakinessReport{PersistentFailures: failures}
}

func persistentFailure(jobID, jobName, testName, hash string, count int) models.TestFlakiness {
	return models.TestFlakiness{
		JobID:               jobID,
		JobName:             jobName,
		TestName:            testName,
		ConsecutiveFailures: count,
		LastFailure: &models.TestFailureInfo{
			BuildID:        "100",
			FailureMessage: "context deadline exceeded",
			ErrorHash:      hash,
		},
	}
}

func TestStateSaveLoad(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	n := newTestNotifier(t, &fakeSender{}, stateFile)
	n.state.Notified["job1::test1"] = NotifiedFailure{ErrorHash: "abc", JobName: "job1", TestName: "test1"}
	if err := n.SaveState(); err != nil {
		t.Fatal(err)
	}

	n2 := newTestNotifier(t, &fakeSender{}, stateFile)
	if n2.state.Channel != notificationChannel || len(n2.state.Notified) != 1 {
		t.Fatalf("state = %+v", n2.state)
	}
}

func TestLegacyStateResetsForEmailChannel(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	legacy := NotificationState{Notified: map[string]NotifiedFailure{"job::test": {ErrorHash: "old"}}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(stateFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	n := newTestNotifier(t, &fakeSender{}, stateFile)
	if n.state.Channel != notificationChannel || len(n.state.Notified) != 0 {
		t.Fatalf("legacy state was not reset: %+v", n.state)
	}
}

func TestNewPersistentFailure(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestSomething", "hash1", 5)

	stats, err := n.ProcessFailures(context.Background(), makeReport(failure), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NewAlerts != 1 || stats.Failed != 0 || len(sender.messages) != 1 {
		t.Fatalf("stats=%+v messages=%d", stats, len(sender.messages))
	}
	if got := n.state.Notified["job-id::TestSomething"]; got.ErrorHash != "hash1" {
		t.Fatalf("state = %+v", got)
	}
}

func TestSameFailureIsNotRepeated(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestSomething", "hash1", 3)
	if _, err := n.ProcessFailures(context.Background(), makeReport(failure), nil); err != nil {
		t.Fatal(err)
	}
	failure.ConsecutiveFailures = 7
	stats, err := n.ProcessFailures(context.Background(), makeReport(failure), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NewAlerts != 0 || len(sender.messages) != 1 {
		t.Fatalf("stats=%+v messages=%d", stats, len(sender.messages))
	}
	if got := n.state.Notified["job-id::TestSomething"].ConsecutiveCount; got != 7 {
		t.Fatalf("consecutive count = %d", got)
	}
}

func TestChangedErrorSendsAgain(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestSomething", "hash1", 3)
	if _, err := n.ProcessFailures(context.Background(), makeReport(failure), nil); err != nil {
		t.Fatal(err)
	}
	failure.LastFailure.ErrorHash = "hash2"
	stats, err := n.ProcessFailures(context.Background(), makeReport(failure), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NewAlerts != 1 || len(sender.messages) != 2 {
		t.Fatalf("stats=%+v messages=%d", stats, len(sender.messages))
	}
	if !strings.Contains(sender.messages[1].Subject, "Failure changed") {
		t.Fatalf("subject = %q", sender.messages[1].Subject)
	}
}

func TestRecoverySendsAndDeletesState(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	n.state.Notified["job-id::TestRecovered"] = NotifiedFailure{
		ConsecutiveCount: 7,
		ErrorHash:        "hash",
		JobName:          "job",
		TestName:         "TestRecovered",
	}

	stats, err := n.ProcessFailures(context.Background(), makeReport(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Recoveries != 1 || len(sender.messages) != 1 {
		t.Fatalf("stats=%+v messages=%d", stats, len(sender.messages))
	}
	if len(n.state.Notified) != 0 {
		t.Fatalf("state not cleared: %+v", n.state.Notified)
	}
}

func TestBelowThresholdIgnored(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestSomething", "hash", 2)

	stats, err := n.ProcessFailures(context.Background(), makeReport(failure), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{}) || len(sender.messages) != 0 {
		t.Fatalf("stats=%+v messages=%d", stats, len(sender.messages))
	}
}

func TestJobIDSeparatesSameNames(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	one := persistentFailure("periodic/job", "same-job", "TestSame", "hash", 3)
	two := persistentFailure("presubmit/repo/1/job", "same-job", "TestSame", "hash", 3)

	stats, err := n.ProcessFailures(context.Background(), makeReport(one, two), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NewAlerts != 2 || len(n.state.Notified) != 2 {
		t.Fatalf("stats=%+v state=%+v", stats, n.state.Notified)
	}
}

func TestAILookupUsesNewestFailedRun(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestAI", "hash", 4)
	details := []models.JobDetail{{
		JobID: "job-id",
		Runs: []models.BuildResult{
			{TestCases: []models.TestCase{{Name: "TestAI", Status: "failed", AIAnalysis: &models.AIAnalysis{RootCause: "new root cause"}}}},
			{TestCases: []models.TestCase{{Name: "TestAI", Status: "failed", AIAnalysis: &models.AIAnalysis{RootCause: "old root cause"}}}},
		},
	}}

	if _, err := n.ProcessFailures(context.Background(), makeReport(failure), details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sender.messages[0].TextBody, "new root cause") || strings.Contains(sender.messages[0].TextBody, "old root cause") {
		t.Fatalf("body = %s", sender.messages[0].TextBody)
	}
}

func TestFailedNewAlertRetries(t *testing.T) {
	sender := &fakeSender{failNext: 1}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestRetry", "hash", 3)

	stats, err := n.ProcessFailures(context.Background(), makeReport(failure), nil)
	if err == nil || stats.Failed != 1 || len(n.state.Notified) != 0 {
		t.Fatalf("first pass stats=%+v err=%v state=%+v", stats, err, n.state.Notified)
	}
	stats, err = n.ProcessFailures(context.Background(), makeReport(failure), nil)
	if err != nil || stats.NewAlerts != 1 || len(n.state.Notified) != 1 {
		t.Fatalf("retry stats=%+v err=%v state=%+v", stats, err, n.state.Notified)
	}
}

func TestFailedChangedAlertPreservesOldHash(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestRetry", "old", 3)
	if _, err := n.ProcessFailures(context.Background(), makeReport(failure), nil); err != nil {
		t.Fatal(err)
	}
	sender.failNext = 1
	failure.LastFailure.ErrorHash = "new"
	if _, err := n.ProcessFailures(context.Background(), makeReport(failure), nil); err == nil {
		t.Fatal("expected delivery error")
	}
	if got := n.state.Notified["job-id::TestRetry"].ErrorHash; got != "old" {
		t.Fatalf("hash advanced after failed send: %q", got)
	}
}

func TestFailedRecoveryRetries(t *testing.T) {
	sender := &fakeSender{failNext: 1}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	n.state.Notified["job-id::TestRecovery"] = NotifiedFailure{JobName: "job", TestName: "TestRecovery", ConsecutiveCount: 4}

	if _, err := n.ProcessFailures(context.Background(), makeReport(), nil); err == nil {
		t.Fatal("expected delivery error")
	}
	if len(n.state.Notified) != 1 {
		t.Fatal("failed recovery was removed from state")
	}
	stats, err := n.ProcessFailures(context.Background(), makeReport(), nil)
	if err != nil || stats.Recoveries != 1 || len(n.state.Notified) != 0 {
		t.Fatalf("retry stats=%+v err=%v state=%+v", stats, err, n.state.Notified)
	}
}

func TestProcessContinuesAfterFailure(t *testing.T) {
	sender := &fakeSender{failNext: 1}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	one := persistentFailure("a", "job-a", "TestA", "hash", 3)
	two := persistentFailure("b", "job-b", "TestB", "hash", 3)

	stats, err := n.ProcessFailures(context.Background(), makeReport(one, two), nil)
	if err == nil || stats.Failed != 1 || stats.NewAlerts != 1 || len(sender.messages) != 1 {
		t.Fatalf("stats=%+v err=%v messages=%d", stats, err, len(sender.messages))
	}
}

func TestParseAddresses(t *testing.T) {
	from, recipients, err := ParseAddresses("Dashboard <dashboard@example.com>", []string{"One <one@example.com>", "two@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if from.Address != "dashboard@example.com" || len(recipients) != 2 || recipients[0].Address != "one@example.com" {
		t.Fatalf("from=%+v recipients=%+v", from, recipients)
	}
	if _, _, err := ParseAddresses("bad", []string{"one@example.com"}); err == nil {
		t.Fatal("expected invalid sender error")
	}
}

var _ Sender = (*fakeSender)(nil)

func systemicPattern(id, jobID, subject string) models.PatternAnalysis {
	return models.PatternAnalysis{
		ID:              id,
		JobID:           jobID,
		Subject:         subject,
		Systemic:        true,
		Confidence:      "high",
		BuildsAnalyzed:  5,
		SharedRootCause: "shared controller timeout",
		SuggestedFix:    "increase the controller timeout",
	}
}

func TestSystemicPatternSendsActionableEmailOnce(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	report := makeReport()
	report.RecurringPatterns = []models.PatternAnalysis{systemicPattern("pattern-1", "job-id", "periodic-job")}

	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PatternAlerts != 1 || len(sender.messages) != 1 {
		t.Fatalf("stats=%+v messages=%d", stats, len(sender.messages))
	}
	message := sender.messages[0]
	for _, want := range []string{"Systemic recurring failure", "Review issue draft", "Review fix proposal", "failure=pattern-1", "action=create-issue", "action=propose-fix"} {
		if !strings.Contains(message.Subject+message.TextBody, want) {
			t.Errorf("message missing %q: subject=%q body=%s", want, message.Subject, message.TextBody)
		}
	}
	if _, ok := n.state.Patterns["job-id"]; !ok {
		t.Fatal("pattern notification state was not recorded")
	}

	stats, err = n.ProcessFailures(context.Background(), report, nil)
	if err != nil || stats.PatternAlerts != 0 || len(sender.messages) != 1 {
		t.Fatalf("repeat stats=%+v err=%v messages=%d", stats, err, len(sender.messages))
	}
}

func TestPatternEmailOmitsActionsWhenDisabled(t *testing.T) {
	sender := &fakeSender{}
	from, to, err := ParseAddresses("Prow Dashboard <prow@example.com>", []string{"team@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	n := NewNotifier(sender, from, to, filepath.Join(t.TempDir(), "state.json"), "Example", "https://dash.example.com", "https://prow.example.com/view/", false)
	report := makeReport()
	report.RecurringPatterns = []models.PatternAnalysis{systemicPattern("pattern-1", "job-id", "periodic-job")}

	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PatternAlerts != 0 || len(sender.messages) != 0 || len(n.state.Patterns) != 0 {
		t.Fatalf("disabled action links sent pattern email: stats=%+v messages=%d state=%+v", stats, len(sender.messages), n.state.Patterns)
	}
}

func TestFailedPatternEmailRetries(t *testing.T) {
	sender := &fakeSender{failNext: 1}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	report := makeReport()
	report.RecurringPatterns = []models.PatternAnalysis{systemicPattern("pattern-1", "job-id", "periodic-job")}

	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err == nil || stats.Failed != 1 || len(n.state.Patterns) != 0 {
		t.Fatalf("first stats=%+v err=%v state=%+v", stats, err, n.state.Patterns)
	}
	stats, err = n.ProcessFailures(context.Background(), report, nil)
	if err != nil || stats.PatternAlerts != 1 || len(n.state.Patterns) != 1 {
		t.Fatalf("retry stats=%+v err=%v state=%+v", stats, err, n.state.Patterns)
	}
}

func TestCurrentEmailStateInitializesPatternMap(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	state := NotificationState{Channel: notificationChannel, Notified: map[string]NotifiedFailure{}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(stateFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	n := newTestNotifier(t, &fakeSender{}, stateFile)
	if n.state.Patterns == nil {
		t.Fatal("pattern state map was not initialized")
	}
}

func TestPatternNotificationComputesMissingIDAndSkipsNonSystemic(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	pattern := systemicPattern("", "job-id", "periodic-job")
	notSystemic := pattern
	notSystemic.JobID = "other-job"
	notSystemic.Systemic = false
	report := makeReport()
	report.RecurringPatterns = []models.PatternAnalysis{pattern, notSystemic}

	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PatternAlerts != 1 || len(n.state.Patterns) != 1 || len(sender.messages) != 1 {
		t.Fatalf("stats=%+v state=%+v messages=%d", stats, n.state.Patterns, len(sender.messages))
	}
	for jobID, state := range n.state.Patterns {
		if jobID != "job-id" || state.PatternID == "" {
			t.Fatal("computed pattern id is empty")
		}
	}
}

func TestPatternWordingDriftDoesNotResend(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	pattern := systemicPattern("pattern-1", "job-id", "periodic-job")
	pattern.SharedRootCause = "clusterctl upgrade removes the Azure Service Operator pods before the replacement webhook service is ready, causing conversion webhook calls to fail with connection refused"
	report := makeReport()
	report.RecurringPatterns = []models.PatternAnalysis{pattern}
	if _, err := n.ProcessFailures(context.Background(), report, nil); err != nil {
		t.Fatal(err)
	}

	paraphrased := pattern
	paraphrased.ID = "pattern-2"
	paraphrased.SharedRootCause = "the Azure Service Operator replacement webhook is not ready when clusterctl upgrade removes the old pods, so conversion calls return connection refused"
	report.RecurringPatterns = []models.PatternAnalysis{paraphrased}
	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil || stats.PatternAlerts != 0 || len(sender.messages) != 1 {
		t.Fatalf("stats=%+v err=%v messages=%d", stats, err, len(sender.messages))
	}
	state := n.state.Patterns["job-id"]
	if state.PatternID != "pattern-2" || state.SharedRootCause != paraphrased.SharedRootCause {
		t.Fatalf("state was not refreshed: %+v", state)
	}
}

func TestMateriallyChangedPatternSendsAgain(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	pattern := systemicPattern("pattern-1", "job-id", "periodic-job")
	pattern.SharedRootCause = "the Azure Service Operator replacement webhook is unavailable during clusterctl upgrade and conversion calls fail with connection refused"
	report := makeReport()
	report.RecurringPatterns = []models.PatternAnalysis{pattern}
	if _, err := n.ProcessFailures(context.Background(), report, nil); err != nil {
		t.Fatal(err)
	}

	changed := pattern
	changed.ID = "pattern-2"
	changed.SharedRootCause = "the job omits required environment variables for the cluster template, so clusterctl exits before creating a workload cluster"
	report.RecurringPatterns = []models.PatternAnalysis{changed}
	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil || stats.PatternAlerts != 1 || len(sender.messages) != 2 {
		t.Fatalf("stats=%+v err=%v messages=%d", stats, err, len(sender.messages))
	}
	message := sender.messages[1]
	for _, want := range []string{"Recurring failure changed", "Previous shared root cause", "Current shared root cause", pattern.SharedRootCause, changed.SharedRootCause} {
		if !strings.Contains(message.Subject+message.TextBody+message.HTMLBody, want) {
			t.Fatalf("changed message missing %q: subject=%q body=%s", want, message.Subject, message.TextBody)
		}
	}
}

func TestFailedChangedPatternEmailRetries(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	pattern := systemicPattern("pattern-1", "job-id", "periodic-job")
	pattern.SharedRootCause = "webhook connection refused while replacing Azure Service Operator pods"
	report := makeReport()
	report.RecurringPatterns = []models.PatternAnalysis{pattern}
	if _, err := n.ProcessFailures(context.Background(), report, nil); err != nil {
		t.Fatal(err)
	}

	changed := pattern
	changed.ID = "pattern-2"
	changed.SharedRootCause = "required cluster template environment variables are missing"
	report.RecurringPatterns = []models.PatternAnalysis{changed}
	sender.failNext = 1
	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err == nil || stats.Failed != 1 || n.state.Patterns["job-id"].PatternID != "pattern-1" {
		t.Fatalf("failed change stats=%+v err=%v state=%+v", stats, err, n.state.Patterns)
	}
	stats, err = n.ProcessFailures(context.Background(), report, nil)
	if err != nil || stats.PatternAlerts != 1 || n.state.Patterns["job-id"].PatternID != "pattern-2" {
		t.Fatalf("retry stats=%+v err=%v state=%+v", stats, err, n.state.Patterns)
	}
}

func TestFailedChangedLegacyPatternEmailRetainsRetryState(t *testing.T) {
	sender := &fakeSender{failNext: 1}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	n.state.Patterns["legacy-pattern-id"] = NotifiedPattern{
		PatternID: "legacy-pattern-id", JobID: "job-id", Subject: "periodic-job",
		SharedRootCause: "webhook connection refused while replacing controller pods",
	}
	changed := systemicPattern("new-pattern-id", "job-id", "periodic-job")
	changed.SharedRootCause = "required cluster template environment variables are missing"
	report := makeReport()
	report.RecurringPatterns = []models.PatternAnalysis{changed}
	details := []models.JobDetail{{
		JobID: "job-id", Name: "periodic-job", Runs: failedRuns(3),
		PatternAnalyses: []models.PatternAnalysis{changed},
	}}
	stats, err := n.ProcessFailures(context.Background(), report, details)
	if err == nil || stats.Failed != 1 {
		t.Fatalf("failed change stats=%+v err=%v", stats, err)
	}
	if _, ok := n.state.Patterns["legacy-pattern-id"]; !ok {
		t.Fatalf("failed delivery lost legacy retry state: %+v", n.state.Patterns)
	}
	stats, err = n.ProcessFailures(context.Background(), report, details)
	if err != nil || stats.PatternAlerts != 1 {
		t.Fatalf("retry stats=%+v err=%v", stats, err)
	}
	if len(n.state.Patterns) != 1 || n.state.Patterns["job-id"].PatternID != "new-pattern-id" {
		t.Fatalf("retry did not collapse state: %+v", n.state.Patterns)
	}
}

func TestPatternStateMigrationPrefersCurrentPatternID(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	state := NotificationState{
		Channel:  notificationChannel,
		Notified: map[string]NotifiedFailure{},
		Patterns: map[string]NotifiedPattern{
			"000-old-pattern": {PatternID: "000-old-pattern", JobID: "job-id", Subject: "periodic-job", SharedRootCause: "required environment variables are missing"},
			"zzz-current":     {PatternID: "zzz-current", JobID: "job-id", Subject: "periodic-job", SharedRootCause: "webhook connection refused"},
		},
	}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(stateFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, stateFile)
	pattern := systemicPattern("zzz-current", "job-id", "periodic-job")
	pattern.SharedRootCause = "webhook connection refused"
	report := makeReport()
	report.RecurringPatterns = []models.PatternAnalysis{pattern}
	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil || stats.PatternAlerts != 0 || len(sender.messages) != 0 {
		t.Fatalf("stats=%+v err=%v messages=%d", stats, err, len(sender.messages))
	}
	if len(n.state.Patterns) != 1 {
		t.Fatalf("legacy candidates were not collapsed: %+v", n.state.Patterns)
	}
	if got := n.state.Patterns["job-id"]; got.PatternID != "zzz-current" {
		t.Fatalf("migrated state = %+v", n.state.Patterns)
	}
}

func TestPatternStateMigrationUsesClosestLegacyCause(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	state := NotificationState{
		Channel:  notificationChannel,
		Notified: map[string]NotifiedFailure{},
		Patterns: map[string]NotifiedPattern{
			"job-id":         {PatternID: "job-scoped-old", JobID: "job-id", Subject: "periodic-job", SharedRootCause: "required environment variables are missing"},
			"old-webhook-id": {PatternID: "old-webhook-id", JobID: "job-id", Subject: "periodic-job", SharedRootCause: "replacement webhook is unavailable and conversion calls return connection refused"},
		},
	}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(stateFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, stateFile)
	pattern := systemicPattern("new-webhook-id", "job-id", "periodic-job")
	pattern.SharedRootCause = "conversion calls fail with connection refused because the replacement webhook is unavailable"
	report := makeReport()
	report.RecurringPatterns = []models.PatternAnalysis{pattern}
	stats, err := n.ProcessFailures(context.Background(), report, nil)
	if err != nil || stats.PatternAlerts != 0 || len(sender.messages) != 0 {
		t.Fatalf("stats=%+v err=%v messages=%d", stats, err, len(sender.messages))
	}
	if got := n.state.Patterns["job-id"]; got.PatternID != "new-webhook-id" {
		t.Fatalf("migrated state = %+v", n.state.Patterns)
	}
}

func failedRuns(count int) []models.BuildResult {
	runs := make([]models.BuildResult, count)
	for i := range runs {
		runs[i] = models.BuildResult{BuildInfo: models.BuildInfo{Passed: false, Result: "FAILURE"}}
	}
	return runs
}

func TestPatternStateClearsAfterAuthoritativeRecovery(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	pattern := systemicPattern("pattern-1", "job-id", "periodic-job")
	report := makeReport()
	report.RecurringPatterns = []models.PatternAnalysis{pattern}
	details := []models.JobDetail{{JobID: "job-id", Name: "periodic-job", Runs: failedRuns(3), PatternAnalyses: []models.PatternAnalysis{pattern}}}

	if _, err := n.ProcessFailures(context.Background(), report, details); err != nil {
		t.Fatal(err)
	}
	if len(n.state.Patterns) != 1 {
		t.Fatalf("pattern state = %+v", n.state.Patterns)
	}

	nonSystemic := pattern
	nonSystemic.Systemic = false
	clearDetails := []models.JobDetail{{JobID: "job-id", Name: "periodic-job", Runs: failedRuns(3), PatternAnalyses: []models.PatternAnalysis{nonSystemic}}}
	if _, err := n.ProcessFailures(context.Background(), makeReport(), clearDetails); err != nil {
		t.Fatal(err)
	}
	if len(n.state.Patterns) != 0 {
		t.Fatalf("recovered pattern remained deduped: %+v", n.state.Patterns)
	}

	stats, err := n.ProcessFailures(context.Background(), report, details)
	if err != nil || stats.PatternAlerts != 1 || len(sender.messages) != 2 {
		t.Fatalf("recurrence stats=%+v err=%v messages=%d", stats, err, len(sender.messages))
	}
}

func TestPatternStateSurvivesUnavailableAnalysis(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	pattern := systemicPattern("pattern-1", "job-id", "periodic-job")
	report := makeReport()
	report.RecurringPatterns = []models.PatternAnalysis{pattern}
	details := []models.JobDetail{{JobID: "job-id", Name: "periodic-job", Runs: failedRuns(3), PatternAnalyses: []models.PatternAnalysis{pattern}}}
	if _, err := n.ProcessFailures(context.Background(), report, details); err != nil {
		t.Fatal(err)
	}

	unavailable := []models.JobDetail{{JobID: "job-id", Name: "periodic-job", Runs: failedRuns(3)}}
	if _, err := n.ProcessFailures(context.Background(), makeReport(), unavailable); err != nil {
		t.Fatal(err)
	}
	if len(n.state.Patterns) != 1 {
		t.Fatalf("unavailable analysis cleared pattern state: %+v", n.state.Patterns)
	}
}

func TestPatternsMateriallyDifferentToleratesObservedWordingDrift(t *testing.T) {
	webhookLong := "clusterctl upgrade list-CRD validation cannot reach the Azure Service Operator webhook because the ASO service is unavailable during upgrade sequencing, yielding conversion webhook connection refused while the client rate limiter retries"
	webhookShort := "clusterctl upgrade removes Azure Service Operator pods before the replacement webhook service is ready, causing conversion webhook calls to fail with connection refused while the rate limiter retries"
	rateLimit := "clusterctl upgrade uses too-low client-side rate limits while listing ASO-managed Azure resource types during provider validation"
	if patternsMateriallyDifferent(webhookLong, webhookShort) {
		t.Fatal("webhook paraphrases were treated as different patterns")
	}
	if !patternsMateriallyDifferent(webhookShort, rateLimit) {
		t.Fatal("webhook outage and rate-limit causes were treated as the same pattern")
	}
}

func TestPatternsMateriallyDifferentPreservesNumericSignals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previous string
		current  string
	}{
		{name: "http status", previous: "identity endpoint returns HTTP 401", current: "identity endpoint returns HTTP 500"},
		{name: "http status code", previous: "identity endpoint returns HTTP status code 401", current: "identity endpoint returns HTTP status code 500"},
		{name: "bare status code", previous: "identity endpoint returns status code 401", current: "identity endpoint returns status code 500"},
		{name: "http protocol", previous: "identity endpoint returns HTTP/1.1 401", current: "identity endpoint returns HTTP/1.1 500"},
		{name: "http colon", previous: "identity endpoint returns HTTP status: 401", current: "identity endpoint returns HTTP status: 500"},
		{name: "tls version", previous: "server requires TLS 1.2", current: "server requires TLS 1.3"},
		{name: "service port", previous: "controller cannot connect to port 443", current: "controller cannot connect to port 8443"},
		{name: "endpoint port", previous: "controller cannot connect to api.example.com:443", current: "controller cannot connect to api.example.com:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !patternsMateriallyDifferent(tc.previous, tc.current) {
				t.Fatalf("numeric change was ignored: %q -> %q", tc.previous, tc.current)
			}
		})
	}
	if patternsMateriallyDifferent("build 2079080542820634624 returns HTTP 500", "build 2078331846319411200 returns HTTP 500") {
		t.Fatal("volatile build IDs caused a material change")
	}
	if patternsMateriallyDifferent("failed at 2026-07-20 10:30:00", "failed at 2026-07-20 10:45:00") {
		t.Fatal("timestamp values caused a material change")
	}
	if patternsMateriallyDifferent("the token is expired and the endpoint returns HTTP 401", "the endpoint rejects requests because the token is expired") {
		t.Fatal("an omitted numeric detail caused a material change")
	}
}

func TestPatternsMateriallyDifferentWeightsSharedSymptoms(t *testing.T) {
	certificate := "webhook conversion calls fail because certificate expired"
	endpoint := "webhook conversion calls fail because service endpoint unavailable"
	if !patternsMateriallyDifferent(certificate, endpoint) {
		t.Fatal("distinct mechanisms sharing symptom vocabulary were treated as the same pattern")
	}
}

func TestPatternsMateriallyDifferentDiscountsSharedComponents(t *testing.T) {
	certificate := "Azure Service Operator webhook certificate expired"
	endpoint := "Azure Service Operator webhook endpoint unavailable"
	if !patternsMateriallyDifferent(certificate, endpoint) {
		t.Fatal("component vocabulary hid a material mechanism change")
	}
}

func TestPatternsMateriallyDifferentDetectsPolarityReversal(t *testing.T) {
	if !patternsMateriallyDifferent("API accepts requests", "API does not accept requests") {
		t.Fatal("negative polarity was treated as unchanged")
	}
	if !patternsMateriallyDifferent("API does not accept requests", "API accepts requests") {
		t.Fatal("positive polarity was treated as unchanged")
	}
	if !patternsMateriallyDifferent("API is available", "API isn't available") {
		t.Fatal("contracted negative polarity was treated as unchanged")
	}
	if !patternsMateriallyDifferent("API can accept requests", "API cannot accept requests") {
		t.Fatal("uncontracted negative polarity was treated as unchanged")
	}
	if !patternsMateriallyDifferent("API accepts requests", "API fails to accept requests") {
		t.Fatal("fail-to negative polarity was treated as unchanged")
	}
	if !patternsMateriallyDifferent("API is available", "API is no longer available") {
		t.Fatal("no-longer polarity was treated as unchanged")
	}
	if patternsMateriallyDifferent("webhook is not available", "webhook is unavailable") {
		t.Fatal("equivalent negative phrasing was treated as changed")
	}
	if patternsMateriallyDifferent("API does not accept requests", "API doesn't accept requests") {
		t.Fatal("equivalent contracted negation was treated as changed")
	}
	if patternsMateriallyDifferent("API cannot accept requests", "API can't accept requests") {
		t.Fatal("cannot and can't were treated as different patterns")
	}
	if patternsMateriallyDifferent("API does not accept requests", "API fails to accept requests") {
		t.Fatal("equivalent fail-to negation was treated as changed")
	}
	if patternsMateriallyDifferent("upgrade removes pods before the webhook is ready", "upgrade finds the webhook is not ready after removing pods") {
		t.Fatal("equivalent readiness phrasing was treated as changed")
	}
}

func TestPatternStateWithEmptyJobIDSurvivesUnavailableAnalysis(t *testing.T) {
	n := newTestNotifier(t, &fakeSender{}, filepath.Join(t.TempDir(), "state.json"))
	n.state.Patterns["job-id"] = NotifiedPattern{
		PatternID: "pattern-id", Subject: "periodic-job", SharedRootCause: "webhook unavailable",
	}
	unavailable := []models.JobDetail{{JobID: "job-id", Name: "periodic-job", Runs: failedRuns(3)}}
	if _, err := n.ProcessFailures(context.Background(), makeReport(), unavailable); err != nil {
		t.Fatal(err)
	}
	if _, ok := n.state.Patterns["job-id"]; !ok {
		t.Fatalf("unavailable analysis cleared state with empty JobID: %+v", n.state.Patterns)
	}
}

package fetcher

import (
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/patterns"
	"github.com/willie-yao/aster/backend/internal/recurrenceledger"
)

func signedDetail(jobID, signature string, builds ...string) models.JobDetail {
	return models.JobDetail{
		JobID: jobID, Name: jobID,
		PatternAnalyses: []models.PatternAnalysis{{
			JobID: jobID, Systemic: true,
			CausalGroups: []models.PatternCausalGroup{{
				Builds: builds, RootCause: "cause", Confidence: "high", Signature: signature,
			}},
		}},
	}
}

func observe(dir string, now time.Time, details ...models.JobDetail) {
	observeRecurrence(dir, details, now)
}

// Recurring failures previously had no memory: a cause whose builds aged out and
// later returned looked brand new. The ledger has to carry that history across
// passes even though the pattern object is rebuilt from the current window.
func TestRecordRecurrenceSurvivesTheWindowRolling(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	observe(dir, start, signedDetail("job-1", "sig-a", "10", "11"))
	first := recurrenceledger.Load(dir).Entries["sig-a"]
	if first.Occurrences != 2 || first.JobID != "job-1" {
		t.Fatalf("first pass entry=%+v", first)
	}

	// The cause leaves the window entirely for a month.
	observe(dir, start.AddDate(0, 1, 0), signedDetail("job-1", "sig-b", "40"))

	// It returns months later in builds the original pass never saw.
	observe(dir, start.AddDate(0, 3, 0), signedDetail("job-1", "sig-a", "90", "91"))

	entry := recurrenceledger.Load(dir).Entries["sig-a"]
	if entry.Occurrences != 4 {
		t.Fatalf("occurrences=%d, want 4 accumulated across the gap", entry.Occurrences)
	}
	if entry.FirstSeen != start.Format(time.RFC3339) {
		t.Fatalf("first seen=%q, want the original sighting", entry.FirstSeen)
	}
	if entry.LastSeen != start.AddDate(0, 3, 0).Format(time.RFC3339) || entry.Watermark != "91" {
		t.Fatalf("entry=%+v", entry)
	}
}

// Re-publishing the same retained verdict every pass must not look like the
// failure is recurring over and over.
func TestRecordRecurrenceDoesNotInflateRetainedPatterns(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for pass := range 5 {
		observe(dir, now.Add(time.Duration(pass)*time.Hour), signedDetail("job-1", "sig-a", "10", "11"))
	}
	if got := recurrenceledger.Load(dir).Entries["sig-a"].Occurrences; got != 2 {
		t.Fatalf("occurrences=%d, want the two failing builds counted once", got)
	}
}

// Memory older than the retention window must not come back to life just because
// the cause did. Pruning therefore runs before the pass records what it sees.
func TestRecordRecurrenceRetiresMemoryOlderThanTheRetentionWindow(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	observe(dir, start, signedDetail("job-1", "sig-a", "10"))
	if err := recurrenceledger.Update(dir, func(ledger *recurrenceledger.Ledger) bool {
		return ledger.RecordVerdict("sig-a", recurrenceledger.Verdict{
			State: models.PatternRemediationEnvironmentOrInfrastructure, RecordedAt: start.Format(time.RFC3339),
		}, start)
	}); err != nil {
		t.Fatal(err)
	}
	if verdict, ok := recurrenceledger.Load(dir).ReusableVerdict("sig-a", start); !ok || verdict.State == "" {
		t.Fatal("the baseline verdict is not reusable")
	}

	returned := start.Add(recurrenceledger.RetentionWindow + 24*time.Hour)
	observe(dir, returned, signedDetail("job-1", "sig-a", "90"))

	entry := recurrenceledger.Load(dir).Entries["sig-a"]
	if entry.Verdict != nil {
		t.Fatalf("entry=%+v, want the expired verdict retired", entry)
	}
	if entry.FirstSeen != returned.Format(time.RFC3339) || entry.Occurrences != 1 {
		t.Fatalf("entry=%+v, want the returning cause to start fresh", entry)
	}
}

// Pruning must not depend on the pass having anything to record, or a project
// that goes quiet keeps expired memory forever.
func TestRecordRecurrencePrunesEvenWithNoSightings(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	observe(dir, start, signedDetail("job-1", "sig-a", "10"))

	recordRecurrence(dir, nil, start.Add(recurrenceledger.RetentionWindow+24*time.Hour))
	if len(recurrenceledger.Load(dir).Entries) != 0 {
		t.Fatal("an expired cause survived a pass with no sightings")
	}
}

func TestCausalGroupSightingsCoversOnlySignedSystemicCauses(t *testing.T) {
	nonSystemic := signedDetail("job-3", "sig-c", "30")
	nonSystemic.PatternAnalyses[0].Systemic = false

	for name, detail := range map[string]models.JobDetail{
		"unsigned group": signedDetail("job-2", "", "20"),
		"non systemic":   nonSystemic,
		"no analyses":    {JobID: "job-4"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := causalGroupSightings(&detail); len(got) != 0 {
				t.Fatalf("sightings=%+v, want none without a signed systemic cause", got)
			}
		})
	}

	systemic := signedDetail("job-1", "sig-a", "10")
	got := causalGroupSightings(&systemic)
	if len(got) != 1 {
		t.Fatalf("sightings=%+v, want the signed systemic cause", got)
	}
	if got[0].Signature != "sig-a" || got[0].JobID != "job-1" || got[0].Subject != "job-1" || len(got[0].Builds) != 1 {
		t.Fatalf("sighting=%+v", got[0])
	}
}

func TestRecordRecurrenceWritesNothingWithoutSignedCauses(t *testing.T) {
	dir := t.TempDir()
	observe(dir, time.Now(), models.JobDetail{JobID: "job-1"})
	if len(recurrenceledger.Load(dir).Entries) != 0 {
		t.Fatal("a pass with no signed causes wrote ledger entries")
	}
}

func failingDetail(jobID, buildID, testName, message string) models.JobDetail {
	return models.JobDetail{JobID: jobID, Name: jobID, Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: buildID, Result: "FAILURE", Passed: false},
		TestCases: []models.TestCase{{
			Name: testName, Status: "failed", FailureMessage: message,
			AISummary:  &models.AISummary{Summary: "failure"},
			AIAnalysis: &models.AIAnalysis{RootCause: "cause", Severity: "High", Mode: "agentic"},
		}},
	}}}
}

// correlatedDetail is a job whose failures were dense enough in one window to be
// grouped, with signatures assigned the way a real pass assigns them.
func correlatedDetail(jobID, message string, builds map[string]string, grouped ...string) models.JobDetail {
	detail := models.JobDetail{JobID: jobID, Name: jobID}
	for _, buildID := range grouped {
		detail.Runs = append(detail.Runs, failingDetail(jobID, buildID, builds[buildID], message).Runs...)
	}
	detail.PatternAnalyses = []models.PatternAnalysis{{
		JobID: jobID, Systemic: true,
		CausalGroups: []models.PatternCausalGroup{{
			Builds: grouped, RootCause: "cause", Confidence: "high",
		}},
	}}
	patterns.ApplyCausalGroupSignatures(&detail)
	return detail
}

// Correlation needs patterns.MinFailedBuilds failures inside a single window, so a
// job that fails once per window never forms a pattern and used to build no
// durable history at all. Exactly the long-lived infrequent flakes that most need
// it were the ones getting no memory.
func TestRecordRecurrenceAccumulatesFailuresTooSparseToCorrelate(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	for pass, buildID := range []string{"10", "40", "90"} {
		detail := failingDetail("job-1", buildID, "TestReconcile", "context deadline exceeded")
		if patterns.IsEligible(&detail) {
			t.Fatal("the fixture correlates, so it does not exercise the sparse path")
		}
		observe(dir, start.AddDate(0, pass, 0), detail)
	}

	entries := recurrenceledger.Load(dir).Entries
	if len(entries) != 1 {
		t.Fatalf("entries=%+v, want one durable cause", entries)
	}
	for _, entry := range entries {
		if entry.Occurrences != 3 {
			t.Fatalf("occurrences=%d, want the three sparse failures accumulated", entry.Occurrences)
		}
		if entry.FirstSeen != start.Format(time.RFC3339) {
			t.Fatalf("first seen=%q, want the earliest sighting", entry.FirstSeen)
		}
	}
}

// A cause that finally fails often enough to correlate must continue the history
// its isolated failures already built. Restarting the count when a flake becomes
// frequent would discard exactly the long-run evidence that makes it worth
// investigating.
func TestRecordRecurrenceCarriesSparseHistoryIntoTheCausalGroup(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	sparse := failingDetail("job-1", "10", "TestReconcile", "Timed out after 3600.001s")
	observe(dir, start, sparse)

	signature := patterns.BuildRecurrenceSignature(sparse, &sparse.Runs[0])
	if signature == "" {
		t.Fatal("the isolated failure produced no recurrence signature")
	}
	if got := recurrenceledger.Load(dir).Entries[signature].Occurrences; got != 1 {
		t.Fatalf("occurrences=%d, want the isolated failure recorded", got)
	}

	// The same failure now fails often enough in one window to correlate, and its
	// message carries a duration that differs on every run.
	correlated := correlatedDetail("job-1", "Timed out after 3612.487s", map[string]string{
		"20": "TestReconcile", "21": "TestReconcile", "22": "TestReconcile",
	}, "20", "21", "22")
	observe(dir, start.AddDate(0, 1, 0), correlated)

	if got := recurrenceledger.Load(dir).Entries[signature].Occurrences; got != 4 {
		t.Fatalf("occurrences=%d, want one isolated plus three correlated failures", got)
	}
}

// Recurrence and verdict reuse need opposite trade-offs on the same failure, so a
// grouped build has to advance its recurrence identity as well as the group's
// signature. Leaving the count to the group alone would tie it to an identity
// that changes whenever the message's numbers do.
func TestObserveRecurrenceCountsGroupedBuildsUnderBothIdentities(t *testing.T) {
	dir := t.TempDir()
	detail := correlatedDetail("job-1", "context deadline exceeded", map[string]string{
		"20": "TestReconcile", "21": "TestReconcile", "22": "TestReconcile",
	}, "20", "21", "22")
	groupSignature := detail.PatternAnalyses[0].CausalGroups[0].Signature
	if groupSignature == "" {
		t.Fatal("the correlated fixture has no group signature")
	}
	recurrenceSignature := patterns.BuildRecurrenceSignature(detail, &detail.Runs[0])
	if recurrenceSignature == groupSignature {
		t.Fatal("both identities hash alike, so this asserts one entry twice")
	}

	observeRecurrence(dir, []models.JobDetail{detail}, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))

	entries := recurrenceledger.Load(dir).Entries
	if got := entries[groupSignature].Occurrences; got != 3 {
		t.Fatalf("group occurrences=%d, want the verdict identity still counted", got)
	}
	if got := entries[recurrenceSignature].Occurrences; got != 3 {
		t.Fatalf("recurrence occurrences=%d, want every grouped build counted", got)
	}
}

// The ledger is private operational state, so history a maintainer never sees
// cannot help them judge whether a flake is new or years old.
func TestObserveRecurrencePublishesHistoryOntoTheJob(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	observe(dir, start, failingDetail("job-1", "10", "TestReconcile", "context deadline exceeded"))

	details := []models.JobDetail{failingDetail("job-1", "20", "TestReconcile", "context deadline exceeded")}
	observeRecurrence(dir, details, start.AddDate(0, 1, 0))

	recurrence := details[0].FailureRecurrence
	if len(recurrence) != 1 {
		t.Fatalf("recurrence=%+v, want the observed cause published", recurrence)
	}
	if recurrence[0].Occurrences != 2 {
		t.Fatalf("occurrences=%d, want history reaching past this window", recurrence[0].Occurrences)
	}
	if len(recurrence[0].Builds) != 1 || recurrence[0].Builds[0] != "20" {
		t.Fatalf("builds=%+v, want only the build this window shows", recurrence[0].Builds)
	}
	if recurrence[0].FirstSeen != start.Format(time.RFC3339) {
		t.Fatalf("first seen=%q, want the original sighting", recurrence[0].FirstSeen)
	}
}

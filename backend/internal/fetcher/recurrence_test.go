package fetcher

import (
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
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
	recordRecurrence(dir, causalGroupSightings(details), now)
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
	systemic := signedDetail("job-1", "sig-a", "10")
	unsigned := signedDetail("job-2", "", "20")
	nonSystemic := signedDetail("job-3", "sig-c", "30")
	nonSystemic.PatternAnalyses[0].Systemic = false

	sightings := causalGroupSightings([]models.JobDetail{systemic, unsigned, nonSystemic, {JobID: "job-4"}})
	if len(sightings) != 1 {
		t.Fatalf("sightings=%+v, want only the signed systemic cause", sightings)
	}
	got := sightings[0]
	if got.Signature != "sig-a" || got.JobID != "job-1" || got.Subject != "job-1" || len(got.Builds) != 1 {
		t.Fatalf("sighting=%+v", got)
	}
}

func TestRecordRecurrenceWritesNothingWithoutSignedCauses(t *testing.T) {
	dir := t.TempDir()
	observe(dir, time.Now(), models.JobDetail{JobID: "job-1"})
	if len(recurrenceledger.Load(dir).Entries) != 0 {
		t.Fatal("a pass with no signed causes wrote ledger entries")
	}
}

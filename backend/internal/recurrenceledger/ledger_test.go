package recurrenceledger

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

func record(l *Ledger, signature string, verdict Verdict, now time.Time) bool {
	return l.RecordVerdict(signature, verdict, now)
}

func at(day int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day)
}

func TestObserveRecordsFirstSightingAndCountsFailingBuilds(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	if !ledger.Observe([]Sighting{{
		Signature: "sig-a", JobID: "job-1", Subject: "periodic-capz", Builds: []string{"10", "12"},
	}}, at(0)) {
		t.Fatal("first sighting reported no change")
	}
	entry := ledger.Entries["sig-a"]
	if entry.Occurrences != 2 || entry.Watermark != "12" {
		t.Fatalf("entry=%+v", entry)
	}
	if entry.FirstSeen != at(0).Format(time.RFC3339) || entry.LastSeen != entry.FirstSeen {
		t.Fatalf("timestamps=%+v", entry)
	}
	if entry.JobID != "job-1" || entry.Subject != "periodic-capz" {
		t.Fatalf("identity=%+v", entry)
	}
}

// A retained pattern is re-observed on every pass with the same builds. Counting
// passes rather than builds would make an idle cause look like it keeps recurring.
func TestObserveDoesNotInflateOnRepeatedSightingsOfTheSameBuilds(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	sighting := Sighting{Signature: "sig-a", JobID: "job-1", Builds: []string{"10", "12"}}
	ledger.Observe([]Sighting{sighting}, at(0))
	if ledger.Observe([]Sighting{sighting}, at(1)) {
		t.Fatal("re-observing the same builds reported a change")
	}
	entry := ledger.Entries["sig-a"]
	if entry.Occurrences != 2 {
		t.Fatalf("occurrences=%d, want 2", entry.Occurrences)
	}
	if entry.LastSeen != at(0).Format(time.RFC3339) {
		t.Fatalf("last seen advanced without a new failing build: %q", entry.LastSeen)
	}
}

// The whole point of the ledger: a cause whose builds age out and later returns
// keeps one identity and one continuous history.
func TestObserveSurvivesTheBuildWindowRolling(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	ledger.Observe([]Sighting{{Signature: "sig-a", JobID: "job-1", Builds: []string{"10", "11"}}}, at(0))
	// The window rolls past the original builds and the cause disappears.
	ledger.Observe(nil, at(30))
	// Months later the same cause returns in new builds.
	ledger.Observe([]Sighting{{Signature: "sig-a", JobID: "job-1", Builds: []string{"90", "91"}}}, at(60))

	entry := ledger.Entries["sig-a"]
	if entry.Occurrences != 4 {
		t.Fatalf("occurrences=%d, want 4 across the gap", entry.Occurrences)
	}
	if entry.FirstSeen != at(0).Format(time.RFC3339) {
		t.Fatalf("first seen was lost: %q", entry.FirstSeen)
	}
	if entry.LastSeen != at(60).Format(time.RFC3339) || entry.Watermark != "91" {
		t.Fatalf("entry=%+v", entry)
	}
}

func TestObserveIgnoresBuildsAtOrBelowTheWatermark(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	ledger.Observe([]Sighting{{Signature: "sig-a", Builds: []string{"20"}}}, at(0))
	ledger.Observe([]Sighting{{Signature: "sig-a", Builds: []string{"5", "20", "21"}}}, at(1))
	entry := ledger.Entries["sig-a"]
	if entry.Occurrences != 2 || entry.Watermark != "21" {
		t.Fatalf("entry=%+v, want one additional build past the watermark", entry)
	}
}

func TestObserveCountsUnparseableBuildIDsOnceAndSkipsEmptySignatures(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	sighting := Sighting{Signature: "sig-a", Builds: []string{"not-a-number", "also-opaque"}}
	ledger.Observe([]Sighting{sighting, {Signature: "  ", Builds: []string{"1"}}}, at(0))
	if len(ledger.Entries) != 1 {
		t.Fatalf("entries=%+v, want the blank signature skipped", ledger.Entries)
	}
	if got := ledger.Entries["sig-a"].Occurrences; got != 2 {
		t.Fatalf("occurrences=%d, want what the first pass actually saw", got)
	}
	// Opaque ids cannot drive watermark arithmetic, so recurrence stops advancing
	// rather than counting the same builds again on every pass.
	ledger.Observe([]Sighting{sighting}, at(1))
	if got := ledger.Entries["sig-a"].Occurrences; got != 2 {
		t.Fatalf("occurrences=%d, want opaque ids not recounted", got)
	}
}

// A verdict-created entry already exists, so the first opaque sighting must still
// take the initialization path rather than being skipped as already observed.
func TestObserveInitializesAVerdictOnlyEntryWithOpaqueBuildIDs(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	record(ledger, "sig-a", Verdict{State: models.PatternRemediationMitigationOnly}, at(0))
	ledger.Observe([]Sighting{{Signature: "sig-a", Builds: []string{"opaque-1", "opaque-2"}}}, at(1))
	entry := ledger.Entries["sig-a"]
	if entry.Occurrences != 2 {
		t.Fatalf("occurrences=%d, want the first observation counted", entry.Occurrences)
	}
	if entry.LastSeen != at(1).Format(time.RFC3339) {
		t.Fatalf("last seen=%q, want the observation time", entry.LastSeen)
	}
}

// A conclusion must not answer forever: no artifact-derived identity guarantees
// an identical symptom has an identical cause, and the world moves on.
func TestClaimReuseIsBoundedAndResetByAFreshVerdict(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	record(ledger, "sig-a", Verdict{State: models.PatternRemediationEnvironmentOrInfrastructure, Reason: "churn"}, at(0))
	for i := range MaxVerdictReuses {
		if _, ok := ledger.ClaimReuse("sig-a", at(1)); !ok {
			t.Fatalf("claim %d was refused within the budget", i+1)
		}
	}
	if _, ok := ledger.ClaimReuse("sig-a", at(1)); ok {
		t.Fatal("reuse continued past the budget")
	}
	if _, ok := ledger.ReusableVerdict("sig-a", at(1)); ok {
		t.Fatal("an exhausted verdict still reported as reusable")
	}
	// The stored answer is retained for history even though it no longer answers.
	if ledger.Entries["sig-a"].Verdict == nil {
		t.Fatal("the exhausted verdict was discarded")
	}
	// A fresh investigation restores the budget.
	record(ledger, "sig-a", Verdict{State: models.PatternRemediationExternalDependency, Reason: "upstream"}, at(2))
	if _, ok := ledger.ClaimReuse("sig-a", at(2)); !ok {
		t.Fatal("a freshly recorded verdict did not restore the reuse budget")
	}
}

// ReusableVerdict is the read-only view of the same decision, so it must not
// charge the budget.
func TestReusableVerdictDoesNotChargeTheReuseBudget(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	record(ledger, "sig-a", Verdict{State: models.PatternRemediationMitigationOnly}, at(0))
	for range MaxVerdictReuses + 5 {
		if _, ok := ledger.ReusableVerdict("sig-a", at(0)); !ok {
			t.Fatal("a read-only check consumed the budget")
		}
	}
	if got := ledger.Entries["sig-a"].Reuses; got != 0 {
		t.Fatalf("reuses=%d, want untouched", got)
	}
}

// RecordedAt is displayed as the completion time, so an unusable one is stamped
// rather than rejected; ordering does not depend on it.
func TestRecordVerdictStampsAnUnusableRecordedTime(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	for _, recordedAt := range []string{"", "   ", "not-a-time"} {
		ledger.Entries = map[string]Entry{}
		if !record(ledger, "sig-a", Verdict{State: models.PatternRemediationMitigationOnly, RecordedAt: recordedAt}, at(4)) {
			t.Fatalf("recordedAt=%q was rejected", recordedAt)
		}
		if got := ledger.Entries["sig-a"].Verdict.RecordedAt; got != at(4).Format(time.RFC3339) {
			t.Fatalf("recordedAt=%q stored as %q", recordedAt, got)
		}
	}
}

// Recurrence is direct evidence the cause was not fixed, so that one verdict is
// dropped while verdicts that stay true across recurrences are kept.
func TestObserveInvalidatesOnlyAlreadyFixedOnRecurrence(t *testing.T) {
	for _, tc := range []struct {
		state models.PatternRemediationInvestigationState
		kept  bool
	}{
		{models.PatternRemediationAlreadyFixed, false},
		{models.PatternRemediationEnvironmentOrInfrastructure, true},
		{models.PatternRemediationActionable, true},
	} {
		ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
		ledger.Observe([]Sighting{{Signature: "sig-a", Builds: []string{"10"}}}, at(0))
		record(ledger, "sig-a", Verdict{State: tc.state, Reason: "recorded"}, at(1))
		ledger.Observe([]Sighting{{Signature: "sig-a", Builds: []string{"11"}}}, at(2))
		if got := ledger.Entries["sig-a"].Verdict != nil; got != tc.kept {
			t.Fatalf("state=%s kept=%v, want kept=%v", tc.state, got, tc.kept)
		}
	}
}

func TestReusableVerdictOnlyReturnsAnswersThatSurviveRecurrence(t *testing.T) {
	reusable := []models.PatternRemediationInvestigationState{
		models.PatternRemediationEnvironmentOrInfrastructure,
		models.PatternRemediationExternalDependency,
		models.PatternRemediationMitigationOnly,
	}
	for _, state := range reusable {
		ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
		record(ledger, "sig-a", Verdict{State: state, Reason: "answered"}, at(0))
		verdict, ok := ledger.ReusableVerdict("sig-a", at(0))
		if !ok || verdict.State != state || verdict.Reason != "answered" {
			t.Fatalf("state=%s verdict=%+v ok=%v", state, verdict, ok)
		}
	}
	// An actionable target is pinned to a revision, "already fixed" is
	// contradicted by the recurrence, and "insufficient evidence" describes what
	// was available at the time, which a later recurrence may change. None of
	// them answers a repeat request.
	for _, state := range []models.PatternRemediationInvestigationState{
		models.PatternRemediationActionable,
		models.PatternRemediationAlreadyFixed,
		models.PatternRemediationInsufficientEvidence,
	} {
		ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
		if !record(ledger, "sig-a", Verdict{State: state}, at(0)) {
			t.Fatalf("state=%s was not retained at all", state)
		}
		if _, ok := ledger.ReusableVerdict("sig-a", at(0)); ok {
			t.Fatalf("state=%s was reused", state)
		}
	}
	empty := &Ledger{Version: Version, Entries: map[string]Entry{}}
	if _, ok := empty.ReusableVerdict("missing", at(0)); ok {
		t.Fatal("an unknown signature returned a verdict")
	}
}

// Pruning is a maintenance write that can fail. Reuse must not depend on it
// having succeeded, or a failed fetcher pass keeps expired memory answering.
func TestReusableVerdictRefusesMemoryPastTheRetentionWindowEvenUnpruned(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	ledger.Observe([]Sighting{{Signature: "sig-a", Builds: []string{"10"}}}, at(0))
	record(ledger, "sig-a", Verdict{State: models.PatternRemediationMitigationOnly}, at(0))
	if _, ok := ledger.ReusableVerdict("sig-a", at(0)); !ok {
		t.Fatal("a fresh verdict was not reusable")
	}
	expired := at(0).Add(RetentionWindow + time.Hour)
	if _, ok := ledger.ReusableVerdict("sig-a", expired); ok {
		t.Fatal("an unpruned but expired verdict was reused")
	}
	if _, present := ledger.Entries["sig-a"]; !present {
		t.Fatal("reading must not mutate the ledger")
	}
}

func TestRecordVerdictRejectsNonTerminalStatesAndBlankSignatures(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	for _, state := range []models.PatternRemediationInvestigationState{
		models.PatternRemediationQueued, models.PatternRemediationInvestigating,
		models.PatternRemediationVerifying, models.PatternRemediationInvestigationFailed,
		models.PatternRemediationStale, models.PatternRemediationNotInvestigated,
	} {
		if record(ledger, "sig-a", Verdict{State: state}, at(0)) {
			t.Fatalf("non-terminal state %s was recorded", state)
		}
	}
	if record(ledger, "   ", Verdict{State: models.PatternRemediationMitigationOnly}, at(0)) {
		t.Fatal("a blank signature was recorded")
	}
	if len(ledger.Entries) != 0 {
		t.Fatalf("entries=%+v, want nothing recorded", ledger.Entries)
	}
}

// A verdict can land before the fetcher has published the cause, so recording
// must not silently drop the answer. It must also not seed a failing-build count
// it has no evidence for, or the first real sighting double-counts.
func TestRecordVerdictCreatesAnEntryWhenTheCauseIsUnseen(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	if !record(ledger, "sig-a", Verdict{State: models.PatternRemediationMitigationOnly}, at(3)) {
		t.Fatal("verdict was not recorded")
	}
	entry := ledger.Entries["sig-a"]
	if entry.Occurrences != 0 || entry.FirstSeen != at(3).Format(time.RFC3339) || entry.Verdict == nil {
		t.Fatalf("entry=%+v", entry)
	}
	if entry.Verdict.RecordedAt != at(3).Format(time.RFC3339) {
		t.Fatalf("recorded at=%q", entry.Verdict.RecordedAt)
	}
	ledger.Observe([]Sighting{{Signature: "sig-a", Builds: []string{"10", "11"}}}, at(4))
	if got := ledger.Entries["sig-a"].Occurrences; got != 2 {
		t.Fatalf("occurrences=%d, want only the two observed builds", got)
	}
}

func TestPruneDropsCausesOutsideTheRetentionWindow(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	ledger.Observe([]Sighting{
		{Signature: "old", Builds: []string{"1"}},
		{Signature: "recent", Builds: []string{"2"}},
	}, at(0))
	entry := ledger.Entries["recent"]
	entry.LastSeen = at(200).Format(time.RFC3339)
	ledger.Entries["recent"] = entry

	if !ledger.Prune(at(200)) {
		t.Fatal("expiry reported no change")
	}
	if _, ok := ledger.Entries["old"]; ok {
		t.Fatal("a cause unseen for longer than the retention window survived")
	}
	if _, ok := ledger.Entries["recent"]; !ok {
		t.Fatal("a recently seen cause was dropped")
	}
}

func TestPruneEvictsLeastRecentlySeenAboveTheEntryCap(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	for i := range MaxEntries + 10 {
		signature := "sig-" + strconv.Itoa(i)
		ledger.Entries[signature] = Entry{
			Signature: signature, Occurrences: 1,
			FirstSeen: at(0).Format(time.RFC3339),
			LastSeen:  at(0).Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
		}
	}
	if !ledger.Prune(at(0)) {
		t.Fatal("eviction reported no change above the cap")
	}
	if len(ledger.Entries) != MaxEntries {
		t.Fatalf("entries=%d, want %d", len(ledger.Entries), MaxEntries)
	}
	if _, ok := ledger.Entries["sig-0"]; ok {
		t.Fatal("the least recently seen cause survived eviction")
	}
	if _, ok := ledger.Entries["sig-"+strconv.Itoa(MaxEntries+9)]; !ok {
		t.Fatal("the most recently seen cause was evicted")
	}
}

// Counting recurrence gives every distinct failure shape an entry, so the cap is
// now reachable. Eviction must not trade an investigated conclusion for a
// counting-only entry that happens to have been seen more recently.
func TestPruneEvictsCountingEntriesBeforeVerdicts(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	// The verdict is the oldest entry, so least-recently-seen order alone would
	// evict it first.
	ledger.Entries["answered"] = Entry{
		Signature: "answered", Occurrences: 1,
		FirstSeen: at(0).Format(time.RFC3339),
		LastSeen:  at(0).Format(time.RFC3339),
		Verdict: &Verdict{
			State:      models.PatternRemediationEnvironmentOrInfrastructure,
			RecordedAt: at(0).Format(time.RFC3339),
		},
	}
	for i := range MaxEntries + 10 {
		signature := "counting-" + strconv.Itoa(i)
		ledger.Entries[signature] = Entry{
			Signature: signature, Occurrences: 1,
			FirstSeen: at(0).Format(time.RFC3339),
			LastSeen:  at(0).Add(time.Duration(i+1) * time.Minute).Format(time.RFC3339),
		}
	}

	if !ledger.Prune(at(0)) {
		t.Fatal("eviction reported no change above the cap")
	}
	if len(ledger.Entries) != MaxEntries {
		t.Fatalf("entries=%d, want %d", len(ledger.Entries), MaxEntries)
	}
	if _, ok := ledger.Entries["answered"]; !ok {
		t.Fatal("an investigated verdict was evicted to keep counting-only entries")
	}
}

func TestPruneDropsEntriesWhoseAgeCannotBeEstablished(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{
		"sig-a": {
			Signature: "sig-a", LastSeen: "not-a-time", Occurrences: 1,
			Verdict: &Verdict{State: models.PatternRemediationMitigationOnly},
		},
	}}
	// An entry that cannot be aged out is dropped rather than repaired, so an
	// answer whose age was never establishable never becomes reusable.
	if _, reusable := ledger.ReusableVerdict("sig-a", at(0)); reusable {
		t.Fatal("an unageable verdict was reusable")
	}
	if !ledger.Prune(at(0)) {
		t.Fatal("prune reported no change")
	}
	if _, ok := ledger.Entries["sig-a"]; ok {
		t.Fatal("an entry with an unusable timestamp survived pruning")
	}
}

func TestUpdateRoundTripsThroughDisk(t *testing.T) {
	dir := t.TempDir()
	err := Update(dir, func(ledger *Ledger) bool {
		return ledger.Observe([]Sighting{{Signature: "sig-a", JobID: "job-1", Builds: []string{"7"}}}, at(0))
	})
	if err != nil {
		t.Fatal(err)
	}
	entry := Load(dir).Entries["sig-a"]
	if entry.Occurrences != 1 || entry.JobID != "job-1" {
		t.Fatalf("entry=%+v", entry)
	}
	if err := Update(dir, func(*Ledger) bool { return false }); err != nil {
		t.Fatal(err)
	}
}

func TestLoadStartsFreshOnMissingCorruptOrForeignVersionFiles(t *testing.T) {
	dir := t.TempDir()
	if got := Load(dir); got.Version != Version || len(got.Entries) != 0 {
		t.Fatalf("missing file yielded %+v", got)
	}
	for name, content := range map[string]string{
		"corrupt":         "{not json",
		"foreign version": `{"version":99,"entries":{"sig":{"signature":"sig"}}}`,
	} {
		writeLedgerFile(t, dir, content)
		got := Load(dir)
		if got.Version != Version || got.Entries == nil || len(got.Entries) != 0 {
			t.Fatalf("%s yielded %+v", name, got)
		}
	}
	writeLedgerFile(t, dir, `{"version":1}`)
	if got := Load(dir); got.Entries == nil || len(got.Entries) != 0 {
		t.Fatalf("nil entries yielded %+v", got)
	}
}

func TestStoreReadsAndRecordsThroughTheLedgerFile(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	store.now = func() time.Time { return at(0) }

	if _, ok, err := store.ClaimReuse("sig-a"); err != nil || ok {
		t.Fatalf("an empty ledger returned a reusable verdict: ok=%v err=%v", ok, err)
	}
	if err := store.RecordVerdict("sig-a", Verdict{State: models.PatternRemediationQueued, Reason: "in flight"}); err != nil {
		t.Fatal(err)
	}
	if len(Load(dir).Entries) != 0 {
		t.Fatal("a non-terminal state was persisted")
	}
	if err := store.RecordVerdict("sig-a", Verdict{
		State: models.PatternRemediationEnvironmentOrInfrastructure, Reason: "cluster churn",
		RecordedAt: at(2).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	verdict, ok, err := store.ClaimReuse("sig-a")
	if err != nil || !ok || verdict.State != models.PatternRemediationEnvironmentOrInfrastructure || verdict.Reason != "cluster churn" {
		t.Fatalf("verdict=%+v ok=%v err=%v", verdict, ok, err)
	}
	if verdict.RecordedAt != at(2).Format(time.RFC3339) {
		t.Fatalf("recorded at=%q, want the time the answer was reached", verdict.RecordedAt)
	}
	if err := store.RecordVerdict("sig-b", Verdict{State: models.PatternRemediationActionable, Reason: "target found"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ClaimReuse("sig-b"); err != nil || ok {
		t.Fatalf("an actionable verdict was offered for reuse: ok=%v err=%v", ok, err)
	}
}

// Several server replicas can investigate the same cause, so a slow older
// answer must not displace a newer conclusion.
func TestRecordVerdictRefusesAnAnswerOlderThanTheOneOnRecord(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	newer := Verdict{State: models.PatternRemediationMitigationOnly, Reason: "newer", RecordedAt: at(5).Format(time.RFC3339)}
	older := Verdict{State: models.PatternRemediationExternalDependency, Reason: "older", RecordedAt: at(1).Format(time.RFC3339)}
	if !record(ledger, "sig-a", newer, at(9)) {
		t.Fatal("the newer verdict was not recorded")
	}
	if record(ledger, "sig-a", older, at(9)) {
		t.Fatal("an older answer displaced a newer one")
	}
	if got := ledger.Entries["sig-a"].Verdict.Reason; got != "newer" {
		t.Fatalf("reason=%q", got)
	}
	latest := Verdict{State: models.PatternRemediationEnvironmentOrInfrastructure, Reason: "latest", RecordedAt: at(7).Format(time.RFC3339)}
	if !record(ledger, "sig-a", latest, at(9)) {
		t.Fatal("a newer answer was rejected")
	}
	if got := ledger.Entries["sig-a"].Verdict.Reason; got != "latest" {
		t.Fatalf("reason=%q", got)
	}
}

func writeLedgerFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Recurrence gives every distinct failure shape an entry, so a pass can add more
// than pruning removed. Enforcing the cap only in Prune would let the file settle
// permanently above it.
func TestObserveEnforcesTheEntryCap(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{}}
	sightings := make([]Sighting, 0, MaxEntries+50)
	for i := range MaxEntries + 50 {
		sightings = append(sightings, Sighting{
			Signature: "sig-" + strconv.Itoa(i), JobID: "job-1",
			Builds: []string{strconv.Itoa(1000 + i)},
		})
	}
	if !ledger.Observe(sightings, at(0)) {
		t.Fatal("observing new causes reported no change")
	}
	if len(ledger.Entries) != MaxEntries {
		t.Fatalf("entries=%d, want the cap enforced at %d", len(ledger.Entries), MaxEntries)
	}
}

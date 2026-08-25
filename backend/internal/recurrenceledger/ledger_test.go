package recurrenceledger

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

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

func TestPruneDropsEntriesWhoseAgeCannotBeEstablished(t *testing.T) {
	ledger := &Ledger{Version: Version, Entries: map[string]Entry{
		"sig-a": {Signature: "sig-a", LastSeen: "not-a-time", Occurrences: 1},
	}}
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

// Package recurrenceledger gives recurring failures durable memory. Causal groups
// and remediation state are otherwise recomputed from the current build window
// every pass, so a cause that disappears and returns is treated as brand new and
// an already-answered investigation is re-run at full model cost.
//
// The ledger is keyed by the causal group's durable signature (see
// patterns.CausalGroupSignature), which is derived from observed failure
// artifacts rather than model prose or the current window. It records when a
// cause was first and last seen, how many failing builds it has produced, and
// the most recent terminal remediation verdict.
//
// The file is private operational state: the fetcher records sightings and the
// server records verdicts, so mutations are serialized through Update on a
// dedicated lock file, independent of the pattern publication lock.
package recurrenceledger

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/statefile"
)

// FileName is the private recurrence ledger in the fetcher output directory.
const FileName = "recurrence_ledger.json"

const (
	// Version guards the on-disk shape. An unrecognized version starts fresh.
	Version = 1
	// RetentionWindow drops causes that have not failed for this long.
	RetentionWindow = 180 * 24 * time.Hour
	// MaxEntries bounds the file, evicting least-recently-seen causes first.
	MaxEntries = 2000
	// MaxVerdictReuses bounds how often one conclusion may answer without a fresh
	// investigation. No artifact-derived identity can guarantee that an identical
	// symptom has an identical cause, so a wrong reuse is bounded rather than
	// permanent, and a conclusion is periodically re-checked against a world that
	// may have moved on.
	MaxVerdictReuses = 3
)

// Verdict is the most recent terminal remediation answer for one cause.
type Verdict struct {
	State  models.PatternRemediationInvestigationState `json:"state"`
	Reason string                                      `json:"reason,omitempty"`
	// RecordedAt is when the answer was reached, not when it was written, so an
	// earlier-completed verdict cannot displace a later-completed one.
	RecordedAt string `json:"recorded_at"`
}

// Entry is the recurrence history of one durable cause.
type Entry struct {
	Signature string `json:"signature"`
	JobID     string `json:"job_id,omitempty"`
	Subject   string `json:"subject,omitempty"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	// Occurrences counts distinct failing builds attributed to this cause over
	// its whole lifetime, not the number of passes that observed it.
	Occurrences int `json:"occurrences"`
	// Watermark is the highest build id counted so far. Only builds past it
	// advance recurrence, so re-observing a retained pattern cannot inflate it.
	Watermark string `json:"watermark,omitempty"`
	// Reuses counts how many times the stored verdict has answered a request
	// without a fresh investigation. Recording a new verdict resets it.
	Reuses  int      `json:"reuses,omitempty"`
	Verdict *Verdict `json:"verdict,omitempty"`
}

// Ledger is the durable recurrence history keyed by causal-group signature.
type Ledger struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

// Sighting is one causal group observed in the current pass.
type Sighting struct {
	Signature string
	JobID     string
	Subject   string
	Builds    []string
}

// Load reads the ledger from dir, returning empty (non-nil) state when the file
// is missing, unreadable, or written by a different version so callers never
// nil-check the map.
func Load(dir string) *Ledger {
	fresh := &Ledger{Version: Version, Entries: map[string]Entry{}}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return fresh
	}
	var ledger Ledger
	if err := json.Unmarshal(data, &ledger); err != nil || ledger.Version != Version || ledger.Entries == nil {
		return fresh
	}
	return &ledger
}

// Save writes the ledger to dir atomically with private permissions.
func (l *Ledger) Save(dir string) error {
	l.Version = Version
	if l.Entries == nil {
		l.Entries = map[string]Entry{}
	}
	return statefile.WriteJSONDurable(filepath.Join(dir, FileName), l)
}

// Observe records sightings and reports whether anything changed. Recurrence
// advances only for builds strictly newer than the stored watermark, so a
// retained pattern re-observed every pass does not inflate its own history.
func (l *Ledger) Observe(sightings []Sighting, now time.Time) bool {
	changed := false
	stamp := now.UTC().Format(time.RFC3339)
	for _, sighting := range sightings {
		signature := strings.TrimSpace(sighting.Signature)
		if signature == "" {
			continue
		}
		entry, existed := l.Entries[signature]
		if !existed {
			entry = Entry{Signature: signature, FirstSeen: stamp, LastSeen: stamp}
			changed = true
		}
		if jobID := strings.TrimSpace(sighting.JobID); jobID != "" && entry.JobID != jobID {
			entry.JobID, changed = jobID, true
		}
		if subject := strings.TrimSpace(sighting.Subject); subject != "" && entry.Subject != subject {
			entry.Subject, changed = subject, true
		}
		fresh, watermark := buildsPastWatermark(sighting.Builds, entry.Watermark)
		switch {
		case fresh > 0:
			entry.Occurrences += fresh
			entry.LastSeen = stamp
			entry.Watermark = watermark
			// Recurrence contradicts a prior "already fixed" answer, so that
			// verdict is dropped and the cause becomes investigable again.
			if entry.Verdict != nil && entry.Verdict.State == models.PatternRemediationAlreadyFixed {
				entry.Verdict = nil
			}
			changed = true
		case entry.Occurrences == 0 && entry.Watermark == "" && len(sighting.Builds) > 0:
			// Never observed before, and no build id parsed, so watermark
			// arithmetic is impossible and recurrence cannot advance for this
			// cause. Record what this pass saw and keep the entry so a verdict
			// can still attach to it.
			entry.Occurrences = len(sighting.Builds)
			entry.LastSeen = stamp
			entry.Watermark = watermark
			changed = true
		}
		l.Entries[signature] = entry
	}
	return changed
}

// RecordVerdict stores the terminal answer a run reached, creating the entry when
// a verdict lands before the fetcher has observed the cause. An answer that
// completed before the one on record is refused, so a run that finishes late with
// an older conclusion does not displace a newer one.
func (l *Ledger) RecordVerdict(signature string, verdict Verdict, now time.Time) bool {
	signature = strings.TrimSpace(signature)
	if signature == "" || !TerminalVerdictState(verdict.State) {
		return false
	}
	stamp := now.UTC().Format(time.RFC3339)
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(verdict.RecordedAt)); err != nil {
		verdict.RecordedAt = stamp
	}
	entry, existed := l.Entries[signature]
	if !existed {
		// Occurrences stays at zero: no failing build has been attributed yet, and
		// seeding it here would double-count the first sighting that arrives.
		entry = Entry{Signature: signature, FirstSeen: stamp, LastSeen: stamp}
	}
	if entry.Verdict != nil && olderThan(verdict.RecordedAt, entry.Verdict.RecordedAt) {
		return false
	}
	entry.Signature = signature
	entry.Reuses = 0
	entry.Verdict = &verdict
	l.Entries[signature] = entry
	return true
}

// ClaimReuse returns a recorded verdict that already answers this cause and
// charges the reuse against its bounded budget, so a repeat investigation need
// not re-spend model budget while no conclusion answers indefinitely.
func (l *Ledger) ClaimReuse(signature string, now time.Time) (Verdict, bool) {
	signature = strings.TrimSpace(signature)
	verdict, ok := l.ReusableVerdict(signature, now)
	if !ok {
		return Verdict{}, false
	}
	entry := l.Entries[signature]
	entry.Reuses++
	l.Entries[signature] = entry
	return verdict, true
}

// ReusableVerdict reports the verdict ClaimReuse would return, without charging
// it. An answer older than the retention window is refused even when pruning has
// not yet removed it, so a failed maintenance write cannot keep stale memory
// alive.
func (l *Ledger) ReusableVerdict(signature string, now time.Time) (Verdict, bool) {
	entry, ok := l.Entries[strings.TrimSpace(signature)]
	if !ok || entry.Verdict == nil || !Reusable(entry.Verdict.State) ||
		entry.Reuses >= MaxVerdictReuses || expired(entry, now) {
		return Verdict{}, false
	}
	return *entry.Verdict, true
}

// Prune enforces the retention window and then the entry cap, reporting whether
// anything was dropped. Retention runs before a pass observes, so a cause
// returning after the window starts fresh rather than reviving an old verdict.
func (l *Ledger) Prune(now time.Time) bool {
	changed := false
	for signature, entry := range l.Entries {
		// expired treats an unparseable timestamp as too old, so an entry whose
		// age cannot be established is dropped rather than repaired.
		if expired(entry, now) {
			delete(l.Entries, signature)
			changed = true
		}
	}
	return l.evictOverCapacity() || changed
}

func (l *Ledger) evictOverCapacity() bool {
	if len(l.Entries) <= MaxEntries {
		return false
	}
	signatures := make([]string, 0, len(l.Entries))
	for signature := range l.Entries {
		signatures = append(signatures, signature)
	}
	sort.Slice(signatures, func(i, j int) bool {
		left, right := l.Entries[signatures[i]], l.Entries[signatures[j]]
		if left.LastSeen != right.LastSeen {
			return left.LastSeen < right.LastSeen
		}
		return signatures[i] < signatures[j]
	})
	for _, signature := range signatures[:len(l.Entries)-MaxEntries] {
		delete(l.Entries, signature)
	}
	return true
}

// Reusable reports whether a terminal verdict still answers the same cause when
// it recurs. Only conclusions about the nature of the cause qualify. An
// actionable target is pinned to a source revision and must be re-verified,
// "already fixed" is contradicted by the recurrence itself, and "insufficient
// evidence" is a statement about what was available at the time, which a later
// recurrence with better artifacts may change.
func Reusable(state models.PatternRemediationInvestigationState) bool {
	switch state {
	case models.PatternRemediationEnvironmentOrInfrastructure,
		models.PatternRemediationExternalDependency,
		models.PatternRemediationMitigationOnly:
		return true
	default:
		return false
	}
}

// TerminalVerdictState reports whether a state is a conclusion worth retaining.
// In-flight, failed, and stale states carry no durable answer.
func TerminalVerdictState(state models.PatternRemediationInvestigationState) bool {
	return state == models.PatternRemediationActionable ||
		state == models.PatternRemediationAlreadyFixed ||
		state == models.PatternRemediationInsufficientEvidence ||
		Reusable(state)
}

// expired fails closed: an age that cannot be established is treated as too old
// to answer with, rather than as fresh.
func expired(entry Entry, now time.Time) bool {
	seen, err := time.Parse(time.RFC3339, entry.LastSeen)
	if err != nil {
		return true
	}
	return seen.Before(now.UTC().Add(-RetentionWindow))
}

// olderThan reports whether an incoming answer predates the stored one. Answers
// that cannot be ordered are treated as current, matching RecordVerdict's
// stamping fallback.
func olderThan(incoming, stored string) bool {
	incomingAt, incomingErr := time.Parse(time.RFC3339, strings.TrimSpace(incoming))
	storedAt, storedErr := time.Parse(time.RFC3339, strings.TrimSpace(stored))
	if incomingErr != nil || storedErr != nil {
		return false
	}
	return incomingAt.Before(storedAt)
}

// buildsPastWatermark counts build ids strictly greater than the watermark and
// returns the watermark advanced to the highest id seen. Build ids are large
// increasing integers; ids that do not parse are ignored rather than guessed at.
func buildsPastWatermark(builds []string, watermark string) (int, string) {
	mark, marked := new(big.Int).SetString(strings.TrimSpace(watermark), 10)
	var highest *big.Int
	count := 0
	for _, build := range builds {
		value, ok := new(big.Int).SetString(strings.TrimSpace(build), 10)
		if !ok {
			continue
		}
		if !marked || value.Cmp(mark) > 0 {
			count++
		}
		if highest == nil || value.Cmp(highest) > 0 {
			highest = value
		}
	}
	if highest == nil || (marked && mark.Cmp(highest) >= 0) {
		return count, watermark
	}
	return count, highest.String()
}

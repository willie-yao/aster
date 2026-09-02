// Package recurrenceledger gives recurring failures durable memory. The ledger
// is keyed by artifact-derived signatures and records when each failure was
// observed and how many distinct failing builds it has produced.
package recurrenceledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/statefile"
)

// FileName is the private recurrence ledger in the fetcher output directory.
const FileName = "recurrence_ledger.json"

const (
	// Version guards the on-disk shape.
	Version = 1
	// RetentionWindow drops causes that have not failed for this long.
	RetentionWindow = 180 * 24 * time.Hour
	// MaxEntries bounds the file, evicting least-recently-seen causes first.
	MaxEntries = 2000
)

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

// Load reads the ledger from dir. A missing file returns empty state; unreadable,
// malformed, and foreign-version files fail closed.
func Load(dir string) (*Ledger, error) {
	fresh := &Ledger{Version: Version, Entries: map[string]Entry{}}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return fresh, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read recurrence ledger: %w", err)
	}
	var ledger Ledger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return nil, fmt.Errorf("decode recurrence ledger: %w", err)
	}
	if ledger.Version != Version {
		return nil, fmt.Errorf("recurrence ledger version %d is not supported", ledger.Version)
	}
	if ledger.Entries == nil {
		return nil, fmt.Errorf("recurrence ledger entries are missing")
	}
	return &ledger, nil
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
// retained pattern re-observed every pass does not inflate its own history. The
// entry cap is enforced afterwards, so a pass cannot leave the file oversized.
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
			changed = true
		case entry.Occurrences == 0 && entry.Watermark == "" && len(sighting.Builds) > 0:
			// Never observed before, and no build id parsed, so watermark
			// arithmetic is impossible. Record what this pass saw once.
			entry.Occurrences = len(sighting.Builds)
			entry.LastSeen = stamp
			entry.Watermark = watermark
			changed = true
		}
		l.Entries[signature] = entry
	}
	// Capacity is enforced after observing, not only in Prune. Recurrence gives
	// every distinct failure shape an entry, so a pass can add more than it
	// pruned and the cap would otherwise never bind.
	evicted := l.evictOverCapacity()
	return changed || evicted
}

// Prune enforces the retention window and then the entry cap, reporting whether
// anything was dropped. Retention runs before a pass observes, so a cause
// returning after the window starts fresh rather than reviving old history.
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

// expired fails closed: an age that cannot be established is treated as too old
// to answer with, rather than as fresh.
func expired(entry Entry, now time.Time) bool {
	seen, err := time.Parse(time.RFC3339, entry.LastSeen)
	if err != nil {
		return true
	}
	return seen.Before(now.UTC().Add(-RetentionWindow))
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

// Package resolve tracks admin-marked "resolved" recurring patterns. A pattern
// a maintainer knows is fixed (often by a change in a repo the engine does not
// watch) is hidden from the active view until it recurs. Resolution is keyed by
// the pattern's stable id and carries a build-id watermark so a newer failing
// build re-opens it automatically.
//
// The state lives in resolved.json in the fetcher output directory, next to the
// other *_state.json files, and is served read-only to the frontend so every
// viewer sees the same resolved set.
package resolve

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

// FileName is the resolved-state file served at /data/resolved.json.
const FileName = "resolved.json"

// Entry records one resolved pattern. Watermark is the highest affected build id
// at resolution time; a later failing build past it re-opens the pattern.
type Entry struct {
	ResolvedAt string `json:"resolved_at"`
	ResolvedBy string `json:"resolved_by"`
	Note       string `json:"note,omitempty"`
	Watermark  string `json:"watermark"`
	Subject    string `json:"subject,omitempty"`
}

// State is the set of resolved patterns keyed by pattern id.
type State struct {
	Resolved map[string]Entry `json:"resolved"`
}

// Load reads resolved.json from dir, returning empty (non-nil) state when the
// file is missing or unreadable so callers never nil-check the map.
func Load(dir string) *State {
	s := &State{Resolved: map[string]Entry{}}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return s
	}
	if err := json.Unmarshal(data, s); err != nil || s.Resolved == nil {
		return &State{Resolved: map[string]Entry{}}
	}
	return s
}

// Save writes resolved.json to dir atomically.
func (s *State) Save(dir string) error {
	return statefile.WriteJSON(filepath.Join(dir, FileName), s)
}

// Watermark returns the highest affected build id of p as a decimal string, or
// "" when the pattern has no usable build ids. Build ids are large increasing
// integers; ids that do not parse are ignored.
func Watermark(p models.PatternAnalysis) string {
	var max *big.Int
	for _, b := range p.SharedBuilds {
		n, ok := new(big.Int).SetString(strings.TrimSpace(b), 10)
		if !ok {
			continue
		}
		if max == nil || n.Cmp(max) > 0 {
			max = n
		}
	}
	if max == nil {
		return ""
	}
	return max.String()
}

// recurredPast reports whether p has a failing build strictly newer than the
// watermark, meaning the resolved failure has come back. It fails open: when the
// watermark cannot be parsed, a still-present pattern is treated as recurred so
// a resolution can never permanently hide an active failure.
func recurredPast(p models.PatternAnalysis, watermark string) bool {
	w, ok := new(big.Int).SetString(strings.TrimSpace(watermark), 10)
	if !ok {
		return true // no reliable watermark: re-show the still-present pattern
	}
	for _, b := range p.SharedBuilds {
		n, ok := new(big.Int).SetString(strings.TrimSpace(b), 10)
		if ok && n.Cmp(w) > 0 {
			return true
		}
	}
	return false
}

// Prune drops resolutions whose pattern has recurred past its watermark, so the
// pattern re-appears in the active view. patterns is the current systemic set
// (from this fetch). It returns the pruned state and whether anything changed.
// Resolutions for patterns absent from the current set are kept: an aged-out
// pattern shows nothing anyway, and it may return within the window.
func (s *State) Prune(patterns []models.PatternAnalysis) (*State, bool) {
	byID := make(map[string]models.PatternAnalysis, len(patterns))
	for _, p := range patterns {
		if p.ID != "" {
			byID[p.ID] = p
		}
	}
	out := &State{Resolved: map[string]Entry{}}
	changed := false
	for id, e := range s.Resolved {
		if p, ok := byID[id]; ok && recurredPast(p, e.Watermark) {
			changed = true
			continue // recurred: drop the resolution
		}
		out.Resolved[id] = e
	}
	return out, changed
}

// IsResolved reports whether pattern id is currently resolved.
func (s *State) IsResolved(id string) bool {
	_, ok := s.Resolved[id]
	return ok
}

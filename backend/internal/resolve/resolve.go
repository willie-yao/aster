// Package resolve tracks admin-marked "resolved" failures. A failure a
// maintainer knows is fixed (often by a change in a repo the engine does not
// watch) is hidden from the active view until it recurs. Every resolution
// carries a build-id watermark so a newer failing build re-opens it
// automatically.
//
// Resolution has two scopes. A pattern resolution is keyed by the pattern's
// stable id and acknowledges the whole pattern. A cause resolution is keyed by a
// causal group's signature and acknowledges one cause of a pattern that has
// several, so acknowledging one cause never hides the others.
//
// Causes key on the signature rather than the causal-group id because the id
// hashes the group's build list and so churns whenever a build joins the group
// or ages out of the window. The signature is derived from the failure
// artifacts and is preserved when a group's builds age out, which is what lets a
// resolution outlive the window it was made in.
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

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/statefile"
)

// FileName is the resolved-state file served at /data/resolved.json.
const FileName = "resolved.json"

// CauseExcerptLimit bounds the cause excerpt stored on an Entry. The excerpt
// exists so the overview can name a resolved cause after it has left the
// published set, not to reproduce the analysis.
const CauseExcerptLimit = 200

// Entry records one resolved failure. Watermark is the highest affected build id
// at resolution time; a later failing build past it re-opens the failure. Cause
// is set only for cause-scoped entries, and holds an excerpt of the root cause
// so a resolved cause stays identifiable once its pattern no longer shows it.
type Entry struct {
	ResolvedAt string `json:"resolved_at"`
	ResolvedBy string `json:"resolved_by"`
	Note       string `json:"note,omitempty"`
	Watermark  string `json:"watermark"`
	Subject    string `json:"subject,omitempty"`
	Cause      string `json:"cause,omitempty"`
}

// State holds resolved patterns keyed by pattern id and resolved causes keyed by
// causal-group signature. The two key spaces are separate maps because a
// signature and a pattern id are both short hex strings and would otherwise be
// indistinguishable.
type State struct {
	Resolved map[string]Entry `json:"resolved"`
	Causes   map[string]Entry `json:"causes,omitempty"`
}

// Load reads resolved.json from dir, returning empty (non-nil) state when the
// file is missing or unreadable so callers never nil-check the maps.
func Load(dir string) *State {
	s := empty()
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return s
	}
	if err := json.Unmarshal(data, s); err != nil || s.Resolved == nil {
		return empty()
	}
	if s.Causes == nil {
		s.Causes = map[string]Entry{}
	}
	return s
}

func empty() *State {
	return &State{Resolved: map[string]Entry{}, Causes: map[string]Entry{}}
}

// Save writes resolved.json to dir atomically.
func (s *State) Save(dir string) error {
	return statefile.WriteJSON(filepath.Join(dir, FileName), s)
}

// Watermark returns the highest affected build id of p as a decimal string, or
// "" when the pattern has no usable build ids.
func Watermark(p models.PatternAnalysis) string {
	return maxBuildID(p.SharedBuilds)
}

// CauseWatermark returns the highest affected build id of one causal group, or
// "" when the group has no usable build ids.
func CauseWatermark(group models.PatternCausalGroup) string {
	return maxBuildID(group.Builds)
}

// maxBuildID returns the largest build id in builds as a decimal string. Build
// ids are large increasing integers; ids that do not parse are ignored.
func maxBuildID(builds []string) string {
	var max *big.Int
	for _, b := range builds {
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

// recurredPast reports whether builds contains one strictly newer than the
// watermark, meaning the resolved failure has come back. It fails open: when the
// watermark cannot be parsed, a still-present failure is treated as recurred so
// a resolution can never permanently hide an active failure.
func recurredPast(builds []string, watermark string) bool {
	w, ok := new(big.Int).SetString(strings.TrimSpace(watermark), 10)
	if !ok {
		return true // no reliable watermark: re-show the still-present failure
	}
	for _, b := range builds {
		n, ok := new(big.Int).SetString(strings.TrimSpace(b), 10)
		if ok && n.Cmp(w) > 0 {
			return true
		}
	}
	return false
}

// Prune drops resolutions whose failure has recurred past its watermark, so the
// failure re-appears in the active view. patterns is the current systemic set
// (from this fetch). It returns the pruned state and whether anything changed.
// Resolutions for failures absent from the current set are kept: an aged-out
// failure shows nothing anyway, and it may return within the window.
func (s *State) Prune(patterns []models.PatternAnalysis) (*State, bool) {
	byID := make(map[string]models.PatternAnalysis, len(patterns))
	for _, p := range patterns {
		if p.ID != "" {
			byID[p.ID] = p
		}
	}
	out := empty()
	changed := false
	for id, e := range s.Resolved {
		if p, ok := byID[id]; ok && recurredPast(p.SharedBuilds, e.Watermark) {
			changed = true
			continue // recurred: drop the resolution
		}
		out.Resolved[id] = e
	}
	buildsBySignature := causeBuilds(patterns)
	for signature, e := range s.Causes {
		if builds, ok := buildsBySignature[signature]; ok && recurredPast(builds, e.Watermark) {
			changed = true
			continue
		}
		out.Causes[signature] = e
	}
	return out, changed
}

// causeBuilds indexes the currently published builds of every signed causal
// group by signature. One signature can appear on more than one pattern, so the
// builds are unioned: a recurrence anywhere it is published re-opens the cause.
func causeBuilds(patterns []models.PatternAnalysis) map[string][]string {
	out := map[string][]string{}
	for _, p := range patterns {
		for _, g := range p.CausalGroups {
			signature := strings.TrimSpace(g.Signature)
			if signature == "" {
				continue
			}
			out[signature] = append(out[signature], g.Builds...)
		}
	}
	return out
}

// IsResolved reports whether pattern id is currently resolved.
func (s *State) IsResolved(id string) bool {
	_, ok := s.Resolved[id]
	return ok
}

// IsCauseResolved reports whether the cause with this signature is currently
// resolved.
func (s *State) IsCauseResolved(signature string) bool {
	_, ok := s.Causes[signature]
	return ok
}

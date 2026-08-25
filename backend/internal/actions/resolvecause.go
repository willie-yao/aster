package actions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/patternstate"
	"github.com/willie-yao/aster/backend/internal/resolve"
	"github.com/willie-yao/aster/backend/internal/textutil"
)

// ResolveCause marks one cause of a recurring pattern as resolved: that cause
// alone is hidden from the active view until a failing build newer than its
// current watermark recurs. Its sibling causes are untouched. note is an
// optional maintainer comment (e.g. the fixing PR). login attributes it.
//
// The cause is addressed by its signature, the artifact-derived identity that
// survives the build window shifting under it. A causal group without one cannot
// be resolved individually; the pattern-level acknowledgement covers it instead.
func (s *Service) ResolveCause(signature, login, note string) error {
	return patternstate.WithLock(s.dataDir, func() error { return s.resolveCauseUnlocked(signature, login, note) })
}

func (s *Service) resolveCauseUnlocked(signature, login, note string) error {
	pattern, group, err := s.findCause(signature)
	if err != nil {
		return err
	}
	if !pattern.Systemic {
		return fmt.Errorf("only causes of systemic recurring patterns can be resolved")
	}
	if !models.PatternIsActive(*pattern) {
		return fmt.Errorf("causes of inactive recurring patterns cannot be manually resolved")
	}
	watermark := resolve.CauseWatermark(*group)
	if watermark == "" {
		return fmt.Errorf("cause has no build history to resolve against")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return resolve.Update(s.dataDir, func(st *resolve.State) bool {
		st.Causes[signature] = resolve.Entry{
			ResolvedAt: time.Now().UTC().Format(time.RFC3339), ResolvedBy: login,
			Note: strings.TrimSpace(note), Watermark: watermark, Subject: pattern.Subject,
			Cause: textutil.Truncate(strings.TrimSpace(group.RootCause), resolve.CauseExcerptLimit),
		}
		return true
	})
}

// UnresolveCause clears a cause's resolved mark so it returns to the active
// view. Like Unresolve it requires only an existing marker: a cause resolution
// outlives the window its group was published in (see resolve.State.Prune), and
// clearing an acknowledgement can only un-hide a failure.
func (s *Service) UnresolveCause(signature string) error {
	return patternstate.WithLock(s.dataDir, func() error { return s.unresolveCauseUnlocked(signature) })
}

func (s *Service) unresolveCauseUnlocked(signature string) error {
	if !resolve.Load(s.dataDir).IsCauseResolved(signature) {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return resolve.Update(s.dataDir, func(st *resolve.State) bool {
		if !st.IsCauseResolved(signature) {
			return false
		}
		delete(st.Causes, signature)
		return true
	})
}

// findCause locates the published causal group carrying this signature and the
// pattern that owns it. A signature published on more than one group identifies
// more than one cause, so it is refused rather than resolved against an
// arbitrary one.
func (s *Service) findCause(signature string) (*models.PatternAnalysis, *models.PatternCausalGroup, error) {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return nil, nil, ErrNotFound
	}
	jobsDir := filepath.Join(s.dataDir, "jobs")
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading job details: %w", err)
	}
	var foundPattern *models.PatternAnalysis
	var foundGroup *models.PatternCausalGroup
	var foundRefresh *models.PatternRefreshStatus
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jobsDir, e.Name()))
		if err != nil {
			continue
		}
		var detail models.JobDetail
		if json.Unmarshal(data, &detail) != nil {
			continue
		}
		for i := range detail.PatternAnalyses {
			for j := range detail.PatternAnalyses[i].CausalGroups {
				group := detail.PatternAnalyses[i].CausalGroups[j]
				if strings.TrimSpace(group.Signature) != signature {
					continue
				}
				if foundGroup != nil {
					return nil, nil, fmt.Errorf("cause signature identifies more than one published cause")
				}
				pattern := detail.PatternAnalyses[i]
				foundPattern, foundGroup = &pattern, &group
				if detail.PatternRefresh != nil {
					refresh := *detail.PatternRefresh
					foundRefresh = &refresh
				}
			}
		}
	}
	if foundGroup == nil {
		return nil, nil, ErrNotFound
	}
	if code := patternRefreshReasonCode(foundRefresh); code != "" {
		return nil, nil, withReason(code, ErrRemediationInconclusive, "")
	}
	return foundPattern, foundGroup, nil
}

package analysischat

import (
	"fmt"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/buildsource"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// PreflightTestFix verifies exact JUnit Fix source eligibility without a provider request.
func (s *Service) PreflightTestFix(sessionID, owner string) error {
	owner = normalizeOwner(owner)
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	var ref AnalysisRef
	var analysisHash string
	var sourceRepository buildsource.Source
	err := s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		if current.View.Analysis.Scope != ScopeTest || current.View.Analysis.Source == models.TestCaseSourceBuild || current.View.Analysis.JUnitFile == "" {
			return changed, fmt.Errorf("%w: exact JUnit Fix requires a failed test analysis", ErrInvalidRequest)
		}
		repository, ok := persistedBuildSourceRepository(current.Resolved, s.sourceRepo)
		if !ok {
			return changed, fmt.Errorf("%w: exact JUnit Fix source identity is unavailable", ErrInvalidRequest)
		}
		ref = current.View.Analysis
		analysisHash = current.Resolved.AnalysisHash
		sourceRepository = buildsource.Source{Owner: repository.Owner, Name: repository.Name, Revision: repository.Revision}
		return changed, nil
	})
	if err != nil {
		return err
	}
	resolved, err := s.resolve(ref)
	if err != nil {
		return err
	}
	currentSource, ok := buildsource.Resolve(resolved.build, sourceRepository.Owner, sourceRepository.Name)
	if !ok || currentSource != sourceRepository || analysisHash == "" || models.TestAnalysisContentHash(resolved.testCase) != analysisHash {
		return ErrAnalysisChanged
	}
	if resolved.testCase.AIAnalysis == nil || len(buildsource.VerifiedPaths(resolved.testCase.AIAnalysis.FileLinks, sourceRepository)) == 0 {
		return fmt.Errorf("%w: exact JUnit Fix has no verified immutable source paths", ErrInvalidRequest)
	}
	return nil
}

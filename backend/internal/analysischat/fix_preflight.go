package analysischat

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/buildsource"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

// ConfigureTestFixPreflight binds the provider-free target source check.
func (s *Service) ConfigureTestFixPreflight(
	check func(context.Context, sourceinvestigation.Repository, string, []string) (string, map[string]string, error),
) error {
	if check == nil {
		return fmt.Errorf("analysis chat Fix source preflight is required")
	}
	s.testFixPreflight = check
	return nil
}

// PreflightTestFix verifies exact JUnit Fix source eligibility without a provider request.
func (s *Service) PreflightTestFix(ctx context.Context, sessionID, owner, requestID string) error {
	owner = normalizeOwner(owner)
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return err
	}
	now := s.opts.Now().UTC()
	storeCtx, cancel := s.store.context()
	defer cancel()
	var ref AnalysisRef
	var analysisHash string
	var sourceRepository buildsource.Source
	var sourceBranch string
	var sourceBranchKnown bool
	var existing *persistedTestFixSource
	var requestAdmitted bool
	err = s.store.update(storeCtx, func(state *persistedState) (bool, error) {
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
		if binding, ok := current.FixSources[requestID]; ok {
			copy := binding
			copy.VerifiedSourceFileHashes = cloneTestFixHashes(binding.VerifiedSourceFileHashes)
			existing = &copy
		}
		_, requestAdmitted = current.Requests[requestID]
		ref = current.View.Analysis
		analysisHash = current.Resolved.AnalysisHash
		sourceRepository = buildsource.Source{Owner: repository.Owner, Name: repository.Name, Revision: repository.Revision}
		sourceBranch, sourceBranchKnown = buildsource.Branch(current.Resolved.Build, repository.Owner, repository.Name)
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
	currentBranch, currentBranchKnown := buildsource.Branch(resolved.build, sourceRepository.Owner, sourceRepository.Name)
	if currentBranchKnown != sourceBranchKnown || sourceBranchKnown && currentBranch != sourceBranch {
		return ErrAnalysisChanged
	}
	if resolved.testCase.AIAnalysis == nil {
		return fmt.Errorf("%w: exact JUnit Fix has no verified immutable source paths", ErrInvalidRequest)
	}
	files := buildsource.VerifiedPaths(resolved.testCase.AIAnalysis.FileLinks, sourceRepository)
	if len(files) == 0 {
		return fmt.Errorf("%w: exact JUnit Fix has no verified immutable source paths", ErrInvalidRequest)
	}
	if existing != nil && requestAdmitted {
		if validTestFixSource(*existing, sourceRepository.Revision, files) {
			return nil
		}
		return fmt.Errorf("%w: exact JUnit Fix source binding is invalid", ErrInvalidRequest)
	}
	if s.testFixPreflight == nil {
		return fmt.Errorf("%w: exact JUnit Fix source compatibility is unavailable", ErrInvalidRequest)
	}
	repository := sourceinvestigation.Repository{Owner: sourceRepository.Owner, Name: sourceRepository.Name, Revision: sourceRepository.Revision}
	generationBase, hashes, err := s.testFixPreflight(ctx, repository, sourceBranch, files)
	if err != nil {
		return fmt.Errorf("%w: exact JUnit Fix source compatibility failed", ErrInvalidRequest)
	}
	binding := persistedTestFixSource{
		FailureRevision: sourceRepository.Revision, GenerationBaseRevision: generationBase,
		VerifiedSourceFileHashes: cloneTestFixHashes(hashes),
	}
	if !validTestFixSource(binding, sourceRepository.Revision, files) {
		return fmt.Errorf("%w: exact JUnit Fix source compatibility is invalid", ErrInvalidRequest)
	}
	persistCtx, persistCancel := s.store.context()
	defer persistCancel()
	return s.store.update(persistCtx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		repository, ok := persistedBuildSourceRepository(current.Resolved, s.sourceRepo)
		if !ok || !strings.EqualFold(repository.Revision, binding.FailureRevision) || current.Resolved.AnalysisHash != analysisHash {
			return changed, ErrAnalysisChanged
		}
		if current.FixSources == nil {
			current.FixSources = map[string]persistedTestFixSource{}
		}
		if previous, ok := current.FixSources[requestID]; ok {
			if !validTestFixSource(previous, repository.Revision, files) {
				return changed, fmt.Errorf("%w: exact JUnit Fix source binding is invalid", ErrInvalidRequest)
			}
			if !sameTestFixSource(previous, binding) {
				return changed, ErrAnalysisChanged
			}
			return changed, nil
		}
		if len(current.FixSources) >= s.opts.MaxTurns {
			return changed, ErrTurnLimit
		}
		current.FixSources[requestID] = binding
		return true, nil
	})
}

func sameTestFixSource(left, right persistedTestFixSource) bool {
	if !strings.EqualFold(left.FailureRevision, right.FailureRevision) ||
		!strings.EqualFold(left.GenerationBaseRevision, right.GenerationBaseRevision) ||
		len(left.VerifiedSourceFileHashes) != len(right.VerifiedSourceFileHashes) {
		return false
	}
	for file, hash := range left.VerifiedSourceFileHashes {
		if right.VerifiedSourceFileHashes[file] != hash {
			return false
		}
	}
	return true
}

func validTestFixSource(binding persistedTestFixSource, failureRevision string, files []string) bool {
	failure, ok := buildsource.NormalizeRevision(binding.FailureRevision)
	if !ok || !strings.EqualFold(failure, failureRevision) {
		return false
	}
	if _, ok := buildsource.NormalizeRevision(binding.GenerationBaseRevision); !ok || len(binding.VerifiedSourceFileHashes) != len(files) {
		return false
	}
	files = slices.Clone(files)
	slices.Sort(files)
	files = slices.Compact(files)
	for _, file := range files {
		hash := binding.VerifiedSourceFileHashes[file]
		decoded, err := hex.DecodeString(hash)
		if err != nil || len(decoded) != 32 {
			return false
		}
	}
	return true
}

func cloneTestFixHashes(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

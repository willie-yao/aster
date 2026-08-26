package analysischat

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/buildsource"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

const analysisFixReferenceTTL = 25 * time.Hour

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

// PreflightAnalysisFix verifies the selected failed-test Fix target without a provider request.
func (s *Service) PreflightAnalysisFix(ctx context.Context, sessionID, owner, requestID string) error {
	owner = normalizeOwner(owner)
	if owner == "" {
		return fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return err
	}
	now := s.opts.Now().UTC()
	storeCtx, cancel := s.store.context()
	defer cancel()
	var ref AnalysisRef
	var targetRef AnalysisRef
	var analysisHash string
	var sourceRepository buildsource.Source
	var sourceBranch string
	var sourceBranchKnown bool
	var existing *persistedTestFixSource
	var requestAdmitted bool
	err = s.store.update(storeCtx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Retired {
			return changed, ErrSessionNotFound
		}
		target := persistedAnalysisFixTarget(current.Resolved)
		if target == nil || target.Ref.Source == models.TestCaseSourceBuild || target.Ref.JUnitFile == "" {
			return changed, fmt.Errorf("%w: Fix requires an eligible failed JUnit test", ErrInvalidRequest)
		}
		repository, ok := persistedFixTargetSourceRepository(target, s.sourceRepo)
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
		targetRef = target.Ref
		analysisHash = target.AnalysisHash
		sourceRepository = buildsource.Source{Owner: repository.Owner, Name: repository.Name, Revision: repository.Revision}
		sourceBranch, sourceBranchKnown = buildsource.Branch(target.Build, repository.Owner, repository.Name)
		return changed, nil
	})
	if err != nil {
		return err
	}
	resolved, err := s.resolve(ref)
	if err != nil {
		return err
	}
	target := resolvedAnalysisFixTarget(resolved)
	if target == nil || target.ref != targetRef {
		return ErrAnalysisChanged
	}
	currentSource, ok := buildsource.Resolve(target.build, sourceRepository.Owner, sourceRepository.Name)
	if !ok || currentSource != sourceRepository || analysisHash == "" || models.TestAnalysisContentHash(target.testCase) != analysisHash {
		return ErrAnalysisChanged
	}
	currentBranch, currentBranchKnown := buildsource.Branch(target.build, sourceRepository.Owner, sourceRepository.Name)
	if currentBranchKnown != sourceBranchKnown || sourceBranchKnown && currentBranch != sourceBranch {
		return ErrAnalysisChanged
	}
	if target.testCase.AIAnalysis == nil {
		return fmt.Errorf("%w: exact JUnit Fix has no verified immutable source paths", ErrInvalidRequest)
	}
	files := buildsource.VerifiedPaths(target.testCase.AIAnalysis.FileLinks, sourceRepository)
	if len(files) == 0 {
		return fmt.Errorf("%w: exact JUnit Fix has no verified immutable source paths", ErrInvalidRequest)
	}
	if existing != nil && requestAdmitted {
		if validTestFixSource(*existing, targetRef, sourceRepository.Revision, files) {
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
		return fmt.Errorf("%w: exact JUnit Fix source compatibility failed: %w", ErrInvalidRequest, err)
	}
	binding := persistedTestFixSource{
		TargetRef: targetRef, FailureRevision: sourceRepository.Revision, GenerationBaseRevision: generationBase,
		VerifiedSourceFileHashes: cloneTestFixHashes(hashes),
	}
	if !validTestFixSource(binding, targetRef, sourceRepository.Revision, files) {
		return fmt.Errorf("%w: exact JUnit Fix source compatibility is invalid", ErrInvalidRequest)
	}
	persistCtx, persistCancel := s.store.context()
	defer persistCancel()
	return s.store.update(persistCtx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Retired {
			return changed, ErrSessionNotFound
		}
		currentTarget := persistedAnalysisFixTarget(current.Resolved)
		if currentTarget == nil || currentTarget.Ref != targetRef || currentTarget.AnalysisHash != analysisHash {
			return changed, ErrAnalysisChanged
		}
		repository, ok := persistedFixTargetSourceRepository(currentTarget, s.sourceRepo)
		if !ok || !strings.EqualFold(repository.Revision, binding.FailureRevision) {
			return changed, ErrAnalysisChanged
		}
		if current.FixSources == nil {
			current.FixSources = map[string]persistedTestFixSource{}
		}
		if previous, ok := current.FixSources[requestID]; ok {
			if !validTestFixSource(previous, targetRef, repository.Revision, files) {
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

// ReserveAnalysisFix protects a source binding while action admission runs.
func (s *Service) ReserveAnalysisFix(sessionID, owner, requestID, reservationID string) error {
	owner = normalizeOwner(owner)
	if owner == "" {
		return fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return err
	}
	reservationID, err = normalizeRequestID(reservationID)
	if err != nil {
		return err
	}
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	return s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Retired {
			return changed, ErrSessionNotFound
		}
		binding, ok := current.FixSources[requestID]
		if !ok {
			return changed, ErrRequestNotFound
		}
		if binding.PendingReservations[reservationID] {
			return changed, nil
		}
		if !hasFixDependency(current.FixSources) {
			current.FixBaseExpiresAt = current.ExpiresAt
		}
		if binding.PendingReservations == nil {
			binding.PendingReservations = map[string]bool{}
		}
		binding.PendingReservations[reservationID] = true
		current.FixSources[requestID] = binding
		retainUntil := now.Add(analysisFixReferenceTTL)
		if current.ExpiresAt.Before(retainUntil) {
			extendSessionExpiry(current, retainUntil)
		}
		return true, nil
	})
}

// CommitAnalysisFix binds one admitted action request to the chat evidence.
func (s *Service) CommitAnalysisFix(sessionID, owner, requestID, reservationID, referenceID string) error {
	owner = normalizeOwner(owner)
	if owner == "" {
		return fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
	var err error
	requestID, err = normalizeRequestID(requestID)
	if err != nil {
		return err
	}
	reservationID, err = normalizeRequestID(reservationID)
	if err != nil {
		return err
	}
	referenceID, err = normalizeRequestID(referenceID)
	if err != nil {
		return err
	}
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	return s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Retired {
			return changed, ErrSessionNotFound
		}
		binding, ok := current.FixSources[requestID]
		if !ok || !binding.PendingReservations[reservationID] {
			return changed, ErrRequestNotFound
		}
		delete(binding.PendingReservations, reservationID)
		if len(binding.PendingReservations) == 0 {
			binding.PendingReservations = nil
		}
		if binding.References == nil {
			binding.References = map[string]bool{}
		}
		binding.References[referenceID] = true
		current.FixSources[requestID] = binding
		retainUntil := now.Add(analysisFixReferenceTTL)
		if current.ExpiresAt.Before(retainUntil) {
			extendSessionExpiry(current, retainUntil)
		}
		return true, nil
	})
}

// ReleaseAnalysisFix rolls back one reservation when action admission fails.
func (s *Service) ReleaseAnalysisFix(sessionID, owner, requestID, reservationID string) error {
	owner = normalizeOwner(owner)
	if owner == "" {
		return fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return err
	}
	reservationID, err = normalizeRequestID(reservationID)
	if err != nil {
		return err
	}
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	return s.store.update(ctx, func(state *persistedState) (bool, error) {
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Retired {
			return false, ErrSessionNotFound
		}
		binding, ok := current.FixSources[requestID]
		if !ok || !binding.PendingReservations[reservationID] {
			return false, nil
		}
		delete(binding.PendingReservations, reservationID)
		if len(binding.PendingReservations) == 0 {
			binding.PendingReservations = nil
		}
		current.FixSources[requestID] = binding
		if !hasFixDependency(current.FixSources) && !current.FixBaseExpiresAt.IsZero() {
			restoreUntil := current.FixBaseExpiresAt
			if normalUntil := now.Add(s.opts.SessionTTL); normalUntil.After(restoreUntil) {
				restoreUntil = normalUntil
			}
			current.ExpiresAt = restoreUntil
			current.View.ExpiresAt = restoreUntil.Format(time.RFC3339)
			current.FixBaseExpiresAt = time.Time{}
		}
		return true, nil
	})
}

func hasFixDependency(sources map[string]persistedTestFixSource) bool {
	for _, source := range sources {
		if hasFixBindingDependency(source) {
			return true
		}
	}
	return false
}

func hasFixBindingDependency(source persistedTestFixSource) bool {
	return len(source.PendingReservations) > 0 || len(source.References) > 0
}

func persistedFixTargetSourceRepository(
	target *persistedResolvedFixTarget,
	configured sourceinvestigation.Repository,
) (sourceinvestigation.Repository, bool) {
	if target == nil {
		return sourceinvestigation.Repository{}, false
	}
	source, ok := resolveBuildSourceRepository(target.Build, configured)
	if !ok || sourceinvestigation.ValidateRepository(target.Source) != nil ||
		!strings.EqualFold(target.Source.Owner, source.Owner) ||
		!strings.EqualFold(target.Source.Name, source.Name) ||
		!strings.EqualFold(target.Source.Revision, source.Revision) {
		return sourceinvestigation.Repository{}, false
	}
	return source, true
}

func sameTestFixSource(left, right persistedTestFixSource) bool {
	if left.TargetRef != right.TargetRef || !strings.EqualFold(left.FailureRevision, right.FailureRevision) ||
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

func validTestFixSource(binding persistedTestFixSource, targetRef AnalysisRef, failureRevision string, files []string) bool {
	if binding.TargetRef != targetRef {
		return false
	}
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

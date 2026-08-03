package analysischat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

// SourceInvestigationOptions bounds read-only source requests attached to chat sessions.
type SourceInvestigationOptions struct {
	Timeout           time.Duration
	LeaseTTL          time.Duration
	MaxPerSession     int
	MaxActivePerOwner int
}

func (o SourceInvestigationOptions) normalized() SourceInvestigationOptions {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Minute
	}
	if o.LeaseTTL <= o.Timeout {
		o.LeaseTTL = o.Timeout + 30*time.Second
	}
	if o.MaxPerSession <= 0 {
		o.MaxPerSession = 8
	}
	if o.MaxActivePerOwner <= 0 {
		o.MaxActivePerOwner = 1
	}
	return o
}

// ConfigureSourceInvestigation enables the optional read-only source runtime.
func (s *Service) ConfigureSourceInvestigation(
	runner sourceinvestigation.Runner,
	repo sourceinvestigation.Repository,
	opts SourceInvestigationOptions,
) error {
	if runner == nil {
		return fmt.Errorf("source investigation runner is required")
	}
	repo.Owner = strings.TrimSpace(repo.Owner)
	repo.Name = strings.TrimSpace(repo.Name)
	if repo.Owner == "" || repo.Name == "" {
		return fmt.Errorf("source investigation repository owner and name are required")
	}
	s.investigator = runner
	s.sourceRepo = repo
	s.sourceOpts = opts.normalized()
	return nil
}

// SourceInvestigation starts or follows one idempotent source request.
func (s *Service) SourceInvestigation(
	ctx context.Context,
	sessionID, owner, requestID, chatRequestID string,
) (sourceinvestigation.View, error) {
	result, err := s.startSourceInvestigation(ctx, sessionID, owner, requestID, chatRequestID)
	if err != nil || result.Status != sourceinvestigation.StatusPending {
		return result, err
	}
	return s.waitForSourceInvestigation(ctx, sessionID, owner, requestID, nil)
}

// StreamSourceInvestigation emits persisted progress while starting or following a request.
func (s *Service) StreamSourceInvestigation(
	ctx context.Context,
	sessionID, owner, requestID, chatRequestID string,
	emit func(sourceinvestigation.Progress) error,
) (sourceinvestigation.View, error) {
	result, err := s.startSourceInvestigation(ctx, sessionID, owner, requestID, chatRequestID)
	if err != nil || result.Status != sourceinvestigation.StatusPending {
		return result, err
	}
	return s.waitForSourceInvestigation(ctx, sessionID, owner, requestID, emit)
}

// GetSourceInvestigation returns one owner-bound source request.
func (s *Service) GetSourceInvestigation(sessionID, owner, requestID string) (sourceinvestigation.View, error) {
	owner = normalizeOwner(owner)
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return sourceinvestigation.View{}, err
	}
	ctx, cancel := s.store.context()
	defer cancel()
	var view sourceinvestigation.View
	err = s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, s.opts.Now().UTC())
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		if current.View.Analysis.Scope == ScopePattern {
			return changed, fmt.Errorf("%w: recurring-pattern source investigation is not supported", ErrInvalidRequest)
		}
		record, ok := current.Investigations[requestID]
		if !ok {
			return changed, ErrRequestNotFound
		}
		view = cloneSourceInvestigationView(record.View)
		return changed, nil
	})
	return view, err
}

// CancelSourceInvestigation requests cancellation across server replicas.
func (s *Service) CancelSourceInvestigation(sessionID, owner, requestID string) error {
	owner = normalizeOwner(owner)
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return err
	}
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	active := false
	err = s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		record, ok := current.Investigations[requestID]
		if !ok {
			return changed, ErrRequestNotFound
		}
		if record.View.Status != sourceinvestigation.StatusPending {
			return changed, nil
		}
		record.CancelRequest = true
		record.View.Phase = sourceinvestigation.PhaseCancelling
		record.View.UpdatedAt = now.Format(time.RFC3339)
		current.Investigations[requestID] = record
		active = true
		return true, nil
	})
	if err != nil {
		return err
	}
	if active {
		key := sourceInvestigationKey(sessionID, requestID)
		s.notifyLocal(key)
		s.cancelLocalKey(key)
	}
	return nil
}

func (s *Service) startSourceInvestigation(
	ctx context.Context,
	sessionID, owner, requestID, chatRequestID string,
) (sourceinvestigation.View, error) {
	if s.investigator == nil {
		return sourceinvestigation.View{}, sourceinvestigation.ErrUnavailable
	}
	owner = normalizeOwner(owner)
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return sourceinvestigation.View{}, err
	}
	chatRequestID, err = normalizeRequestID(chatRequestID)
	if err != nil {
		return sourceinvestigation.View{}, err
	}
	subject, err := s.sourceInvestigationSubject(sessionID, owner, chatRequestID)
	if err != nil {
		return sourceinvestigation.View{}, err
	}
	inputHash, err := hashSourceInvestigationInput(subject)
	if err != nil {
		return sourceinvestigation.View{}, err
	}
	now := s.opts.Now().UTC()
	leaseID, err := newSessionID()
	if err != nil {
		return sourceinvestigation.View{}, fmt.Errorf("creating source investigation lease: %w", err)
	}
	var view sourceinvestigation.View
	started := false
	storeCtx, cancel := context.WithTimeout(ctx, s.opts.StoreLockTimeout)
	err = s.store.update(storeCtx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		if current.Investigations == nil {
			current.Investigations = map[string]persistedInvestigation{}
			changed = true
		}
		if previous, ok := current.Investigations[requestID]; ok {
			if previous.InputHash != inputHash {
				return changed, ErrIdempotencyConflict
			}
			view = cloneSourceInvestigationView(previous.View)
			return changed, sourceInvestigationRecordError(previous)
		}
		if len(current.Investigations) >= s.sourceOpts.MaxPerSession {
			return changed, ErrSourceInvestigationLimit
		}
		if s.activeSourceInvestigationsForOwner(state, owner) >= s.sourceOpts.MaxActivePerOwner {
			return changed, ErrSourceInvestigationActiveLimit
		}
		if len(state.OwnerRequests[owner]) >= s.opts.MaxRequestsPerOwnerPerMinute {
			return changed, ErrRateLimit
		}
		stamp := now.Format(time.RFC3339)
		view = sourceinvestigation.View{
			ID: requestID, SessionID: current.View.ID, ChatRequestID: chatRequestID,
			Status: sourceinvestigation.StatusPending, Phase: sourceinvestigation.PhaseQueued,
			CreatedAt: stamp, UpdatedAt: stamp, ExpiresAt: current.View.ExpiresAt,
		}
		current.Investigations[requestID] = persistedInvestigation{
			View: view, InputHash: inputHash, Subject: subject, Revision: strings.ToLower(subject.Repository.Revision),
			LeaseID: leaseID, LeaseExpires: now.Add(s.sourceOpts.LeaseTTL),
		}
		state.OwnerRequests[owner] = append(state.OwnerRequests[owner], now)
		started = true
		return true, nil
	})
	cancel()
	if err != nil {
		return sourceinvestigation.View{}, err
	}
	if started {
		s.activeWG.Add(1)
		go func() {
			defer s.activeWG.Done()
			s.runSourceInvestigation(sessionID, owner, requestID, leaseID, subject)
		}()
	}
	return view, nil
}

func (s *Service) runSourceInvestigation(
	sessionID, owner, requestID, leaseID string,
	subject sourceinvestigation.Subject,
) {
	runCtx, cancel := context.WithTimeout(s.lifecycle, s.sourceOpts.Timeout)
	key := sourceInvestigationKey(sessionID, requestID)
	s.activeMu.Lock()
	s.active[key] = cancel
	s.activeMu.Unlock()
	defer func() {
		cancel()
		s.activeMu.Lock()
		delete(s.active, key)
		s.activeMu.Unlock()
	}()

	request := sourceinvestigation.Request{
		ID: requestID, Subject: subject, Timeout: s.sourceOpts.Timeout,
		Progress: func(phase string) {
			_ = s.updateSourceInvestigationProgress(sessionID, owner, requestID, leaseID, phase)
		},
	}
	request.ReportProgress(sourceinvestigation.PhaseCloning)
	go s.watchSourceInvestigationCancellation(runCtx, sessionID, owner, requestID, leaseID, cancel)
	started := time.Now()
	result, runErr := s.investigator.Investigate(runCtx, request)
	result.ElapsedMs = int(time.Since(started).Milliseconds())
	if err := s.finishSourceInvestigation(sessionID, owner, requestID, leaseID, result, runErr); err != nil &&
		!errors.Is(err, ErrRequestOutcomeUnknown) && !errors.Is(err, ErrSessionNotFound) {
		log.Printf("source investigation %s finalize: %v", requestID, err)
	}
}

func (s *Service) finishSourceInvestigation(
	sessionID, owner, requestID, leaseID string,
	result sourceinvestigation.Result,
	runErr error,
) error {
	finishedAt := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	err := s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, finishedAt)
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		record, ok := current.Investigations[requestID]
		if !ok || record.LeaseID != leaseID || record.View.Status != sourceinvestigation.StatusPending {
			return changed, ErrRequestOutcomeUnknown
		}
		if record.CancelRequest {
			runErr = context.Canceled
		}
		record.LeaseID = ""
		record.LeaseExpires = time.Time{}
		record.CancelRequest = false
		record.Subject = sourceinvestigation.Subject{}
		record.View.Phase = ""
		record.View.UpdatedAt = finishedAt.Format(time.RFC3339)
		extendSessionExpiry(current, finishedAt.Add(s.opts.SessionTTL))
		record.View.ExpiresAt = current.View.ExpiresAt
		if runErr != nil {
			record.View.Status = sourceinvestigation.StatusFailed
			record.FailureKind = requestFailureKind(runErr)
			current.Investigations[requestID] = record
			return true, nil
		}
		if err := sourceinvestigation.ValidateVerifiedResult(result); err != nil {
			record.View.Status = sourceinvestigation.StatusFailed
			record.FailureKind = failureSource
			current.Investigations[requestID] = record
			return true, nil
		}
		record.View.Status = sourceinvestigation.StatusSucceeded
		record.View.Result = sourceinvestigation.CloneResult(&result)
		record.FailureKind = ""
		current.Investigations[requestID] = record
		return true, nil
	})
	s.notifyLocal(sourceInvestigationKey(sessionID, requestID))
	return err
}

func (s *Service) waitForSourceInvestigation(
	ctx context.Context,
	sessionID, owner, requestID string,
	emit func(sourceinvestigation.Progress) error,
) (sourceinvestigation.View, error) {
	owner = normalizeOwner(owner)
	updates, unsubscribe := s.subscribe(sourceInvestigationKey(sessionID, requestID))
	defer unsubscribe()
	lastPhase := ""
	lastEmit := time.Time{}
	for {
		view, err := s.GetSourceInvestigation(sessionID, owner, requestID)
		if err != nil {
			return view, err
		}
		if view.Status != sourceinvestigation.StatusPending {
			return view, s.sourceInvestigationTerminalError(sessionID, owner, requestID)
		}
		if sourceProgressDue(view.Phase, lastPhase, lastEmit) {
			lastPhase = view.Phase
			lastEmit = time.Now()
			if emit != nil {
				if err := emit(sourceinvestigation.Progress{RequestID: requestID, Phase: view.Phase, UpdatedAt: view.UpdatedAt}); err != nil {
					return sourceinvestigation.View{}, err
				}
			}
		}
		timer := time.NewTimer(s.opts.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return sourceinvestigation.View{}, fmt.Errorf("%w: %v", ErrRequestPending, ctx.Err())
		case <-updates:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func sourceProgressDue(phase, lastPhase string, lastEmit time.Time) bool {
	return phase != "" && (phase != lastPhase || time.Since(lastEmit) >= progressHeartbeat)
}

func extendSessionExpiry(current *persistedSession, expires time.Time) {
	if current.ExpiresAt.After(expires) {
		expires = current.ExpiresAt
	}
	current.ExpiresAt = expires
	current.View.ExpiresAt = expires.Format(time.RFC3339)
	for requestID, record := range current.Investigations {
		record.View.ExpiresAt = current.View.ExpiresAt
		current.Investigations[requestID] = record
	}
}

func (s *Service) updateSourceInvestigationProgress(
	sessionID, owner, requestID, leaseID, phase string,
) error {
	if !sourceinvestigation.ValidPhase(phase) {
		return nil
	}
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	changed := false
	err := s.store.update(ctx, func(state *persistedState) (bool, error) {
		current := state.Sessions[sessionID]
		if current == nil || current.Owner != owner {
			return false, nil
		}
		record, ok := current.Investigations[requestID]
		if !ok || record.LeaseID != leaseID || record.View.Status != sourceinvestigation.StatusPending {
			return false, nil
		}
		if record.CancelRequest && phase != sourceinvestigation.PhaseCancelling {
			return false, nil
		}
		if record.View.Phase == phase {
			return false, nil
		}
		record.View.Phase = phase
		record.View.UpdatedAt = now.Format(time.RFC3339)
		current.Investigations[requestID] = record
		changed = true
		return true, nil
	})
	if err == nil && changed {
		s.notifyLocal(sourceInvestigationKey(sessionID, requestID))
	}
	return err
}

func (s *Service) watchSourceInvestigationCancellation(
	ctx context.Context,
	sessionID, owner, requestID, leaseID string,
	cancel context.CancelFunc,
) {
	ticker := time.NewTicker(s.opts.PollInterval)
	defer ticker.Stop()
	for {
		requested, valid, err := s.sourceInvestigationCancellationRequested(sessionID, owner, requestID, leaseID)
		if err == nil && (!valid || requested) {
			if requested {
				cancel()
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) sourceInvestigationCancellationRequested(
	sessionID, owner, requestID, leaseID string,
) (bool, bool, error) {
	ctx, cancel := s.store.context()
	defer cancel()
	requested, valid := false, false
	err := s.store.update(ctx, func(state *persistedState) (bool, error) {
		current := state.Sessions[sessionID]
		if current != nil && current.Owner == owner {
			record, ok := current.Investigations[requestID]
			if ok && record.LeaseID == leaseID && record.View.Status == sourceinvestigation.StatusPending {
				requested, valid = record.CancelRequest, true
			}
		}
		return false, nil
	})
	return requested, valid, err
}

func (s *Service) sourceInvestigationSubject(
	sessionID, owner, chatRequestID string,
) (sourceinvestigation.Subject, error) {
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	var subject sourceinvestigation.Subject
	err := s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		if current.View.Analysis.Scope == ScopePattern {
			return changed, fmt.Errorf("%w: recurring-pattern source investigation is not supported", ErrInvalidRequest)
		}
		request, ok := current.Requests[chatRequestID]
		if !ok || request.Status != requestSucceeded {
			return changed, ErrRequestNotFound
		}
		resolved := restoreResolved(current.Resolved)
		revision, ok := repoRevision(resolved.build.RepoRefs, s.sourceRepo.Owner, s.sourceRepo.Name)
		if !ok {
			refreshed, err := s.resolve(current.View.Analysis)
			if err != nil {
				return changed, err
			}
			if !sameResolvedContext(resolved, refreshed) {
				return changed, ErrAnalysisChanged
			}
			refreshedPersisted := persistResolved(refreshed, sourceRepositoryName(s.sourceRepo))
			current.Resolved.Build.RepoRefs = refreshedPersisted.Build.RepoRefs
			resolved = restoreResolved(current.Resolved)
			revision, ok = repoRevision(resolved.build.RepoRefs, s.sourceRepo.Owner, s.sourceRepo.Name)
			if !ok {
				return changed, fmt.Errorf("%w: build has no revision for %s/%s", sourceinvestigation.ErrUnavailable, s.sourceRepo.Owner, s.sourceRepo.Name)
			}
			changed = true
		}
		repo := sourceinvestigation.Repository{Owner: s.sourceRepo.Owner, Name: s.sourceRepo.Name, Revision: revision}
		if err := sourceinvestigation.ValidateRepository(repo); err != nil {
			return changed, err
		}
		question, answer := chatRequestMessages(current.View.Messages, chatRequestID)
		if question == "" || answer == "" {
			return changed, ErrRequestNotFound
		}
		bounded := persistResolved(resolved, sourceRepositoryName(s.sourceRepo))
		subject = sourceinvestigation.Subject{
			SessionID: current.View.ID, ChatRequestID: chatRequestID, Repository: repo,
			JobID: resolved.jobID, BuildPrefix: resolved.buildPrefix,
			Build: cloneBuildInfo(bounded.Build), TestCase: cloneTestCase(bounded.TestCase),
			Question: question, Answer: answer,
			AnalysisGeneratedAt: resolved.ref.AnalysisGeneratedAt,
		}
		return changed, nil
	})
	return subject, err
}

func sameResolvedContext(left, right resolvedAnalysis) bool {
	if left.ref.Scope == ScopePattern || right.ref.Scope == ScopePattern {
		return left.pattern != nil && right.pattern != nil && models.PatternHash(*left.pattern) == models.PatternHash(*right.pattern)
	}
	if left.ref.JobID != right.ref.JobID || left.ref.BuildID != right.ref.BuildID || left.ref.TestName != right.ref.TestName ||
		left.ref.Source != right.ref.Source || left.ref.SuiteName != right.ref.SuiteName || left.ref.ClassName != right.ref.ClassName || left.ref.JUnitFile != right.ref.JUnitFile {
		return false
	}
	return sameAnalysisSnapshot(analysisSnapshot(left.testCase.AIAnalysis), analysisSnapshot(right.testCase.AIAnalysis))
}

func repoRevision(refs map[string]string, owner, name string) (string, bool) {
	wanted := sourceRepositoryName(sourceinvestigation.Repository{Owner: owner, Name: name})
	var revision string
	for repo, candidate := range refs {
		if strings.ToLower(strings.TrimSpace(repo)) != wanted {
			continue
		}
		candidate, ok := exactRepoRevision(candidate)
		if !ok || revision != "" && revision != candidate {
			return "", false
		}
		revision = candidate
	}
	return revision, revision != ""
}

func exactRepoRevision(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if validSourceRevision(value) {
		return strings.ToLower(value), true
	}
	if strings.Count(value, ":") != 1 || strings.Contains(value, ",") {
		return "", false
	}
	_, value, _ = strings.Cut(value, ":")
	value = strings.TrimSpace(value)
	if !validSourceRevision(value) {
		return "", false
	}
	return strings.ToLower(value), true
}

func validSourceRevision(revision string) bool {
	return sourceinvestigation.ValidateRepository(sourceinvestigation.Repository{
		Owner: "source", Name: "repository", Revision: revision,
	}) == nil
}

func sourceRepositoryName(repo sourceinvestigation.Repository) string {
	return strings.ToLower(strings.TrimSpace(repo.Owner) + "/" + strings.TrimSpace(repo.Name))
}

func chatRequestMessages(messages []Message, requestID string) (string, string) {
	var question, answer string
	for _, message := range messages {
		if message.RequestID != requestID {
			continue
		}
		switch message.Role {
		case "user":
			question = strings.TrimSpace(message.Content)
		case "assistant":
			answer = strings.TrimSpace(message.Content)
		}
	}
	return question, answer
}

func hashSourceInvestigationInput(subject sourceinvestigation.Subject) (string, error) {
	data, err := json.Marshal(subject)
	if err != nil {
		return "", fmt.Errorf("encoding source investigation input: %w", err)
	}
	return hashBytes(data), nil
}

func (s *Service) sourceInvestigationTerminalError(sessionID, owner, requestID string) error {
	ctx, cancel := s.store.context()
	defer cancel()
	var record persistedInvestigation
	err := s.store.update(ctx, func(state *persistedState) (bool, error) {
		current := state.Sessions[strings.TrimSpace(sessionID)]
		if current == nil || current.Owner != owner {
			return false, ErrSessionNotFound
		}
		var ok bool
		record, ok = current.Investigations[requestID]
		if !ok {
			return false, ErrRequestNotFound
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	return sourceInvestigationRecordError(record)
}

func sourceInvestigationRecordError(record persistedInvestigation) error {
	switch record.View.Status {
	case sourceinvestigation.StatusFailed:
		return persistedRequestError(record.FailureKind)
	case sourceinvestigation.StatusUnknown:
		return ErrRequestOutcomeUnknown
	default:
		return nil
	}
}

func activeSourceInvestigation(record persistedInvestigation) bool {
	return record.View.Status == sourceinvestigation.StatusPending && record.LeaseID != ""
}

func (s *Service) activeSourceInvestigationsForOwner(state *persistedState, owner string) int {
	count := 0
	for _, session := range state.Sessions {
		if session.Owner != owner {
			continue
		}
		for _, record := range session.Investigations {
			if activeSourceInvestigation(record) {
				count++
			}
		}
	}
	return count
}

func cloneSourceInvestigationView(view sourceinvestigation.View) sourceinvestigation.View {
	view.Result = sourceinvestigation.CloneResult(view.Result)
	return view
}

func sourceInvestigationKey(sessionID, requestID string) string {
	return "source\x00" + sessionID + "\x00" + requestID
}

func (s *Service) cancelLocalKey(key string) {
	s.activeMu.Lock()
	cancel := s.active[key]
	s.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

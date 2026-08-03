package analysischat

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
)

const (
	requestRateWindow = time.Minute
	progressHeartbeat = 15 * time.Second
)

type startTurnResult struct {
	View    SessionView
	Turn    Turn
	LeaseID string
	Started bool
	Pending bool
}

type requestSnapshot struct {
	View        SessionView
	Status      string
	FailureKind string
	Progress    Progress
}

// Send waits for one idempotent turn and returns the authoritative transcript.
func (s *Service) Send(ctx context.Context, id, owner, requestID, question string) (SessionView, error) {
	result, err := s.startTurn(ctx, id, owner, requestID, question)
	if err != nil || result.View.ID != "" {
		return result.View, err
	}
	if result.Pending && !result.Started {
		return SessionView{}, ErrSessionBusy
	}
	return s.waitForRequest(ctx, id, owner, requestID, nil)
}

// Stream starts or follows one idempotent turn and emits persisted progress.
func (s *Service) Stream(
	ctx context.Context,
	id, owner, requestID, question string,
	emit func(Progress) error,
) (SessionView, error) {
	result, err := s.startTurn(ctx, id, owner, requestID, question)
	if err != nil || result.View.ID != "" {
		return result.View, err
	}
	return s.waitForRequest(ctx, id, owner, requestID, emit)
}

// Cancel requests cancellation of one active idempotent turn.
func (s *Service) Cancel(id, owner, requestID string) error {
	owner = normalizeOwner(owner)
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return err
	}
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	var active bool
	err = s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(id)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		request, ok := current.Requests[requestID]
		if !ok {
			return changed, ErrRequestNotFound
		}
		if request.Status != requestPending {
			return changed, nil
		}
		if current.Active == nil || current.Active.RequestID != requestID {
			return changed, ErrRequestOutcomeUnknown
		}
		stamp := now.Format(time.RFC3339)
		request.UpdatedAt = stamp
		current.Requests[requestID] = request
		current.Active.CancelRequested = true
		current.Active.Phase = PhaseCancelling
		current.Active.UpdatedAt = now
		current.View.UpdatedAt = stamp
		active = true
		return true, nil
	})
	if err != nil {
		return err
	}
	if active {
		s.notifyLocal(activeTurnKey(id, requestID))
		s.cancelLocal(id, requestID)
	}
	return nil
}

// Wait blocks until all server-owned turns have persisted a terminal outcome.
func (s *Service) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.activeWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) startTurn(ctx context.Context, id, owner, requestID, question string) (startTurnResult, error) {
	question = strings.TrimSpace(question)
	if question == "" || len(question) > s.opts.MaxQuestionBytes {
		return startTurnResult{}, fmt.Errorf("%w: question must be 1-%d bytes", ErrInvalidRequest, s.opts.MaxQuestionBytes)
	}
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return startTurnResult{}, err
	}
	owner = normalizeOwner(owner)
	questionHash := hashText(question)
	now := s.opts.Now().UTC()
	leaseID, err := newSessionID()
	if err != nil {
		return startTurnResult{}, fmt.Errorf("creating analysis chat turn lease: %w", err)
	}

	var result startTurnResult
	storeCtx, cancel := context.WithTimeout(ctx, s.opts.StoreLockTimeout)
	err = s.store.update(storeCtx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(id)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		if current.Requests == nil {
			current.Requests = map[string]persistedRequest{}
			changed = true
		}
		if previous, ok := current.Requests[requestID]; ok {
			if previous.QuestionHash != questionHash {
				return changed, ErrIdempotencyConflict
			}
			switch previous.Status {
			case requestSucceeded:
				result.View = s.sessionView(current)
				return changed, nil
			case requestFailed:
				return changed, persistedRequestError(previous.FailureKind)
			case requestUnknown:
				return changed, ErrRequestOutcomeUnknown
			default:
				result.Pending = true
				return changed, nil
			}
		}
		if current.Active != nil {
			return changed, ErrSessionBusy
		}
		if current.Turns >= s.opts.MaxTurns {
			return changed, ErrTurnLimit
		}
		if s.activeTurnsForOwner(state, owner) >= s.opts.MaxActiveTurnsPerOwner {
			return changed, ErrActiveTurnLimit
		}
		if len(state.OwnerRequests[owner]) >= s.opts.MaxRequestsPerOwnerPerMinute {
			return changed, ErrRateLimit
		}

		current.Turns++
		stamp := now.Format(time.RFC3339)
		state.OwnerRequests[owner] = append(state.OwnerRequests[owner], now)
		current.Requests[requestID] = persistedRequest{
			QuestionHash: questionHash, Question: question, Status: requestPending,
			Turn: current.Turns, CreatedAt: stamp, UpdatedAt: stamp,
		}
		current.Active = &persistedActiveTurn{
			RequestID: requestID, Question: question, LeaseID: leaseID,
			ExpiresAt: now.Add(s.opts.TurnLeaseTTL), Phase: PhaseQueued, UpdatedAt: now,
		}
		current.View.UpdatedAt = stamp
		resolved := restoreResolved(current.Resolved)
		result = startTurnResult{
			LeaseID: leaseID, Started: true, Pending: true,
			Turn: Turn{
				SessionID: current.View.ID, JobID: resolved.jobID,
				BuildPrefix: resolved.buildPrefix, Build: cloneBuildInfo(resolved.build),
				TestCase: cloneTestCase(resolved.testCase), Pattern: clonePattern(resolved.pattern),
				EvidenceBuilds: cloneArtifactBuilds(resolved.evidenceBuilds),
				History:        cloneSessionView(current.View).Messages, Question: question,
			},
		}
		return true, nil
	})
	cancel()
	if err != nil {
		return startTurnResult{}, err
	}
	if result.Started {
		s.activeWG.Add(1)
		go func() {
			defer s.activeWG.Done()
			s.runTurn(id, owner, requestID, leaseID, result.Turn)
		}()
	}
	return result, nil
}

func (s *Service) runTurn(id, owner, requestID, leaseID string, turn Turn) {
	runCtx, cancel := context.WithTimeout(s.lifecycle, s.opts.TurnTimeout)
	key := activeTurnKey(id, requestID)
	s.activeMu.Lock()
	s.active[key] = cancel
	s.activeMu.Unlock()
	defer func() {
		cancel()
		s.activeMu.Lock()
		delete(s.active, key)
		s.activeMu.Unlock()
	}()

	turn.Progress = func(phase string) { _ = s.updateProgress(id, owner, requestID, leaseID, phase) }
	turn.ReportProgress(PhaseInvestigating)
	runCtx, usageOperation := aiusage.Begin(runCtx, s.opts.UsageRecorder, aiusage.Metadata{
		LogicalID: requestID, Origin: aiusage.OriginServer, Feature: aiusage.FeatureAnalysisChat,
		Correlation: aiusage.Correlation{JobID: turn.JobID, BuildID: turn.Build.BuildID, TestName: turn.TestCase.Name},
	})
	go s.watchCancellation(runCtx, id, owner, requestID, leaseID, cancel)
	reply, runErr := s.runner.Reply(runCtx, turn)
	usageOutcome := aiusage.OutcomeSuccess
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		usageOutcome = aiusage.OutcomeCancelled
	} else if runErr != nil {
		usageOutcome = aiusage.OutcomeError
	}
	usageOperation.Finish(usageOutcome)
	if err := s.finishTurn(id, owner, requestID, leaseID, turn.Question, reply, runErr); err != nil &&
		!errors.Is(err, ErrRequestOutcomeUnknown) && !errors.Is(err, ErrSessionNotFound) {
		log.Printf("analysis chat turn %s finalize: %v", requestID, err)
	}
}

func (s *Service) finishTurn(id, owner, requestID, leaseID, question string, reply Reply, runErr error) error {
	finishedAt := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	err := s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, finishedAt)
		current := state.Sessions[strings.TrimSpace(id)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		if current.Active == nil || current.Active.RequestID != requestID || current.Active.LeaseID != leaseID {
			return changed, ErrRequestOutcomeUnknown
		}
		active := current.Active
		previous := current.Requests[requestID]
		if current.Active.CancelRequested {
			runErr = context.Canceled
		}
		current.Active = nil
		extendSessionExpiry(current, finishedAt.Add(s.opts.SessionTTL))
		stamp := finishedAt.Format(time.RFC3339)
		if previous.Question == "" {
			previous.Question = question
		}
		if previous.Turn == 0 {
			previous.Turn = current.Turns
		}
		if previous.CreatedAt == "" && active != nil && !active.UpdatedAt.IsZero() {
			previous.CreatedAt = active.UpdatedAt.UTC().Format(time.RFC3339)
		}
		previous.UpdatedAt = stamp
		current.View.UpdatedAt = stamp
		if runErr != nil {
			previous.Status = requestFailed
			previous.FailureKind = requestFailureKind(runErr)
			current.Requests[requestID] = previous
			return true, nil
		}
		current.View.Messages = append(current.View.Messages,
			Message{Role: "user", RequestID: requestID, Content: question, CreatedAt: stamp},
			Message{
				Role: "assistant", RequestID: requestID, Content: reply.Answer, Assessment: reply.Assessment,
				Citations: slices.Clone(reply.Citations), ProposedRevision: cloneRevision(reply.ProposedRevision),
				ToolCalls: reply.ToolCalls, GCSBytes: reply.GCSBytes, ElapsedMs: reply.ElapsedMs,
				CreatedAt: stamp,
			},
		)
		current.View.UpdatedAt = stamp
		previous.Status = requestSucceeded
		current.Requests[requestID] = previous
		return true, nil
	})
	s.notifyLocal(activeTurnKey(id, requestID))
	return err
}

func (s *Service) waitForRequest(
	ctx context.Context,
	id, owner, requestID string,
	emit func(Progress) error,
) (SessionView, error) {
	owner = normalizeOwner(owner)
	requestID, err := normalizeRequestID(requestID)
	if err != nil {
		return SessionView{}, err
	}
	updates, unsubscribe := s.subscribe(activeTurnKey(id, requestID))
	defer unsubscribe()
	lastPhase := ""
	lastEmit := time.Time{}
	for {
		snapshot, err := s.requestSnapshot(id, owner, requestID)
		if err != nil {
			return SessionView{}, err
		}
		if snapshot.Progress.Phase != "" &&
			(snapshot.Progress.Phase != lastPhase || time.Since(lastEmit) >= progressHeartbeat) {
			lastPhase = snapshot.Progress.Phase
			lastEmit = time.Now()
			if emit != nil {
				if err := emit(snapshot.Progress); err != nil {
					return SessionView{}, err
				}
			}
		}
		switch snapshot.Status {
		case requestSucceeded:
			return snapshot.View, nil
		case requestFailed:
			return SessionView{}, persistedRequestError(snapshot.FailureKind)
		case requestUnknown:
			return SessionView{}, ErrRequestOutcomeUnknown
		}
		timer := time.NewTimer(s.opts.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return SessionView{}, fmt.Errorf("%w: %v", ErrRequestPending, ctx.Err())
		case <-updates:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (s *Service) requestSnapshot(id, owner, requestID string) (requestSnapshot, error) {
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	var snapshot requestSnapshot
	err := s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(id)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		request, ok := current.Requests[requestID]
		if !ok {
			return changed, ErrRequestNotFound
		}
		snapshot.Status = request.Status
		snapshot.FailureKind = request.FailureKind
		if request.Status == requestSucceeded {
			snapshot.View = s.sessionView(current)
		}
		if current.Active != nil && current.Active.RequestID == requestID {
			snapshot.Progress = Progress{
				RequestID: requestID, Phase: current.Active.Phase,
				UpdatedAt: current.Active.UpdatedAt.Format(time.RFC3339),
				TurnsUsed: current.Turns, MaxTurns: s.opts.MaxTurns,
			}
		}
		return changed, nil
	})
	return snapshot, err
}

func (s *Service) updateProgress(id, owner, requestID, leaseID, phase string) error {
	if !validProgressPhase(phase) {
		return nil
	}
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	changed := false
	err := s.store.update(ctx, func(state *persistedState) (bool, error) {
		current := state.Sessions[id]
		if current == nil || current.Owner != owner || current.Active == nil ||
			current.Active.RequestID != requestID || current.Active.LeaseID != leaseID {
			return false, nil
		}
		if current.Active.CancelRequested && phase != PhaseCancelling {
			return false, nil
		}
		if current.Active.Phase == phase {
			return false, nil
		}
		current.Active.Phase = phase
		current.Active.UpdatedAt = now
		changed = true
		return true, nil
	})
	if err == nil && changed {
		s.notifyLocal(activeTurnKey(id, requestID))
	}
	return err
}

func (s *Service) watchCancellation(
	ctx context.Context,
	id, owner, requestID, leaseID string,
	cancel context.CancelFunc,
) {
	ticker := time.NewTicker(s.opts.PollInterval)
	defer ticker.Stop()
	for {
		requested, valid, err := s.cancellationRequested(id, owner, requestID, leaseID)
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

func (s *Service) cancellationRequested(id, owner, requestID, leaseID string) (bool, bool, error) {
	ctx, cancel := s.store.context()
	defer cancel()
	requested, valid := false, false
	err := s.store.update(ctx, func(state *persistedState) (bool, error) {
		current := state.Sessions[id]
		if current != nil && current.Owner == owner && current.Active != nil &&
			current.Active.RequestID == requestID && current.Active.LeaseID == leaseID {
			requested, valid = current.Active.CancelRequested, true
		}
		return false, nil
	})
	return requested, valid, err
}

func (s *Service) cancelLocal(id, requestID string) {
	s.activeMu.Lock()
	cancel := s.active[activeTurnKey(id, requestID)]
	s.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Service) activeTurnsForOwner(state *persistedState, owner string) int {
	count := 0
	for _, session := range state.Sessions {
		if session.Owner == owner && session.Active != nil {
			count++
		}
	}
	return count
}

func (s *Service) subscribe(key string) (<-chan struct{}, func()) {
	updates := make(chan struct{}, 1)
	s.notifyMu.Lock()
	listeners := s.notify[key]
	if listeners == nil {
		listeners = map[chan struct{}]struct{}{}
		s.notify[key] = listeners
	}
	listeners[updates] = struct{}{}
	s.notifyMu.Unlock()
	return updates, func() {
		s.notifyMu.Lock()
		delete(s.notify[key], updates)
		if len(s.notify[key]) == 0 {
			delete(s.notify, key)
		}
		s.notifyMu.Unlock()
	}
}

func (s *Service) notifyLocal(key string) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	for listener := range s.notify[key] {
		select {
		case listener <- struct{}{}:
		default:
		}
	}
}

func activeTurnKey(id, requestID string) string {
	return id + "\x00" + requestID
}

func validProgressPhase(phase string) bool {
	switch phase {
	case PhaseQueued, PhaseInvestigating, PhaseReadingEvidence, PhaseEvaluating, PhaseFinalizing, PhaseCancelling:
		return true
	default:
		return false
	}
}

func pruneOwnerRequestTimes(times []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-requestRateWindow)
	return slices.DeleteFunc(times, func(value time.Time) bool { return value.Before(cutoff) })
}

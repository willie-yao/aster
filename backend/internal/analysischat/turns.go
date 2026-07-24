package analysischat

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
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
		current.Active.CancelRequested = true
		current.Active.Phase = PhaseCancelling
		current.Active.UpdatedAt = now
		active = true
		return true, nil
	})
	if err != nil {
		return err
	}
	if active {
		s.cancelLocal(id, requestID)
	}
	return nil
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
				result.View = cloneSessionView(current.View)
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
		state.OwnerRequests[owner] = append(state.OwnerRequests[owner], now)
		current.Requests[requestID] = persistedRequest{QuestionHash: questionHash, Status: requestPending}
		current.Active = &persistedActiveTurn{
			RequestID: requestID, LeaseID: leaseID,
			ExpiresAt: now.Add(s.opts.TurnLeaseTTL), Phase: PhaseQueued, UpdatedAt: now,
		}
		resolved := restoreResolved(current.Resolved)
		result = startTurnResult{
			LeaseID: leaseID, Started: true, Pending: true,
			Turn: Turn{
				SessionID: current.View.ID, JobID: resolved.jobID,
				BuildPrefix: resolved.buildPrefix, Build: cloneBuildInfo(resolved.build),
				TestCase: cloneTestCase(resolved.testCase),
				History:  cloneSessionView(current.View).Messages, Question: question,
			},
		}
		return true, nil
	})
	cancel()
	if err != nil {
		return startTurnResult{}, err
	}
	if result.Started {
		go s.runTurn(id, owner, requestID, leaseID, result.Turn)
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
	go s.watchCancellation(runCtx, id, owner, requestID, leaseID, cancel)
	reply, runErr := s.runner.Reply(runCtx, turn)
	if err := s.finishTurn(id, owner, requestID, leaseID, turn.Question, reply, runErr); err != nil &&
		!errors.Is(err, ErrRequestOutcomeUnknown) && !errors.Is(err, ErrSessionNotFound) {
		log.Printf("analysis chat turn %s finalize: %v", requestID, err)
	}
}

func (s *Service) finishTurn(id, owner, requestID, leaseID, question string, reply Reply, runErr error) error {
	finishedAt := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	return s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, finishedAt)
		current := state.Sessions[strings.TrimSpace(id)]
		if current == nil || current.Owner != owner {
			return changed, ErrSessionNotFound
		}
		if current.Active == nil || current.Active.RequestID != requestID || current.Active.LeaseID != leaseID {
			return changed, ErrRequestOutcomeUnknown
		}
		previous := current.Requests[requestID]
		current.Active = nil
		current.ExpiresAt = finishedAt.Add(s.opts.SessionTTL)
		current.View.ExpiresAt = current.ExpiresAt.Format(time.RFC3339)
		if runErr != nil {
			previous.Status = requestFailed
			previous.FailureKind = requestFailureKind(runErr)
			current.Requests[requestID] = previous
			return true, nil
		}
		stamp := finishedAt.Format(time.RFC3339)
		current.View.Messages = append(current.View.Messages,
			Message{Role: "user", RequestID: requestID, Content: question, CreatedAt: stamp},
			Message{
				Role: "assistant", Content: reply.Answer, Assessment: reply.Assessment,
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
		select {
		case <-ctx.Done():
			return SessionView{}, fmt.Errorf("%w: %v", ErrRequestPending, ctx.Err())
		case <-time.After(s.opts.PollInterval):
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
			snapshot.View = cloneSessionView(current.View)
		}
		if current.Active != nil && current.Active.RequestID == requestID {
			snapshot.Progress = Progress{
				RequestID: requestID, Phase: current.Active.Phase,
				UpdatedAt: current.Active.UpdatedAt.Format(time.RFC3339),
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
	return s.store.update(ctx, func(state *persistedState) (bool, error) {
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
		return true, nil
	})
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

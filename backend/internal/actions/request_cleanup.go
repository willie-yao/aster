package actions

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/willie-yao/aster/backend/internal/runtime"
)

func (s *Service) registerCleanupLocked(id string) bool {
	if s.requestCleanups == nil {
		s.requestCleanups = map[string]struct{}{}
	}
	if _, running := s.requestCleanups[id]; running {
		return false
	}
	s.requestCleanups[id] = struct{}{}
	s.requestWG.Add(1)
	return true
}

func (s *Service) startCleanup(id string) {
	if s.claimCleanup(id) {
		s.launchCleanup(id)
	}
}

// claimCleanup reserves the single-flight cleanup slot for a request.
func (s *Service) claimCleanup(id string) bool {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	return s.registerCleanupLocked(id)
}

// releaseCleanup frees the slot taken by claimCleanup.
func (s *Service) releaseCleanup(id string) {
	s.rmu.Lock()
	delete(s.requestCleanups, id)
	s.rmu.Unlock()
	s.requestWG.Done()
}

func (s *Service) launchCleanup(id string) {
	go func() {
		defer s.releaseCleanup(id)
		backoff := 250 * time.Millisecond
		for {
			ctx, cancel := context.WithTimeout(context.Background(), defaultRuntimeCleanupTimeout)
			view, err := s.cleanupRequest(ctx, id)
			cancel()
			current := s.currentRequestView(id)
			if current.Status != RequestCancelling && view.Status != RequestCancelling {
				return
			}
			if errors.Is(err, runtime.ErrWorkIdentityChanged) {
				if s.markCleanupBlocked(id) {
					return
				}
				err = runtime.ErrCleanupPending
			}
			if err != nil {
				log.Printf("action request %s: runtime cleanup retry: %v", id, err)
			}
			time.Sleep(backoff)
			if backoff < 5*time.Second {
				backoff *= 2
				if backoff > 5*time.Second {
					backoff = 5 * time.Second
				}
			}
		}
	}()
}

func (s *Service) markCleanupBlocked(id string) bool {
	s.rmu.Lock()
	request := s.requests.Requests[id]
	if request == nil || request.Status != RequestCancelling {
		s.rmu.Unlock()
		return true
	}
	now := time.Now().UTC().Format(time.RFC3339)
	previous := *request
	cleanup := &actionCleanupState{FinalStatus: RequestFailed, Reason: "runtime work identity changed during cleanup", RequestedAt: now}
	if request.Kind == requestKindAnalysisFix {
		cleanup.ReasonCode = ReasonGenerationFailed
		cleanup.Failure = &AnalysisFixFailureView{Category: AnalysisFixFailureSafetyIntegrity}
	}
	request.Cleanup = cleanup
	request.UpdatedAt = now
	if err := s.saveRequestsLocked(); err != nil {
		*request = previous
		log.Printf("action request %s: persist identity-change cleanup: %v", id, err)
		s.rmu.Unlock()
		return false
	}
	s.rmu.Unlock()
	if _, err := s.finalizeCleanup(id); err != nil {
		log.Printf("action request %s: finalize identity-change cleanup: %v", id, err)
		return false
	}
	return true
}

func (s *Service) cleanupRequest(ctx context.Context, id string) (ActionRequestView, error) {
	generationDone := false
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, defaultRuntimeCleanupTimeout)
	defer cancel()
	for {
		s.rmu.Lock()
		request := s.requests.Requests[id]
		if request == nil {
			s.rmu.Unlock()
			return ActionRequestView{}, ErrRequestNotFound
		}
		if request.Status != RequestCancelling {
			view := request.ActionRequestView
			s.rmu.Unlock()
			return view, nil
		}
		var work *runtime.WorkRef
		if request.Runtime != nil {
			copy := *request.Runtime
			work = &copy
		}
		done := s.requestDone[id]
		s.rmu.Unlock()

		if work != nil {
			if s.managedRuntime == nil {
				return s.currentRequestView(id), runtime.ErrUnavailable
			}
			cleaner, err := s.managedRuntime()
			if err != nil {
				return s.currentRequestView(id), err
			}
			if cleaner == nil {
				return s.currentRequestView(id), runtime.ErrUnavailable
			}
			if err := cleaner.Cleanup(ctx, *work); err != nil {
				return s.currentRequestView(id), err
			}
			if work.UID == "" && done != nil && !generationDone {
				select {
				case <-done:
					generationDone = true
					continue
				case <-ctx.Done():
					return s.currentRequestView(id), ctx.Err()
				}
			}
			return s.finalizeCleanup(id)
		}
		if done == nil || generationDone {
			return s.finalizeCleanup(id)
		}
		select {
		case <-done:
			// The observer may have persisted runtime identity before generation exited.
			generationDone = true
			continue
		case <-ctx.Done():
			return s.currentRequestView(id), ctx.Err()
		}
	}
}

func (s *Service) currentRequestView(id string) ActionRequestView {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if request := s.requests.Requests[id]; request != nil {
		return request.ActionRequestView
	}
	return ActionRequestView{}
}

func (s *Service) finalizeCleanup(id string) (ActionRequestView, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	request := s.requests.Requests[id]
	if request == nil {
		return ActionRequestView{}, ErrRequestNotFound
	}
	if request.Status != RequestCancelling {
		return request.ActionRequestView, nil
	}
	finalStatus := RequestCancelled
	reason := ""
	var reasonCode ReasonCode
	var failure *AnalysisFixFailureView
	if request.Cleanup != nil {
		if request.Cleanup.FinalStatus != "" {
			finalStatus = request.Cleanup.FinalStatus
		}
		reason = request.Cleanup.Reason
		reasonCode = request.Cleanup.ReasonCode
		failure = cloneAnalysisFixFailureView(request.Cleanup.Failure)
	}
	previous := *request
	request.Status = finalStatus
	request.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if finalStatus != RequestFailed || request.Kind != requestKindAnalysisFix {
		request.Warning = ""
	}
	request.Preview = nil
	request.Instruction = ""
	request.AnalysisFix = nil
	request.Issue = nil
	request.Fix = nil
	request.BaseIssue = nil
	request.BaseTargetRepo = ""
	request.BasePatternHash = ""
	request.Runtime = nil
	request.Cleanup = nil
	request.EmailError = ""
	if finalStatus == RequestFailed {
		request.Error = reason
		request.ReasonCode = ReasonGenerationFailed
		request.Failure = failure
		if validReasonCode(reasonCode) {
			request.ReasonCode = reasonCode
		}
	} else {
		request.Error = ""
		request.ReasonCode = ""
		request.Failure = nil
	}
	if err := s.saveRequestsLocked(); err != nil {
		*request = previous
		return ActionRequestView{}, err
	}
	return request.ActionRequestView, nil
}

func (s *Service) transitionToCleanup(id, finalStatus, reason string) (context.CancelFunc, error) {
	return s.transitionToCleanupWithFailure(id, finalStatus, reason, "", nil)
}

func (s *Service) transitionToCleanupWithFailure(
	id, finalStatus, reason string,
	reasonCode ReasonCode,
	failure *AnalysisFixFailureView,
) (context.CancelFunc, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	request := s.requests.Requests[id]
	if request == nil {
		return nil, ErrRequestNotFound
	}
	if request.Status == RequestCancelling || request.Status == RequestCancelled {
		return s.requestCancels[id], nil
	}
	if request.Status != RequestPending && request.Status != RequestReady {
		return nil, fmt.Errorf("action request is %s", request.Status)
	}
	previous := *request
	now := time.Now().UTC().Format(time.RFC3339)
	request.Status = RequestCancelling
	request.Cleanup = &actionCleanupState{
		FinalStatus: finalStatus, Reason: reason, ReasonCode: reasonCode,
		Failure: cloneAnalysisFixFailureView(failure), RequestedAt: now,
	}
	request.UpdatedAt = now
	if err := s.saveRequestsLocked(); err != nil {
		*request = previous
		return nil, err
	}
	s.cancelRequestNotificationLocked(id)
	return s.requestCancels[id], nil
}

func (s *Service) stopActiveRequests() {
	s.rmu.Lock()
	s.stopping = true
	var ids []string
	var cancels []context.CancelFunc
	now := time.Now().UTC().Format(time.RFC3339)
	for id, request := range s.requests.Requests {
		if request.Status != RequestPending {
			continue
		}
		request.Status = RequestCancelling
		request.Cleanup = &actionCleanupState{FinalStatus: RequestFailed, Reason: "server stopped before draft generation completed", RequestedAt: now}
		request.UpdatedAt = now
		ids = append(ids, id)
		if cancel := s.requestCancels[id]; cancel != nil {
			cancels = append(cancels, cancel)
		}
	}
	if len(ids) > 0 {
		if err := s.saveRequestsLocked(); err != nil {
			log.Printf("action requests: persist shutdown cleanup: %v", err)
		}
	}
	s.rmu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, id := range ids {
		s.startCleanup(id)
	}
}

// cleanupCancellingRequest cleans up a cancelling request inline and hands it to
// a retry goroutine when the inline attempt cannot finish. It takes the same
// single-flight slot the retry goroutines use, so a runtime work ref is never
// cleaned up twice concurrently.
func (s *Service) cleanupCancellingRequest(ctx context.Context, id string) (ActionRequestView, error) {
	if !s.claimCleanup(id) {
		return s.currentRequestView(id), nil
	}
	retry := false
	defer func() {
		if retry {
			// Hand the claim to the retry goroutine so the pending cleanup stays
			// tracked by requestWG without the count dropping to zero.
			s.launchCleanup(id)
			return
		}
		s.releaseCleanup(id)
	}()
	view, err := s.cleanupRequest(ctx, id)
	if err != nil && s.currentRequestView(id).Status == RequestCancelling {
		retry = !errors.Is(err, runtime.ErrWorkIdentityChanged) || !s.markCleanupBlocked(id)
		// Report the state observed before any retry starts so the response does
		// not depend on how quickly the retry goroutine runs.
		view, err = s.currentRequestView(id), nil
	}
	return view, err
}

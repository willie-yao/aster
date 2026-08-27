package actions

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/willie-yao/aster/backend/internal/issues"
	"github.com/willie-yao/aster/backend/internal/patternstate"
	"github.com/willie-yao/aster/backend/internal/redact"
	"github.com/willie-yao/aster/backend/internal/runtime"
)

func (s *Service) startGeneration(id, userToken string) {
	s.rmu.Lock()
	if s.requestDone == nil {
		s.requestDone = map[string]chan struct{}{}
	}
	if s.requestDone[id] == nil {
		s.requestDone[id] = make(chan struct{})
	}
	kind := ""
	if request := s.requests.Requests[id]; request != nil {
		kind = request.Kind
	}
	s.rmu.Unlock()
	s.requestWG.Add(1)
	go func() {
		defer s.requestWG.Done()
		if kind == requestKindAnalysisFix {
			s.generateAnalysisFixRequest(id, userToken)
			return
		}
		s.generateRequest(id, userToken)
	}()
}

func (s *Service) finishGeneration(id string) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if done := s.requestDone[id]; done != nil {
		select {
		case <-done:
		default:
			close(done)
		}
		delete(s.requestDone, id)
	}
}

type requestPreviewGenerator func(context.Context, string, string, string, string, *issues.IssueSpec, string, string) (PreviewResult, *previewEntry, error)

func (s *Service) generateRequest(id, userToken string) {
	s.generateRequestWith(id, userToken, s.generateRequestPreview)
}

func (s *Service) generateRequestPreview(ctx context.Context, failureID, kind, userToken, instruction string, baseIssue *issues.IssueSpec, baseTargetRepo, basePatternHash string) (PreviewResult, *previewEntry, error) {
	switch kind {
	case "create-issue":
		return s.generateIssuePreview(ctx, failureID, userToken, instruction, baseIssue, baseTargetRepo, basePatternHash)
	case "propose-fix":
		return s.generateFixPreview(ctx, failureID, userToken, instruction)
	default:
		return PreviewResult{}, nil, fmt.Errorf("unsupported action %q", kind)
	}
}

func (s *Service) generateRequestWith(id, userToken string, generate requestPreviewGenerator) {
	s.generateRequestOperation(id, func(ctx context.Context, input requestGenerationInput) (PreviewResult, *previewEntry, error) {
		return generate(ctx, input.failureID, input.kind, userToken, input.instruction, input.baseIssue, input.baseTargetRepo, input.basePatternHash)
	})
}

type requestGenerationInput struct {
	failureID, kind, owner, instruction string
	baseTargetRepo, basePatternHash     string
	baseIssue                           *issues.IssueSpec
	analysisFix                         *AnalysisFixInput
}

type requestOperationGenerator func(context.Context, requestGenerationInput) (PreviewResult, *previewEntry, error)

func (s *Service) generateAnalysisFixRequest(id, userToken string) {
	s.generateRequestOperation(id, func(ctx context.Context, input requestGenerationInput) (PreviewResult, *previewEntry, error) {
		if input.analysisFix == nil {
			return PreviewResult{}, nil, fmt.Errorf("analysis fix request context is missing")
		}
		generate := s.analysisRequestGenerator
		if generate == nil {
			generate = s.PreviewAnalysisFix
		}
		preview, err := generate(ctx, *input.analysisFix, input.owner, userToken, input.instruction)
		return preview, nil, err
	})
}

// logGenerationFailure records why one draft generation failed. The runtime
// wraps the executor's failure reason into the error chain, so this is where an
// operator recovers a sandbox-side cause such as a rejected provider credential.
func logGenerationFailure(id string, code ReasonCode, failure *AnalysisFixFailureView, err error) {
	detail := "no additional detail"
	if cause := generationFailureCause(err); cause != nil {
		detail = redact.OperatorText(cause.Error())
	}
	category := "unknown"
	if failure != nil && failure.Category != "" {
		category = string(failure.Category)
	}
	if failure != nil && failure.Category == AnalysisFixFailureNoReviewablePatch &&
		failure.Detail == AnalysisFixFailureDetailNoRepositoryChange && failure.TerminalState == runtime.TerminalSucceeded {
		if failure.OperatorSummary != "" {
			log.Printf("action request %s: generation completed without a reviewable patch (reason=%s category=%s): %s; agent_summary=%q", id, code, category, detail, failure.OperatorSummary)
			return
		}
		log.Printf("action request %s: generation completed without a reviewable patch (reason=%s category=%s): %s", id, code, category, detail)
		return
	}
	log.Printf("action request %s: generation failed (reason=%s category=%s): %s", id, code, category, detail)
}

// generationFailureCause returns the private cause behind a classified action
// error. A classified error reports the static operator message, so the cause is
// the only place the underlying diagnosis survives.
func generationFailureCause(err error) error {
	var reasonErr *ReasonError
	if errors.As(err, &reasonErr) && reasonErr.Cause != nil {
		return reasonErr.Cause
	}
	return err
}

func generationFailureMessage(analysisRequest bool, code ReasonCode, failure *AnalysisFixFailureView, err error) string {
	if analysisRequest && code == ReasonNoReviewablePatch && failure != nil && failure.Category == AnalysisFixFailureNoReviewablePatch {
		return err.Error()
	}
	return ReasonMessage(code)
}

func (s *Service) generateRequestOperation(id string, generate requestOperationGenerator) {
	ctx, cancel := context.WithTimeout(withActionRequestID(context.Background(), id), s.requestTimeout)
	needsCleanup := false
	s.rmu.Lock()
	s.requestCancels[id] = cancel
	s.rmu.Unlock()
	defer func() {
		cancel()
		s.rmu.Lock()
		delete(s.requestCancels, id)
		s.rmu.Unlock()
		s.finishGeneration(id)
		if needsCleanup {
			s.startCleanup(id)
		}
	}()

	s.rmu.Lock()
	request := s.requests.Requests[id]
	if request == nil || request.Status != RequestPending {
		s.rmu.Unlock()
		return
	}
	input := requestGenerationInput{
		failureID: request.FailureID, kind: request.Kind, owner: request.Owner, instruction: request.Instruction,
		baseTargetRepo: request.BaseTargetRepo, basePatternHash: request.BasePatternHash,
	}
	if request.BaseIssue != nil {
		base := *request.BaseIssue
		base.Labels = slices.Clone(base.Labels)
		input.baseIssue = &base
	}
	if request.AnalysisFix != nil {
		input.analysisFix = cloneAnalysisFixInput(*request.AnalysisFix)
	}
	s.rmu.Unlock()

	preview, entry, err := generate(ctx, input)
	analysisRequest := input.kind == requestKindAnalysisFix
	fallbackPreview := !analysisRequest && errors.Is(err, ErrDraftRefinementRejected) && entry != nil
	if err == nil || fallbackPreview {
		if analysisRequest {
			if validated, validateErr := validatedAnalysisFixRequestPreview(preview); validateErr != nil {
				err = classifiedAnalysisPreviewValidationError(validateErr)
			} else {
				preview = validated
			}
		} else if validateErr := s.validateSubjectSnapshot(input.failureID, entry.patternHash, entry.kind); validateErr != nil {
			err = validateErr
			fallbackPreview = false
		} else if validated, validateErr := validatedPreviewEntry(entry); validateErr != nil {
			err = withReason(previewValidationReasonCode(validateErr), ErrPreviewRejected, "")
			fallbackPreview = false
		} else {
			preview = validated
		}
	}

	if ctx.Err() != nil || errors.Is(err, runtime.ErrCleanupPending) {
		if analysisRequest && input.analysisFix != nil {
			_ = s.previewStore.revoke(input.owner, idempotentPreviewToken(input.owner, input.analysisFix.PreviewRequestHash))
		}
		reason := "draft runtime cleanup did not complete"
		if ctx.Err() != nil {
			reason = "draft generation timed out"
		}
		reasonCode := ReasonGenerationFailed
		failure := analysisFixFailureView(err)
		if ctx.Err() != nil {
			failure = &AnalysisFixFailureView{Category: AnalysisFixFailureTimedOut, TerminalState: runtime.TerminalTimedOut}
		}
		logGenerationFailure(id, reasonCode, failure, err)
		_, transitionErr := s.transitionToCleanupWithFailure(id, RequestFailed, reason, reasonCode, failure)
		if transitionErr == nil {
			needsCleanup = true
		} else {
			log.Printf("action request %s: persist timeout cleanup: %v", id, transitionErr)
		}
		return
	}

	s.rmu.Lock()
	request = s.requests.Requests[id]
	if request == nil || request.Status != RequestPending {
		needsCleanup = request != nil && request.Status == RequestCancelling
		s.rmu.Unlock()
		if analysisRequest && input.analysisFix != nil {
			_ = s.previewStore.revoke(input.owner, idempotentPreviewToken(input.owner, input.analysisFix.PreviewRequestHash))
		}
		return
	}
	now := time.Now().UTC()
	request.UpdatedAt = now.Format(time.RFC3339)
	request.BaseIssue = nil
	request.BaseTargetRepo = ""
	request.BasePatternHash = ""
	request.AnalysisFix = nil
	if err != nil {
		request.Status = RequestFailed
		request.ReasonCode = ReasonCodeOf(err)
		if analysisRequest {
			request.Failure = analysisFixFailureView(err)
		} else {
			request.Failure = nil
		}
		logGenerationFailure(id, request.ReasonCode, request.Failure, err)
		if fallbackPreview {
			request.Warning = draftRefinementWarning
			request.Preview = &preview
		} else {
			request.Error = generationFailureMessage(analysisRequest, request.ReasonCode, request.Failure, err)
		}
	} else {
		request.Status = RequestReady
		request.ReasonCode = ""
		request.Failure = nil
		request.Preview = &preview
		if analysisRequest {
			request.ExpiresAt = now.Add(previewTTL).Format(time.RFC3339)
			request.Instruction = ""
		} else {
			request.TargetRepo = entry.targetRepo
			request.TargetConfig = entry.targetConfig
			request.VerificationVersion = entry.verificationVersion
			request.PatternHash = entry.patternHash
			if entry.kind == "issue" {
				spec := entry.spec
				request.Issue = &spec
			} else {
				request.Fix = entry.fix.Snapshot()
			}
		}
	}
	saveErr := s.saveRequestsLocked()
	view := request.ActionRequestView
	notifier := s.requestNotify
	if analysisRequest {
		notifier = nil
	}
	s.rmu.Unlock()
	if saveErr != nil {
		log.Printf("action request %s: save result: %v", id, saveErr)
		return
	}
	if err != nil || notifier == nil {
		return
	}
	s.notifyRequestReady(view)
}

func (s *Service) cancelRequestNotificationLocked(id string) {
	if cancel := s.requestNotifyCancels[id]; cancel != nil {
		cancel()
		delete(s.requestNotifyCancels, id)
	}
}

func (s *Service) notifyRequestReady(view ActionRequestView) {
	s.rmu.Lock()
	changed := s.expireRequestsLocked(time.Now().UTC())
	if changed {
		if err := s.saveRequestsLocked(); err != nil {
			log.Printf("Warning: failed to save expired action requests: %v", err)
		}
	}
	current := s.requests.Requests[view.ID]
	if current == nil || current.Status != RequestReady || current.EmailSent {
		s.rmu.Unlock()
		return
	}
	view = current.ActionRequestView
	notifier := s.requestNotify
	notifyCtx, notifyCancel := context.WithCancel(context.Background())
	if s.requestNotifyCancels == nil {
		s.requestNotifyCancels = map[string]context.CancelFunc{}
	}
	s.cancelRequestNotificationLocked(view.ID)
	s.requestNotifyCancels[view.ID] = notifyCancel
	s.rmu.Unlock()
	defer func() {
		notifyCancel()
		s.rmu.Lock()
		delete(s.requestNotifyCancels, view.ID)
		s.rmu.Unlock()
	}()
	if notifier == nil || s.validateSubjectSnapshot(view.FailureID, view.PatternHash, view.Kind) != nil {
		return
	}
	var notifyErr error
	for attempt := 0; attempt < 3; attempt++ {
		if s.validateSubjectSnapshot(view.FailureID, view.PatternHash, view.Kind) != nil {
			return
		}
		notifyErr = patternstate.WithLock(s.dataDir, func() error {
			if err := s.validateSubjectSnapshot(view.FailureID, view.PatternHash, view.Kind); err != nil {
				return err
			}
			notifyCtx, notifyCancel := context.WithTimeout(notifyCtx, 30*time.Second)
			defer notifyCancel()
			return notifier(notifyCtx, view)
		})
		if notifyErr == nil {
			break
		}
		if attempt < 2 {
			select {
			case <-notifyCtx.Done():
				return
			case <-time.After(time.Duration(1+attempt*2) * time.Second):
			}
		}
	}
	s.rmu.Lock()
	if current := s.requests.Requests[view.ID]; current != nil && current.Status == RequestReady {
		current.EmailSent = notifyErr == nil
		if notifyErr != nil {
			current.EmailError = notifyErr.Error()
		} else {
			current.EmailError = ""
		}
		current.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := s.saveRequestsLocked(); err != nil {
			log.Printf("action request %s: save notification status: %v", view.ID, err)
		}
	}
	s.rmu.Unlock()
}

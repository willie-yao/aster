package actions

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/issues"
	"github.com/willie-yao/aster/backend/internal/patternstate"
	"github.com/willie-yao/aster/backend/internal/redact"
	"github.com/willie-yao/aster/backend/internal/runtime"
)

const (
	defaultRequestTimeout        = 10 * time.Minute
	actionRequestTTL             = 24 * time.Hour
	maxActiveRequests            = 50
	maxPendingPerOwner           = 3
	defaultRuntimeCleanupTimeout = 30 * time.Second
)

// Action request states.
const (
	RequestPending    = "pending"
	RequestCancelling = "cancelling"
	RequestReady      = "ready"
	RequestUnknown    = "unknown"
	RequestFailed     = "failed"
	RequestConfirmed  = "confirmed"
	RequestCancelled  = "cancelled"
	RequestExpired    = "expired"
)

const (
	RequestStageVerifying     = "verifying_remediation"
	RequestStageDrafting      = "drafting"
	requestKindAnalysisFix    = "analysis-fix"
	actionRequestStateVersion = 7
)

const draftRefinementWarning = "The revised draft could not be generated or did not pass safety validation. The safe fallback draft is shown below, but this replacement request cannot be confirmed."

var ErrRequestNotFound = errors.New("action request not found")

// RequestReadyNotifier sends a draft-ready notification after async generation.
type RequestReadyNotifier func(context.Context, ActionRequestView) error

// ActionVerificationView reports the deterministic pinned-source preflight.
type ActionVerificationView struct {
	State  string     `json:"state"`
	Code   ReasonCode `json:"code,omitempty"`
	Reason string     `json:"reason"`
}

// ActionRequestView is the API-safe representation of a persisted request.
type ActionRequestView struct {
	ID           string                  `json:"id"`
	FailureID    string                  `json:"failure_id"`
	PatternHash  string                  `json:"pattern_hash,omitempty"`
	Kind         string                  `json:"kind"`
	Owner        string                  `json:"owner"`
	Status       string                  `json:"status"`
	Stage        string                  `json:"stage,omitempty"`
	Verification *ActionVerificationView `json:"verification,omitempty"`
	CreatedAt    string                  `json:"created_at"`
	UpdatedAt    string                  `json:"updated_at"`
	ExpiresAt    string                  `json:"expires_at"`
	Error        string                  `json:"error,omitempty"`
	ReasonCode   ReasonCode              `json:"reason_code,omitempty"`
	Warning      string                  `json:"warning,omitempty"`
	Failure      *AnalysisFixFailureView `json:"failure,omitempty"`
	ResultURL    string                  `json:"result_url,omitempty"`
	SupersededBy string                  `json:"superseded_by,omitempty"`
	Preview      *PreviewResult          `json:"preview,omitempty"`
	EmailSent    bool                    `json:"email_sent,omitempty"`
}

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

// Wait waits for active generation and cleanup goroutines.
func (s *Service) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.requestWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ConfigureAsyncRequests sets the generation timeout and draft-ready notifier.
func (s *Service) ConfigureAsyncRequests(timeout time.Duration, notifier RequestReadyNotifier) {
	s.ConfigureAsyncRequestsWithContext(context.Background(), timeout, notifier)
}

// ConfigureAsyncRequestsWithContext also stops active requests during shutdown.
func (s *Service) ConfigureAsyncRequestsWithContext(ctx context.Context, timeout time.Duration, notifier RequestReadyNotifier) {
	if timeout > 0 {
		s.requestTimeout = timeout
	}
	s.requestNotify = notifier
	s.rmu.Lock()
	s.requestsConfigured = true
	changed := s.expireRequestsLocked(time.Now().UTC())
	var pending []ActionRequestView
	var cleanupIDs []string
	for id, request := range s.requests.Requests {
		if request.Status == RequestReady && request.Kind != requestKindAnalysisFix && !request.EmailSent && notifier != nil {
			pending = append(pending, request.ActionRequestView)
		}
		if request.Status == RequestCancelling {
			cleanupIDs = append(cleanupIDs, id)
		}
	}
	if changed {
		if err := s.saveRequestsLocked(); err != nil {
			log.Printf("Warning: failed to save expired action requests: %v", err)
		}
	}
	s.rmu.Unlock()
	for _, request := range pending {
		go s.notifyRequestReady(request)
	}
	for _, id := range cleanupIDs {
		s.startCleanup(id)
	}
	if ctx != nil && ctx.Done() != nil {
		s.requestWG.Add(1)
		go func() {
			defer s.requestWG.Done()
			<-ctx.Done()
			s.stopActiveRequests()
		}()
	}
}

// CreateRequest persists a pending request and starts draft generation.
func (s *Service) CreateRequest(failureID, kind, owner, userToken, instruction, supersedesID string) (ActionRequestView, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if owner == "" || userToken == "" {
		return ActionRequestView{}, fmt.Errorf("authenticated owner and token are required")
	}
	if kind != "create-issue" && kind != "propose-fix" {
		return ActionRequestView{}, fmt.Errorf("unsupported action %q", kind)
	}
	if kind == "propose-fix" {
		if err := s.requireFixActions(); err != nil {
			return ActionRequestView{}, err
		}
	}
	subject, err := s.resolveSubject(failureID)
	if err != nil {
		return ActionRequestView{}, err
	}
	if code, reason := subjectEligibilityReason(subject); code != "" {
		return ActionRequestView{}, reasonErrorForCode(code, reason)
	}

	id, err := newToken()
	if err != nil {
		return ActionRequestView{}, fmt.Errorf("creating action request id: %w", err)
	}
	now := time.Now().UTC()
	request := &actionRequest{ActionRequestView: ActionRequestView{
		ID: id, FailureID: failureID, Kind: kind, Owner: owner,
		Status: RequestPending, Stage: RequestStageVerifying, CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(actionRequestTTL).Format(time.RFC3339),
	}, Instruction: strings.TrimSpace(instruction)}
	supersedesID = strings.TrimSpace(supersedesID)

	s.rmu.Lock()
	if s.stopping {
		s.rmu.Unlock()
		return ActionRequestView{}, fmt.Errorf("action service is stopping")
	}
	s.expireRequestsLocked(now)
	var superseded *actionRequest
	var supersededStatus, supersededUpdatedAt, supersededBy string
	var supersededCleanup *actionCleanupState
	var supersededCancel context.CancelFunc
	var supersededNotifyCancel context.CancelFunc
	if supersedesID != "" {
		superseded = s.requests.Requests[supersedesID]
		if superseded == nil || superseded.Owner != owner {
			s.rmu.Unlock()
			return ActionRequestView{}, ErrRequestNotFound
		}
		if superseded.FailureID != failureID {
			s.rmu.Unlock()
			return ActionRequestView{}, fmt.Errorf("superseded action request does not match failure")
		}
		if _, confirming := s.requestConfirms[supersedesID]; confirming {
			s.rmu.Unlock()
			return ActionRequestView{}, fmt.Errorf("action request is being confirmed")
		}
		if superseded.Status != RequestPending && superseded.Status != RequestReady {
			status := superseded.Status
			s.rmu.Unlock()
			return ActionRequestView{}, fmt.Errorf("action request is %s", status)
		}
	}
	if supersedesID == "" {
		for _, existing := range s.requests.Requests {
			if existing.Owner != owner || existing.FailureID != failureID || existing.Kind != kind || existing.Instruction != request.Instruction {
				continue
			}
			if existing.Status == RequestReady && !s.readyRequestMatchesCurrent(existing, subject) {
				continue
			}
			if existing.Status == RequestPending || existing.Status == RequestReady || existing.Status == RequestCancelling || existing.Status == RequestUnknown {
				view := existing.ActionRequestView
				s.rmu.Unlock()
				return view, nil
			}
		}
	}
	for existingID, existing := range s.requests.Requests {
		if existingID == supersedesID || existing.Status != RequestUnknown || existing.FailureID != failureID || existing.Kind != kind {
			continue
		}
		if existing.Owner == owner {
			view := existing.ActionRequestView
			s.rmu.Unlock()
			return view, nil
		}
		s.rmu.Unlock()
		return ActionRequestView{}, fmt.Errorf("an existing action for this failure has an unknown GitHub outcome")
	}
	pending := 0
	active := 0
	for existingID, existing := range s.requests.Requests {
		if existingID == supersedesID {
			continue
		}
		if (existing.Status == RequestPending || existing.Status == RequestCancelling) && existing.Owner == owner {
			pending++
		}
		if existing.Status == RequestPending || existing.Status == RequestCancelling || existing.Status == RequestReady || existing.Status == RequestUnknown {
			active++
		}
	}
	if pending >= maxPendingPerOwner {
		s.rmu.Unlock()
		return ActionRequestView{}, fmt.Errorf("too many pending action requests")
	}
	if active >= maxActiveRequests {
		s.rmu.Unlock()
		return ActionRequestView{}, fmt.Errorf("too many active action requests")
	}
	if superseded != nil {
		if request.Instruction != "" && kind == "create-issue" && superseded.Kind == kind && superseded.Status == RequestReady && superseded.Issue != nil {
			if superseded.PatternHash == "" || superseded.PatternHash != subject.ContentHash {
				s.rmu.Unlock()
				return ActionRequestView{}, ErrPreviewTargetChanged
			}
			base := *superseded.Issue
			base.Labels = slices.Clone(base.Labels)
			request.BaseIssue = &base
			request.BaseTargetRepo = superseded.TargetRepo
			request.BasePatternHash = superseded.PatternHash
		}
		supersededStatus = superseded.Status
		supersededUpdatedAt = superseded.UpdatedAt
		supersededBy = superseded.SupersededBy
		supersededCleanup = superseded.Cleanup
		superseded.Status = RequestCancelling
		superseded.Cleanup = &actionCleanupState{FinalStatus: RequestCancelled, Reason: "superseded by a replacement request", RequestedAt: now.Format(time.RFC3339)}
		superseded.UpdatedAt = now.Format(time.RFC3339)
		superseded.SupersededBy = request.ID
		supersededCancel = s.requestCancels[supersedesID]
		supersededNotifyCancel = s.requestNotifyCancels[supersedesID]
	}
	s.requests.Requests[request.ID] = request
	if err := s.saveRequestsLocked(); err != nil {
		delete(s.requests.Requests, request.ID)
		if superseded != nil {
			superseded.Status = supersededStatus
			superseded.UpdatedAt = supersededUpdatedAt
			superseded.SupersededBy = supersededBy
			superseded.Cleanup = supersededCleanup
		}
		s.rmu.Unlock()
		return ActionRequestView{}, err
	}
	view := request.ActionRequestView
	s.rmu.Unlock()

	if supersededCancel != nil {
		supersededCancel()
	}
	if supersededNotifyCancel != nil {
		supersededNotifyCancel()
	}
	if superseded != nil {
		s.startCleanup(supersedesID)
	}
	s.startGeneration(request.ID, userToken)
	return view, nil
}

// CreateAnalysisFixRequest persists one exact JUnit chat-to-fix preview request
// and starts generation independently of the initiating HTTP request.
func (s *Service) CreateAnalysisFixRequest(input AnalysisFixInput, owner, userToken, instruction string, replacesRequestIDs ...string) (ActionRequestView, error) {
	owner = normalizeActionOwner(owner)
	instruction = strings.TrimSpace(instruction)
	if s.cfg != nil {
		input.Identity.Project = strings.TrimSpace(s.cfg.Name)
	}
	input.Identity = normalizeAnalysisIdentity(input.Identity)
	if owner == "" || strings.TrimSpace(userToken) == "" {
		return ActionRequestView{}, fmt.Errorf("authenticated owner and token are required")
	}
	if len(instruction) > 4096 {
		return ActionRequestView{}, fmt.Errorf("instruction must not exceed 4096 bytes")
	}
	if err := s.requireFixActions(); err != nil {
		return ActionRequestView{}, err
	}
	if err := validateAnalysisFixInput(input); err != nil {
		return ActionRequestView{}, err
	}
	if len(replacesRequestIDs) > 1 {
		return ActionRequestView{}, fmt.Errorf("at most one replacement request id is allowed")
	}
	replacesRequestID := ""
	if len(replacesRequestIDs) == 1 {
		replacesRequestID = strings.TrimSpace(replacesRequestIDs[0])
	}
	replacementHash := analysisFixReplacementHash(input)

	id, err := newToken()
	if err != nil {
		return ActionRequestView{}, fmt.Errorf("creating action request id: %w", err)
	}
	now := time.Now().UTC()
	request := &actionRequest{
		ActionRequestView: ActionRequestView{
			ID: id, FailureID: analysisActionID(input.Identity), Kind: requestKindAnalysisFix, Owner: owner,
			Status: RequestPending, Stage: RequestStageVerifying, CreatedAt: now.Format(time.RFC3339),
			UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(actionRequestTTL).Format(time.RFC3339),
		},
		Instruction:       instruction,
		RequestHash:       input.PreviewRequestHash,
		ReplacementHash:   replacementHash,
		ReplacesRequestID: replacesRequestID,
		AnalysisFix:       cloneAnalysisFixInput(input),
	}

	s.rmu.Lock()
	if s.stopping {
		s.rmu.Unlock()
		return ActionRequestView{}, fmt.Errorf("action service is stopping")
	}
	s.expireRequestsLocked(now)
	for _, existing := range s.requests.Requests {
		if existing == nil || existing.Owner != owner || existing.Kind != requestKindAnalysisFix || existing.RequestHash != input.PreviewRequestHash {
			continue
		}
		if existing.Status == RequestPending || existing.Status == RequestReady || existing.Status == RequestCancelling || existing.Status == RequestUnknown {
			view := existing.ActionRequestView
			s.rmu.Unlock()
			return view, nil
		}
	}
	if replacesRequestID != "" {
		replaced := s.requests.Requests[replacesRequestID]
		if replaced == nil || replaced.Owner != owner || replaced.Kind != requestKindAnalysisFix ||
			replaced.Status != RequestFailed || replaced.ReasonCode != ReasonNoReviewablePatch ||
			replaced.Failure == nil || replaced.Failure.Category != AnalysisFixFailureNoReviewablePatch ||
			replaced.ReplacementHash != replacementHash {
			s.rmu.Unlock()
			return ActionRequestView{}, withReason(ReasonNoReviewablePatch, ErrPreviewRejected, "replacement request does not match a recoverable exact JUnit preview")
		}
		if instruction == "" || instruction == strings.TrimSpace(replaced.Instruction) {
			s.rmu.Unlock()
			return ActionRequestView{}, withReason(ReasonNoReviewablePatch, ErrPreviewRejected, "edit the maintainer instruction before regenerating")
		}
	} else {
		for _, existing := range s.requests.Requests {
			if existing != nil && existing.Owner == owner && existing.Kind == requestKindAnalysisFix &&
				existing.Status == RequestFailed && existing.ReasonCode == ReasonNoReviewablePatch &&
				existing.ReplacementHash == replacementHash {
				s.rmu.Unlock()
				return ActionRequestView{}, withReason(ReasonNoReviewablePatch, ErrPreviewRejected, "use explicit regeneration with changed maintainer feedback")
			}
		}
	}
	pending := 0
	active := 0
	for _, existing := range s.requests.Requests {
		if existing == nil {
			continue
		}
		if (existing.Status == RequestPending || existing.Status == RequestCancelling) && existing.Owner == owner {
			pending++
		}
		if existing.Status == RequestPending || existing.Status == RequestCancelling || existing.Status == RequestReady || existing.Status == RequestUnknown {
			active++
		}
	}
	if pending >= maxPendingPerOwner {
		s.rmu.Unlock()
		return ActionRequestView{}, fmt.Errorf("too many pending action requests")
	}
	if active >= maxActiveRequests {
		s.rmu.Unlock()
		return ActionRequestView{}, fmt.Errorf("too many active action requests")
	}
	s.requests.Requests[request.ID] = request
	if err := s.saveRequestsLocked(); err != nil {
		delete(s.requests.Requests, request.ID)
		s.rmu.Unlock()
		return ActionRequestView{}, err
	}
	view := request.ActionRequestView
	s.rmu.Unlock()

	s.startGeneration(request.ID, userToken)
	return view, nil
}

func cloneAnalysisFixInput(input AnalysisFixInput) *AnalysisFixInput {
	clone := input
	clone.VerifiedSourceFileHashes = cloneStringMap(input.VerifiedSourceFileHashes)
	clone.ArtifactCitations = slices.Clone(input.ArtifactCitations)
	clone.EvidenceWarnings = slices.Clone(input.EvidenceWarnings)
	if input.ProposedRevision != nil {
		revision := *input.ProposedRevision
		clone.ProposedRevision = &revision
	}
	return &clone
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

// GetRequest returns one request only to its owning admin.
func (s *Service) GetRequest(id, owner string) (ActionRequestView, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.expireRequestsLocked(time.Now().UTC()) {
		if err := s.saveRequestsLocked(); err != nil {
			return ActionRequestView{}, err
		}
	}
	request := s.requests.Requests[id]
	if request == nil || request.Owner != strings.ToLower(strings.TrimSpace(owner)) {
		return ActionRequestView{}, ErrRequestNotFound
	}
	return request.ActionRequestView, nil
}

// ConfirmRequest posts the exact persisted draft using the current admin token.
func (s *Service) ConfirmRequest(ctx context.Context, id, owner, userToken string) (string, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	s.rmu.Lock()
	if s.expireRequestsLocked(time.Now().UTC()) {
		if err := s.saveRequestsLocked(); err != nil {
			s.rmu.Unlock()
			return "", err
		}
	}
	request := s.requests.Requests[id]
	if request == nil || request.Owner != owner {
		s.rmu.Unlock()
		return "", ErrRequestNotFound
	}
	if request.Kind == "propose-fix" {
		if err := s.requireFixActions(); err != nil {
			s.rmu.Unlock()
			return "", err
		}
	}
	if request.Status == RequestConfirmed && request.ResultURL != "" {
		url := request.ResultURL
		s.rmu.Unlock()
		return url, nil
	}
	if request.Status != RequestReady && request.Status != RequestUnknown {
		status := request.Status
		s.rmu.Unlock()
		return "", fmt.Errorf("action request is %s", status)
	}
	reconcileOnly := request.Status == RequestUnknown
	if _, confirming := s.requestConfirms[id]; confirming {
		s.rmu.Unlock()
		return "", fmt.Errorf("action request is being confirmed")
	}
	entryKind := ""
	if request.Preview != nil {
		entryKind = request.Preview.Kind
	} else if reconcileOnly {
		switch request.Kind {
		case "create-issue":
			entryKind = "issue"
		case "propose-fix":
			entryKind = gfKind
		}
	}
	if entryKind == "" {
		s.rmu.Unlock()
		return "", fmt.Errorf("action request has no persisted preview")
	}
	entry := &previewEntry{failureID: request.FailureID, patternHash: request.PatternHash, kind: entryKind, targetRepo: request.TargetRepo, targetConfig: request.TargetConfig, verificationVersion: request.VerificationVersion}
	switch entry.kind {
	case "issue":
		if request.Issue == nil {
			s.rmu.Unlock()
			return "", fmt.Errorf("action request has no persisted issue draft")
		}
		entry.spec = *request.Issue
	case gfKind:
		if request.Fix == nil {
			s.rmu.Unlock()
			return "", fmt.Errorf("action request has no persisted fix draft")
		}
		entry.fix = fixpr.RestoreGeneratedFix(request.Fix)
	default:
		s.rmu.Unlock()
		return "", fmt.Errorf("action request has invalid preview kind %q", entry.kind)
	}
	if !reconcileOnly && entry.kind == gfKind && entry.verificationVersion != sourceVerificationVersion {
		s.rmu.Unlock()
		return "", ErrPreviewTargetChanged
	}
	if !reconcileOnly && entry.kind == gfKind && (entry.failureID == "" || entry.patternHash == "") {
		s.rmu.Unlock()
		return "", ErrPreviewTargetChanged
	}
	if !reconcileOnly {
		if _, err := validatedPreviewEntry(entry); err != nil {
			s.rmu.Unlock()
			return "", withReason(previewValidationReasonCode(err), ErrPreviewRejected, "")
		}
	}
	if !reconcileOnly && entry.failureID != "" {
		if err := s.validateSubjectSnapshot(entry.failureID, entry.patternHash, entry.kind); err != nil {
			s.rmu.Unlock()
			return "", err
		}
		request.Status = RequestUnknown
		request.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := s.saveRequestsLocked(); err != nil {
			request.Status = RequestReady
			s.rmu.Unlock()
			return "", err
		}
		s.cancelRequestNotificationLocked(id)
	}
	s.requestConfirms[id] = struct{}{}
	s.rmu.Unlock()
	defer func() {
		s.rmu.Lock()
		delete(s.requestConfirms, id)
		s.rmu.Unlock()
	}()

	var url string
	if reconcileOnly {
		reconciledURL, found, err := s.reconcileEntry(ctx, entry, userToken)
		if err != nil {
			return "", err
		}
		if !found {
			return "", ErrPreviewOutcomeUnknown
		}
		url = reconciledURL
	} else {
		confirmedURL, err := s.confirmEntry(ctx, entry, userToken)
		if errors.Is(err, ErrPreviewOutcomeUnknown) {
			return "", err
		}
		if err != nil {
			s.rmu.Lock()
			if current := s.requests.Requests[id]; current != nil {
				current.Status = RequestReady
				current.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				_ = s.saveRequestsLocked()
			}
			s.rmu.Unlock()
			return "", err
		}
		url = confirmedURL
	}
	s.rmu.Lock()
	if current := s.requests.Requests[id]; current != nil {
		current.Status = RequestConfirmed
		current.ResultURL = url
		current.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := s.saveRequestsLocked(); err != nil {
			s.rmu.Unlock()
			return "", err
		}
	}
	s.rmu.Unlock()
	return url, nil
}

// CancelRequest stops generation and confirms external runtime cleanup.
func (s *Service) CancelRequest(ctx context.Context, id, owner string) (ActionRequestView, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	s.rmu.Lock()
	if s.expireRequestsLocked(time.Now().UTC()) {
		if err := s.saveRequestsLocked(); err != nil {
			s.rmu.Unlock()
			return ActionRequestView{}, err
		}
	}
	request := s.requests.Requests[id]
	if request == nil || request.Owner != owner {
		s.rmu.Unlock()
		return ActionRequestView{}, ErrRequestNotFound
	}
	if _, confirming := s.requestConfirms[id]; confirming {
		s.rmu.Unlock()
		return ActionRequestView{}, fmt.Errorf("action request is being confirmed")
	}
	if request.Status == RequestCancelled || request.Status == RequestCancelling {
		view := request.ActionRequestView
		s.rmu.Unlock()
		if view.Status != RequestCancelling {
			return view, nil
		}
		return s.cleanupCancellingRequest(ctx, id)
	}
	if request.Status != RequestPending && request.Status != RequestReady {
		status := request.Status
		s.rmu.Unlock()
		return ActionRequestView{}, fmt.Errorf("action request is %s", status)
	}
	analysisToken := ""
	requestStatus := request.Status
	if request.Kind == requestKindAnalysisFix && request.RequestHash != "" {
		analysisToken = idempotentPreviewToken(owner, request.RequestHash)
		if request.Preview != nil && strings.TrimSpace(request.Preview.Token) != "" {
			analysisToken = request.Preview.Token
		}
	}
	s.rmu.Unlock()
	if analysisToken != "" && requestStatus == RequestReady {
		if err := s.previewStore.revoke(owner, analysisToken); err != nil {
			return ActionRequestView{}, err
		}
	}
	cancel, err := s.transitionToCleanup(id, RequestCancelled, "")
	if err != nil {
		return ActionRequestView{}, err
	}
	if cancel != nil {
		cancel()
	}
	if analysisToken != "" && requestStatus == RequestPending {
		if err := s.previewStore.revoke(owner, analysisToken); err != nil && !errors.Is(err, ErrPreviewPending) {
			return s.currentRequestView(id), err
		}
	}
	return s.cleanupCancellingRequest(ctx, id)
}

package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/actiondraft"
	"github.com/willie-yao/aster/backend/internal/actionverify"
	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/issues"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/remediationpolicy"
	"github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/statefile"
)

type actionCleanupState struct {
	FinalStatus string                  `json:"final_status"`
	Reason      string                  `json:"reason,omitempty"`
	ReasonCode  ReasonCode              `json:"reason_code,omitempty"`
	Failure     *AnalysisFixFailureView `json:"failure,omitempty"`
	RequestedAt string                  `json:"requested_at"`
}

type actionRequest struct {
	ActionRequestView
	Instruction         string                      `json:"instruction,omitempty"`
	RequestHash         string                      `json:"request_hash,omitempty"`
	ReplacementHash     string                      `json:"replacement_hash,omitempty"`
	ReplacesRequestID   string                      `json:"replaces_request_id,omitempty"`
	AnalysisFix         *AnalysisFixInput           `json:"analysis_fix,omitempty"`
	Issue               *issues.IssueSpec           `json:"issue,omitempty"`
	Fix                 *fixpr.GeneratedFixSnapshot `json:"fix,omitempty"`
	TargetRepo          string                      `json:"target_repo,omitempty"`
	TargetConfig        string                      `json:"target_config,omitempty"`
	VerificationVersion int                         `json:"verification_version,omitempty"`
	BaseIssue           *issues.IssueSpec           `json:"base_issue,omitempty"`
	BaseTargetRepo      string                      `json:"base_target_repo,omitempty"`
	BasePatternHash     string                      `json:"base_pattern_hash,omitempty"`
	Runtime             *runtime.WorkRef            `json:"runtime,omitempty"`
	Cleanup             *actionCleanupState         `json:"cleanup,omitempty"`
	EmailError          string                      `json:"email_error,omitempty"`
}

type actionRequestState struct {
	Version  int                       `json:"version"`
	Requests map[string]*actionRequest `json:"requests"`
}

func (s *Service) requestStatePath() string {
	return filepath.Join(s.dataDir, "action_request_state.json")
}

type actionRequestContextKey struct{}

func withActionRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, actionRequestContextKey{}, id)
}

func actionRequestID(ctx context.Context) string {
	id, _ := ctx.Value(actionRequestContextKey{}).(string)
	return id
}

func (s *Service) setRequestVerification(ctx context.Context, state string, code ReasonCode, reason string) error {
	id := actionRequestID(ctx)
	if id == "" {
		return nil
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	request := s.requests.Requests[id]
	if request == nil || request.Status != RequestPending {
		return nil
	}
	previous, previousUpdatedAt := request.Verification, request.UpdatedAt
	request.Verification = &ActionVerificationView{State: state, Code: code, Reason: reason}
	request.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.saveRequestsLocked(); err != nil {
		request.Verification, request.UpdatedAt = previous, previousUpdatedAt
		return err
	}
	return nil
}

func normalizeRequestReason(request *actionRequest) bool {
	if request == nil {
		return false
	}
	changed := false
	if request.Verification != nil && !validReasonCode(request.Verification.Code) {
		switch request.Verification.State {
		case actionverify.StateAlreadyPresent:
			request.Verification.Code = ReasonAlreadyPresent
		case actionverify.StateInconclusive:
			request.Verification.Code = ReasonSourceVerificationInconclusive
		case actionverify.StateUnresolved:
			request.Verification.Code = ReasonActionable
		}
		changed = request.Verification.Code != ""
	}
	if validReasonCode(request.ReasonCode) || request.Status != RequestFailed {
		return changed
	}
	switch {
	case request.Verification != nil && request.Verification.Code != "":
		request.ReasonCode = request.Verification.Code
	case strings.Contains(request.Error, "pattern changed"):
		request.ReasonCode = ReasonEvidenceUnavailable
	case strings.Contains(request.Error, "requires regeneration"):
		request.ReasonCode = ReasonContractGenerationFailed
	case strings.Contains(request.Error, "safety validation"):
		request.ReasonCode = ReasonUnsafeRemediation
	default:
		request.ReasonCode = ReasonGenerationFailed
	}
	return true
}

func (s *Service) setRequestWarning(ctx context.Context, warnings ...string) error {
	id := actionRequestID(ctx)
	if id == "" || len(warnings) == 0 {
		return nil
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	request := s.requests.Requests[id]
	if request == nil || request.Status != RequestPending {
		return nil
	}
	next := boundedWarningSummary(append([]string{request.Warning}, warnings...)...)
	if next == request.Warning {
		return nil
	}
	previous, previousUpdatedAt := request.Warning, request.UpdatedAt
	request.Warning = next
	request.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.saveRequestsLocked(); err != nil {
		request.Warning, request.UpdatedAt = previous, previousUpdatedAt
		return err
	}
	return nil
}

func boundedWarningSummary(warnings ...string) string {
	const maxWarningBytes = 2048
	seen := map[string]bool{}
	parts := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		warning = strings.Join(strings.Fields(warning), " ")
		if warning == "" || seen[warning] {
			continue
		}
		seen[warning] = true
		parts = append(parts, warning)
	}
	summary := strings.Join(parts, " ")
	if len(summary) <= maxWarningBytes {
		return summary
	}
	limit := maxWarningBytes
	for limit > 0 && limit < len(summary) && summary[limit]&0xc0 == 0x80 {
		limit--
	}
	return strings.TrimSpace(summary[:limit])
}

func (s *Service) setRequestStage(ctx context.Context, stage string) error {
	id := actionRequestID(ctx)
	if id == "" {
		return nil
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	request := s.requests.Requests[id]
	if request == nil || request.Status != RequestPending || request.Stage == stage {
		return nil
	}
	previous, previousUpdatedAt := request.Stage, request.UpdatedAt
	request.Stage = stage
	request.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.saveRequestsLocked(); err != nil {
		request.Stage, request.UpdatedAt = previous, previousUpdatedAt
		return err
	}
	return nil
}

func (s *Service) observeRuntimeWork(id string) runtime.WorkObserver {
	return func(_ context.Context, work runtime.WorkRef) error {
		s.rmu.Lock()
		defer s.rmu.Unlock()
		request := s.requests.Requests[id]
		if request == nil || (request.Status != RequestPending && request.Status != RequestCancelling) {
			return context.Canceled
		}
		copy := work
		previousRuntime, previousUpdatedAt := request.Runtime, request.UpdatedAt
		request.Runtime = &copy
		request.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := s.saveRequestsLocked(); err != nil {
			request.Runtime, request.UpdatedAt = previousRuntime, previousUpdatedAt
			return err
		}
		return nil
	}
}

func (s *Service) loadActionRequests() {
	state := &actionRequestState{Version: actionRequestStateVersion, Requests: map[string]*actionRequest{}}
	data, err := os.ReadFile(s.requestStatePath())
	if err == nil {
		if err := json.Unmarshal(data, state); err != nil {
			log.Printf("Warning: failed to parse action request state: %v", err)
			state = &actionRequestState{Version: actionRequestStateVersion, Requests: map[string]*actionRequest{}}
		}
	}
	migrated := state.Version != actionRequestStateVersion
	if migrated {
		state = &actionRequestState{Version: actionRequestStateVersion, Requests: map[string]*actionRequest{}}
	}
	if state.Requests == nil {
		state.Requests = map[string]*actionRequest{}
	}
	now := time.Now().UTC()
	s.requests = state
	changed := migrated || s.expireRequestsLocked(now)
	nowText := now.Format(time.RFC3339)
	for _, request := range state.Requests {
		if request == nil || request.Status != RequestReady {
			continue
		}
		invalidVersion := request.Kind == "propose-fix" && request.VerificationVersion != sourceVerificationVersion
		missingFixIdentity := request.Kind == "propose-fix" && (request.FailureID == "" || request.PatternHash == "")
		if invalidVersion || missingFixIdentity {
			request.Status = RequestFailed
			request.Error = "saved preview requires regeneration after source verification upgrade"
			request.ReasonCode = ReasonContractGenerationFailed
			request.Preview = nil
			request.AnalysisFix = nil
			request.Issue = nil
			request.Fix = nil
			request.UpdatedAt = nowText
			changed = true
		}
	}
	for _, request := range state.Requests {
		if request.Status != RequestReady && request.Status != RequestUnknown {
			continue
		}
		preview, err := validatedReadyPreview(request)
		if err != nil {
			if request.Status == RequestUnknown {
				if request.Preview != nil {
					request.Preview = nil
					changed = true
				}
				continue
			}
			request.Status = RequestFailed
			request.ReasonCode = previewValidationReasonCode(err)
			request.Error = ReasonMessage(request.ReasonCode)
			request.Warning = ""
			request.Preview = nil
			request.AnalysisFix = nil
			request.Issue = nil
			request.Fix = nil
			request.Instruction = ""
			request.UpdatedAt = nowText
			changed = true
			continue
		}
		if !reflect.DeepEqual(request.Preview, preview) {
			request.Preview = preview
			changed = true
		}
	}
	for _, request := range state.Requests {
		switch request.Status {
		case RequestPending:
			if request.Runtime != nil {
				request.Status = RequestCancelling
				request.Cleanup = &actionCleanupState{FinalStatus: RequestFailed, Reason: "server restarted before draft generation completed", RequestedAt: nowText}
			} else {
				request.Status = RequestFailed
				if request.BaseIssue != nil {
					entry := &previewEntry{kind: "issue", spec: *request.BaseIssue}
					if preview, err := validatedPreviewEntry(entry); err == nil {
						request.Warning = draftRefinementWarning
						request.Preview = &preview
					} else {
						request.Error = "saved fallback draft did not pass current safety validation"
						request.ReasonCode = ReasonUnsafeRemediation
					}
				} else {
					request.Error = "server restarted before draft generation completed"
					request.ReasonCode = ReasonGenerationFailed
				}
			}
			request.BaseIssue = nil
			request.BaseTargetRepo = ""
			request.BasePatternHash = ""
			request.AnalysisFix = nil
			request.UpdatedAt = nowText
			changed = true
		case RequestCancelling:
			if request.Cleanup == nil {
				request.Cleanup = &actionCleanupState{FinalStatus: RequestFailed, Reason: "server restarted during runtime cleanup", RequestedAt: nowText}
				request.UpdatedAt = nowText
				changed = true
			}
		}
	}
	for _, request := range state.Requests {
		if normalizeRequestReason(request) {
			changed = true
		}
	}
	if changed {
		if err := statefile.WritePrivateJSONDurable(s.requestStatePath(), state); err != nil {
			log.Printf("Warning: failed to save recovered action request state: %v", err)
		}
	}
}

func validatedReadyPreview(request *actionRequest) (*PreviewResult, error) {
	entry := &previewEntry{kind: request.Kind}
	switch request.Kind {
	case "create-issue":
		if request.Issue == nil {
			return nil, fmt.Errorf("ready issue request has no issue draft")
		}
		entry.kind = "issue"
		entry.spec = *request.Issue
	case "propose-fix":
		if request.Fix == nil {
			return nil, fmt.Errorf("ready fix request has no fix draft")
		}
		entry.kind = gfKind
		entry.fix = fixpr.RestoreGeneratedFix(request.Fix)
	case requestKindAnalysisFix:
		if request.Preview == nil || strings.TrimSpace(request.Preview.Token) == "" {
			return nil, fmt.Errorf("ready analysis fix request has no preview token")
		}
		preview, err := validatedAnalysisFixRequestPreview(*request.Preview)
		if err != nil {
			return nil, err
		}
		return &preview, nil
	default:
		return nil, fmt.Errorf("ready request has unsupported action %q", request.Kind)
	}
	preview, err := validatedPreviewEntry(entry)
	if err != nil {
		return nil, err
	}
	return &preview, nil
}

func validatedAnalysisFixRequestPreview(preview PreviewResult) (PreviewResult, error) {
	if strings.TrimSpace(preview.Token) == "" || preview.Kind != gfKind || strings.TrimSpace(preview.Diff) == "" {
		return PreviewResult{}, fmt.Errorf("analysis fix request has an invalid fix preview")
	}
	if err := actiondraft.ValidateTitleBody(preview.Title, preview.Body); err != nil {
		return PreviewResult{}, err
	}
	return preview, nil
}

func validatedPreviewEntry(entry *previewEntry) (PreviewResult, error) {
	if entry == nil {
		return PreviewResult{}, fmt.Errorf("preview entry is missing")
	}
	switch entry.kind {
	case "issue":
		if strings.TrimSpace(entry.spec.Key) == "" {
			return PreviewResult{}, fmt.Errorf("issue preview key is missing")
		}
		body := strings.ReplaceAll(entry.spec.Body, issues.MarkerFor(entry.spec.Key), "")
		if err := actiondraft.ValidateTitleBody(entry.spec.Title, body); err != nil {
			return PreviewResult{}, err
		}
		return PreviewResult{Kind: "issue", Title: entry.spec.Title, Body: entry.spec.Body}, nil
	case gfKind:
		if entry.fix == nil {
			return PreviewResult{}, fmt.Errorf("fix preview is missing")
		}
		if err := actiondraft.ValidateTitleBody(entry.fix.Title, entry.fix.Description); err != nil {
			return PreviewResult{}, err
		}
		if err := entry.fix.ValidateExecutionVerification(); err != nil {
			return PreviewResult{}, err
		}
		snapshot := entry.fix.Snapshot()
		policyParts := []string{
			snapshot.Pattern.SuggestedFix, snapshot.Pattern.SharedRootCause, snapshot.Pattern.Summary,
			entry.fix.Title, entry.fix.Description,
		}
		if entry.analysisBinding != nil {
			policyParts = append(policyParts, entry.fix.Preview.Diff)
		}
		policyText := strings.Join(policyParts, "\n")
		unsafe := remediationpolicy.Reason(policyText, snapshot.Pattern.RemediationTargets) != ""
		if entry.analysisBinding != nil {
			unsafe = remediationpolicy.RelationshipTextWarning(entry.fix.Preview.Diff) != ""
		}
		if unsafe {
			return PreviewResult{}, withReason(ReasonUnsafeRemediation, ErrPreviewRejected, "")
		}
		return PreviewResult{
			Kind: gfKind, Title: entry.fix.Title, Body: entry.fix.Description, Diff: entry.fix.Preview.Diff,
			VerifyStatus: string(entry.fix.Preview.Verify.Status), VerifySummary: entry.fix.Preview.Verify.Summary, VerifyOutput: entry.fix.Preview.Verify.Output,
		}, nil
	default:
		return PreviewResult{}, fmt.Errorf("preview kind %q is unsupported", entry.kind)
	}
}

func (s *Service) readyRequestMatchesCurrent(request *actionRequest, subject *ActionSubject) bool {
	if request == nil || subject == nil || request.PatternHash == "" || request.PatternHash != subject.ContentHash {
		return false
	}
	switch request.Kind {
	case "create-issue":
		eff := s.cfg.EffectiveIssues()
		return eff.Repo != nil && request.TargetRepo == eff.Repo.Owner+"/"+eff.Repo.Name
	case "propose-fix":
		eff := s.cfg.EffectiveFixPRs()
		var destination project.FixDestination
		var err error
		if subject.Kind == actionSubjectPattern && subject.Pattern != nil {
			destination, _, err = s.fixDestinationForPattern(*subject.Pattern)
		} else {
			destination, err = s.cfg.ResolveFixDestination("", "")
		}
		return err == nil && request.Fix != nil && request.TargetRepo == destination.Repo.Owner+"/"+destination.Repo.Name &&
			request.TargetConfig == fixDestinationFingerprint(eff, destination) && s.validateFixFiles(destination, request.Fix.Files) == nil
	default:
		return false
	}
}

func (s *Service) expireRequestsLocked(now time.Time) bool {
	changed := false
	for id, request := range s.requests.Requests {
		if _, confirming := s.requestConfirms[id]; confirming {
			continue
		}
		expires, err := time.Parse(time.RFC3339, request.ExpiresAt)
		if err != nil || !now.After(expires) {
			continue
		}
		if request.Status == RequestPending || request.Status == RequestCancelling {
			if request.Status == RequestPending {
				request.Status = RequestCancelling
				if cancel := s.requestCancels[id]; cancel != nil {
					cancel()
				}
			}
			request.Cleanup = &actionCleanupState{FinalStatus: RequestExpired, Reason: "action request expired during runtime cleanup", RequestedAt: now.Format(time.RFC3339)}
			request.UpdatedAt = now.Format(time.RFC3339)
			changed = true
			if s.requestsConfigured && s.registerCleanupLocked(id) {
				s.launchCleanup(id)
			}
			continue
		}
		if request.Status != RequestExpired {
			s.cancelRequestNotificationLocked(id)
			request.Status = RequestExpired
			request.UpdatedAt = now.Format(time.RFC3339)
			changed = true
		}
		if request.Error != "" || request.Warning != "" || request.Failure != nil || request.Preview != nil || request.Instruction != "" || request.AnalysisFix != nil || request.Issue != nil || request.BaseIssue != nil || request.BaseTargetRepo != "" || request.BasePatternHash != "" || request.Runtime != nil || request.Cleanup != nil || request.Fix != nil || request.EmailError != "" {
			request.Error = ""
			request.Warning = ""
			request.Failure = nil
			request.Preview = nil
			request.Instruction = ""
			request.AnalysisFix = nil
			request.Issue = nil
			request.BaseIssue = nil
			request.BaseTargetRepo = ""
			request.BasePatternHash = ""
			request.Runtime = nil
			request.Cleanup = nil
			request.Fix = nil
			request.EmailError = ""
			changed = true
		}
	}
	if len(s.requests.Requests) <= 200 {
		return changed
	}
	type item struct{ id, updated string }
	var completed []item
	for id, request := range s.requests.Requests {
		if request.Status != RequestPending && request.Status != RequestReady && request.Status != RequestCancelling {
			completed = append(completed, item{id, request.UpdatedAt})
		}
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].updated < completed[j].updated })
	for len(s.requests.Requests) > 200 && len(completed) > 0 {
		delete(s.requests.Requests, completed[0].id)
		completed = completed[1:]
		changed = true
	}
	return changed
}

func (s *Service) validateSubjectSnapshot(failureID, patternHash string, kind ...string) error {
	subject, err := s.resolveSubject(failureID)
	if err != nil {
		return err
	}
	if patternHash == "" || subject.ContentHash != patternHash {
		return ErrPreviewTargetChanged
	}
	fix := len(kind) > 0 && (kind[0] == gfKind || kind[0] == "propose-fix")
	if fix && subject.Kind == actionSubjectBuild {
		eff := s.cfg.EffectiveFixPRs()
		if eff.Repo == nil || len(verifiedBuildSourceFiles(subject.Build, eff.Repo.Owner, eff.Repo.Name)) == 0 {
			return ErrPreviewTargetChanged
		}
	}
	return nil
}

func (s *Service) saveRequestsLocked() error {
	write := s.requestStateWriter
	if write == nil {
		write = statefile.WritePrivateJSONDurable
	}
	if err := write(s.requestStatePath(), s.requests); err != nil {
		return fmt.Errorf("saving action request state: %w", err)
	}
	return nil
}

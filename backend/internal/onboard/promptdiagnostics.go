package onboard

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
)

type promptPreparationRequest string

const (
	promptRequestTemplate        promptPreparationRequest = "todo-template"
	promptRequestAgent           promptPreparationRequest = "agent"
	promptRequestHandoff         promptPreparationRequest = "handoff"
	promptRequestAPIExperimental promptPreparationRequest = "api-experimental"
)

type promptPreparationStatus string

const (
	promptStatusTemplate      promptPreparationStatus = "todo-template"
	promptStatusAPIDraft      promptPreparationStatus = "api-draft"
	promptStatusFallback      promptPreparationStatus = "api-fallback"
	promptStatusAgentDraft    promptPreparationStatus = "agent-draft"
	promptStatusAgentFallback promptPreparationStatus = "agent-fallback"
	promptStatusHandoff       promptPreparationStatus = "handoff"
)

type promptOutputKind string

const (
	promptOutputTemplate   promptOutputKind = "todo-template"
	promptOutputAPIDraft   promptOutputKind = "api-draft"
	promptOutputAgentDraft promptOutputKind = "agent-draft"
)

type promptPreparationStage string

const (
	promptStageTokenPreflight        promptPreparationStage = "token-preflight"
	promptStageSourceRevision        promptPreparationStage = "source-revision-resolution"
	promptStageSourceTree            promptPreparationStage = "source-tree-listing"
	promptStageSourceExcerpt         promptPreparationStage = "source-excerpt-retrieval"
	promptStageEvidenceExtraction    promptPreparationStage = "structured-evidence-extraction"
	promptStageEvidenceGrounding     promptPreparationStage = "evidence-grounding-validation"
	promptStageFinalPromptValidation promptPreparationStage = "final-rendering-and-prompt-validation"
)

func (s promptPreparationStage) label() string {
	switch s {
	case promptStageTokenPreflight:
		return "token preflight"
	case promptStageSourceRevision:
		return "source revision resolution"
	case promptStageSourceTree:
		return "source tree listing"
	case promptStageSourceExcerpt:
		return "source excerpt retrieval"
	case promptStageEvidenceExtraction:
		return "structured evidence extraction"
	case promptStageEvidenceGrounding:
		return "evidence grounding validation"
	case promptStageFinalPromptValidation:
		return "final rendering and prompt validation"
	default:
		return "prompt preparation"
	}
}

type promptFailureCategory string

const (
	promptFailureMissingToken        promptFailureCategory = "missing-token"
	promptFailureMissingCoordinates  promptFailureCategory = "missing-provider-coordinates"
	promptFailureSourceUnavailable   promptFailureCategory = "source-unavailable"
	promptFailureNoSourceEvidence    promptFailureCategory = "no-usable-source-evidence"
	promptFailureProviderAuth        promptFailureCategory = "provider-authentication-failed"
	promptFailureProviderRejected    promptFailureCategory = "provider-request-rejected"
	promptFailureProviderRateLimited promptFailureCategory = "provider-rate-limited"
	promptFailureProviderUnavailable promptFailureCategory = "provider-unavailable"
	promptFailureInvalidStructured   promptFailureCategory = "invalid-structured-response"
	promptFailureUngroundedEvidence  promptFailureCategory = "ungrounded-evidence"
	promptFailurePromptValidation    promptFailureCategory = "prompt-validation-failed"
	promptFailureTimedOut            promptFailureCategory = "timed-out"
	promptFailureUnknown             promptFailureCategory = "prompt-preparation-failed"
)

func (c promptFailureCategory) reason() string {
	switch c {
	case promptFailureMissingToken:
		return "AI_TOKEN is not set"
	case promptFailureMissingCoordinates:
		return "the prompt-drafting endpoint or model is not configured"
	case promptFailureSourceUnavailable:
		return "bounded repository evidence could not be retrieved"
	case promptFailureNoSourceEvidence:
		return "no usable bounded source excerpts were found"
	case promptFailureProviderAuth:
		return "the provider rejected prompt-drafting authentication"
	case promptFailureProviderRejected:
		return "the provider rejected the structured prompt request"
	case promptFailureProviderRateLimited:
		return "the provider rate-limited prompt drafting"
	case promptFailureProviderUnavailable:
		return "the provider was unavailable"
	case promptFailureInvalidStructured:
		return "the provider returned no valid structured response"
	case promptFailureUngroundedEvidence:
		return "the structured response failed evidence grounding"
	case promptFailurePromptValidation:
		return "the rendered prompt failed deterministic validation"
	case promptFailureTimedOut:
		return "prompt preparation exceeded its time limit"
	default:
		return "prompt preparation could not complete safely"
	}
}

func (c promptFailureCategory) action() string {
	switch c {
	case promptFailureMissingToken:
		return "Set AI_TOKEN to the bearer token that authenticates prompt drafting, then retry."
	case promptFailureMissingCoordinates:
		return "Set AI_ENDPOINT and AI_MODEL for the reviewed prompt-drafting provider."
	case promptFailureSourceUnavailable:
		return "Verify repository access and the pinned default branch, then retry."
	case promptFailureNoSourceEvidence:
		return "Add diagnostic documentation or source signals, or continue with the TODO template."
	case promptFailureProviderAuth:
		return "Verify AI_TOKEN has access to the reviewed provider."
	case promptFailureProviderRejected:
		return "Verify the reviewed API, endpoint, and model support structured prompt drafting."
	case promptFailureProviderRateLimited:
		return "Wait for the provider limit to recover, then retry the same reviewed provider."
	case promptFailureProviderUnavailable:
		return "Retry the same reviewed provider after it recovers."
	case promptFailureInvalidStructured, promptFailureUngroundedEvidence:
		return "Retry once or continue with the reviewable TODO template."
	case promptFailureTimedOut:
		return "Retry the same reviewed provider or continue with the TODO template."
	case promptFailurePromptValidation:
		return "Continue with the TODO template and inspect the deterministic validation tests."
	default:
		return "Continue with the reviewable TODO template."
	}
}

type promptFailureDebug struct {
	StructuredAttempt string
	HTTPStatus        int
	RetryAfter        string
	RequestID         string
	ValidationCode    string
	ValidationField   string
	Phase             string
}

type promptPreparationFailure struct {
	Stage    promptPreparationStage
	Category promptFailureCategory
	Debug    promptFailureDebug
	cause    error
}

func (f *promptPreparationFailure) Error() string {
	if f == nil {
		return "prompt preparation failed"
	}
	return fmt.Sprintf("%s: %s", f.Stage.label(), f.Category.reason())
}

func (f *promptPreparationFailure) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, f.Error())
}

func (f *promptPreparationFailure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.cause
}

type promptPreparationResult struct {
	Requested promptPreparationRequest
	Status    promptPreparationStatus
	Output    promptOutputKind
	Failure   *promptPreparationFailure
	Handoff   string
}

func newTemplatePromptResult() promptPreparationResult {
	return promptPreparationResult{Requested: promptRequestTemplate, Status: promptStatusTemplate, Output: promptOutputTemplate}
}

func newAPIPromptResult() promptPreparationResult {
	return promptPreparationResult{Requested: promptRequestAPIExperimental, Status: promptStatusAPIDraft, Output: promptOutputAPIDraft}
}

func newAPIFallbackResult(failure *promptPreparationFailure) promptPreparationResult {
	return promptPreparationResult{
		Requested: promptRequestAPIExperimental,
		Status:    promptStatusFallback,
		Output:    promptOutputTemplate,
		Failure:   failure,
	}
}

func (r promptPreparationResult) reviewLabel() string {
	switch r.Status {
	case promptStatusAPIDraft:
		return "Experimental API draft"
	case promptStatusAgentDraft:
		return "OpenCode agent draft"
	case promptStatusAgentFallback, promptStatusHandoff:
		return "Agent handoff bundle with TODO template"
	case promptStatusFallback:
		return "TODO template after experimental API failure"
	default:
		return "TODO template"
	}
}

func (r promptPreparationResult) promptPlan(opts Options) PromptPlan {
	plan := PromptPlan{
		RequestedMode: string(r.Requested),
		FinalStatus:   string(r.Status),
		Output:        string(r.Output),
		Source:        r.reviewLabel(),
	}
	if r.Failure != nil {
		plan.FailureStage = string(r.Failure.Stage)
		plan.FailureCategory = string(r.Failure.Category)
		plan.FailureAction = r.Failure.Category.action()
	}
	if r.Requested == promptRequestAPIExperimental {
		plan.Timeout = effectivePromptDraftTimeout(opts).String()
	}
	if r.Status == promptStatusAPIDraft {
		plan.API = opts.AIAPI
		plan.Endpoint = opts.AIEndpoint
		plan.Model = opts.AIModel
	}
	return plan
}

func validatePromptPlan(plan PromptPlan) error {
	if plan.RequestedMode != string(promptRequestTemplate) && plan.RequestedMode != string(promptRequestAPIExperimental) && plan.RequestedMode != string(promptRequestAgent) && plan.RequestedMode != string(promptRequestHandoff) {
		return fmt.Errorf("onboarding plan prompt request %q is invalid", plan.RequestedMode)
	}
	if plan.RequestedMode == string(promptRequestAPIExperimental) {
		timeout, err := time.ParseDuration(plan.Timeout)
		if err != nil || timeout < minPromptDraftTimeout || timeout > maxPromptDraftTimeout {
			return fmt.Errorf("onboarding plan prompt timeout is invalid")
		}
	} else if plan.Timeout != "" {
		return fmt.Errorf("onboarding plan TODO prompt retained an API timeout")
	}
	switch plan.FinalStatus {
	case string(promptStatusTemplate):
		if plan.RequestedMode != string(promptRequestTemplate) || plan.Output != string(promptOutputTemplate) || plan.Source != "TODO template" {
			return fmt.Errorf("onboarding plan TODO prompt result is inconsistent")
		}
	case string(promptStatusAPIDraft):
		if plan.RequestedMode != string(promptRequestAPIExperimental) || plan.Output != string(promptOutputAPIDraft) || plan.Source != "Experimental API draft" {
			return fmt.Errorf("onboarding plan API prompt result is inconsistent")
		}
		if strings.TrimSpace(plan.API) == "" || strings.TrimSpace(plan.Endpoint) == "" || strings.TrimSpace(plan.Model) == "" {
			return fmt.Errorf("onboarding plan API prompt result is missing provider coordinates")
		}
	case string(promptStatusAgentDraft):
		if plan.RequestedMode != string(promptRequestAgent) || plan.Output != string(promptOutputAgentDraft) || plan.Source != "OpenCode agent draft" {
			return fmt.Errorf("onboarding plan agent prompt result is inconsistent")
		}
	case string(promptStatusHandoff), string(promptStatusAgentFallback):
		if plan.Output != string(promptOutputTemplate) || plan.Source != "Agent handoff bundle with TODO template" {
			return fmt.Errorf("onboarding plan handoff result is inconsistent")
		}
	case string(promptStatusFallback):
		if plan.RequestedMode != string(promptRequestAPIExperimental) || plan.Output != string(promptOutputTemplate) || plan.Source != "TODO template after experimental API failure" {
			return fmt.Errorf("onboarding plan fallback prompt result is inconsistent")
		}
		stage := promptPreparationStage(plan.FailureStage)
		category := promptFailureCategory(plan.FailureCategory)
		if !knownPromptStage(stage) || !knownPromptFailureCategory(category) || plan.FailureAction != category.action() {
			return fmt.Errorf("onboarding plan fallback diagnostics are invalid")
		}
	default:
		return fmt.Errorf("onboarding plan prompt status %q is invalid", plan.FinalStatus)
	}
	if plan.FinalStatus != string(promptStatusAPIDraft) && (plan.API != "" || plan.Endpoint != "" || plan.Model != "") {
		return fmt.Errorf("onboarding plan non-API prompt result retained provider coordinates")
	}
	if plan.FinalStatus != string(promptStatusFallback) && (plan.FailureStage != "" || plan.FailureCategory != "" || plan.FailureAction != "") {
		return fmt.Errorf("onboarding plan successful prompt result retained failure diagnostics")
	}
	return nil
}

func knownPromptStage(stage promptPreparationStage) bool {
	switch stage {
	case promptStageTokenPreflight, promptStageSourceRevision, promptStageSourceTree, promptStageSourceExcerpt,
		promptStageEvidenceExtraction, promptStageEvidenceGrounding, promptStageFinalPromptValidation:
		return true
	default:
		return false
	}
}

func knownPromptFailureCategory(category promptFailureCategory) bool {
	switch category {
	case promptFailureMissingToken, promptFailureMissingCoordinates, promptFailureSourceUnavailable, promptFailureNoSourceEvidence,
		promptFailureProviderAuth, promptFailureProviderRejected, promptFailureProviderRateLimited, promptFailureProviderUnavailable,
		promptFailureInvalidStructured, promptFailureUngroundedEvidence, promptFailurePromptValidation,
		promptFailureTimedOut, promptFailureUnknown:
		return true
	default:
		return false
	}
}

func requestedPromptPreparation(opts Options) promptPreparationRequest {
	if opts.NoPrompt || (opts.AIToken == "" && !opts.RequirePromptDraft) {
		return promptRequestTemplate
	}
	return promptRequestAPIExperimental
}

func isPromptDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

func classifyPromptFailure(stage promptPreparationStage, err error) *promptPreparationFailure {
	failure := &promptPreparationFailure{Stage: stage, Category: promptFailureUnknown, cause: err}
	if err == nil {
		return failure
	}
	if isPromptDeadline(err) {
		failure.Category = promptFailureTimedOut
		return failure
	}
	if metadata, ok := ai.SafeProviderErrorMetadata(err); ok {
		failure.Debug.StructuredAttempt = metadata.StructuredAttempt
		failure.Debug.HTTPStatus = metadata.StatusCode
		failure.Debug.RetryAfter = metadata.RetryAfter
		failure.Debug.RequestID = metadata.RequestID
		switch metadata.StatusCode {
		case 401, 403:
			failure.Category = promptFailureProviderAuth
		case 429:
			failure.Category = promptFailureProviderRateLimited
		case 500, 502, 503, 504:
			failure.Category = promptFailureProviderUnavailable
		case 400, 404, 405, 415, 422:
			failure.Category = promptFailureProviderRejected
		default:
			failure.Category = promptFailureInvalidStructured
		}
		return failure
	}
	failure.Category = promptFailureInvalidStructured
	return failure
}

func writePromptFailure(out io.Writer, title string, failure *promptPreparationFailure, fallback string) {
	if out == nil || failure == nil {
		return
	}
	fmt.Fprintf(out, "[warn] %s\n", title)
	fmt.Fprintf(out, "       stage: %s\n", failure.Stage.label())
	fmt.Fprintf(out, "       reason: %s\n", failure.Category.reason())
	fmt.Fprintf(out, "       fallback: %s\n", fallback)
	if action := failure.Category.action(); action != "" {
		fmt.Fprintf(out, "       action: %s\n", action)
	}
}

type requiredPromptDraftError struct {
	failure *promptPreparationFailure
}

func (e *requiredPromptDraftError) Error() string {
	if e == nil || e.failure == nil {
		return "required experimental API prompt draft was not produced"
	}
	return fmt.Sprintf("required experimental API prompt draft was not produced: %s: %s", e.failure.Stage.label(), e.failure.Category.reason())
}

func (e *requiredPromptDraftError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.failure
}

type promptDebugger struct {
	enabled     bool
	out         io.Writer
	credentials []string
}

func newPromptDebugger(enabled bool, out io.Writer, credentials ...string) promptDebugger {
	return promptDebugger{enabled: enabled, out: out, credentials: credentials}
}

func (d promptDebugger) line(format string, args ...any) {
	if !d.enabled || d.out == nil {
		return
	}
	fmt.Fprintf(d.out, "[debug] prompt "+format+"\n", args...)
}

func (d promptDebugger) stage(stage promptPreparationStage, duration time.Duration) {
	d.line("stage=%s duration=%s", stage, duration.Round(time.Millisecond))
}

func (d promptDebugger) provider(opts Options) {
	d.line("api=%s endpoint_host=%s model_fingerprint=%s", d.safeValue(opts.AIAPI), d.safeValue(promptEndpointHost(opts.AIEndpoint)), promptModelFingerprint(opts.AIModel))
}

func (d promptDebugger) source(source promptSource) {
	path := redactPromptText(source.Path, d.credentials...)
	d.line("source_path=%s lines=%d-%d bytes=%d", safeTerminal(path), source.StartLine, source.EndLine, len(source.Text))
}

func (d promptDebugger) sourceSummary(sources []promptSource, jobs int, attempted int) {
	bytes := 0
	for _, source := range sources {
		bytes += len(source.Text)
	}
	d.line("source_count=%d source_bytes=%d source_attempts=%d matched_prow_jobs=%d", len(sources), bytes, attempted, jobs)
}

func (d promptDebugger) extractionChunks(total, completed, attempts int) {
	d.line("extraction_chunks_total=%d extraction_chunks_completed=%d extraction_attempts=%d", total, completed, attempts)
}

func (d promptDebugger) failure(failure *promptPreparationFailure) {
	if failure == nil {
		return
	}
	d.line("failure_stage=%s category=%s", failure.Stage, failure.Category)
	if failure.Debug.StructuredAttempt != "" {
		d.line("structured_transport_attempt=%s", safeTerminal(failure.Debug.StructuredAttempt))
	}
	if failure.Debug.HTTPStatus != 0 {
		d.line("http_status=%d", failure.Debug.HTTPStatus)
	}
	if failure.Debug.RetryAfter != "" {
		d.line("retry_after=%s", d.safeValue(failure.Debug.RetryAfter))
	}
	if failure.Debug.RequestID != "" {
		d.line("provider_request_id=%s", d.safeValue(failure.Debug.RequestID))
	}
	if failure.Debug.ValidationCode != "" {
		d.line("validation_code=%s", safeTerminal(failure.Debug.ValidationCode))
	}
	if failure.Debug.ValidationField != "" {
		d.line("validation_field=%s", safeTerminal(failure.Debug.ValidationField))
	}
	if failure.Debug.Phase != "" {
		d.line("structured_phase=%s", safeTerminal(failure.Debug.Phase))
	}
}

func (d promptDebugger) safeValue(value string) string {
	return safeTerminal(redactPromptText(value, d.credentials...))
}

func (d promptDebugger) total(start time.Time) {
	d.line("total_elapsed=%s", time.Since(start).Round(time.Millisecond))
}

func promptEndpointHost(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "unavailable"
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "unavailable"
	}
	return safeTerminal(host)
}

func promptModelFingerprint(model string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(model)))
	return fmt.Sprintf("sha256:%x", sum[:6])
}

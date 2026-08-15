package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

const (
	defaultStructuredResponseBytes int64 = 128 << 10
	maxStructuredCandidates              = 32
	maxStructuredCandidateStarts         = 4096
)

// ResponseFormat describes one strict JSON Schema response.
type ResponseFormat struct {
	Name        string
	Description string
	Schema      map[string]any
}

// ToolChoice forces one named function call.
type ToolChoice struct {
	Name string
}

// StructuredValidator accepts one complete JSON object. It must decode and
// validate the expected fields before returning nil.
type StructuredValidator func(json.RawMessage) error

// StructuredAttemptPath identifies one structured completion transport path.
type StructuredAttemptPath string

const (
	StructuredAttemptResponseFormat StructuredAttemptPath = "response_format"
	StructuredAttemptForcedFunction StructuredAttemptPath = "forced_function"
	StructuredAttemptPlainFallback  StructuredAttemptPath = "plain_fallback"
)

// StructuredAttemptOutcome is one content-free structured attempt result.
type StructuredAttemptOutcome string

const (
	StructuredOutcomeAccepted              StructuredAttemptOutcome = "accepted"
	StructuredOutcomeProviderError         StructuredAttemptOutcome = "provider_error"
	StructuredOutcomeEmptyResponse         StructuredAttemptOutcome = "empty_response"
	StructuredOutcomeMissingForcedFunction StructuredAttemptOutcome = "missing_forced_function"
	StructuredOutcomeInvalidJSON           StructuredAttemptOutcome = "invalid_json"
	StructuredOutcomeValidatorRejected     StructuredAttemptOutcome = "validator_rejected"
	StructuredOutcomeNoCandidate           StructuredAttemptOutcome = "no_candidate"
)

// StructuredAttemptMetadata contains only bounded control-flow information.
type StructuredAttemptMetadata struct {
	Phase                 string                   `json:"phase,omitempty"`
	Path                  StructuredAttemptPath    `json:"path"`
	Outcome               StructuredAttemptOutcome `json:"outcome"`
	ValidatorCalled       bool                     `json:"validator_called"`
	ValidationCode        string                   `json:"validation_code,omitempty"`
	ProviderCategory      string                   `json:"provider_category,omitempty"`
	ProviderStatus        int                      `json:"provider_status,omitempty"`
	ProviderAttempts      int                      `json:"provider_attempts,omitempty"`
	ProviderAttemptsKnown bool                     `json:"provider_attempts_known,omitempty"`
}

// StructuredCompletionMetadata is the bounded attempt history for one call.
type StructuredCompletionMetadata struct {
	Attempts []StructuredAttemptMetadata `json:"attempts"`
}

// FinalAttempt returns the last attempted structured path.
func (m StructuredCompletionMetadata) FinalAttempt() (StructuredAttemptMetadata, bool) {
	if len(m.Attempts) == 0 {
		return StructuredAttemptMetadata{}, false
	}
	return m.Attempts[len(m.Attempts)-1], true
}

// StructuredCompletionFailureMetadata extracts bounded attempt metadata.
func StructuredCompletionFailureMetadata(err error) (StructuredCompletionMetadata, bool) {
	var structured *structuredCompletionError
	if !errors.As(err, &structured) {
		return StructuredCompletionMetadata{}, false
	}
	return cloneStructuredCompletionMetadata(structured.metadata), true
}

// WithStructuredCompletionPhase attaches one bounded caller phase to attempts.
func WithStructuredCompletionPhase(ctx context.Context, phase string) context.Context {
	phase = structuredPhaseCode(phase)
	if ctx == nil || phase == "" {
		return ctx
	}
	return context.WithValue(ctx, structuredCompletionPhaseContextKey{}, phase)
}

// StructuredCompletionPhase returns the bounded caller phase from context.
func StructuredCompletionPhase(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	phase, _ := ctx.Value(structuredCompletionPhaseContextKey{}).(string)
	return structuredPhaseCode(phase)
}

// CompleteStructured requests a schema-bound object and accepts it only after
// deterministic caller validation. Provider schema support is preferred, then
// a forced function call, then bounded extraction from a plain completion.
func (c *Client) CompleteStructured(ctx context.Context, system, user string, format ResponseFormat, validate StructuredValidator) error {
	_, err := c.CompleteStructuredWithMetadata(ctx, system, user, format, validate)
	return err
}

// CompleteStructuredWithMetadata preserves bounded attempt metadata without
// retaining prompts, response text, tool arguments, or provider bodies.
func (c *Client) CompleteStructuredWithMetadata(ctx context.Context, system, user string, format ResponseFormat, validate StructuredValidator) (StructuredCompletionMetadata, error) {
	if validate == nil {
		return StructuredCompletionMetadata{Attempts: []StructuredAttemptMetadata{}}, fmt.Errorf("structured completion validator is required")
	}
	messages := []modelMessage{
		{Role: "system", Content: strPtr(system)},
		{Role: "user", Content: strPtr(user)},
	}
	result, err := c.completeStructuredMessagesWithMetadata(
		ctx, messages, format, defaultStructuredResponseBytes, true,
		func(raw string) structuredValidationResult { return evaluateStructuredCandidates(raw, validate) },
	)
	return result.Metadata, err
}

type structuredContentValidator func(string) structuredValidationResult

type structuredMessagesResult struct {
	Response *modelResponse
	Metadata StructuredCompletionMetadata
}

func (r structuredMessagesResult) modelCalls() int { return len(r.Metadata.Attempts) }

func (r structuredMessagesResult) providerAttempts() int {
	attempts := 0
	for _, attempt := range r.Metadata.Attempts {
		if attempt.ProviderAttemptsKnown && attempt.ProviderAttempts > 0 {
			attempts += attempt.ProviderAttempts
		} else {
			attempts++
		}
	}
	return attempts
}

func (r structuredMessagesResult) finalPath() StructuredAttemptPath {
	attempt, _ := r.Metadata.FinalAttempt()
	return attempt.Path
}

func (r structuredMessagesResult) httpStatus() int {
	if r.Response != nil && r.Response.HTTPStatus != 0 {
		return r.Response.HTTPStatus
	}
	attempt, _ := r.Metadata.FinalAttempt()
	return attempt.ProviderStatus
}

func (c *Client) completeStructuredMessagesWithMetadata(
	ctx context.Context,
	messages []modelMessage,
	format ResponseFormat,
	maxResponseBytes int64,
	omitReasoning bool,
	validate structuredContentValidator,
) (structuredMessagesResult, error) {
	result := structuredMessagesResult{Metadata: StructuredCompletionMetadata{Attempts: []StructuredAttemptMetadata{}}}
	if validate == nil {
		return result, fmt.Errorf("structured completion validator is required")
	}
	if strings.TrimSpace(format.Name) == "" || len(format.Schema) == 0 {
		return result, fmt.Errorf("structured completion schema is required")
	}
	if maxResponseBytes <= 0 {
		maxResponseBytes = defaultStructuredResponseBytes
	}
	messages = append([]modelMessage(nil), messages...)
	parallel := false
	requests := []modelRequest{
		{
			Model: c.model, Messages: messages, ResponseFormat: &format,
			MaxResponseBytes: maxResponseBytes, OmitReasoning: omitReasoning,
		},
		{
			Model: c.model, Messages: messages,
			Tools: []tools.Schema{{
				Type: "function",
				Function: tools.FunctionDecl{
					Name: format.Name, Description: format.Description,
					Parameters: format.Schema, Strict: true,
				},
			}},
			ToolChoice: &ToolChoice{Name: format.Name}, ParallelToolCalls: &parallel,
			MaxResponseBytes: maxResponseBytes, OmitReasoning: omitReasoning,
		},
		{
			Model: c.model, Messages: messages,
			MaxResponseBytes: maxResponseBytes, OmitReasoning: omitReasoning,
		},
	}

	for index, request := range requests {
		attempt := StructuredAttemptMetadata{Phase: StructuredCompletionPhase(ctx), Path: structuredAttemptPath(index)}
		response, err := c.callModelRequest(ctx, request)
		result.Response = response
		setStructuredProviderAttempts(&attempt, response)
		if response != nil {
			attempt.ProviderStatus = response.HTTPStatus
		}
		if err != nil {
			attempt.Outcome = StructuredOutcomeProviderError
			attempt.ProviderCategory = traceErrorCode(err)
			if provider, ok := SafeProviderErrorMetadata(err); ok {
				attempt.ProviderStatus = provider.StatusCode
				if provider.Category != "" {
					attempt.ProviderCategory = provider.Category
				}
			}
			result.Metadata = appendStructuredAttempt(ctx, result.Metadata, attempt)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return result, err
			}
			if index < len(requests)-1 && structuredFallbackAllowed(err) {
				continue
			}
			return result, newStructuredCompletionError("provider request failed", result.Metadata, err)
		}
		if request.ToolChoice != nil {
			if response == nil || !response.HasMessage || len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Function.Name != format.Name {
				attempt.Outcome = StructuredOutcomeMissingForcedFunction
				result.Metadata = appendStructuredAttempt(ctx, result.Metadata, attempt)
				continue
			}
			raw := response.Message.ToolCalls[0].Function.Arguments
			if strings.TrimSpace(raw) == "" {
				attempt.Outcome = StructuredOutcomeEmptyResponse
				result.Metadata = appendStructuredAttempt(ctx, result.Metadata, attempt)
				continue
			}
			validation := validate(raw)
			attempt.Outcome = validation.outcome
			attempt.ValidatorCalled = validation.validatorCalled
			attempt.ValidationCode = validation.validationCode
			result.Metadata = appendStructuredAttempt(ctx, result.Metadata, attempt)
			if validation.err == nil {
				return result, nil
			}
			continue
		}
		if response == nil || !response.HasMessage || response.Message.Content == nil || strings.TrimSpace(*response.Message.Content) == "" {
			attempt.Outcome = StructuredOutcomeEmptyResponse
			result.Metadata = appendStructuredAttempt(ctx, result.Metadata, attempt)
			continue
		}
		validation := validate(*response.Message.Content)
		attempt.Outcome = validation.outcome
		attempt.ValidatorCalled = validation.validatorCalled
		attempt.ValidationCode = validation.validationCode
		result.Metadata = appendStructuredAttempt(ctx, result.Metadata, attempt)
		if validation.err == nil {
			return result, nil
		}
	}
	return result, newStructuredCompletionError("no valid structured response", result.Metadata, nil)
}

// completeForcedFunction accepts only one call to the exact named function.
func (c *Client) completeForcedFunction(ctx context.Context, system, user string, format ResponseFormat, validate StructuredValidator) error {
	if validate == nil {
		return fmt.Errorf("structured completion validator is required")
	}
	if strings.TrimSpace(format.Name) == "" || len(format.Schema) == 0 {
		return fmt.Errorf("structured completion schema is required")
	}
	parallel := false
	request := modelRequest{
		Model: c.model,
		Messages: []modelMessage{
			{Role: "system", Content: strPtr(system)},
			{Role: "user", Content: strPtr(user)},
		},
		Tools: []tools.Schema{{
			Type: "function",
			Function: tools.FunctionDecl{
				Name: format.Name, Description: format.Description,
				Parameters: format.Schema, Strict: true,
			},
		}},
		ToolChoice: &ToolChoice{Name: format.Name}, ParallelToolCalls: &parallel,
		MaxResponseBytes: defaultStructuredResponseBytes, OmitReasoning: true,
	}
	attempt := StructuredAttemptMetadata{Phase: StructuredCompletionPhase(ctx), Path: StructuredAttemptForcedFunction}
	resp, err := c.callModelRequest(ctx, request)
	setStructuredProviderAttempts(&attempt, resp)
	if err != nil {
		attempt.Outcome = StructuredOutcomeProviderError
		attempt.ProviderCategory = traceErrorCode(err)
		if provider, ok := SafeProviderErrorMetadata(err); ok {
			attempt.ProviderStatus = provider.StatusCode
			if provider.Category != "" {
				attempt.ProviderCategory = provider.Category
			}
		}
		metadata := appendStructuredAttempt(ctx, StructuredCompletionMetadata{}, attempt)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return newStructuredCompletionError("provider request failed", metadata, err)
	}
	if resp == nil || !resp.HasMessage || len(resp.Message.ToolCalls) != 1 ||
		resp.Message.ToolCalls[0].Function.Name != format.Name {
		attempt.Outcome = StructuredOutcomeMissingForcedFunction
		metadata := appendStructuredAttempt(ctx, StructuredCompletionMetadata{}, attempt)
		return newStructuredCompletionError("exact forced function was not returned", metadata, nil)
	}
	raw := json.RawMessage(resp.Message.ToolCalls[0].Function.Arguments)
	if !json.Valid(raw) {
		attempt.Outcome = StructuredOutcomeInvalidJSON
		metadata := appendStructuredAttempt(ctx, StructuredCompletionMetadata{}, attempt)
		return newStructuredCompletionError("forced function arguments failed validation", metadata, nil)
	}
	attempt.ValidatorCalled = true
	if err := validate(raw); err != nil {
		attempt.Outcome = StructuredOutcomeValidatorRejected
		attempt.ValidationCode = structuredValidationCode(err)
		metadata := appendStructuredAttempt(ctx, StructuredCompletionMetadata{}, attempt)
		return newStructuredCompletionError("forced function arguments failed validation", metadata, nil)
	}
	attempt.Outcome = StructuredOutcomeAccepted
	appendStructuredAttempt(ctx, StructuredCompletionMetadata{}, attempt)
	return nil
}

func structuredFallbackAllowed(err error) bool {
	var httpErr *modelHTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	switch httpErr.StatusCode {
	case 400, 404, 405, 415, 422:
		return true
	default:
		return false
	}
}

type structuredCompletionError struct {
	reason   string
	metadata StructuredCompletionMetadata
	provider ProviderErrorMetadata
	cause    error
}

func (e *structuredCompletionError) Error() string {
	return fmt.Sprintf("structured completion rejected: %s", e.reason)
}

func (e *structuredCompletionError) Unwrap() error { return e.cause }

type structuredProviderFailure struct{ category string }

func (e *structuredProviderFailure) Error() string {
	if e.category == "" {
		return "structured provider failure"
	}
	return "structured provider failure: " + e.category
}

func newStructuredCompletionError(reason string, metadata StructuredCompletionMetadata, cause error) error {
	provider, _ := safeProviderErrorMetadataFromCause(cause)
	provider = ProviderErrorMetadata{Category: provider.Category, StatusCode: provider.StatusCode}
	var safeCause error
	if cause != nil {
		safeCause = &structuredProviderFailure{category: provider.Category}
	}
	return &structuredCompletionError{reason: reason, metadata: cloneStructuredCompletionMetadata(metadata), provider: provider, cause: safeCause}
}

func structuredFailureAt(reason, attempt string, cause error) error {
	outcome := StructuredOutcomeNoCandidate
	if cause != nil {
		outcome = StructuredOutcomeProviderError
	}
	item := StructuredAttemptMetadata{Path: structuredAttemptPathName(attempt), Outcome: outcome}
	if cause != nil {
		item.ProviderCategory = traceErrorCode(cause)
		if provider, ok := SafeProviderErrorMetadata(cause); ok {
			item.ProviderStatus = provider.StatusCode
			if provider.Category != "" {
				item.ProviderCategory = provider.Category
			}
		}
	}
	return newStructuredCompletionError(reason, StructuredCompletionMetadata{Attempts: []StructuredAttemptMetadata{item}}, cause)
}

func structuredAttemptPath(index int) StructuredAttemptPath {
	switch index {
	case 0:
		return StructuredAttemptResponseFormat
	case 1:
		return StructuredAttemptForcedFunction
	default:
		return StructuredAttemptPlainFallback
	}
}

func structuredAttemptPathName(value string) StructuredAttemptPath {
	switch value {
	case "json-schema", string(StructuredAttemptResponseFormat):
		return StructuredAttemptResponseFormat
	case "forced-function", string(StructuredAttemptForcedFunction):
		return StructuredAttemptForcedFunction
	default:
		return StructuredAttemptPlainFallback
	}
}

type structuredValidationResult struct {
	outcome         StructuredAttemptOutcome
	validatorCalled bool
	validationCode  string
	err             error
}

func evaluateStructuredCandidates(raw string, validate StructuredValidator) structuredValidationResult {
	tracker := &structuredValidationResult{outcome: StructuredOutcomeNoCandidate}
	if len(raw) > int(defaultStructuredResponseBytes) {
		tracker.err = fmt.Errorf("structured response exceeds %d bytes", defaultStructuredResponseBytes)
		return *tracker
	}
	trimmed := strings.TrimSpace(raw)
	if json.Valid([]byte(trimmed)) {
		tracker.validatorCalled = true
		if err := validate(json.RawMessage(trimmed)); err == nil {
			tracker.outcome = StructuredOutcomeAccepted
			return *tracker
		} else {
			tracker.validationCode = structuredValidationCode(err)
		}
	}
	tracker.err = validateExtractedCandidatesTracked(raw, validate, tracker)
	if tracker.err == nil {
		tracker.outcome = StructuredOutcomeAccepted
		tracker.validationCode = ""
		return *tracker
	}
	if strings.Contains(tracker.err.Error(), "conflicting valid JSON objects") ||
		strings.Contains(tracker.err.Error(), "too many candidate") ||
		strings.Contains(tracker.err.Error(), "too many JSON candidates") ||
		strings.Contains(tracker.err.Error(), "exceeds") {
		tracker.outcome = StructuredOutcomeNoCandidate
		return *tracker
	}
	if tracker.validatorCalled {
		tracker.outcome = StructuredOutcomeValidatorRejected
		return *tracker
	}
	if strings.Contains(tracker.err.Error(), "no valid JSON object") {
		tracker.outcome = StructuredOutcomeInvalidJSON
		return *tracker
	}
	tracker.outcome = StructuredOutcomeNoCandidate
	return *tracker
}

func validateStructuredCandidates(raw string, validate StructuredValidator) error {
	return evaluateStructuredCandidates(raw, validate).err
}

func validateExtractedCandidatesTracked(raw string, validate StructuredValidator, tracker *structuredValidationResult) error {
	data := []byte(raw)
	type acceptedCandidate struct {
		canonical []byte
	}
	var accepted []acceptedCandidate
	seenStarts := 0
	decodedCandidates := 0
	for index, b := range data {
		if b != '{' {
			continue
		}
		seenStarts++
		if seenStarts > maxStructuredCandidateStarts {
			return fmt.Errorf("structured response contains too many candidate starts")
		}
		decoder := json.NewDecoder(bytes.NewReader(data[index:]))
		decoder.UseNumber()
		var candidate json.RawMessage
		if err := decoder.Decode(&candidate); err != nil || len(candidate) == 0 {
			continue
		}
		decodedCandidates++
		if decodedCandidates > maxStructuredCandidates {
			return fmt.Errorf("structured response contains too many JSON candidates")
		}
		tracker.validatorCalled = true
		if err := validate(candidate); err != nil {
			if code := structuredValidationCode(err); code != "" {
				tracker.validationCode = code
			}
			continue
		}
		canonical, err := canonicalJSON(candidate)
		if err != nil {
			continue
		}
		accepted = append(accepted, acceptedCandidate{canonical: canonical})
	}
	if len(accepted) == 0 {
		return fmt.Errorf("structured response contains no valid JSON object")
	}
	last := accepted[len(accepted)-1].canonical
	for _, candidate := range accepted[:len(accepted)-1] {
		if !bytes.Equal(candidate.canonical, last) {
			return fmt.Errorf("structured response contains conflicting valid JSON objects")
		}
	}
	return nil
}

func structuredValidationCode(err error) string {
	if err == nil {
		return ""
	}
	var coded interface{ StructuredValidationCode() string }
	if !errors.As(err, &coded) {
		return ""
	}
	code := strings.ToLower(strings.TrimSpace(coded.StructuredValidationCode()))
	if !traceCodePattern.MatchString(code) {
		return ""
	}
	return code
}

func appendStructuredAttempt(ctx context.Context, metadata StructuredCompletionMetadata, attempt StructuredAttemptMetadata) StructuredCompletionMetadata {
	attempt.Phase = structuredPhaseCode(attempt.Phase)
	attempt.ValidationCode = structuredValidationCodeValue(attempt.ValidationCode)
	attempt.ProviderCategory = structuredProviderCategory(attempt.ProviderCategory)
	metadata.Attempts = append(metadata.Attempts, attempt)
	validatorCalled := attempt.ValidatorCalled
	recordTrace(ctx, TraceEvent{
		Kind: "structured_completion", StructuredPhase: attempt.Phase,
		StructuredAttempt: string(attempt.Path), StructuredOutcome: string(attempt.Outcome),
		ValidatorCalled: &validatorCalled, ValidationCode: attempt.ValidationCode,
		ErrorCode: attempt.ProviderCategory, HTTPStatus: attempt.ProviderStatus, Attempts: attempt.ProviderAttempts,
	})
	return metadata
}

func setStructuredProviderAttempts(attempt *StructuredAttemptMetadata, response *modelResponse) {
	if attempt == nil || response == nil || response.Attempts <= 0 {
		return
	}
	attempt.ProviderAttempts = response.Attempts
	attempt.ProviderAttemptsKnown = true
}

func structuredPhaseCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if traceCodePattern.MatchString(value) {
		return value
	}
	return ""
}

func structuredValidationCodeValue(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if traceCodePattern.MatchString(value) {
		return value
	}
	return ""
}

func structuredProviderCategory(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if traceCodePattern.MatchString(value) {
		return value
	}
	return ""
}

func cloneStructuredCompletionMetadata(metadata StructuredCompletionMetadata) StructuredCompletionMetadata {
	metadata.Attempts = append([]StructuredAttemptMetadata(nil), metadata.Attempts...)
	return metadata
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

type structuredCompletionPhaseContextKey struct{}

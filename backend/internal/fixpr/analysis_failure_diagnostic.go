package fixpr

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/willie-yao/aster/backend/internal/redact"
	"github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/textutil"
)

// AnalysisFailureCategory classifies an exact-JUnit generation failure without output.
type AnalysisFailureCategory string

const (
	AnalysisFailureNoReviewablePatch     AnalysisFailureCategory = "no_reviewable_patch"
	AnalysisFailureRuntimeInfrastructure AnalysisFailureCategory = "runtime_infrastructure"
	AnalysisFailureProviderCredential    AnalysisFailureCategory = "provider_credential"
	AnalysisFailureResultContract        AnalysisFailureCategory = "result_contract"
	AnalysisFailureSafetyIntegrity       AnalysisFailureCategory = "safety_integrity"
	AnalysisFailureSourceChanged         AnalysisFailureCategory = "source_changed"
	AnalysisFailureCancelled             AnalysisFailureCategory = "cancelled"
	AnalysisFailureTimedOut              AnalysisFailureCategory = "timed_out"
)

// AnalysisFailureDetail distinguishes safe outcomes within one failure category.
type AnalysisFailureDetail string

const (
	AnalysisFailureDetailNoRepositoryChange   AnalysisFailureDetail = "no_repository_change"
	AnalysisFailureDetailReviewScopeExceeded  AnalysisFailureDetail = "review_scope_exceeded"
	AnalysisFailureDetailProviderUnauthorized AnalysisFailureDetail = "provider_unauthorized"
	AnalysisFailureDetailProviderForbidden    AnalysisFailureDetail = "provider_forbidden"
)

// AnalysisFailureDiagnostic retains bounded exact-JUnit failure metadata.
type AnalysisFailureDiagnostic struct {
	Category      AnalysisFailureCategory
	Detail        AnalysisFailureDetail
	TerminalState runtime.TerminalState
	// OperatorSummary is a redacted failure explanation for the owning admin.
	OperatorSummary string
	CommandResults  []runtime.CommandResult
	ChangedFiles    []string
}

type analysisGenerationError struct {
	diagnostic AnalysisFailureDiagnostic
	cause      error
}

func (e *analysisGenerationError) Error() string { return e.cause.Error() }
func (e *analysisGenerationError) Unwrap() error { return e.cause }

// AnalysisFailureDiagnosticOf returns the safe exact-JUnit failure classification.
func AnalysisFailureDiagnosticOf(err error) (AnalysisFailureDiagnostic, bool) {
	var generationErr *analysisGenerationError
	if !errors.As(err, &generationErr) {
		return AnalysisFailureDiagnostic{}, false
	}
	return cloneAnalysisFailureDiagnostic(generationErr.diagnostic), true
}

func newAnalysisGenerationError(category AnalysisFailureCategory, agent *AgentConfig, result runtime.ExecutionResult, cause error) error {
	detail := analysisFailureDetail(category, result, cause)
	diagnostic := AnalysisFailureDiagnostic{
		Category: category, Detail: detail, TerminalState: result.TerminalState,
	}
	if detail == AnalysisFailureDetailNoRepositoryChange {
		diagnostic.OperatorSummary = redact.OperatorText(result.StdoutSummary)
	}
	if category == AnalysisFailureProviderCredential {
		diagnostic.OperatorSummary = providerCredentialOperatorSummary(result.ProviderError)
	}
	if agent != nil && agent.RequireCommandResults && runtime.ValidateCommandResults(agent.CommandPolicy.Commands, result.CommandResults) == nil {
		diagnostic.CommandResults = cloneCommandResults(result.CommandResults)
	}
	return &analysisGenerationError{diagnostic: diagnostic, cause: cause}
}

func analysisFailureDetail(category AnalysisFailureCategory, result runtime.ExecutionResult, cause error) AnalysisFailureDetail {
	if category == AnalysisFailureProviderCredential && result.ProviderError != nil {
		switch result.ProviderError.StatusCode {
		case 401:
			return AnalysisFailureDetailProviderUnauthorized
		case 403:
			return AnalysisFailureDetailProviderForbidden
		}
	}
	if category != AnalysisFailureNoReviewablePatch {
		return ""
	}
	if errors.Is(cause, runtime.ErrResultScope) || result.FailureCode == runtime.ExecutionFailureReviewScope || len(result.Files) > 0 {
		return AnalysisFailureDetailReviewScopeExceeded
	}
	if result.TerminalState == runtime.TerminalSucceeded {
		return AnalysisFailureDetailNoRepositoryChange
	}
	return ""
}

const (
	providerOperatorSummaryBytes = 500
	providerMessageSummaryBytes  = 190
	providerConfigSummaryBytes   = 155
)

func providerCredentialOperatorSummary(detail *runtime.ProviderErrorDetail) string {
	if detail == nil {
		return ""
	}

	status := "Provider authentication failed; check credential and authorization."
	switch detail.StatusCode {
	case 401:
		status = "HTTP 401: credential rejected; check it."
	case 403:
		status = "HTTP 403: request refused."
	}
	config := providerConfigSummary(detail, providerConfigSummaryBytes)
	advisory := ""
	if detail.StatusCode == 403 {
		advisory = " Check credential access or provider entitlement, organization policy, quota, and proxy or mesh authorization."
	}
	rest := status + config + advisory + " Provider message: "
	messageBudget := max(providerMessageSummaryBytes, providerOperatorSummaryBytes-len(rest))
	message := boundedOperatorComponent(detail.Message, messageBudget)
	if message == "" {
		message = "unavailable"
	}
	return rest + message
}

func providerConfigSummary(detail *runtime.ProviderErrorDetail, maxBytes int) string {
	endpointScheme, endpointAddress := providerEndpointReference(detail.Endpoint)
	secretName := redact.OperatorText(detail.AuthSecretName)
	secretKey := redact.OperatorText(detail.AuthSecretKey)
	endpointScheme = redact.OperatorText(endpointScheme)
	endpointAddress = redact.OperatorText(endpointAddress)
	model := redact.OperatorText(detail.Model)
	providerID := redact.OperatorText(detail.ProviderID)

	render := func() string {
		config := fmt.Sprintf(" Secret %s/%s; endpoint %s|%s; model %s.",
			secretName, secretKey, endpointScheme, endpointAddress, model)
		if providerID != "" {
			config += " Provider " + providerID + "."
		}
		return config
	}
	shrinkTail := func(value *string, minBytes int) {
		excess := len(render()) - maxBytes
		if excess <= 0 || len(*value) <= minBytes {
			return
		}
		target := max(minBytes, len(*value)-excess)
		*value = truncateOperatorComponent(*value, target)
	}
	shrinkMiddle := func(value *string, minBytes int) {
		excess := len(render()) - maxBytes
		if excess <= 0 || len(*value) <= minBytes {
			return
		}
		target := max(minBytes, len(*value)-excess)
		*value = truncateMiddle(*value, target)
	}

	shrinkTail(&model, 3)
	shrinkTail(&endpointAddress, 3)
	shrinkTail(&endpointScheme, 3)
	shrinkTail(&providerID, 3)
	shrinkMiddle(&secretName, 16)
	shrinkMiddle(&secretKey, 16)
	return render()
}

func boundedOperatorComponent(value string, maxBytes int) string {
	return truncateOperatorComponent(redact.OperatorText(value), maxBytes)
}

func truncateOperatorComponent(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	return textutil.Truncate(value, maxBytes-len("…"))
}

func truncateMiddle(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	const ellipsis = "…"
	available := maxBytes - len(ellipsis)
	prefixBytes := available / 2
	suffixBytes := available - prefixBytes
	for prefixBytes > 0 && !utf8.ValidString(value[:prefixBytes]) {
		prefixBytes--
	}
	suffixStart := len(value) - suffixBytes
	for suffixStart < len(value) && !utf8.RuneStart(value[suffixStart]) {
		suffixStart++
	}
	return value[:prefixBytes] + ellipsis + value[suffixStart:]
}

func providerEndpointReference(endpoint string) (string, string) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", redact.OperatorText(endpoint)
	}
	return parsed.Scheme, parsed.Host + parsed.EscapedPath()
}

func classifyAnalysisRuntimeFailure(result runtime.ExecutionResult, err error) AnalysisFailureCategory {
	switch {
	case errors.Is(err, runtime.ErrResultScope), result.FailureCode == runtime.ExecutionFailureReviewScope:
		return AnalysisFailureNoReviewablePatch
	case errors.Is(err, context.DeadlineExceeded), result.TerminalState == runtime.TerminalTimedOut:
		return AnalysisFailureTimedOut
	case errors.Is(err, context.Canceled), errors.Is(err, runtime.ErrCancelled), result.TerminalState == runtime.TerminalCancelled:
		return AnalysisFailureCancelled
	case result.FailureCode == runtime.ExecutionFailureProviderCredential:
		return AnalysisFailureProviderCredential
	case errors.Is(err, runtime.ErrMalformedResult), errors.Is(err, runtime.ErrResultContract):
		return AnalysisFailureResultContract
	case errors.Is(err, runtime.ErrResultDeletion), errors.Is(err, runtime.ErrResultRename), errors.Is(err, runtime.ErrResultExtraFile), result.FailureCode == runtime.ExecutionFailureSafetyIntegrity:
		return AnalysisFailureSafetyIntegrity
	default:
		return AnalysisFailureRuntimeInfrastructure
	}
}

func cloneAnalysisFailureDiagnostic(in AnalysisFailureDiagnostic) AnalysisFailureDiagnostic {
	return AnalysisFailureDiagnostic{
		Category: in.Category, Detail: in.Detail, TerminalState: in.TerminalState, OperatorSummary: in.OperatorSummary,
		CommandResults: cloneCommandResults(in.CommandResults), ChangedFiles: slices.Clone(in.ChangedFiles),
	}
}

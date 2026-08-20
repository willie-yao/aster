package fixpr

import (
	"context"
	"errors"
	"slices"

	"github.com/willie-yao/aster/backend/internal/runtime"
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
	AnalysisFailureDetailNoRepositoryChange  AnalysisFailureDetail = "no_repository_change"
	AnalysisFailureDetailReviewScopeExceeded AnalysisFailureDetail = "review_scope_exceeded"
)

// AnalysisFailureDiagnostic retains only public-safe exact-JUnit failure metadata.
type AnalysisFailureDiagnostic struct {
	Category       AnalysisFailureCategory
	Detail         AnalysisFailureDetail
	TerminalState  runtime.TerminalState
	CommandResults []runtime.CommandResult
	ChangedFiles   []string
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

func newAnalysisGenerationError(category AnalysisFailureCategory, agent *AgentConfig, result runtime.GenerateResult, cause error) error {
	diagnostic := AnalysisFailureDiagnostic{
		Category: category, Detail: analysisFailureDetail(category, result, cause), TerminalState: result.TerminalState,
	}
	if agent != nil && agent.RequireCommandResults && runtime.ValidateCommandResults(agent.CommandPolicy.Commands, result.CommandResults) == nil {
		diagnostic.CommandResults = cloneCommandResults(result.CommandResults)
	}
	return &analysisGenerationError{diagnostic: diagnostic, cause: cause}
}

func analysisFailureDetail(category AnalysisFailureCategory, result runtime.GenerateResult, cause error) AnalysisFailureDetail {
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

func classifyAnalysisRuntimeFailure(result runtime.GenerateResult, err error) AnalysisFailureCategory {
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
		Category: in.Category, Detail: in.Detail, TerminalState: in.TerminalState,
		CommandResults: cloneCommandResults(in.CommandResults), ChangedFiles: slices.Clone(in.ChangedFiles),
	}
}

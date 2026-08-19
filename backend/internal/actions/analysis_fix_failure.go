package actions

import (
	"context"
	"errors"

	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/runtime"
)

// AnalysisFixFailureCategory is a public-safe exact-JUnit failure class.
type AnalysisFixFailureCategory string

const (
	AnalysisFixFailureNoReviewablePatch     AnalysisFixFailureCategory = "no_reviewable_patch"
	AnalysisFixFailureRuntimeInfrastructure AnalysisFixFailureCategory = "runtime_infrastructure"
	AnalysisFixFailureProviderCredential    AnalysisFixFailureCategory = "provider_credential"
	AnalysisFixFailureResultContract        AnalysisFixFailureCategory = "result_contract"
	AnalysisFixFailureSafetyIntegrity       AnalysisFixFailureCategory = "safety_integrity"
	AnalysisFixFailureSourceChanged         AnalysisFixFailureCategory = "source_changed"
	AnalysisFixFailureCancelled             AnalysisFixFailureCategory = "cancelled"
	AnalysisFixFailureTimedOut              AnalysisFixFailureCategory = "timed_out"
)

// SafeCommandResult omits all command output from a failed request record.
type SafeCommandResult struct {
	Argv       []string `json:"argv"`
	ExitCode   int      `json:"exit_code"`
	DurationMs int64    `json:"duration_ms"`
	TimedOut   bool     `json:"timed_out,omitempty"`
}

// AnalysisFixFailureView reports a bounded exact-JUnit generation failure.
type AnalysisFixFailureView struct {
	Category       AnalysisFixFailureCategory `json:"category"`
	TerminalState  runtime.TerminalState      `json:"terminal_state,omitempty"`
	CommandResults []SafeCommandResult        `json:"command_results,omitempty"`
	ChangedFiles   []string                   `json:"changed_files,omitempty"`
}

type classifiedAnalysisFixError struct {
	failure *AnalysisFixFailureView
	cause   error
}

func (e *classifiedAnalysisFixError) Error() string { return e.cause.Error() }
func (e *classifiedAnalysisFixError) Unwrap() error { return e.cause }

func safeAnalysisFixPreviewError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if diagnostic, ok := fixpr.AnalysisFailureDiagnosticOf(err); ok {
		code := analysisFixReasonCode(diagnostic.Category)
		return withReason(code, err, ReasonMessage(code))
	}
	if errors.Is(err, fixpr.ErrPreviewBaseChanged) {
		return classifiedAnalysisFixFailure(ReasonEvidenceUnavailable, AnalysisFixFailureSourceChanged, "", err)
	}
	if errors.Is(err, runtime.ErrCleanupPending) {
		return classifiedAnalysisFixFailure(ReasonGenerationFailed, AnalysisFixFailureRuntimeInfrastructure, "", err)
	}
	return classifiedAnalysisFixFailure(ReasonGenerationFailed, AnalysisFixFailureRuntimeInfrastructure, "", err)
}

func classifiedAnalysisFixFailure(
	code ReasonCode,
	category AnalysisFixFailureCategory,
	terminalState runtime.TerminalState,
	cause error,
) error {
	return &classifiedAnalysisFixError{
		failure: &AnalysisFixFailureView{Category: category, TerminalState: terminalState},
		cause:   withReason(code, cause, ReasonMessage(code)),
	}
}

func classifiedAnalysisPreviewValidationError(err error) error {
	code := previewValidationReasonCode(err)
	category := AnalysisFixFailureResultContract
	if code == ReasonUnsafeRemediation {
		category = AnalysisFixFailureSafetyIntegrity
	}
	return classifiedAnalysisFixFailure(code, category, "", err)
}

func analysisFixReasonCode(category fixpr.AnalysisFailureCategory) ReasonCode {
	switch category {
	case fixpr.AnalysisFailureNoReviewablePatch:
		return ReasonNoReviewablePatch
	case fixpr.AnalysisFailureResultContract:
		return ReasonContractGenerationFailed
	case fixpr.AnalysisFailureSafetyIntegrity:
		return ReasonUnsafeRemediation
	case fixpr.AnalysisFailureSourceChanged:
		return ReasonEvidenceUnavailable
	case fixpr.AnalysisFailureProviderCredential:
		return ReasonProviderCredentialRejected
	default:
		return ReasonGenerationFailed
	}
}

func analysisFixFailureView(err error) *AnalysisFixFailureView {
	var classified *classifiedAnalysisFixError
	if errors.As(err, &classified) {
		return cloneAnalysisFixFailureView(classified.failure)
	}
	if diagnostic, ok := fixpr.AnalysisFailureDiagnosticOf(err); ok {
		view := &AnalysisFixFailureView{
			Category: AnalysisFixFailureCategory(diagnostic.Category), TerminalState: diagnostic.TerminalState,
			ChangedFiles: append([]string(nil), diagnostic.ChangedFiles...),
		}
		view.CommandResults = make([]SafeCommandResult, len(diagnostic.CommandResults))
		for index, result := range diagnostic.CommandResults {
			view.CommandResults[index] = SafeCommandResult{
				Argv: append([]string(nil), result.Argv...), ExitCode: result.ExitCode,
				DurationMs: result.DurationMs, TimedOut: result.TimedOut,
			}
		}
		return view
	}
	switch {
	case errors.Is(err, fixpr.ErrPreviewBaseChanged), errors.Is(err, ErrPreviewTargetChanged):
		return &AnalysisFixFailureView{Category: AnalysisFixFailureSourceChanged}
	case errors.Is(err, context.DeadlineExceeded):
		return &AnalysisFixFailureView{Category: AnalysisFixFailureTimedOut, TerminalState: runtime.TerminalTimedOut}
	case errors.Is(err, context.Canceled), errors.Is(err, runtime.ErrCancelled):
		return &AnalysisFixFailureView{Category: AnalysisFixFailureCancelled, TerminalState: runtime.TerminalCancelled}
	default:
		return nil
	}
}

func cloneAnalysisFixFailureView(in *AnalysisFixFailureView) *AnalysisFixFailureView {
	if in == nil {
		return nil
	}
	out := &AnalysisFixFailureView{
		Category: in.Category, TerminalState: in.TerminalState,
		ChangedFiles: append([]string(nil), in.ChangedFiles...),
	}
	out.CommandResults = make([]SafeCommandResult, len(in.CommandResults))
	for index, result := range in.CommandResults {
		out.CommandResults[index] = SafeCommandResult{
			Argv: append([]string(nil), result.Argv...), ExitCode: result.ExitCode,
			DurationMs: result.DurationMs, TimedOut: result.TimedOut,
		}
	}
	return out
}

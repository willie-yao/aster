package agentanalysis

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	agentruntime "github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

//go:embed skill/failure-analysis.md
var failureAnalysisSkill string

// maxAgentPromptBytes leaves room for runtime framing under the Linux per-string exec limit.
const maxAgentPromptBytes = 112 << 10

// Spec describes one private experimental Agent analysis.
type Spec struct {
	Repo         agentruntime.RepoRef
	Bundle       EvidenceBundle
	SourceReader sourceinvestigation.Reader
	MaxTurns     int
	Timeout      time.Duration
	ExecutionID  string
}

// Result is one validated private experimental analysis.
type Result struct {
	Status               ShadowStatus
	Analysis             Analysis
	Quality              ShadowQuality
	Runtime              string
	AgentNamespace       string
	AgentRef             string
	AgentVersion         string
	ContractVersion      string
	ToolPolicyVersion    string
	EvidenceHash         string
	SkillHash            string
	SourceSHA            string
	IdentityHash         string
	ExecutionID          string
	MaxTurns             int
	Timeout              time.Duration
	Retries              int
	Attempts             int
	Duration             time.Duration
	FinalizationDuration time.Duration
	Telemetry            agentruntime.GenerateTelemetry
	CleanupPending       bool
	CleanupWork          *agentruntime.WorkRef
}

// Runtime delegates one bounded analysis to a generic AgentRuntime.
type Runtime struct {
	Agent          agentruntime.AgentRuntime
	Name           string
	AgentNamespace string
	AgentRef       string
	AgentVersion   string
	Retries        int
}

// Generate runs the Agent and validates its one-file structured result.
func (r *Runtime) Generate(ctx context.Context, spec Spec) (result Result, retErr error) {
	defer func() {
		result.Status = ResolveShadowStatus(result, retErr)
	}()
	if r == nil || r.Agent == nil {
		return Result{}, fmt.Errorf("agent analysis: runtime is unavailable")
	}
	if err := ValidateEvidenceBundle(spec.Bundle); err != nil {
		return Result{}, err
	}
	if err := r.validateSpec(spec); err != nil {
		return Result{}, err
	}
	identity := r.identityHash(spec)
	executionID := strings.TrimSpace(spec.ExecutionID)
	if executionID == "" {
		executionID = "agent-analysis-" + identity[:16]
	}
	instruction, err := buildInstruction(spec.Bundle)
	if err != nil {
		return Result{}, err
	}
	var observed agentruntime.WorkRef
	runtimeStarted := time.Now()
	generated, runErr := r.Agent.Generate(ctx, agentruntime.GenerateSpec{
		Repo: spec.Repo, Instruction: instruction,
		Skills:   map[string]string{SkillName: failureAnalysisSkill},
		MaxTurns: spec.MaxTurns, AllowBash: false, Timeout: spec.Timeout,
		ExecutionID: executionID,
		WorkObserver: func(_ context.Context, work agentruntime.WorkRef) error {
			observed = work
			return nil
		},
	})
	result = Result{
		Runtime: strings.TrimSpace(r.Name), AgentNamespace: strings.TrimSpace(r.AgentNamespace), AgentRef: strings.TrimSpace(r.AgentRef), AgentVersion: strings.TrimSpace(r.AgentVersion),
		ContractVersion: ContractVersion, ToolPolicyVersion: ToolPolicyVersion,
		EvidenceHash: spec.Bundle.Hash, SkillHash: SkillHash(), SourceSHA: spec.Bundle.Source.Revision,
		IdentityHash: identity, ExecutionID: executionID, MaxTurns: spec.MaxTurns, Timeout: spec.Timeout,
		Retries: r.Retries, Attempts: generated.Attempts, Duration: time.Since(runtimeStarted), Telemetry: generated.Telemetry,
	}
	if result.Runtime == "" {
		result.Runtime = "agent"
	}
	cleanupPending := errors.Is(runErr, agentruntime.ErrCleanupPending)
	result.CleanupPending = cleanupPending
	if cleanupPending && observed.Name != "" {
		work := observed
		result.CleanupWork = &work
	}
	if runErr != nil && !cleanupOnly(runErr) {
		return result, runErr
	}
	finalizationStarted := time.Now()
	if err := validateGeneratedOutput(generated); err != nil {
		result.FinalizationDuration = time.Since(finalizationStarted)
		return result, err
	}
	analysis, err := parseAndValidateAnalysis(ctx, generated.Files[OutputPath], spec.Bundle, spec.SourceReader)
	result.FinalizationDuration = time.Since(finalizationStarted)
	if err != nil {
		if _, ok := shadowStatusFromError(err); !ok {
			err = newShadowResultError(ShadowStatusContractViolation, err)
		}
		return result, err
	}
	result.Analysis = analysis
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

type shadowResultError struct {
	status ShadowStatus
	err    error
}

func (e *shadowResultError) Error() string        { return e.err.Error() }
func (e *shadowResultError) Unwrap() error        { return e.err }
func (e *shadowResultError) Is(target error) bool { return target == ErrInvalidResult }

func newShadowResultError(status ShadowStatus, err error) error {
	return &shadowResultError{status: status, err: err}
}

func shadowStatusFromError(err error) (ShadowStatus, bool) {
	var resultErr *shadowResultError
	if errors.As(err, &resultErr) {
		return resultErr.status, true
	}
	return "", false
}

func cleanupOnly(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !cleanupOnly(child) {
				return false
			}
		}
		return true
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return cleanupOnly(wrapped)
	}
	return err == agentruntime.ErrCleanupPending
}

// ResolveShadowStatus returns the canonical private outcome for one Agent run.
func ResolveShadowStatus(result Result, err error) ShadowStatus {
	if status, ok := shadowStatusFromError(err); ok {
		return status
	}
	switch {
	case err == nil:
		return ShadowStatusSucceeded
	case errors.Is(err, agentruntime.ErrMalformedResult):
		return ShadowStatusMalformedResult
	case errors.Is(err, agentruntime.ErrResultDeletion):
		return ShadowStatusDeletion
	case errors.Is(err, agentruntime.ErrResultRename):
		return ShadowStatusRename
	case errors.Is(err, agentruntime.ErrResultExtraFile):
		return ShadowStatusExtraFile
	case errors.Is(err, agentruntime.ErrResultContract):
		return ShadowStatusContractViolation
	case errors.Is(err, context.DeadlineExceeded):
		return ShadowStatusTimeout
	case errors.Is(err, context.Canceled), errors.Is(err, agentruntime.ErrCancelled):
		return ShadowStatusCancellation
	case errors.Is(err, agentruntime.ErrCleanupPending) && result.Analysis.Summary != "":
		return ShadowStatusCleanupPending
	default:
		return ShadowStatusRuntimeFailed
	}
}

// SkillHash returns the immutable embedded analysis-skill fingerprint.
func SkillHash() string { return hashString(failureAnalysisSkill) }

func (r *Runtime) validateSpec(spec Spec) error {
	if strings.TrimSpace(r.AgentNamespace) == "" || strings.TrimSpace(r.AgentRef) == "" || strings.TrimSpace(r.AgentVersion) == "" {
		return fmt.Errorf("agent analysis: Agent namespace, reference, and declared version are required")
	}
	if r.Retries < 0 || r.Retries > 2 {
		return fmt.Errorf("agent analysis: retries must be between 0 and 2")
	}
	if spec.MaxTurns < 1 || spec.MaxTurns > 1000 {
		return fmt.Errorf("agent analysis: max turns must be between 1 and 1000")
	}
	if spec.Timeout <= 0 || spec.Timeout > 30*time.Minute {
		return fmt.Errorf("agent analysis: timeout must be greater than zero and at most 30m")
	}
	if spec.ExecutionID != "" && (!utf8.ValidString(spec.ExecutionID) || len(spec.ExecutionID) > 128) {
		return fmt.Errorf("agent analysis: execution id is invalid or oversized")
	}
	if spec.Repo.Token != "" || spec.Repo.CloneURL != "" {
		return fmt.Errorf("agent analysis: repository credentials and clone overrides are not accepted")
	}
	if spec.Repo.Owner != spec.Bundle.Source.Owner || spec.Repo.Name != spec.Bundle.Source.Name || spec.Repo.Ref != spec.Bundle.Source.Revision {
		return fmt.Errorf("agent analysis: repository does not match the frozen source identity")
	}
	return nil
}

func (r *Runtime) identityHash(spec Spec) string {
	return r.identityHashWithPolicy(spec, ToolPolicyVersion)
}

func (r *Runtime) identityHashWithPolicy(spec Spec, toolPolicyVersion string) string {
	parts := []string{
		ContractVersion, strings.TrimSpace(toolPolicyVersion), spec.Bundle.Hash, SkillHash(), spec.Bundle.Source.Revision,
		strings.TrimSpace(r.AgentNamespace), strings.TrimSpace(r.AgentRef), strings.TrimSpace(r.AgentVersion),
		spec.Timeout.String(), fmt.Sprintf("%d", spec.MaxTurns), fmt.Sprintf("%d", r.Retries),
	}
	return hashString(strings.Join(parts, "\x00"))
}

func buildInstruction(bundle EvidenceBundle) (string, error) {
	data, err := json.Marshal(bundle)
	if err != nil {
		return "", fmt.Errorf("agent analysis: encode evidence bundle: %w", err)
	}
	instruction := "Use the " + SkillName + " skill. Analyze only the frozen evidence bundle below and the checked-out source at its pinned revision. Write exactly " + OutputPath + ".\n\nFrozen evidence bundle:\n" + string(data)
	if len(instruction)+len(failureAnalysisSkill) > maxAgentPromptBytes {
		return "", fmt.Errorf("%w: composed agent prompt exceeds %d bytes", ErrInvalidBundle, maxAgentPromptBytes)
	}
	return instruction, nil
}

func validateGeneratedOutput(generated agentruntime.GenerateResult) error {
	switch len(generated.Files) {
	case 0:
		return newShadowResultError(ShadowStatusNoResult, fmt.Errorf("agent did not write %s", OutputPath))
	case 1:
	default:
		return newShadowResultError(ShadowStatusExtraFile, fmt.Errorf("agent changed %d files, want only %s", len(generated.Files), OutputPath))
	}
	body, ok := generated.Files[OutputPath]
	if !ok {
		return newShadowResultError(ShadowStatusExtraFile, fmt.Errorf("agent did not write %s", OutputPath))
	}
	if body == "" || len(body) > maxResultBytes {
		return newShadowResultError(ShadowStatusMalformedResult, fmt.Errorf("output file is empty or oversized"))
	}
	return validateNewFileDiff(generated.Diff)
}

func validateNewFileDiff(diff string) error {
	sections := 0
	newFileMode, oldNull, newPath := false, false, false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			sections++
			if line != "diff --git a/"+OutputPath+" b/"+OutputPath {
				return newShadowResultError(ShadowStatusExtraFile, fmt.Errorf("diff contains unexpected path metadata"))
			}
		case line == "new file mode 100644":
			newFileMode = true
		case line == "--- /dev/null":
			oldNull = true
		case line == "+++ b/"+OutputPath:
			newPath = true
		case strings.HasPrefix(line, "deleted file mode "):
			return newShadowResultError(ShadowStatusDeletion, fmt.Errorf("diff contains deletion metadata"))
		case strings.HasPrefix(line, "rename from "), strings.HasPrefix(line, "rename to "), strings.HasPrefix(line, "similarity index "):
			return newShadowResultError(ShadowStatusRename, fmt.Errorf("diff contains rename metadata"))
		case strings.HasPrefix(line, "copy from "), strings.HasPrefix(line, "copy to "):
			return newShadowResultError(ShadowStatusExtraFile, fmt.Errorf("diff contains copy metadata"))
		case strings.HasPrefix(line, "old mode "), strings.HasPrefix(line, "new mode "), strings.HasPrefix(line, "Binary files "):
			return newShadowResultError(ShadowStatusContractViolation, fmt.Errorf("diff contains unsupported metadata"))
		}
	}
	if sections != 1 || !newFileMode || !oldNull || !newPath {
		return newShadowResultError(ShadowStatusContractViolation, fmt.Errorf("output must be one newly added regular file"))
	}
	return nil
}

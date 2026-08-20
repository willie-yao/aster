package agentanalysis

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentsandbox"
	"github.com/willie-yao/aster/backend/internal/models"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

type fakeWorkspaceSandbox struct {
	identity string
	run      func(agentsandbox.Spec) (agentsandbox.Result, error)
}

func (f fakeWorkspaceSandbox) Run(_ context.Context, spec agentsandbox.Spec) (agentsandbox.Result, error) {
	return f.run(spec)
}
func (fakeWorkspaceSandbox) Cleanup(context.Context, engineruntime.WorkRef) error { return nil }
func (f fakeWorkspaceSandbox) RuntimeIdentity() string                            { return f.identity }

func TestVerifyWorkspaceSourcesUsesIndependentDeadlines(t *testing.T) {
	sources := []WorkspaceSourceRef{{ID: "first"}, {ID: "second"}}
	policies := []WorkspaceSourceMode{{SourceID: "first", Policy: WorkspaceSourceModePreserve}, {SourceID: "second", Policy: WorkspaceSourceModePreserve}}
	calls := 0
	verify := func(ctx context.Context, root, _ string, _ WorkspaceSourceModePolicy) error {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("source verification context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining < WorkspaceSourceVerificationTimeout-100*time.Millisecond || remaining > WorkspaceSourceVerificationTimeout {
			t.Fatalf("source %s deadline=%s", filepath.Base(root), remaining)
		}
		if calls == 1 {
			time.Sleep(50 * time.Millisecond)
		}
		return nil
	}
	if err := verifyWorkspaceSources(t.Context(), t.TempDir(), sources, policies, WorkspaceSourceVerificationTimeout, verify); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestWorkspaceSandboxRuntimeValidatesOneResult(t *testing.T) {
	runtime, spec := workspaceSandboxFixture(t)
	calls := 0
	runtime.Sandbox = fakeWorkspaceSandbox{identity: strings.Repeat("c", 64), run: func(got agentsandbox.Spec) (agentsandbox.Result, error) {
		calls++
		if got.Purpose != "analysis" || got.RequestEnv != WorkspaceExecutionRequestEnv || got.FinalizationGrace != WorkspacePostModelGraceForSources(len(spec.Request.Manifest.Sources)) || got.PreparedWorkspace != nil || got.StagedWorkspace == nil || got.StagedWorkspace.RequestEnv != WorkspaceStageRequestEnv {
			t.Fatalf("spec=%+v", got)
		}
		var request WorkspaceExecutionRequest
		if err := json.Unmarshal(got.Request, &request); err != nil || request.Hash != spec.Request.Hash {
			t.Fatalf("request=%+v err=%v", request, err)
		}
		var stage WorkspaceStageRequest
		if err := json.Unmarshal(got.StagedWorkspace.Request, &stage); err != nil || stage.Hash != spec.StageRequest.Hash {
			t.Fatalf("stage=%+v err=%v", stage, err)
		}
		data, _ := json.Marshal(validWorkspaceExecution(spec.Request))
		return agentsandbox.Result{
			Output: string(data), FinishedReason: "PodSucceeded",
			Resources: engineruntime.ResourceMetadata{Backend: agentsandbox.Backend, Namespace: "analysis", Name: "analysis-1"},
			Telemetry: engineruntime.GenerateTelemetry{TaskFinalized: true, ResultAvailable: true, CleanupCompleted: true},
		}, nil
	}}
	result, err := runtime.Analyze(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.Execution.Analysis == nil || !result.Telemetry.FinalizationChecked || !result.Telemetry.FinalizationValid || result.CleanupWork != nil {
		t.Fatalf("result=%+v calls=%d", result, calls)
	}
}

func TestWorkspaceSandboxRuntimePreservesValidResultWhenCleanupIsPending(t *testing.T) {
	runtime, spec := workspaceSandboxFixture(t)
	work := &engineruntime.WorkRef{Backend: agentsandbox.Backend, Namespace: "analysis", Name: "analysis-1", UID: "uid-1"}
	runtime.Sandbox = fakeWorkspaceSandbox{identity: strings.Repeat("c", 64), run: func(agentsandbox.Spec) (agentsandbox.Result, error) {
		data, _ := json.Marshal(validWorkspaceExecution(spec.Request))
		return agentsandbox.Result{Output: string(data), FinishedReason: "PodSucceeded", Work: work, Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: false}}, engineruntime.ErrCleanupPending
	}}
	result, err := runtime.Analyze(t.Context(), spec)
	if !errors.Is(err, engineruntime.ErrCleanupPending) || result.Execution.Analysis == nil || result.CleanupWork == nil || result.CleanupWork.UID != "uid-1" || !result.Telemetry.FinalizationValid {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestWorkspaceSandboxRuntimeRejectsPodResultMismatch(t *testing.T) {
	runtime, spec := workspaceSandboxFixture(t)
	runtime.Sandbox = fakeWorkspaceSandbox{identity: strings.Repeat("c", 64), run: func(agentsandbox.Spec) (agentsandbox.Result, error) {
		data, _ := json.Marshal(validWorkspaceExecution(spec.Request))
		return agentsandbox.Result{Output: string(data), FinishedReason: "PodFailed", Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: true}}, nil
	}}
	_, err := runtime.Analyze(t.Context(), spec)
	if !errors.Is(err, engineruntime.ErrResultContract) {
		t.Fatalf("error=%v", err)
	}
}

func TestWorkspaceSandboxRuntimeRejectsMalformedResult(t *testing.T) {
	runtime, spec := workspaceSandboxFixture(t)
	runtime.Sandbox = fakeWorkspaceSandbox{identity: strings.Repeat("c", 64), run: func(agentsandbox.Spec) (agentsandbox.Result, error) {
		return agentsandbox.Result{Output: `{"version":`, FinishedReason: "PodFailed", Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: true}}, nil
	}}
	result, err := runtime.Analyze(t.Context(), spec)
	if !errors.Is(err, engineruntime.ErrMalformedResult) || !result.Telemetry.FinalizationChecked || result.Telemetry.FinalizationValid {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestWorkspaceSandboxRuntimeMapsTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state engineruntime.TerminalState
		want  error
	}{
		{name: "cancelled", state: engineruntime.TerminalCancelled, want: engineruntime.ErrCancelled},
		{name: "timed out", state: engineruntime.TerminalTimedOut, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime, spec := workspaceSandboxFixture(t)
			runtime.Sandbox = fakeWorkspaceSandbox{identity: strings.Repeat("c", 64), run: func(agentsandbox.Spec) (agentsandbox.Result, error) {
				execution := WorkspaceExecutionResult{
					Version: WorkspaceResultVersion, ContractVersion: WorkspaceContractVersion, RequestHash: spec.Request.Hash,
					TerminalState: tc.state, FailureReason: "stopped", DurationMs: 100, Usage: WorkspaceUsage{Status: WorkspaceTelemetryUnavailable}, OpenCodeTelemetry: WorkspaceOpenCodeTelemetry{Status: WorkspaceTelemetryUnavailable, StructuredOutputRetriesKnown: true},
				}
				data, _ := json.Marshal(execution)
				return agentsandbox.Result{Output: string(data), FinishedReason: "PodFailed", Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: true}}, nil
			}}
			result, err := runtime.Analyze(t.Context(), spec)
			if err == nil || result.Execution.TerminalState != tc.state || !result.Telemetry.FinalizationValid {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
		})
	}
}

func TestWorkspaceSandboxRuntimeRejectsInvalidStageRequest(t *testing.T) {
	runtime, spec := workspaceSandboxFixture(t)
	runtime.Sandbox = fakeWorkspaceSandbox{identity: strings.Repeat("c", 64), run: func(agentsandbox.Spec) (agentsandbox.Result, error) {
		t.Fatal("sandbox should not run for invalid stage request")
		return agentsandbox.Result{}, nil
	}}
	spec.StageRequest.ManifestHash = strings.Repeat("0", 64)
	if _, err := runtime.Analyze(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "stage request") {
		t.Fatalf("error=%v", err)
	}
}

func TestWorkspaceSandboxRuntimeIdentityIncludesConfiguration(t *testing.T) {
	runtime, _ := workspaceSandboxFixture(t)
	runtime.Sandbox = fakeWorkspaceSandbox{identity: strings.Repeat("c", 64)}
	base := runtime.RuntimeIdentity()
	if base == "" {
		t.Fatal("runtime identity is empty")
	}
	changed := *runtime
	changed.OutputLimitBytes++
	if changed.RuntimeIdentity() == base {
		t.Fatal("output limit did not affect runtime identity")
	}
	changed = *runtime
	changed.Sandbox = fakeWorkspaceSandbox{identity: strings.Repeat("d", 64)}
	if changed.RuntimeIdentity() == base {
		t.Fatal("Sandbox identity did not affect runtime identity")
	}
	changed = *runtime
	changed.SourceModePolicy = WorkspaceSourceModeIgnoreExecutable
	if changed.RuntimeIdentity() == base {
		t.Fatal("source mode policy did not affect runtime identity")
	}
}

func TestWorkspaceSandboxRuntimeRejectsUnknownPodReason(t *testing.T) {
	for _, reason := range []string{"", "Evicted"} {
		t.Run(reason, func(t *testing.T) {
			runtime, spec := workspaceSandboxFixture(t)
			runtime.Sandbox = fakeWorkspaceSandbox{identity: strings.Repeat("c", 64), run: func(agentsandbox.Spec) (agentsandbox.Result, error) {
				data, _ := json.Marshal(validWorkspaceExecution(spec.Request))
				return agentsandbox.Result{Output: string(data), FinishedReason: reason, Telemetry: engineruntime.GenerateTelemetry{CleanupCompleted: true}}, nil
			}}
			_, err := runtime.Analyze(t.Context(), spec)
			if !errors.Is(err, engineruntime.ErrResultContract) {
				t.Fatalf("reason=%q error=%v", reason, err)
			}
		})
	}
}

func TestWorkspaceSandboxRuntimeRejectsConfigurationMismatch(t *testing.T) {
	runtime, spec := workspaceSandboxFixture(t)
	runtime.Sandbox = fakeWorkspaceSandbox{identity: strings.Repeat("c", 64), run: func(agentsandbox.Spec) (agentsandbox.Result, error) {
		t.Fatal("sandbox should not run for configuration mismatch")
		return agentsandbox.Result{}, nil
	}}
	runtime.Provider.Model = "other-model"
	if _, err := runtime.Analyze(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "configured provider") {
		t.Fatalf("error=%v", err)
	}
}

func TestWorkspaceSandboxRuntimeRejectsSourceModePolicyMismatch(t *testing.T) {
	runtime, spec := workspaceSandboxFixture(t)
	runtime.Sandbox = fakeWorkspaceSandbox{identity: strings.Repeat("c", 64), run: func(agentsandbox.Spec) (agentsandbox.Result, error) {
		t.Fatal("sandbox should not run for source mode policy mismatch")
		return agentsandbox.Result{}, nil
	}}
	runtime.SourceModePolicy = WorkspaceSourceModeIgnoreExecutable
	if _, err := runtime.Analyze(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "configured source mode policy") {
		t.Fatalf("error=%v", err)
	}

	runtime.SourceModePolicy = WorkspaceSourceModePreserve
	spec.StageRequest, _ = NewWorkspaceStageRequestWithSourceModePolicies(spec.Request.Manifest, WorkspaceSourceModePreserve, WorkspaceSourceModeIgnoreExecutable)
	if _, err := runtime.Analyze(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "stage output and execution source mode policies differ") {
		t.Fatalf("error=%v", err)
	}
}

func workspaceSandboxFixture(t *testing.T) (*WorkspaceSandboxRuntime, WorkspaceSandboxSpec) {
	t.Helper()
	sourceRoot, artifactRoot, failure, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(failure, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	gateway := testGatewayProvider("https://model-gateway.platform.svc.cluster.local:8443/v1", "test-model")
	request, err := NewWorkspaceExecutionRequest(manifest, gateway, time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := NewWorkspaceStageRequest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &WorkspaceSandboxRuntime{Provider: gateway, Timeout: time.Minute, OutputLimitBytes: 128 << 10}
	spec := WorkspaceSandboxSpec{Request: request, StageRequest: stage, SourcesRoot: workspaceTestSourcesRoot(t, sourceRoot), ArtifactRoot: artifactRoot, ExecutionID: "analysis-1"}
	return runtime, spec
}

func validWorkspaceExecution(request WorkspaceExecutionRequest) WorkspaceExecutionResult {
	analysis := WorkspaceAnalysis{
		Summary: "The controller rejected the request.", RootCause: "The specific failure occurred before cleanup.", Severity: "High",
		SuggestedFix: "Correct the request before retrying.", RelevantFiles: []string{"pkg/controller.go"},
		EvidenceCitations: []models.EvidenceCitation{{Path: "logs/build.log", LineStart: 2, LineEnd: 2, Quote: "artifact-only-marker specific failure"}},
		SourceCitations:   []WorkspaceSourceCitation{{SourceID: "primary", Path: "pkg/controller.go", LineStart: 3, LineEnd: 3, Quote: "func reconcile() {}", Verified: true}},
	}
	return WorkspaceExecutionResult{
		Version: WorkspaceResultVersion, ContractVersion: WorkspaceContractVersion, RequestHash: request.Hash,
		TerminalState: engineruntime.TerminalSucceeded, Analysis: &analysis, ResultValidation: WorkspaceResultValidation{Status: WorkspaceResultAccepted}, DurationMs: 100, Usage: WorkspaceUsage{Status: WorkspaceTelemetryUnavailable},
		OpenCodeTelemetry: WorkspaceOpenCodeTelemetry{
			Available: true, Status: WorkspaceTelemetryAvailable, EventCount: 4, ProviderRequests: 2, ProviderRequestsKnown: true, StepsUsed: 2, StructuredOutputRetriesKnown: true,
			EvidencePhaseCompleted: true, EvidencePhaseSteps: 1, EvidencePhaseRequests: 1, ArtifactEvidenceToolCalls: 1, SourceEvidenceToolCalls: 1,
			FinalizationPhaseCompleted: true, FinalizationPhaseSteps: 1, FinalizationPhaseRequests: 1, StructuredOutputToolCalls: 1,
		},
	}
}

func TestWorkspaceSandboxRuntimeVerifiesEverySource(t *testing.T) {
	runtime, spec := workspaceSandboxFixture(t)
	primaryRoot := filepath.Join(spec.SourcesRoot, "primary")
	dependencyRoot := filepath.Join(spec.SourcesRoot, "dependency")
	if err := copyWorkspaceTestTree(primaryRoot, dependencyRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dependencyRoot, "pkg", "controller.go"), []byte("package dependency\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, dependencyRoot, "add", "pkg/controller.go")
	runWorkspaceGit(t, dependencyRoot, "commit", "-qm", "dependency revision")
	dependencyRevision := strings.TrimSpace(runWorkspaceGit(t, dependencyRoot, "rev-parse", "HEAD"))
	primary := spec.Request.Manifest.Sources[0]
	_, err := NewWorkspaceManifestWithSources(spec.Request.Manifest.Request, []WorkspaceSourceRef{
		{ID: "dependency", Repository: primary.Repository},
		{ID: "primary", Repository: primary.Repository},
	}, spec.Request.Manifest.ConsumerPrompt, spec.Request.Manifest.Artifacts)
	if err == nil {
		t.Fatal("duplicate source revisions were accepted")
	}
	dependency := primary.Repository
	dependency.Revision = dependencyRevision
	manifest, err := NewWorkspaceManifestWithSources(spec.Request.Manifest.Request, []WorkspaceSourceRef{
		{ID: "dependency", Repository: dependency},
		{ID: "primary", Repository: primary.Repository},
	}, spec.Request.Manifest.ConsumerPrompt, spec.Request.Manifest.Artifacts)
	if err != nil {
		t.Fatal(err)
	}
	spec.Request, err = NewWorkspaceExecutionRequest(manifest, runtime.Provider, runtime.Timeout, 20, 200000, 8192, runtime.OutputLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	spec.StageRequest, err = NewWorkspaceStageRequest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dependencyRoot, "pkg", "controller.go"), []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime.Sandbox = fakeWorkspaceSandbox{run: func(agentsandbox.Spec) (agentsandbox.Result, error) {
		t.Fatal("sandbox ran before every source was verified")
		return agentsandbox.Result{}, nil
	}}
	if _, err := runtime.Analyze(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "verify workspace source dependency") {
		t.Fatalf("error=%v", err)
	}
}

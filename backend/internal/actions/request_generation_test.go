package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	fixpr "github.com/willie-yao/aster/backend/internal/fix/pr"
	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/issues"
	"github.com/willie-yao/aster/backend/internal/runtime"
)

func TestGenerationFailureRetainsRuntimeCleanup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hasRuntime bool
		identity   bool
	}{
		{name: "without runtime"},
		{name: "cleanup API error", hasRuntime: true},
		{name: "identity mismatch", hasRuntime: true, identity: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, pattern := requestTestService(t)
			const id = "generation-failure"
			request := savedCleanupRequest(id, pattern.ID, "alice")
			request.Status, request.Runtime, request.Cleanup = RequestPending, nil, nil
			service.requests.Requests[id] = request
			service.requestDone[id] = make(chan struct{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			service.ConfigureAsyncRequestsWithContext(ctx, time.Minute, nil)
			fake := &fakeManagedAgentRuntime{}
			var available atomic.Bool
			service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) {
				if !available.Load() {
					return nil, runtime.ErrUnavailable
				}
				return fake, nil
			}
			cause := errors.New("cleanup API read failed")
			if tc.identity {
				cause = runtime.ErrWorkIdentityChanged
			}
			failure := withReason(ReasonSourceVerificationInconclusive, cause, "")
			service.generateRequestWith(id, "token", func(ctx context.Context, _, _, _, _ string, _ *issues.IssueSpec, _, _ string) (PreviewResult, *previewEntry, error) {
				if tc.hasRuntime {
					if err := service.observeRuntimeWork(id)(ctx, runtime.WorkRef{Backend: "agent-sandbox", Namespace: "sandbox", Name: "fix-" + id, UID: "uid-" + id, ExecutionID: id}); err != nil {
						return PreviewResult{}, nil, err
					}
				}
				return PreviewResult{}, nil, failure
			})
			if tc.hasRuntime {
				assertCleanupDebt(t, service, id)
				service.rmu.Lock()
				cleanup := *service.requests.Requests[id].Cleanup
				service.rmu.Unlock()
				if cleanup.ReasonCode != ReasonSourceVerificationInconclusive || cleanup.Reason != ReasonMessage(ReasonSourceVerificationInconclusive) || cleanup.Blocked != tc.identity {
					t.Fatalf("generation diagnosis lost: %+v", cleanup)
				}
				if tc.identity {
					cancel()
					waitForCleanupWorkers(t, service)
					reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
					assertCleanupDebt(t, reloaded, id)
					if !reloaded.requests.Requests[id].Cleanup.Blocked {
						t.Fatal("identity mismatch was not retained")
					}
					return
				}
				available.Store(true)
			}
			view := waitRequest(t, service, id, "alice", RequestFailed)
			if view.ReasonCode != ReasonSourceVerificationInconclusive || view.Error != ReasonMessage(ReasonSourceVerificationInconclusive) {
				t.Fatalf("final generation diagnosis = %+v", view)
			}
			cancel()
			waitForCleanupWorkers(t, service)
			fake.mu.Lock()
			defer fake.mu.Unlock()
			want := 0
			if tc.hasRuntime {
				want = 1
			}
			if len(fake.refs) != want {
				t.Fatalf("cleanup refs = %+v, want %d", fake.refs, want)
			}
		})
	}
}

type timeoutPreviewPRClient struct {
	*ghpr.Client
}

func (timeoutPreviewPRClient) ResolveBase(context.Context, string, string, string) (ghpr.Base, error) {
	return ghpr.Base{Branch: "main", HeadSHA: analysisFixRevision, TreeSHA: "tree"}, nil
}

type timeoutPreviewRuntime struct {
	actionDeadline bool
	cleanupPending bool
}

func (r timeoutPreviewRuntime) Generate(ctx context.Context, _ runtime.GenerateSpec) (runtime.ExecutionResult, error) {
	if r.actionDeadline {
		<-ctx.Done()
	}
	err := errors.New("coding agent deadline exceeded")
	if r.cleanupPending {
		err = errors.Join(err, runtime.ErrCleanupPending)
	}
	return runtime.ExecutionResult{
		TerminalState: runtime.TerminalTimedOut,
		FailureReason: "coding agent deadline exceeded",
		StdoutSummary: "investigated pinned source",
		StderrSummary: "partial progress https://private.example/path token=fixture-secret",
		CommandResults: []runtime.CommandResult{{
			Argv: []string{"go", "test", "./..."}, ExitCode: -1, DurationMs: 1000, TimedOut: true,
			Stdout: "private stdout", Stderr: "private stderr",
		}},
	}, err
}

func TestTimedOutExecutorDiagnosticReachesRequestAndLog(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime timeoutPreviewRuntime
	}{
		{name: "executor timeout"},
		{name: "cleanup pending", runtime: timeoutPreviewRuntime{cleanupPending: true}},
		{name: "action deadline", runtime: timeoutPreviewRuntime{actionDeadline: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				service, _ := analysisRequestTestService(t)
				service.ConfigureAsyncRequests(time.Minute, nil)
				var logs bytes.Buffer
				log.SetOutput(&logs)
				t.Cleanup(func() { log.SetOutput(os.Stderr) })
				commands := []runtime.ExecutionCommand{
					{Argv: []string{"go", "test", "./..."}, TimeoutSeconds: 1},
					{Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: 1},
				}
				manager := fixpr.NewManager(timeoutPreviewPRClient{}, filepath.Join(service.dataDir, "fixture-fix-state.json"), fixpr.Options{
					SourceOwner: "kubernetes-sigs", SourceName: "cluster-api-provider-azure", MaxFiles: 2,
					Agent: &fixpr.AgentConfig{
						Runtime: tc.runtime, RequireCommandResults: true,
						CommandPolicy: runtime.CommandPolicy{Commands: commands},
					},
				})
				service.analysisRequestGenerator = func(ctx context.Context, input AnalysisFixInput, _, _, instruction string) (PreviewResult, error) {
					subject, err := service.resolveAnalysisFixTarget(input, true)
					if err != nil {
						return PreviewResult{}, err
					}
					failure := analysisFailureForGeneration(subject, input)
					_, err = manager.GenerateAnalysisPreview(ctx, failure, instruction)
					return PreviewResult{}, safeAnalysisFixPreviewError(err)
				}
				created, err := service.CreateAnalysisFixRequest(t.Context(), exactAnalysisRequestInput(), "alice", "write-token", "")
				if err != nil {
					t.Fatal(err)
				}
				if err := service.Wait(t.Context()); err != nil {
					t.Fatal(err)
				}
				final, err := service.GetRequest(created.ID, "alice")
				if err != nil || final.Status != RequestFailed || final.Failure == nil ||
					final.Failure.Category != AnalysisFixFailureTimedOut || final.Failure.TerminalState != runtime.TerminalTimedOut {
					t.Fatalf("request=%+v err=%v", final, err)
				}
				wantSummary := "coding agent deadline exceeded partial progress [redacted-url] token=[redacted] investigated pinned source"
				if final.Failure.OperatorSummary != wantSummary || len(final.Failure.CommandResults) != 1 {
					t.Fatalf("failure = %+v", final.Failure)
				}
				if strings.Contains(final.Error, "cleanup") {
					t.Fatalf("cleanup masked the timeout: %q", final.Error)
				}
				command := final.Failure.CommandResults[0]
				if !command.TimedOut || command.ExitCode != -1 || command.DurationMs != 1000 ||
					strings.Join(command.Argv, " ") != "go test ./..." {
					t.Fatalf("command = %+v", command)
				}
				encoded, err := json.Marshal(final)
				if err != nil {
					t.Fatal(err)
				}
				persisted, err := os.ReadFile(service.requestStatePath())
				if err != nil {
					t.Fatal(err)
				}
				for _, data := range []string{string(encoded), string(persisted), logs.String()} {
					if !strings.Contains(data, wantSummary) {
						t.Fatalf("timeout summary was lost: %s", data)
					}
					for _, forbidden := range []string{"fixture-secret", "private.example", "private stdout", "private stderr"} {
						if strings.Contains(data, forbidden) {
							t.Fatalf("timeout diagnostic disclosed %q: %s", forbidden, data)
						}
					}
				}
				if !strings.Contains(logs.String(), `category=timed_out`) ||
					!strings.Contains(logs.String(), `operator_summary="coding agent deadline exceeded`) {
					t.Fatalf("log = %q", logs.String())
				}
				reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
				restored, err := reloaded.GetRequest(created.ID, "alice")
				if err != nil || restored.Failure == nil || restored.Failure.OperatorSummary != wantSummary ||
					len(restored.Failure.CommandResults) != 1 || restored.Failure.Category != AnalysisFixFailureTimedOut {
					t.Fatalf("restored=%+v err=%v", restored, err)
				}
			})
		})
	}
}

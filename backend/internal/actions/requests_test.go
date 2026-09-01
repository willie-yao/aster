package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/actionverify"
	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/issues"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
	"github.com/willie-yao/aster/backend/internal/statefile"
)

func requestTestService(t *testing.T) (*Service, models.PatternAnalysis) {
	t.Helper()
	dataDir := t.TempDir()
	pattern := systemicPattern()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{
		JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pattern},
	})
	cfg := &project.Config{
		Name:     "capz",
		Branding: project.Branding{SiteURL: "https://dash.example.com", SourceRepo: project.SourceRepo{Owner: "o", Name: "r"}},
		Issues:   &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}},
	}
	service := NewService(cfg, dataDir, AIConfig{})
	service.sourceVerifier = nil
	return service, pattern
}

func waitRequest(t *testing.T, service *Service, id, owner string, want ...string) ActionRequestView {
	t.Helper()
	allowed := map[string]bool{}
	for _, status := range want {
		allowed[status] = true
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, err := service.GetRequest(id, owner)
		if err == nil && allowed[view.Status] {
			return view
		}
		time.Sleep(10 * time.Millisecond)
	}
	view, err := service.GetRequest(id, owner)
	t.Fatalf("request did not reach %v: view=%+v err=%v", want, view, err)
	return ActionRequestView{}
}

func exactAnalysisRequestInput() AnalysisFixInput {
	return AnalysisFixInput{
		Identity: AnalysisIdentity{
			Project: "CAPZ", JobID: "periodic-capz", BuildID: "123", TestName: "TestCluster", Source: "test",
			SuiteName: "CAPZ", ClassName: "e2e", JUnitFile: "junit.xml", AnalysisGeneratedAt: "2026-08-13T01:00:00Z",
		},
		ChatSessionID: "session", ChatRequestID: "chat-request", ChatResponseHash: strings.Repeat("a", 64),
		PreviewRequestHash: strings.Repeat("b", 64), AnalysisContentHash: strings.Repeat("c", 64),
		SourceRepository: sourceinvestigation.Repository{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure", Revision: strings.Repeat("d", 40)},
		FailureRevision:  strings.Repeat("d", 40), GenerationBaseRevision: strings.Repeat("e", 40), SourceBranch: "main",
		VerifiedSourceFileHashes: map[string]string{"test/e2e/cni.go": strings.Repeat("f", 64)},
		AssistantAnswer:          "The cited conflict is handled by `InstallCNIManifest`.",
		ArtifactCitations:        []fixpr.Evidence{{Path: "build-log.txt", LineStart: 10, LineEnd: 10, Quote: "Conflict"}},
	}
}

func TestAnalysisFixRequestSurvivesInitiatingRequestLifecycle(t *testing.T) {
	service, _ := requestTestService(t)
	service.ConfigureAsyncRequests(time.Minute, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var generatedInput AnalysisFixInput
	service.analysisRequestGenerator = func(ctx context.Context, input AnalysisFixInput, owner, token, instruction string) (PreviewResult, error) {
		calls.Add(1)
		generatedInput = input
		if owner != "alice" || token != "write-token" || instruction != "keep compatibility" || input.Identity.Project != "capz" {
			return PreviewResult{}, fmt.Errorf("generation identity project=%q owner=%q token=%q instruction=%q", input.Identity.Project, owner, token, instruction)
		}
		if err := service.setRequestStage(ctx, RequestStageDrafting); err != nil {
			return PreviewResult{}, err
		}
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return PreviewResult{}, ctx.Err()
		}
		return PreviewResult{
			Token: "preview-token", Kind: gfKind, Title: "Fix the CNI conflict",
			Body: "## Summary\nRetry the CNI update conflict.\n", Diff: "diff --git a/test/e2e/cni.go b/test/e2e/cni.go",
		}, nil
	}

	input := exactAnalysisRequestInput()
	input.Identity.Project = ""
	created, err := service.CreateAnalysisFixRequest(input, "Alice", "write-token", "keep compatibility")
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != RequestPending || created.Kind != requestKindAnalysisFix || created.Owner != "alice" {
		t.Fatalf("created = %+v", created)
	}
	<-started
	if generatedInput.Identity.Project != "capz" {
		t.Fatalf("background input project = %q", generatedInput.Identity.Project)
	}
	duplicateInput := input
	duplicateInput.Identity.Project = "caller-project"
	duplicate, err := service.CreateAnalysisFixRequest(duplicateInput, "alice", "write-token", "keep compatibility")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.ID != created.ID || calls.Load() != 1 {
		t.Fatalf("duplicate=%+v calls=%d", duplicate, calls.Load())
	}
	pendingState, err := os.ReadFile(service.requestStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pendingState), "write-token") {
		t.Fatal("write token was persisted in pending analysis fix state")
	}
	var pending actionRequestState
	if err := json.Unmarshal(pendingState, &pending); err != nil {
		t.Fatal(err)
	}
	pendingRecord := pending.Requests[created.ID]
	if pendingRecord == nil || pendingRecord.AnalysisFix == nil || pendingRecord.AnalysisFix.Identity.Project != "capz" {
		t.Fatalf("pending persisted request = %+v", pendingRecord)
	}
	if view := waitRequest(t, service, created.ID, "alice", RequestPending); view.Stage != RequestStageDrafting {
		t.Fatalf("pending view = %+v", view)
	}
	close(release)
	ready := waitRequest(t, service, created.ID, "alice", RequestReady)
	if ready.Preview == nil || ready.Preview.Token != "preview-token" || ready.Preview.Diff == "" {
		t.Fatalf("ready = %+v", ready)
	}
	recovered, err := service.CreateAnalysisFixRequest(input, "alice", "write-token", "keep compatibility")
	if err != nil || recovered.ID != created.ID || recovered.Status != RequestReady || calls.Load() != 1 {
		t.Fatalf("recovered=%+v calls=%d err=%v", recovered, calls.Load(), err)
	}
	expiresAt, err := time.Parse(time.RFC3339, ready.ExpiresAt)
	if err != nil || time.Until(expiresAt) < 14*time.Minute || time.Until(expiresAt) > 16*time.Minute {
		t.Fatalf("ready expiry=%q err=%v", ready.ExpiresAt, err)
	}

	data, err := os.ReadFile(service.requestStatePath())
	if err != nil {
		t.Fatal(err)
	}
	var persisted actionRequestState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	record := persisted.Requests[created.ID]
	if record == nil || record.AnalysisFix != nil || record.Instruction != "" || record.RequestHash != input.PreviewRequestHash {
		t.Fatalf("persisted request = %+v", record)
	}
	if err := service.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAnalysisFixRequestOverridesCallerProject(t *testing.T) {
	service, _ := requestTestService(t)
	service.cfg.Name = " capz "
	service.ConfigureAsyncRequests(time.Minute, nil)
	started := make(chan AnalysisFixInput, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	service.analysisRequestGenerator = func(ctx context.Context, input AnalysisFixInput, _, _, _ string) (PreviewResult, error) {
		calls.Add(1)
		started <- input
		select {
		case <-release:
		case <-ctx.Done():
			return PreviewResult{}, ctx.Err()
		}
		return PreviewResult{Token: "preview-token", Kind: gfKind, Title: "Fix conflict", Body: "## Summary\nRetry conflict.\n", Diff: "diff --git a/a b/a"}, nil
	}
	input := exactAnalysisRequestInput()
	input.Identity.Project = "caller-project"
	created, err := service.CreateAnalysisFixRequest(input, "alice", "write-token", "")
	if err != nil {
		t.Fatal(err)
	}
	generated := <-started
	if generated.Identity.Project != "capz" {
		t.Fatalf("background input project = %q", generated.Identity.Project)
	}
	data, err := os.ReadFile(service.requestStatePath())
	if err != nil {
		t.Fatal(err)
	}
	var pending actionRequestState
	if err := json.Unmarshal(data, &pending); err != nil {
		t.Fatal(err)
	}
	record := pending.Requests[created.ID]
	if record == nil || record.AnalysisFix == nil || record.AnalysisFix.Identity.Project != "capz" {
		t.Fatalf("pending persisted request = %+v", record)
	}
	close(release)
	waitRequest(t, service, created.ID, "alice", RequestReady)
	if calls.Load() != 1 {
		t.Fatalf("generator calls = %d", calls.Load())
	}
	if err := service.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAnalysisFixRequestRejectsInvalidInputBeforePersistence(t *testing.T) {
	service, _ := requestTestService(t)
	service.ConfigureAsyncRequests(time.Minute, nil)
	var writes atomic.Int32
	var calls atomic.Int32
	service.requestStateWriter = func(string, any) error {
		writes.Add(1)
		return nil
	}
	service.analysisRequestGenerator = func(context.Context, AnalysisFixInput, string, string, string) (PreviewResult, error) {
		calls.Add(1)
		return PreviewResult{}, nil
	}
	input := exactAnalysisRequestInput()
	input.Identity.Project = "caller-project"
	input.Identity.JobID = ""
	if _, err := service.CreateAnalysisFixRequest(input, "alice", "write-token", ""); err == nil {
		t.Fatal("invalid exact analysis fix request was admitted")
	}
	service.rmu.Lock()
	persisted := len(service.requests.Requests)
	service.rmu.Unlock()
	if writes.Load() != 0 || calls.Load() != 0 || persisted != 0 {
		t.Fatalf("writes=%d calls=%d persisted=%d", writes.Load(), calls.Load(), persisted)
	}
}

func TestActionRequestStateV6MigratesToV7(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: 6, Requests: map[string]*actionRequest{
		"existing": {ActionRequestView: ActionRequestView{
			ID: "existing", FailureID: "failure", Kind: "create-issue", Owner: "alice", Status: RequestFailed,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		}},
	}}
	if err := statefile.WritePrivateJSONDurable(service.requestStatePath(), state); err != nil {
		t.Fatal(err)
	}
	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	if view, err := reloaded.GetRequest("existing", "alice"); err != nil || view.Status != RequestFailed {
		t.Fatalf("migrated view=%+v err=%v", view, err)
	}
	data, err := os.ReadFile(reloaded.requestStatePath())
	if err != nil {
		t.Fatal(err)
	}
	var migrated actionRequestState
	if err := json.Unmarshal(data, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.Version != actionRequestStateVersion {
		t.Fatalf("migrated version=%d", migrated.Version)
	}
}

func TestReadyAnalysisFixRequestReloads(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: actionRequestStateVersion, Requests: map[string]*actionRequest{
		"analysis-fix": {ActionRequestView: ActionRequestView{
			ID: "analysis-fix", FailureID: "analysis::id", Kind: requestKindAnalysisFix, Owner: "alice", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			Preview: &PreviewResult{Token: "preview-token", Kind: gfKind, Title: "Fix conflict", Body: "## Summary\nRetry conflict.\n", Diff: "diff --git a/a b/a"},
		}, RequestHash: strings.Repeat("a", 64)},
	}}
	if err := statefile.WritePrivateJSONDurable(service.requestStatePath(), state); err != nil {
		t.Fatal(err)
	}
	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("analysis-fix", "alice")
	if err != nil || view.Status != RequestReady || view.Preview == nil || view.Preview.Token != "preview-token" {
		t.Fatalf("reloaded view=%+v err=%v", view, err)
	}
}

func TestPendingAnalysisFixRequestFailsClosedAfterRestart(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: actionRequestStateVersion, Requests: map[string]*actionRequest{
		"analysis-fix": {
			ActionRequestView: ActionRequestView{
				ID: "analysis-fix", FailureID: "analysis::id", Kind: requestKindAnalysisFix, Owner: "alice", Status: RequestPending,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			},
			RequestHash: strings.Repeat("a", 64), AnalysisFix: cloneAnalysisFixInput(exactAnalysisRequestInput()),
		},
	}}
	if err := statefile.WritePrivateJSONDurable(service.requestStatePath(), state); err != nil {
		t.Fatal(err)
	}
	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("analysis-fix", "alice")
	if err != nil || view.Status != RequestFailed || view.Preview != nil {
		t.Fatalf("reloaded view=%+v err=%v", view, err)
	}
	record := reloaded.requests.Requests["analysis-fix"]
	if record == nil || record.AnalysisFix != nil {
		t.Fatalf("reloaded request = %+v", record)
	}
}

func TestAnalysisFixRequestCancellationCleansObservedRuntime(t *testing.T) {
	service, _ := requestTestService(t)
	service.ConfigureAsyncRequests(time.Minute, nil)
	fake := &fakeManagedAgentRuntime{}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	started := make(chan struct{})
	exited := make(chan struct{})
	var calls atomic.Int32
	service.analysisRequestGenerator = func(ctx context.Context, _ AnalysisFixInput, _, _, _ string) (PreviewResult, error) {
		calls.Add(1)
		defer close(exited)
		id := actionRequestID(ctx)
		if id == "" {
			return PreviewResult{}, errors.New("missing action request identity")
		}
		if err := service.observeRuntimeWork(id)(ctx, runtime.WorkRef{Backend: "agent-sandbox", Namespace: "sandbox", Name: "fix-task", UID: "uid-one", ExecutionID: id}); err != nil {
			return PreviewResult{}, err
		}
		close(started)
		<-ctx.Done()
		return PreviewResult{}, ctx.Err()
	}
	created, err := service.CreateAnalysisFixRequest(exactAnalysisRequestInput(), "alice", "write-token", "")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	view, err := service.CancelRequest(context.Background(), created.ID, "alice")
	if err != nil || (view.Status != RequestCancelled && view.Status != RequestCancelling) {
		t.Fatalf("cancel view=%+v err=%v", view, err)
	}
	final := waitRequest(t, service, created.ID, "alice", RequestCancelled)
	if final.Preview != nil || calls.Load() != 1 {
		t.Fatalf("final=%+v calls=%d", final, calls.Load())
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("generator did not exit after cancellation")
	}
	if err := service.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	refs := slices.Clone(fake.refs)
	fake.mu.Unlock()
	if len(refs) != 1 || refs[0].Name != "fix-task" || refs[0].UID != "uid-one" || refs[0].ExecutionID != created.ID {
		t.Fatalf("cleanup refs = %+v", refs)
	}
	service.rmu.Lock()
	record := service.requests.Requests[created.ID]
	service.rmu.Unlock()
	if record == nil || record.AnalysisFix != nil || record.Instruction != "" || record.Runtime != nil || record.Status != RequestCancelled {
		t.Fatalf("persisted cancellation = %+v", record)
	}
}

func TestCancellingReadyAnalysisFixRequestRevokesPreviewToken(t *testing.T) {
	service, _ := requestTestService(t)
	requestHash := strings.Repeat("a", 64)
	token := idempotentPreviewToken("alice", requestHash)
	if err := service.previewStore.update(func(state *previewState, now time.Time) (bool, error) {
		state.Previews[tokenHash(token)] = &persistedPreview{
			Owner: tokenHash("alice"), InitiatedBy: "alice", InitiatedAt: now.Format(time.RFC3339Nano),
			Kind: gfKind, Status: previewStatusReady, CreatedAt: now.Format(time.RFC3339Nano),
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	service.requests.Requests["analysis-fix"] = &actionRequest{
		ActionRequestView: ActionRequestView{
			ID: "analysis-fix", FailureID: "analysis::id", Kind: requestKindAnalysisFix, Owner: "alice", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			Preview: &PreviewResult{Token: token, Kind: gfKind, Title: "Fix conflict", Body: "## Summary\nRetry conflict.\n", Diff: "diff --git a/a b/a"},
		},
		RequestHash: requestHash,
	}
	view, err := service.CancelRequest(context.Background(), "analysis-fix", "alice")
	if err != nil || view.Status != RequestCancelled {
		t.Fatalf("cancel view=%+v err=%v", view, err)
	}
	if _, _, _, _, err := service.previewStore.begin("alice", token, time.Minute); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("revoked preview begin err=%v", err)
	}
}

func TestAnalysisFixRequestTimeoutFailsAndCleansRuntime(t *testing.T) {
	service, _ := requestTestService(t)
	service.ConfigureAsyncRequests(50*time.Millisecond, nil)
	fake := &fakeManagedAgentRuntime{}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var calls atomic.Int32
	service.analysisRequestGenerator = func(ctx context.Context, _ AnalysisFixInput, _, _, _ string) (PreviewResult, error) {
		calls.Add(1)
		id := actionRequestID(ctx)
		if err := service.observeRuntimeWork(id)(ctx, runtime.WorkRef{Backend: "agent-sandbox", Namespace: "sandbox", Name: "timeout-task", UID: "timeout-uid", ExecutionID: id}); err != nil {
			return PreviewResult{}, err
		}
		close(started)
		<-ctx.Done()
		close(cancelled)
		return PreviewResult{}, ctx.Err()
	}
	created, err := service.CreateAnalysisFixRequest(exactAnalysisRequestInput(), "alice", "write-token", "")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	final := waitRequest(t, service, created.ID, "alice", RequestFailed)
	if final.Preview != nil || calls.Load() != 1 || final.ReasonCode != ReasonGenerationFailed ||
		final.Failure == nil || final.Failure.Category != AnalysisFixFailureTimedOut || final.Failure.TerminalState != runtime.TerminalTimedOut {
		t.Fatalf("final=%+v calls=%d", final, calls.Load())
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("generator did not observe timeout cancellation")
	}
	if err := service.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	refs := slices.Clone(fake.refs)
	fake.mu.Unlock()
	if len(refs) != 1 || refs[0].Name != "timeout-task" || refs[0].UID != "timeout-uid" || refs[0].ExecutionID != created.ID {
		t.Fatalf("cleanup refs = %+v", refs)
	}
	if after, err := service.GetRequest(created.ID, "alice"); err != nil || after.Status != RequestFailed || after.Preview != nil ||
		after.Failure == nil || after.Failure.Category != AnalysisFixFailureTimedOut {
		t.Fatalf("post-timeout=%+v err=%v", after, err)
	}
}

func TestAnalysisFixRequestReportsProviderCredentialRejection(t *testing.T) {
	message := strings.Repeat("provider-detail-", 18) + "final-detail"
	if len(message) != 300 {
		t.Fatalf("fixture message is %d bytes", len(message))
	}
	for _, testCase := range []struct {
		name       string
		statusCode int
		detail     AnalysisFixFailureDetail
		statusText string
	}{
		{name: "unauthorized", statusCode: 401, detail: AnalysisFixFailureDetailProviderUnauthorized, statusText: "credential rejected"},
		{name: "forbidden", statusCode: 403, detail: AnalysisFixFailureDetailProviderForbidden, statusText: "request refused"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service, _ := requestTestService(t)
			service.ConfigureAsyncRequests(time.Minute, nil)
			var logs bytes.Buffer
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })
			summary := fmt.Sprintf(
				"Provider message: %s HTTP %d: %s. Secret capz-aster-fix-model/AI_TOKEN; endpoint scheme=https address=api.githubcopilot.com/chat/completions; model gpt-fixture. Provider github-copilot.",
				message, testCase.statusCode, testCase.statusText,
			)
			service.analysisRequestGenerator = func(context.Context, AnalysisFixInput, string, string, string) (PreviewResult, error) {
				return PreviewResult{}, &classifiedAnalysisFixError{
					failure: &AnalysisFixFailureView{
						Category: AnalysisFixFailureProviderCredential, Detail: testCase.detail,
						TerminalState: runtime.TerminalFailed, OperatorSummary: summary,
					},
					cause: withReason(
						ReasonProviderCredentialRejected,
						errors.New("agent fix generation: provider request failed from https://gateway.internal/v1 with Bearer ghp-fixture-secret"),
						summary,
					),
				}
			}
			created, err := service.CreateAnalysisFixRequest(exactAnalysisRequestInput(), "alice", "write-token", "")
			if err != nil {
				t.Fatal(err)
			}
			final := waitRequest(t, service, created.ID, "alice", RequestFailed)
			if final.ReasonCode != ReasonProviderCredentialRejected || final.Failure == nil ||
				final.Failure.Category != AnalysisFixFailureProviderCredential || final.Failure.Detail != testCase.detail {
				t.Fatalf("final = %+v", final)
			}
			if !strings.Contains(final.Failure.OperatorSummary, message) {
				t.Fatalf("persisted provider message was truncated: %q", final.Failure.OperatorSummary)
			}
			if final.Error != ReasonMessage(ReasonProviderCredentialRejected) {
				t.Fatalf("error = %q", final.Error)
			}
			encoded, err := json.Marshal(final)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"ghp-fixture-secret", "gateway.internal"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("API-visible request disclosed %q: %s", secret, encoded)
				}
			}
			reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
			restored, err := reloaded.GetRequest(created.ID, "alice")
			if err != nil || restored.Failure == nil || !strings.Contains(restored.Failure.OperatorSummary, message) ||
				restored.Error != ReasonMessage(ReasonProviderCredentialRejected) {
				t.Fatalf("restored=%+v err=%v", restored, err)
			}
			logged := logs.String()
			if !strings.Contains(logged, string(ReasonProviderCredentialRejected)) {
				t.Fatalf("log = %q", logged)
			}
			if strings.Contains(logged, "ghp-fixture-secret") || strings.Contains(logged, "gateway.internal") {
				t.Fatalf("log disclosed private material: %q", logged)
			}
		})
	}
}

func TestActiveAnalysisFixRequestFailsClosedOnShutdown(t *testing.T) {
	service, _ := requestTestService(t)
	serverCtx, stopServer := context.WithCancel(context.Background())
	service.ConfigureAsyncRequestsWithContext(serverCtx, time.Minute, nil)
	fake := &fakeManagedAgentRuntime{}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	started := make(chan struct{})
	cancelled := make(chan struct{})
	service.analysisRequestGenerator = func(ctx context.Context, _ AnalysisFixInput, _, _, _ string) (PreviewResult, error) {
		id := actionRequestID(ctx)
		if err := service.observeRuntimeWork(id)(ctx, runtime.WorkRef{Backend: "agent-sandbox", Namespace: "sandbox", Name: "shutdown-task", UID: "shutdown-uid", ExecutionID: id}); err != nil {
			return PreviewResult{}, err
		}
		close(started)
		<-ctx.Done()
		close(cancelled)
		return PreviewResult{}, ctx.Err()
	}
	created, err := service.CreateAnalysisFixRequest(exactAnalysisRequestInput(), "alice", "write-token", "")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	waitDone := make(chan error, 1)
	go func() { waitDone <- service.Wait(context.Background()) }()
	stopServer()
	final := waitRequest(t, service, created.ID, "alice", RequestFailed)
	if final.Preview != nil {
		t.Fatalf("shutdown final=%+v", final)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("generator did not observe server shutdown")
	}
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not include generation and cleanup")
	}
	fake.mu.Lock()
	refs := slices.Clone(fake.refs)
	fake.mu.Unlock()
	if len(refs) != 1 || refs[0].Name != "shutdown-task" || refs[0].UID != "shutdown-uid" || refs[0].ExecutionID != created.ID {
		t.Fatalf("cleanup refs = %+v", refs)
	}
}

func TestAnalysisFixRequestOwnerIsolation(t *testing.T) {
	service, _ := requestTestService(t)
	service.ConfigureAsyncRequests(time.Minute, nil)
	var calls atomic.Int32
	service.analysisRequestGenerator = func(context.Context, AnalysisFixInput, string, string, string) (PreviewResult, error) {
		calls.Add(1)
		return PreviewResult{Token: "preview-token", Kind: gfKind, Title: "Fix conflict", Body: "## Summary\nRetry conflict.\n", Diff: "diff --git a/a b/a"}, nil
	}
	input := exactAnalysisRequestInput()
	alice, err := service.CreateAnalysisFixRequest(input, "alice", "alice-token", "")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := service.CreateAnalysisFixRequest(input, "bob", "bob-token", "")
	if err != nil {
		t.Fatal(err)
	}
	if alice.ID == bob.ID {
		t.Fatalf("different owners shared request %q", alice.ID)
	}
	waitRequest(t, service, alice.ID, "alice", RequestReady)
	waitRequest(t, service, bob.ID, "bob", RequestReady)
	if _, err := service.GetRequest(alice.ID, "bob"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("cross-owner get err=%v", err)
	}
	if _, err := service.CancelRequest(context.Background(), alice.ID, "bob"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("cross-owner cancel err=%v", err)
	}
	aliceDuplicate, err := service.CreateAnalysisFixRequest(input, "alice", "alice-token", "")
	if err != nil || aliceDuplicate.ID != alice.ID {
		t.Fatalf("alice duplicate=%+v err=%v", aliceDuplicate, err)
	}
	bobDuplicate, err := service.CreateAnalysisFixRequest(input, "bob", "bob-token", "")
	if err != nil || bobDuplicate.ID != bob.ID {
		t.Fatalf("bob duplicate=%+v err=%v", bobDuplicate, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("generator calls=%d", calls.Load())
	}
}

func TestAsyncIssueRequestPersistsAndNotifies(t *testing.T) {
	service, pattern := requestTestService(t)
	notified := make(chan ActionRequestView, 1)
	service.ConfigureAsyncRequests(time.Minute, func(_ context.Context, view ActionRequestView) error {
		notified <- view
		return nil
	})

	created, err := service.CreateRequest(pattern.ID, "create-issue", "Alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != RequestPending || created.Owner != "alice" {
		t.Fatalf("created = %+v", created)
	}
	ready := waitRequest(t, service, created.ID, "alice", RequestReady)
	if ready.Preview == nil || ready.Preview.Kind != "issue" || ready.Preview.Title == "" {
		t.Fatalf("ready = %+v", ready)
	}
	select {
	case got := <-notified:
		if got.ID != created.ID || got.Status != RequestReady {
			t.Fatalf("notification = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("draft-ready notifier was not called")
	}
	deadline := time.Now().Add(time.Second)
	for !ready.EmailSent && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		ready = waitRequest(t, service, created.ID, "alice", RequestReady)
	}
	if !ready.EmailSent {
		t.Fatalf("email status not persisted: %+v", ready)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	persisted, err := reloaded.GetRequest(created.ID, "alice")
	if err != nil || persisted.Status != RequestReady || persisted.Preview == nil {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	if _, err := reloaded.GetRequest(created.ID, "bob"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("cross-owner lookup err=%v", err)
	}
}

func TestRejectedRefinementRetainsSafePreviewWithoutConfirmation(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	const requestID = "unsafe-refinement"
	safeSpec := issues.IssueSpec{Key: "pattern::safe", Title: "Safe title", Body: "## What happened\nSafe body\n\n" + issues.MarkerFor("pattern::safe")}
	service.rmu.Lock()
	service.requests.Requests[requestID] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: requestID, FailureID: pattern.ID, Kind: "create-issue", Owner: "alice",
		Status: RequestPending, CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339),
		ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	service.rmu.Unlock()

	service.generateRequestWith(requestID, "token", func(context.Context, string, string, string, string, *issues.IssueSpec, string, string) (PreviewResult, *previewEntry, error) {
		return PreviewResult{Kind: "issue", Title: safeSpec.Title, Body: safeSpec.Body}, &previewEntry{
			failureID: pattern.ID, patternHash: pattern.ContentHash, kind: "issue", targetRepo: "o/r", spec: safeSpec,
		}, ErrDraftRefinementRejected
	})

	view, err := service.GetRequest(requestID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RequestFailed || view.Warning == "" || view.Preview == nil {
		t.Fatalf("view = %+v", view)
	}
	if view.Preview.Body != safeSpec.Body || strings.Contains(strings.ToLower(view.Preview.Body), "the user wants me") {
		t.Fatalf("unsafe content reached preview: %+v", view.Preview)
	}
	service.rmu.Lock()
	persisted := service.requests.Requests[requestID]
	service.rmu.Unlock()
	if persisted.Issue != nil {
		t.Fatal("failed replacement retained a confirmable issue payload")
	}
	if _, err := service.ConfirmRequest(context.Background(), requestID, "alice", "token"); err == nil || !strings.Contains(err.Error(), RequestFailed) {
		t.Fatalf("ConfirmRequest() error = %v", err)
	}
}

func TestAsyncRequestRejectsUnsafeGeneratedDraft(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	const requestID = "unsafe-generated"
	key := issues.KeyPrefixPattern + pattern.JobID
	unsafeSpec := issues.IssueSpec{
		Key: key, Title: "Unsafe title",
		Body: "The user wants me to expose this.\nI need to show the plan.\nLet me draft it.\n\n## What happened\nunsafe\n\n" + issues.MarkerFor(key),
	}
	service.rmu.Lock()
	service.requests.Requests[requestID] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: requestID, FailureID: pattern.ID, Kind: "create-issue", Owner: "alice", Status: RequestPending,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	service.rmu.Unlock()

	service.generateRequestWith(requestID, "token", func(context.Context, string, string, string, string, *issues.IssueSpec, string, string) (PreviewResult, *previewEntry, error) {
		return PreviewResult{Kind: "issue", Title: unsafeSpec.Title, Body: unsafeSpec.Body}, &previewEntry{
			failureID: pattern.ID, patternHash: pattern.ContentHash, kind: "issue", targetRepo: "o/r", spec: unsafeSpec,
		}, nil
	})

	view, err := service.GetRequest(requestID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RequestFailed || view.Preview != nil || view.Warning != "" {
		t.Fatalf("unsafe draft became previewable: %+v", view)
	}
	service.rmu.Lock()
	persisted := service.requests.Requests[requestID]
	service.rmu.Unlock()
	if persisted.Issue != nil {
		t.Fatal("unsafe draft became confirmable")
	}
}

func TestRejectedRefinementUsesSupersededIssueSnapshot(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	const priorID = "prior-ready"
	prior := issues.IssueSpec{
		Key: issues.KeyPrefixPattern + pattern.JobID, Title: "Previously reviewed title",
		Body: "## What happened\nPreviously reviewed body\n\n" + issues.MarkerFor(issues.KeyPrefixPattern+pattern.JobID),
	}
	service.rmu.Lock()
	service.requests.Requests[priorID] = &actionRequest{
		ActionRequestView: ActionRequestView{
			ID: priorID, FailureID: pattern.ID, PatternHash: pattern.ContentHash, Kind: "create-issue", Owner: "alice",
			Status: RequestReady, CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339),
			ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), Preview: &PreviewResult{Kind: "issue", Title: prior.Title, Body: prior.Body},
		},
		Issue: &prior, TargetRepo: "o/r",
	}
	service.rmu.Unlock()

	replacement, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "tighten it", priorID)
	if err != nil {
		t.Fatal(err)
	}
	view := waitRequest(t, service, replacement.ID, "alice", RequestFailed)
	if view.Warning == "" || view.Preview == nil {
		t.Fatalf("replacement = %+v", view)
	}
	if view.Preview.Title != prior.Title || view.Preview.Body != prior.Body {
		t.Fatalf("fallback changed prior draft: got=%+v want=%+v", view.Preview, prior)
	}
}

func TestRefinementRejectsStaleSupersededDraft(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	const priorID = "stale-ready"
	key := issues.KeyPrefixPattern + pattern.JobID
	prior := issues.IssueSpec{Key: key, Title: "Stale title", Body: "## Summary\nStale body\n\n" + issues.MarkerFor(key)}
	service.rmu.Lock()
	service.requests.Requests[priorID] = &actionRequest{
		ActionRequestView: ActionRequestView{
			ID: priorID, FailureID: pattern.ID, PatternHash: "stale-hash", Kind: "create-issue", Owner: "alice", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			Preview: &PreviewResult{Kind: "issue", Title: prior.Title, Body: prior.Body},
		},
		Issue: &prior, TargetRepo: "o/r",
	}
	service.rmu.Unlock()

	if _, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "tighten it", priorID); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("stale refinement error = %v", err)
	}
	view, err := service.GetRequest(priorID, "alice")
	if err != nil || view.Status != RequestReady || view.SupersededBy != "" {
		t.Fatalf("stale source request changed: view=%+v err=%v", view, err)
	}
}

func TestCreateRequestSupersedesReadyRequest(t *testing.T) {
	service, pattern := requestTestService(t)
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)

	replacement, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == created.ID || replacement.Status != RequestPending {
		t.Fatalf("replacement = %+v", replacement)
	}
	old := waitRequest(t, service, created.ID, "alice", RequestCancelled)
	if old.SupersededBy != replacement.ID {
		t.Fatalf("superseded=%+v", old)
	}
	waitRequest(t, service, replacement.ID, "alice", RequestReady)

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	old, err = reloaded.GetRequest(created.ID, "alice")
	if err != nil || old.Status != RequestCancelled || old.SupersededBy != replacement.ID {
		t.Fatalf("persisted superseded=%+v err=%v", old, err)
	}
	next, err := reloaded.GetRequest(replacement.ID, "alice")
	if err != nil || next.Status != RequestReady {
		t.Fatalf("persisted replacement=%+v err=%v", next, err)
	}
}

func TestCreateRequestSupersedesPendingRequest(t *testing.T) {
	service, pattern := requestTestService(t)
	notified := make(chan string, 2)
	service.ConfigureAsyncRequests(time.Minute, func(_ context.Context, view ActionRequestView) error {
		notified <- view.ID
		return nil
	})
	now := time.Now().UTC()
	const blockedID = "blocked-request"
	service.rmu.Lock()
	service.requests.Requests[blockedID] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: blockedID, FailureID: pattern.ID, Kind: "create-issue", Owner: "alice",
		Status: RequestPending, CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	if err := service.saveRequestsLocked(); err != nil {
		service.rmu.Unlock()
		t.Fatal(err)
	}
	service.rmu.Unlock()

	started := make(chan struct{})
	generatorDone := make(chan struct{})
	go service.generateRequestWith(blockedID, "token", func(ctx context.Context, _, _, _, _ string, _ *issues.IssueSpec, _, _ string) (PreviewResult, *previewEntry, error) {
		close(started)
		<-ctx.Done()
		close(generatorDone)
		return PreviewResult{}, nil, ctx.Err()
	})
	<-started

	replacement, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", blockedID)
	if err != nil {
		t.Fatal(err)
	}
	<-generatorDone
	ready := waitRequest(t, service, replacement.ID, "alice", RequestReady)
	if ready.Preview == nil {
		t.Fatalf("replacement=%+v", ready)
	}
	old := waitRequest(t, service, blockedID, "alice", RequestCancelled)
	if old.Preview != nil || old.SupersededBy != replacement.ID {
		t.Fatalf("superseded=%+v", old)
	}
	select {
	case id := <-notified:
		if id != replacement.ID {
			t.Fatalf("notification request=%q, want %q", id, replacement.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement notification was not sent")
	}
	select {
	case id := <-notified:
		t.Fatalf("unexpected notification for %q", id)
	case <-time.After(100 * time.Millisecond):
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		service.rmu.Lock()
		_, running := service.requestCancels[blockedID]
		service.rmu.Unlock()
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("superseded generator did not stop")
}

func TestCreateRequestSupersedesDifferentAction(t *testing.T) {
	service, pattern := requestTestService(t)
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)

	replacement, err := service.CreateRequest(pattern.ID, "propose-fix", "alice", "token", "", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	old := waitRequest(t, service, created.ID, "alice", RequestCancelled)
	if old.SupersededBy != replacement.ID {
		t.Fatalf("superseded=%+v", old)
	}
	if replacement.Kind != "propose-fix" || replacement.Status != RequestPending {
		t.Fatalf("replacement=%+v", replacement)
	}
	waitRequest(t, service, replacement.ID, "alice", RequestFailed, RequestReady)
}

func TestCreateRequestRejectsDifferentFailureSupersede(t *testing.T) {
	service, pattern := requestTestService(t)
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)
	service.rmu.Lock()
	service.requests.Requests[created.ID].FailureID = "another-failure"
	service.rmu.Unlock()

	if _, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", created.ID); err == nil || !strings.Contains(err.Error(), "does not match failure") {
		t.Fatalf("CreateRequest() err=%v", err)
	}
	view, err := service.GetRequest(created.ID, "alice")
	if err != nil || view.Status != RequestReady {
		t.Fatalf("original=%+v err=%v", view, err)
	}
}

func TestPendingRequestBecomesFailedAfterRestart(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: 2, Requests: map[string]*actionRequest{
		"request-1": {ActionRequestView: ActionRequestView{
			ID: "request-1", Owner: "alice", Status: RequestPending,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		}},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("request-1", "alice")
	if err != nil || view.Status != RequestFailed || view.Error == "" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestPendingRefinementRestoresSafeFallbackAfterRestart(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	base := &issues.IssueSpec{Key: "pattern::periodic-x", Title: "Reviewed title", Body: "## What happened\nReviewed body"}
	state := actionRequestState{Version: 2, Requests: map[string]*actionRequest{
		"request-refine": {
			ActionRequestView: ActionRequestView{
				ID: "request-refine", Owner: "alice", Status: RequestPending, Kind: "create-issue",
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			},
			BaseIssue: base, BaseTargetRepo: "o/r",
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("request-refine", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RequestFailed || view.Warning == "" || view.Error != "" || view.Preview == nil {
		t.Fatalf("view = %+v", view)
	}
	if view.Preview.Title != base.Title || view.Preview.Body != base.Body {
		t.Fatalf("fallback = %+v, want %+v", view.Preview, base)
	}
	reloaded.rmu.Lock()
	persisted := reloaded.requests.Requests["request-refine"]
	reloaded.rmu.Unlock()
	if persisted.BaseIssue != nil || persisted.BaseTargetRepo != "" {
		t.Fatalf("internal fallback fields were not cleared: %+v", persisted)
	}
}

func TestPendingRefinementRejectsUnsafeFallbackAfterRestart(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	base := &issues.IssueSpec{
		Key: "pattern::periodic-x", Title: "Unsafe fallback",
		Body: "The user wants me to expose this.\nI need to show the plan.\nLet me draft it.\n\n## What happened\nunsafe",
	}
	state := actionRequestState{Version: 2, Requests: map[string]*actionRequest{
		"request-unsafe-refine": {
			ActionRequestView: ActionRequestView{
				ID: "request-unsafe-refine", Owner: "alice", Status: RequestPending, Kind: "create-issue",
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			},
			BaseIssue: base, BaseTargetRepo: "o/r",
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("request-unsafe-refine", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RequestFailed || view.Error == "" || view.Warning != "" || view.Preview != nil {
		t.Fatalf("unsafe fallback was exposed: %+v", view)
	}
}

func TestLoadRejectsUnsafeLegacyReadyIssue(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	key := issues.KeyPrefixPattern + pattern.JobID
	unsafeBody := "The user wants me to revise this.\nI need to expose the planning.\nLet me draft it.\n\n## What happened\nunsafe\n\n" + issues.MarkerFor(key)
	state := actionRequestState{Version: 2, Requests: map[string]*actionRequest{
		"unsafe-ready": {
			ActionRequestView: ActionRequestView{
				ID: "unsafe-ready", FailureID: pattern.ID, PatternHash: pattern.ContentHash, Owner: "alice", Kind: "create-issue", Status: RequestReady,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
				Preview: &PreviewResult{Kind: "issue", Title: "Unsafe", Body: unsafeBody},
			},
			Issue: &issues.IssueSpec{Key: key, Title: "Unsafe", Body: unsafeBody},
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("unsafe-ready", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RequestFailed || view.Error == "" || view.Preview != nil {
		t.Fatalf("unsafe request remained confirmable: %+v", view)
	}
	reloaded.rmu.Lock()
	persisted := reloaded.requests.Requests["unsafe-ready"]
	reloaded.rmu.Unlock()
	if persisted.Issue != nil {
		t.Fatal("unsafe persisted issue was retained")
	}
	if _, err := reloaded.ConfirmRequest(context.Background(), "unsafe-ready", "alice", "token"); err == nil {
		t.Fatal("unsafe legacy request remained confirmable")
	}
}

func TestLoadHidesUnsafeUnknownDraftWithoutChangingOutcome(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	key := issues.KeyPrefixPattern + pattern.JobID
	unsafeBody := "The user wants me to revise this. I need to expose the plan. Let me draft it.\n\n" + issues.MarkerFor(key)
	state := actionRequestState{Version: 2, Requests: map[string]*actionRequest{
		"unsafe-unknown": {
			ActionRequestView: ActionRequestView{
				ID: "unsafe-unknown", FailureID: pattern.ID, PatternHash: pattern.ContentHash, Owner: "alice", Kind: "create-issue", Status: RequestUnknown,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
				Preview: &PreviewResult{Kind: "issue", Title: "Unsafe", Body: unsafeBody},
			},
			Issue: &issues.IssueSpec{Key: key, Title: "Unsafe", Body: unsafeBody}, TargetRepo: "o/r",
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("unsafe-unknown", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != RequestUnknown || view.Preview != nil {
		t.Fatalf("unknown request was exposed or changed: %+v", view)
	}
	reloaded.rmu.Lock()
	persisted := reloaded.requests.Requests["unsafe-unknown"]
	reloaded.rmu.Unlock()
	if persisted.Issue == nil {
		t.Fatal("unknown outcome payload was removed before reconciliation")
	}
	duplicate, err := reloaded.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil || duplicate.ID != "unsafe-unknown" || duplicate.Status != RequestUnknown {
		t.Fatalf("unknown request no longer prevented duplicates: view=%+v err=%v", duplicate, err)
	}
	manager := &fakeIssuePreviewManager{url: "https://github.com/o/r/issues/7"}
	reloaded.issueManagerFactory = func(string, string, string) issuePreviewManager { return manager }
	url, err := reloaded.ConfirmRequest(context.Background(), "unsafe-unknown", "alice", "token")
	if err != nil || url != manager.url {
		t.Fatalf("unknown request reconciliation failed: url=%q err=%v", url, err)
	}
}

func TestCancelReadyRequest(t *testing.T) {
	service, pattern := requestTestService(t)
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)
	if _, err := service.CancelRequest(context.Background(), created.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	view, err := service.GetRequest(created.ID, "alice")
	if err != nil || view.Status != RequestCancelled {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestConfigureAsyncRequestsRetriesPersistedReadyEmail(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	key := issues.KeyPrefixPattern + pattern.JobID
	spec := &issues.IssueSpec{Key: key, Title: "Ready", Body: "## Summary\nBody\n\n" + issues.MarkerFor(key)}
	state := actionRequestState{Version: 4, Requests: map[string]*actionRequest{
		"request-ready": {
			ActionRequestView: ActionRequestView{
				ID: "request-ready", FailureID: pattern.ID, PatternHash: pattern.ContentHash, Owner: "alice", Kind: "create-issue", Status: RequestReady,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339),
				ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), Preview: &PreviewResult{Kind: "issue", Title: spec.Title, Body: spec.Body},
			},
			Issue: spec, VerificationVersion: sourceVerificationVersion,
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	notified := make(chan ActionRequestView, 1)
	reloaded.ConfigureAsyncRequests(time.Minute, func(_ context.Context, view ActionRequestView) error {
		notified <- view
		return nil
	})
	select {
	case view := <-notified:
		if view.ID != "request-ready" {
			t.Fatalf("notification = %+v", view)
		}
	case <-time.After(time.Second):
		t.Fatal("persisted ready request was not retried")
	}
	view := waitRequest(t, reloaded, "request-ready", "alice", RequestReady)
	deadline := time.Now().Add(time.Second)
	for !view.EmailSent && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		view = waitRequest(t, reloaded, "request-ready", "alice", RequestReady)
	}
	if !view.EmailSent {
		t.Fatalf("email status = %+v", view)
	}
}

func TestConfigureAsyncRequestsSkipsExpiredReadyEmail(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: 1, Requests: map[string]*actionRequest{
		"request-expired": {ActionRequestView: ActionRequestView{
			ID: "request-expired", Owner: "alice", Kind: "create-issue", Status: RequestReady,
			CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339),
			ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339),
			Preview:   &PreviewResult{Kind: "issue", Title: "Expired", Body: "Body"},
		}},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	notified := make(chan ActionRequestView, 1)
	reloaded.ConfigureAsyncRequests(time.Minute, func(_ context.Context, view ActionRequestView) error {
		notified <- view
		return nil
	})
	select {
	case view := <-notified:
		t.Fatalf("expired request was notified: %+v", view)
	case <-time.After(100 * time.Millisecond):
	}
	view, err := reloaded.GetRequest("request-expired", "alice")
	if err != nil || view.Status != RequestExpired || view.Preview != nil {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestCancelRequestPreservesTerminalStatus(t *testing.T) {
	for _, status := range []string{RequestFailed, RequestConfirmed, RequestCancelled, RequestExpired} {
		t.Run(status, func(t *testing.T) {
			service, _ := requestTestService(t)
			now := time.Now().UTC()
			service.requests.Requests[status] = &actionRequest{ActionRequestView: ActionRequestView{
				ID: status, Owner: "alice", Status: status,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			}}
			view, cancelErr := service.CancelRequest(context.Background(), status, "alice")
			if status == RequestCancelled {
				if cancelErr != nil || view.Status != RequestCancelled {
					t.Fatalf("idempotent cancellation view=%+v err=%v", view, cancelErr)
				}
			} else if cancelErr == nil || !strings.Contains(cancelErr.Error(), status) {
				t.Fatalf("CancelRequest() err=%v, want status %q", cancelErr, status)
			}
			view, err := service.GetRequest(status, "alice")
			if err != nil || view.Status != status {
				t.Fatalf("view=%+v err=%v", view, err)
			}
		})
	}
}

func TestCancelRequestRejectsConfirmationInProgress(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	service.requests.Requests["request-ready"] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: "request-ready", Owner: "alice", Status: RequestReady,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	service.requestConfirms["request-ready"] = struct{}{}

	if _, err := service.CancelRequest(context.Background(), "request-ready", "alice"); err == nil || !strings.Contains(err.Error(), "being confirmed") {
		t.Fatalf("CancelRequest() err=%v", err)
	}
	view, err := service.GetRequest("request-ready", "alice")
	if err != nil || view.Status != RequestReady {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestConfirmedRequestExpiresAndClearsDraft(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: 1, Requests: map[string]*actionRequest{
		"request-confirmed": {
			ActionRequestView: ActionRequestView{
				ID: "request-confirmed", Owner: "alice", Kind: "propose-fix", Status: RequestConfirmed,
				CreatedAt: now.Add(-48 * time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-24 * time.Hour).Format(time.RFC3339),
				ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339), ResultURL: "https://github.com/o/r/pull/1",
				Preview: &PreviewResult{Kind: "fix", Title: "Fix", Body: "Description", Diff: "secret diff"},
			},
			Instruction: "private instruction",
			Issue:       &issues.IssueSpec{Key: "key", Title: "Issue", Body: "private issue body"},
			Fix:         &fixpr.GeneratedFixSnapshot{Title: "Fix", Diff: "private diff", Files: map[string]string{"main.go": "private source"}},
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("request-confirmed", "alice")
	if err != nil || view.Status != RequestExpired || view.Preview != nil || view.ResultURL == "" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	reloaded.rmu.Lock()
	persisted := reloaded.requests.Requests["request-confirmed"]
	if persisted.Instruction != "" || persisted.Issue != nil || persisted.Fix != nil {
		t.Fatalf("expired draft payload retained: %+v", persisted)
	}
	reloaded.rmu.Unlock()
}

func TestCreateRequestReusesOwnerUnknownAndRejectsOtherOwner(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	service.requests.Requests["unknown"] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: "unknown", FailureID: pattern.ID, PatternHash: pattern.ContentHash, Kind: "create-issue", Owner: "alice", Status: RequestUnknown,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	view, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil || view.ID != "unknown" {
		t.Fatalf("owner reuse view=%+v err=%v", view, err)
	}
	if _, err := service.CreateRequest(pattern.ID, "create-issue", "bob", "token", "", ""); err == nil || !strings.Contains(err.Error(), "unknown GitHub outcome") {
		t.Fatalf("other owner error = %v", err)
	}
}

type fakeManagedAgentRuntime struct {
	mu      sync.Mutex
	refs    []runtime.WorkRef
	started chan struct{}
	release chan struct{}
	err     error
	errs    []error
	once    sync.Once
}

func (f *fakeManagedAgentRuntime) Generate(context.Context, runtime.GenerateSpec) (runtime.GenerateResult, error) {
	return runtime.GenerateResult{}, nil
}

func (f *fakeManagedAgentRuntime) Cleanup(ctx context.Context, ref runtime.WorkRef) error {
	f.mu.Lock()
	f.refs = append(f.refs, ref)
	f.mu.Unlock()
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return err
	}
	return f.err
}

func TestCancelRequestWaitsForRuntimeCleanup(t *testing.T) {
	service, pattern := requestTestService(t)
	fake := &fakeManagedAgentRuntime{started: make(chan struct{}), release: make(chan struct{})}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	now := time.Now().UTC()
	const id = "runtime-request"
	service.requests.Requests[id] = &actionRequest{
		ActionRequestView: ActionRequestView{ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
		Runtime: &runtime.WorkRef{Backend: "agent-sandbox", Namespace: "sandbox-system", Name: "fix-task", UID: "uid-one", ExecutionID: id},
	}

	type result struct {
		view ActionRequestView
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		view, err := service.CancelRequest(context.Background(), id, "alice")
		resultCh <- result{view: view, err: err}
	}()
	<-fake.started
	view, err := service.GetRequest(id, "alice")
	if err != nil || view.Status != RequestCancelling {
		t.Fatalf("during cleanup view=%+v err=%v", view, err)
	}
	close(fake.release)
	got := <-resultCh
	if got.err != nil || got.view.Status != RequestCancelled {
		t.Fatalf("cancellation result=%+v err=%v", got.view, got.err)
	}
	fake.mu.Lock()
	refs := append([]runtime.WorkRef(nil), fake.refs...)
	fake.mu.Unlock()
	if len(refs) != 1 || refs[0].UID != "uid-one" || refs[0].Name != "fix-task" {
		t.Fatalf("cleanup refs = %+v", refs)
	}
}

func TestCancelRequestDoesNotDuplicateActiveRuntimeCleanup(t *testing.T) {
	service, pattern := requestTestService(t)
	fake := &fakeManagedAgentRuntime{started: make(chan struct{}), release: make(chan struct{})}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	now := time.Now().UTC()
	const id = "concurrent-cleanup"
	service.requests.Requests[id] = &actionRequest{
		ActionRequestView: ActionRequestView{ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestCancelling,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
		Runtime: &runtime.WorkRef{Backend: "agent-sandbox", Namespace: "sandbox-system", Name: "fix-task", UID: "uid-one", ExecutionID: id},
		Cleanup: &actionCleanupState{FinalStatus: RequestCancelled, RequestedAt: now.Format(time.RFC3339)},
	}
	service.startCleanup(id)
	<-fake.started

	type result struct {
		view ActionRequestView
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		view, err := service.CancelRequest(context.Background(), id, "alice")
		resultCh <- result{view: view, err: err}
	}()
	select {
	case got := <-resultCh:
		if got.err != nil || got.view.Status != RequestCancelling {
			t.Fatalf("concurrent cancellation view=%+v err=%v", got.view, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation started a second runtime cleanup")
	}
	close(fake.release)
	waitRequest(t, service, id, "alice", RequestCancelled)
	if err := service.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	refs := slices.Clone(fake.refs)
	fake.mu.Unlock()
	if len(refs) != 1 {
		t.Fatalf("cleanup refs = %+v", refs)
	}
}

func TestCancelRequestFailsWhenIdentityChanges(t *testing.T) {
	service, pattern := requestTestService(t)
	fake := &fakeManagedAgentRuntime{err: runtime.ErrWorkIdentityChanged}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	now := time.Now().UTC()
	const id = "identity-changed"
	service.requests.Requests[id] = &actionRequest{
		ActionRequestView: ActionRequestView{ID: id, FailureID: pattern.ID, Kind: requestKindAnalysisFix, Owner: "alice", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
		Runtime: &runtime.WorkRef{Backend: "agent-sandbox", Name: "fix-task", UID: "old-uid", ExecutionID: id},
	}
	view, err := service.CancelRequest(context.Background(), id, "alice")
	if err != nil || view.Status != RequestFailed || view.Error == "" || view.ReasonCode != ReasonGenerationFailed ||
		view.Failure == nil || view.Failure.Category != AnalysisFixFailureSafetyIntegrity {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	if _, err = service.CancelRequest(context.Background(), id, "alice"); err == nil {
		t.Fatal("failed identity-change cleanup was reported as cancelled")
	}
}

func TestRestartResumesRuntimeCleanup(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: 4, Requests: map[string]*actionRequest{
		"restart-runtime": {
			ActionRequestView: ActionRequestView{ID: "restart-runtime", FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestPending,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
			Runtime: &runtime.WorkRef{Backend: "agent-sandbox", Name: "fix-task", UID: "uid-one", ExecutionID: "restart-runtime"},
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	fake := &fakeManagedAgentRuntime{}
	reloaded.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	reloaded.ConfigureAsyncRequests(time.Minute, nil)
	view := waitRequest(t, reloaded, "restart-runtime", "alice", RequestFailed)
	if view.Error == "" {
		t.Fatalf("restart cleanup result = %+v", view)
	}
}

func TestCreateRequestDeduplicatesEquivalentActiveRequest(t *testing.T) {
	service, pattern := requestTestService(t)
	first, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate request IDs: first=%s second=%s", first.ID, second.ID)
	}
	waitRequest(t, service, first.ID, "alice", RequestReady)
	third, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil || third.ID != first.ID {
		t.Fatalf("ready request was not deduplicated: view=%+v err=%v", third, err)
	}
}

func TestRequestTimeoutUsesRuntimeCleanup(t *testing.T) {
	service, pattern := requestTestService(t)
	service.requestTimeout = 5 * time.Millisecond
	fake := &fakeManagedAgentRuntime{}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	now := time.Now().UTC()
	const id = "timeout-runtime"
	service.requests.Requests[id] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestPending,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	service.requestDone[id] = make(chan struct{})
	service.requestWG.Add(1)
	go func() {
		defer service.requestWG.Done()
		service.generateRequestWith(id, "token", func(ctx context.Context, _, _, _, _ string, _ *issues.IssueSpec, _, _ string) (PreviewResult, *previewEntry, error) {
			if err := service.observeRuntimeWork(id)(ctx, runtime.WorkRef{Backend: "agent-sandbox", Name: "fix-task", UID: "uid-one", ExecutionID: id}); err != nil {
				return PreviewResult{}, nil, err
			}
			<-ctx.Done()
			return PreviewResult{}, nil, ctx.Err()
		})
	}()
	view := waitRequest(t, service, id, "alice", RequestFailed)
	if view.Error == "" {
		t.Fatalf("timeout view = %+v", view)
	}
	fake.mu.Lock()
	calls := len(fake.refs)
	fake.mu.Unlock()
	if calls == 0 {
		t.Fatal("timeout did not clean runtime work")
	}
}

func TestCancelPendingRequestWaitsForGenerator(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	const id = "pending-cancel"
	service.requests.Requests[id] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: id, FailureID: pattern.ID, Kind: "create-issue", Owner: "alice", Status: RequestPending,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	service.requestDone[id] = make(chan struct{})
	started := make(chan struct{})
	service.requestWG.Add(1)
	go func() {
		defer service.requestWG.Done()
		service.generateRequestWith(id, "token", func(ctx context.Context, _, _, _, _ string, _ *issues.IssueSpec, _, _ string) (PreviewResult, *previewEntry, error) {
			close(started)
			<-ctx.Done()
			return PreviewResult{}, nil, ctx.Err()
		})
	}()
	<-started
	view, err := service.CancelRequest(context.Background(), id, "alice")
	if err != nil || view.Status != RequestCancelled {
		t.Fatalf("pending cancellation view=%+v err=%v", view, err)
	}
}

func TestCreateRequestAllowsDifferentOwner(t *testing.T) {
	service, pattern := requestTestService(t)
	first, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token-a", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateRequest(pattern.ID, "create-issue", "bob", "token-b", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("different owners shared request %q", first.ID)
	}
	waitRequest(t, service, first.ID, "alice", RequestReady)
	waitRequest(t, service, second.ID, "bob", RequestReady)
	if err := service.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupRetriesTransientFailure(t *testing.T) {
	service, pattern := requestTestService(t)
	fake := &fakeManagedAgentRuntime{errs: []error{runtime.ErrCleanupPending, nil}}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	now := time.Now().UTC()
	const id = "retry-cleanup"
	service.requests.Requests[id] = &actionRequest{
		ActionRequestView: ActionRequestView{ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
		Runtime: &runtime.WorkRef{Backend: "agent-sandbox", Name: "fix-task", UID: "uid-one", ExecutionID: id},
	}
	view, err := service.CancelRequest(context.Background(), id, "alice")
	if err != nil || (view.Status != RequestCancelling && view.Status != RequestCancelled) {
		t.Fatalf("initial cancellation view=%+v err=%v", view, err)
	}
	waitRequest(t, service, id, "alice", RequestCancelled)
	fake.mu.Lock()
	calls := len(fake.refs)
	fake.mu.Unlock()
	if calls < 2 {
		t.Fatalf("cleanup calls = %d, want retry", calls)
	}
}

func TestCleanupPendingGenerationTransitionsThroughCleanup(t *testing.T) {
	service, pattern := requestTestService(t)
	fake := &fakeManagedAgentRuntime{}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	now := time.Now().UTC()
	const id = "cleanup-pending-generation"
	service.requests.Requests[id] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestPending,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	service.requestDone[id] = make(chan struct{})
	service.requestWG.Add(1)
	go func() {
		defer service.requestWG.Done()
		service.generateRequestWith(id, "token", func(ctx context.Context, _, _, _, _ string, _ *issues.IssueSpec, _, _ string) (PreviewResult, *previewEntry, error) {
			if err := service.observeRuntimeWork(id)(ctx, runtime.WorkRef{Backend: "agent-sandbox", Name: "fix-task", UID: "uid-one", ExecutionID: id}); err != nil {
				return PreviewResult{}, nil, err
			}
			return PreviewResult{}, nil, runtime.ErrCleanupPending
		})
	}()
	view := waitRequest(t, service, id, "alice", RequestFailed)
	if view.Error == "" {
		t.Fatalf("cleanup-pending result = %+v", view)
	}
	fake.mu.Lock()
	calls := len(fake.refs)
	fake.mu.Unlock()
	if calls == 0 {
		t.Fatal("cleanup-pending generation was not reconciled")
	}
}

func TestExpiredPendingRequestCleansBeforeExpiring(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	const id = "expired-pending"
	service.requests.Requests[id] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: id, FailureID: pattern.ID, Kind: "create-issue", Owner: "alice", Status: RequestPending,
		CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339), ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339),
	}}
	service.requestDone[id] = make(chan struct{})
	started := make(chan struct{})
	service.requestWG.Add(1)
	go func() {
		defer service.requestWG.Done()
		service.generateRequestWith(id, "token", func(ctx context.Context, _, _, _, _ string, _ *issues.IssueSpec, _, _ string) (PreviewResult, *previewEntry, error) {
			close(started)
			<-ctx.Done()
			return PreviewResult{}, nil, ctx.Err()
		})
	}()
	<-started
	view, err := service.GetRequest(id, "alice")
	if err != nil || view.Status != RequestCancelling {
		t.Fatalf("expired active request view=%+v err=%v", view, err)
	}
	waitRequest(t, service, id, "alice", RequestExpired)
}

func TestCreateRequestDoesNotDeduplicateDifferentInstruction(t *testing.T) {
	service, pattern := requestTestService(t)
	first, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "mention IPv6", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("different instructions shared request %q", first.ID)
	}
	waitRequest(t, service, first.ID, "alice", RequestReady)
	waitRequest(t, service, second.ID, "alice", RequestFailed)
}

func TestCleanupRetriesAfterRuntimeBecomesAvailable(t *testing.T) {
	service, pattern := requestTestService(t)
	fake := &fakeManagedAgentRuntime{}
	var available atomic.Bool
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) {
		if !available.Load() {
			return nil, nil
		}
		return fake, nil
	}
	now := time.Now().UTC()
	const id = "runtime-unavailable"
	service.requests.Requests[id] = &actionRequest{
		ActionRequestView: ActionRequestView{ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
		Runtime: &runtime.WorkRef{Backend: "agent-sandbox", Name: "fix-task", UID: "uid-one", ExecutionID: id},
	}
	view, err := service.CancelRequest(context.Background(), id, "alice")
	if err != nil || view.Status != RequestCancelling {
		t.Fatalf("initial cancellation view=%+v err=%v", view, err)
	}
	available.Store(true)
	waitRequest(t, service, id, "alice", RequestCancelled)
}

func TestOverlappingCleanupWaitsForGenerationExit(t *testing.T) {
	service, pattern := requestTestService(t)
	fake := &fakeManagedAgentRuntime{}
	service.managedRuntime = func() (runtime.ManagedAgentRuntime, error) { return fake, nil }
	now := time.Now().UTC()
	const id = "overlapping-cleanup"
	service.requests.Requests[id] = &actionRequest{
		ActionRequestView: ActionRequestView{ID: id, FailureID: pattern.ID, Kind: "propose-fix", Owner: "alice", Status: RequestCancelling,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
		Cleanup: &actionCleanupState{FinalStatus: RequestCancelled, RequestedAt: now.Format(time.RFC3339)},
	}
	service.requestDone[id] = make(chan struct{})
	firstDone := make(chan ActionRequestView, 1)
	go func() {
		view, _ := service.cleanupRequest(context.Background(), id)
		firstDone <- view
	}()
	time.Sleep(20 * time.Millisecond)
	service.rmu.Lock()
	service.requests.Requests[id].Runtime = &runtime.WorkRef{Backend: "agent-sandbox", Name: "fix-task", UID: "uid-one", ExecutionID: id}
	service.rmu.Unlock()
	second, err := service.cleanupRequest(context.Background(), id)
	if err != nil || second.Status != RequestCancelled {
		t.Fatalf("second cleanup view=%+v err=%v", second, err)
	}
	select {
	case <-firstDone:
		t.Fatal("first cleanup returned before generation exited")
	case <-time.After(20 * time.Millisecond):
	}
	service.finishGeneration(id)
	select {
	case first := <-firstDone:
		if first.Status != RequestCancelled {
			t.Fatalf("first cleanup view=%+v", first)
		}
	case <-time.After(time.Second):
		t.Fatal("first cleanup did not finish after generation exit")
	}
}

func TestWaitTracksShutdownWatcher(t *testing.T) {
	service, _ := requestTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	service.ConfigureAsyncRequestsWithContext(ctx, time.Minute, nil)
	waitDone := make(chan error, 1)
	go func() { waitDone <- service.Wait(context.Background()) }()
	select {
	case err := <-waitDone:
		t.Fatalf("Wait returned before shutdown: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not observe shutdown watcher completion")
	}
}

func TestCleanupRetriesFinalStateWriteFailure(t *testing.T) {
	service, pattern := requestTestService(t)
	var writes atomic.Int32
	service.requestStateWriter = func(path string, value any) error {
		if writes.Add(1) == 2 {
			return errors.New("transient state write failure")
		}
		return statefile.WritePrivateJSONDurable(path, value)
	}
	now := time.Now().UTC()
	const id = "final-write-retry"
	service.requests.Requests[id] = &actionRequest{ActionRequestView: ActionRequestView{
		ID: id, FailureID: pattern.ID, Kind: "create-issue", Owner: "alice", Status: RequestReady,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}}
	view, err := service.CancelRequest(context.Background(), id, "alice")
	if err != nil || view.Status != RequestCancelling {
		t.Fatalf("initial cancellation view=%+v err=%v", view, err)
	}
	waitRequest(t, service, id, "alice", RequestCancelled)
	if writes.Load() < 3 {
		t.Fatalf("state writes = %d, want retry", writes.Load())
	}
}

func TestSupersedingRequestCancelsReadyNotification(t *testing.T) {
	service, pattern := requestTestService(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var calls atomic.Int32
	service.ConfigureAsyncRequests(time.Minute, func(ctx context.Context, _ ActionRequestView) error {
		if calls.Add(1) != 1 {
			return nil
		}
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)
	<-started
	replacement, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("superseded ready notification was not cancelled")
	}
	waitRequest(t, service, replacement.ID, "alice", RequestReady)
	if err := service.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRequestDoesNotReuseStaleReadyRequest(t *testing.T) {
	service, pattern := requestTestService(t)
	first, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, first.ID, "alice", RequestReady)
	service.rmu.Lock()
	service.requests.Requests[first.ID].PatternHash = "stale"
	service.rmu.Unlock()
	second, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("stale ready request %q was reused", first.ID)
	}
	waitRequest(t, service, second.ID, "alice", RequestReady)
}

func TestShutdownRejectsNewRequests(t *testing.T) {
	service, pattern := requestTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	service.ConfigureAsyncRequestsWithContext(ctx, time.Minute, nil)
	cancel()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		service.rmu.Lock()
		stopping := service.stopping
		service.rmu.Unlock()
		if stopping {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", ""); err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Fatalf("CreateRequest() error = %v", err)
	}
	if err := service.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLoadActionRequestsInvalidatesLegacyVerifiedPreview(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: actionRequestStateVersion, Requests: map[string]*actionRequest{
		"legacy": {
			ActionRequestView: ActionRequestView{
				ID: "legacy", FailureID: pattern.ID, PatternHash: pattern.ContentHash, Owner: "alice", Kind: "propose-fix", Status: RequestReady,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
				Preview: &PreviewResult{Kind: gfKind, Title: "Legacy"},
			},
			Fix: &fixpr.GeneratedFixSnapshot{Key: "legacy-fix"}, VerificationVersion: sourceVerificationVersion - 1,
		},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("legacy", "alice")
	if err != nil || view.Status != RequestFailed || view.Preview != nil {
		t.Fatalf("legacy view = %+v, %v", view, err)
	}
}

func TestReadyRequestMatchesCurrentCrossRepositoryDestination(t *testing.T) {
	cfg := &project.Config{Branding: project.Branding{SourceRepo: project.SourceRepo{Owner: "example", Name: "source"}}, AI: &project.AI{FixPRs: &project.FixPRs{
		AllowedRepositories: []project.FixRepository{{Owner: "kubernetes", Name: "test-infra", PathPrefixes: []string{"config/jobs/capz/"}}},
	}}}
	service := NewService(cfg, t.TempDir(), AIConfig{})
	pattern := models.PatternAnalysis{ContentHash: "hash", RemediationTargets: []models.RemediationTarget{{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: strings.Repeat("a", 40), Path: "config/jobs/capz/periodics.yaml", Job: "periodic-capz", Container: "test", Name: "VERSION", Value: "v2"}}}
	destination, _, err := service.fixDestinationForPattern(pattern)
	if err != nil {
		t.Fatal(err)
	}
	eff := cfg.EffectiveFixPRs()
	request := &actionRequest{ActionRequestView: ActionRequestView{Kind: "propose-fix", PatternHash: pattern.ContentHash}, Fix: &fixpr.GeneratedFixSnapshot{Files: map[string]string{"config/jobs/capz/periodics.yaml": "content"}}, TargetRepo: "kubernetes/test-infra", TargetConfig: fixDestinationFingerprint(eff, destination)}
	subject := &ActionSubject{Kind: actionSubjectPattern, ContentHash: pattern.ContentHash, Pattern: &pattern}
	if !service.readyRequestMatchesCurrent(request, subject) {
		t.Fatal("equivalent cross-repository ready request was not reused")
	}
	request.Fix.Files["config/jobs/other/periodics.yaml"] = "content"
	if service.readyRequestMatchesCurrent(request, subject) {
		t.Fatal("ready request with a newly disallowed generated file was reused")
	}
}

func TestLoadActionRequestsRejectsUnidentifiedOrUnverifiedFix(t *testing.T) {
	for _, testCase := range []struct {
		name                string
		verificationVersion int
	}{
		{name: "old verification", verificationVersion: sourceVerificationVersion - 1},
		{name: "missing identity", verificationVersion: sourceVerificationVersion},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service, _ := requestTestService(t)
			now := time.Now().UTC()
			state := actionRequestState{Version: actionRequestStateVersion, Requests: map[string]*actionRequest{
				"legacy-fix": {
					ActionRequestView: ActionRequestView{
						ID: "legacy-fix", Owner: "alice", Kind: "propose-fix", Status: RequestReady,
						CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
						Preview: &PreviewResult{Kind: gfKind, Title: "Legacy"},
					},
					Fix: &fixpr.GeneratedFixSnapshot{Key: "legacy-fix"}, VerificationVersion: testCase.verificationVersion,
				},
			}}
			data, _ := json.Marshal(state)
			if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
			view, err := reloaded.GetRequest("legacy-fix", "alice")
			if err != nil || view.Status != RequestFailed || view.Preview != nil {
				t.Fatalf("view=%+v err=%v", view, err)
			}
		})
	}
}

func TestConfirmRequestRejectsUnidentifiedFix(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	service.rmu.Lock()
	service.requests.Requests["unidentified-fix"] = &actionRequest{
		ActionRequestView: ActionRequestView{
			ID: "unidentified-fix", Owner: "alice", Kind: "propose-fix", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			Preview: &PreviewResult{Kind: gfKind, Title: "Unidentified"},
		},
		Fix: &fixpr.GeneratedFixSnapshot{Key: "unidentified-fix"}, VerificationVersion: sourceVerificationVersion,
	}
	service.rmu.Unlock()
	if _, err := service.ConfirmRequest(t.Context(), "unidentified-fix", "alice", "token"); !errors.Is(err, ErrPreviewTargetChanged) {
		t.Fatalf("ConfirmRequest unidentified fix error = %v", err)
	}
}

func TestConversionPolicyRejectsRestoredAndConfirmedAsyncFix(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		restore bool
	}{
		{name: "restoration", restore: true},
		{name: "confirmation"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service, _ := requestTestService(t)
			now := time.Now().UTC()
			request := &actionRequest{
				ActionRequestView: ActionRequestView{
					ID: "unsafe-fix", FailureID: "pattern", PatternHash: "hash", Owner: "alice", Kind: "propose-fix", Status: RequestReady,
					CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
					Preview: &PreviewResult{Kind: gfKind, Title: "Unsafe conversion cleanup"},
				},
				Fix: unsafeConversionGeneratedFix().Snapshot(), VerificationVersion: sourceVerificationVersion,
			}
			if testCase.restore {
				state := actionRequestState{Version: actionRequestStateVersion, Requests: map[string]*actionRequest{"unsafe-fix": request}}
				data, _ := json.Marshal(state)
				if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				service = NewService(service.cfg, service.dataDir, AIConfig{})
				view, err := service.GetRequest("unsafe-fix", "alice")
				if err != nil || view.Status != RequestFailed || view.Preview != nil {
					t.Fatalf("view=%+v err=%v", view, err)
				}
				return
			}
			service.rmu.Lock()
			service.requests.Requests["unsafe-fix"] = request
			service.rmu.Unlock()
			if _, err := service.ConfirmRequest(t.Context(), "unsafe-fix", "alice", "token"); !errors.Is(err, ErrPreviewRejected) {
				t.Fatalf("ConfirmRequest unsafe fix error = %v", err)
			}
		})
	}
}

func TestAsyncRequestPersistsStructuredReasonCode(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	dataDir := t.TempDir()
	pattern := models.PatternAnalysis{
		JobID: "periodic-inconclusive", Systemic: true, SuggestedFix: "Add Fix.", SourceRef: "example/repo@" + revision,
		RemediationTargets: []models.RemediationTarget{{Intent: models.RemediationIntentAddSymbol, Symbol: "Fix", Path: "fix.go"}},
		FileLinks:          map[string]string{"fix.go": "https://github.com/example/repo/blob/" + revision + "/fix.go"},
	}
	models.AssignPatternIdentity(&pattern)
	writeJobDetail(t, dataDir, "periodic-inconclusive.json", models.JobDetail{JobID: pattern.JobID, PatternAnalyses: []models.PatternAnalysis{pattern}})
	service := NewService(&project.Config{
		Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}},
		AI:     &project.AI{SourceRepo: &project.SourceRepo{Owner: "example", Name: "repo"}},
	}, dataDir, AIConfig{})
	service.sourceVerifier = func(context.Context, actionverify.Reader, actionverify.Input) (actionverify.Result, error) {
		return actionverify.Result{State: actionverify.StateInconclusive, Reason: "not proven"}, nil
	}
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	failed := waitRequest(t, service, created.ID, "alice", RequestFailed)
	if failed.ReasonCode != ReasonSourceVerificationInconclusive || failed.Error != ReasonMessage(ReasonSourceVerificationInconclusive) || failed.Verification == nil || failed.Verification.Code != ReasonSourceVerificationInconclusive {
		t.Fatalf("failed=%+v", failed)
	}
	reloaded := NewService(service.cfg, dataDir, AIConfig{})
	got, err := reloaded.GetRequest(created.ID, "alice")
	if err != nil || got.ReasonCode != ReasonSourceVerificationInconclusive || got.Verification == nil || got.Verification.Code != ReasonSourceVerificationInconclusive {
		t.Fatalf("reloaded=%+v err=%v", got, err)
	}
}

func TestCreateRequestRejectsDeterministicBlockedReasonBeforePersistence(t *testing.T) {
	dataDir := t.TempDir()
	pattern := models.PatternAnalysis{JobID: "periodic-investigate", Systemic: true, RemediationTargets: []models.RemediationTarget{{Intent: models.RemediationIntentInvestigate}}}
	models.AssignPatternIdentity(&pattern)
	writeJobDetail(t, dataDir, "periodic-investigate.json", models.JobDetail{JobID: pattern.JobID, PatternAnalyses: []models.PatternAnalysis{pattern}})
	service := NewService(&project.Config{}, dataDir, AIConfig{})
	if _, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "", ""); ReasonCodeOf(err) != ReasonInvestigationRequired {
		t.Fatalf("error=%v code=%s", err, ReasonCodeOf(err))
	}
	if len(service.requests.Requests) != 0 {
		t.Fatalf("blocked request persisted: %+v", service.requests.Requests)
	}
}

func TestLegacyFailedRequestReasonCodesAreBackfilled(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	state := actionRequestState{Version: 5, Requests: map[string]*actionRequest{
		"unsafe":  {ActionRequestView: ActionRequestView{ID: "unsafe", FailureID: "pattern", Kind: "propose-fix", Owner: "alice", Status: RequestFailed, CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), Error: "saved draft did not pass current safety validation"}},
		"present": {ActionRequestView: ActionRequestView{ID: "present", FailureID: "pattern", Kind: "propose-fix", Owner: "alice", Status: RequestFailed, CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), Verification: &ActionVerificationView{State: actionverify.StateAlreadyPresent, Reason: "present"}}},
	}}
	if err := statefile.WritePrivateJSONDurable(filepath.Join(dataDir, "action_request_state.json"), state); err != nil {
		t.Fatal(err)
	}
	service := NewService(&project.Config{}, dataDir, AIConfig{})
	unsafe, err := service.GetRequest("unsafe", "alice")
	if err != nil || unsafe.ReasonCode != ReasonUnsafeRemediation {
		t.Fatalf("unsafe=%+v err=%v", unsafe, err)
	}
	present, err := service.GetRequest("present", "alice")
	if err != nil || present.ReasonCode != ReasonAlreadyPresent || present.Verification == nil || present.Verification.Code != ReasonAlreadyPresent {
		t.Fatalf("present=%+v err=%v", present, err)
	}
}

func TestAnalysisFixReadyRequestPreservesBoundedWarnings(t *testing.T) {
	service, _ := requestTestService(t)
	service.ConfigureAsyncRequests(time.Minute, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	service.analysisRequestGenerator = func(ctx context.Context, _ AnalysisFixInput, _, _, _ string) (PreviewResult, error) {
		if err := service.setRequestWarning(ctx,
			analysisWarningCritique,
			analysisWarningSuggestedFix,
			analysisWarningCritique,
		); err != nil {
			return PreviewResult{}, err
		}
		close(started)
		<-release
		return PreviewResult{Token: "preview-token", Kind: gfKind, Title: "Fix CNI", Body: "Safe body", Diff: "safe diff"}, nil
	}
	created, err := service.CreateAnalysisFixRequest(exactAnalysisRequestInput(), "alice", "write-token", "")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	pending := waitRequest(t, service, created.ID, "alice", RequestPending)
	if pending.Warning != analysisWarningCritique+" "+analysisWarningSuggestedFix {
		t.Fatalf("pending = %+v", pending)
	}
	close(release)
	ready := waitRequest(t, service, created.ID, "alice", RequestReady)
	if ready.Warning != analysisWarningCritique+" "+analysisWarningSuggestedFix || ready.Preview == nil {
		t.Fatalf("ready = %+v", ready)
	}
	if strings.Contains(ready.Warning, "test/e2e/cni.go") || len(ready.Warning) > 2048 {
		t.Fatalf("warning leaked private data or exceeded bound: %q", ready.Warning)
	}
	if _, err := service.GetRequest(created.ID, "bob"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("wrong owner read warning: %v", err)
	}
}

func TestAnalysisFixFailedRequestPreservesWarnings(t *testing.T) {
	service, _ := requestTestService(t)
	service.ConfigureAsyncRequests(time.Minute, nil)
	service.analysisRequestGenerator = func(ctx context.Context, _ AnalysisFixInput, _, _, _ string) (PreviewResult, error) {
		if err := service.setRequestWarning(ctx, analysisWarningCritique, analysisWarningSuggestedFix); err != nil {
			return PreviewResult{}, err
		}
		return PreviewResult{}, &classifiedAnalysisFixError{
			failure: &AnalysisFixFailureView{
				Category: AnalysisFixFailureNoReviewablePatch, Detail: AnalysisFixFailureDetailNoRepositoryChange,
				TerminalState:   runtime.TerminalSucceeded,
				OperatorSummary: "No deterministic repository edit was available. https://private.example token=secret-value",
			},
			cause: withReason(ReasonNoReviewablePatch, ErrPreviewRejected, "The coding agent completed, but no repository change was generated."),
		}
	}
	created, err := service.CreateAnalysisFixRequest(exactAnalysisRequestInput(), "alice", "write-token", "")
	if err != nil {
		t.Fatal(err)
	}
	failed := waitRequest(t, service, created.ID, "alice", RequestFailed)
	if failed.Warning != analysisWarningCritique+" "+analysisWarningSuggestedFix {
		t.Fatalf("failed warning = %q", failed.Warning)
	}
	if failed.ReasonCode != ReasonNoReviewablePatch {
		t.Fatalf("reason code = %q", failed.ReasonCode)
	}
	if failed.Error != "The coding agent completed, but no repository change was generated." {
		t.Fatalf("error = %q", failed.Error)
	}
	wantSummary := "No deterministic repository edit was available. [redacted-url] token=[redacted]"
	if failed.Failure == nil || failed.Failure.OperatorSummary != wantSummary {
		t.Fatalf("failure = %+v", failed.Failure)
	}
	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	restored, err := reloaded.GetRequest(created.ID, "alice")
	if err != nil || restored.Failure == nil || restored.Failure.OperatorSummary != failed.Failure.OperatorSummary {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
}

func TestAnalysisFixOverlyBroadFailureIsRecoverable(t *testing.T) {
	service, _ := requestTestService(t)
	service.ConfigureAsyncRequests(time.Minute, nil)
	service.analysisRequestGenerator = func(context.Context, AnalysisFixInput, string, string, string) (PreviewResult, error) {
		return PreviewResult{}, withReason(ReasonNoReviewablePatch, ErrPreviewRejected, ReasonMessage(ReasonNoReviewablePatch))
	}
	created, err := service.CreateAnalysisFixRequest(exactAnalysisRequestInput(), "alice", "write-token", "")
	if err != nil {
		t.Fatal(err)
	}
	failed := waitRequest(t, service, created.ID, "alice", RequestFailed)
	if failed.ReasonCode != ReasonNoReviewablePatch || failed.Preview != nil {
		t.Fatalf("failed = %+v", failed)
	}
}

func TestFrozenCAPZFindingReachesAsyncGeneratorWithoutProvider(t *testing.T) {
	service, _ := requestTestService(t)
	service.ConfigureAsyncRequests(time.Minute, nil)
	const finding = "The artifact evidence is entirely in build-log.txt. Here is the exact chain:\n\n" +
		"1. **First CNI install** (line 2205): `STEP: Installing a CNI plugin to the workload cluster @ 08/12/26 08:51:19.322` — this step runs during `EnsureCloudProviderAzure` and succeeds, creating the `azure-cni` DaemonSet on the workload cluster.\n\n" +
		"2. **Second CNI install** (line 2217): `INFO: Installing a CNI plugin to the workload cluster capz-e2e-5p1bg6/capz-e2e-5p1bg6-azcni-v1` — this fires immediately after the CSI driver becomes available at 08:52:40.572. The very next line (2218) is the `[FAILED]` marker, meaning the call returned the 409 error without any retry.\n\n" +
		"3. **409 Conflict body** (lines 2567, 2580–2583): The full `StatusError` is printed:\n" +
		"   - `Operation cannot be fulfilled on daemonsets.apps \"azure-cni\": the object has been modified; please apply your changes to the latest version and try again`\n" +
		"   - `Reason: \"Conflict\"`, `Code: 409`\n\n" +
		"This implicates `InstallCNIManifest` in `test/e2e/cni.go` because that function is the only code path in the CAPZ e2e suite that applies the Azure CNI v1 manifest to the workload cluster. The second \"Installing a CNI plugin\" log message at line 2217 is the `Logf` call that `InstallCNIManifest` emits immediately before invoking `workloadCluster.CreateOrUpdate(ctx, cniYaml)`. `CreateOrUpdate` performs a `Get` then an `Update` using the `resourceVersion` embedded in the static manifest bytes, but the kube-controller-manager had already mutated the DaemonSet (advancing its `resourceVersion`) between the two install calls. The `Update` is therefore rejected by the API server with HTTP 409, and because `InstallCNIManifest` does not retry on conflict, the error propagates immediately as the terminal test failure at 08:52:40.919 (line 2218)."
	seen := make(chan AnalysisFixInput, 1)
	service.analysisRequestGenerator = func(_ context.Context, input AnalysisFixInput, _, _, _ string) (PreviewResult, error) {
		seen <- input
		return PreviewResult{Token: "preview-token", Kind: gfKind, Title: "Fix CNI", Body: "Safe body", Diff: "safe diff"}, nil
	}
	input := exactAnalysisRequestInput()
	input.AssistantAnswer = finding
	created, err := service.CreateAnalysisFixRequest(input, "alice", "write-token", "")
	if err != nil {
		t.Fatal(err)
	}
	ready := waitRequest(t, service, created.ID, "alice", RequestReady)
	if ready.Preview == nil {
		t.Fatalf("ready = %+v", ready)
	}
	select {
	case got := <-seen:
		if got.AssistantAnswer != finding {
			t.Fatalf("generator finding = %q", got.AssistantAnswer)
		}
	case <-time.After(time.Second):
		t.Fatal("fake generator was not reached")
	}
}

func TestAnalysisFixRecoverableReplacementIsExplicitAndImmutable(t *testing.T) {
	service, _ := requestTestService(t)
	service.ConfigureAsyncRequests(time.Minute, nil)
	var calls atomic.Int32
	service.analysisRequestGenerator = func(context.Context, AnalysisFixInput, string, string, string) (PreviewResult, error) {
		calls.Add(1)
		return PreviewResult{}, withReason(ReasonNoReviewablePatch, ErrPreviewRejected, ReasonMessage(ReasonNoReviewablePatch))
	}
	input := exactAnalysisRequestInput()
	created, err := service.CreateAnalysisFixRequest(input, "alice", "write-token", "")
	if err != nil {
		t.Fatal(err)
	}
	failed := waitRequest(t, service, created.ID, "alice", RequestFailed)
	service.rmu.Lock()
	record := service.requests.Requests[created.ID]
	record.Failure = &AnalysisFixFailureView{Category: AnalysisFixFailureNoReviewablePatch}
	if err := service.saveRequestsLocked(); err != nil {
		service.rmu.Unlock()
		t.Fatal(err)
	}
	before, err := json.Marshal(record)
	service.rmu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if failed.ReasonCode != ReasonNoReviewablePatch || calls.Load() != 1 {
		t.Fatalf("failed=%+v calls=%d", failed, calls.Load())
	}
	changed := input
	changed.PreviewRequestHash = "replacement-preview-hash"
	if _, err := service.CreateAnalysisFixRequest(changed, "alice", "write-token", "bounded feedback"); err == nil {
		t.Fatal("implicit replacement was accepted")
	}
	if _, err := service.CreateAnalysisFixRequest(changed, "alice", "write-token", "", created.ID); err == nil {
		t.Fatal("unchanged empty feedback was accepted")
	}
	replacement, err := service.CreateAnalysisFixRequest(changed, "alice", "write-token", "bounded feedback", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == created.ID {
		t.Fatalf("replacement reused failed request id %q", replacement.ID)
	}
	waitRequest(t, service, replacement.ID, "alice", RequestFailed)
	service.rmu.Lock()
	after, err := json.Marshal(service.requests.Requests[created.ID])
	service.rmu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("failed request mutated\nbefore=%s\nafter=%s", before, after)
	}
	if calls.Load() != 2 {
		t.Fatalf("generator calls = %d", calls.Load())
	}
}

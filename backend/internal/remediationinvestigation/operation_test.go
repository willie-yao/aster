package remediationinvestigation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type fakeOperationResolver struct {
	resolved      map[string]ResolvedOperation
	validateError error
	refreshActive bool
	refreshError  error
}

func (f *fakeOperationResolver) Validate(context.Context, OperationRef) error { return f.validateError }
func (f *fakeOperationResolver) Resolve(_ context.Context, ref OperationRef) (ResolvedOperation, error) {
	if f.validateError != nil {
		return ResolvedOperation{}, f.validateError
	}
	resolved, ok := f.resolved[operationIdentity(ref)]
	if !ok {
		return ResolvedOperation{}, ErrOperationNotFound
	}
	return resolved, nil
}
func (f *fakeOperationResolver) RefreshActive() (bool, error) { return f.refreshActive, f.refreshError }

type blockingOperationModel struct {
	result    string
	started   chan struct{}
	release   chan struct{}
	active    atomic.Int32
	maxActive atomic.Int32
	calls     atomic.Int32
}

func (m *blockingOperationModel) ToolLoop(_ context.Context, _, _ string, _ *tools.Registry, _ []string, _ *tools.Env, opts ai.ToolLoopOptions) (string, error) {
	m.calls.Add(1)
	active := m.active.Add(1)
	for {
		current := m.maxActive.Load()
		if active <= current || m.maxActive.CompareAndSwap(current, active) {
			break
		}
	}
	if opts.Observe != nil {
		opts.Observe(ai.ToolLoopEvent{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19})
		opts.Observe(ai.ToolLoopEvent{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80})
		opts.Observe(ai.ToolLoopEvent{Name: "grep_repo", ContentBytes: len("applyFix")})
	}
	m.started <- struct{}{}
	<-m.release
	m.active.Add(-1)
	return "bounded evidence", nil
}
func (m *blockingOperationModel) CompleteStructured(_ context.Context, _, _ string, _ ai.ResponseFormat, validate ai.StructuredValidator) error {
	return validate(json.RawMessage(m.result))
}
func (*blockingOperationModel) ModelName() string                   { return "test-model" }
func (*blockingOperationModel) ModelFingerprint() string            { return strings.Repeat("d", 16) }
func (*blockingOperationModel) APIMode() string                     { return ai.APIChatCompletions }
func (*blockingOperationModel) ReasoningEffort() ai.ReasoningEffort { return "" }

func operationFixture(t *testing.T, model Model) (*OperationService, *fakeOperationResolver, *Cache, OperationRef, OperationRef) {
	t.Helper()
	input := testFrozenInput()
	input.Group.RootCause = "reconcile is missing the required applyFix call"
	input.Group.ContentHash = models.PatternCausalGroupHash(input.Group)
	input.Group.ID = models.PatternCausalGroupID(input.PatternID, input.Group)
	input.CausalGroupID = input.Group.ID
	input.CausalGroupHash = input.Group.ContentHash
	for index := range input.Analyses {
		input.Analyses[index].RootCause = "reconcile is missing the required applyFix call"
		input.Analyses[index].RelevantFiles = []string{"controllers/reconcile.go"}
	}
	ref := operationRefForInput(input)
	second := input
	second.PatternID = "pattern-two"
	second.PatternHash = strings.Repeat("e", 64)
	second.Group.ID = models.PatternCausalGroupID(second.PatternID, second.Group)
	second.CausalGroupID = second.Group.ID
	secondRef := operationRefForInput(second)
	browser := fakeBrowser{files: map[string]string{
		"builds/1/log.txt": "reconcile missing applyFix transition\n", "builds/2/log.txt": "reconcile missing applyFix transition\n",
	}}
	resolved := func(value FrozenInput) ResolvedOperation {
		return ResolvedOperation{Input: value, Browser: browser, Source: fakeSource{files: map[string]string{"controllers/reconcile.go": serviceSourceContent}}}
	}
	resultJSON := operationActionableJSON(input)
	switch value := model.(type) {
	case *blockingOperationModel:
		value.result = resultJSON
	case *fakeModel:
		value.result = resultJSON
	case *refreshTimeoutModel:
		value.result = resultJSON
	}
	resolver := &fakeOperationResolver{resolved: map[string]ResolvedOperation{
		operationIdentity(ref): resolved(input), operationIdentity(secondRef): resolved(second),
	}}
	cache, err := NewCache("", CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewOperationService(t.Context(), model, cache, resolver, OperationOptions{Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return service, resolver, cache, ref, secondRef
}

func operationActionableJSON(input FrozenInput) string {
	sourceRecord := EvidenceRecord{
		Kind: EvidenceSource,
		Source: &SourceEvidenceIdentity{
			Repository: input.InvestigationSource, Path: "controllers/reconcile.go", ContentDigest: HashText(serviceSourceContent),
		},
	}
	sourceRecord.ID = evidenceRecordID(sourceRecord)
	evidenceIDs := []string{sourceRecord.ID}
	for _, analysis := range input.Analyses {
		record := EvidenceRecord{
			Kind: EvidenceAnalysis,
			Analysis: &AnalysisEvidenceIdentity{
				BuildID: analysis.BuildID, GeneratedAt: analysis.GeneratedAt, RootCauseDigest: HashText(analysis.RootCause),
			},
		}
		record.ID = evidenceRecordID(record)
		evidenceIDs = append(evidenceIDs, record.ID)
	}
	result := Result{
		Version: ResultVersion, CauseAssessment: CauseSupports, Reason: "the controller omits applyFix",
		Candidate: &RequiredCallCandidate{
			Kind: CandidateRequiredCall, Path: "controllers/reconcile.go", ContainingSymbol: "reconcile", RequiredCall: "applyFix",
		},
		EvidenceIDs: evidenceIDs,
	}
	encoded, _ := json.Marshal(result)
	return string(encoded)
}

func operationRefForInput(input FrozenInput) OperationRef {
	return OperationRef{
		JobID: input.JobID, PatternID: input.PatternID, PatternHash: input.PatternHash,
		CausalGroupID: input.CausalGroupID, CausalGroupHash: input.CausalGroupHash,
	}
}

func TestOperationSingleflightAndOneActiveInvestigation(t *testing.T) {
	model := &blockingOperationModel{result: actionableJSON(), started: make(chan struct{}, 2), release: make(chan struct{}, 2)}
	service, _, _, first, second := operationFixture(t, model)
	view, err := service.Start(t.Context(), first, "alice", "request-one", false)
	if err != nil || view.State != models.PatternRemediationQueued {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	<-model.started
	if _, err := service.Start(t.Context(), first, "alice", "request-two", false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start(t.Context(), second, "alice", "request-three", false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
		t.Fatal("second investigation became active before the first completed")
	case <-time.After(50 * time.Millisecond):
	}
	model.release <- struct{}{}
	<-model.started
	model.release <- struct{}{}
	waitOperation(t, service)
	if model.calls.Load() != 2 || model.maxActive.Load() != 1 {
		t.Fatalf("calls=%d max_active=%d", model.calls.Load(), model.maxActive.Load())
	}
	result, err := service.Get(t.Context(), first)
	if err != nil || result.State != models.PatternRemediationActionable || result.Target == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := service.Start(t.Context(), second, "alice", "request-one", false); !errors.Is(err, ErrOperationIdempotencyConflict) {
		t.Fatalf("idempotency conflict err=%v", err)
	}
}

func TestOperationFailedRefreshPreservesPreviousVerifiedResult(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence", result: actionableJSON(),
		toolEvents: []ai.ToolLoopEvent{{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19}, {Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80}, {Name: "grep_repo", ContentBytes: len("applyFix")}},
	}
	service, _, _, ref, _ := operationFixture(t, model)
	if _, err := service.Start(t.Context(), ref, "alice", "first", false); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service)
	model.toolErr = errors.New("private provider failure")
	if _, err := service.Start(t.Context(), ref, "alice", "refresh", true); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service)
	view, err := service.Get(t.Context(), ref)
	if err != nil || view.State != models.PatternRemediationActionable || view.Target == nil {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestOperationRestartDoesNotPublishPartialStateAndRecoversCompletedCache(t *testing.T) {
	model := &blockingOperationModel{result: actionableJSON(), started: make(chan struct{}, 1), release: make(chan struct{}, 1)}
	service, resolver, cache, ref, _ := operationFixture(t, model)
	if _, err := service.Start(t.Context(), ref, "alice", "first", false); err != nil {
		t.Fatal(err)
	}
	<-model.started
	restarted, err := NewOperationService(t.Context(), model, cache, resolver, OperationOptions{Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	partial, err := restarted.Get(t.Context(), ref)
	if err != nil || partial.State != models.PatternRemediationNotInvestigated {
		t.Fatalf("partial=%+v err=%v", partial, err)
	}
	model.release <- struct{}{}
	waitOperation(t, service)
	recovered, err := restarted.Get(t.Context(), ref)
	if err != nil || recovered.State != models.PatternRemediationActionable || recovered.Target == nil {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
}

func TestOperationRejectsRefreshAndStalePublishedIdentity(t *testing.T) {
	model := &fakeModel{fingerprint: strings.Repeat("d", 16)}
	service, resolver, _, ref, _ := operationFixture(t, model)
	resolver.refreshActive = true
	if _, err := service.Start(t.Context(), ref, "alice", "request", false); !errors.Is(err, ErrOperationRefreshRunning) {
		t.Fatalf("refresh err=%v", err)
	}
	resolver.refreshActive = false
	resolver.validateError = ErrOperationStale
	view, err := service.Get(t.Context(), ref)
	if err != nil || view.State != models.PatternRemediationStale {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func waitOperation(t *testing.T, service *OperationService) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := service.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

type timeoutOperationModel struct{}

func (*timeoutOperationModel) ToolLoop(ctx context.Context, _, _ string, _ *tools.Registry, _ []string, _ *tools.Env, _ ai.ToolLoopOptions) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}
func (*timeoutOperationModel) CompleteStructured(context.Context, string, string, ai.ResponseFormat, ai.StructuredValidator) error {
	return errors.New("unexpected finalization")
}
func (*timeoutOperationModel) ModelName() string                   { return "timeout-model" }
func (*timeoutOperationModel) ModelFingerprint() string            { return strings.Repeat("d", 16) }
func (*timeoutOperationModel) APIMode() string                     { return ai.APIChatCompletions }
func (*timeoutOperationModel) ReasoningEffort() ai.ReasoningEffort { return "" }

func TestOperationRejectsResultWhenPublishedIdentityChangesBeforeCompletion(t *testing.T) {
	model := &blockingOperationModel{started: make(chan struct{}, 1), release: make(chan struct{}, 1)}
	service, resolver, _, ref, _ := operationFixture(t, model)
	if _, err := service.Start(t.Context(), ref, "alice", "request", false); err != nil {
		t.Fatal(err)
	}
	<-model.started
	resolver.validateError = ErrOperationStale
	model.release <- struct{}{}
	waitOperation(t, service)
	view, err := service.Get(t.Context(), ref)
	if err != nil || view.State != models.PatternRemediationStale {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestOperationTimeoutPublishesOnlySafeFailure(t *testing.T) {
	model := &timeoutOperationModel{}
	input := testFrozenInput()
	ref := operationRefForInput(input)
	resolver := &fakeOperationResolver{resolved: map[string]ResolvedOperation{
		operationIdentity(ref): {Input: input, Browser: fakeBrowser{files: map[string]string{"builds/1/log.txt": "failure\n"}}, Source: fakeSource{files: map[string]string{"controllers/reconcile.go": serviceSourceContent}}},
	}}
	cache, err := NewCache("", CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewOperationService(t.Context(), model, cache, resolver, OperationOptions{Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start(t.Context(), ref, "alice", "request", false); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service)
	view, err := service.Get(t.Context(), ref)
	if err != nil || view.State != models.PatternRemediationInvestigationFailed || !strings.Contains(view.Reason, "timed out") || view.Target != nil {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestOperationTerminalStatusBecomesStaleWhenCurrentSourceIdentityChanges(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence",
		toolEvents: []ai.ToolLoopEvent{{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19}, {Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80}, {Name: "grep_repo", ContentBytes: len("applyFix")}},
	}
	service, resolver, _, ref, _ := operationFixture(t, model)
	if _, err := service.Start(t.Context(), ref, "alice", "request", false); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service)
	changed := resolver.resolved[operationIdentity(ref)]
	changed.Input.InvestigationSource.Revision = strings.Repeat("f", 40)
	resolver.resolved[operationIdentity(ref)] = changed
	view, err := service.Get(t.Context(), ref)
	if err != nil || view.State != models.PatternRemediationStale {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

type refreshTimeoutModel struct {
	calls  atomic.Int32
	result string
}

func (m *refreshTimeoutModel) ToolLoop(ctx context.Context, _, _ string, _ *tools.Registry, _ []string, _ *tools.Env, opts ai.ToolLoopOptions) (string, error) {
	if m.calls.Add(1) == 1 {
		if opts.Observe != nil {
			opts.Observe(ai.ToolLoopEvent{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19})
			opts.Observe(ai.ToolLoopEvent{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80})
			opts.Observe(ai.ToolLoopEvent{Name: "grep_repo", ContentBytes: len("applyFix")})
		}
		return "bounded evidence", nil
	}
	<-ctx.Done()
	return "", ctx.Err()
}
func (m *refreshTimeoutModel) CompleteStructured(_ context.Context, _, _ string, _ ai.ResponseFormat, validate ai.StructuredValidator) error {
	return validate(json.RawMessage(m.result))
}
func (*refreshTimeoutModel) ModelName() string                   { return "refresh-timeout-model" }
func (*refreshTimeoutModel) ModelFingerprint() string            { return strings.Repeat("d", 16) }
func (*refreshTimeoutModel) APIMode() string                     { return ai.APIChatCompletions }
func (*refreshTimeoutModel) ReasoningEffort() ai.ReasoningEffort { return "" }

type contextAwareSource struct{ files map[string]string }

func (s contextAwareSource) ListFiles(ctx context.Context, _ sourceinvestigation.Repository) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(s.files))
	for path := range s.files {
		paths = append(paths, path)
	}
	return paths, nil
}
func (s contextAwareSource) ReadFile(ctx context.Context, _ sourceinvestigation.Repository, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	content, ok := s.files[path]
	if !ok {
		return "", os.ErrNotExist
	}
	return content, nil
}

func TestOperationTimedOutRefreshPreservesPreviousResultWithFreshRecoveryContext(t *testing.T) {
	model := &refreshTimeoutModel{}
	_, resolver, cache, ref, _ := operationFixture(t, model)
	resolved := resolver.resolved[operationIdentity(ref)]
	resolved.Source = contextAwareSource{files: map[string]string{"controllers/reconcile.go": serviceSourceContent}}
	resolver.resolved[operationIdentity(ref)] = resolved
	service, err := NewOperationService(t.Context(), model, cache, resolver, OperationOptions{Timeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start(t.Context(), ref, "alice", "first", false); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service)
	before, err := service.Get(t.Context(), ref)
	if err != nil || before.State != models.PatternRemediationActionable {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	if _, err := service.Start(t.Context(), ref, "alice", "refresh", true); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service)
	after, err := service.Get(t.Context(), ref)
	if err != nil || after.State != models.PatternRemediationActionable || after.Target == nil {
		t.Fatalf("after=%+v err=%v", after, err)
	}
}

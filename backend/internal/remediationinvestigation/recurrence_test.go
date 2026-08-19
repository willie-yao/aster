package remediationinvestigation

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/models"
)

type recordedVerdict struct {
	signature  string
	state      models.PatternRemediationInvestigationState
	reason     string
	recordedAt string
}

type fakeRecurrenceLedger struct {
	mu        sync.Mutex
	reusable  map[string]recordedVerdict
	recorded  []recordedVerdict
	recordErr error
	claimErr  error
	claims    int
}

func (f *fakeRecurrenceLedger) ClaimReuse(signature string) (RecurrenceVerdict, bool, error) {
	if f.claimErr != nil {
		return RecurrenceVerdict{}, false, f.claimErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	verdict, ok := f.reusable[signature]
	if !ok {
		return RecurrenceVerdict{}, false, nil
	}
	f.claims++
	return RecurrenceVerdict{State: verdict.state, Reason: verdict.reason, RecordedAt: verdict.recordedAt}, true, nil
}

func (f *fakeRecurrenceLedger) RecordVerdict(signature string, verdict RecurrenceVerdict) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, recordedVerdict{
		signature: signature, state: verdict.State, reason: verdict.Reason, recordedAt: verdict.RecordedAt,
	})
	return f.recordErr
}

func (f *fakeRecurrenceLedger) records() []recordedVerdict {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedVerdict(nil), f.recorded...)
}

func ledgerOperationFixture(t *testing.T, model Model, ledger RecurrenceLedger, signature string) (*OperationService, OperationRef) {
	t.Helper()
	input := testFrozenInput()
	input.Group.RootCause = "reconcile is missing the required applyFix call"
	// Set before hashing: the signature is deliberately excluded from the
	// causal-group hash, so published identity must be unaffected by it.
	input.Group.Signature = signature
	input.Group.ContentHash = models.PatternCausalGroupHash(input.Group)
	input.Group.ID = models.PatternCausalGroupID(input.PatternID, input.Group)
	input.CausalGroupID = input.Group.ID
	input.CausalGroupHash = input.Group.ContentHash
	for index := range input.Analyses {
		input.Analyses[index].RootCause = "reconcile is missing the required applyFix call"
		input.Analyses[index].RelevantFiles = []string{"controllers/reconcile.go"}
	}
	ref := operationRefForInput(input)
	if value, ok := model.(*fakeModel); ok {
		value.result = operationActionableJSON(input)
	}
	resolver := &fakeOperationResolver{resolved: map[string]ResolvedOperation{
		operationIdentity(ref): {
			Input: input,
			Browser: fakeBrowser{files: map[string]string{
				"builds/1/log.txt": "reconcile missing applyFix transition\n",
				"builds/2/log.txt": "reconcile missing applyFix transition\n",
			}},
			Source: fakeSource{files: map[string]string{"controllers/reconcile.go": serviceSourceContent}},
		},
	}}
	cache, err := NewCache("", CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewOperationService(t.Context(), model, cache, resolver, OperationOptions{
		Timeout: time.Minute, Ledger: ledger,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, ref
}

func investigatingModel() *fakeModel {
	return &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence",
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80},
			{Name: "grep_repo", ContentBytes: len("applyFix")},
		},
	}
}

// A cause that aged out and came back gets a fresh frozen-input cache key even
// though the question was already answered, so without durable memory the same
// investigation is re-run at full model cost.
func TestOperationReusesPriorVerdictInsteadOfReinvestigating(t *testing.T) {
	ledger := &fakeRecurrenceLedger{reusable: map[string]recordedVerdict{
		"sig-a": {
			state:  models.PatternRemediationEnvironmentOrInfrastructure,
			reason: "cluster churn, not a code defect", recordedAt: "2026-02-01T00:00:00Z",
		},
	}}
	model := &blockingOperationModel{started: make(chan struct{}, 1), release: make(chan struct{}, 1)}
	service, ref := ledgerOperationFixture(t, model, ledger, "sig-a")

	view, err := service.Start(t.Context(), ref, "alice", "request-one", false)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != models.PatternRemediationEnvironmentOrInfrastructure {
		t.Fatalf("view=%+v, want the recorded verdict", view)
	}
	if view.Reason != "cluster churn, not a code defect" {
		t.Fatalf("view=%+v, want the recorded reason", view)
	}
	// A reused answer must not claim it was reached just now.
	if view.CompletedAt != "2026-02-01T00:00:00Z" {
		t.Fatalf("completed at=%q, want the original completion time", view.CompletedAt)
	}
	if view.CausalGroupID != ref.CausalGroupID || view.CausalGroupHash != ref.CausalGroupHash {
		t.Fatalf("view=%+v, want the current published identity", view)
	}
	if got := model.calls.Load(); got != 0 {
		t.Fatalf("model calls=%d, want the investigation skipped entirely", got)
	}
	if len(ledger.records()) != 0 {
		t.Fatalf("records=%+v, want a reused answer not re-recorded", ledger.records())
	}

	// The reused answer must survive a follow-up read rather than reverting to
	// "not investigated" and inviting another run.
	after, err := service.Get(t.Context(), ref)
	if err != nil || after.State != models.PatternRemediationEnvironmentOrInfrastructure {
		t.Fatalf("after=%+v err=%v", after, err)
	}
}

// A ledger written before completion times were recorded, or with a corrupt one,
// must still be reusable rather than failing closed on a cosmetic field.
func TestOperationReusedVerdictFallsBackWhenTheRecordedTimeIsUnusable(t *testing.T) {
	for _, recordedAt := range []string{"", "not-a-time"} {
		ledger := &fakeRecurrenceLedger{reusable: map[string]recordedVerdict{
			"sig-a": {state: models.PatternRemediationMitigationOnly, recordedAt: recordedAt},
		}}
		service, ref := ledgerOperationFixture(t, investigatingModel(), ledger, "sig-a")
		view, err := service.Start(t.Context(), ref, "alice", "request-one", false)
		if err != nil || view.State != models.PatternRemediationMitigationOnly {
			t.Fatalf("recordedAt=%q view=%+v err=%v", recordedAt, view, err)
		}
		if _, parseErr := time.Parse(time.RFC3339, view.CompletedAt); parseErr != nil {
			t.Fatalf("recordedAt=%q produced completed at=%q", recordedAt, view.CompletedAt)
		}
	}
}

func TestOperationRefreshBypassesTheReusedVerdict(t *testing.T) {
	ledger := &fakeRecurrenceLedger{reusable: map[string]recordedVerdict{
		"sig-a": {state: models.PatternRemediationInsufficientEvidence, reason: "not enough evidence"},
	}}
	model := investigatingModel()
	service, ref := ledgerOperationFixture(t, model, ledger, "sig-a")

	view, err := service.Start(t.Context(), ref, "alice", "request-one", true)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != models.PatternRemediationQueued {
		t.Fatalf("view=%+v, want an explicit refresh to investigate", view)
	}
	waitOperation(t, service)
	final, err := service.Get(t.Context(), ref)
	if err != nil || final.State != models.PatternRemediationActionable {
		t.Fatalf("final=%+v err=%v", final, err)
	}
}

// The ledger orders concurrent writers by when an answer was actually reached,
// not when it was written, so finish must hand it the view's completion time
// rather than letting it stamp the current clock.
func TestOperationRecordsTerminalVerdictInDurableMemory(t *testing.T) {
	ledger := &fakeRecurrenceLedger{}
	service, ref := ledgerOperationFixture(t, investigatingModel(), ledger, "sig-a")
	if _, err := service.Start(t.Context(), ref, "alice", "request-one", false); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service)

	records := ledger.records()
	if len(records) != 1 {
		t.Fatalf("records=%+v, want exactly one verdict", records)
	}
	if records[0].signature != "sig-a" || records[0].state != models.PatternRemediationActionable {
		t.Fatalf("record=%+v", records[0])
	}
	view, err := service.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if records[0].recordedAt == "" || records[0].recordedAt != view.CompletedAt {
		t.Fatalf("recorded at=%q, want the view's completion time %q", records[0].recordedAt, view.CompletedAt)
	}
}

func TestOperationSkipsDurableMemoryWithoutASignature(t *testing.T) {
	ledger := &fakeRecurrenceLedger{reusable: map[string]recordedVerdict{
		"": {state: models.PatternRemediationMitigationOnly, reason: "should never be consulted"},
	}}
	service, ref := ledgerOperationFixture(t, investigatingModel(), ledger, "")
	view, err := service.Start(t.Context(), ref, "alice", "request-one", false)
	if err != nil || view.State != models.PatternRemediationQueued {
		t.Fatalf("view=%+v err=%v, want a normal investigation", view, err)
	}
	waitOperation(t, service)
	if records := ledger.records(); len(records) != 0 {
		t.Fatalf("records=%+v, want nothing recorded without a signature", records)
	}
}

// A ledger returning something the published contract does not allow must never
// reach the maintainer as a state the frontend cannot render.
func TestOperationIgnoresAnInvalidReusedState(t *testing.T) {
	ledger := &fakeRecurrenceLedger{reusable: map[string]recordedVerdict{
		"sig-a": {state: models.PatternRemediationInvestigationState("not-a-real-state")},
	}}
	service, ref := ledgerOperationFixture(t, investigatingModel(), ledger, "sig-a")
	view, err := service.Start(t.Context(), ref, "alice", "request-one", false)
	if err != nil || view.State != models.PatternRemediationQueued {
		t.Fatalf("view=%+v err=%v, want the invalid verdict ignored", view, err)
	}
	waitOperation(t, service)
}

// Durable memory is an optimization, so losing it must not fail the operation
// the maintainer actually asked for.
func TestOperationSurvivesADurableMemoryWriteFailure(t *testing.T) {
	ledger := &fakeRecurrenceLedger{recordErr: errors.New("disk full")}
	service, ref := ledgerOperationFixture(t, investigatingModel(), ledger, "sig-a")
	if _, err := service.Start(t.Context(), ref, "alice", "request-one", false); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service)
	view, err := service.Get(t.Context(), ref)
	if err != nil || view.State != models.PatternRemediationActionable {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

// An exact frozen-input cache entry is a precise match for this input, so it must
// win over a signature-level answer. Recurrence memory exists for the case where
// that cache misses because the builds rolled.
func TestOperationPrefersAnExactCacheHitOverRecurrenceMemory(t *testing.T) {
	ledger := &fakeRecurrenceLedger{reusable: map[string]recordedVerdict{
		"sig-a": {
			state:  models.PatternRemediationEnvironmentOrInfrastructure,
			reason: "stale signature-level answer", recordedAt: "2026-01-01T00:00:00Z",
		},
	}}
	service, ref := ledgerOperationFixture(t, investigatingModel(), ledger, "sig-a")

	// Populate the exact frozen-input cache with a real completed investigation.
	if _, err := service.Start(t.Context(), ref, "alice", "first", true); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service)
	recordedBefore := len(ledger.records())

	// A fresh service shares the cache but has no in-memory record, which is the
	// state after a restart or on another replica.
	restartedModel := investigatingModel()
	restarted, err := NewOperationService(t.Context(), restartedModel, service.cache, service.resolver, OperationOptions{
		Timeout: time.Minute, Ledger: ledger,
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := restarted.Start(t.Context(), ref, "alice", "second", false)
	if err != nil {
		t.Fatal(err)
	}
	if view.State == models.PatternRemediationEnvironmentOrInfrastructure {
		t.Fatalf("view=%+v, want the exact cache result rather than the ledger answer", view)
	}
	waitOperation(t, restarted)
	final, err := restarted.Get(t.Context(), ref)
	if err != nil || final.State != models.PatternRemediationActionable {
		t.Fatalf("final=%+v err=%v", final, err)
	}
	if restartedModel.toolCalls != 0 || restartedModel.finalCalls != 0 {
		t.Fatalf("model was re-run despite an exact cache hit: tool=%d final=%d",
			restartedModel.toolCalls, restartedModel.finalCalls)
	}
	// A cache hit is not new work, so it must not refresh durable memory and hand
	// the stored verdict a fresh reuse budget.
	if got := len(ledger.records()); got != recordedBefore {
		t.Fatalf("records grew from %d to %d on a cache hit", recordedBefore, got)
	}
}

// A reused answer is published immediately, so a subject that is no longer the
// current one must yield the stale view rather than an answer, without spending
// any model budget.
func TestOperationReuseRefusesAStalePublishedSubject(t *testing.T) {
	ledger := &fakeRecurrenceLedger{reusable: map[string]recordedVerdict{
		"sig-a": {state: models.PatternRemediationMitigationOnly, recordedAt: "2026-02-01T00:00:00Z"},
	}}
	model := &blockingOperationModel{started: make(chan struct{}, 1), release: make(chan struct{}, 1)}
	service, ref := ledgerOperationFixture(t, model, ledger, "sig-a")
	service.resolver.(*fakeOperationResolver).publishValidateError = ErrOperationStale

	view, err := service.Start(t.Context(), ref, "alice", "request-one", false)
	if err != nil {
		t.Fatalf("err=%v, want a stale view", err)
	}
	if view.State != models.PatternRemediationStale {
		t.Fatalf("view=%+v, want the stale view", view)
	}
	if model.calls.Load() != 0 {
		t.Fatalf("model calls=%d, want none", model.calls.Load())
	}
	if ledger.claims != 0 {
		t.Fatalf("claims=%d, want the reuse budget untouched", ledger.claims)
	}
}

// A claim that cannot be charged must not be served: an unaccounted answer would
// escape the reuse bound entirely.
func TestOperationInvestigatesWhenAReuseCannotBeCharged(t *testing.T) {
	ledger := &fakeRecurrenceLedger{
		reusable: map[string]recordedVerdict{"sig-a": {state: models.PatternRemediationMitigationOnly}},
		claimErr: errors.New("ledger unavailable"),
	}
	service, ref := ledgerOperationFixture(t, investigatingModel(), ledger, "sig-a")
	view, err := service.Start(t.Context(), ref, "alice", "request-one", false)
	if err != nil || view.State != models.PatternRemediationQueued {
		t.Fatalf("view=%+v err=%v, want a normal investigation", view, err)
	}
	waitOperation(t, service)
}

// Recording a recovered previous result would reset the bounded reuse budget
// without any new work having happened, which would make the bound meaningless.
// Only a freshly computed, verified investigation counts.
func TestOperationDoesNotRecordARecoveredPreviousResult(t *testing.T) {
	ledger := &fakeRecurrenceLedger{}
	model := investigatingModel()
	service, ref := ledgerOperationFixture(t, model, ledger, "sig-a")

	if _, err := service.Start(t.Context(), ref, "alice", "first", false); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service)
	before := len(ledger.records())
	if before != 1 {
		t.Fatalf("records=%d, want the first investigation recorded", before)
	}

	// A refresh whose run fails falls back to the previous verified result. That
	// is a republication, not new work.
	model.toolErr = errors.New("private provider failure")
	if _, err := service.Start(t.Context(), ref, "alice", "recovered", true); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, service)
	view, err := service.Get(t.Context(), ref)
	if err != nil || view.State != models.PatternRemediationActionable {
		t.Fatalf("view=%+v err=%v, want the recovered result published", view, err)
	}
	if got := len(ledger.records()); got != before {
		t.Fatalf("records grew from %d to %d on a recovered result", before, got)
	}
}

// Without recurrence memory there is nothing to reuse, so the reuse path must not
// run at all: it would cost a second Resolve and could fail an otherwise valid
// investigation on a transient publication-state error.
func TestOperationWithoutALedgerSkipsTheReusePathEntirely(t *testing.T) {
	service, ref := ledgerOperationFixture(t, investigatingModel(), nil, "sig-a")
	resolver := service.resolver.(*fakeOperationResolver)
	before := resolver.resolveCalls.Load()
	if _, err := service.Start(t.Context(), ref, "alice", "request-one", false); err != nil {
		t.Fatal(err)
	}
	if got := resolver.resolveCalls.Load() - before; got != 1 {
		t.Fatalf("resolve calls=%d, want the reuse path skipped", got)
	}
	waitOperation(t, service)
	view, err := service.Get(t.Context(), ref)
	if err != nil || view.State != models.PatternRemediationActionable {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

// A cause with no durable identity is likewise not a reuse candidate.
func TestOperationWithoutASignatureSkipsTheReusePath(t *testing.T) {
	ledger := &fakeRecurrenceLedger{}
	service, ref := ledgerOperationFixture(t, investigatingModel(), ledger, "")
	resolver := service.resolver.(*fakeOperationResolver)
	before := resolver.resolveCalls.Load()
	if _, err := service.Start(t.Context(), ref, "alice", "request-one", false); err != nil {
		t.Fatal(err)
	}
	if got := resolver.resolveCalls.Load() - before; got != 1 {
		t.Fatalf("resolve calls=%d, want the reuse path skipped", got)
	}
	waitOperation(t, service)
	if ledger.claims != 0 || len(ledger.records()) != 0 {
		t.Fatalf("claims=%d records=%d, want durable memory untouched", ledger.claims, len(ledger.records()))
	}
}

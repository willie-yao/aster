package remediationinvestigation

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/repotree"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type fakeModel struct {
	fingerprint       string
	toolEvents        []ai.ToolLoopEvent
	toolEventSets     [][]ai.ToolLoopEvent
	privateToolEvents []ai.ToolLoopPrivateEvent
	memo              string
	result            string
	results           []string
	toolErr           error
	finalErr          error
	toolCalls         int
	finalCalls        int
	toolOptions       []ai.ToolLoopOptions
}

func (m *fakeModel) ToolLoop(_ context.Context, _, _ string, _ *tools.Registry, _ []string, _ *tools.Env, opts ai.ToolLoopOptions) (string, error) {
	m.toolCalls++
	opts.RequiredTools = append([]ai.RequiredTool(nil), opts.RequiredTools...)
	m.toolOptions = append(m.toolOptions, opts)
	events := m.toolEvents
	if index := m.toolCalls - 1; index >= 0 && index < len(m.toolEventSets) {
		events = m.toolEventSets[index]
	}
	for _, event := range events {
		if opts.Observe != nil {
			opts.Observe(event)
		}
	}
	for _, event := range m.privateToolEvents {
		if opts.ObservePrivate != nil {
			opts.ObservePrivate(event)
		}
	}
	return m.memo, m.toolErr
}

func (m *fakeModel) CompleteStructured(_ context.Context, _, _ string, _ ai.ResponseFormat, validate ai.StructuredValidator) error {
	m.finalCalls++
	if m.finalErr != nil {
		return m.finalErr
	}
	result := m.result
	if index := m.finalCalls - 1; index >= 0 && index < len(m.results) {
		result = m.results[index]
	}
	return validate(json.RawMessage(result))
}

func (m *fakeModel) CompleteStructuredWithMetadata(ctx context.Context, system, user string, format ai.ResponseFormat, validate ai.StructuredValidator) (ai.StructuredCompletionMetadata, error) {
	err := m.CompleteStructured(ctx, system, user, format, validate)
	attempt := ai.StructuredAttemptMetadata{
		Phase: ai.StructuredCompletionPhase(ctx), Path: ai.StructuredAttemptResponseFormat,
		ProviderAttempts: 1, ProviderAttemptsKnown: true,
	}
	if err == nil {
		attempt.Outcome = ai.StructuredOutcomeAccepted
		attempt.ValidatorCalled = true
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || m.finalErr != nil {
		attempt.Outcome = ai.StructuredOutcomeProviderError
		attempt.ProviderCategory = "request_transport"
	} else {
		attempt.Outcome = ai.StructuredOutcomeValidatorRejected
		attempt.ValidatorCalled = true
		var coded interface{ StructuredValidationCode() string }
		if errors.As(err, &coded) {
			attempt.ValidationCode = coded.StructuredValidationCode()
		}
	}
	return ai.StructuredCompletionMetadata{Attempts: []ai.StructuredAttemptMetadata{attempt}}, err
}

func (*fakeModel) ModelName() string                   { return "test-model" }
func (m *fakeModel) ModelFingerprint() string          { return m.fingerprint }
func (*fakeModel) APIMode() string                     { return ai.APIResponses }
func (*fakeModel) ReasoningEffort() ai.ReasoningEffort { return ai.ReasoningEffortHigh }

type fakeSource struct{ files map[string]string }

func (s fakeSource) ListFiles(context.Context, sourceinvestigation.Repository) ([]string, error) {
	paths := make([]string, 0, len(s.files))
	for file := range s.files {
		paths = append(paths, file)
	}
	return paths, nil
}

func (s fakeSource) ReadFile(_ context.Context, _ sourceinvestigation.Repository, file string) (string, error) {
	content, ok := s.files[file]
	if !ok {
		return "", errors.New("not found")
	}
	return content, nil
}

type fakeBrowser struct{ files map[string]string }

func (fakeBrowser) BuildRoot() string { return "frozen builds" }
func (fakeBrowser) List(context.Context, string) (*artifacts.Listing, error) {
	return &artifacts.Listing{}, nil
}
func (b fakeBrowser) ListTree(context.Context, int) ([]string, bool, error) {
	paths := make([]string, 0, len(b.files))
	for file := range b.files {
		paths = append(paths, file)
	}
	return paths, false, nil
}
func (b fakeBrowser) Read(_ context.Context, file string, offset, length int) ([]byte, int64, error) {
	content, ok := b.files[file]
	if !ok {
		return nil, -1, errors.New("not found")
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(content) {
		offset = len(content)
	}
	end := offset + length
	if end > len(content) {
		end = len(content)
	}
	return []byte(content[offset:end]), int64(len(content)), nil
}
func (b fakeBrowser) Tail(ctx context.Context, file string, _ int, maxBytes int) (*artifacts.TailResult, error) {
	content, size, err := b.Read(ctx, file, 0, maxBytes)
	return &artifacts.TailResult{FileSize: size, LinesReturned: strings.Count(string(content), "\n") + 1, Content: content}, err
}
func (b fakeBrowser) Grep(_ context.Context, file string, re *regexp.Regexp, _ int, _ int, _ int, _ int) (*artifacts.GrepResult, error) {
	content, ok := b.files[file]
	if !ok {
		return nil, errors.New("not found")
	}
	result := &artifacts.GrepResult{FileSize: int64(len(content)), BytesScanned: int64(len(content))}
	for index, line := range strings.Split(content, "\n") {
		if re.MatchString(line) {
			result.TotalMatches++
			result.Matches = append(result.Matches, artifacts.GrepMatch{LineNo: index + 1, Context: []string{"> " + line}})
		}
	}
	return result, nil
}

const serviceSourceContent = "package controllers\nfunc reconcile() error {\n\treturn nil\n}\nfunc applyFix() {}\n"

func serviceTestInput() FrozenInput {
	input := testFrozenInput()
	for index := range input.Analyses {
		input.Analyses[index].RootCause = "reconcile is missing the required applyFix call"
		input.Analyses[index].RelevantFiles = []string{"controllers/reconcile.go"}
	}
	return input
}

func actionableJSON() string {
	input := serviceTestInput()
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
	extraction := TargetExtraction{
		Version: TargetExtractionVersion,
		Hypotheses: []TargetHypothesis{{
			Target: &RequiredCallCandidate{
				Kind: CandidateRequiredCall, Path: "controllers/reconcile.go", ContainingSymbol: "reconcile", RequiredCall: "applyFix",
			},
			EvidenceIDs: evidenceIDs, RelationshipReason: "the controller omits applyFix",
		}},
	}
	encoded, _ := json.Marshal(extraction)
	return string(encoded)
}

func nonActionableJSON() string {
	input := serviceTestInput()
	record := EvidenceRecord{
		Kind: EvidenceAnalysis,
		Analysis: &AnalysisEvidenceIdentity{
			BuildID: input.Analyses[0].BuildID, GeneratedAt: input.Analyses[0].GeneratedAt, RootCauseDigest: HashText(input.Analyses[0].RootCause),
		},
	}
	record.ID = evidenceRecordID(record)
	assessment := NonActionableAssessment{
		Version: NonActionableAssessmentVersion, CauseAssessment: CauseInconclusive,
		Reason: "no target passed deterministic verification", EvidenceIDs: []string{record.ID},
		NonActionableReason: NonActionableInsufficientEvidence,
	}
	encoded, _ := json.Marshal(assessment)
	return string(encoded)
}

func serviceFixture(t *testing.T, model *fakeModel) (*Service, FrozenInput, fakeBrowser, *Cache) {
	t.Helper()
	input := serviceTestInput()
	input.ProviderFingerprint = model.fingerprint
	cache, err := NewCache("", CacheOptions{Now: func() time.Time { return time.Date(2026, 8, 12, 2, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(model, fakeSource{files: map[string]string{
		"controllers/reconcile.go": serviceSourceContent,
		"docs/notes.md":            "applyFix diagnostic notes\n",
	}}, cache, ServiceOptions{Now: func() time.Time { return time.Date(2026, 8, 12, 2, 0, 1, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	browser := fakeBrowser{files: map[string]string{"builds/1/log.txt": "reconcile missing applyFix transition\n", "builds/2/log.txt": "reconcile missing applyFix transition\n"}}
	return service, input, browser, cache
}

func TestServiceCachesEvidenceBackedTypedResult(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "The failed builds report a missing transition and reconcile returns without applyFix.",
		result: actionableJSON(),
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80},
			{Name: "grep_repo", ContentBytes: len("applyFix")},
		},
	}
	service, input, browser, _ := serviceFixture(t, model)
	got, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheHit || len(got.Entry.Result.Hypotheses) == 0 || got.Entry.Provenance.Evidence.SourceReads != 1 || got.Entry.Provenance.Evidence.ArtifactReads != 1 || got.Entry.Provenance.ReasoningEffort != "high" || len(got.Entry.EvidenceCatalog.Records) < 4 {
		t.Fatalf("result=%+v", got)
	}
	cached, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil || !cached.CacheHit || model.toolCalls != 1 || model.finalCalls != 1 {
		t.Fatalf("cached=%+v err=%v tool=%d final=%d", cached, err, model.toolCalls, model.finalCalls)
	}
}

func TestServiceIssuesAndVerifiesSourceGrepEvidence(t *testing.T) {
	input := serviceTestInput()
	grepRecord := EvidenceRecord{
		Kind: EvidenceSourceGrep,
		SourceGrep: &SourceGrepEvidenceIdentity{
			Repository: input.InvestigationSource, Path: "controllers/reconcile.go", LineStart: 5, LineEnd: 5,
			ContentDigest: HashText(serviceSourceContent), Match: "func applyFix() {}",
		},
	}
	grepRecord.ID = evidenceRecordID(grepRecord)
	evidenceIDs := []string{grepRecord.ID}
	for _, analysis := range input.Analyses {
		record := EvidenceRecord{Kind: EvidenceAnalysis, Analysis: &AnalysisEvidenceIdentity{
			BuildID: analysis.BuildID, GeneratedAt: analysis.GeneratedAt, RootCauseDigest: HashText(analysis.RootCause),
		}}
		record.ID = evidenceRecordID(record)
		evidenceIDs = append(evidenceIDs, record.ID)
	}
	extraction := TargetExtraction{
		Version: TargetExtractionVersion,
		Hypotheses: []TargetHypothesis{{
			Target: &RequiredCallCandidate{
				Kind: CandidateRequiredCall, Path: "controllers/reconcile.go", ContainingSymbol: "reconcile", RequiredCall: "applyFix",
			},
			EvidenceIDs: evidenceIDs, RelationshipReason: "the controller omits applyFix",
		}},
	}
	encoded, _ := json.Marshal(extraction)
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "applyFix exists but reconcile does not call it", result: string(encoded),
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: len(serviceSourceContent), ContentBytes: len(serviceSourceContent)},
			{Name: "grep_repo", ContentBytes: len("func applyFix() {}")},
		},
		privateToolEvents: []ai.ToolLoopPrivateEvent{{
			Name: "grep_repo", Observation: repotree.GrepObservation{Matches: []repotree.GrepMatchObservation{{
				Path: "controllers/reconcile.go", LineStart: 5, LineEnd: 5,
			}}},
		}},
	}
	service, input, browser, _ := serviceFixture(t, model)
	got, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range got.Entry.EvidenceCatalog.Records {
		if record.ID == grepRecord.ID && record.SourceGrep != nil && record.SourceGrep.Match == "func applyFix() {}" {
			found = true
		}
	}
	if !found || len(got.Entry.Result.Hypotheses) == 0 || got.Entry.Provenance.Evidence.SourceReads != 1 || got.Entry.Provenance.Evidence.SourceGreps != 1 {
		t.Fatalf("result=%+v", got)
	}
}

func TestServiceEvidenceFloorReturnsSafeNonActionableResult(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "No source was read.",
		toolEvents: []ai.ToolLoopEvent{{Name: "list_repo_tree"}},
	}
	service, input, browser, _ := serviceFixture(t, model)
	got, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entry.Result.Hypotheses) != 0 || got.Entry.Result.NonActionable == nil || got.Entry.Result.NonActionable.NonActionableReason != NonActionableInsufficientEvidence || model.finalCalls != 0 {
		t.Fatalf("result=%+v finalCalls=%d", got, model.finalCalls)
	}
}

func TestServiceZeroMatchGrepDoesNotPassEvidenceFloor(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "read source but grep found no match", result: actionableJSON(),
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80},
			{Name: "grep_repo", ContentBytes: 0},
		},
	}
	service, input, browser, _ := serviceFixture(t, model)
	got, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entry.Result.Hypotheses) != 0 || got.Entry.Result.NonActionable == nil ||
		got.Entry.Result.NonActionable.NonActionableReason != NonActionableInsufficientEvidence || model.finalCalls != 0 ||
		got.Entry.Provenance.Evidence.SourceReads != 1 || got.Entry.Provenance.Evidence.SourceGreps != 0 {
		t.Fatalf("result=%+v finalCalls=%d", got, model.finalCalls)
	}
}

func TestServiceContentBearingGrepDoesNotReplaceRequiredSourceRead(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "grep evidence only",
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "grep_repo", ContentBytes: len("func applyFix() {}")},
		},
		privateToolEvents: []ai.ToolLoopPrivateEvent{{
			Name: "grep_repo", Observation: repotree.GrepObservation{Matches: []repotree.GrepMatchObservation{{
				Path: "controllers/reconcile.go", LineStart: 5, LineEnd: 5,
			}}},
		}},
	}
	service, input, browser, _ := serviceFixture(t, model)
	got, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entry.Result.Hypotheses) != 0 || got.Entry.Result.NonActionable == nil ||
		got.Entry.Result.NonActionable.NonActionableReason != NonActionableInsufficientEvidence || model.finalCalls != 0 || got.Entry.Provenance.Evidence.SourceReads != 0 {
		t.Fatalf("result=%+v finalCalls=%d", got, model.finalCalls)
	}
}

func TestServiceUsesForcedRequiredSourceReadWithoutRestart(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence", result: actionableJSON(),
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80, Forced: true},
			{Name: "grep_repo", ContentBytes: len("applyFix"), Forced: true},
		},
	}
	service, input, browser, _ := serviceFixture(t, model)
	got, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil {
		t.Fatal(err)
	}
	if model.toolCalls != 1 || model.finalCalls != 1 || got.Entry.Provenance.Metrics.EvidenceRetryCount != 2 || len(got.Entry.Result.Hypotheses) == 0 {
		t.Fatalf("tool=%d final=%d result=%+v", model.toolCalls, model.finalCalls, got)
	}
	required := model.toolOptions[0].RequiredTools
	if len(required) != 2 || required[0].Name != "read_repo_file" || !required[0].RequireContent || required[0].MaxAttempts != 1 ||
		required[1].Name != "grep_repo" || !required[1].RequireContent || required[1].MaxAttempts != 1 {
		t.Fatalf("required tools=%+v", required)
	}
	for _, anchor := range []string{"job names", "environment names", "symbols", "calls", "configuration values"} {
		if !strings.Contains(required[1].CorrectivePrompt, anchor) {
			t.Fatalf("grep corrective prompt lacks %q", anchor)
		}
	}
	for _, private := range []string{"applyFix", "KUBERNETES_VERSION", "v1.23.5"} {
		if strings.Contains(required[1].CorrectivePrompt, private) {
			t.Fatalf("grep corrective prompt leaked expected identity %q", private)
		}
	}
}

func TestServiceRequiresTreeListingWhenNoRelevantFileHintResolves(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence", results: []string{actionableJSON(), nonActionableJSON()},
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "list_repo_tree", Forced: true},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80, Forced: true},
			{Name: "grep_repo", ContentBytes: len("applyFix"), Forced: true},
		},
	}
	service, input, browser, _ := serviceFixture(t, model)
	input.RelevantFiles = nil
	for index := range input.Analyses {
		input.Analyses[index].RelevantFiles = nil
	}
	got, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil {
		t.Fatal(err)
	}
	required := model.toolOptions[0].RequiredTools
	if len(required) != 3 || required[0].Name != "list_repo_tree" || required[1].Name != "read_repo_file" || !required[1].RequireContent ||
		required[2].Name != "grep_repo" || !required[2].RequireContent {
		t.Fatalf("required tools=%+v", required)
	}
	if got.Entry.Provenance.Metrics.EvidenceRetryCount != 3 || got.Entry.Provenance.Evidence.SourceLists != 1 ||
		got.Entry.Provenance.Evidence.SourceReads != 1 || got.Entry.Provenance.Evidence.SourceGreps != 1 {
		t.Fatalf("provenance=%+v", got.Entry.Provenance)
	}
}

func TestServiceWrongPathGrepDoesNotGroundCandidate(t *testing.T) {
	input := testFrozenInput()
	docsContent := "applyFix diagnostic notes\n"
	docsRecord := EvidenceRecord{
		Kind: EvidenceSource,
		Source: &SourceEvidenceIdentity{
			Repository: input.InvestigationSource, Path: "docs/notes.md", ContentDigest: HashText(docsContent),
		},
	}
	docsRecord.ID = evidenceRecordID(docsRecord)
	extraction := TargetExtraction{
		Version: TargetExtractionVersion,
		Hypotheses: []TargetHypothesis{{
			Target: &RequiredCallCandidate{
				Kind: CandidateRequiredCall, Path: "controllers/reconcile.go", ContainingSymbol: "reconcile", RequiredCall: "applyFix",
			},
			EvidenceIDs: []string{docsRecord.ID}, RelationshipReason: "notes mention applyFix",
		}},
	}
	encoded, _ := json.Marshal(extraction)
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "unrelated notes mention applyFix", result: string(encoded),
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "docs/notes.md", BytesFetched: len(docsContent), ContentBytes: len(docsContent)},
			{Name: "grep_repo", ContentBytes: len("applyFix diagnostic notes")},
		},
	}
	service, input, browser, _ := serviceFixture(t, model)
	if _, err := service.Investigate(t.Context(), input, browser, false); err == nil || ErrorCode(err) != "missing_source_evidence" {
		t.Fatalf("wrong-path grep err=%v code=%q", err, ErrorCode(err))
	}
}

func TestServiceFailedRefreshPreservesAcceptedResult(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence", result: actionableJSON(),
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80},
			{Name: "grep_repo", ContentBytes: len("applyFix")},
		},
	}
	service, input, browser, cache := serviceFixture(t, model)
	if _, err := service.Investigate(t.Context(), input, browser, false); err != nil {
		t.Fatal(err)
	}
	model.toolErr = errors.New("private provider failure")
	if _, err := service.Investigate(t.Context(), input, browser, true); err == nil {
		t.Fatal("failed refresh succeeded")
	}
	key, _ := CacheKey(input)
	entry, ok, err := cache.Lookup(key)
	if err != nil || !ok || len(entry.Result.Hypotheses) == 0 || entry.LastFailure == nil {
		t.Fatalf("entry=%+v ok=%v err=%v", entry, ok, err)
	}
}

func TestServiceRepairsOneInvalidStructuredResult(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence",
		results: []string{`{"version":0}`, actionableJSON()},
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80},
			{Name: "grep_repo", ContentBytes: len("applyFix")},
		},
	}
	service, input, browser, _ := serviceFixture(t, model)
	got, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil {
		t.Fatal(err)
	}
	if model.finalCalls != 2 || got.Entry.Provenance.Metrics.RepairCount != 1 || len(got.Entry.Result.Hypotheses) == 0 {
		t.Fatalf("calls=%d result=%+v", model.finalCalls, got)
	}
}

func TestServiceInvalidRefreshPreservesAcceptedResult(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence", result: actionableJSON(),
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80},
			{Name: "grep_repo", ContentBytes: len("applyFix")},
		},
	}
	service, input, browser, cache := serviceFixture(t, model)
	if _, err := service.Investigate(t.Context(), input, browser, false); err != nil {
		t.Fatal(err)
	}
	model.result = `{"version":0}`
	if _, err := service.Investigate(t.Context(), input, browser, true); err == nil || ErrorCode(err) == "" {
		t.Fatalf("invalid refresh err=%v", err)
	}
	key, _ := CacheKey(input)
	entry, ok, err := cache.Lookup(key)
	if err != nil || !ok || len(entry.Result.Hypotheses) == 0 || entry.LastFailure == nil || entry.LastFailure.Category != FailureTargetExtractionValidation {
		t.Fatalf("entry=%+v ok=%v err=%v", entry, ok, err)
	}
}

func TestServiceRejectsCandidateWithoutIssuedSourceEvidence(t *testing.T) {
	var result TargetExtraction
	if err := json.Unmarshal([]byte(actionableJSON()), &result); err != nil {
		t.Fatal(err)
	}
	result.Hypotheses[0].Target = &RequiredCallCandidate{
		Kind: CandidateRequiredCall, Path: "controllers/other.go", ContainingSymbol: "reconcile", RequiredCall: "applyFix",
	}
	encoded, _ := json.Marshal(result)
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence", result: string(encoded),
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80},
			{Name: "grep_repo", ContentBytes: len("applyFix")},
		},
	}
	service, input, browser, cache := serviceFixture(t, model)
	if _, err := service.Investigate(t.Context(), input, browser, false); err == nil {
		t.Fatal("candidate without engine-issued source evidence was accepted")
	}
	key, _ := CacheKey(input)
	if _, ok, err := cache.Lookup(key); err != nil || ok {
		t.Fatalf("invalid candidate entered cache: ok=%v err=%v", ok, err)
	}
}

func TestServiceNoVerifiedHypothesisRunsNonActionableStage(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "no repository target",
		results: []string{`{"version":1,"hypotheses":[]}`, nonActionableJSON()},
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80},
			{Name: "grep_repo", ContentBytes: len("applyFix")},
		},
	}
	service, input, browser, _ := serviceFixture(t, model)
	got, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil {
		t.Fatal(err)
	}
	if model.finalCalls != 2 || len(got.Entry.Result.Hypotheses) != 0 || got.Entry.Result.NonActionable == nil || got.Entry.Result.NonActionable.NonActionableReason != NonActionableInsufficientEvidence {
		t.Fatalf("calls=%d result=%+v", model.finalCalls, got.Entry.Result)
	}
}

func TestServiceRejectedNonemptyHypothesisRunsNonActionableStage(t *testing.T) {
	var extraction TargetExtraction
	if err := json.Unmarshal([]byte(actionableJSON()), &extraction); err != nil {
		t.Fatal(err)
	}
	extraction.Hypotheses[0].Target = &SymbolAdditionCandidate{Kind: CandidateSymbolAddition, Path: "controllers/reconcile.go", Symbol: "unsupportedSymbol"}
	extraction.Hypotheses[0].RelationshipReason = "diagnostic unsupported symbol"
	raw, _ := json.Marshal(extraction)
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "unsupported symbol only",
		results: []string{string(raw), nonActionableJSON()},
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80},
			{Name: "grep_repo", ContentBytes: len("unsupportedSymbol")},
		},
	}
	service, input, browser, _ := serviceFixture(t, model)
	got, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil {
		t.Fatal(err)
	}
	if model.finalCalls != 2 || len(got.Entry.Result.Hypotheses) != 1 || got.Entry.Result.NonActionable == nil {
		t.Fatalf("calls=%d result=%+v", model.finalCalls, got.Entry.Result)
	}
	verifier, _ := NewVerifier(service.source)
	verified, err := verifier.Verify(t.Context(), input, got.Entry, browser)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Classification != ClassificationInsufficientEvidence || verified.Proposal != nil {
		t.Fatalf("verified=%+v", verified)
	}
	view := safeOperationView(OperationRef{CausalGroupID: input.CausalGroupID, CausalGroupHash: input.CausalGroupHash}, verified, got.Entry.Provenance.CompletedAt)
	if view.State != models.PatternRemediationInsufficientEvidence || view.Target != nil {
		t.Fatalf("view=%+v", view)
	}
}

func TestServiceTargetExtractionInitialFailureRepairSuccessTelemetry(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence",
		results: []string{`{"version":0}`, actionableJSON()},
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80},
			{Name: "grep_repo", ContentBytes: len("applyFix")},
		},
	}
	service, input, browser, _ := serviceFixture(t, model)
	got, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil {
		t.Fatal(err)
	}
	metrics := got.Entry.Provenance.Metrics
	if metrics.TargetExtractionModelRequests == nil || *metrics.TargetExtractionModelRequests != 2 ||
		metrics.TargetExtractionProviderAttempts == nil || *metrics.TargetExtractionProviderAttempts != 2 ||
		metrics.TargetExtractionRepairCount == nil || *metrics.TargetExtractionRepairCount != 1 ||
		metrics.TargetExtractionFinalAttempt != string(ai.StructuredAttemptResponseFormat) {
		t.Fatalf("metrics=%+v", metrics)
	}
}

func TestServiceTargetExtractionInitialAndRepairFailurePreservesFinalCode(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence",
		results: []string{`{"version":0,"hypotheses":[]}`, `{"version":0,"hypotheses":[]}`},
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80},
			{Name: "grep_repo", ContentBytes: len("applyFix")},
		},
	}
	service, input, browser, cache := serviceFixture(t, model)
	_, err := service.Investigate(t.Context(), input, browser, false)
	if err == nil {
		t.Fatal("invalid initial and repair extraction succeeded")
	}
	details, ok := FailureDetailsOf(err)
	if !ok || details.Category != FailureTargetExtractionValidation || details.Phase != PhaseTargetExtractionRepair || details.ValidationCode != "invalid_version" || details.DiagnosticErrorCode() != "target_extraction_invalid_version" {
		t.Fatalf("details=%+v ok=%v err=%v", details, ok, err)
	}
	if len(details.StructuredAttempts) != 2 || details.StructuredAttempts[0].Phase != string(PhaseTargetExtractionInitial) || details.StructuredAttempts[1].Phase != string(PhaseTargetExtractionRepair) {
		t.Fatalf("attempts=%+v", details.StructuredAttempts)
	}
	key, _ := CacheKey(input)
	record, ok := cache.state.Failures[key]
	if !ok || record.Category != FailureTargetExtractionValidation || record.Phase != PhaseTargetExtractionRepair || record.ValidationCode != "invalid_version" || record.Code != "target_extraction_invalid_version" || len(record.StructuredAttempts) != 2 {
		t.Fatalf("record=%+v ok=%v", record, ok)
	}
}

func TestServiceNonActionableAssessmentFailureIsSeparate(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "no repository target",
		results: []string{`{"version":1,"hypotheses":[]}`, `{"version":0,"cause_assessment":"inconclusive","reason":"no target","evidence_ids":[],"non_actionable_reason":"insufficient_evidence"}`, `{"version":0,"cause_assessment":"inconclusive","reason":"no target","evidence_ids":[],"non_actionable_reason":"insufficient_evidence"}`},
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80},
			{Name: "grep_repo", ContentBytes: len("applyFix")},
		},
	}
	service, input, browser, cache := serviceFixture(t, model)
	_, err := service.Investigate(t.Context(), input, browser, false)
	if err == nil {
		t.Fatal("invalid non-actionable assessment succeeded")
	}
	details, ok := FailureDetailsOf(err)
	if !ok || details.Category != FailureNonActionableAssessment || details.Phase != PhaseNonActionableAssessmentRepair || details.ValidationCode != "invalid_version" || details.DiagnosticErrorCode() != "non_actionable_assessment_invalid_version" {
		t.Fatalf("details=%+v ok=%v", details, ok)
	}
	key, _ := CacheKey(input)
	if record := cache.state.Failures[key]; record.Category != FailureNonActionableAssessment || record.Phase != PhaseNonActionableAssessmentRepair {
		t.Fatalf("record=%+v", record)
	}
}

func TestSuccessfulResultDigestIgnoresPrivateTelemetry(t *testing.T) {
	result := testNonActionableResult()
	before := ResultDigest(result)
	metrics := Metrics{
		TargetExtractionModelRequests: intPointer(6), TargetExtractionProviderAttempts: intPointer(6),
		TargetExtractionRepairCount: intPointer(1), TargetExtractionFinalAttempt: string(ai.StructuredAttemptPlainFallback),
	}
	_ = NewProvenance(testFrozenInput(), "model", "responses", "", EvidenceStats{}, metrics, time.Now())
	if after := ResultDigest(result); after != before {
		t.Fatalf("result digest changed with private telemetry: before=%s after=%s", before, after)
	}
}

func TestValidationErrorCodeCategories(t *testing.T) {
	tests := []struct {
		message string
		want    string
	}{
		{"decode remediation target extraction: malformed", "decode"},
		{"unknown field path repository", "unknown_field"},
		{"duplicate field \"version\"", "duplicate_field"},
		{"target extraction version must be the integer 1", "invalid_version"},
		{"candidate field path is missing", "candidate_missing_field"},
		{"candidate kind is invalid", "candidate_kind"},
		{"required-call candidate is invalid", "required_call_target"},
		{"prow environment candidate is invalid", "prow_environment_target"},
		{"configuration field is invalid", "configuration_target"},
		{"engine-issued source evidence ID is required", "missing_source_evidence"},
		{"evidence ID was not issued by the investigation ledger", "unknown_evidence_id"},
		{"duplicate evidence ID", "duplicate_evidence_id"},
		{"evidence catalog is invalid", "evidence_catalog"},
	}
	for _, tt := range tests {
		if got := validationErrorCode(errors.New(tt.message)); got != tt.want {
			t.Errorf("message=%q code=%q want=%q", tt.message, got, tt.want)
		}
	}
}

type structuredMetadataDecorator struct{ Model }

func (m *structuredMetadataDecorator) CompleteStructuredWithMetadata(ctx context.Context, system, user string, format ai.ResponseFormat, validate ai.StructuredValidator) (ai.StructuredCompletionMetadata, error) {
	model, ok := m.Model.(structuredCompletionModel)
	if !ok {
		err := m.Model.CompleteStructured(ctx, system, user, format, validate)
		metadata, _ := ai.StructuredCompletionFailureMetadata(err)
		return metadata, err
	}
	return model.CompleteStructuredWithMetadata(ctx, system, user, format, validate)
}

func TestServiceDecoratorPreservesCancellationAndDeadlineMetadata(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		category FailureCategory
		phase    Phase
	}{
		{name: "cancelled", err: context.Canceled, category: FailureCancelled, phase: PhaseTargetExtractionInitial},
		{name: "deadline", err: context.DeadlineExceeded, category: FailureTimeout, phase: PhaseTargetExtractionInitial},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &fakeModel{
				fingerprint: strings.Repeat("d", 16), memo: "evidence", finalErr: tt.err,
				toolEvents: []ai.ToolLoopEvent{
					{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
					{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80, ContentBytes: 80},
					{Name: "grep_repo", ContentBytes: len("applyFix")},
				},
			}
			service, input, browser, _ := serviceFixture(t, model)
			service.model = &structuredMetadataDecorator{Model: model}
			_, err := service.Investigate(t.Context(), input, browser, false)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err=%v", err)
			}
			details, ok := FailureDetailsOf(err)
			if !ok || details.Category != tt.category || details.Phase != tt.phase || len(details.StructuredAttempts) != 1 || model.finalCalls != 1 {
				t.Fatalf("details=%+v ok=%v calls=%d", details, ok, model.finalCalls)
			}
			attempt := details.StructuredAttempts[0]
			if attempt.Path != ai.StructuredAttemptResponseFormat || attempt.Outcome != ai.StructuredOutcomeProviderError || attempt.ProviderCategory != "request_transport" || attempt.ValidatorCalled {
				t.Fatalf("attempt=%+v", attempt)
			}
		})
	}
}

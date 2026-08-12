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
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type fakeModel struct {
	fingerprint string
	toolEvents  []ai.ToolLoopEvent
	memo        string
	result      string
	results     []string
	toolErr     error
	finalErr    error
	toolCalls   int
	finalCalls  int
}

func (m *fakeModel) ToolLoop(_ context.Context, _, _ string, _ *tools.Registry, _ []string, _ *tools.Env, opts ai.ToolLoopOptions) (string, error) {
	m.toolCalls++
	for _, event := range m.toolEvents {
		if opts.Observe != nil {
			opts.Observe(event)
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

func (*fakeModel) ModelName() string          { return "test-model" }
func (m *fakeModel) ModelFingerprint() string { return m.fingerprint }
func (*fakeModel) APIMode() string            { return ai.APIResponses }

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

func actionableJSON() string {
	input := testFrozenInput()
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
		Version: ResultVersion, CauseAssessment: CauseSupports,
		Reason: "the controller omits applyFix",
		Candidate: &RequiredCallCandidate{
			Kind: CandidateRequiredCall, Path: "controllers/reconcile.go", ContainingSymbol: "reconcile", RequiredCall: "applyFix",
		},
		EvidenceIDs: evidenceIDs,
	}
	encoded, _ := json.Marshal(result)
	return string(encoded)
}

func serviceFixture(t *testing.T, model *fakeModel) (*Service, FrozenInput, fakeBrowser, *Cache) {
	t.Helper()
	input := testFrozenInput()
	input.ProviderFingerprint = model.fingerprint
	cache, err := NewCache("", CacheOptions{Now: func() time.Time { return time.Date(2026, 8, 12, 2, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(model, fakeSource{files: map[string]string{
		"controllers/reconcile.go": serviceSourceContent,
	}}, cache, ServiceOptions{Now: func() time.Time { return time.Date(2026, 8, 12, 2, 0, 1, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	browser := fakeBrowser{files: map[string]string{"builds/1/log.txt": "missing transition\n", "builds/2/log.txt": "missing transition\n"}}
	return service, input, browser, cache
}

func TestServiceCachesEvidenceBackedTypedResult(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "The failed builds report a missing transition and reconcile returns without applyFix.",
		result: actionableJSON(),
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80},
		},
	}
	service, input, browser, _ := serviceFixture(t, model)
	got, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheHit || got.Entry.Result.Candidate == nil || got.Entry.Provenance.Evidence.SourceReads != 1 || got.Entry.Provenance.Evidence.ArtifactReads != 1 || len(got.Entry.EvidenceCatalog.Records) < 4 {
		t.Fatalf("result=%+v", got)
	}
	cached, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil || !cached.CacheHit || model.toolCalls != 1 || model.finalCalls != 1 {
		t.Fatalf("cached=%+v err=%v tool=%d final=%d", cached, err, model.toolCalls, model.finalCalls)
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
	if got.Entry.Result.Candidate != nil || got.Entry.Result.NonActionableReason == nil || *got.Entry.Result.NonActionableReason != NonActionableInsufficientEvidence || model.finalCalls != 0 {
		t.Fatalf("result=%+v finalCalls=%d", got, model.finalCalls)
	}
}

func TestServiceFailedRefreshPreservesAcceptedResult(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence", result: actionableJSON(),
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80},
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
	if err != nil || !ok || entry.Result.Candidate == nil || entry.LastFailure == nil {
		t.Fatalf("entry=%+v ok=%v err=%v", entry, ok, err)
	}
}

func TestServiceRepairsOneInvalidStructuredResult(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence",
		results: []string{`{"version":0}`, actionableJSON()},
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80},
		},
	}
	service, input, browser, _ := serviceFixture(t, model)
	got, err := service.Investigate(t.Context(), input, browser, false)
	if err != nil {
		t.Fatal(err)
	}
	if model.finalCalls != 2 || got.Entry.Provenance.Metrics.RepairCount != 1 || got.Entry.Result.Candidate == nil {
		t.Fatalf("calls=%d result=%+v", model.finalCalls, got)
	}
}

func TestServiceInvalidRefreshPreservesAcceptedResult(t *testing.T) {
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence", result: actionableJSON(),
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80},
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
	if err != nil || !ok || entry.Result.Candidate == nil || entry.LastFailure == nil || entry.LastFailure.Category != FailureInvalidResult {
		t.Fatalf("entry=%+v ok=%v err=%v", entry, ok, err)
	}
}

func TestServiceRejectsCandidateWithoutIssuedSourceEvidence(t *testing.T) {
	var result Result
	if err := json.Unmarshal([]byte(actionableJSON()), &result); err != nil {
		t.Fatal(err)
	}
	result.Candidate = &RequiredCallCandidate{
		Kind: CandidateRequiredCall, Path: "controllers/other.go", ContainingSymbol: "reconcile", RequiredCall: "applyFix",
	}
	encoded, _ := json.Marshal(result)
	model := &fakeModel{
		fingerprint: strings.Repeat("d", 16), memo: "evidence", result: string(encoded),
		toolEvents: []ai.ToolLoopEvent{
			{Name: "read_artifact", Path: "builds/1/log.txt", BytesFetched: 19},
			{Name: "read_repo_file", Path: "controllers/reconcile.go", BytesFetched: 80},
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

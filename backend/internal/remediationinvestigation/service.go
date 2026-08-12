package remediationinvestigation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/repotree"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

const maxEvidenceMemoBytes = 48 << 10

type Model interface {
	ToolLoop(context.Context, string, string, *tools.Registry, []string, *tools.Env, ai.ToolLoopOptions) (string, error)
	CompleteStructured(context.Context, string, string, ai.ResponseFormat, ai.StructuredValidator) error
	ModelName() string
	ModelFingerprint() string
	APIMode() string
}

type ServiceOptions struct {
	Timeout           time.Duration
	MaxIters          int
	ContextByteBudget int
	Now               func() time.Time
	UsageRecorder     *aiusage.Recorder
}

func (o ServiceOptions) normalized() ServiceOptions {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Minute
	}
	if o.MaxIters <= 0 {
		o.MaxIters = 10
	}
	if o.ContextByteBudget <= 0 {
		o.ContextByteBudget = 256 << 10
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

type Service struct {
	model  Model
	source sourceinvestigation.TreeReader
	cache  *Cache
	opts   ServiceOptions
}

type RunResult struct {
	Entry    CacheEntry
	CacheHit bool
}

func NewService(model Model, source sourceinvestigation.TreeReader, cache *Cache, options ServiceOptions) (*Service, error) {
	if model == nil || source == nil {
		return nil, fmt.Errorf("remediation investigation model and source reader are required")
	}
	return &Service{model: model, source: source, cache: cache, opts: options.normalized()}, nil
}

// Investigate runs one bounded read-only investigation. A private model
// candidate is only a proposal and never action eligibility.
func (s *Service) Investigate(ctx context.Context, input FrozenInput, browser artifacts.Browser, refresh bool) (RunResult, error) {
	if browser == nil {
		return RunResult{}, fmt.Errorf("remediation investigation artifact browser is required")
	}
	if input.ProviderFingerprint != s.model.ModelFingerprint() {
		return RunResult{}, fmt.Errorf("provider fingerprint does not match the configured model")
	}
	if err := ValidateFrozenInput(input); err != nil {
		return RunResult{}, err
	}
	key, err := CacheKey(input)
	if err != nil {
		return RunResult{}, err
	}
	bound := &boundSourceReader{reader: s.source, repository: input.InvestigationSource}
	sourceFiles, err := bound.ListTree(ctx)
	if err != nil {
		_ = s.cache.RecordFailure(key, FailureSourceUnavailable, err)
		return RunResult{}, fmt.Errorf("pinned investigation source is unavailable: %w", err)
	}
	if !refresh {
		entry, ok, err := s.cache.Lookup(key)
		if err != nil {
			return RunResult{}, err
		}
		if ok {
			return RunResult{Entry: entry, CacheHit: true}, nil
		}
	}

	started := s.opts.Now().UTC()
	ctx, operation := aiusage.Begin(ctx, s.opts.UsageRecorder, aiusage.Metadata{
		LogicalID: input.CausalGroupID, Origin: aiusage.OriginServer,
		Feature:          aiusage.FeatureRemediationInvestigation,
		ModelFingerprint: s.model.ModelFingerprint(), Model: s.model.ModelName(),
		Correlation: aiusage.Correlation{JobID: input.JobID}, StartedAt: started,
	})
	outcome := aiusage.OutcomeSuccess
	finish := func() aiusage.OperationUsage {
		if operation == nil {
			return aiusage.OperationUsage{}
		}
		return operation.Finish(outcome)
	}

	runCtx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	ledger := newEvidenceLedger()
	preparedArtifactEvidence, err := prepareArtifactEvidence(runCtx, input, browser, ledger)
	if err != nil {
		outcome = aiusage.OutcomeError
		finish()
		_ = s.cache.RecordFailure(key, FailureInvalidInput, err)
		return RunResult{}, err
	}
	registry := tools.NewRegistry()
	filesystem.Register(registry)
	repotree.Register(registry)
	enabled, err := registry.Enable([]string{filesystem.Group, repotree.Group})
	if err != nil {
		outcome = aiusage.OutcomeError
		finish()
		return RunResult{}, err
	}
	env := &tools.Env{Browser: browser, Repo: bound, Cache: tools.NewBoundedCache(128, 4<<20)}
	evidencePrompt, err := renderEvidencePrompt(input, preparedArtifactEvidence)
	if err != nil {
		outcome = aiusage.OutcomeError
		finish()
		return RunResult{}, err
	}
	memo, runErr := s.model.ToolLoop(runCtx, evidenceSystemPrompt(input.ConsumerPrompt), evidencePrompt, registry, enabled, env, ai.ToolLoopOptions{
		MaxIters: s.opts.MaxIters, SingleToolCall: true,
		ContextByteBudget: s.opts.ContextByteBudget, PropagateFinalizeError: true,
		RequiredTools: requiredSourceTools(input, sourceFiles), Observe: ledger.observe,
	})
	if runErr != nil {
		outcome = usageOutcomeForError(runErr)
		usage := finish()
		_ = usage
		_ = s.cache.RecordFailure(key, failureCategory(runErr), runErr)
		return RunResult{}, fmt.Errorf("remediation evidence phase failed: %w", runErr)
	}
	if len(memo) > maxEvidenceMemoBytes {
		memo = memo[:maxEvidenceMemoBytes]
	}

	catalog, err := s.buildEvidenceCatalog(runCtx, input, browser, ledger)
	if err != nil {
		outcome = aiusage.OutcomeError
		finish()
		_ = s.cache.RecordFailure(key, FailureInvalidResult, err)
		return RunResult{}, err
	}
	var result Result
	repairCount := 0
	if !ledger.gatePassed() {
		result = insufficientEvidenceResult(catalog, "The bounded investigation did not read both recurring-build evidence and pinned source content.")
	} else {
		finalPrompt, err := renderFinalPrompt(input, memo, catalog)
		if err != nil {
			outcome = aiusage.OutcomeError
			finish()
			return RunResult{}, err
		}
		lastValidationCode := "invalid_result"
		validate := func(raw json.RawMessage) error {
			topLevel := looksLikeResultCandidate(raw)
			candidate, err := DecodeResult(raw)
			if err != nil {
				if topLevel {
					lastValidationCode = validationErrorCode(err)
				}
				return err
			}
			if err := validateResultAgainstInput(candidate, input, catalog); err != nil {
				if topLevel {
					lastValidationCode = validationErrorCode(err)
				}
				return err
			}
			result = candidate
			return nil
		}
		finalizeErr := s.model.CompleteStructured(runCtx, finalSystemPrompt(), finalPrompt, resultFormat(), validate)
		if finalizeErr != nil {
			repairCount = 1
			repairPrompt := finalPrompt + "\n\nValidation feedback: the previous structured result failed with code " + lastValidationCode + ". Return the exact schema only. version must be the integer " + fmt.Sprint(ResultVersion) + ", candidate and non_actionable_reason must be mutually exclusive, evidence_ids must come from the supplied catalog, and no unknown fields are allowed."
			finalizeErr = s.model.CompleteStructured(runCtx, finalSystemPrompt(), repairPrompt, resultFormat(), validate)
		}
		if finalizeErr != nil {
			outcome = usageOutcomeForError(finalizeErr)
			finish()
			wrapped := &resultError{code: lastValidationCode, err: finalizeErr}
			_ = s.cache.RecordFailure(key, FailureInvalidResult, wrapped)
			return RunResult{}, fmt.Errorf("remediation finalization failed: %w", wrapped)
		}
	}
	if err := s.verifyEvidence(runCtx, input, browser, catalog, &result); err != nil {
		outcome = aiusage.OutcomeError
		finish()
		_ = s.cache.RecordFailure(key, FailureInvalidResult, err)
		return RunResult{}, err
	}
	usage := finish()
	completed := s.opts.Now().UTC()
	metrics := Metrics{
		ElapsedMs: int(completed.Sub(started).Milliseconds()), ModelRequests: usage.ModelRequests,
		ReportedRequests: usage.ReportedRequests, UnreportedRequests: usage.UnreportedRequests,
		CoverageCountsKnown: usage.CoverageCountsKnown, UsageInvalid: usage.UsageInvalid,
		Currency: usage.Currency, PricingHash: usage.PricingHash, InputTokens: usage.InputTokens,
		CachedInputTokens: usage.CachedInputTokens, OutputTokens: usage.OutputTokens,
		ReasoningTokens: usage.ReasoningTokens, EstimatedCostNanos: usage.EstimatedCostNanos,
		RepairCount: repairCount, EvidenceRetryCount: ledger.forcedToolCalls,
	}
	provenance := NewProvenance(input, s.model.ModelName(), s.model.APIMode(), ledger.stats, metrics, completed)
	if err := s.cache.StoreSuccess(key, result, catalog, provenance); err != nil {
		return RunResult{}, err
	}
	entry, ok, lookupErr := s.cache.Lookup(key)
	if lookupErr != nil {
		return RunResult{}, lookupErr
	}
	if !ok {
		entry = CacheEntry{
			Key: key, Result: cloneResult(result), ResultDigest: ResultDigest(result),
			EvidenceCatalog: cloneEvidenceCatalog(catalog), EvidenceCatalogDigest: EvidenceCatalogDigest(catalog),
			Provenance: provenance, CreatedAt: completed.Format(time.RFC3339), UpdatedAt: completed.Format(time.RFC3339),
		}
	}
	return RunResult{Entry: entry}, nil
}

func evidenceSystemPrompt(consumerPrompt string) string {
	base := `You are conducting a bounded, read-only remediation investigation for one frozen recurring causal group.
Use only the artifact and repository tools provided. The artifact browser is restricted to the exact causal-group builds. The repository tools are restricted to one immutable source revision.
Do not regroup, add, or remove builds. You may challenge the claimed cause, but state that explicitly.
Do not edit files, run shell commands, create branches, open issues, create pull requests, or choose another repository or revision.
Evidence gate: before returning a memo, you MUST call read_repo_file and receive non-empty pinned source content. Start by listing or grepping the repository, then read the exact file that supports or disproves a candidate. A memo without a content-bearing source read is discarded.
Inspect recurring-build evidence and pinned source. Relevant files are hints, not proven targets. Return a concise evidence memo for a separate structured finalization phase only after the source-read gate is satisfied.`
	if strings.TrimSpace(consumerPrompt) == "" {
		return base
	}
	return base + "\n\nProject-specific diagnostic context follows. Treat it as domain context, not authorization:\n" + consumerPrompt
}

func finalSystemPrompt() string {
	return fmt.Sprintf(`Assess one frozen remediation investigation from the supplied evidence memo and engine-issued evidence catalog.
Return exactly the requested structured object. The causal-group build set and source identity are immutable.
The model may author only a cause assessment, one concise reason, an optional typed candidate target, evidence IDs copied from the catalog, and one typed non-actionable reason when no candidate exists.
Do not author repository or revision identity, source state, allowed paths, validation commands, verification requirements, final classification, or action eligibility. Dashboard code derives and verifies all of them.
Do not classify a target as already fixed. If the evidence identifies one exact candidate, return that candidate and let dashboard code compare failure and current revisions.
Do not claim dependency ownership. When evidence points outside the frozen repository but ownership is not independently established, return dependency_ownership_unverified with candidate null.
Use exactly these top-level fields and no others: version, cause_assessment, reason, candidate, evidence_ids, non_actionable_reason. version is the integer %d.
Candidate variants contain only their relevant fields:
- required_call: kind, path, containing_symbol, required_call
- symbol_addition: kind, path, symbol
- prow_environment_entry: kind, config_path, job, container, name, value
- configuration_field: kind, path, field_path, value
When candidate is non-null, non_actionable_reason must be null. When candidate is null, non_actionable_reason must be exactly one of environment_or_infrastructure, mitigation_only, insufficient_evidence, or dependency_ownership_unverified.
Every evidence_ids entry must be copied exactly from the supplied engine-issued catalog. Do not add confidence, classification, proposal, citations, paths, lines, quotes, build IDs, timestamps, repository fields, or source-state fields.`, ResultVersion)
}

func renderEvidencePrompt(input FrozenInput, preparedArtifactEvidence []EvidenceRecord) (string, error) {
	view := struct {
		PatternID                string              `json:"pattern_id"`
		PatternHash              string              `json:"pattern_hash"`
		CausalGroupID            string              `json:"causal_group_id"`
		CausalGroupHash          string              `json:"causal_group_hash"`
		JobID                    string              `json:"job_id"`
		JobName                  string              `json:"job_name"`
		Recurrence               string              `json:"recurrence"`
		Group                    any                 `json:"causal_group"`
		Builds                   []BuildReference    `json:"builds"`
		Analyses                 []AnalysisReference `json:"analyses"`
		RelevantFiles            []string            `json:"relevant_files"`
		Source                   any                 `json:"source"`
		Policy                   DestinationPolicy   `json:"destination_policy"`
		PreparedArtifactEvidence []EvidenceRecord    `json:"prepared_artifact_evidence,omitempty"`
	}{
		PatternID: input.PatternID, PatternHash: input.PatternHash,
		CausalGroupID: input.CausalGroupID, CausalGroupHash: input.CausalGroupHash,
		JobID: input.JobID, JobName: input.JobName, Recurrence: string(input.Recurrence), Group: input.Group,
		Builds: input.Builds, Analyses: input.Analyses, RelevantFiles: input.RelevantFiles,
		Source: input.InvestigationSource, Policy: input.DestinationPolicy,
		PreparedArtifactEvidence: preparedArtifactEvidence,
	}
	encoded, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		return "", err
	}
	return "Investigate this exact frozen input. Artifact paths are under builds/<build-id>/... .\n" + string(encoded), nil
}

func renderFinalPrompt(input FrozenInput, memo string, catalog EvidenceCatalog) (string, error) {
	identity := struct {
		PatternID, PatternHash, CausalGroupID, CausalGroupHash string
		JobID, JobName                                         string
		Group                                                  any
		Source                                                 any
		EvidenceCatalog                                        EvidenceCatalog `json:"evidence_catalog"`
	}{
		PatternID: input.PatternID, PatternHash: input.PatternHash,
		CausalGroupID: input.CausalGroupID, CausalGroupHash: input.CausalGroupHash,
		JobID: input.JobID, JobName: input.JobName, Group: input.Group,
		Source: input.InvestigationSource, EvidenceCatalog: catalog,
	}
	encoded, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return "", err
	}
	return "Frozen identity and engine-issued evidence catalog:\n" + string(encoded) + "\n\nEvidence memo:\n" + memo, nil
}

func looksLikeResultCandidate(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return value["candidate"] != nil || value["non_actionable_reason"] != nil || value["version"] != nil
}

func insufficientEvidenceResult(catalog EvidenceCatalog, reason string) Result {
	evidenceID := ""
	for _, record := range catalog.Records {
		if record.Kind == EvidenceAnalysis {
			evidenceID = record.ID
			break
		}
	}
	if evidenceID == "" && len(catalog.Records) > 0 {
		evidenceID = catalog.Records[0].ID
	}
	nonActionable := NonActionableInsufficientEvidence
	return Result{
		Version: ResultVersion, CauseAssessment: CauseInconclusive,
		Reason: reason, Candidate: nil, EvidenceIDs: []string{evidenceID},
		NonActionableReason: &nonActionable,
	}
}

func usageOutcomeForError(err error) aiusage.Outcome {
	switch {
	case errors.Is(err, context.Canceled):
		return aiusage.OutcomeCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return aiusage.OutcomeUnavailable
	default:
		return aiusage.OutcomeError
	}
}

func failureCategory(err error) FailureCategory {
	switch {
	case errors.Is(err, context.Canceled):
		return FailureCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	default:
		return FailureProvider
	}
}

type boundSourceReader struct {
	reader     sourceinvestigation.TreeReader
	repository sourceinvestigation.Repository
	files      []string
	listed     bool
}

func (r *boundSourceReader) ListTree(ctx context.Context) ([]string, error) {
	if r.listed {
		return slices.Clone(r.files), nil
	}
	files, err := r.reader.ListFiles(ctx, r.repository)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		clean, pathErr := artifacts.SafePath(file)
		if pathErr != nil || clean == "" || clean != file {
			return nil, fmt.Errorf("source tree contains an unsafe path")
		}
	}
	r.files = slices.Clone(files)
	r.listed = true
	return slices.Clone(r.files), nil
}

func (r *boundSourceReader) ReadFile(ctx context.Context, file string) (string, bool, error) {
	clean, pathErr := artifacts.SafePath(file)
	if pathErr != nil || clean == "" || clean != file {
		return "", false, fmt.Errorf("unsafe source path")
	}
	content, err := r.reader.ReadFile(ctx, r.repository, file)
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}

type evidenceLedger struct {
	stats           EvidenceStats
	sourceReads     map[string]bool
	artifactReads   map[string]bool
	forcedToolCalls int
}

func newEvidenceLedger() *evidenceLedger {
	return &evidenceLedger{sourceReads: map[string]bool{}, artifactReads: map[string]bool{}}
}

func (l *evidenceLedger) observe(event ai.ToolLoopEvent) {
	l.stats.ToolCalls++
	if event.Forced {
		l.forcedToolCalls++
	}
	if event.Error {
		l.stats.ToolErrors++
		return
	}
	switch event.Name {
	case "list_repo_tree":
		l.stats.SourceLists++
	case "grep_repo":
		l.stats.SourceGreps++
	case "read_repo_file":
		if event.ContentBytes > 0 {
			l.stats.SourceReads++
			l.stats.SourceReadBytes += event.ContentBytes
			l.sourceReads[event.Path] = true
		}
	case "list_artifacts", "find_artifacts":
		l.stats.ArtifactLists++
	case "grep_artifact":
		l.stats.ArtifactGreps++
		if event.BytesFetched > 0 {
			l.stats.ArtifactReads++
			l.stats.ArtifactReadBytes += event.BytesFetched
			l.artifactReads[event.Path] = true
		}
	case "read_artifact", "tail_artifact":
		if event.BytesFetched > 0 {
			l.stats.ArtifactReads++
			l.stats.ArtifactReadBytes += event.BytesFetched
			l.artifactReads[event.Path] = true
		}
	}
}

func requiredSourceTools(input FrozenInput, sourceFiles []string) []ai.RequiredTool {
	fileSet := make(map[string]bool, len(sourceFiles))
	for _, file := range sourceFiles {
		fileSet[file] = true
	}
	hasUsableHint := false
	for _, file := range relevantFileHints(input) {
		if fileSet[file] {
			hasUsableHint = true
			break
		}
	}
	read := ai.RequiredTool{
		Name: "read_repo_file", MaxAttempts: 1, RequireContent: true,
		CorrectivePrompt: "The pinned-source read requirement is not satisfied. Call read_repo_file now with one exact source path and inspect non-empty content before answering.",
	}
	if hasUsableHint {
		return []ai.RequiredTool{read}
	}
	return []ai.RequiredTool{
		{
			Name: "list_repo_tree", MaxAttempts: 1,
			CorrectivePrompt: "No frozen relevant-file hint resolves at the pinned revision. Call list_repo_tree now to inspect the repository structure before choosing a source file.",
		},
		read,
	}
}

func relevantFileHints(input FrozenInput) []string {
	files := slices.Clone(input.RelevantFiles)
	for _, analysis := range input.Analyses {
		files = append(files, analysis.RelevantFiles...)
	}
	return files
}

func (l *evidenceLedger) gatePassed() bool {
	return l.stats.SourceReads > 0 && l.stats.ArtifactReads > 0
}

func validateResultAgainstInput(result Result, input FrozenInput, catalog EvidenceCatalog) error {
	if err := ValidateResult(result); err != nil {
		return err
	}
	if err := ValidateEvidenceCatalog(catalog); err != nil {
		return err
	}
	records, err := selectedEvidenceRecords(result.EvidenceIDs, catalog)
	if err != nil {
		return err
	}
	if result.Candidate == nil {
		return nil
	}
	targetPath := candidatePath(result.Candidate)
	for _, record := range records {
		if record.Kind == EvidenceSource && record.Source != nil && record.Source.Path == targetPath && record.Source.Repository == input.InvestigationSource {
			return nil
		}
	}
	return fmt.Errorf("candidate target path %s lacks an engine-issued source evidence ID", targetPath)
}

func selectedEvidenceRecords(ids []string, catalog EvidenceCatalog) ([]EvidenceRecord, error) {
	byID := make(map[string]EvidenceRecord, len(catalog.Records))
	for _, record := range catalog.Records {
		byID[record.ID] = record
	}
	records := make([]EvidenceRecord, 0, len(ids))
	for _, id := range ids {
		record, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("evidence ID %s was not issued by the investigation ledger", id)
		}
		records = append(records, record)
	}
	return records, nil
}

func (s *Service) buildEvidenceCatalog(ctx context.Context, input FrozenInput, browser artifacts.Browser, ledger *evidenceLedger) (EvidenceCatalog, error) {
	catalog := EvidenceCatalog{Version: EvidenceCatalogVersion}
	paths := make([]string, 0, len(ledger.sourceReads))
	for file := range ledger.sourceReads {
		paths = append(paths, file)
	}
	slices.Sort(paths)
	for _, file := range paths {
		content, err := s.source.ReadFile(ctx, input.InvestigationSource, file)
		if err != nil {
			return EvidenceCatalog{}, fmt.Errorf("reconstruct source evidence %s: %w", file, err)
		}
		record := EvidenceRecord{
			Kind: EvidenceSource,
			Source: &SourceEvidenceIdentity{
				Repository: input.InvestigationSource, Path: file, ContentDigest: HashText(content),
			},
		}
		record.ID = evidenceRecordID(record)
		catalog.Records = append(catalog.Records, record)
	}
	for _, analysis := range input.Analyses {
		record := EvidenceRecord{
			Kind: EvidenceAnalysis,
			Analysis: &AnalysisEvidenceIdentity{
				BuildID: analysis.BuildID, GeneratedAt: analysis.GeneratedAt, RootCauseDigest: HashText(analysis.RootCause),
			},
		}
		record.ID = evidenceRecordID(record)
		catalog.Records = append(catalog.Records, record)
	}
	artifactPaths := make([]string, 0, len(ledger.artifactReads))
	for file := range ledger.artifactReads {
		artifactPaths = append(artifactPaths, file)
	}
	slices.Sort(artifactPaths)
	for _, file := range artifactPaths {
		buildID, ok := artifactBuildID(file, input.Group.Builds)
		if !ok {
			return EvidenceCatalog{}, fmt.Errorf("artifact evidence %s does not belong to the frozen causal group", file)
		}
		content, err := readArtifactEvidence(ctx, browser, file)
		if err != nil {
			return EvidenceCatalog{}, err
		}
		record := EvidenceRecord{
			Kind: EvidenceArtifact,
			Artifact: &ArtifactEvidenceIdentity{
				BuildID: buildID, Path: file, ContentDigest: HashText(content),
			},
		}
		record.ID = evidenceRecordID(record)
		catalog.Records = append(catalog.Records, record)
	}
	catalog = canonicalEvidenceCatalog(catalog)
	if err := ValidateEvidenceCatalog(catalog); err != nil {
		return EvidenceCatalog{}, err
	}
	return catalog, nil
}

func (s *Service) verifyEvidence(ctx context.Context, input FrozenInput, browser artifacts.Browser, catalog EvidenceCatalog, result *Result) error {
	if result == nil {
		return fmt.Errorf("remediation investigation result is missing")
	}
	if err := validateResultAgainstInput(*result, input, catalog); err != nil {
		return err
	}
	records, err := selectedEvidenceRecords(result.EvidenceIDs, catalog)
	if err != nil {
		return err
	}
	for _, record := range records {
		switch record.Kind {
		case EvidenceSource:
			if record.Source == nil || record.Source.Repository != input.InvestigationSource {
				return fmt.Errorf("source evidence does not match the frozen source identity")
			}
			content, readErr := s.source.ReadFile(ctx, input.InvestigationSource, record.Source.Path)
			if readErr != nil || HashText(content) != record.Source.ContentDigest {
				return fmt.Errorf("source evidence %s could not be reconstructed", record.ID)
			}
		case EvidenceAnalysis:
			if !analysisEvidenceMatches(record, input.Analyses) {
				return fmt.Errorf("analysis evidence %s is not frozen evidence", record.ID)
			}
		case EvidenceArtifact:
			if err := verifyArtifactEvidence(ctx, browser, record); err != nil {
				return err
			}
		}
	}
	return nil
}

func analysisEvidenceMatches(record EvidenceRecord, analyses []AnalysisReference) bool {
	if record.Kind != EvidenceAnalysis || record.Analysis == nil {
		return false
	}
	for _, analysis := range analyses {
		if analysis.BuildID == record.Analysis.BuildID && analysis.GeneratedAt == record.Analysis.GeneratedAt && HashText(analysis.RootCause) == record.Analysis.RootCauseDigest {
			return true
		}
	}
	return false
}

func prepareArtifactEvidence(ctx context.Context, input FrozenInput, browser artifacts.Browser, ledger *evidenceLedger) ([]EvidenceRecord, error) {
	var prepared []EvidenceRecord
	seen := map[string]bool{}
	for _, analysis := range input.Analyses {
		for _, citation := range analysis.Evidence {
			file := strings.TrimSpace(citation.Path)
			if !strings.HasPrefix(file, "builds/") {
				file = "builds/" + analysis.BuildID + "/" + file
			}
			content, err := readArtifactEvidence(ctx, browser, file)
			if err != nil {
				return nil, fmt.Errorf("frozen analysis artifact evidence is unavailable: %w", err)
			}
			lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
			if citation.LineStart < 1 || citation.LineEnd < citation.LineStart || citation.LineEnd > len(lines) {
				return nil, fmt.Errorf("frozen analysis artifact evidence has an invalid line range")
			}
			selected := strings.Join(lines[citation.LineStart-1:citation.LineEnd], "\n")
			if !strings.Contains(selected, strings.TrimSpace(citation.Quote)) {
				return nil, fmt.Errorf("frozen analysis artifact evidence quote does not match %s", file)
			}
			record := EvidenceRecord{
				Kind: EvidenceArtifact,
				Artifact: &ArtifactEvidenceIdentity{
					BuildID: analysis.BuildID, Path: file, ContentDigest: HashText(content),
				},
			}
			record.ID = evidenceRecordID(record)
			if err := validateEvidenceRecord(record); err != nil {
				return nil, fmt.Errorf("frozen analysis artifact evidence is invalid: %w", err)
			}
			if seen[record.ID] {
				continue
			}
			seen[record.ID] = true
			prepared = append(prepared, record)
			if !ledger.artifactReads[file] {
				ledger.stats.ToolCalls++
				ledger.stats.ArtifactReads++
				ledger.stats.ArtifactReadBytes += len(content)
				ledger.artifactReads[file] = true
			}
		}
	}
	return prepared, nil
}

func artifactBuildID(file string, buildIDs []string) (string, bool) {
	parts := strings.SplitN(file, "/", 3)
	if len(parts) != 3 || parts[0] != "builds" || parts[2] == "" || !slices.Contains(buildIDs, parts[1]) {
		return "", false
	}
	return parts[1], true
}

func readArtifactEvidence(ctx context.Context, browser artifacts.Browser, file string) (string, error) {
	const maxArtifactEvidenceBytes = 256 << 10
	content, size, err := browser.Read(ctx, file, 0, maxArtifactEvidenceBytes)
	if err != nil {
		return "", fmt.Errorf("read artifact evidence %s: %w", file, err)
	}
	if size > maxArtifactEvidenceBytes {
		return "", fmt.Errorf("artifact evidence %s exceeds bounded verification size", file)
	}
	return string(content), nil
}

func verifyArtifactEvidence(ctx context.Context, browser artifacts.Browser, record EvidenceRecord) error {
	if record.Kind != EvidenceArtifact || record.Artifact == nil {
		return fmt.Errorf("artifact evidence identity is missing")
	}
	content, err := readArtifactEvidence(ctx, browser, record.Artifact.Path)
	if err != nil {
		return err
	}
	if HashText(content) != record.Artifact.ContentDigest {
		return fmt.Errorf("artifact evidence content does not match %s", record.Artifact.Path)
	}
	return nil
}

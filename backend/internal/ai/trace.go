package ai

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/redact"
	"github.com/willie-yao/aster/backend/internal/statefile"
	"github.com/willie-yao/aster/backend/internal/textutil"
)

const (
	analysisTraceVersion   = 1
	analysisTraceMaxEvents = 128
	// analysisTraceMaxTraces bounds the rolling window the ledger retains across
	// fetch runs. Cache hits are not recorded, so this counts fresh analyses and
	// keeps the whole-file admin fetch to a workable size.
	analysisTraceMaxTraces     = 250
	analysisTraceMaxText       = 256
	analysisTraceMaxResponseID = 2048
)

// TraceEngine identifies the engine that produced a trace snapshot.
type TraceEngine struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	ImageTag string `json:"image_tag"`
}

// AnalysisTraceFile is the private, bounded trace snapshot for one fetch run.
type AnalysisTraceFile struct {
	Version       int             `json:"version"`
	GeneratedAt   string          `json:"generated_at"`
	RetainedSince string          `json:"retained_since,omitempty"`
	DroppedTraces int             `json:"dropped_traces,omitempty"`
	Engine        *TraceEngine    `json:"engine,omitempty"`
	Traces        []AnalysisTrace `json:"traces"`
}

// AnalysisTrace records sanitized control-flow metadata for one failure.
type AnalysisTrace struct {
	JobID           string       `json:"job_id"`
	BuildID         string       `json:"build_id"`
	TestName        string       `json:"test_name"`
	APIMode         string       `json:"api_mode"`
	Model           string       `json:"model,omitempty"`
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
	StartedAt       string       `json:"started_at"`
	RecordedAt      string       `json:"recorded_at,omitempty"`
	ElapsedMs       int          `json:"elapsed_ms"`
	Outcome         string       `json:"outcome"`
	ErrorCode       string       `json:"error_code,omitempty"`
	Truncated       bool         `json:"truncated,omitempty"`
	Events          []TraceEvent `json:"events"`
}

// DraftDecisionTrace records one content-free draft replacement decision.
type DraftDecisionTrace struct {
	Target                          string   `json:"target"`
	CurrentAttempt                  int      `json:"current_attempt"`
	CandidateAttempt                int      `json:"candidate_attempt"`
	CurrentRawHardRules             []string `json:"current_raw_hard_rules,omitempty"`
	CandidateRawHardRules           []string `json:"candidate_raw_hard_rules,omitempty"`
	CurrentRawSoftRules             []string `json:"current_raw_soft_rules,omitempty"`
	CandidateRawSoftRules           []string `json:"candidate_raw_soft_rules,omitempty"`
	CurrentPublishedHardRules       []string `json:"current_published_hard_rules,omitempty"`
	CandidatePublishedHardRules     []string `json:"candidate_published_hard_rules,omitempty"`
	CurrentPublishedSoftRules       []string `json:"current_published_soft_rules,omitempty"`
	CandidatePublishedSoftRules     []string `json:"candidate_published_soft_rules,omitempty"`
	CurrentPublishedHardIssues      int      `json:"current_published_hard_issues"`
	CandidatePublishedHardIssues    int      `json:"candidate_published_hard_issues"`
	CurrentPublishedMissingGroups   int      `json:"current_published_missing_groups"`
	CandidatePublishedMissingGroups int      `json:"candidate_published_missing_groups"`
	CurrentPublishedPunts           int      `json:"current_published_punts"`
	CandidatePublishedPunts         int      `json:"candidate_published_punts"`
	CurrentEvidenceRevision         int      `json:"current_evidence_revision"`
	CandidateEvidenceRevision       int      `json:"candidate_evidence_revision"`
	RootCauseMateriallyChanged      bool     `json:"root_cause_materially_changed"`
	RawSemanticRegression           bool     `json:"raw_semantic_regression"`
	PublishedStrictDominance        bool     `json:"published_strict_dominance"`
	CurrentQualityRefreshed         bool     `json:"current_quality_refreshed"`
	CurrentSupportedFacts           int      `json:"current_supported_facts"`
	CandidateSupportedFacts         int      `json:"candidate_supported_facts"`
	SupportedFactsRetained          int      `json:"supported_facts_retained"`
	SupportedFactsAdded             int      `json:"supported_facts_added"`
	SupportedFactsDropped           int      `json:"supported_facts_dropped"`
	SupportedCauseRegression        bool     `json:"supported_cause_regression"`
	ReplacementAccepted             bool     `json:"replacement_accepted"`
	ReplacementReason               string   `json:"replacement_reason"`
}

// EvidencePlanGroupTrace names one initial-plan evidence group without content.
type EvidencePlanGroupTrace struct {
	SkillID string `json:"skill_id"`
	GroupID string `json:"group_id,omitempty"`
}

// EvidencePlanTrace records content-free coverage of the initial ranked
// evidence plan at one decision point. UnreadGroups is the set the gate could
// still act on: unmet plan groups plus any group the draft's own prose newly
// required, both restricted to groups with a candidate path in this build.
type EvidencePlanTrace struct {
	Applicable     int                      `json:"applicable"`
	Satisfied      int                      `json:"satisfied"`
	Unavailable    int                      `json:"unavailable"`
	Unmet          int                      `json:"unmet"`
	DraftTriggered int                      `json:"draft_triggered,omitempty"`
	UnreadGroups   []EvidencePlanGroupTrace `json:"unread_groups,omitempty"`
}

type GrepCallTrace = tools.GrepCallObservation

// TraceEvent is one bounded, content-free analysis event.
type TraceEvent struct {
	Sequence                      int                 `json:"sequence"`
	ElapsedMs                     int                 `json:"elapsed_ms"`
	Kind                          string              `json:"kind"`
	Outcome                       string              `json:"outcome,omitempty"`
	ResponseID                    string              `json:"response_id,omitempty"`
	Status                        string              `json:"status,omitempty"`
	FinishReason                  string              `json:"finish_reason,omitempty"`
	Tool                          string              `json:"tool,omitempty"`
	DurationMs                    int                 `json:"duration_ms,omitempty"`
	Attempts                      int                 `json:"attempts,omitempty"`
	HTTPStatus                    int                 `json:"http_status,omitempty"`
	UsageReported                 bool                `json:"usage_reported,omitempty"`
	InputTokens                   int                 `json:"input_tokens,omitempty"`
	CachedInputTokens             int                 `json:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens         int                 `json:"cache_write_input_tokens,omitempty"`
	CacheWriteInputTokensReported bool                `json:"cache_write_input_tokens_reported,omitempty"`
	OutputTokens                  int                 `json:"output_tokens,omitempty"`
	ReasoningTokens               int                 `json:"reasoning_tokens,omitempty"`
	ReasoningEffort               string              `json:"reasoning_effort,omitempty"`
	EstimatedPromptTokens         int                 `json:"estimated_prompt_tokens,omitempty"`
	ContextLimitTokens            int                 `json:"context_limit_tokens,omitempty"`
	ReservedTokens                int                 `json:"reserved_tokens,omitempty"`
	MessageCount                  int                 `json:"message_count,omitempty"`
	ModelCallCount                int                 `json:"model_call_count,omitempty"`
	ToolCallCount                 int                 `json:"tool_call_count,omitempty"`
	CandidateCount                int                 `json:"candidate_count,omitempty"`
	ValidCount                    int                 `json:"valid_count,omitempty"`
	UniqueCandidateCount          int                 `json:"unique_candidate_count,omitempty"`
	IncompleteCount               int                 `json:"incomplete_count,omitempty"`
	ContractLikeRejectedCount     int                 `json:"contract_like_rejected_count,omitempty"`
	NormalizedCount               int                 `json:"normalized_count,omitempty"`
	ScanTruncated                 bool                `json:"scan_truncated,omitempty"`
	Bytes                         int                 `json:"bytes,omitempty"`
	WireRequestBytes              int                 `json:"wire_request_bytes,omitempty"`
	Elided                        int                 `json:"elided,omitempty"`
	Retry                         int                 `json:"retry,omitempty"`
	IssueCount                    int                 `json:"issue_count,omitempty"`
	CritiquePunts                 int                 `json:"critique_punts,omitempty"`
	CritiqueUnread                int                 `json:"critique_unread,omitempty"`
	CritiqueCitations             int                 `json:"critique_citations,omitempty"`
	CritiqueSkills                int                 `json:"critique_skills,omitempty"`
	CritiqueGroups                int                 `json:"critique_groups,omitempty"`
	CritiqueTransient             int                 `json:"critique_transient,omitempty"`
	CritiqueRules                 []string            `json:"critique_rules,omitempty"`
	CritiqueHardRules             []string            `json:"critique_hard_rules,omitempty"`
	CritiqueSoftRules             []string            `json:"critique_soft_rules,omitempty"`
	SemanticFindings              []string            `json:"semantic_findings,omitempty"`
	CacheRejectionReason          string              `json:"cache_rejection_reason,omitempty"`
	DraftDecision                 *DraftDecisionTrace `json:"draft_decision,omitempty"`
	EvidencePlan                  *EvidencePlanTrace  `json:"evidence_plan,omitempty"`
	Grep                          *GrepCallTrace      `json:"grep,omitempty"`
	RetryAdmitted                 bool                `json:"retry_admitted,omitempty"`
	RetryDeniedReason             string              `json:"retry_denied_reason,omitempty"`
	InitialIssueCount             int                 `json:"initial_issue_count,omitempty"`
	RevisedIssueCount             int                 `json:"revised_issue_count,omitempty"`
	NewEvidenceReads              int                 `json:"new_evidence_reads,omitempty"`
	RootCauseChanged              bool                `json:"root_cause_changed,omitempty"`
	SelectedAttempt               int                 `json:"selected_attempt,omitempty"`
	RetryDurationMs               int                 `json:"retry_duration_ms,omitempty"`
	RemainingTimeMs               int                 `json:"remaining_time_ms,omitempty"`
	ErrorCode                     string              `json:"error_code,omitempty"`
	ValidationCode                string              `json:"validation_code,omitempty"`
	StructuredPhase               string              `json:"structured_phase,omitempty"`
	StructuredAttempt             string              `json:"structured_attempt,omitempty"`
	StructuredOutcome             string              `json:"structured_outcome,omitempty"`
	ValidatorCalled               *bool               `json:"validator_called,omitempty"`
}

// TraceMetadata identifies one analysis without endpoint details.
type TraceMetadata struct {
	JobID           string
	BuildID         string
	TestName        string
	APIMode         string
	Model           string
	ReasoningEffort string
}

// TraceStore collects completed traces for one fetch run.
type TraceStore struct {
	mu      sync.Mutex
	engine  *TraceEngine
	traces  []AnalysisTrace
	dropped int
}

// NewTraceStore creates an empty trace store.
func NewTraceStore() *TraceStore { return &TraceStore{} }

// SetEngine records the producer identity for future snapshots.
func (s *TraceStore) SetEngine(engine TraceEngine) {
	if s == nil {
		return
	}
	engine.Version = strings.TrimSpace(engine.Version)
	engine.Commit = strings.TrimSpace(engine.Commit)
	engine.ImageTag = strings.TrimSpace(engine.ImageTag)
	if engine.Version == "" && engine.Commit == "" && engine.ImageTag == "" {
		return
	}
	if engine.Version == "" {
		engine.Version = "unknown"
	}
	if engine.Commit == "" {
		engine.Commit = "unknown"
	}
	if engine.ImageTag == "" {
		engine.ImageTag = "unknown"
	}
	s.mu.Lock()
	s.engine = &engine
	s.mu.Unlock()
}

// Start begins a trace session for one failure.
func (s *TraceStore) Start(meta TraceMetadata) *TraceSession {
	if s == nil {
		return nil
	}
	now := time.Now().UTC()
	return &TraceSession{store: s, record: newRunRecord(meta, now)}
}

// Snapshot returns a deterministic copy of all completed traces.
func (s *TraceStore) Snapshot() AnalysisTraceFile {
	out := AnalysisTraceFile{Version: analysisTraceVersion, GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), Traces: []AnalysisTrace{}}
	if s == nil {
		return out
	}
	s.mu.Lock()
	out.DroppedTraces = s.dropped
	if s.engine != nil {
		engine := *s.engine
		out.Engine = &engine
	}
	out.Traces = append(out.Traces, s.traces...)
	s.mu.Unlock()
	if out.DroppedTraces > 0 && len(out.Traces) > 0 {
		out.RetainedSince = out.Traces[oldestTraceIndex(out.Traces)].RecordedAt
	}
	sort.Slice(out.Traces, func(i, j int) bool { return traceBefore(out.Traces[i], out.Traces[j]) })
	return out
}

// Save writes the private trace snapshot atomically.
func (s *TraceStore) Save(path string) error {
	snapshot, err := s.snapshotWithinLimit(analysisTraceMaxFileBytes)
	if err != nil {
		return err
	}
	return statefile.WriteJSON(path, snapshot)
}

// runRecord is the in-memory, content-free event record for one analysis run.
// AnalysisTrace is projected from it only when the run finishes.
type runRecord struct {
	metadata   TraceMetadata
	startedAt  time.Time
	recordedAt string
	elapsedMs  int
	outcome    string
	errorCode  string
	events     []TraceEvent
	truncated  bool
}

func newRunRecord(meta TraceMetadata, startedAt time.Time) runRecord {
	return runRecord{
		metadata: TraceMetadata{
			JobID: traceText(meta.JobID), BuildID: traceText(meta.BuildID), TestName: traceText(meta.TestName),
			APIMode: traceText(meta.APIMode), Model: traceText(meta.Model), ReasoningEffort: safeReasoningEffortTrace(meta.ReasoningEffort),
		},
		startedAt: startedAt,
		events:    []TraceEvent{},
	}
}

func (r *runRecord) append(event TraceEvent) {
	event.ElapsedMs = int(time.Since(r.startedAt) / time.Millisecond)
	event.Kind = traceText(event.Kind)
	event.Outcome = traceText(event.Outcome)
	event.ResponseID = traceResponseID(event.ResponseID)
	event.Status = traceText(event.Status)
	event.FinishReason = traceText(event.FinishReason)
	event.ReasoningEffort = safeReasoningEffortTrace(event.ReasoningEffort)
	event.Tool = traceText(event.Tool)
	if event.ErrorCode != "" {
		event.ErrorCode = traceCode(event.ErrorCode)
	}
	if event.ValidationCode != "" {
		event.ValidationCode = traceCode(event.ValidationCode)
	}
	event.StructuredPhase = structuredPhaseCode(event.StructuredPhase)
	switch StructuredAttemptPath(event.StructuredAttempt) {
	case StructuredAttemptResponseFormat, StructuredAttemptForcedFunction, StructuredAttemptPlainFallback:
	default:
		event.StructuredAttempt = ""
	}
	switch StructuredAttemptOutcome(event.StructuredOutcome) {
	case StructuredOutcomeAccepted, StructuredOutcomeProviderError, StructuredOutcomeEmptyResponse,
		StructuredOutcomeMissingForcedFunction, StructuredOutcomeInvalidJSON, StructuredOutcomeValidatorRejected, StructuredOutcomeNoCandidate:
	default:
		event.StructuredOutcome = ""
	}
	event.SemanticFindings = sanitizeSemanticFindingClasses(event.SemanticFindings)
	if event.DraftDecision != nil {
		decision := *event.DraftDecision
		decision.Target = traceCode(decision.Target)
		decision.ReplacementReason = traceCode(decision.ReplacementReason)
		decision.CurrentRawHardRules = append([]string(nil), decision.CurrentRawHardRules...)
		decision.CandidateRawHardRules = append([]string(nil), decision.CandidateRawHardRules...)
		decision.CurrentRawSoftRules = append([]string(nil), decision.CurrentRawSoftRules...)
		decision.CandidateRawSoftRules = append([]string(nil), decision.CandidateRawSoftRules...)
		decision.CurrentPublishedHardRules = append([]string(nil), decision.CurrentPublishedHardRules...)
		decision.CandidatePublishedHardRules = append([]string(nil), decision.CandidatePublishedHardRules...)
		decision.CurrentPublishedSoftRules = append([]string(nil), decision.CurrentPublishedSoftRules...)
		decision.CandidatePublishedSoftRules = append([]string(nil), decision.CandidatePublishedSoftRules...)
		event.DraftDecision = &decision
	}
	if event.EvidencePlan != nil {
		event.EvidencePlan = sanitizeEvidencePlanTrace(*event.EvidencePlan)
	}
	if event.Grep != nil {
		event.Grep = sanitizeGrepCallObservation(*event.Grep)
	}
	event.Sequence = nextTraceSequence(r.events)
	if len(r.events) < analysisTraceMaxEvents {
		r.events = append(r.events, event)
		return
	}
	r.truncated = true
	if event.Kind != "draft_selection" {
		return
	}
	for i := range r.events {
		if r.events[i].Kind == "draft_selection" {
			continue
		}
		copy(r.events[i:], r.events[i+1:])
		r.events[len(r.events)-1] = event
		return
	}
}

func (r *runRecord) complete(outcome string, err error, recordedAt time.Time) {
	r.recordedAt = recordedAt.Format(time.RFC3339Nano)
	r.elapsedMs = int(recordedAt.Sub(r.startedAt) / time.Millisecond)
	r.outcome = traceText(outcome)
	if err != nil {
		r.errorCode = traceErrorCode(err)
	}
}

func (r runRecord) analysisTrace() AnalysisTrace {
	return normalizeAnalysisTrace(AnalysisTrace{
		JobID: r.metadata.JobID, BuildID: r.metadata.BuildID, TestName: r.metadata.TestName,
		APIMode: r.metadata.APIMode, Model: r.metadata.Model, ReasoningEffort: r.metadata.ReasoningEffort,
		StartedAt: r.startedAt.Format(time.RFC3339Nano), RecordedAt: r.recordedAt,
		ElapsedMs: r.elapsedMs, Outcome: r.outcome, ErrorCode: r.errorCode,
		Truncated: r.truncated, Events: append([]TraceEvent(nil), r.events...),
	})
}

// TraceSession records one analysis until Finish is called.
type TraceSession struct {
	mu       sync.Mutex
	store    *TraceStore
	record   runRecord
	finished bool
}

// Record appends one sanitized event while preserving draft decisions at the cap.
func (s *TraceSession) Record(event TraceEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.record.append(event)
}

// analysisTraceMaxEvidenceGroups bounds the group refs retained per event so a
// large plan cannot inflate a trace.
const analysisTraceMaxEvidenceGroups = 8

const analysisTraceMaxGrepRanges = 100

func sanitizeEvidencePlanTrace(plan EvidencePlanTrace) *EvidencePlanTrace {
	groups := plan.UnreadGroups
	if len(groups) > analysisTraceMaxEvidenceGroups {
		groups = groups[:analysisTraceMaxEvidenceGroups]
	}
	out := EvidencePlanTrace{
		Applicable:     plan.Applicable,
		Satisfied:      plan.Satisfied,
		Unavailable:    plan.Unavailable,
		Unmet:          plan.Unmet,
		DraftTriggered: plan.DraftTriggered,
	}
	for _, group := range groups {
		out.UnreadGroups = append(out.UnreadGroups, EvidencePlanGroupTrace{
			SkillID: traceText(group.SkillID),
			GroupID: traceText(group.GroupID),
		})
	}
	return &out
}

func sanitizeGrepCallObservation(observation tools.GrepCallObservation) *tools.GrepCallObservation {
	out := observation
	out.SelectorID = tools.ContentFreeSelectorID(observation.SelectorID)
	if observation.PathFilterRedacted {
		out.PathFilter = ""
	} else {
		filter, supplied, length, redacted := tools.ContentFreePathFilter(observation.PathFilter)
		out.PathFilter = filter
		out.PathFilterSupplied = supplied
		out.PathFilterLength = length
		out.PathFilterRedacted = redacted
	}
	out.ContextLines = max(observation.ContextLines, 0)
	out.MaxMatches = max(observation.MaxMatches, 0)
	out.MatchCount = max(observation.MatchCount, 0)
	out.FilesAttempted = max(observation.FilesAttempted, 0)
	out.FilesScanned = max(observation.FilesScanned, 0)
	out.FileReadErrors = max(observation.FileReadErrors, 0)
	switch observation.Outcome {
	case tools.GrepOutcomeMatched, tools.GrepOutcomeZeroMatches, tools.GrepOutcomeError:
	default:
		out.Outcome = tools.GrepOutcomeError
	}
	ranges := observation.ReturnedRanges
	if len(ranges) > analysisTraceMaxGrepRanges {
		ranges = ranges[:analysisTraceMaxGrepRanges]
		out.RangesTruncated = true
	}
	out.ReturnedRanges = make([]tools.GrepRangeObservation, 0, len(ranges))
	for _, item := range ranges {
		if item.LineStart <= 0 || item.LineEnd < item.LineStart {
			continue
		}
		out.ReturnedRanges = append(out.ReturnedRanges, tools.GrepRangeObservation{
			SelectorID: traceText(item.SelectorID), Path: traceText(item.Path), LineStart: item.LineStart, LineEnd: item.LineEnd,
		})
	}
	return &out
}

func sanitizeSemanticFindingClasses(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !semanticFindingClasses[value] || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func nextTraceSequence(events []TraceEvent) int {
	if len(events) == 0 {
		return 1
	}
	return events[len(events)-1].Sequence + 1
}

// Discard ends the session without storing it. Cache hits reuse an existing
// verdict and record no new evidence, so keeping them would evict real
// analyses from the bounded ledger.
func (s *TraceSession) Discard() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.finished = true
	s.mu.Unlock()
}

// Finish completes the trace and transfers it to the store.
func (s *TraceSession) Finish(outcome string, err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.record.complete(outcome, err, time.Now().UTC())
	completed := s.record.analysisTrace()
	s.mu.Unlock()

	s.store.Upsert(completed)
}

type analysisTraceContextKey struct{}

func withAnalysisTrace(ctx context.Context, trace *TraceSession) context.Context {
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, analysisTraceContextKey{}, trace)
}

func recordTrace(ctx context.Context, event TraceEvent) {
	if ctx == nil {
		return
	}
	trace, _ := ctx.Value(analysisTraceContextKey{}).(*TraceSession)
	trace.Record(event)
}

func traceText(s string) string {
	s = strings.TrimSpace(redact.Credentials(redact.URLs(s)))
	return textutil.Truncate(s, analysisTraceMaxText)
}

func safeReasoningEffortTrace(value string) string {
	effort, err := NormalizeReasoningEffort(value)
	if err != nil {
		return ""
	}
	return string(effort)
}

func traceResponseID(s string) string {
	s = strings.TrimSpace(redact.Credentials(redact.URLs(s)))
	return textutil.Truncate(s, analysisTraceMaxResponseID)
}

var traceCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func traceCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if traceCodePattern.MatchString(code) {
		return code
	}
	return "analysis_error"
}

func traceErrorCode(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "unsupported ai api"):
		return "unsupported_api"
	case strings.Contains(message, "function-calling support"), strings.Contains(message, "does not support function calling"):
		return "tools_unsupported"
	case strings.Contains(message, "marshal request"):
		return "request_marshal"
	case strings.Contains(message, "build request"):
		return "request_build"
	case strings.Contains(message, "post:"):
		return "request_transport"
	case strings.Contains(message, "decode response"):
		return "response_decode"
	case strings.Contains(message, "responses status"):
		return "provider_status"
	case strings.Contains(message, "chat returned"), strings.Contains(message, "responses returned"):
		return "http_error"
	case strings.Contains(message, "empty"):
		return "empty_response"
	default:
		return "analysis_error"
	}
}

package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/evidenceplan"
	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/ai/tools/repotree"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/models"
)

// AgenticMode is stored in models.AIAnalysis.Mode for agentic results. A cached
// entry with any other mode is stale.
const AgenticMode = "agentic"

const preliminaryCacheTTL = 5 * time.Minute

// ErrToolsUnsupported is returned from the agentic loop when the configured
// provider rejects function-calling on the first call. There is no tools-free
// fallback, so the affected failure is marked AI-unavailable for the run.
var ErrToolsUnsupported = errors.New("ai endpoint does not support function calling")

// ErrContextHeadroom means no safe provider request could be formed after compaction.
var ErrContextHeadroom = errors.New("agentic request exceeds context headroom")

// ErrMissingArtifactCitation is retained for callers that decode older private
// benchmark records. Current analysis publishes safe ungrounded drafts as
// preliminary instead of returning this error.
var ErrMissingArtifactCitation = errors.New("no validated artifact citation supports the analysis")

// ErrRejectedAnalysis means no safe structured analysis was available to publish.
var ErrRejectedAnalysis = errors.New("analysis result failed the safe publication contract")

// AgenticOptions is the resolved per-failure budget config. Build it once per
// fetcher run via project.AI.EffectiveAgentic and reuse it.
type AgenticOptions struct {
	MaxIters        int
	ModelByteBudget int
	GCSByteBudget   int
	Timeout         time.Duration

	// ContextByteBudget is the legacy compaction ceiling. Runtime wiring sets
	// it to RequestTokenBudget so request sizing remains conservative.
	ContextByteBudget int

	// ContextWindowTokens is the provider-advertised total context window.
	// RequestTokenBudget is the request-side share after fixed headroom.
	ContextWindowTokens int
	RequestTokenBudget  int

	// MinToolCalls is the minimum number of tool calls before a tools-free
	// final answer is accepted as cacheable. Defaults to 0 for no floor. The
	// loop nudges the model with a "you haven't investigated enough" user
	// message and skips the cache write for any final that lands below
	// the floor so the next run gets a fresh attempt.
	MinToolCalls int

	// MinGCSBytes is the minimum cumulative GCS bytes fetched via tool
	// calls before a tools-free final answer is accepted. Complements
	// MinToolCalls because tool-call count alone is gameable: weak models
	// can satisfy a calls floor with cheap list calls or tiny reads. Complete
	// initial evidence-plan coverage also satisfies this floor. Defaults to 0.
	MinGCSBytes int

	// CritiqueMaxRetries controls eligibility for one bounded deterministic repair.
	// 0 evaluates once without a critique repair request. Positive values remain
	// subject to headroom guards. CritiqueCachePolicy controls reuse independently.
	CritiqueMaxRetries int

	// CritiqueCachePolicy independently controls which findings block cache reuse.
	// Empty preserves the legacy max-retries behavior.
	CritiqueCachePolicy CritiqueCachePolicy

	// SingleToolCall caps the loop to one tool call per assistant turn. Extra
	// tool calls in a multi-call response are dropped after the first. Needed
	// for endpoints whose chat template rejects multiple tool calls per
	// assistant message. Defaults to false so providers that support parallel
	// tool calls keep their efficiency.
	SingleToolCall bool

	// SemanticJudge enables the second-line LLM judge that reviews an accepted
	// draft's reasoning for a fluent-but-wrong root cause (see semantic.go). It
	// runs at most once per analysis and only drives a re-prompt. Engine-owned
	// and set by the fetcher; not a project.yaml knob. Defaults to false so
	// deterministic-critique unit tests are not perturbed by the extra call.
	SemanticJudge bool
}

// SourceEvidenceObservation is private, content-free source range telemetry.
type SourceEvidenceObservation struct {
	SourceID  string
	Tool      string
	Path      string
	LineStart int
	LineEnd   int
}

type SourceEvidenceObserver func(SourceEvidenceObservation)

// DraftObservation is a value-only snapshot of one parseable analysis draft.
// The quality benchmark uses it to compare retries from the same investigation
// without persisting model content in analysis traces.
type DraftObservation struct {
	Attempt             int
	Phase               string
	Summary             string
	RootCause           string
	SuggestedFix        string
	IsTransient         bool
	Severity            string
	RelevantFiles       []string
	PuntCount           int
	UnreadCitationCount int
	CitationIssueCount  int
	MissingGroupCount   int
	TransientConflict   bool
	RuleIDs             []string
	MatchedSkillIDs     []string
	MissingGroups       []CritiqueEvidenceGroupRef
	UnavailableGroups   []CritiqueEvidenceGroupRef
	PublishedRuleIDs    []string
	PublishedHardRules  []string
	PublishedSoftRules  []string
	PublishedHardIssues int
	PublishedPuntCount  int
	PublishedMissing    int
	ToolCalls           int
	EvidenceReads       int
}

// DraftObserver receives parseable draft snapshots in attempt order. It is nil
// outside the opt-in quality benchmark.
type DraftObserver func(DraftObservation)

// DraftSelectionObserver receives the selected parseable attempt number. It is
// nil outside the opt-in quality benchmark.
type DraftSelectionObserver func(int)

type critiqueRetryBudget struct {
	max  int
	used int
}

type critiqueQuality struct {
	Passed               bool
	HardRules            []string
	SoftRules            []string
	HardIssueCount       int
	MissingEvidenceCount int
	PuntCount            int
}

type critiqueDraftCandidate struct {
	parsed                        analysisResponse
	content                       string
	providerItems                 []json.RawMessage
	rawQuality                    critiqueQuality
	quality                       critiqueQuality
	attempt                       int
	evidenceRevision              int
	createdEvidenceRevision       int
	supportedFacts                []supportedCausalFact
	semanticInitialFindingClasses []string
	semanticFindingClasses        []string
	semanticReviewPassed          bool
	semanticRevision              bool
}

type draftReplacementDecision struct {
	accepted                 bool
	reason                   string
	rootCauseChanged         bool
	rawSemanticRegression    bool
	publishedStrictDominance bool
	currentQualityRefreshed  bool
	currentSupportedFacts    int
	candidateSupportedFacts  int
	supportedFactsRetained   int
	supportedFactsAdded      int
	supportedFactsDropped    int
	supportedCauseRegression bool
}

const (
	draftReasonCandidatePublishedDominates  = "candidate_published_dominates"
	draftReasonCandidatePublishedHard       = "candidate_has_published_hard_failure"
	draftReasonCandidateSemanticRegression  = "candidate_adds_semantic_regression"
	draftReasonCandidateRootWithoutEvidence = "candidate_changes_root_without_evidence"
	draftReasonCandidateEvidenceBackedRoot  = "candidate_evidence_backed_root_change"
	draftReasonCandidateSemanticFindings    = "candidate_has_semantic_findings"
	draftReasonCandidateSemanticUnavailable = "candidate_semantic_review_unavailable"
	draftReasonCandidatePolicyUnaccepted    = "candidate_not_accepted_for_policy"
	draftReasonCandidateDropsSupportedCause = "candidate_drops_supported_cause"
	draftReasonCandidateNotBetter           = "candidate_not_strictly_better"
	draftReasonTiePreservesEarlier          = "tie_preserves_earlier"
	draftReasonSemanticTieReplaces          = "semantic_tie_replaces"
	draftReasonFallbackPromoted             = "fallback_promoted"
)

func (b *critiqueRetryBudget) available() bool {
	return b != nil && b.used < b.max
}

func (b *critiqueRetryBudget) admit() (int, bool) {
	if b == nil {
		return 0, false
	}
	if !b.available() {
		return b.used, false
	}
	b.used++
	return b.used, true
}

// artifactTreeMaxPaths caps how many artifact paths the seed lists, bounding
// the prompt size on builds with very large artifact trees.
const artifactTreeMaxPaths = 500

// initialArtifactTreeMaxPaths bounds the one recursive listing shared by the
// initial prompt seed, evidence plan, and complete-tree absence checks.
const initialArtifactTreeMaxPaths = 5000

// Bound the artifact-tree seed by bytes, not just path count: a few hundred
// deeply-nested paths can still overflow the window on iter 1. Budget is this
// fraction of the context budget, or the static fallback when the endpoint
// reports no window.
const artifactTreeSeedBudgetPct = 15
const artifactTreeSeedFallbackBytes = 48 * 1024

// artifactTreeSeedBytes is the seed's byte ceiling: a fraction of the detected
// context budget, falling back from ContextByteBudget to ModelByteBudget and
// then to the static fallback.
func artifactTreeSeedBytes(opts AgenticOptions) int {
	base := opts.ContextByteBudget
	if base <= 0 {
		base = opts.ModelByteBudget
	}
	if base <= 0 {
		return artifactTreeSeedFallbackBytes
	}
	return base * artifactTreeSeedBudgetPct / 100
}

// artifactTreeNoiseExt are file extensions the seed drops before capping:
// non-text artifacts the model cannot usefully read, so excluding them leaves
// more of the path budget for diagnostic logs.
var artifactTreeNoiseExt = map[string]bool{
	".png": true, ".svg": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".gz": true, ".tar": true, ".tgz": true, ".zip": true, ".bz2": true,
}

// limitToolCalls returns the tool calls the loop should execute and echo this
// turn. With single=true and more than one call, only the first is kept so the
// echoed assistant message stays compatible with single-call templates. The
// dropped count is returned for logging. The model can re-request dropped calls.
func limitToolCalls(calls []modelToolCall, single bool) (kept []modelToolCall, dropped int) {
	if single && len(calls) > 1 {
		return calls[:1], len(calls) - 1
	}
	return calls, 0
}

// evidenceInjectionMaxArtifacts caps how many artifacts are fetched per
// critique-failure round, bounding the context the injection adds.
const evidenceInjectionMaxArtifacts = 4

// evidenceTreeMaxPaths bounds the single recursive listing that resolves cited
// basenames and skill-required patterns to real artifact paths, capping the GCS
// list cost of one injection.
const evidenceTreeMaxPaths = 1000

// evidenceInjectionPerArtifactBytes caps the bytes injected per artifact.
const evidenceInjectionPerArtifactBytes = 8 * 1024

// agenticToolBudget caps bytes returned to the model by any single tool
// call. Keeps one runaway response from eating the whole ModelByteBudget.
// 32 KB leaves room for a useful log excerpt plus the JSON envelope.
const agenticToolBudget = 32 * 1024

// agToolDocs is the tool-usage strategy section appended to the system
// prompt by the agentic loop. Tool names + descriptions reach the model
// via the schema array; this section adds investigation strategy: drill
// into specifics, don't punt to the user, stop only when evidence is
// genuinely exhausted, not at the first plausible symptom.
const agToolDocs = `

## Tool usage strategy

You have a set of tools for browsing the build's GCS artifact tree (see the
tools field of this request for names, descriptions, and parameters).

1. Start by listing the build root to see what's there.
2. Triage for a known transient FIRST. If the failure matches a transient class named in the project-specific knowledge above (infrastructure flake such as API throttling, quota exhaustion, transient DNS, image-pull backoff, API server / etcd still forming, node not yet registered, or a cleanup-phase deadline), set is_transient=true and stop drilling for a code root cause. Do NOT reclassify it as a real bug or manufacture a remediation for infrastructure flake; doing so produces a false "real bug" verdict. Stopping the drill does not mean returning an empty remediation: when the artifacts you already read show a durable resilience improvement that would have absorbed the flake (a missing retry or backoff on a retryable status, a wait or timeout budget shorter than the observed duration, a missing readiness guard), name it in suggested_fix and set cause_location when that evidence establishes the repository that owns it. When that improvement belongs to the project's own repository, verify the file with the repository tools before naming it in relevant_files. Only continue to the deep investigation below when the failure is not a known transient. Some symptoms (x509 / "certificate signed by unknown authority", webhook or join timeouts, "connection refused", "context deadline exceeded") occur in BOTH transient flakes and real bugs; the error string alone does not decide. For these, classify by EVIDENCE, not by the string: an error that recovered (later calls succeeded, or the resource eventually reached its desired state), or that the project knowledge names as a known flake, is transient; an error with a specific upstream cause in the related logs (a concrete bootstrap, config, cert, or code failure) is a real bug. When unsure, drill the related logs (the resource's own logs and the owning controller's log; the project-specific section names them) to find that specific cause before deciding; absence of a specific cause favors transient.
3. For multi-MB build-logs, ALWAYS use grep_artifact (with wide surrounding context, e.g. ctx=20), never read_artifact or tail_artifact.
4. Drill into the most relevant named resources. If your current best causal lead depends on a specific resource (a failing Machine, Pod, Node, VM, container, controller, or owning workload), read that resource's own artifacts before finalizing. Do not chase every resource name mentioned in passing; pick the 1-3 most directly tied to the failure. Examples: a failing resource X → read its manifest/status conditions, events, owner-controller log filtered for "X", and any resource-specific runtime logs. The project-specific section names the exact artifact paths these live at. Stopping at the first plausible symptom is the most common failure mode of this tool; treat each symptom as a lead, not the answer. Before settling on a cause, compare specific request/list/watch/assertion failures with repeated timeout, readiness, and cleanup noise. Prefer a specific failure only when its timing and mechanism explain the downstream symptom. Treat a later successful operation as counterevidence against assigning that component ownership; if no other cited error proves ownership, keep the remaining boundary unresolved.
5. Investigation is YOUR job, not the user's. suggested_fix must be a concrete remediation action (a code change, config edit, command to run, retry, redeploy, rollback, operational fix). It must NOT be a diagnostic or information-gathering task. If the sentence's primary purpose is to learn more (check, verify, investigate, ensure, inspect, examine, confirm, audit, review, look into, determine), it belongs in your tool work BEFORE finalizing, not in suggested_fix. A "then validate by ..." clause is fine, but only after a concrete remediation. If after following the directly relevant artifact leads you still cannot identify a concrete remediation, say so explicitly in suggested_fix and include all three of: (a) the strongest fact you established, (b) the specific artifacts/logs you consulted, (c) the exact missing evidence that prevents a remediation. Do not invoke this escape hatch if any standard remediation or best-evidence operational action is supported by the artifacts you read.
6. Cite actual paths and quoted log lines in your final answer. Do not speculate; if evidence is incomplete, state what is known and what remains unclear.
7. Watch the remaining_model_bytes and remaining_gcs_bytes returned with each tool result; stop browsing and produce the final JSON answer before they hit zero. A tool result that also returns unread_evidence_groups is telling you the required evidence plan still has groups you have not read; read one of their candidate paths before finalizing, or state in root_cause why that evidence is not present in this build.
8. When repository tools are available, establish the runtime cause from build artifacts first. Then use read_repo_file or grep_repo at the tested commit to trace the project caller or verify a source file before naming it.

Before finalizing, self-check:
- Before drilling, did I rule out a known-transient class named in the project knowledge?
- If I set is_transient=true, did I still name the durable resilience improvement the evidence supports, or state that none is supported?
- For an overloaded symptom (cert/x509, webhook or join timeout, connection refused, context deadline), did I check whether it recovered or has a specific upstream cause before deciding is_transient?
- Did I identify the earliest upstream cause of a non-transient failure, not just the terminal symptom?
- Did I clear every group the tool results reported in unread_evidence_groups, or explain why that evidence is absent?
- For a non-transient failure, did I read the artifacts for the 1-3 named resources most central to it?
- Is suggested_fix a remediation action, not a request for more investigation?

A confident "I found X by reading Y at line Z" answer always beats "you should check X". The difference between a useful diagnosis and a useless one is whether the agent did the drilling itself or passed the work back to the user.`

// agenticSourceCatalogSection enumerates immutable source selectors and marks
// the project-owned primary source.
func agenticSourceCatalogSection(catalog *tools.SourceCatalog) string {
	if catalog == nil {
		return ""
	}
	primary, ok := catalog.Primary()
	if !ok {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nThe project under test is the GitHub repository ")
	b.WriteString(primary.Owner + "/" + primary.Name)
	b.WriteString(". Source tools require a source_id from this immutable source catalog:")
	for _, source := range catalog.Sources() {
		b.WriteString("\n- source_id `")
		b.WriteString(source.ID)
		b.WriteString("`: ")
		b.WriteString(source.Owner + "/" + source.Name + " @ " + source.Revision)
		if source.ID == catalog.PrimaryID() {
			b.WriteString(" (primary project source)")
		}
	}
	b.WriteString("\nUse the matching source_id in every repository tool call. Only repository-relative paths read from the primary project source can be published as project-owned relevant_files. Other catalog entries are dependencies this project only consumes.")
	return b.String()
}

func agenticSourceContextSection(catalog *tools.SourceCatalog, owner, name string) string {
	if catalog != nil {
		return agenticSourceCatalogSection(catalog)
	}
	return agenticSourceRepoSection(owner, name)
}

// agenticSourceRepoSection names the project when no immutable source is available.
func agenticSourceRepoSection(owner, name string) string {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" {
		return ""
	}
	return "\n\nThe project under test is the GitHub repository " + owner + "/" + name +
		". Code from any other repository is a dependency this project only consumes."
}

// agForceFinalizePrompt is the user message that forces a JSON-only final round
// when the model has exhausted iterations or returned text without valid JSON.
const agForceFinalizePrompt = `Stop calling tools. Produce the final JSON
analysis now using the evidence you have already gathered, following the
"Response format" section of the system prompt exactly. If you did not find a
definitive root cause, say so explicitly in root_cause (e.g. "Investigation
reached budget; best-evidence hypothesis is X based on Y") rather than
continuing to investigate.

Output ONLY the JSON object: no prose, no explanation, no markdown fences.
Your entire response must start with { and end with }.`

const analysisFinalizeToolName = "submit_analysis"

func analysisFinalizeFormat() ResponseFormat {
	stringArray := func() map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	}
	return ResponseFormat{
		Name:        analysisFinalizeToolName,
		Description: "Submit one structured failure analysis.",
		Schema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"summary":            map[string]any{"type": "string"},
				"is_transient":       map[string]any{"type": "boolean"},
				"root_cause":         map[string]any{"type": "string"},
				"severity":           map[string]any{"type": "string", "enum": []string{"Critical", "High", "Medium", "Low"}},
				"suggested_fix":      map[string]any{"type": "string"},
				"relevant_files":     stringArray(),
				"search_suggestions": stringArray(),
				"evidence_citations": map[string]any{
					"type": "array", "maxItems": 20,
					"items": map[string]any{
						"type": "object", "additionalProperties": false,
						"properties": map[string]any{
							"path":       map[string]any{"type": "string"},
							"line_start": map[string]any{"type": "integer", "minimum": 1},
							"line_end":   map[string]any{"type": "integer", "minimum": 1},
							"quote":      map[string]any{"type": "string"},
						},
						"required": []string{"path", "line_start", "line_end", "quote"},
					},
				},
			},
			"required": []string{"summary", "is_transient", "root_cause", "severity", "suggested_fix", "relevant_files", "search_suggestions", "evidence_citations"},
		},
	}
}

// formatFloorsNudge builds the user message appended after a tools-free
// model response when one or both per-project floors are unmet. Mentions
// only the axes that are actually unmet so a project configuring only
// MinToolCalls doesn't see a misleading "0 KB" complaint.
func formatFloorsNudge(state *agentState, opts AgenticOptions) string {
	var unmet []string
	floors := evalFloors(state, opts)
	if floors.callsUnmet {
		unmet = append(unmet, fmt.Sprintf("only %d tool call(s) but need at least %d", state.calls, opts.MinToolCalls))
	}
	if floors.gcsUnmet {
		unmet = append(unmet, fmt.Sprintf("only %d KB of GCS evidence but need at least %d KB", state.gcsBytes/1024, opts.MinGCSBytes/1024))
	}
	return fmt.Sprintf(`You attempted to finalize after %s, which this project requires before a final answer is accepted. Before responding:

1. List the build root with list_artifacts to see what's actually there.
2. For multi-MB build logs, use grep_artifact (not read_artifact) and ask for many matches with wide surrounding context so you see chains of causation, not isolated lines.
3. When build-log.txt shows an error, cross-reference the corresponding timestamp in the relevant component or controller log (the project-specific section names where these live). Symptoms surfaced in build-log are often downstream of root causes in the controller.
4. Don't accept the first plausible explanation. Common terminal symptoms (for example kubelet/API-server timeouts, context deadline exceeded, NotReady nodes) usually have earlier upstream causes such as webhook/cert problems, leader-election loss, image pull failures, or missing dependencies. Search nearby logs before concluding.
5. Cite specific file paths and log line numbers in your root_cause. Include enough evidence to explain the causal chain, not just the surface error.

If after this investigation the evidence is genuinely inconclusive, say so explicitly in root_cause rather than speculating.`, strings.Join(unmet, " and "))
}

const (
	// maxEvidenceNudges bounds how many times the loop reopens a finalize
	// attempt because available planned evidence is still unread.
	maxEvidenceNudges = 2
	// evidenceNudgeMaxGroups bounds how many unread groups one nudge names.
	evidenceNudgeMaxGroups = 3
	// evidenceNudgeMaxCandidates bounds candidate paths listed per named group.
	evidenceNudgeMaxCandidates = 3
)

// evidenceGateOutcome is the recorded reason the finalize branch either
// reopened the investigation or accepted the draft with evidence still unread.
type evidenceGateOutcome string

const (
	evidenceGateNudge           evidenceGateOutcome = "nudge"
	evidenceGateCovered         evidenceGateOutcome = "covered"
	evidenceGateNoProgress      evidenceGateOutcome = "no_progress"
	evidenceGateNudgeBudget     evidenceGateOutcome = "nudge_budget"
	evidenceGateBudgetExhausted evidenceGateOutcome = "budget_exhausted"
	evidenceGateTimeHeadroom    evidenceGateOutcome = "time_headroom"
)

// evidenceGate is the anti-thrash state for reopening a finalize attempt while
// available planned evidence remains unread.
type evidenceGate struct {
	nudges           int
	nudgedAtRevision int
}

// decide reports whether the loop should reopen the investigation for the
// unread groups, and why. Only groups with resolved candidate paths are
// actionable; a group with no candidate is deterministically unavailable.
func (g *evidenceGate) decide(state *agentState, unread []skills.PlanGroupRef) (evidenceGateOutcome, []skills.PlanGroupRef) {
	actionable := make([]skills.PlanGroupRef, 0, len(unread))
	for _, group := range unread {
		if len(group.CandidatePaths) > 0 {
			actionable = append(actionable, group)
		}
	}
	switch {
	case len(actionable) == 0:
		return evidenceGateCovered, nil
	case state.budgetExhausted:
		return evidenceGateBudgetExhausted, actionable
	case g.nudges >= maxEvidenceNudges:
		return evidenceGateNudgeBudget, actionable
	case state.evidenceRevision <= g.nudgedAtRevision:
		return evidenceGateNoProgress, actionable
	case time.Until(state.deadline) < 2*state.recentModelRequest+critiqueFinalizationReserve:
		return evidenceGateTimeHeadroom, actionable
	}
	return evidenceGateNudge, actionable
}

func (g *evidenceGate) recordNudge(state *agentState) {
	g.nudges++
	g.nudgedAtRevision = state.evidenceRevision
}

// draftTriggeredEvidenceGroups returns critique-required groups the initial
// plan never surfaced, because the recipe fired on the draft's own prose rather
// than on the failure signal. Candidates are resolved from the bounded artifact
// tree so the nudge can name real paths; a group with no candidate is dropped.
func (s *agentState) draftTriggeredEvidenceGroups(out critiqueOutcome, planned []skills.PlanGroupRef) []skills.PlanGroupRef {
	if len(out.MissingSkillEvidence) == 0 {
		return nil
	}
	seen := make(map[[2]string]bool, len(planned))
	for _, group := range planned {
		seen[[2]string{group.SkillID, group.GroupID}] = true
	}
	var refs []skills.PlanGroupRef
	for _, miss := range out.MissingSkillEvidence {
		for _, group := range miss.Missing {
			key := [2]string{miss.Skill.ID, group.ID}
			if seen[key] {
				continue
			}
			seen[key] = true
			candidates := group.CandidatePaths(s.initialFailureSignal, s.initialArtifactTree.paths, evidenceplan.CandidatePathLimit)
			if len(candidates) == 0 {
				continue
			}
			refs = append(refs, skills.PlanGroupRef{
				SkillID:        miss.Skill.ID,
				GroupID:        group.ID,
				Description:    group.Description,
				CandidatePaths: candidates,
			})
		}
	}
	return refs
}

// formatEvidenceNudge names the unread evidence groups and their ranked
// candidate paths so the model can finish the plan it was given.
func formatEvidenceNudge(groups []skills.PlanGroupRef) string {
	if len(groups) > evidenceNudgeMaxGroups {
		groups = groups[:evidenceNudgeMaxGroups]
	}
	var b strings.Builder
	b.WriteString("You attempted to finalize while required evidence for this failure class is still unread. Read at least one candidate path from every group below with read_artifact, tail_artifact, or grep_artifact before producing the final JSON:\n")
	for _, group := range groups {
		label := strings.TrimSpace(group.Description)
		if label == "" {
			label = group.GroupID
		}
		candidates := group.CandidatePaths
		if len(candidates) > evidenceNudgeMaxCandidates {
			candidates = candidates[:evidenceNudgeMaxCandidates]
		}
		fmt.Fprintf(&b, "\n- %s (%s): %s", group.GroupID, label, strings.Join(candidates, ", "))
	}
	b.WriteString("\n\nA candidate path is a starting point, not the whole answer: if it does not contain the evidence, use list_artifacts or find_artifacts on the relevant subtree. Do not repeat reads you have already made. If the evidence genuinely is not present in this build, say so explicitly in root_cause instead of leaving the group unexplained.")
	return b.String()
}

func evidencePlanTraceEvent(outcome evidenceGateOutcome, coverage skills.PlanCoverage, unread []skills.PlanGroupRef, draftTriggered int) TraceEvent {
	plan := &EvidencePlanTrace{
		Applicable:     coverage.Applicable,
		Satisfied:      coverage.Satisfied,
		Unavailable:    coverage.Unavailable,
		Unmet:          coverage.Unmet,
		DraftTriggered: draftTriggered,
	}
	for _, group := range unread {
		plan.UnreadGroups = append(plan.UnreadGroups, EvidencePlanGroupTrace{SkillID: group.SkillID, GroupID: group.GroupID})
	}
	return TraceEvent{Kind: "evidence_plan", Outcome: string(outcome), EvidencePlan: plan}
}

// agenticCacheData is the on-disk shape of a cached agentic analysis. Embeds
// the raw model response and tags it with per-analysis telemetry so cache
// reads can re-stamp the published AIAnalysis and re-validate against the
// project's current floors.
type agenticCacheData struct {
	analysisResponse
	GeneratedAt            string `json:"generated_at,omitempty"`
	Model                  string `json:"model,omitempty"`
	ToolCalls              int    `json:"tool_calls,omitempty"`
	ModelBytes             int    `json:"model_bytes,omitempty"`
	GCSBytes               int    `json:"gcs_bytes,omitempty"`
	EvidencePlanCovered    bool   `json:"evidence_plan_covered,omitempty"`
	GCSFloorRetryExhausted bool   `json:"gcs_floor_retry_exhausted,omitempty"`
	BudgetExhausted        bool   `json:"budget_exhausted,omitempty"`
	SameFailureReuse       bool   `json:"same_failure_reuse,omitempty"`
	JudgeRan               bool   `json:"judge_ran,omitempty"`
	JudgeObjected          bool   `json:"judge_objected,omitempty"`
	JudgeRevised           bool   `json:"judge_revised,omitempty"`
	JudgeResolutionKnown   bool   `json:"judge_resolution_known,omitempty"`
	JudgeRevisionRejected  bool   `json:"judge_revision_rejected,omitempty"`

	// CritiquePassed marks entries that cleared the critique gate.
	// Defaults to false on pre-critique entries and on entries written
	// while critique was disabled. The cache-read gate uses this to
	// invalidate uncritiqued entries when a consumer later enables
	// critique.
	CritiquePassed bool `json:"critique_passed,omitempty"`
	// CritiqueHardFailures and CritiqueSoftWarnings retain content-free rule IDs
	// for independent cache policy enforcement.
	CritiqueHardFailures []string `json:"critique_hard_failures,omitempty"`
	CritiqueSoftWarnings []string `json:"critique_soft_warnings,omitempty"`

	// CritiqueVersion records which deterministic critique and publication
	// contract processed the draft. CritiquePassed records whether it passed.
	// The cache-read gate always requires the current version.
	CritiqueVersion int `json:"critique_version,omitempty"`

	// SkillSetHash is the fingerprint of the merged diagnostic skill
	// set at the time this draft was accepted. Empty when skills were
	// disabled or no recipes were loaded.
	SkillSetHash string `json:"skill_set_hash,omitempty"`

	// ModelHash is the fingerprint of the model and endpoint that produced
	// this draft.
	ModelHash string `json:"model_hash,omitempty"`

	// PromptHash is the fingerprint of the effective prompt contract under
	// which this entry was produced.
	PromptHash string `json:"prompt_hash,omitempty"`
}

// floorStatus tracks which per-project floors are currently unmet for a
// given agent state. Used by both the loop's nudge decision and the
// nudge message composer so the two stay in sync.
type floorStatus struct {
	callsUnmet bool
	gcsUnmet   bool
}

func (fs floorStatus) anyUnmet() bool { return fs.callsUnmet || fs.gcsUnmet }

func (fs floorStatus) traceStatus() string {
	switch {
	case fs.callsUnmet && fs.gcsUnmet:
		return "tool_calls+gcs_bytes"
	case fs.callsUnmet:
		return "tool_calls"
	case fs.gcsUnmet:
		return "gcs_bytes"
	default:
		return ""
	}
}

func gcsFloorUnmet(gcsBytes, minGCSBytes int, evidencePlanCovered, retryExhausted bool) bool {
	return gcsBytes < minGCSBytes && !evidencePlanCovered && !retryExhausted
}

// markGCSFloorRetryExhausted records a completed byte-only retry that still misses the floor.
func markGCSFloorRetryExhausted(ctx context.Context, state *agentState, opts AgenticOptions, retries int) bool {
	if state == nil || state.gcsFloorRetryExhausted || retries < 1 {
		return false
	}
	floors := floorStatus{
		callsUnmet: state.calls < opts.MinToolCalls,
		gcsUnmet:   gcsFloorUnmet(state.gcsBytes, opts.MinGCSBytes, state.evidencePlanCovered(), false),
	}
	if floors.callsUnmet || !floors.gcsUnmet {
		return false
	}
	state.gcsFloorRetryExhausted = true
	recordTrace(ctx, TraceEvent{Kind: "floor_nudge", Outcome: "retry_exhausted", Status: floors.traceStatus(), ToolCallCount: state.calls, Bytes: state.gcsBytes})
	log.Printf("  ⓘ agentic GCS byte-floor retry exhausted: gcs_kb=%d/min=%d", state.gcsBytes/1024, opts.MinGCSBytes/1024)
	return true
}

// evalFloors returns which per-project floors the current agent state
// fails to meet. A floor configured as 0 is never reported as unmet.
func evalFloors(state *agentState, opts AgenticOptions) floorStatus {
	return floorStatus{
		callsUnmet: state.calls < opts.MinToolCalls,
		gcsUnmet:   gcsFloorUnmet(state.gcsBytes, opts.MinGCSBytes, state.evidencePlanCovered(), state.gcsFloorRetryExhausted),
	}
}

type agentState struct {
	browser            artifacts.Browser
	sources            *tools.SourceCatalog
	opts               AgenticOptions
	registry           *tools.Registry
	enabledTools       []string
	cache              *tools.Cache
	webURLBase         string
	startTime          time.Time
	modelBytes         int
	gcsBytes           int
	calls              int
	budgetExhausted    bool
	draftObserver      DraftObserver
	selectionObserver  DraftSelectionObserver
	sourceObserver     SourceEvidenceObserver
	traceCtx           context.Context
	draftAttempt       int
	bestDraft          *critiqueDraftCandidate
	fallbackDraft      *critiqueDraftCandidate
	evidenceRevision   int
	recentModelRequest time.Duration
	deadline           time.Time

	// critiquePassed records whether the accepted answer cleared the
	// always-on critique gate. Stamped onto the published AIAnalysis so the
	// build-level shouldReanalyze gate can invalidate uncritiqued entries.
	critiquePassed            bool
	critiqueHardFailures      []string
	critiqueSoftWarnings      []string
	cachePersistenceAttempted bool
	cachePersistenceAccepted  bool
	cacheRejectionReason      CacheRejectionReason

	// readArtifactsFull / readArtifactsBase track artifacts the agent
	// successfully fetched via read_artifact / tail_artifact /
	// grep_artifact. Used by the critique gate to flag prose citations
	// of files the agent never opened. "full" keeps the directory
	// prefix, which catches cross-machine basename collisions. "base" is
	// just path.Base and matches bare-basename citations. Populated only after
	// a successful tool dispatch. Both maps stay nil when critique is disabled.
	readArtifactsFull map[string]bool
	readArtifactsBase map[string]bool
	readSourceFull    map[string]bool
	sourceOwner       string
	sourceName        string

	// evidenceArtifactsFull tracks successful non-empty content reads for
	// evaluating coverage of the initial ranked evidence plan. Listing calls,
	// failed reads, and empty reads do not enter this set.
	evidenceArtifactsFull map[string]bool

	// gcsFloorRetryExhausted records that the loop used its one retry whose only
	// remaining reason was the raw GCS byte floor. The marker makes the resulting
	// analysis reusable without weakening old cache entries.
	gcsFloorRetryExhausted bool

	// evidenceContentByPath retains bounded tool-result content in memory so
	// content-aware evidence groups can prove positive matches in the same file.
	// It is never copied into caches, traces, manifests, or progress state.
	evidenceContentByPath map[string][]string
	// analysisEvidence retains bounded artifact lines for citation validation.
	analysisEvidence         map[string]*analysisChatEvidence
	analysisEvidenceRevision map[string]map[int]int
	analysisEvidenceFull     bool
	analysisEvidenceBudget   int
	// sourceContentByPath retains bounded repo-tool snippets for CLI grounding.
	// Neither map is copied into caches, traces, or public output.
	sourceContentByPath map[string][]string

	// skillSet is the merged diagnostic recipe set. nil disables recipes
	// or no recipes are configured. Held on state
	// so in-loop and post-loop critique paths both consult the same
	// set, and so cacheAcceptedAnalysis / stampAgenticTelemetry can
	// stamp the hash without re-threading it.
	skillSet *skills.Set

	// initialEvidencePlan is matched against the bounded failure signal before
	// iteration one. Critique repair uses its ranked paths before falling back to
	// a tree walk when the final diagnosis needs a different or unresolved group.
	initialEvidencePlan  []skills.PlannedSkill
	initialFailureSignal string

	// consecutiveFailures is how many consecutive builds this test has failed.
	// Passed to the critique gate to contradict an is_transient=true verdict on
	// a persistent failure.
	consecutiveFailures int

	// promptHash is the fingerprint of the composed system prompt for this
	// run. Stamped onto the accepted analysis and the cache entry so a later
	// prompt edit invalidates them on read. Held on state so the stamp and
	// cache-write paths reuse it without re-threading sysPrompt.
	promptHash string

	// Semantic-judge telemetry, for measuring the always-on second-line judge.
	// judgeRan is set when the judge was invoked; judgeObjected when it raised
	// objections; judgeRevised when its objections drove an accepted revision.
	judgeRan              bool
	judgeObjected         bool
	judgeRevised          bool
	judgeRevisionRejected bool

	// initialArtifactTree is the single bounded listing shared by the seed and
	// ranked plan. A complete snapshot also supports absence pruning without a
	// second full tree walk.
	initialArtifactTree artifactTreeSnapshot

	// artifactTreeSetCache is the normalized form of a complete initial tree.
	// nil after artifactTreeChecked means the listing failed or was truncated,
	// so the absence check is skipped.
	artifactTreeSetCache map[string]bool
	artifactTreeChecked  bool
}

type artifactTreeSnapshot struct {
	paths     []string
	truncated bool
	failed    bool
}

// artifactTreeSet returns the normalized complete initial tree. Returns nil
// when the listing failed or was truncated, so absence is never inferred from
// incomplete data.
func (s *agentState) artifactTreeSet() map[string]bool {
	if s.artifactTreeChecked {
		return s.artifactTreeSetCache
	}
	s.artifactTreeChecked = true
	if s.initialArtifactTree.failed || s.initialArtifactTree.truncated {
		return nil
	}
	set := make(map[string]bool, len(s.initialArtifactTree.paths))
	for _, p := range s.initialArtifactTree.paths {
		if norm := NormalizeArtifactCitation(p); norm != "" {
			set[norm] = true
		}
	}
	s.artifactTreeSetCache = set
	return set
}

// planCoverage classifies the initial ranked evidence plan against the content
// reads made so far. Returns a zero value when the artifact-tree scan was
// incomplete, since only a complete scan can prove a group unavailable.
func (s *agentState) planCoverage() skills.PlanCoverage {
	if s == nil || s.skillSet == nil || s.initialArtifactTree.failed || s.initialArtifactTree.truncated {
		return skills.PlanCoverage{}
	}
	return s.skillSet.PlanCoverageWithContent(s.initialFailureSignal, s.initialEvidencePlan, s.evidenceArtifactsFull, s.evidenceContentByPath)
}

// evidencePlanCovered reports whether the complete initial ranked plan was
// satisfied by non-empty content reads. It is deliberately narrower than the
// critique gate, which may match additional recipes against the final draft.
func (s *agentState) evidencePlanCovered() bool {
	return s.planCoverage().Covered()
}

func (s *agentState) modelRemaining() int { return s.opts.ModelByteBudget - s.modelBytes }
func (s *agentState) gcsRemaining() int   { return s.opts.GCSByteBudget - s.gcsBytes }

// stampAgenticTelemetry copies per-call counters onto the AIAnalysis so the
// published JSON exposes per-failure cost. Called at every successful exit
// point: cache hit, normal finish, finalize-round finish, or synthesized
// fallback. An empty mode defaults to AgenticMode.
func stampAgenticTelemetry(analysis *models.AIAnalysis, state *agentState, mode string, cacheHit bool, start time.Time) {
	if analysis == nil {
		return
	}
	if mode == "" {
		mode = AgenticMode
	}
	analysis.Mode = mode
	analysis.CacheHit = cacheHit
	analysis.ElapsedMs = int(time.Since(start) / time.Millisecond)
	if state != nil {
		analysis.ToolCalls = state.calls
		analysis.ContextBytes = state.modelBytes
		analysis.GCSBytes = state.gcsBytes
		analysis.EvidencePlanCovered = state.evidencePlanCovered()
		analysis.GCSFloorRetryExhausted = state.gcsFloorRetryExhausted
		analysis.BudgetExhausted = state.budgetExhausted
		analysis.CritiquePassed = state.critiquePassed
		analysis.CritiqueHardFailures = append([]string(nil), state.critiqueHardFailures...)
		analysis.CritiqueSoftWarnings = append([]string(nil), state.critiqueSoftWarnings...)
		analysis.CachePersistenceAttempted = state.cachePersistenceAttempted
		analysis.CachePersistenceAccepted = state.cachePersistenceAccepted
		analysis.CachePolicyRejectionReason = string(state.cacheRejectionReason)
		analysis.CritiqueVersion = currentCritiqueVersion
		if state.skillSet != nil {
			analysis.SkillSetHash = state.skillSet.Hash()
		}
		analysis.PromptHash = state.promptHash
		analysis.JudgeRan = state.judgeRan
		analysis.JudgeObjected = state.judgeObjected
		analysis.JudgeRevised = state.judgeRevised
		analysis.JudgeResolutionKnown = true
		analysis.JudgeRevisionRejected = state.judgeRevisionRejected
	}
}

func (s *agentState) setCritiqueOutcome(out critiqueOutcome) {
	if s == nil {
		return
	}
	s.critiquePassed = out.Passed
	s.critiqueHardFailures = critiqueRuleStrings(out.HardRuleIDs())
	s.critiqueSoftWarnings = critiqueRuleStrings(out.SoftRuleIDs())
}

// AgenticInputs bundles the per-failure context required by the agentic loop.
// Lifetime notes:
//   - Browser, Cache, and WebURLBase are scoped to one build.
//   - Registry and EnabledTools are scoped to one pipeline and reused across
//     analyses.
//   - Opts and Skills are per-project.
//   - Mode is stamped on the returned AIAnalysis and defaults to AgenticMode.
type AgenticInputs struct {
	Browser      artifacts.Browser
	Sources      *tools.SourceCatalog
	ProjectOwner string
	ProjectName  string
	Opts         AgenticOptions
	Registry     *tools.Registry
	EnabledTools []string
	Cache        *tools.Cache
	WebURLBase   string
	Mode         string
	// PromptHash overrides the system-only fingerprint when the per-failure
	// module prompt is part of the recorded provenance.
	PromptHash string

	// Skills is the merged diagnostic recipe set. nil disables skill
	// matching entirely. Critique-disabled runs also skip recipes because recipes
	// are consulted only inside the critique gate. Skills.Hash records the
	// profile and consumer recipes that produced the analysis.
	Skills *skills.Set

	// ConsecutiveFailures is how many consecutive builds this test has failed,
	// used by the critique gate to contradict an is_transient=true verdict on a
	// persistent failure. 1 (or 0) means not persistent.
	ConsecutiveFailures int

	// FailureSignal is bounded test-failure evidence used only for initial skill
	// matching. It excludes module and backend instructions.
	FailureSignal string

	// DraftObserver is an optional in-memory benchmark hook. It is disabled in
	// production and receives value-only copies that cannot mutate runtime state.
	DraftObserver DraftObserver

	// DraftSelectionObserver reports the selected parseable attempt to the
	// benchmark after production selection completes.
	DraftSelectionObserver DraftSelectionObserver

	SourceEvidenceObserver SourceEvidenceObserver
}

const (
	// compactionTargetRatio is the fraction of ContextByteBudget compaction
	// drives toward once triggered, leaving headroom so it does not re-fire
	// every iteration.
	compactionTargetRatio = 0.7
	// compactionKeepRecentTools tool results are kept at full content when
	// possible so the model always has its latest evidence verbatim.
	compactionKeepRecentTools = 3
	// compactionStubHead is how many leading bytes of an elided tool result
	// are retained as a hint before the elision note, usually the envelope head
	// with the artifact path and status.
	compactionStubHead = 160
	// compactionMsgOverhead approximates per-message JSON framing bytes.
	compactionMsgOverhead = 48
)

// elisionMarker tags a stubbed message so compaction is idempotent across
// iterations and tests can detect elision.
const elisionMarker = "bytes elided to fit context"

func isStubbed(c *string) bool {
	return c != nil && strings.Contains(*c, elisionMarker)
}

// stubContent keeps a short head of the original tool result plus an elision
// note that tells the model how to recover the evidence.
func stubContent(orig string) string {
	head := orig
	if len(head) > compactionStubHead {
		head = head[:compactionStubHead]
	}
	return fmt.Sprintf("%s\n...[%d %s; re-call the tool if you need this evidence again]",
		head, len(orig)-len(head), elisionMarker)
}

// schemaPayloadBytes is the serialized size of the tool schemas sent on every
// loop call. Computed once per loop and added to the size estimate so
// compaction accounts for the fixed schema cost, not just message content.
func schemaPayloadBytes(schemas []tools.Schema) int {
	if len(schemas) == 0 {
		return 0
	}
	b, err := json.Marshal(schemas)
	if err != nil {
		return 0
	}
	return len(b)
}

// requestSizeEstimate approximates the serialized chat-request size in bytes:
// message content + tool-call arguments + per-message framing + the fixed
// schema payload.
func requestSizeEstimate(messages []modelMessage, schemaBytes int) int {
	total := schemaBytes + 64 // request framing
	for i := range messages {
		total += compactionMsgOverhead
		if messages[i].Content != nil {
			total += len(*messages[i].Content)
		}
		for _, tc := range messages[i].ToolCalls {
			total += len(tc.Function.Name) + len(tc.Function.Arguments) + 32
		}
		for _, item := range messages[i].ProviderItems {
			total += len(item)
		}
	}
	return total
}

// compactMessages elides accumulated tool results and, if needed, assistant
// reasoning so the estimated request stays under budgetBytes. Disabled when
// budgetBytes <= 0. Preserves the system prompt, task, message order, and
// tool_call_id wiring so OpenAI tool-call pairing stays valid. Returns the
// slice and the number of messages elided this call.
func compactMessages(messages []modelMessage, schemaBytes, budgetBytes int) ([]modelMessage, int) {
	if budgetBytes <= 0 || requestSizeEstimate(messages, schemaBytes) <= budgetBytes {
		return messages, 0
	}
	target := int(float64(budgetBytes) * compactionTargetRatio)
	elided := 0

	// Tool-result messages, oldest first, that are not already stubbed.
	var toolIdx []int
	for i := 2; i < len(messages); i++ {
		if messages[i].Role == "tool" && messages[i].Content != nil && !isStubbed(messages[i].Content) {
			toolIdx = append(toolIdx, i)
		}
	}
	stub := func(i int) {
		messages[i].Content = strPtr(stubContent(*messages[i].Content))
		elided++
	}
	// Stage 1: stub older tool results, preferring to keep the most recent
	// compactionKeepRecentTools verbatim.
	keepFrom := len(toolIdx) - compactionKeepRecentTools
	for p := 0; p < keepFrom && requestSizeEstimate(messages, schemaBytes) > target; p++ {
		stub(toolIdx[p])
	}
	// Stage 2: still over target, so stub the recent tool results too.
	for p := 0; p < len(toolIdx) && requestSizeEstimate(messages, schemaBytes) > target; p++ {
		if !isStubbed(messages[toolIdx[p]].Content) {
			stub(toolIdx[p])
		}
	}
	// Stage 3: still over target, so stub older assistant reasoning, keeping
	// the tool_calls wiring intact.
	for i := 2; i < len(messages) && requestSizeEstimate(messages, schemaBytes) > target; i++ {
		m := &messages[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		elidedMessage := false
		if m.Content != nil && !isStubbed(m.Content) && len(*m.Content) > compactionStubHead {
			m.Content = strPtr(stubContent(*m.Content))
			elidedMessage = true
		}
		if elidedMessage {
			elided++
		}
	}
	// Stage 4: replace tools-free Responses turns with replayable text.
	for i := 2; i < len(messages) && requestSizeEstimate(messages, schemaBytes) > target; i++ {
		m := &messages[i]
		if m.Role != "assistant" || len(m.ProviderItems) == 0 || len(m.ToolCalls) > 0 || m.Content == nil {
			continue
		}
		m.ProviderItems = nil
		elided++
	}
	// Stage 5: remove older Responses assistant turns and their paired tool
	// outputs atomically when continuation state keeps the request over budget.
	for i := 2; i < len(messages) && requestSizeEstimate(messages, schemaBytes) > target; {
		m := messages[i]
		if m.Role != "assistant" || len(m.ProviderItems) == 0 || len(m.ToolCalls) == 0 {
			i++
			continue
		}
		ids := map[string]bool{}
		for _, call := range m.ToolCalls {
			ids[call.ID] = true
		}
		end := i + 1
		for end < len(messages) && messages[end].Role == "tool" && ids[messages[end].ToolCallID] {
			end++
		}
		if end == i+1 {
			i++
			continue
		}
		elided += end - i
		messages = append(messages[:i], messages[end:]...)
	}
	return messages, elided
}

func effectiveAgenticPromptHash(in AgenticInputs, sysPrompt string) string {
	if in.PromptHash != "" {
		return in.PromptHash
	}
	return PromptFingerprint(sysPrompt + agToolDocs + agenticSourceContextSection(in.Sources, in.ProjectOwner, in.ProjectName))
}

func (c *Client) cachedAgenticAnalysis(in AgenticInputs, cacheKey, sysPrompt string, start time.Time) (*models.AISummary, *models.AIAnalysis, bool) {
	skillSetHash := ""
	if in.Skills != nil {
		skillSetHash = in.Skills.Hash()
	}
	result, reason := LookupAgenticCache(c.cache, cacheKey, agenticCachePolicy(
		c, in.Opts, skillSetHash, effectiveAgenticPromptHash(in, sysPrompt), in.ConsecutiveFailures,
	))
	if reason != CacheAccepted {
		return nil, nil, false
	}
	stampAgenticTelemetry(result.Analysis, nil, in.Mode, true, start)
	if !StampAnalysisDisposition(result.Analysis) {
		return result.Summary, result.Analysis, true
	}
	if result.Analysis.Disposition == models.AnalysisDispositionPreliminary {
		generatedAt, err := time.Parse(time.RFC3339, result.Analysis.GeneratedAt)
		if err != nil || time.Since(generatedAt) > preliminaryCacheTTL {
			return nil, nil, false
		}
	}
	return result.Summary, result.Analysis, true
}

// doAnalyzeAgentic runs the tool-calling AI loop for one failure. Returns the
// summary and analysis pair for the published output.
//
// The caller is responsible for constructing a fresh Browser per failure and
// choosing a cache key that encodes build and failure. Two builds of the same
// test must never share an agentic cache entry.
//
// Returns ErrToolsUnsupported wrapped on the first API call if the endpoint
// rejects function-calling. There is no tools-free fallback; the caller marks
// the failure AI-unavailable for the run.
func (c *Client) doAnalyzeAgentic(
	ctx context.Context,
	in AgenticInputs,
	cacheKey, sysPrompt, userPrompt string,
) (*models.AISummary, *models.AIAnalysis, error) {
	start := time.Now()
	if summary, analysis, ok := c.cachedAgenticAnalysis(in, cacheKey, sysPrompt, start); ok {
		return summary, analysis, nil
	}

	state := &agentState{
		browser:           in.Browser,
		sources:           in.Sources,
		opts:              in.Opts,
		registry:          in.Registry,
		enabledTools:      in.EnabledTools,
		cache:             in.Cache,
		webURLBase:        in.WebURLBase,
		startTime:         time.Now(),
		promptHash:        effectiveAgenticPromptHash(in, sysPrompt),
		draftObserver:     in.DraftObserver,
		selectionObserver: in.DraftSelectionObserver,
		sourceObserver:    in.SourceEvidenceObserver,
	}
	// Skills are consulted inside the always-on critique gate. Recipe presence
	// is the opt-in; an empty set is a no-op.
	state.skillSet = in.Skills
	state.initialFailureSignal = in.FailureSignal
	state.consecutiveFailures = in.ConsecutiveFailures
	// Pre-init the read-tracking maps so findUnreadArtifactCitations runs the
	// check even when the model has made zero successful reads. Otherwise the
	// nil-disables contract would skip the worst-case hallucination scenario.
	state.readArtifactsFull = map[string]bool{}
	state.readArtifactsBase = map[string]bool{}
	state.sourceOwner = in.ProjectOwner
	state.sourceName = in.ProjectName
	if in.Sources != nil {
		state.readSourceFull = map[string]bool{}
		if primary, ok := in.Sources.Primary(); ok {
			state.sourceOwner = primary.Owner
			state.sourceName = primary.Name
		}
	}
	state.evidenceArtifactsFull = map[string]bool{}
	state.analysisEvidence = map[string]*analysisChatEvidence{}
	state.analysisEvidenceRevision = map[string]map[int]int{}
	state.analysisEvidenceBudget = analysisChatEvidenceBudget(state.opts.ContextByteBudget)

	fullSysPrompt := sysPrompt + agToolDocs + agenticSourceContextSection(in.Sources, in.ProjectOwner, in.ProjectName)
	state.initialArtifactTree = listInitialArtifactTree(ctx, in.Browser)
	if seed := buildArtifactTreeSeed(state.initialArtifactTree.paths, state.initialArtifactTree.truncated, artifactTreeSeedBytes(in.Opts)); seed != "" {
		userPrompt = prependPrompt(userPrompt, seed)
	}
	if in.Skills != nil && strings.TrimSpace(in.FailureSignal) != "" {
		state.initialEvidencePlan = in.Skills.Plan(in.FailureSignal, state.initialArtifactTree.paths, evidenceplan.CandidatePathLimit)
		plan, _ := evidenceplan.Render(state.initialEvidencePlan, evidenceplan.ScanStatus{
			Truncated: state.initialArtifactTree.truncated,
			Failed:    state.initialArtifactTree.failed,
		})
		if plan != "" {
			userPrompt = prependPrompt(userPrompt, plan)
		}
	}
	messages := []modelMessage{
		{Role: "system", Content: strPtr(fullSysPrompt)},
		{Role: "user", Content: strPtr(userPrompt)},
	}
	schemas := state.registry.Schemas(state.enabledTools)

	state.deadline = state.startTime.Add(in.Opts.Timeout)
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(state.deadline) {
		state.deadline = parentDeadline
	}
	loopCtx, cancel := context.WithDeadline(ctx, state.deadline)
	defer cancel()
	state.traceCtx = loopCtx

	var finalContent string
	var finalProviderItems []json.RawMessage
	// The raw GCS byte floor gets at most one retry after all other floors pass.
	gcsFloorOnlyRetries := 0

	// Per-floor anti-thrash: track the calls + gcsBytes counters at the
	// time we last nudged so we can detect whether the model has made
	// progress on the unmet axis since then. A model that keeps coming
	// back tools-free without progressing gets accepted but not cached
	// so the loop doesn't burn iterations on a refusing model. Sentinel
	// -1 ensures the very first iteration's zero-state counts as progress.
	nudgedAtCalls := -1
	nudgedAtGCSBytes := -1

	// Available evidence from the initial ranked plan keeps the loop open, with
	// the same anti-thrash discipline as the floor nudge. Sentinel -1 makes the
	// first tools-free answer eligible even before any evidence was recorded.
	evidence := evidenceGate{nudgedAtRevision: -1}

	// critiqueRetries is shared by every deterministic critique repair path.
	// Each admitted retry may extend maxIters so an in-loop repair has room for
	// follow-up tool calls plus a re-emit.
	critiqueRetries := &critiqueRetryBudget{max: in.Opts.CritiqueMaxRetries}
	maxIters := in.Opts.MaxIters

	// Fixed schema cost added to every size estimate so compaction budgets
	// against the real request, not just message content.
	schemaBytes := schemaPayloadBytes(schemas)
	headroom := contextHeadroomFor(in.Opts)

	finalDraftObserved := false
	draftPhase := "initial"

	// When single_tool_call is on, request parallel_tool_calls=false so
	// compliant endpoints emit a single call. The client-side cap below still
	// trims endpoints that ignore the flag.
	var parallelToolCalls *bool
	if in.Opts.SingleToolCall {
		f := false
		parallelToolCalls = &f
	}

agentLoop:
	for iter := 0; iter < maxIters; iter++ {
		var fits bool
		messages, fits = prepareContextRequest(loopCtx, messages, schemaBytes, headroom, "applied")
		if !fits {
			if fallback := state.promoteFallbackDraft(); fallback != nil {
				finalContent = fallback.content
				finalProviderItems = fallback.providerItems
				finalDraftObserved = true
				recordTrace(loopCtx, TraceEvent{Kind: "context_headroom", Outcome: "best_draft", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
				log.Printf("  ⚠ agentic context headroom exhausted; publishing the best prior draft without another provider request")
				break agentLoop
			}
			recordTrace(loopCtx, TraceEvent{Kind: "context_headroom", Outcome: "unavailable", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
			return nil, nil, ErrContextHeadroom
		}
		requestStart := time.Now()
		resp, err := c.callModel(loopCtx, messages, schemas, parallelToolCalls)
		state.recentModelRequest = time.Since(requestStart)
		if err != nil {
			// Detect "tools not supported" on the first call only.
			if iter == 0 && isToolsUnsupportedError(err) {
				return nil, nil, fmt.Errorf("%w: %v", ErrToolsUnsupported, err)
			}
			// A retained parseable draft is better output than losing the whole
			// analysis to one failed follow-up request.
			if fallback := state.promoteFallbackDraft(); fallback != nil {
				finalContent = fallback.content
				finalProviderItems = fallback.providerItems
				finalDraftObserved = true
				recordTrace(loopCtx, TraceEvent{Kind: "draft_recovery", Outcome: "model_request_error", SelectedAttempt: fallback.attempt})
				log.Printf("  ⚠ agentic request failed (%v); publishing the best prior draft", err)
				break agentLoop
			}
			return nil, nil, fmt.Errorf("agentic iter %d: %w", iter+1, err)
		}
		if !resp.HasMessage {
			return nil, nil, fmt.Errorf("agentic iter %d: empty choices", iter+1)
		}
		msg := resp.Message

		if len(msg.ToolCalls) > 0 && msg.Content != nil {
			if parsedCandidate, parsedOK := tryParseAnalysis(*msg.Content); parsedOK {
				candidateCritique := critiqueDraftWithContent(parsedCandidate, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, parsedCandidate), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
				if len(candidateCritique.MissingSkillEvidence) > 0 {
					if treeSet := state.artifactTreeSet(); treeSet != nil {
						pruneAbsentSkillEvidence(parsedCandidate, &candidateCritique, treeSet)
					}
				}
				candidateDraft := state.newDraftCandidate(draftPhase, *msg.Content, msg.ProviderItems, parsedCandidate, candidateCritique)
				semanticAccepted := draftPhase == "semantic_retry" && state.judgeObjected
				state.considerFallbackDraft(candidateDraft, semanticAccepted)
				recordTrace(loopCtx, TraceEvent{Kind: "draft_recovery", Outcome: "tool_bearing_candidate", SelectedAttempt: candidateDraft.attempt})
			}
		}

		if len(msg.ToolCalls) == 0 {
			candidate := ""
			if msg.Content != nil {
				candidate = *msg.Content
			}

			// Enforce per-project floors by nudging the model to
			// investigate further before accepting its final answer.
			// Skip the nudge when no floor is unmet, budgets are exhausted, or the
			// model has not progressed on any unmet floor since the last nudge.
			// Avoid fighting the tool-side "finalize now" signal. The per-axis progress check
			// covers the pathological list-only loop: a model calling
			// list_artifacts repeatedly raises calls but never gcsBytes
			// and would otherwise be re-nudged every iteration.
			parsedCandidate, parsedOK := tryParseAnalysis(candidate)
			var candidateCritique critiqueOutcome
			var candidateDraft *critiqueDraftCandidate
			if parsedOK {
				candidateCritique = critiqueDraftWithContent(parsedCandidate, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, parsedCandidate), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
				if len(candidateCritique.MissingSkillEvidence) > 0 {
					if treeSet := state.artifactTreeSet(); treeSet != nil {
						if n := pruneAbsentSkillEvidence(parsedCandidate, &candidateCritique, treeSet); n > 0 {
							log.Printf("  ⓘ skill-evidence: %d required group(s) absent from this build's artifacts; not held against the draft", n)
						}
					}
				}
				candidateDraft = state.newDraftCandidate(draftPhase, candidate, msg.ProviderItems, parsedCandidate, candidateCritique)
				semanticAccepted := draftPhase == "semantic_retry" && state.judgeObjected
				state.considerFallbackDraft(candidateDraft, semanticAccepted)
			}

			floors := evalFloors(state, in.Opts)
			gcsFloorOnly := floors.gcsUnmet && !floors.callsUnmet
			if markGCSFloorRetryExhausted(loopCtx, state, in.Opts, gcsFloorOnlyRetries) {
				floors = evalFloors(state, in.Opts)
			}
			if floors.anyUnmet() && !state.budgetExhausted {
				progressed := false
				if floors.callsUnmet && state.calls > nudgedAtCalls {
					progressed = true
				}
				if floors.gcsUnmet && state.gcsBytes > nudgedAtGCSBytes {
					progressed = true
				}
				if progressed {
					echo := modelMessage{Role: "assistant", ProviderItems: msg.ProviderItems}
					if msg.Content != nil {
						echo.Content = msg.Content
					}
					messages = append(messages, echo, modelMessage{
						Role:    "user",
						Content: strPtr(formatFloorsNudge(state, in.Opts)),
					})
					var fits bool
					messages, fits = prepareContextRequest(loopCtx, messages, schemaBytes, headroom, "floor_nudge")
					if !fits {
						if fallback := state.promoteFallbackDraft(); fallback != nil {
							finalContent = fallback.content
							finalProviderItems = fallback.providerItems
						} else {
							finalContent = candidate
							finalProviderItems = msg.ProviderItems
						}
						finalDraftObserved = parsedOK
						recordTrace(loopCtx, TraceEvent{Kind: "context_headroom", Outcome: "retry_denied", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
						recordTrace(loopCtx, TraceEvent{Kind: "context_headroom", Outcome: "best_draft", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
						break agentLoop
					}
					if gcsFloorOnly {
						// Leave room for the tools-enabled response when the nudge lands
						// on the configured iteration boundary.
						if iter+1 >= maxIters {
							maxIters++
						}
						gcsFloorOnlyRetries++
					}
					nudgedAtCalls = state.calls
					nudgedAtGCSBytes = state.gcsBytes
					draftPhase = "floor_retry"
					recordTrace(loopCtx, TraceEvent{Kind: "floor_nudge", Outcome: "retry", Status: floors.traceStatus(), ToolCallCount: state.calls, Bytes: state.gcsBytes})
					log.Printf("  ↻ agentic nudge: tool_calls=%d/min=%d, gcs_kb=%d/min=%d, asking model to investigate further",
						state.calls, in.Opts.MinToolCalls, state.gcsBytes/1024, in.Opts.MinGCSBytes/1024)
					continue
				}
			}

			// Evidence gate. The initial ranked plan already resolved candidate
			// paths for every applicable group; a group with candidates that the
			// model never read is available evidence left unread, so reopen the
			// investigation instead of finalizing on a partial picture. Groups
			// the draft's own prose newly required are included, since the plan
			// never had the chance to show them. Floors measure effort; this
			// measures whether the available evidence was actually consulted.
			coverage := state.planCoverage()
			unreadGroups := coverage.UnmetGroups
			draftTriggered := 0
			if parsedOK {
				extra := state.draftTriggeredEvidenceGroups(candidateCritique, unreadGroups)
				draftTriggered = len(extra)
				unreadGroups = append(unreadGroups, extra...)
			}
			if coverage.Applicable > 0 || len(unreadGroups) > 0 {
				outcome, unread := evidence.decide(state, unreadGroups)
				recordTrace(loopCtx, evidencePlanTraceEvent(outcome, coverage, unread, draftTriggered))
				if outcome == evidenceGateNudge {
					// Seed the selected draft before reopening. Otherwise a later,
					// worse draft becomes the first candidate the critique gate
					// sees and wins against an empty selection.
					if candidateDraft != nil {
						state.considerDraft(candidateDraft, draftPhase == "semantic_retry" && state.judgeObjected)
					}
					echo := modelMessage{Role: "assistant", ProviderItems: msg.ProviderItems}
					if msg.Content != nil {
						echo.Content = msg.Content
					}
					messages = append(messages, echo, modelMessage{
						Role:    "user",
						Content: strPtr(formatEvidenceNudge(unread)),
					})
					var fits bool
					messages, fits = prepareContextRequest(loopCtx, messages, schemaBytes, headroom, "evidence_nudge")
					if !fits {
						if fallback := state.promoteFallbackDraft(); fallback != nil {
							finalContent = fallback.content
							finalProviderItems = fallback.providerItems
						} else {
							finalContent = candidate
							finalProviderItems = msg.ProviderItems
						}
						finalDraftObserved = parsedOK
						recordTrace(loopCtx, TraceEvent{Kind: "context_headroom", Outcome: "retry_denied", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
						recordTrace(loopCtx, TraceEvent{Kind: "context_headroom", Outcome: "best_draft", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
						break agentLoop
					}
					// Leave room for the tools-enabled response when the nudge lands
					// on the configured iteration boundary.
					if iter+1 >= maxIters {
						maxIters++
					}
					evidence.recordNudge(state)
					draftPhase = "evidence_retry"
					log.Printf("  ↻ agentic evidence nudge: %d evidence group(s) still unread, asking model to read them before finalizing", len(unread))
					continue
				}
			}

			// Critique gate (always on). Re-prompts the model with targeted
			// feedback when the draft punts, hallucinates, fabricates an import
			// path, or fails recipe-driven evidence. Only fires on parseable
			// candidates; unparseable finals fall through to runFinalizeRound
			// below.
			if parsed, ok := parsedCandidate, parsedOK; ok {
				out := candidateCritique
				semanticAccepted := draftPhase == "semantic_retry" && state.judgeObjected
				state.considerDraft(candidateDraft, semanticAccepted)
				if out.Passed {
					recordTrace(loopCtx, critiqueTraceEvent("passed", out))
					// The initial semantic review runs only on the selected draft.
					// An objected draft may spend one tools-free refinalization and
					// one revision-review call. Failures preserve the selected draft.
					if in.Opts.SemanticJudge && !state.judgeRan && state.bestDraft == candidateDraft {
						state.judgeRan = true
						result, err := c.semanticCritiqueTracked(loopCtx, state, semanticJudgeStageDraft, parsed, nil, nil, headroom)
						switch {
						case err != nil:
							recordTrace(loopCtx, semanticJudgeTraceEvent(semanticJudgeStageDraft, "error", result, "semantic_judge_error"))
							log.Printf("  ⓘ semantic judge: skipped (%v)", err)
						case len(result.Findings) > 0:
							recordTrace(loopCtx, semanticJudgeTraceEvent(semanticJudgeStageDraft, "objected", result, ""))
							state.judgeObjected = true
							prior := parsed
							echo := modelMessage{Role: "assistant", ProviderItems: msg.ProviderItems}
							if msg.Content != nil {
								echo.Content = msg.Content
							}
							repairMessages := append(messages, echo, modelMessage{
								Role:    "user",
								Content: strPtr(formatSemanticFindings(result.Findings)),
							})
							revised, revisedItems, safe := c.runFinalizeRoundTracked(loopCtx, state, repairMessages, headroom)
							if safe {
								if rp, ok := tryParseAnalysis(revised); ok {
									revisedCritique := critiqueDraftWithContent(rp, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, rp), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
									if len(revisedCritique.MissingSkillEvidence) > 0 {
										if treeSet := state.artifactTreeSet(); treeSet != nil {
											pruneAbsentSkillEvidence(rp, &revisedCritique, treeSet)
										}
									}
									semanticCandidate := state.newDraftCandidate("semantic_retry", revised, revisedItems, rp, revisedCritique)
									policy := effectiveCritiqueCachePolicy(in.Opts.CritiqueCachePolicy, in.Opts.CritiqueMaxRetries)
									decision := c.reviewSemanticRevision(loopCtx, state, prior, result.Findings, semanticCandidate, headroom, policy)
									if !decision.accepted && semanticRevisionRejected(decision, semanticCandidate, policy) {
										state.judgeRevisionRejected = true
										recordTrace(loopCtx, TraceEvent{Kind: "semantic_judge", Status: semanticJudgeStageRevision, Outcome: "revision_rejected", IssueCount: len(semanticCandidate.semanticFindingClasses)})
									} else if decision.accepted {
										state.considerFallbackDraftForPolicy(semanticCandidate, true, policy)
										state.judgeRevised = true
										recordTrace(loopCtx, TraceEvent{Kind: "semantic_judge", Status: semanticJudgeStageRevision, Outcome: "revised"})
									} else {
										recordTrace(loopCtx, TraceEvent{Kind: "semantic_judge", Status: semanticJudgeStageRevision, Outcome: "revision_not_selected"})
									}
								} else {
									state.judgeRevisionRejected = true
									recordTrace(loopCtx, TraceEvent{Kind: "semantic_judge", Status: semanticJudgeStageRevision, Outcome: "revision_unparseable", IssueCount: len(result.Findings)})
								}
							} else {
								state.judgeRevisionRejected = true
								recordTrace(loopCtx, TraceEvent{Kind: "semantic_judge", Status: semanticJudgeStageRevision, Outcome: "revision_denied", IssueCount: len(result.Findings)})
							}
							fallback := state.promoteFallbackDraft()
							finalContent = fallback.content
							finalProviderItems = fallback.providerItems
							finalDraftObserved = true
							state.critiquePassed = fallback.quality.Passed
							break agentLoop
						default:
							recordTrace(loopCtx, semanticJudgeTraceEvent(semanticJudgeStageDraft, "passed", result, ""))
							log.Printf("  ✓ semantic judge: no findings")
							state.considerDraft(candidateDraft, true)
							state.considerFallbackDraft(candidateDraft, true)
						}
					}
					// Reaching acceptance after the judge objected on an earlier
					// draft means its objections drove an accepted revision.
					if state.judgeObjected && !state.judgeRevisionRejected && state.bestDraft != nil && candidateDraft != nil && state.bestDraft.attempt == candidateDraft.attempt {
						state.judgeRevised = true
					}
					state.critiquePassed = state.bestDraft != nil && state.bestDraft.quality.Passed
				} else {
					state.critiquePassed = false
				}
			}

			if parsedOK && state.bestDraft != nil {
				finalContent = state.bestDraft.content
				finalProviderItems = state.bestDraft.providerItems
			} else {
				finalContent = candidate
				finalProviderItems = msg.ProviderItems
			}
			finalDraftObserved = parsedOK
			break
		}

		toolCalls, dropped := limitToolCalls(msg.ToolCalls, in.Opts.SingleToolCall)
		if dropped > 0 {
			log.Printf("  ⤵ single_tool_call: model returned %d tool calls; executing the first and dropping %d (model may re-request them)",
				len(msg.ToolCalls), dropped)
		}

		echoCalls, skippedOutputs := continuationCalls(c.apiMode, msg, toolCalls)
		echo := modelMessage{Role: "assistant", ToolCalls: echoCalls, ProviderItems: msg.ProviderItems}
		if msg.Content != nil {
			echo.Content = msg.Content
		}
		messages = append(messages, echo)

		messages = append(messages, skippedOutputs...)

		for _, tc := range toolCalls {
			result := dispatchAgenticTool(loopCtx, state, tc)
			state.modelBytes += len(result)
			messages = append(messages, modelMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    strPtr(result),
			})
		}
	}

	// If the model never returned a tools-free final message, OR returned one
	// without parseable JSON, force a finalize round with tools omitted.
	parsed, ok := tryParseAnalysis(finalContent)
	if !ok {
		var safe bool
		finalContent, finalProviderItems, safe = c.runFinalizeRoundTracked(loopCtx, state, messages, headroom)
		if !safe {
			return nil, nil, ErrContextHeadroom
		}
		parsed, ok = tryParseAnalysis(finalContent)
		if ok {
			recordTrace(loopCtx, TraceEvent{Kind: "finalize_parse", Outcome: "accepted"})
		} else {
			code := "invalid_structured_response"
			if strings.TrimSpace(finalContent) == "" {
				code = "empty_content"
			}
			recordTrace(loopCtx, TraceEvent{Kind: "finalize_parse", Outcome: "rejected", ErrorCode: code})
		}
	}
	if !ok && state.fallbackDraft != nil {
		fallback := state.promoteFallbackDraft()
		finalContent = fallback.content
		finalProviderItems = fallback.providerItems
		finalDraftObserved = true
		parsed, ok = tryParseAnalysis(finalContent)
		recordTrace(loopCtx, TraceEvent{Kind: "finalize_recovery", Outcome: "retained_draft", SelectedAttempt: fallback.attempt})
		log.Printf("  ⚠ agentic repair: finalize did not parse; keeping selected draft")
	}
	if !ok {
		recordTrace(loopCtx, TraceEvent{Kind: "finalize_recovery", Outcome: "rejected", ErrorCode: "invalid_structured_response"})
		return nil, nil, ErrRejectedAnalysis
	}

	parsed = c.applyPostLoopCritique(loopCtx, state, messages, finalContent, finalProviderItems, parsed, in.Opts, critiqueRetries, finalDraftObserved, draftPhase)
	markGCSFloorRetryExhausted(loopCtx, state, in.Opts, gcsFloorOnlyRetries)
	parsed = c.prepareCacheablePublishedAnalysis(loopCtx, state, messages, parsed, in.Opts)

	state.notifyDraftSelection()
	summary, analysis := c.buildOutputs(parsed)
	stampAgenticTelemetry(analysis, state, in.Mode, false, start)
	if !StampAnalysisDisposition(analysis) {
		recordTrace(loopCtx, TraceEvent{Kind: "publication", Outcome: "rejected", ErrorCode: "unsafe_analysis"})
		return nil, nil, ErrRejectedAnalysis
	}
	if analysis.Disposition == models.AnalysisDispositionPreliminary {
		recordTrace(loopCtx, TraceEvent{Kind: "publication", Outcome: "preliminary"})
	} else {
		recordTrace(loopCtx, TraceEvent{Kind: "publication", Outcome: "grounded"})
	}
	c.cacheAcceptedAnalysis(loopCtx, cacheKey, parsed, analysis.GeneratedAt, state, in.Opts)
	stampAgenticTelemetry(analysis, state, in.Mode, false, start)
	return summary, analysis, nil
}

func (c *Client) prepareCacheablePublishedAnalysis(ctx context.Context, state *agentState, messages []modelMessage, parsed analysisResponse, opts AgenticOptions) analysisResponse {
	parsed = sanitizePublishedCitations(parsed, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
	parsed = state.preparePublishedAnalysis(parsed)
	out := critiqueDraftWithContent(parsed, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, parsed), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
	if len(out.MissingSkillEvidence) > 0 {
		if treeSet := state.artifactTreeSet(); treeSet != nil {
			pruneAbsentSkillEvidence(parsed, &out, treeSet)
		}
	}
	policy := effectiveCritiqueCachePolicy(opts.CritiqueCachePolicy, opts.CritiqueMaxRetries)
	newlyPassed := out.Passed && !state.critiquePassed
	state.setCritiqueOutcome(out)
	accepted := critiqueAcceptedForPolicy(out, policy)
	switch {
	case out.Passed && newlyPassed:
		recordTrace(ctx, critiqueTraceEvent("published_passed", out))
	case accepted && len(out.RuleIDs()) > 0:
		recordTrace(ctx, critiqueTraceEvent("published_warning", out))
	case !accepted:
		recordTrace(ctx, critiqueTraceEvent("published_rejected", out))
	}
	if !accepted {
		return parsed
	}
	if state.bestDraft != nil {
		state.bestDraft.parsed = parsed
		state.bestDraft.quality = critiqueQualityFor(out)
		state.bestDraft.evidenceRevision = state.evidenceRevision
		if raw, err := json.Marshal(parsed); err == nil {
			state.bestDraft.content = string(raw)
			state.bestDraft.providerItems = nil
		}
	}
	if opts.SemanticJudge && !state.judgeRan && len(out.HardRuleIDs()) == 0 {
		content := ""
		if raw, err := json.Marshal(parsed); err == nil {
			content = string(raw)
		}
		parsed = c.applySemanticJudgePostLoop(ctx, state, messages, content, nil, parsed, contextHeadroomFor(opts), policy)
		parsed = sanitizePublishedCitations(parsed, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
		parsed = state.preparePublishedAnalysis(parsed)
		out = critiqueDraftWithContent(parsed, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, parsed), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
		if len(out.MissingSkillEvidence) > 0 {
			if treeSet := state.artifactTreeSet(); treeSet != nil {
				pruneAbsentSkillEvidence(parsed, &out, treeSet)
			}
		}
		state.setCritiqueOutcome(out)
	}
	return parsed
}

func sanitizePublishedCitations(parsed analysisResponse, context analysisCitationContext) analysisResponse {
	const maxPublishedCitations = 20
	valid := make([]models.EvidenceCitation, 0, min(len(parsed.EvidenceCitations), maxPublishedCitations))
	if !context.Full {
		for _, citation := range parsed.EvidenceCitations {
			if evidenceCitationIssue(citation, context.Evidence) == "" {
				valid = append(valid, citation)
				if len(valid) == maxPublishedCitations {
					break
				}
			}
		}
	}
	parsed.EvidenceCitations = valid
	parsed.RootCause = removeUncitedLineClaims(parsed.RootCause, valid)
	parsed.Summary = removeUncitedLineClaims(parsed.Summary, valid)
	parsed.SuggestedFix = removeUncitedLineClaims(parsed.SuggestedFix, valid)
	return parsed
}

func removeUncitedLineClaims(text string, citations []models.EvidenceCitation) string {
	claims := proseLineClaims(text)
	if len(claims) == 0 {
		return text
	}
	var out strings.Builder
	last := 0
	for _, claim := range claims {
		out.WriteString(text[last:claim.MatchStart])
		supported := false
		for _, citation := range citations {
			if citationSupportsLineClaim(citation, claim) {
				supported = true
				break
			}
		}
		if supported {
			out.WriteString(text[claim.MatchStart:claim.MatchEnd])
		} else if text[claim.MatchStart] == ':' || text[claim.MatchStart] == '#' {
			// Keep the artifact path while dropping an unsupported attached suffix.
		} else {
			out.WriteString("the cited artifact evidence")
		}
		last = claim.MatchEnd
	}
	out.WriteString(text[last:])
	return out.String()
}

var longCLIFlagRE = regexp.MustCompile(`--[A-Za-z][A-Za-z0-9-]*`)
var shortCLIFlagRE = regexp.MustCompile(`(^|[[:space:]])(-[A-Za-z][A-Za-z0-9]*)`)

// unverifiedSourceReference stands in for a remediation path the model never
// opened. It is a single token with no shell metacharacters so that substituting
// it into a command leaves one obvious placeholder argument rather than several
// words the shell would read as separate paths.
const unverifiedSourceReference = "REPORTED_SOURCE_FILE"

type cliFlagMatch struct {
	Value string
	Start int
	End   int
}

func cliFlagMatches(text string) []cliFlagMatch {
	matches := make([]cliFlagMatch, 0)
	for _, indexes := range longCLIFlagRE.FindAllStringIndex(text, -1) {
		matches = append(matches, cliFlagMatch{Value: text[indexes[0]:indexes[1]], Start: indexes[0], End: indexes[1]})
	}
	for _, indexes := range shortCLIFlagRE.FindAllStringSubmatchIndex(text, -1) {
		matches = append(matches, cliFlagMatch{Value: text[indexes[4]:indexes[5]], Start: indexes[4], End: indexes[5]})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Start < matches[j].Start })
	return matches
}

func (s *agentState) preparePublishedAnalysis(parsed analysisResponse) analysisResponse {
	matchedSkills := matchSkillsForDraft(s, parsed)
	verified := make([]string, 0, len(parsed.RelevantFiles))
	suggestions := append([]string(nil), parsed.SearchSuggestions...)
	for _, candidate := range parsed.RelevantFiles {
		candidate = strings.TrimSpace(candidate)
		clean, err := artifacts.SafePath(candidate)
		if err != nil || clean == "" || !isSourceCitation(clean) {
			continue
		}
		if sourceReadMatches(strings.ToLower(clean), s.readSourceFull) {
			verified = append(verified, clean)
		} else {
			suggestions = append(suggestions, candidate)
		}
	}
	parsed.RelevantFiles = compactPublishedStrings(verified, 50)
	parsed.SearchSuggestions = compactPublishedStrings(suggestions, 50)
	parsed.CauseLocation = normalizeCauseLocation(parsed.CauseLocation, s.sourceOwner, s.sourceName, parsed.RelevantFiles)
	// A cause owned by a dependency cannot be remediated by this project, and a
	// dependency path resolved against this project's repository would link to
	// the wrong file, so it names the owning repository instead and keeps its
	// paths in the structured cause location. Decided before the sanitizer runs
	// and applied after it, so a source-shaped repository slug such as
	// nats-io/nats.go is never rewritten as a path.
	externalRemediation := parsed.CauseLocation != nil && parsed.CauseLocation.External &&
		s.hasUngroundedSourcePath(parsed.SuggestedFix, false)
	// Every prose field sheds ungrounded source paths. A diagnosis reads fine
	// without one, but a remediation's path is the object of its instruction, so
	// it is replaced by a placeholder instead of cut, and the path still reaches
	// the maintainer through search_suggestions. An artifact read does not ground
	// a remediation path: the critique's source scan accepts only a source read,
	// so keeping one here would publish a draft it then hard-fails.
	parsed.RootCause = s.removeUngroundedSourcePaths(parsed.RootCause, "", true)
	parsed.Summary = s.removeUngroundedSourcePaths(parsed.Summary, "", true)
	parsed.SuggestedFix = s.removeUngroundedSourcePaths(parsed.SuggestedFix, unverifiedSourceReference, false)
	if externalRemediation {
		parsed.SuggestedFix = externalRemediationFallback(parsed.CauseLocation)
	}
	// Flag grounding applies to the diagnosis, not the remedy. A root cause
	// citing a flag present nowhere in the evidence is a hallucination, but a
	// remediation proposes the flag that is missing, so requiring one to already
	// appear in the failing evidence would reject the most actionable fixes.
	parsed.RootCause = s.removeUngroundedCLIFlags(parsed.RootCause, matchedSkills)
	parsed.Summary = s.removeUngroundedCLIFlags(parsed.Summary, matchedSkills)
	return parsed
}

func (s *agentState) sourcePathGrounded(candidate string, allowArtifact bool) bool {
	clean := strings.ToLower(strings.TrimPrefix(candidate, "./"))
	return sourceReadMatches(clean, s.readSourceFull) || allowArtifact && readsArtifact(clean, s.readArtifactsFull, s.readArtifactsBase)
}

func (s *agentState) hasUngroundedSourcePath(text string, allowArtifact bool) bool {
	for _, candidate := range sourceCitationRE.FindAllString(text, -1) {
		if !s.sourcePathGrounded(candidate, allowArtifact) {
			return true
		}
	}
	return false
}

func (s *agentState) removeUngroundedSourcePaths(text, replacement string, allowArtifact bool) string {
	return sourceCitationRE.ReplaceAllStringFunc(text, func(candidate string) string {
		if s.sourcePathGrounded(candidate, allowArtifact) {
			return candidate
		}
		return replacement
	})
}

func readsArtifact(candidate string, full, base map[string]bool) bool {
	normalized := NormalizeArtifactCitation(candidate)
	if strings.Contains(normalized, "/") {
		return full[normalized]
	}
	return full[normalized] || base[path.Base(normalized)]
}

func (s *agentState) removeUngroundedCLIFlags(text string, matchedSkills []skills.Skill) string {
	flags := cliFlagMatches(text)
	if len(flags) == 0 {
		return text
	}
	var grounding []string
	for _, snippets := range s.evidenceContentByPath {
		grounding = append(grounding, snippets...)
	}
	for _, snippets := range s.sourceContentByPath {
		grounding = append(grounding, snippets...)
	}
	for _, skill := range matchedSkills {
		grounding = append(grounding, skill.Procedure)
	}
	joined := strings.Join(grounding, "\n")
	groundedFlags := map[string]bool{}
	for _, flag := range cliFlagMatches(joined) {
		groundedFlags[flag.Value] = true
	}
	unsupported := map[string]bool{}
	for _, flag := range flags {
		if !groundedFlags[flag.Value] {
			unsupported[flag.Value] = true
		}
	}
	if len(unsupported) == 0 {
		return text
	}
	var cleaned strings.Builder
	last := 0
	for _, flag := range flags {
		cleaned.WriteString(text[last:flag.Start])
		if !unsupported[flag.Value] {
			cleaned.WriteString(flag.Value)
		}
		last = flag.End
	}
	cleaned.WriteString(text[last:])
	return strings.Join(strings.Fields(cleaned.String()), " ")
}

func compactPublishedStrings(values []string, limit int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.Join(strings.Fields(value), " ")
		if value == "" || len(value) > 1024 || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
		if len(out) == limit {
			break
		}
	}
	return out
}

func (c *Client) applyPostLoopCritique(ctx context.Context, state *agentState, messages []modelMessage, finalContent string, finalProviderItems []json.RawMessage, parsed analysisResponse, opts AgenticOptions, retries *critiqueRetryBudget, draftObserved bool, draftPhase string) analysisResponse {
	if state.critiquePassed {
		return state.bestDraft.parsed
	}
	out := critiqueDraftWithContent(parsed, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, parsed), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
	if len(out.MissingSkillEvidence) > 0 {
		if treeSet := state.artifactTreeSet(); treeSet != nil {
			pruneAbsentSkillEvidence(parsed, &out, treeSet)
		}
	}
	if !draftObserved {
		if draftPhase == "initial" {
			draftPhase = "finalize"
		}
		candidate := state.newDraftCandidate(draftPhase, finalContent, finalProviderItems, parsed, out)
		semanticAccepted := draftPhase == "semantic_retry" && state.judgeObjected
		state.considerFallbackDraft(candidate, semanticAccepted)
		if state.considerDraft(candidate, semanticAccepted) && semanticAccepted {
			state.judgeRevised = true
			recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Status: semanticJudgeStageRevision, Outcome: "revised"})
		}
	} else if state.bestDraft == nil {
		candidate := state.newDraftCandidate(draftPhase, finalContent, finalProviderItems, parsed, out)
		semanticAccepted := draftPhase == "semantic_retry" && state.judgeObjected
		state.considerFallbackDraft(candidate, semanticAccepted)
		if state.considerDraft(candidate, semanticAccepted) && semanticAccepted {
			state.judgeRevised = true
			recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Status: semanticJudgeStageRevision, Outcome: "revised"})
		}
	}
	if out.Passed {
		recordTrace(ctx, critiqueTraceEvent("passed", out))
		if opts.SemanticJudge && !state.judgeRan {
			c.applySemanticJudgePostLoop(ctx, state, messages, finalContent, finalProviderItems, parsed, contextHeadroomFor(opts), effectiveCritiqueCachePolicy(opts.CritiqueCachePolicy, opts.CritiqueMaxRetries))
		}
		state.critiquePassed = state.bestDraft != nil && state.bestDraft.quality.Passed
		return state.bestDraft.parsed
	}
	recordTrace(ctx, critiqueTraceEvent("objected", out))
	return c.runBoundedCritiqueRepair(ctx, state, messages, finalContent, finalProviderItems, parsed, out, opts, retries)
}

const critiqueFinalizationReserve = 5 * time.Second

func (c *Client) runFinalizeRoundTracked(ctx context.Context, state *agentState, messages []modelMessage, headroom contextHeadroom) (string, []json.RawMessage, bool) {
	started := time.Now()
	content, items, safe := c.runFinalizeRound(ctx, messages, headroom)
	state.recentModelRequest = time.Since(started)
	return content, items, safe
}

func (c *Client) semanticCritiqueTracked(ctx context.Context, state *agentState, stage string, parsed analysisResponse, prior *analysisResponse, initialFindings []semanticFinding, headroom contextHeadroom) (semanticJudgeResult, error) {
	started := time.Now()
	result, err := c.semanticCritique(ctx, state, stage, parsed, prior, initialFindings, headroom)
	state.recentModelRequest = time.Since(started)
	return result, err
}

func critiqueTraceEvent(outcome string, out critiqueOutcome) TraceEvent {
	return TraceEvent{
		Kind: "critique", Outcome: outcome, IssueCount: len(out.Matches()),
		CritiquePunts: len(out.PuntMatches), CritiqueUnread: len(out.UnreadCitations),
		CritiqueCitations: len(out.CitationIssues), CritiqueSkills: len(out.MissingSkillEvidence),
		CritiqueGroups: out.MissingEvidenceCount(), CritiqueTransient: out.TransientPersistCount,
		CritiqueRules: critiqueRuleStrings(out.RuleIDs()),
	}
}

func selectedDraftAttempt(state *agentState) int {
	if state.bestDraft != nil {
		return state.bestDraft.attempt
	}
	return 0
}

func (c *Client) runBoundedCritiqueRepair(ctx context.Context, state *agentState, messages []modelMessage, finalContent string, finalProviderItems []json.RawMessage, parsed analysisResponse, initial critiqueOutcome, opts AgenticOptions, retries *critiqueRetryBudget) analysisResponse {
	if !retries.available() {
		recordTrace(ctx, TraceEvent{Kind: "critique_retry_denied", Outcome: "retry_budget", RetryDeniedReason: "retry_budget", InitialIssueCount: len(initial.Matches()), SelectedAttempt: selectedDraftAttempt(state), RemainingTimeMs: int(time.Until(state.deadline) / time.Millisecond)})
		return state.bestDraft.parsed
	}
	remaining := time.Until(state.deadline)
	required := 2*state.recentModelRequest + critiqueFinalizationReserve
	if remaining < required {
		recordTrace(ctx, TraceEvent{Kind: "critique_retry_denied", Outcome: "time_headroom", RetryDeniedReason: "time_headroom", InitialIssueCount: len(initial.Matches()), SelectedAttempt: selectedDraftAttempt(state), RemainingTimeMs: int(remaining / time.Millisecond)})
		return state.bestDraft.parsed
	}

	started := time.Now()
	initialEvidenceRevision := state.evidenceRevision
	injection := c.buildEvidenceInjection(ctx, state, initial)
	feedback := initial.Feedback
	if injection != "" {
		feedback += "\n\n" + injection
	}
	repairMessages := append(messages,
		modelMessage{Role: "assistant", Content: strPtr(finalContent), ProviderItems: finalProviderItems},
		modelMessage{Role: "user", Content: strPtr(feedback)})
	retry, _ := retries.admit()

	updated := critiqueDraftWithContent(parsed, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, parsed), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
	if len(updated.MissingSkillEvidence) > 0 {
		if treeSet := state.artifactTreeSet(); treeSet != nil {
			pruneAbsentSkillEvidence(parsed, &updated, treeSet)
		}
	}
	if critiqueRepairNeedsTools(updated) {
		remaining = time.Until(state.deadline)
		if remaining < 2*state.recentModelRequest+critiqueFinalizationReserve {
			recordTrace(ctx, TraceEvent{Kind: "critique_retry_denied", Outcome: "time_headroom", Retry: retry, RetryAdmitted: true, RetryDeniedReason: "time_headroom", InitialIssueCount: len(initial.Matches()), SelectedAttempt: selectedDraftAttempt(state), RetryDurationMs: int(time.Since(started) / time.Millisecond), RemainingTimeMs: int(remaining / time.Millisecond)})
			return state.bestDraft.parsed
		}
		schemas := state.registry.Schemas(state.enabledTools)
		var parallelToolCalls *bool
		if opts.SingleToolCall {
			f := false
			parallelToolCalls = &f
		}
		var fits bool
		repairMessages, fits = prepareContextRequest(ctx, repairMessages, schemaPayloadBytes(schemas), contextHeadroomFor(opts), "critique_repair")
		if !fits {
			recordTrace(ctx, TraceEvent{Kind: "critique_retry_denied", Outcome: "context_headroom", Retry: retry, RetryAdmitted: true, RetryDeniedReason: "context_headroom", InitialIssueCount: len(initial.Matches()), SelectedAttempt: selectedDraftAttempt(state), RetryDurationMs: int(time.Since(started) / time.Millisecond), RemainingTimeMs: int(time.Until(state.deadline) / time.Millisecond)})
			return state.bestDraft.parsed
		}
		requestStart := time.Now()
		resp, err := c.callModel(ctx, repairMessages, schemas, parallelToolCalls)
		state.recentModelRequest = time.Since(requestStart)
		if err != nil || !resp.HasMessage {
			recordTrace(ctx, TraceEvent{Kind: "critique_retry", Outcome: "tool_turn_error", Retry: retry, RetryAdmitted: true, InitialIssueCount: len(initial.Matches()), RetryDurationMs: int(time.Since(started) / time.Millisecond)})
			return state.bestDraft.parsed
		}
		msg := resp.Message
		toolCalls, _ := limitToolCalls(msg.ToolCalls, opts.SingleToolCall)
		echoCalls, skippedOutputs := continuationCalls(c.apiMode, msg, toolCalls)
		echo := modelMessage{Role: "assistant", ToolCalls: echoCalls, ProviderItems: msg.ProviderItems}
		if msg.Content != nil {
			echo.Content = msg.Content
		}
		repairMessages = append(repairMessages, echo)
		repairMessages = append(repairMessages, skippedOutputs...)
		for _, tc := range toolCalls {
			result := dispatchAgenticTool(ctx, state, tc)
			state.modelBytes += len(result)
			repairMessages = append(repairMessages, modelMessage{Role: "tool", ToolCallID: tc.ID, Content: strPtr(result)})
		}
	}

	remaining = time.Until(state.deadline)
	if remaining < state.recentModelRequest+critiqueFinalizationReserve {
		recordTrace(ctx, TraceEvent{Kind: "critique_retry_denied", Outcome: "time_headroom", Retry: retry, RetryAdmitted: true, RetryDeniedReason: "time_headroom", InitialIssueCount: len(initial.Matches()), SelectedAttempt: selectedDraftAttempt(state), RetryDurationMs: int(time.Since(started) / time.Millisecond), RemainingTimeMs: int(remaining / time.Millisecond)})
		return state.bestDraft.parsed
	}

	revised, revisedItems, safe := c.runFinalizeRoundTracked(ctx, state, repairMessages, contextHeadroomFor(opts))
	if !safe {
		recordTrace(ctx, TraceEvent{Kind: "critique_retry_denied", Outcome: "context_headroom", Retry: retry, RetryAdmitted: true, RetryDeniedReason: "context_headroom", InitialIssueCount: len(initial.Matches()), SelectedAttempt: selectedDraftAttempt(state), RetryDurationMs: int(time.Since(started) / time.Millisecond), RemainingTimeMs: int(time.Until(state.deadline) / time.Millisecond)})
		return state.bestDraft.parsed
	}
	next, ok := tryParseAnalysis(revised)
	if !ok {
		recordTrace(ctx, TraceEvent{Kind: "critique_retry", Outcome: "unparseable", Retry: retry, RetryAdmitted: true, InitialIssueCount: len(initial.Matches()), NewEvidenceReads: state.evidenceRevision - initialEvidenceRevision, RetryDurationMs: int(time.Since(started) / time.Millisecond), RemainingTimeMs: int(time.Until(state.deadline) / time.Millisecond), SelectedAttempt: state.bestDraft.attempt})
		return state.bestDraft.parsed
	}
	out := critiqueDraftWithContent(next, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, next), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
	if len(out.MissingSkillEvidence) > 0 {
		if treeSet := state.artifactTreeSet(); treeSet != nil {
			pruneAbsentSkillEvidence(next, &out, treeSet)
		}
	}
	candidate := state.newDraftCandidate("critique_retry", revised, revisedItems, next, out)
	state.considerFallbackDraft(candidate, false)
	state.considerDraft(candidate, false)
	state.critiquePassed = state.bestDraft.quality.Passed
	if state.critiquePassed && opts.SemanticJudge && !state.judgeRan {
		selected := state.bestDraft
		c.applySemanticJudgePostLoop(ctx, state, repairMessages, selected.content, selected.providerItems, selected.parsed, contextHeadroomFor(opts), effectiveCritiqueCachePolicy(opts.CritiqueCachePolicy, opts.CritiqueMaxRetries))
		state.critiquePassed = state.bestDraft.quality.Passed
	}
	selected := state.bestDraft
	selectedOut := critiqueDraftWithContent(selected.parsed, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, selected.parsed), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
	if len(selectedOut.MissingSkillEvidence) > 0 {
		if treeSet := state.artifactTreeSet(); treeSet != nil {
			pruneAbsentSkillEvidence(selected.parsed, &selectedOut, treeSet)
		}
	}
	recordTrace(ctx, critiqueTraceEvent("revised", selectedOut))
	recordTrace(ctx, TraceEvent{
		Kind: "critique_retry", Outcome: "completed", Retry: retry, RetryAdmitted: true,
		InitialIssueCount: len(initial.Matches()), RevisedIssueCount: len(selectedOut.Matches()),
		NewEvidenceReads: state.evidenceRevision - initialEvidenceRevision,
		RootCauseChanged: rootCauseMateriallyChanged(parsed.RootCause, selected.parsed.RootCause),
		SelectedAttempt:  selected.attempt, RetryDurationMs: int(time.Since(started) / time.Millisecond),
		RemainingTimeMs: int(time.Until(state.deadline) / time.Millisecond),
	})
	return state.bestDraft.parsed
}

func critiqueRepairNeedsTools(out critiqueOutcome) bool {
	return out.MissingArtifactCitationNeedsTool || len(out.UnreadCitations) > 0 || len(out.CitationIssues) > 0 || len(out.MissingSkillEvidence) > 0
}

func (s *agentState) observeDraft(phase string, parsed analysisResponse, out, publishedOut critiqueOutcome) int {
	s.draftAttempt++
	attempt := s.draftAttempt
	if s.draftObserver == nil {
		return attempt
	}
	summary := parsed.Summary
	if summary == "" {
		summary = firstSentence(parsed.RootCause)
	}
	s.draftObserver(DraftObservation{
		Attempt:             attempt,
		Phase:               phase,
		Summary:             summary,
		RootCause:           parsed.RootCause,
		SuggestedFix:        parsed.SuggestedFix,
		IsTransient:         parsed.IsTransient,
		Severity:            parsed.Severity,
		RelevantFiles:       append([]string(nil), parsed.RelevantFiles...),
		PuntCount:           len(out.PuntMatches),
		UnreadCitationCount: len(out.UnreadCitations),
		CitationIssueCount:  len(out.CitationIssues),
		MissingGroupCount:   out.MissingEvidenceCount(),
		TransientConflict:   out.TransientPersistCount > 0,
		RuleIDs:             critiqueRuleStrings(out.RuleIDs()),
		MatchedSkillIDs:     append([]string(nil), out.MatchedSkillIDs...),
		MissingGroups:       critiqueEvidenceGroupRefs(out.MissingSkillEvidence),
		UnavailableGroups:   critiqueEvidenceGroupRefs(out.UnavailableSkillEvidence),
		PublishedRuleIDs:    critiqueRuleStrings(publishedOut.RuleIDs()),
		PublishedHardRules:  critiqueRuleStrings(publishedOut.HardRuleIDs()),
		PublishedSoftRules:  critiqueRuleStrings(publishedOut.SoftRuleIDs()),
		PublishedHardIssues: critiqueHardIssueCount(publishedOut),
		PublishedPuntCount:  len(publishedOut.PuntMatches),
		PublishedMissing:    publishedOut.MissingEvidenceCount(),
		ToolCalls:           s.calls,
		EvidenceReads:       len(s.readArtifactsFull),
	})
	return attempt
}

func critiqueQualityFor(out critiqueOutcome) critiqueQuality {
	return critiqueQuality{
		Passed:               out.Passed,
		HardRules:            critiqueRuleStrings(out.HardRuleIDs()),
		SoftRules:            critiqueRuleStrings(out.SoftRuleIDs()),
		HardIssueCount:       critiqueHardIssueCount(out),
		MissingEvidenceCount: out.MissingEvidenceCount(),
		PuntCount:            len(out.PuntMatches),
	}
}

func critiqueHardIssueCount(out critiqueOutcome) int {
	n := len(out.UnreadCitations)
	if out.MissingArtifactCitation {
		n++
	}
	for _, issue := range out.CitationIssues {
		if critiqueRuleSeverity(critiqueCitationRule(issue)) == CritiqueRuleHard {
			n++
		}
	}
	if out.TransientPersistCount > 0 {
		n++
	}
	if out.RerunOnlyRemediation {
		n++
	}
	return n
}

// compareCritiqueQuality returns positive only when a strictly dominates b.
func compareCritiqueQuality(a, b critiqueQuality) int {
	if critiqueQualityDominates(a, b) {
		return 1
	}
	if critiqueQualityDominates(b, a) {
		return -1
	}
	return 0
}

func critiqueQualityDominates(candidate, current critiqueQuality) bool {
	if !critiqueQualityNoWorse(candidate, current) {
		return false
	}
	hardImproved := candidate.HardIssueCount < current.HardIssueCount
	if hardImproved {
		return true
	}
	return candidate.PuntCount < current.PuntCount || candidate.MissingEvidenceCount < current.MissingEvidenceCount
}

func critiqueQualityNoWorse(candidate, current critiqueQuality) bool {
	if critiqueHardRegression(candidate, current) {
		return false
	}
	if candidate.HardIssueCount < current.HardIssueCount {
		return true
	}
	return candidate.PuntCount <= current.PuntCount && candidate.MissingEvidenceCount <= current.MissingEvidenceCount
}

func critiqueHardRegression(candidate, current critiqueQuality) bool {
	return candidate.HardIssueCount > current.HardIssueCount || !stringSetSubset(candidate.HardRules, current.HardRules)
}

func critiqueQualityAcceptedForPolicy(quality critiqueQuality, policy CritiqueCachePolicy) bool {
	switch policy {
	case CritiqueCachePolicyAdvisory:
		return true
	case CritiqueCachePolicyHard:
		return quality.HardIssueCount == 0
	case CritiqueCachePolicyStrict:
		return quality.Passed
	default:
		return false
	}
}

func stringSetSubset(candidate, current []string) bool {
	if len(candidate) > len(current) {
		return false
	}
	set := make(map[string]bool, len(current))
	for _, value := range current {
		set[value] = true
	}
	for _, value := range candidate {
		if !set[value] {
			return false
		}
	}
	return true
}

var rootCauseTokenRE = regexp.MustCompile(`[a-z0-9]+(?:[._/-][a-z0-9]+)*`)

var rootCauseStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true,
	"at": true, "because": true, "before": true, "by": true, "caused": true,
	"causes": true, "due": true, "error": true, "failed": true, "failure": true,
	"for": true, "from": true, "in": true, "is": true, "of": true,
	"on": true, "or": true, "that": true, "the": true, "this": true,
	"to": true, "was": true, "were": true, "with": true,
}

// rootCauseMateriallyChanged ignores formatting but treats diagnosis token
// additions, deletions, reordering, and negation as material.
func rootCauseMateriallyChanged(a, b string) bool {
	return rootCauseFingerprint(a) != rootCauseFingerprint(b)
}

func rootCauseFingerprint(rootCause string) string {
	var tokens []string
	for _, token := range rootCauseTokenRE.FindAllString(strings.ToLower(rootCause), -1) {
		if !rootCauseStopwords[token] {
			tokens = append(tokens, token)
		}
	}
	return strings.Join(tokens, " ")
}

func (s *agentState) newDraftCandidate(phase, content string, providerItems []json.RawMessage, parsed analysisResponse, out critiqueOutcome) *critiqueDraftCandidate {
	published := s.publishedAnalysis(parsed)
	publishedOut := s.currentCritiqueOutcome(published)
	return &critiqueDraftCandidate{
		parsed:                  parsed,
		content:                 content,
		providerItems:           providerItems,
		rawQuality:              critiqueQualityFor(out),
		quality:                 critiqueQualityFor(publishedOut),
		attempt:                 s.observeDraft(phase, parsed, out, publishedOut),
		evidenceRevision:        s.evidenceRevision,
		createdEvidenceRevision: s.evidenceRevision,
		supportedFacts:          supportedCausalFacts(published, s.analysisEvidence, s.analysisEvidenceRevision),
		semanticRevision:        phase == "semantic_retry",
	}
}

func (s *agentState) publishedCritiqueOutcome(parsed analysisResponse) critiqueOutcome {
	return s.currentCritiqueOutcome(s.publishedAnalysis(parsed))
}

func (s *agentState) publishedAnalysis(parsed analysisResponse) analysisResponse {
	parsed = sanitizePublishedCitations(parsed, analysisCitationContext{Evidence: s.analysisEvidence, Full: s.analysisEvidenceFull})
	parsed = s.preparePublishedAnalysis(parsed)
	return parsed
}

func (s *agentState) currentCritiqueOutcome(parsed analysisResponse) critiqueOutcome {
	out := critiqueDraftWithContent(parsed, s.readArtifactsFull, s.readArtifactsBase, s.evidenceContentByPath, s.readSourceFull, matchSkillsForDraft(s, parsed), s.consecutiveFailures, analysisCitationContext{Evidence: s.analysisEvidence, Full: s.analysisEvidenceFull})
	if len(out.MissingSkillEvidence) > 0 {
		if treeSet := s.artifactTreeSet(); treeSet != nil {
			pruneAbsentSkillEvidence(parsed, &out, treeSet)
		}
	}
	return out
}

// considerDraft applies deterministic quality ordering and records the decision.
func (s *agentState) considerDraft(candidate *critiqueDraftCandidate, semanticAccepted bool) bool {
	return s.considerDraftDecision(candidate, semanticAccepted).accepted
}

func (s *agentState) considerDraftDecision(candidate *critiqueDraftCandidate, semanticAccepted bool) draftReplacementDecision {
	policy := effectiveCritiqueCachePolicy(s.opts.CritiqueCachePolicy, s.opts.CritiqueMaxRetries)
	return s.considerDraftDecisionForPolicy(candidate, semanticAccepted, policy)
}

func (s *agentState) considerDraftDecisionForPolicy(candidate *critiqueDraftCandidate, semanticAccepted bool, policy CritiqueCachePolicy) draftReplacementDecision {
	decision := s.evaluateDraftReplacement(s.bestDraft, candidate, semanticAccepted, policy)
	s.recordDraftDecision("best", s.bestDraft, candidate, decision)
	if decision.accepted {
		s.bestDraft = candidate
	}
	return decision
}

func (s *agentState) considerFallbackDraft(candidate *critiqueDraftCandidate, semanticAccepted bool) bool {
	policy := effectiveCritiqueCachePolicy(s.opts.CritiqueCachePolicy, s.opts.CritiqueMaxRetries)
	return s.considerFallbackDraftForPolicy(candidate, semanticAccepted, policy)
}

func (s *agentState) considerFallbackDraftForPolicy(candidate *critiqueDraftCandidate, semanticAccepted bool, policy CritiqueCachePolicy) bool {
	decision := s.evaluateDraftReplacement(s.fallbackDraft, candidate, semanticAccepted, policy)
	s.recordDraftDecision("fallback", s.fallbackDraft, candidate, decision)
	if decision.accepted {
		s.fallbackDraft = candidate
	}
	return decision.accepted
}

func (s *agentState) evaluateDraftReplacement(current, candidate *critiqueDraftCandidate, semanticAccepted bool, policy CritiqueCachePolicy) draftReplacementDecision {
	refreshed := false
	if current != nil && candidate != nil {
		rootChanged := rootCauseMateriallyChanged(current.parsed.RootCause, candidate.parsed.RootCause)
		newEvidence := candidate.evidenceRevision > current.evidenceRevision
		// Preserve the earlier draft's historical quality when new evidence drove a
		// different diagnosis. Re-evaluating it with evidence fetched for the new
		// diagnosis can give the earlier text credit for evidence it never used.
		preserveHistorical := !semanticAccepted && rootChanged && newEvidence
		if semanticAccepted {
			current.rawQuality = critiqueQualityFor(s.currentCritiqueOutcome(current.parsed))
		}
		if current.evidenceRevision < candidate.evidenceRevision && !preserveHistorical {
			s.refreshPublishedDraftQuality(current)
			current.evidenceRevision = candidate.evidenceRevision
			refreshed = true
		}
	}
	decision := decideDraftReplacement(current, candidate, semanticAccepted, policy)
	decision.currentQualityRefreshed = refreshed
	return decision
}

func (s *agentState) refreshPublishedDraftQuality(candidate *critiqueDraftCandidate) {
	if candidate == nil {
		return
	}
	candidate.quality = critiqueQualityFor(s.publishedCritiqueOutcome(candidate.parsed))
}

func decideDraftReplacement(current, candidate *critiqueDraftCandidate, semanticAccepted bool, policy CritiqueCachePolicy) draftReplacementDecision {
	decision := draftReplacementDecision{reason: draftReasonCandidateNotBetter}
	if candidate == nil {
		return decision
	}
	if current == nil {
		decision.accepted = true
		decision.publishedStrictDominance = true
		decision.reason = draftReasonCandidatePublishedDominates
		return decision
	}
	factDelta := compareSupportedCausalFacts(
		current.supportedFacts,
		candidate.supportedFacts,
		semanticInitialFindingsAllowCauseReplacement(candidate.semanticInitialFindingClasses),
		current.createdEvidenceRevision,
	)
	decision.currentSupportedFacts = len(current.supportedFacts)
	decision.candidateSupportedFacts = len(candidate.supportedFacts)
	decision.supportedFactsRetained = factDelta.retained
	decision.supportedFactsAdded = factDelta.added
	decision.supportedFactsDropped = factDelta.dropped
	decision.rootCauseChanged = rootCauseMateriallyChanged(current.parsed.RootCause, candidate.parsed.RootCause)
	publishedHardRegression := critiqueHardRegression(candidate.quality, current.quality)
	decision.rawSemanticRegression = semanticAccepted &&
		critiqueHardRegression(candidate.rawQuality, current.rawQuality) && publishedHardRegression
	if decision.rawSemanticRegression {
		decision.reason = draftReasonCandidateSemanticRegression
		return decision
	}
	if publishedHardRegression {
		decision.reason = draftReasonCandidatePublishedHard
		return decision
	}
	if semanticAccepted && !critiqueQualityAcceptedForPolicy(candidate.quality, policy) {
		if candidate.quality.HardIssueCount > 0 {
			decision.reason = draftReasonCandidatePublishedHard
		} else {
			// A revision the semantic judge passed can still be blocked by a
			// retained soft rule such as unread available evidence. Name that
			// distinctly so telemetry does not read as "not better".
			decision.reason = draftReasonCandidatePolicyUnaccepted
		}
		return decision
	}
	if semanticAccepted && candidate.semanticRevision && len(candidate.semanticFindingClasses) > 0 {
		decision.reason = draftReasonCandidateSemanticFindings
		return decision
	}
	if semanticAccepted && candidate.semanticRevision && !candidate.semanticReviewPassed {
		decision.reason = draftReasonCandidateSemanticUnavailable
		return decision
	}
	if semanticAccepted && candidate.semanticRevision && decision.rootCauseChanged && factDelta.dropped > 0 && !factDelta.strongerReplacement {
		decision.supportedCauseRegression = true
		decision.reason = draftReasonCandidateDropsSupportedCause
		return decision
	}
	if decision.rootCauseChanged && candidate.evidenceRevision <= current.evidenceRevision && !semanticAccepted {
		decision.reason = draftReasonCandidateRootWithoutEvidence
		return decision
	}
	comparison := compareCritiqueQuality(candidate.quality, current.quality)
	evidenceBackedChange := !semanticAccepted && decision.rootCauseChanged && candidate.evidenceRevision > current.evidenceRevision && critiqueQualityNoWorse(candidate.quality, current.quality)
	semanticTie := semanticAccepted && critiqueQualityEqual(candidate.quality, current.quality)
	switch {
	case comparison > 0:
		decision.accepted = true
		decision.publishedStrictDominance = true
		decision.reason = draftReasonCandidatePublishedDominates
	case evidenceBackedChange:
		decision.accepted = true
		decision.reason = draftReasonCandidateEvidenceBackedRoot
	case semanticTie:
		decision.accepted = true
		decision.reason = draftReasonSemanticTieReplaces
	case critiqueQualityEqual(candidate.quality, current.quality):
		decision.reason = draftReasonTiePreservesEarlier
	default:
		decision.reason = draftReasonCandidateNotBetter
	}
	return decision
}

func draftShouldReplace(current, candidate *critiqueDraftCandidate, semanticAccepted bool) bool {
	return decideDraftReplacement(current, candidate, semanticAccepted, CritiqueCachePolicyHard).accepted
}

func critiqueQualityEqual(a, b critiqueQuality) bool {
	return a.Passed == b.Passed && a.HardIssueCount == b.HardIssueCount &&
		slices.Equal(a.HardRules, b.HardRules) && slices.Equal(a.SoftRules, b.SoftRules) &&
		a.MissingEvidenceCount == b.MissingEvidenceCount && a.PuntCount == b.PuntCount
}

func (s *agentState) recordDraftDecision(target string, current, candidate *critiqueDraftCandidate, decision draftReplacementDecision) {
	if candidate == nil {
		return
	}
	trace := &DraftDecisionTrace{
		Target:                          target,
		CandidateAttempt:                candidate.attempt,
		CandidateRawHardRules:           append([]string(nil), candidate.rawQuality.HardRules...),
		CandidateRawSoftRules:           append([]string(nil), candidate.rawQuality.SoftRules...),
		CandidatePublishedHardRules:     append([]string(nil), candidate.quality.HardRules...),
		CandidatePublishedSoftRules:     append([]string(nil), candidate.quality.SoftRules...),
		CandidatePublishedHardIssues:    candidate.quality.HardIssueCount,
		CandidatePublishedMissingGroups: candidate.quality.MissingEvidenceCount,
		CandidatePublishedPunts:         candidate.quality.PuntCount,
		CandidateEvidenceRevision:       candidate.evidenceRevision,
		RootCauseMateriallyChanged:      decision.rootCauseChanged,
		RawSemanticRegression:           decision.rawSemanticRegression,
		PublishedStrictDominance:        decision.publishedStrictDominance,
		CurrentQualityRefreshed:         decision.currentQualityRefreshed,
		CurrentSupportedFacts:           decision.currentSupportedFacts,
		CandidateSupportedFacts:         decision.candidateSupportedFacts,
		SupportedFactsRetained:          decision.supportedFactsRetained,
		SupportedFactsAdded:             decision.supportedFactsAdded,
		SupportedFactsDropped:           decision.supportedFactsDropped,
		SupportedCauseRegression:        decision.supportedCauseRegression,
		ReplacementAccepted:             decision.accepted,
		ReplacementReason:               decision.reason,
	}
	if current != nil {
		trace.CurrentAttempt = current.attempt
		trace.CurrentRawHardRules = append([]string(nil), current.rawQuality.HardRules...)
		trace.CurrentRawSoftRules = append([]string(nil), current.rawQuality.SoftRules...)
		trace.CurrentPublishedHardRules = append([]string(nil), current.quality.HardRules...)
		trace.CurrentPublishedSoftRules = append([]string(nil), current.quality.SoftRules...)
		trace.CurrentPublishedHardIssues = current.quality.HardIssueCount
		trace.CurrentPublishedMissingGroups = current.quality.MissingEvidenceCount
		trace.CurrentPublishedPunts = current.quality.PuntCount
		trace.CurrentEvidenceRevision = current.evidenceRevision
	}
	outcome := "rejected"
	if decision.accepted {
		outcome = "accepted"
	}
	recordTrace(s.traceCtx, TraceEvent{Kind: "draft_selection", Outcome: outcome, Status: target, DraftDecision: trace})
}

func (s *agentState) promoteFallbackDraft() *critiqueDraftCandidate {
	if s.fallbackDraft != nil {
		decision := draftReplacementDecision{accepted: true, reason: draftReasonFallbackPromoted}
		s.recordDraftDecision("promotion", s.bestDraft, s.fallbackDraft, decision)
		s.bestDraft = s.fallbackDraft
	}
	return s.bestDraft
}

func (s *agentState) notifyDraftSelection() {
	if s.selectionObserver != nil && s.bestDraft != nil {
		s.selectionObserver(s.bestDraft.attempt)
	}
}

// listInitialArtifactTree fetches the one bounded tree snapshot shared by the
// initial seed, ranked evidence plan, and complete-tree absence checks.
func listInitialArtifactTree(ctx context.Context, browser artifacts.Browser) artifactTreeSnapshot {
	if browser == nil {
		return artifactTreeSnapshot{failed: true}
	}
	paths, truncated, err := browser.ListTree(ctx, initialArtifactTreeMaxPaths)
	if err != nil {
		log.Printf("  ⓘ artifact-tree seed and evidence plan skipped: %v", err)
		return artifactTreeSnapshot{failed: true}
	}
	return artifactTreeSnapshot{paths: paths, truncated: truncated}
}

// buildArtifactTreeSeed returns a prompt addendum listing the build's artifact
// paths from a prior tree snapshot. It drops non-text noise, then caps the seed
// by path count and bytes.
func buildArtifactTreeSeed(raw []string, rawTruncated bool, maxBytes int) string {
	paths := make([]string, 0, len(raw))
	for _, artifactPath := range raw {
		if artifactTreeNoiseExt[strings.ToLower(path.Ext(artifactPath))] {
			continue
		}
		paths = append(paths, artifactPath)
	}
	if len(paths) == 0 {
		return ""
	}
	sort.Strings(paths)
	truncated := rawTruncated
	if len(paths) > artifactTreeMaxPaths {
		paths = paths[:artifactTreeMaxPaths]
		truncated = true
	}
	var lines strings.Builder
	kept := 0
	for _, artifactPath := range paths {
		if maxBytes > 0 && lines.Len()+len(artifactPath)+1 > maxBytes {
			truncated = true
			break
		}
		lines.WriteString(artifactPath)
		lines.WriteByte('\n')
		kept++
	}
	if kept == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Artifact paths for this build (%d file(s)). These are the EXACT paths to pass to read_artifact / tail_artifact / grep_artifact; do NOT guess paths, and do NOT spend tool calls on list_artifacts / find_artifacts to rediscover paths that are already listed here. Read the relevant logs directly:\n", kept)
	b.WriteString(lines.String())
	if truncated {
		b.WriteString("... [list truncated; use list_artifacts for subtrees not shown above]\n")
	}
	log.Printf("  🗂 artifact-tree seed: %d path(s) injected (%d bytes)", kept, b.Len())
	return b.String()
}

func prependPrompt(prompt, section string) string {
	section = strings.TrimSpace(section)
	if section == "" {
		return prompt
	}
	return section + "\n\n---\n\n" + prompt
}

// buildEvidenceInjection fetches evidence a critique-failing draft needed but
// did not read, and returns a feedback addendum embedding it. Ranked initial
// candidates direct skill repair; unresolved groups and unread basenames share
// one bounded fallback tree walk. Content-aware groups try ranked candidates in
// order until one artifact provides positive proof or the shared cap is reached.
func (c *Client) buildEvidenceInjection(ctx context.Context, state *agentState, out critiqueOutcome) string {
	ctx = withEvidenceReadSource(ctx, EvidenceReadSourceRepairInjection)
	if state == nil || state.browser == nil {
		return ""
	}
	var sections []string
	fetched := 0
	attempted := map[string]bool{}
	fetchTail := func(rawPath string) (string, bool) {
		if fetched >= evidenceInjectionMaxArtifacts {
			return "", false
		}
		realPath, err := artifacts.SafePath(strings.TrimSpace(rawPath))
		if err != nil || realPath == "" {
			return "", false
		}
		if attempted[realPath] {
			return "", false
		}
		attempted[realPath] = true
		res, err := state.browser.Tail(ctx, realPath, 200, evidenceInjectionPerArtifactBytes)
		if err != nil || res == nil || len(bytes.TrimSpace(res.Content)) == 0 {
			return "", false
		}
		content := res.Content
		if len(content) > evidenceInjectionPerArtifactBytes {
			content = content[len(content)-evidenceInjectionPerArtifactBytes:]
		}
		return string(content), true
	}
	add := func(realPath, label, content string) {
		state.gcsBytes += len(content)
		state.modelBytes += len(content)
		state.recordSuccessfulRead(realPath)
		state.recordEvidenceSnippets(realPath, []string{content})
		sections = append(sections, fmt.Sprintf("### %s\n%s", label, content))
		fetched++
	}

	type walkTarget struct {
		match func(string) bool
		label func(string) string
	}
	var walkTargets []walkTarget
	type unreadWalk struct {
		target int
	}
	var unreadWalks []unreadWalk

	// Fetch exact unread citations first. Bare names and failed exact reads use
	// the shared fallback walk without retrying the same path.
	for _, cited := range out.UnreadCitations {
		if strings.Contains(cited, "/") {
			if content, ok := fetchTail(cited); ok {
				add(cited, cited+" (tail)", content)
				continue
			}
		}
		base := path.Base(cited)
		citedCopy := cited
		unreadWalks = append(unreadWalks, unreadWalk{target: len(walkTargets)})
		walkTargets = append(walkTargets, walkTarget{
			match: func(p string) bool { return strings.EqualFold(path.Base(p), base) },
			label: func(real string) string {
				return fmt.Sprintf("%s (tail; nearest match for cited %q)", real, citedCopy)
			},
		})
	}

	type groupTarget struct {
		skillID    string
		group      skills.EvidenceGroup
		candidates []string
		walkIndex  int
	}
	var groups []groupTarget
	for _, miss := range out.MissingSkillEvidence {
		for _, group := range miss.Missing {
			if group.SatisfiedWithContent(state.readArtifactsFull, state.evidenceContentByPath) {
				continue
			}
			target := groupTarget{skillID: miss.Skill.ID, group: group, walkIndex: -1}
			candidates, planned := initialPlanCandidates(state.initialEvidencePlan, miss.Skill.ID, group.ID)
			if planned {
				for _, candidate := range candidates {
					realPath, err := artifacts.SafePath(strings.TrimSpace(candidate))
					norm := NormalizeArtifactCitation(realPath)
					if err == nil && realPath != "" && norm != "" && group.Satisfied(map[string]bool{norm: true}) {
						target.candidates = append(target.candidates, realPath)
					}
				}
			}
			if len(target.candidates) == 0 {
				groupCopy := group
				target.walkIndex = len(walkTargets)
				walkTargets = append(walkTargets, walkTarget{
					match: func(p string) bool {
						norm := NormalizeArtifactCitation(p)
						return norm != "" && groupCopy.Satisfied(map[string]bool{norm: true})
					},
				})
			}
			groups = append(groups, target)
		}
	}

	var walked [][]string
	if len(walkTargets) > 0 && fetched < evidenceInjectionMaxArtifacts {
		preds := make([]func(string) bool, len(walkTargets))
		for i := range walkTargets {
			preds[i] = walkTargets[i].match
		}
		walked = resolveEvidenceCandidatesByWalk(ctx, state.browser, preds, evidenceplan.CandidatePathLimit)
	}
	for _, target := range unreadWalks {
		if target.target >= len(walked) || len(walked[target.target]) == 0 || fetched >= evidenceInjectionMaxArtifacts {
			continue
		}
		realPath := walked[target.target][0]
		if content, ok := fetchTail(realPath); ok {
			add(realPath, walkTargets[target.target].label(realPath), content)
		}
	}
	for i := range groups {
		if fetched >= evidenceInjectionMaxArtifacts {
			break
		}
		target := &groups[i]
		if target.group.SatisfiedWithContent(state.readArtifactsFull, state.evidenceContentByPath) {
			continue
		}
		if len(target.candidates) == 0 && target.walkIndex >= 0 && target.walkIndex < len(walked) {
			target.candidates = append(target.candidates, walked[target.walkIndex]...)
		}
		for _, candidate := range target.candidates {
			if fetched >= evidenceInjectionMaxArtifacts {
				break
			}
			realPath, err := artifacts.SafePath(strings.TrimSpace(candidate))
			norm := NormalizeArtifactCitation(realPath)
			if err != nil || realPath == "" || norm == "" || attempted[realPath] || !target.group.Satisfied(map[string]bool{norm: true}) {
				continue
			}
			content, ok := fetchTail(realPath)
			if !ok {
				continue
			}
			add(realPath, fmt.Sprintf("%s (tail; required evidence %q for skill %q)", realPath, target.group.ID, target.skillID), content)
			if target.group.SatisfiedWithContent(state.readArtifactsFull, state.evidenceContentByPath) {
				break
			}
		}
	}

	if fetched == 0 {
		return ""
	}
	log.Printf("  📎 evidence injection: fetched %d artifact(s) into the retry", fetched)
	return "The engine fetched evidence you cited but had not read, and/or evidence required for this failure class. Ground your root_cause in what these artifacts ACTUALLY show below; correct or drop any claim they do not support.\n\n" + strings.Join(sections, "\n\n")
}

func initialPlanCandidates(plan []skills.PlannedSkill, skillID, groupID string) ([]string, bool) {
	for _, plannedSkill := range plan {
		if plannedSkill.ID != skillID {
			continue
		}
		for _, group := range plannedSkill.RequiredEvidence {
			if group.ID == groupID {
				return group.CandidatePaths, true
			}
		}
		return nil, false
	}
	return nil, false
}

// resolveEvidenceByWalk lists the build's artifact tree once and returns the
// first matching real path for each predicate, or "" if unmatched. Bounded by
// evidenceTreeMaxPaths to cap GCS list cost. Stops early once every predicate
// has a match.
func resolveEvidenceByWalk(ctx context.Context, browser artifacts.Browser, preds []func(string) bool) []string {
	candidates := resolveEvidenceCandidatesByWalk(ctx, browser, preds, 1)
	found := make([]string, len(candidates))
	for i := range candidates {
		if len(candidates[i]) > 0 {
			found[i] = candidates[i][0]
		}
	}
	return found
}

func resolveEvidenceCandidatesByWalk(ctx context.Context, browser artifacts.Browser, preds []func(string) bool, limit int) [][]string {
	found := make([][]string, len(preds))
	if browser == nil || len(preds) == 0 || limit <= 0 {
		return found
	}
	paths, _, err := browser.ListTree(ctx, evidenceTreeMaxPaths)
	if err != nil {
		return found
	}
	sort.Strings(paths)
	for _, p := range paths {
		for i, pred := range preds {
			if len(found[i]) < limit && pred(p) {
				found[i] = append(found[i], p)
			}
		}
	}
	return found
}

// matchSkillsForDraft joins the candidate draft's prose fields and matches
// them against the loaded recipe set. Returns nil if skills are disabled or
// no recipes are loaded. Used by both the in-loop and post-loop critique so
// both paths match against the same draft text.
func matchSkillsForDraft(state *agentState, parsed analysisResponse) []skills.Skill {
	if state == nil || state.skillSet == nil {
		return nil
	}
	return state.skillSet.Match(strings.Join(parsed.proseFields(), "\n"))
}

// cacheAcceptedAnalysis evaluates the independent floor, critique, and semantic
// gates, then records the exact persistence outcome.
func (c *Client) cacheAcceptedAnalysis(ctx context.Context, cacheKey string, parsed analysisResponse, generatedAt string, state *agentState, opts AgenticOptions) {
	state.cachePersistenceAttempted = false
	state.cachePersistenceAccepted = false
	state.cacheRejectionReason = cachePersistenceRejection(state, opts)
	if state.cacheRejectionReason != CacheAccepted {
		recordTrace(ctx, TraceEvent{
			Kind: "cache_persistence", Outcome: "rejected", CacheRejectionReason: string(state.cacheRejectionReason),
			CritiqueHardRules: append([]string(nil), state.critiqueHardFailures...), CritiqueSoftRules: append([]string(nil), state.critiqueSoftWarnings...),
		})
		return
	}
	skillHash := ""
	if state.skillSet != nil {
		skillHash = state.skillSet.Hash()
	}
	state.cachePersistenceAttempted = true
	err := c.cache.Set(cacheKey, agenticCacheData{
		analysisResponse:       parsed,
		GeneratedAt:            generatedAt,
		Model:                  c.model,
		ToolCalls:              state.calls,
		ModelBytes:             state.modelBytes,
		GCSBytes:               state.gcsBytes,
		EvidencePlanCovered:    state.evidencePlanCovered(),
		GCSFloorRetryExhausted: state.gcsFloorRetryExhausted,
		BudgetExhausted:        state.budgetExhausted,
		JudgeRan:               state.judgeRan,
		JudgeObjected:          state.judgeObjected,
		JudgeRevised:           state.judgeRevised,
		JudgeResolutionKnown:   true,
		JudgeRevisionRejected:  state.judgeRevisionRejected,
		CritiquePassed:         state.critiquePassed,
		CritiqueHardFailures:   append([]string(nil), state.critiqueHardFailures...),
		CritiqueSoftWarnings:   append([]string(nil), state.critiqueSoftWarnings...),
		CritiqueVersion:        currentCritiqueVersion,
		SkillSetHash:           skillHash,
		ModelHash:              c.modelFingerprint(),
		PromptHash:             state.promptHash,
	})
	if err != nil {
		state.cacheRejectionReason = CacheRejectedNotPersisted
		recordTrace(ctx, TraceEvent{Kind: "cache_persistence", Outcome: "error", CacheRejectionReason: string(state.cacheRejectionReason)})
		return
	}
	state.cachePersistenceAccepted = true
	recordTrace(ctx, TraceEvent{
		Kind: "cache_persistence", Outcome: "accepted",
		CritiqueHardRules: append([]string(nil), state.critiqueHardFailures...), CritiqueSoftRules: append([]string(nil), state.critiqueSoftWarnings...),
	})
}

func cachePersistenceRejection(state *agentState, opts AgenticOptions) CacheRejectionReason {
	floors := evalFloors(state, opts)
	if floors.callsUnmet {
		return CacheRejectedToolFloor
	}
	if floors.gcsUnmet {
		return CacheRejectedEvidenceFloor
	}
	policyAnalysis := &models.AIAnalysis{
		Mode: AgenticMode, CritiquePassed: state.critiquePassed, CritiqueVersion: currentCritiqueVersion,
		CritiqueHardFailures: state.critiqueHardFailures, CritiqueSoftWarnings: state.critiqueSoftWarnings,
		JudgeObjected: state.judgeObjected, JudgeRevised: state.judgeRevised,
		JudgeResolutionKnown: true, JudgeRevisionRejected: state.judgeRevisionRejected,
	}
	policy := effectiveCritiqueCachePolicy(opts.CritiqueCachePolicy, opts.CritiqueMaxRetries)
	if reason := critiqueCacheRejection(policyAnalysis, policy); reason != CacheAccepted {
		return reason
	}
	return semanticCacheRejection(policyAnalysis)
}

// runFinalizeRound asks the model for one schema-constrained response containing
// just the final analysis. Used when the agent ran out of iterations or returned
// prose without parseable JSON. Returns raw content; callers handle unparseable
// responses.
func (c *Client) runFinalizeRound(ctx context.Context, messages []modelMessage, headroom contextHeadroom) (string, []json.RawMessage, bool) {
	messages = append(messages, modelMessage{Role: "user", Content: strPtr(agForceFinalizePrompt)})
	format := analysisFinalizeFormat()
	toolDefs := []tools.Schema{{
		Type: "function",
		Function: tools.FunctionDecl{
			Name: format.Name, Description: format.Description,
			Parameters: format.Schema, Strict: true,
		},
	}}
	var safe bool
	messages, safe = prepareContextRequest(ctx, messages, schemaPayloadBytes(toolDefs), headroom, "finalize")
	if !safe {
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "headroom_denied", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
		recordTrace(ctx, TraceEvent{Kind: "context_headroom", Outcome: "unavailable", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
		log.Printf("  ⚠ agentic finalize round skipped: request exceeds context headroom")
		return "", nil, false
	}
	recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "requested"})
	parallel := false
	resp, err := c.callModelRequest(ctx, modelRequest{
		Model: c.model, Messages: messages, Tools: toolDefs,
		ToolChoice: &ToolChoice{Name: format.Name}, ParallelToolCalls: &parallel,
	})
	if err != nil {
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "error", ErrorCode: "model_request_error"})
		log.Printf("  ⚠ agentic finalize round failed: %v", err)
		return "", nil, true
	}
	if !resp.HasMessage {
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "empty", ErrorCode: "missing_message"})
		return "", resp.Message.ProviderItems, true
	}
	captureToolLoopContinuation(ctx, c, appendToolsFreeAssistant(messages, resp.Message))
	if len(resp.Message.ToolCalls) == 1 && resp.Message.ToolCalls[0].Function.Name == format.Name {
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "success", Status: "forced_function"})
		return resp.Message.ToolCalls[0].Function.Arguments, resp.Message.ProviderItems, true
	}
	if resp.Message.Content != nil {
		recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "success", Status: "plain_content"})
		return *resp.Message.Content, resp.Message.ProviderItems, true
	}
	code := "nil_content"
	if len(resp.Message.ToolCalls) > 0 {
		code = "unexpected_tool_call"
	}
	recordTrace(ctx, TraceEvent{Kind: "finalize", Outcome: "empty", ErrorCode: code})
	return "", resp.Message.ProviderItems, true
}

// tryParseAnalysis extracts and unmarshals the JSON answer, returning ok=false
// if no valid JSON object could be found.
func tryParseAnalysis(s string) (analysisResponse, bool) {
	if strings.TrimSpace(s) == "" {
		return analysisResponse{}, false
	}
	var out analysisResponse
	cleaned := extractJSON(s)
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return analysisResponse{}, false
	}
	if out.RootCause == "" && out.Summary == "" {
		return analysisResponse{}, false
	}
	return out, true
}

var toolsUnsupportedRe = regexp.MustCompile(`(?i)tool[s_]?call|function[s_]?call|tools_choice|tools provided|tools?\s+(?:are\s+)?not supported|function calling`)

func isToolsUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, " 400") && !strings.Contains(msg, " 422") {
		return false
	}
	return toolsUnsupportedRe.MatchString(msg)
}

// dispatchAgenticTool routes one tool call and returns its model-bound envelope.
func dispatchAgenticTool(ctx context.Context, s *agentState, tc modelToolCall) string {
	envelope, _ := dispatchAgenticToolWithPayload(ctx, s, tc)
	return envelope
}

// dispatchAgenticToolWithPayload also returns the uncapped structured payload.
func dispatchAgenticToolWithPayload(ctx context.Context, s *agentState, tc modelToolCall) (string, map[string]interface{}) {
	s.calls++
	if !agenticToolEnabled(s.enabledTools, tc.Function.Name) {
		message := fmt.Sprintf("tool %q is not enabled for this analysis", tc.Function.Name)
		payload := map[string]interface{}{"error": message}
		recordTrace(ctx, TraceEvent{Kind: "tool_call", Tool: tc.Function.Name, Outcome: "disabled"})
		return toolErrJSON(message), payload
	}
	if s.modelRemaining() <= 0 {
		s.budgetExhausted = true
		recordTrace(ctx, TraceEvent{Kind: "tool_call", Tool: tc.Function.Name, Outcome: "model_budget_exhausted"})
		message := "model byte budget exhausted; produce final JSON now"
		payload := map[string]interface{}{"error": message}
		return toolErrJSON(message), payload
	}
	if !isRepoTool(tc.Function.Name) && s.gcsRemaining() <= 0 {
		s.budgetExhausted = true
		recordTrace(ctx, TraceEvent{Kind: "tool_call", Tool: tc.Function.Name, Outcome: "gcs_budget_exhausted"})
		message := "GCS byte budget exhausted; produce final JSON now"
		payload := map[string]interface{}{"error": message}
		return toolErrJSON(message), payload
	}

	env := &tools.Env{
		Browser:    s.browser,
		Sources:    s.sources,
		Cache:      s.cache,
		WebURLBase: s.webURLBase,
	}
	toolCtx := withEvidenceReadSource(ctx, EvidenceReadSourceModelTool)
	result := dispatchToolCall(toolCtx, s.registry, env, tc, s.modelRemaining(), s.gcsRemaining())
	if !isRepoTool(tc.Function.Name) {
		s.gcsBytes += result.BytesFetched
	}
	if result.BudgetExhausted {
		s.budgetExhausted = true
	}
	_, toolFailed := result.Payload["error"]
	toolOutcome := "success"
	if toolFailed {
		toolOutcome = "error"
	}
	recordTrace(ctx, TraceEvent{Kind: "tool_call", Tool: tc.Function.Name, Outcome: toolOutcome, Bytes: result.BytesFetched})
	envelope := toolEnvelopeJSON(s, result.Payload)
	visiblePayload := modelVisibleToolPayload(envelope)

	// Record successful artifact reads so critiqueDraft can flag prose
	// citations of files the agent never opened. Only content-fetching
	// tools count; list/find tools don't justify content claims. The
	// "error" key check prevents a failed read from silently satisfying
	// the hallucination gate.
	if isContentFetchingTool(tc.Function.Name) {
		if !toolFailed {
			if p := extractToolPathArg(tc.Function.Arguments); p != "" && visiblePayload != nil {
				s.recordSuccessfulRead(p)
				beforeLines := analysisEvidenceLineSnapshot(s.analysisEvidence, p)
				if _, roomLeft := recordAnalysisChatEvidence(s.analysisEvidence, tc, visiblePayload, s.analysisEvidenceBudget); !roomLeft {
					s.analysisEvidenceFull = true
				}
				visibleSnippets := toolResultSnippets(tc.Function.Name, visiblePayload)
				newPath := false
				if len(visibleSnippets) > 0 {
					newPath = s.recordEvidenceRead(p)
				}
				contentAdded := false
				for _, snippet := range visibleSnippets {
					contentAdded = s.recordEvidenceContent(p, snippet) || contentAdded
				}
				if contentAdded && !newPath {
					s.evidenceRevision++
				}
				s.recordAnalysisEvidenceRevisions(p, beforeLines)
			}
		}
	}
	if !toolFailed && s.sources != nil {
		emitSourceEvidenceObservations(s.sourceObserver, tc.Function.Name, result.Observation)
		if extractToolSourceID(tc.Function.Arguments) == s.sources.PrimaryID() {
			for _, repoPath := range visibleRepoReadPaths(tc, visiblePayload) {
				s.recordSourceRead(repoPath)
			}
			s.recordSourceContent(tc, visiblePayload)
		}
	}

	// Keep the ranked evidence plan alive across turns. Computed after the read
	// is recorded so a group the model just satisfied is not reported unread,
	// and dropped when it would push the envelope past the per-call budget.
	if visiblePayload != nil {
		if unread := s.unreadEvidenceGroupIDs(); len(unread) > 0 {
			result.Payload["unread_evidence_groups"] = unread
			if rebuilt := toolEnvelopeJSON(s, result.Payload); len(rebuilt) <= agenticToolBudget {
				envelope = rebuilt
			} else {
				delete(result.Payload, "unread_evidence_groups")
			}
		}
	}

	return envelope, result.Payload
}

// unreadEvidenceGroupIDs names the initial-plan groups that have a candidate
// path in this build but no substantive read yet. Bounded so the reminder
// cannot meaningfully grow a tool result.
func (s *agentState) unreadEvidenceGroupIDs() []string {
	var out []string
	for _, group := range s.planCoverage().UnmetGroups {
		if group.GroupID == "" || len(group.CandidatePaths) == 0 {
			continue
		}
		out = append(out, group.GroupID)
		if len(out) == evidenceNudgeMaxGroups {
			break
		}
	}
	return out
}

func analysisEvidenceLineSnapshot(evidence map[string]*analysisChatEvidence, rawPath string) map[int]string {
	path, err := artifacts.SafePath(strings.TrimSpace(rawPath))
	if err != nil || path == "" || evidence[path] == nil {
		return nil
	}
	out := make(map[int]string, len(evidence[path].Lines))
	for line, text := range evidence[path].Lines {
		out[line] = text
	}
	return out
}

func (s *agentState) recordAnalysisEvidenceRevisions(rawPath string, before map[int]string) {
	path, err := artifacts.SafePath(strings.TrimSpace(rawPath))
	if err != nil || path == "" || s.analysisEvidence[path] == nil {
		return
	}
	if s.analysisEvidenceRevision == nil {
		s.analysisEvidenceRevision = map[string]map[int]int{}
	}
	if s.analysisEvidenceRevision[path] == nil {
		s.analysisEvidenceRevision[path] = map[int]int{}
	}
	for line, text := range s.analysisEvidence[path].Lines {
		if previous, ok := before[line]; ok && previous == text {
			continue
		}
		s.analysisEvidenceRevision[path][line] = s.evidenceRevision
	}
}

func modelVisibleToolPayload(envelope string) map[string]interface{} {
	var payload map[string]interface{}
	if json.Unmarshal([]byte(envelope), &payload) != nil {
		return nil
	}
	return payload
}

func agenticToolEnabled(enabledTools []string, name string) bool {
	for _, enabled := range enabledTools {
		if enabled == name {
			return true
		}
	}
	return false
}

// isContentFetchingTool reports whether a tool name is one of the three
// filesystem read primitives that actually return file bytes. Listing
// tools are excluded: a directory listing doesn't justify content claims.
func isContentFetchingTool(name string) bool {
	switch name {
	case "read_artifact", "tail_artifact", "grep_artifact":
		return true
	}
	return false
}

func isRepoTool(name string) bool {
	return name == "list_repo_tree" || name == "read_repo_file" || name == "grep_repo"
}

func emitSourceEvidenceObservations(observer SourceEvidenceObserver, tool string, observation any) {
	if observer == nil {
		return
	}
	switch value := observation.(type) {
	case repotree.ReadObservation:
		observer(SourceEvidenceObservation{SourceID: value.SourceID, Tool: tool, Path: value.Path, LineStart: value.LineStart, LineEnd: value.LineEnd})
	case repotree.GrepObservation:
		for _, match := range value.Matches {
			observer(SourceEvidenceObservation{SourceID: match.SourceID, Tool: tool, Path: match.Path, LineStart: match.LineStart, LineEnd: match.LineEnd})
		}
	}
}

func visibleRepoReadPaths(tc modelToolCall, payload map[string]interface{}) []string {
	if payload == nil {
		return nil
	}
	switch tc.Function.Name {
	case "read_repo_file":
		if _, visible := payload["content"]; visible {
			if p := extractToolPathArg(tc.Function.Arguments); p != "" {
				return []string{p}
			}
		}
	case "grep_repo":
		seen := map[string]bool{}
		var out []string
		if matches, ok := payload["matches"].([]interface{}); ok {
			for _, raw := range matches {
				match, _ := raw.(map[string]interface{})
				p, _ := match["path"].(string)
				if p != "" && !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
		return out
	}
	return nil
}

// extractToolPathArg pulls the "path" field out of a content-fetching tool's
// args. Returns "" on parse error or missing field. All content-fetching tools
// use the same `{"path": "..."}` arg shape.
func extractToolSourceID(raw string) string {
	if raw == "" {
		return ""
	}
	var args struct {
		SourceID string `json:"source_id"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return ""
	}
	return strings.TrimSpace(args.SourceID)
}

func extractToolPathArg(raw string) string {
	if raw == "" {
		return ""
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return ""
	}
	return strings.TrimSpace(args.Path)
}

// toolResultSnippets extracts bounded positive evidence from filesystem reads.
// Each grep match remains a separate snippet so distant hits cannot fabricate
// regex adjacency.
func toolResultSnippets(name string, payload map[string]interface{}) []string {
	switch name {
	case "read_artifact", "tail_artifact":
		if content := flattenToolContent(payload["content"]); content != "" {
			return []string{content}
		}
	case "grep_artifact":
		var sections []string
		switch matches := payload["matches"].(type) {
		case []map[string]interface{}:
			for _, match := range matches {
				if content := flattenGrepContext(match["context"]); content != "" {
					sections = append(sections, content)
				}
			}
		case []interface{}:
			for _, raw := range matches {
				match, _ := raw.(map[string]interface{})
				if content := flattenGrepContext(match["context"]); content != "" {
					sections = append(sections, content)
				}
			}
		}
		return sections
	}
	return nil
}

var grepContextLineRE = regexp.MustCompile(`^[> ]\s*\d+:\s?(.*)$`)

func flattenGrepContext(value interface{}) string {
	switch context := value.(type) {
	case string:
		if match := grepContextLineRE.FindStringSubmatch(context); len(match) == 2 {
			if strings.TrimSpace(match[1]) == "" {
				return ""
			}
			return match[1]
		}
		if strings.TrimSpace(context) == "" {
			return ""
		}
		return context
	case []string:
		var sections []string
		for _, line := range context {
			if content := flattenGrepContext(line); content != "" {
				sections = append(sections, content)
			}
		}
		return strings.Join(sections, "\n")
	case []interface{}:
		var sections []string
		for _, item := range context {
			if content := flattenGrepContext(item); content != "" {
				sections = append(sections, content)
			}
		}
		return strings.Join(sections, "\n")
	}
	return ""
}

func flattenToolContent(value interface{}) string {
	switch content := value.(type) {
	case string:
		if strings.TrimSpace(content) == "" {
			return ""
		}
		return content
	case []string:
		var sections []string
		for _, line := range content {
			if strings.TrimSpace(line) != "" {
				sections = append(sections, line)
			}
		}
		return strings.Join(sections, "\n")
	case []interface{}:
		var sections []string
		for _, item := range content {
			if section := flattenToolContent(item); section != "" {
				sections = append(sections, section)
			}
		}
		return strings.Join(sections, "\n")
	}
	return ""
}

// recordSuccessfulRead normalizes a successfully-read path and adds it to
// both the full-path and basename indices. Silent no-op when critique is
// disabled because the maps are nil. Uses the same NormalizeArtifactCitation as
// findUnreadArtifactCitations so writer and reader stay consistent.
func (s *agentState) recordSuccessfulRead(rawPath string) {
	if s.readArtifactsFull == nil && s.readArtifactsBase == nil {
		return
	}
	_, norm := canonicalTrackedArtifactPath(rawPath)
	if norm == "" {
		return
	}
	s.readArtifactsFull[norm] = true
	s.readArtifactsBase[path.Base(norm)] = true
}

func (s *agentState) recordSourceRead(rawPath string) {
	_, norm := canonicalTrackedArtifactPath(rawPath)
	if norm != "" {
		if s.readSourceFull == nil {
			s.readSourceFull = map[string]bool{}
		}
		s.readSourceFull[norm] = true
		if s.sourceOwner != "" && s.sourceName != "" {
			s.readSourceFull[strings.ToLower(s.sourceOwner+"/"+s.sourceName+"/"+norm)] = true
			s.readSourceFull[strings.ToLower("github.com/"+s.sourceOwner+"/"+s.sourceName+"/"+norm)] = true
			s.readSourceFull[strings.ToLower(s.sourceName+"/"+norm)] = true
		}
	}
}

// recordEvidenceRead adds a successful non-empty content read to the set used
// for initial evidence-plan coverage.
func (s *agentState) recordEvidenceRead(rawPath string) bool {
	if _, norm := canonicalTrackedArtifactPath(rawPath); norm != "" {
		if s.evidenceArtifactsFull == nil {
			s.evidenceArtifactsFull = map[string]bool{}
		}
		if !s.evidenceArtifactsFull[norm] {
			s.evidenceArtifactsFull[norm] = true
			s.evidenceRevision++
			return true
		}
	}
	return false
}

func (s *agentState) recordEvidenceSnippets(rawPath string, snippets []string) {
	if len(snippets) == 0 {
		return
	}
	newPath := s.recordEvidenceRead(rawPath)
	contentAdded := false
	for _, snippet := range snippets {
		contentAdded = s.recordEvidenceContent(rawPath, snippet) || contentAdded
	}
	if contentAdded && !newPath {
		s.evidenceRevision++
	}
}

func (s *agentState) recordEvidenceContent(rawPath, content string) bool {
	norm, _ := canonicalTrackedArtifactPath(rawPath)
	if norm == "" || strings.TrimSpace(content) == "" {
		return false
	}
	if s.evidenceContentByPath == nil {
		s.evidenceContentByPath = map[string][]string{}
	}
	for _, existing := range s.evidenceContentByPath[norm] {
		if existing == content {
			return false
		}
	}
	s.evidenceContentByPath[norm] = append(s.evidenceContentByPath[norm], content)
	return true
}

func canonicalTrackedArtifactPath(rawPath string) (string, string) {
	casePath, err := artifacts.SafePath(strings.TrimSpace(rawPath))
	if err != nil || casePath == "" {
		return "", ""
	}
	return casePath, NormalizeArtifactCitation(casePath)
}

func (s *agentState) recordSourceContent(tc modelToolCall, payload map[string]interface{}) {
	if payload == nil {
		return
	}
	if s.sourceContentByPath == nil {
		s.sourceContentByPath = map[string][]string{}
	}
	add := func(rawPath, content string) {
		_, norm := canonicalTrackedArtifactPath(rawPath)
		if norm == "" || strings.TrimSpace(content) == "" {
			return
		}
		s.sourceContentByPath[norm] = append(s.sourceContentByPath[norm], content)
	}
	switch tc.Function.Name {
	case "read_repo_file":
		path := extractToolPathArg(tc.Function.Arguments)
		if content, _ := payload["content"].(string); content != "" {
			add(path, content)
		}
	case "grep_repo":
		for _, match := range analysisChatEvidenceMatches(payload["matches"]) {
			path, _ := match["path"].(string)
			add(path, flattenGrepContext(match["context"]))
		}
	}
}

func toolEnvelopeJSON(s *agentState, payload map[string]interface{}) string {
	payload["remaining_model_bytes"] = s.modelRemaining()
	payload["remaining_gcs_bytes"] = s.gcsRemaining()
	payload["elapsed_seconds"] = int(time.Since(s.startTime).Seconds())
	out, _ := json.Marshal(payload)
	return capJSON(string(out))
}

func toolErrJSON(msg string) string {
	out, _ := json.Marshal(map[string]string{"error": msg})
	return string(out)
}

// capJSON trims a tool result to agenticToolBudget so a single response can't
// blow the per-call budget. Returned as-is when within budget.
func capJSON(s string) string {
	if len(s) <= agenticToolBudget {
		return s
	}
	return s[:agenticToolBudget] + `..."truncated":true}`
}

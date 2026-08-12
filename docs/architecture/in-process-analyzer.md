# In-process failure analyzer architecture

This document describes the authoritative per-build failure analyzer used by
`prow-ai-dashboard`. It focuses on implementation ownership and control flow.
Configuration details remain in [Agentic analysis](../agentic.md),
[Project configuration](../project-configuration.md), [AI providers](../ai-providers.md),
and [diagnostic skills](../skills.md).

## Architectural role

The dashboard owns failure-analysis policy. Both the GitHub Pages workflow and
the normal Kubernetes deployment run the same Go analyzer through the fetcher
or worker. `backend/internal/analysisruntime` wires project configuration,
provider transport, tools, budgets, cache, and usage recording. The
implementation that decides what to publish is `backend/internal/ai`, exposed
through the `ai.FailureAnalyzer` interface and implemented by `ai.Service`.

The Kubernetes API server serves published data and optional interactive
features. It does not replace the fetcher or worker as the owner of scheduled
failure analysis.

The experimental Orka container placement is different from a shadow analyzer:
it runs the same dashboard-owned `FailureAnalyzer` in a separate analyzer image.
The optional Orka Agent shadow and Agent Sandbox causal critic run after an
authoritative refresh and write private comparison ledgers. They do not replace
the published result or participate in normal analysis cache acceptance.

The Agent Sandbox OpenCode analyzer is a separate disabled-by-default deployment
prototype. It is exercised manually or by explicit evaluation tests, not by the
fetcher or worker. It has no public output or normal cache authority.

## End-to-end flow

`backend/cmd/fetcher` calls `fetcher.Run` for a one-shot refresh.
`backend/cmd/worker` calls `fetcher.RunWatch` for repeated refreshes. After job
discovery, artifact loading, JUnit parsing, and aggregation,
`fetcher.analyzeFailuresWithAI` schedules each failed `models.TestCase` through
the shared `FailureAnalyzer` contract.

```mermaid
flowchart TD
    A["Fetcher or worker finds a failed test"] --> B["analysisruntime wires ai.Service"]
    B --> C["Build failure prompt and current cache policy"]
    C --> D{"Attached published result passes current floors?"}
    D -->|Yes| K["Canonical AISummary and AIAnalysis"]
    D -->|No| E{"Private cache entry passes current policy?"}
    E -->|Yes| K
    E -->|No| F["Artifact-tree seed and ranked evidence plan"]
    F --> G["Agentic provider loop"]
    G <--> H["Read-only artifact, Kubernetes, and pinned source tools"]
    G --> I["Structured draft parsing"]
    I --> J["Deterministic critique, bounded repair, semantic review, and draft selection"]
    J --> K
    K --> L["Private state: eligible cache entry, traces, and usage ledger"]
    L --> M["Separate recurring causal-group correlation"]
    M --> N["Last-known-good pattern merge"]
    N --> O["Public job and flakiness JSON"]
```

The scheduling pass and `ai.Service.analyze` both revalidate an analysis already
attached to a cached build. If it is stale, the service checks a private
policy-unavailable cooldown when applicable, then the private agentic cache.
Artifact-tree scanning, evidence-plan construction, and provider calls occur
only after those reuse paths miss.

## Prompt composition and skills

Project startup calls `analysisruntime.LoadProject`. The system prompt has a
fixed ownership order:

1. engine `ai.BasePrompt`;
2. the mandatory consumer `prompts/system.md`, under a project-specific heading;
3. engine `ai.ResponseFormatFooter`;
4. engine agentic tool guidance appended when `doAnalyzeAgentic` starts.

The per-failure user message comes from
`backend/internal/ai/modules/universal`. It contains bounded failure metadata,
the failure message and body, and starting tool suggestions. The service adds
bounded Prow job metadata. The experimental Orka container placement can also
add same-build failure-cohort context when it groups equivalent failures for
one representative Task. Normal in-process execution analyzes one test per
request. After a private cache miss, the agentic loop prepends a bounded
artifact-path seed and any ranked evidence plan.

Skills are not another free-form prompt layer. Engine profiles and consumer
`skills/*.yaml` files are loaded into one deterministic set. The Prow profile is
always selected; Kubernetes recipes are selected when Kubernetes tools are in
the effective tool configuration. The initial failure signal selects evidence
plans before the first model call. Later, draft text selects applicable recipes
for critique, evidence requirements, and bounded repair. The merged skill hash
is retained as provenance.

## Investigation tools and boundaries

The authoritative loop is function-calling only. An endpoint that rejects tools
makes analysis unavailable; there is no tools-free diagnostic fallback.
`analysisruntime.New` registers these read-only capabilities:

- `filesystem`: list, find, read, tail, grep, and timeline operations over one
  build's artifact browser;
- `k8s`: optional discovery helpers that navigate Kubernetes-shaped artifact
  trees without contacting a live cluster;
- `repotree`: read-only list, read, and grep operations added automatically only
  when the build resolves to an immutable source repository revision.

No authoritative analysis tool can execute a shell command, modify artifacts,
patch source, call GitHub write APIs, or change a cluster.

The main boundaries are layered rather than represented by one counter:

| Boundary | Enforcement |
| --- | --- |
| Tools | Only enabled registry schemas are sent. `single_tool_call` can limit one call per turn. |
| Iterations | `max_iters` bounds normal provider turns; bounded repair and finalization receive only guarded extra turns. |
| Time | The per-failure timeout is capped by the parent fetch context. Repair also requires explicit time headroom. |
| Artifact bytes | A fixed engine GCS ceiling limits bytes fetched by artifact and Kubernetes tools. |
| Model-visible bytes | A context-derived model-byte budget and per-tool result caps bound inserted evidence. |
| Context | The runtime prefers an operator context-window override, then provider metadata, then a bounded fallback. A conservative one-byte-per-token estimate reserves completion and finalization headroom. |

Before each provider request, old tool results may be compacted into bounded
stubs while recent evidence remains verbatim. If the request still cannot fit,
the loop publishes the best previously selected draft when one exists or marks
the analysis unavailable.

## Evidence and citations

The loop takes one bounded artifact-tree snapshot for the path seed and ranked
skill evidence plan. A plan group is covered only by a successful, non-empty
content read that matches the required path and any same-artifact content
predicates. Complete plan coverage can satisfy the evidence-byte floor without
requiring arbitrary extra reads.

Only content-bearing `read_artifact`, `tail_artifact`, and `grep_artifact`
results count as artifact evidence. Directory listings, failed calls, empty
results, and guessed paths do not. Source reads are tracked separately so
source-file claims cannot be justified by artifact reads.

The loop retains a bounded line ledger from the exact model-visible artifact
payloads. `evidence_citations` are validated against that ledger for safe path,
line range, and exact quote. Deterministic critique also rejects unread artifact
or source citations. Before publication, invalid citations are removed and
unsupported line-number claims are stripped. The public citation list therefore
reflects evidence actually returned to the model, not paths merely present in
the artifact tree.

`min_tool_calls` and `min_gcs_bytes` are cacheability floors. If a model tries to
finish below a floor, the loop can nudge it to continue investigating. A final
result may still be publishable under the configured critique policy while
remaining ineligible for cache persistence.

## Draft lifecycle and selection

Model output is never accepted directly. The current lifecycle is:

1. A tools-free response is parsed into the `analysisResponse` JSON shape. If
   the loop exhausts iterations or the response does not parse, a no-tools
   finalize round requests JSON only.
2. Deterministic critique checks citation integrity, unread paths, unsafe or
   unverified source paths, unsupported line claims, persistent-failure versus
   transient conflicts, remediation punts, and recipe-required evidence.
3. When permitted by retry, time, context, and evidence budgets, critique can
   inject bounded missing evidence, allow another tool turn, and request a
   revised structured draft.
4. A focused semantic judge reviews a deterministically acceptable draft for a
   fluent but unsupported causal conclusion. Findings can drive one bounded
   refinalization, followed by a separate review of the proposed revision.
5. Deterministic draft selection keeps the best parseable candidate. A
   replacement cannot introduce a hard-rule regression. Semantic revisions
   must also avoid dropping supported causal facts unless the judge established
   a valid cause-replacement condition.

Critique rules are classified as hard failures or soft warnings. Hard findings
cover structural, citation, source-grounding, and contradiction failures. Soft
findings cover evidence availability and remediation-quality warnings. The
configured cache policy determines which classifications block reuse:
`strict` accepts almost no warnings, `hard` blocks hard findings, and `advisory`
records findings without making critique itself a cache barrier. Semantic review
is separate; an unresolved semantic objection prevents cache acceptance.

If finalization fails, the loop prefers an earlier parseable draft. Only when no
parseable draft exists does it synthesize a bounded fallback, which is not
cached. Non-advisory citation policy can instead make the result unavailable.

## Cache and current-floor revalidation

The private cache key scopes the universal module, optional cache-generation
fingerprint, job, build, and a short hash of the test name plus normalized
failure message. Build scope is required because analyses cite build-specific
paths and lines.

Cache data records the structured result plus investigation counters, critique
and semantic state, and model, prompt, and skill fingerprints. The model hash is
a one-way fingerprint of provider API mode, endpoint, model, and reasoning
effort. These fingerprints are provenance, not automatic invalidation selectors.
A model, endpoint, prompt, skill, or failure-streak change affects new results
but does not by itself reject an otherwise current entry.

Every attached or private entry is reconstructed and checked against the current
contract. Reuse requires a valid, unexpired agentic result that meets current
tool and evidence floors, the current critique version and configured critique
policy, a resolved semantic-review state, and the current cache generation. A
rejection is treated as a miss without deleting all cache state, so a later run
can reuse entries under a compatible configuration.

Normal floor changes and critique-version changes do not need a generation
change because current-floor revalidation handles them. Prompt, model, endpoint,
and skill changes also do not require one, but they do not force reanalysis. Set
`ai.cache_generation` only when an operator intentionally wants a reversible
full rebaseline that existing acceptance gates would otherwise allow.

## Publication and private state

Public job JSON contains the failure summary, root cause, severity, suggested
fix, grounded evidence citations, source links, and intentionally exposed
per-analysis telemetry such as tool calls, context and artifact bytes, elapsed
time, cache or same-failure reuse, budget state, critique and semantic-judge
status, and provenance fingerprints. The provider model name is not serialized.
The in-process path records provider-reported token and cost details in private
traces and usage ledgers rather than copying those totals into the public
analysis object.

After the separate pattern pass, `patterns.MergeLastGood` classifies each job's
refresh as current, retained, failed or unavailable, or not applicable. A failed
eligible refresh retains a prior valid pattern when one exists and otherwise
publishes no fabricated fallback. The resulting causal-group projections are
published in job details and aggregated into `flakiness.json`.

Raw provider exchanges are transient and are not included in traces or public
output. Private persisted state includes `ai_cache.json`, `ai_traces.json`,
fetcher and server usage ledgers, fetch status, side-effect state, Agent Sandbox
comparison ledgers, and remediation investigation caches. Pages strips private
files before deployment, and the Kubernetes server rejects them from the
data-serving path.

## Downstream and separate operations

- **Recurring causal-group correlation** is owned by
  `backend/internal/patterns` with model contracts in
  `backend/internal/ai/pattern.go`, `backend/internal/ai/pattern_repo.go`, and
  `backend/internal/ai/pattern_verification.go`. It consumes representative
  failed tests with an attached `AIAnalysis`, groups evidence across builds, and
  publishes analysis-only recurrence projections. The attached analysis may be
  newly accepted or a prior real result preserved when reanalysis is
  unavailable. Correlation does not rewrite the per-build diagnosis, and
  `patterns.MergeLastGood` isolates per-job correlation failures before public
  output.
- **Analysis chat** resolves a published test or pattern into a bounded private
  session under `backend/internal/analysischat`. Chat replies do not change job
  JSON. A separately enabled, explicitly confirmed correction workflow can
  publish an overlay without mutating fetcher output.
- **Remediation investigation** consumes a frozen active causal group, its
  referenced analyses and artifacts, and pinned source. The model produces
  bounded target hypotheses, not authoritative proposals. Dashboard code
  validates selected evidence, converts each hypothesis to a typed target,
  applies `actionverify` and `remediationpolicy` to engine-derived behavior, and
  verifies current and failure-revision source state through
  `sourceinvestigation`. Only then can the engine derive a private verified
  proposal and a safe public target summary. Model-authored relationship prose
  is non-authoritative. This flow uses a separate private cache and does not
  alter per-build analysis.
- **Actions** are authenticated server operations over current published
  subjects with independent lifecycle and quality gates. File Issue and Fix PR
  use preview-confirm workflows. Resolve and unresolve are direct lifecycle
  state changes for action-eligible subjects. Asynchronous action requests have
  their own request, confirmation, cancellation, and result state. Causal-group
  patterns remain categorically analysis-only through
  `models.PatternAllowsActions`, even after a remediation investigation reaches
  the public `actionable` state.
- **Fix PR generation** consumes an eligible action subject and verified source
  through `backend/internal/fixpr` and `backend/internal/fixruntime`. Its coding
  agent, review, validation, and PR state are independent of failure-analysis
  tools and cache acceptance.
- **Scheduled analysis shadows** use `agentanalysis.Runtime` from
  `backend/internal/agentanalysis/runtime.go` and
  `backend/internal/fetcher/shadow_analysis.go`, plus the separate
  `backend/internal/causalcritic` path. They run after an authoritative refresh,
  freeze bounded evidence, compare private results with the authoritative
  snapshot, and write private ledgers. They cannot publish a replacement or
  seed the normal cache.
- **Agent Sandbox OpenCode analyzer** uses the workspace contracts under
  `backend/internal/agentanalysis`, the read-only workspace stager in
  `backend/internal/analysisstager`, and the executor in
  `backend/internal/analysisexecutor`. It validates sealed source and artifact
  workspaces, evidence handles, and one canonical result while retaining only
  private content-free telemetry. The Helm option installs its security
  boundary, but the fetcher and worker do not schedule analyzer workloads.

## Contributor map

| Change | Start here |
| --- | --- |
| Fetcher entry and runtime wiring | `backend/internal/fetcher/analysis.go`, `backend/internal/analysisruntime/runtime.go` |
| Per-failure contract | `backend/internal/ai/runner.go`, `backend/internal/ai/service.go` |
| Provider wire format | `backend/internal/ai/transport.go`, `backend/internal/ai/transport_chat.go`, `backend/internal/ai/transport_responses.go` |
| Authoritative provider loop | `backend/internal/ai/agentic.go`, `backend/internal/ai/tools/` |
| Generic downstream tool loop | `backend/internal/ai/toolloop.go` |
| Prompt composition | `backend/internal/ai/compose.go`, `backend/internal/ai/baseprompt.go`, `backend/internal/ai/responseformat.go`, `backend/internal/ai/modules/universal/` |
| Evidence planning and skill coverage | `backend/internal/ai/evidenceplan/`, `backend/internal/ai/skills/`, `backend/internal/ai/agentic.go` |
| Deterministic critique | `backend/internal/ai/critique.go`, `backend/internal/ai/critique_rules.go` |
| Semantic review and draft selection | `backend/internal/ai/semantic.go`, draft-selection code in `backend/internal/ai/agentic.go` |
| Cache identity and acceptance | `backend/internal/ai/service.go`, `backend/internal/ai/cache.go`, `backend/internal/ai/cache_acceptance.go` |
| Public and private output boundary | `backend/internal/models/models.go`, `backend/internal/output/`, `backend/internal/ai/trace.go`, `backend/internal/ai/trace_store.go`, `backend/internal/aiusage/` |
| Recurring patterns | `backend/internal/patterns/`, `backend/internal/ai/pattern.go`, `backend/internal/ai/pattern_repo.go`, `backend/internal/ai/pattern_verification.go` |
| Analysis chat and actions | `backend/internal/analysischat/`, `backend/internal/actions/` |
| Remediation investigation authority | `backend/internal/remediationinvestigation/`, `backend/internal/remediationpolicy/`, `backend/internal/actionverify/`, `backend/internal/sourceinvestigation/` |
| Fix PR runtime | `backend/internal/fixpr/`, `backend/internal/fixruntime/` |
| Scheduled analysis shadows | `backend/internal/agentanalysis/runtime.go`, `backend/internal/fetcher/shadow_analysis.go`, `backend/internal/causalcritic/` |
| Agent Sandbox analyzer prototype | `backend/internal/agentanalysis/workspace_analysis.go`, `backend/internal/agentanalysis/workspace_evidence_handles.go`, `backend/internal/agentanalysis/workspace_result_validation.go`, `backend/internal/analysisstager/`, `backend/internal/analysisexecutor/` |

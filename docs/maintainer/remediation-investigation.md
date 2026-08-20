# Causal-group remediation investigation

> **Status: authenticated preview, maintainer reference.** The read-only
> investigator, minimal candidate contract, private evidence ledger and cache,
> deterministic verifier, frozen benchmark, authenticated operation, and safe
> public lifecycle exist. File Issue eligibility and Fix PR handoff are not
> enabled. User-facing setup remains in the server and Fix PR guides.

Version-10 causal-group correlation remains analysis-only. It does not emit a
suggested fix, remediation target, source target, or action field. A separate
operation investigates one exact repeated causal group only after an explicit
request.

## Frozen input

`backend/internal/remediationinvestigation` owns the private input. It binds:

- pattern ID and published pattern content hash;
- causal-group ID and causal-group content hash;
- job ID, recurrence classification, exact group, and exact build IDs;
- referenced per-build analyses and their artifact citations;
- relevant files as evidence hints only;
- source revisions available for each build;
- one immutable repository revision for bounded source verification;
- project destination repositories, allowed paths, and exact validation commands as argv plus timeout;
- consumer prompt and skill hashes;
- provider/model fingerprint; and
- prompt, schema, and verification versions.

The cache key hashes the complete canonical frozen input. Reordering set-like
build, analysis, file, repository, path, or command lists does not change the
key. Changing causal content, source revision, provider, prompt, skills, policy,
or a contract version produces a different key. The global analysis cache
generation is not used or changed.

## Read-only model flow

The trusted in-process model client runs two bounded phases:

1. **Evidence phase.** Only existing artifact filesystem and pinned-source
   `repotree` tools are available. Artifact access is bound to the exact causal
   builds. Source access is bound to one immutable repository revision. A
   successful content-bearing artifact read, source-file read, and repository
   grep are required. The generic tool loop first requires one content-bearing
   `read_repo_file`, then one content-bearing `grep_repo`. The model authors both
   calls and the grep query. If either is missing, the next request forces that
   exact function and asks the model to search exact identifiers from failure
   evidence and source, such as job names, environment names, symbols, calls,
   and configuration values. A zero-match grep does not satisfy the requirement.
   When no frozen relevant-file hint resolves, the loop first forces one bounded
   `list_repo_tree`. Forced turns disable parallel tool calls and remain inside
   the same bounded conversation and iteration budget. Successful grep matches
   become supplemental private source evidence, but never replace the mandatory
   file read.
2. **Target extraction.** Read tools are removed, but the bounded evidence
   conversation is retained in one opaque, single-use in-memory continuation.
   The engine appends the frozen identity, current engine-issued evidence
   catalog, and exact target-extraction contract to the same message history.
   The response-format, forced-function, and plain-fallback attempts therefore
   retain prior source and artifact tool results even when the diagnostic memo
   omits a useful field. The model returns version 1 with zero to three typed
   target hypotheses. Each hypothesis contains one
   target identity, selected engine-issued evidence IDs, and a concise
   relationship reason. A hypothesis is a verification subject, including when
   the target appears already present. The bounded relationship reason remains
   private and may produce a content-free quality warning, but it never
   authorizes or blocks the typed target. It contains no lifecycle, repository,
   revision, policy, command, source-state, or action fields.
3. **Deterministic hypothesis verification.** Dashboard code validates and
   independently verifies every hypothesis. Exactly one verified identity may
   produce `actionable` or `already_fixed`. No verified identity remains
   `insufficient_evidence`; multiple distinct verified identities become
   `ambiguous` and cannot enable an action.
4. **Non-actionable assessment.** Only when no hypothesis verifies, a separate
   structured model call returns a cause assessment, one typed non-actionable
   reason, a concise safe explanation, and evidence IDs. This stage cannot
   introduce a target.

The continuation is bound to the model client, expires after the operation
budget, and can be consumed only once. It rejects JSON or text serialization and
clears its message history when consumed or discarded. It is never written to a
cache, trace, public response, or benchmark row. Context compaction remains
active for the continued structured request and preserves tool-call wiring and
provider continuation items.

If either required source tool cannot complete, the generic loop returns a
typed, content-free failure and target extraction does not run. The evidence
ledger also keeps the source and artifact floors as defense in depth. No
workspace, shell, branch, issue, pull request, or write tool is available. The
experimental Agent Sandbox analyzer is not used. Agent Sandbox/OpenCode remains
the later patch-generation stage after deterministic target verification.

## Two-stage model contracts

Target extraction version 1 contains exactly `version` and `hypotheses`. Each
hypothesis contains `target`, `evidence_ids`, and `relationship_reason`. At most
three hypotheses are accepted. The model-visible target variants are limited to
required calls and Prow environment entries, the two kinds with deterministic
present-or-missing verification.

Published symbol-addition and general-configuration candidate and target
constants, concrete Go types, JSON decoding, validation, and cache payloads
remain so existing private diagnostic entries still decode safely. They are not
offered to the model. If encountered in a persisted response, the deterministic
verifier rejects them as `insufficient_evidence`.

Non-actionable assessment version 1 contains exactly `version`,
`cause_assessment`, `reason`, `evidence_ids`, and `non_actionable_reason`. The
reason is one of environment or infrastructure, mitigation only, insufficient
evidence, or unverified dependency ownership. It has no target field.

The private engine result is version 4 and combines the extracted hypotheses
with an optional non-actionable assessment. The model never authors final
lifecycle classification or action eligibility.

## Engine-issued evidence ledger

The model no longer authors evidence paths, source revisions, line numbers,
quotes, build IDs, or timestamps. Dashboard code reconstructs successful tool
reads into a private versioned catalog:

- file-read source evidence binds repository, revision, path, and content digest;
- source-grep evidence binds repository, revision, safe path, canonical line
  range, full-file content digest, and one bounded engine-reconstructed match;
  at most 64 matches and 64 KiB of reconstructed grep text enter one catalog;
- analysis evidence binds build ID, analysis timestamp, and root-cause digest;
- artifact evidence binds build ID, artifact path, and content digest.

Each record receives a deterministic ID over its complete identity. The final
model response may cite only those IDs. Before caching and again during
deterministic verification, dashboard code resolves every selected ID, rereads
source and artifacts, rechecks digests, reconstructs selected grep ranges from
the immutable source, and matches analysis identities to the frozen input.
Unknown, duplicate, or mutated IDs fail closed.

The exact candidate path must have a selected source evidence ID. Every causal
build must have selected analysis or artifact evidence before a terminal result
can pass deterministic verification.

## Deterministic verification

Verification version 5 rechecks one accepted private cache entry before it can
be considered actionable. The verifier binds the model-result digest, evidence
catalog digest, and full provenance to the current frozen input, then
independently:

- reconstructs every selected evidence ID and requires coverage for every exact
  causal-group build;
- requires an exact engine-issued source identity for the candidate path;
- treats relevant files only as an additional relationship hint, never as target
  proof;
- derives the destination repository and immutable revision from the frozen
  source and destination policy;
- derives allowed changed paths and validation commands from project policy;
- converts the candidate variant to an existing typed
  `models.RemediationTarget`;
- runs existing `actionverify` target verification at current source and every
  available failure revision;
- proves required calls are actually missing and rejects already-present calls;
- resolves Prow job, container, environment name, and desired value uniquely;
- requires selected source evidence for the exact Prow environment value;
- keeps repository-local call resolution within module boundaries;
- applies conversion and destructive-remediation policy to the typed target and
  engine-derived expected behavior;
- records suspicious model-authored relationship prose only as a content-free,
  non-authoritative warning; and
- rejects mutated, duplicate, unknown, fabricated, unlinked, ambiguous,
  workspace, module-cache, or wrong-repository targets.

The currently actionable deterministic target kinds remain limited to a
`modify_symbol` target with an exact `required_call` and exact Prow job
environment changes. Legacy package-symbol additions and general configuration
fields remain `insufficient_evidence` until they have deterministic
behavioral-role and field-state predicates. Textual mention plus source absence
is not sufficient proof for a new symbol. Model-authored relationship prose
cannot make an unsafe typed target safe or block an otherwise verified safe
target.

The engine derives the terminal classification:

- a target already present in current source becomes `already_fixed` with no
  proposal;
- a target not proven unresolved at every failure revision becomes
  `insufficient_evidence`;
- only a target proven unresolved in current source and all failure sources
  becomes `actionable`;
- multiple distinct verified hypotheses become `ambiguous` with no proposal;
- environment and mitigation reasons retain their typed non-actionable
  classifications; and
- dependency ownership that cannot be independently verified remains
  `insufficient_evidence` rather than claiming an external repository owner.

`external_dependency` remains reserved for a future typed dependency identity
that dashboard code can independently verify. A prose or module-cache path alone
cannot produce that classification.

Only the private verified proposal contains repository, revision, target,
expected behavior, selected evidence IDs, verification requirements, allowed
changed paths, and validation commands. The verifier does not publish a public
state or grant Fix PR eligibility.

## Private cache

The cache lives at:

```text
<data-dir>/.remediation-investigations/cache.json
```

The directory is `0700`, the cache and lock files are `0600`, and writes use a
cross-process file lock plus durable atomic replacement. Cache version 3 binds
both the minimal model-result digest and the engine-issued evidence-catalog
digest. Corrupt, oversized, or mutated current-version state fails closed.
Entries from an older prompt, verification, or evidence-catalog version are
dropped as semantically stale instead of blocking cache startup. A failed
refresh records only a bounded category, remediation phase, validation code,
structured attempt path and outcome, validator-called flag, safe provider
category and status, timestamp, and digest while preserving the previous valid
result for the same semantic key. Attempt paths are `response_format`,
`forced_function`, and `plain_fallback`. No prompt, response text, function
arguments, provider body, header, evidence ID, or target identity is retained. A
changed identity creates a cache miss instead of reusing the old result.

The server hides dot-directories under `/data`. The Pages workflow also strips
the remediation-investigation cache before upload. Cached entries contain the typed private result, deterministic evidence
identities, content-free provenance, evidence counters, usage totals, and
latency. A source-grep identity may contain one bounded, verifier-reconstructed
match. The private cache does not contain credentials, endpoints, raw prompts,
raw model responses, transcripts, unrestricted source bundles, or raw tool
payloads. No evidence-catalog contents are published in public causal data.

## Recurrence memory

The private cache above is keyed by the frozen input, which includes the causal
group's current member builds. When those builds age out of the fetch window the
group is regrouped with a new identity, so the same cause returns as a cache miss
and the same question is re-investigated at full model cost.

`backend/internal/recurrenceledger` closes that gap. It records recurrence history
keyed by a durable signature that survives both regrouping and rewording:

```text
<data-dir>/recurrence_ledger.json
```

### Signature

`patterns.CausalGroupSignature` hashes the job plus the dominant
`(suite, class, test, failure-location file, normalized message)` tuple among the
group's builds. Normalization is `aggregator.NormalizeErrorSignature`, which
strips timestamps but **preserves numbers**, unlike the `NormalizeErrorMessage`
the flakiness report uses: a status or exit code is often the only thing
separating causes that need different answers, and collapsing `status 401` and
`status 503` would let a conclusion about one apply to the other.

No signature is derived, and therefore no memory is kept, when the failure has no
message, when it is a build-level stand-in (`models.NewProwJobExecutionFailure`
has a constant name and message), when no member build is still in the window, or
when two causal groups on the same job resolve to the same signature. That last
case means the signature does not identify a single cause, and both groups lose
it: another investigation is cheap, a wrong answer is not.

That collision check is **pass-local**. It sees only the groups present in one
pass, so a signature that collided in an earlier pass and no longer does can carry
a verdict recorded before the collision. Reuse being capped at three bounds what
that costs.

A group whose builds have all aged out keeps the signature it was published with,
so a cause that returns is still recognized.

The signature is published as an inert `signature` field on each causal group in
`jobs/*.json` and `flakiness.json`, and is excluded from `PatternCausalGroupHash`
and `PatternHash` so it never churns published identity.

### Ledger

Each entry records first seen, last seen, a count of distinct failing builds, a
build-id watermark, and the most recent terminal verdict. Only builds strictly
past the watermark advance recurrence, so a retained pattern re-observed every
pass does not inflate its history. Counting relies on Prow build ids being
increasing integers; opaque ids are recorded once and never advance the count.

Retention runs before each pass observes, so a cause returning after 180 quiet
days starts fresh rather than reviving a verdict too old to trust, and the file is
capped at `MaxEntries`, evicting least-recently-seen causes.

The fetcher records sightings inside the publication lock after output is written;
the server records verdicts when an investigation finishes. Both mutate through
`recurrenceledger.Update`, which holds a dedicated lock file rather than the
pattern publication lock, so it is safe to call while publication is in progress.

### Verdict reuse

An exact frozen-input cache entry is a precise match for the request and always
wins, so recurrence memory is consulted only when that cache misses, which is the
case the ledger exists for. A recorded verdict for the same signature then answers
the request without launching a run:

| Verdict | Reused | Why |
| --- | --- | --- |
| `environment_or_infrastructure` | yes | A conclusion about the nature of the cause. |
| `external_dependency` | yes | A conclusion about the nature of the cause. |
| `mitigation_only` | yes | A conclusion about the nature of the cause. |
| `actionable` | no | The target is pinned to a source revision and must be re-verified. |
| `already_fixed` | no | The recurrence is itself evidence it is not fixed. |
| `insufficient_evidence` | no | Describes what was available at the time; a later recurrence may carry better artifacts. |
| in-flight, `failed`, `stale` | no | Not a conclusion. |

Reuse is capped at `MaxVerdictReuses` (3) per verdict. That is a damage bound, not
a solution to symptom-versus-cause aliasing: no artifact-derived identity can
guarantee that an identical symptom has an identical cause, so a wrong reuse costs
at most three suppressed investigations, and a conclusion is periodically
re-checked. Only a freshly computed, verified investigation resets the budget; a
cache hit or a recovered previous result is a republication, not new work. The
claim and the charge happen together under the ledger lock.

Other guards:

- An `already_fixed` verdict is discarded as soon as a build past the watermark
  arrives.
- An explicit refresh always forces a fresh investigation.
- A reused answer is re-validated against the current published subject, and
  carries the completion time it was originally reached at.
- A verdict that completed before the one on record is refused, so a run that
  finishes late with an older conclusion does not displace a newer one.
- Reuse refuses memory past the retention window on read, independently of whether
  pruning has run.
- A ledger write failure is logged without failing the operation.

The file is private: it is in `output.NonPublishedFiles`, so the server refuses to
serve it and the Pages workflow strips it before upload.

The ledger informs analysis; it does not change published output. Surfacing
recurrence counts in the dashboard, and re-scoping resolution to this durable
identity, are separate changes.

## Private benchmark diagnostics

The temporal capability runner may compute scorer-private, content-free signals
for whether the evidence memo mentioned the expected job, container, environment
name, and value, and whether final structured output contained a candidate. The
runner stores only those booleans. It does not retain the memo or raw model
output. Diagnostic case and repetition filters do not relax the default
comparison scorer, which still requires the complete frozen two-model result
set. The explicit `--model gpt-5.4` scorer mode requires exactly the six frozen GPT-5.4
temporal trials and does not accept partial or replacement results.

## Frozen benchmark

The committed manifest is:

```text
backend/benchmarks/testdata/benchmarks/remediation-investigation-v3.json
SHA-256: 84620efb7e127207d6891bdfaa8614cb9454213d0f1155d32b60a2cc97dace72
```

It freezes 12 categories:

1. actionable missing call;
2. actionable missing Prow environment configuration;
3. target already present at the pinned revision;
4. fixed in current source;
5. external dependency evidence without verified ownership;
6. environment or infrastructure;
7. mitigation only;
8. insufficient or ambiguous evidence;
9. unsafe conversion-webhook proposal;
10. wrong module-cache or dependency repository;
11. duplicated or unknown target; and
12. legacy fabricated symbol or configuration field.

The external-dependency and wrong-repository cases expect
`insufficient_evidence` until a typed ownership identity can be independently
verified.

Hermetic tests validate the manifest, hashes, exact category coverage, frozen
Prow job identity, two distinct positive target kinds, current model-visible and
legacy-compatible discriminated candidate contracts, evidence ID
reconstruction, cache preservation, read floors, deterministic source-state
checks, and scoring controls.

Provider-backed evaluation must use the final merged prompt, schema, and exact
intended provider. Prompt version 4 requires a content-bearing pinned source
read through the generic required-tool mechanism. `evidence_retry_count`
records forced required-tool dispatches separately from structured repair count.
The earlier 0/12 run against result version 2 is evidence that the old model
contract was unsuitable. The later prompt-version-3 Claude Stage A run proved
that an automatic tool choice plus a prompt-only retry did not produce source
reads. Neither result is readiness evidence for version 4, and neither is
evidence that source-grounded remediation is infeasible.

Run the provider gate in stages and stop at the first failed stage.

### Stage A: exact-provider source gate

Run these three cases with two cold repetitions each using the exact intended
provider configuration:

```bash
RUN_REMEDIATION_INVESTIGATION_BENCHMARK=1 \
REMEDIATION_BENCH_CASES=actionable-missing-call,fixed-in-current-source,environment-or-infrastructure \
REMEDIATION_BENCH_REPETITIONS=2 \
REMEDIATION_BENCH_RESULTS_JSONL=/private/path/stage-a.jsonl \
AI_ENDPOINT=<configured-endpoint> \
AI_MODEL=<configured-model> \
go test ./benchmarks -run '^TestRemediationInvestigationBenchmark$' -v -timeout 60m
```

Require all six trials to complete a successful content-bearing source read,
zero unsafe acceptances, no wrong-repository or fabricated target, and at least
one exact verified positive before proceeding. If all source reads succeed but
no useful candidate is produced, stop and report a reasoning-quality limitation.

### Stage B: full matrix

Only after Stage A passes, run all 12 cases with three cold repetitions:

```bash
RUN_REMEDIATION_INVESTIGATION_BENCHMARK=1 \
REMEDIATION_BENCH_REPETITIONS=3 \
REMEDIATION_BENCH_RESULTS_JSONL=/private/path/stage-b.jsonl \
AI_ENDPOINT=<configured-endpoint> \
AI_MODEL=<configured-model> \
go test ./benchmarks -run '^TestRemediationInvestigationBenchmark$' -v -timeout 120m
```

Require 100% actionable precision, zero unsafe or wrong-repository acceptance,
and at least two distinct positive target kinds before continuing to the frozen
real historical holdouts.

### Stage C: real historical holdouts

The committed manifest is:

```text
backend/benchmarks/testdata/benchmarks/remediation-investigation-history-v1.json
SHA-256: 426ecf50a6f7773c55463eb6f920af4eef153d4c70facbd5234b57c0b199a094
```

It freezes the recurring CAPZ ASO-upgrade failure at its initial and merged
repair revisions, one upstream DRA dependency failure, and the Azure Linux 3
control-plane join causal group reconstructed from the deployed safe snapshot.
Each case binds public build IDs, immutable source revisions, public artifact
identities and hashes, bounded artifact excerpts, source snapshot hashes, and a
private scorer expectation. Expected outcomes and known-fix scorer metadata are
not included in provider prompts. Published pattern IDs, pattern hashes,
analysis timestamps, test names, and causal content come from the frozen safe
dashboard snapshot; group IDs are reconstructed because that snapshot predates
per-group publication.

Run the four cases with two cold repetitions each:

```bash
RUN_REMEDIATION_INVESTIGATION_HISTORY_BENCHMARK=1 \
REMEDIATION_HISTORY_RESULTS_JSONL=/private/path/stage-c.jsonl \
AI_ENDPOINT=<configured-endpoint> \
AI_MODEL=<configured-model> \
go test ./benchmarks -run '^TestRemediationInvestigationHistoricalBenchmark$' -v -timeout 120m
```

Require zero unsafe acceptance, no wrong-repository actionable target, every
already-fixed result blocked from patch generation, and a content-bearing source
read in every valid trial. Report `known_fix_recall` separately and do not count
an unrelated candidate as an exact historical target.

Set `AI_API` only when the intended provider mode is not the client default.
`AI_TOKEN` is read by the client but is never printed, persisted, compared, or
hashed. Summarize each private JSONL with:

```bash
python3 hack/summarize-remediation-investigation-benchmark.py \
  /private/path/stage-a.jsonl
```

The report separates structural validity, engine-derived classification
confusion, model candidate and non-actionable-reason distributions, actionable
precision and recall, exact-target accuracy, unsafe false acceptances,
already-fixed blocking, verification status, invalid/no-result/runtime trials,
requests, tokens, cost coverage, latency, and repair counts.

## Current boundary

Authenticated server deployments may explicitly enable the preview-only
**Investigate possible fix** operation. It validates the exact current pattern,
causal group, lifecycle, builds, source revisions, provider identity, and
destination policy before running the trusted read-only investigator. Only safe
status, a fixed concise reason, optional verified target identity, completion
time, and causal-group hash are returned.

Private evidence remains in the remediation cache. The operation does not modify
published causal analysis, does not run during fetch/watch refresh, and does not
automatically investigate any group. Causal-group patterns remain blocked by
`models.PatternAllowsActions`, so File Issue and Fix PR preview remain disabled.

The later Fix PR handoff must consume only `VerifiedResult.Proposal`. OpenCode
must receive the frozen verified target and cannot repair, replace, or rediscover
an unverified target. That handoff remains deferred until the Claude and real
historical gates pass and the exact final Fix executor passes a separate
single-use smoke.

Because the operation is per causal group, the dashboard renders remediation
state inside each causal group card rather than once per pattern. The UI reports
why an operation cannot start instead of showing a pending-looking default.
Conditions are evaluated from the most permanent to the most deployment-scoped:

- Causal group without an assigned id and content hash: **Not addressable**,
  since no operation reference can be constructed.
- Causal group inside a pattern the engine did not classify as recurring and
  systemic: **Not eligible**, matching the pattern-level gate in `resolver.go`.
  The cause itself may cover any number of builds, including one.
- Capability disabled or unauthenticated deploy: **Unavailable on this
  deployment**, with no control. Only terminal verdicts survive the capability
  being off, so a real earlier result is still shown while a state that could
  only advance through the operation is not left looking pending.

The first two are verdicts about one cause; the third is a property of the whole
deployment. `CausalRemediationBlockedReason.scope` carries that distinction, and
the UI renders a deployment-scoped block as a filled chip with its own icon so it
cannot be read as a verdict about the cause it sits in.

Separately, and only where a chat deploy advertises both `analysis_chat` and
`junit_chat_fix`, each causal group card offers an action that opens the
representative test's detail page, where its analysis chat can start a fix
proposal. The action targets the representative analyzed failure the
causal group was built from, and only when that failure meets every Fix
eligibility requirement and is the occurrence the test detail page will open. Its
visible label names that test and build, so several causes on one briefing stay
tellable apart. A single-build cause reaches Fix this way rather than through
remediation. When no cause has a reachable target, one pattern-level fallback
panel explains that instead.

## Experimental causal Fix PR preview

The v0.9 beta may separately enable `server.remediationInvestigation.fixPreview.enabled` after authenticated remediation investigation and an Agent Sandbox fix runtime are configured. The gate is disabled by default and advertises `causal_remediation_fix_preview` only when the server has authentication, remediation re-verification, and a fix runtime.

A preview request is explicitly initiated for one exact pattern and causal-group hash. The server reloads the private cache entry, rechecks provenance and immutable source identity, and requires exactly one current `actionable` verified proposal. Already-fixed, ambiguous, insufficient-evidence, failed, inactive, stale, unsupported, or drifted results fail closed.

The coding runtime receives only the immutable repository revision, engine-derived typed target, expected behavior, selected private evidence, allowed paths, exact validation commands, and output bounds. The repository clone uses no GitHub write token. The Agent Sandbox executor runs every target-controlled validator and the final staged diff check. The server independently reapplies the returned diff, reconstructs changed files, compares the reported file map, rejects unexpected paths and unsupported Git changes, verifies the typed target postcondition, and validates the complete ordered executor command results without rerunning target code.

The response is a human-visible preview containing a summary, base revision, safe target identity, changed paths, canonical unified diff, validation outcomes, and a safe runtime identity. It contains no confirmation token, branch, push state, PR URL, private cache key, raw model output, or raw private evidence. It is not persisted as an action request and no existing confirmation endpoint accepts it. Causal groups remain blocked by `PatternAllowsActions`; this beta uses a separate preview-only eligibility path and does not enable normal actions or chat-to-fix.

Preview generation is nondeterministic and may safely end with generation failure, patch rejection, validation failure, stale target, or no actionable target. No GitHub PR is created by this flow.

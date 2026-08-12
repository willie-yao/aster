# Causal-group remediation investigation

> **Status:** private verification foundation. The read-only investigator,
> typed result, private cache, deterministic verifier, and frozen benchmark
> exist, but no server endpoint, public actionable state, File Issue eligibility,
> or Fix PR handoff is enabled yet.

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
- one immutable repository revision for the bounded source investigation;
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
   successful content-bearing artifact read and source read are required.
2. **Structured finalization.** Read tools are removed. The model must return one
   strict typed classification. Unknown and duplicate JSON fields, trailing
   data, partial targets, unread citations, and unverified citation quotes are
   rejected.

If the evidence floor is not met, dashboard code produces a safe private
`insufficient_evidence` result without asking the model to invent a target.
No workspace, shell, branch, issue, pull request, or write tool is available.
The experimental Agent Sandbox analyzer is not used. Agent Sandbox/OpenCode
remains the later patch-generation stage after deterministic target verification.

## Typed private result

The result classifications are:

- `actionable`
- `already_fixed`
- `external_dependency`
- `environment_or_infrastructure`
- `mitigation_only`
- `insufficient_evidence`

Every result also states whether the source supports, refines, contradicts, or
cannot resolve the published cause. All classifications require bounded evidence.
Non-actionable results must set the proposal to `null`.

An `actionable` result is still only a private model proposal. It must contain:

- one immutable destination repository revision;
- one existing typed `models.RemediationTarget`;
- expected behavioral change;
- proof connecting the target to the recurring cause;
- claimed current-source absence;
- verification requirements;
- allowed changed paths; and
- allowed validation commands.

The exact target path must have been read during the evidence phase. Source and
artifact citations are reread and quote-verified before the result can enter the
private cache. This structural acceptance does not grant action eligibility.
Deterministic repository, current-source, dependency ownership, target behavior,
conversion, ambiguity, and already-present verification remain the next stage.

## Deterministic verification

Verification version 2 rechecks one accepted private cache entry before it can
be considered actionable. The verifier binds the cached result digest and full
provenance to the current frozen input, then independently:

- rereads every source and artifact citation and validates every referenced
  per-build analysis;
- requires evidence coverage for every exact causal-group build;
- requires an exact source citation for the typed target path;
- treats relevant files only as an additional relationship hint, never as target
  proof;
- requires the proposal repository and revision, path, and commands to match the
  engine-frozen source and destination policy;
- runs existing `actionverify` target verification at current source and every
  available failure revision;
- proves required calls are actually missing and rejects already-present calls;
- resolves Prow job, container, environment name, and desired value uniquely;
- keeps repository-local call resolution within module boundaries;
- reapplies conversion and destructive-remediation policy; and
- rejects mutated, duplicate, unknown, fabricated, unlinked, ambiguous, or
  dependency-owned targets.

The first deterministic version accepts only package-symbol addition, a
`modify_symbol` target with an exact `required_call`, and exact Prow job
environment changes. A prose-only `modify_symbol` and general configuration
change remain `insufficient_evidence` until they have a typed behavioral or
field-path predicate. Module-cache and workspace paths are never repository
targets.

A target already present in current source becomes `already_fixed` with no
proposal. A target not proven unresolved at every failure revision becomes
`insufficient_evidence`. Only a target proven unresolved in current source and
all failure sources remains `actionable`. Evidence-backed non-actionable
classifications remain terminal and cannot gain a target. A model-only
`already_fixed` claim without a typed target is downgraded because current-source
presence cannot be independently verified. An `external_dependency` claim is
also downgraded until the private contract carries a typed dependency ownership
identity that can be resolved to another repository.

The verifier returns a private `VerifiedResult`. It does not publish a public
state or grant Fix PR eligibility.

## Private cache

The cache lives at:

```text
<data-dir>/.remediation-investigations/cache.json
```

The directory is `0700`, the cache and lock files are `0600`, and writes use a
cross-process file lock plus durable atomic replacement. Cache version 2 binds a
canonical result digest, so an in-memory or on-disk result mutation is rejected
before verification. Corrupt, oversized, or unsupported state fails closed. A
failed refresh records only a bounded category,
timestamp, and error digest while preserving the previous valid result for the
same semantic key. A changed identity creates a cache miss instead of reusing the
old result.

The server already hides dot-directories under `/data`. The Pages workflow also
strips the remediation-investigation cache before upload. Cached entries contain
only the typed result, content-free provenance, evidence counters, usage totals,
and latency. They do not contain credentials, endpoints, raw prompts, raw model
responses, transcripts, source bundles, or tool payloads.

## Frozen benchmark

The committed manifest is:

```text
backend/internal/e2e/testdata/benchmarks/remediation-investigation-v1.json
SHA-256: 93fff9a14a51abba3490d18d93a3404d830f9244ccb102dc82c623fbb52596ae
```

It freezes 12 categories:

1. actionable missing call;
2. actionable missing Prow environment configuration;
3. target already present at the pinned revision;
4. fixed in current source;
5. external dependency;
6. environment or infrastructure;
7. mitigation only;
8. insufficient or ambiguous evidence;
9. unsafe conversion-webhook proposal;
10. wrong module-cache or dependency repository;
11. duplicated or unknown target; and
12. fabricated symbol or configuration field.

Hermetic tests validate the manifest, hashes, exact category coverage, two
distinct positive target kinds, typed contracts, cache preservation, read floors,
and scoring controls. Provider-backed trials are opt-in and write sanitized
private JSONL only:

```bash
RUN_REMEDIATION_INVESTIGATION_BENCHMARK=1 \
REMEDIATION_BENCH_REPETITIONS=3 \
REMEDIATION_BENCH_RESULTS_JSONL=/private/path/results.jsonl \
AI_API=responses \
AI_ENDPOINT=<configured-endpoint> \
AI_MODEL=<configured-model> \
go test ./internal/e2e -run '^TestRemediationInvestigationBenchmark$' -v -timeout 90m
```

`AI_TOKEN` is read by the client but is never printed, persisted, compared, or
hashed. Summarize the private JSONL with:

```bash
python3 hack/summarize-remediation-investigation-benchmark.py \
  /private/path/results.jsonl
```

The report separates structural validity, classification confusion, actionable
precision and recall, exact-target accuracy, unsafe false acceptances,
already-fixed blocking, verification status, invalid/no-result/runtime trials,
requests, tokens, cost coverage, latency, and repair counts.

## Current boundary

This foundation does not update the public remediation state and does not expose
an investigation API. Causal-group patterns remain blocked by
`models.PatternAllowsActions`. Deterministic verification is available privately,
but the provider holdout did not produce the two distinct verified actionable
positives required by the production gate. Therefore **Investigate possible
fix**, public terminal-state publication, File Issue eligibility, and Fix PR
preview remain disabled.

The later Fix PR handoff must consume only `VerifiedResult.Proposal`. It remains
deferred until remediation provider quality passes repeated cold holdouts and the
exact final Fix executor passes a separate direct-runtime smoke.

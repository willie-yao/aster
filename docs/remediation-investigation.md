# Causal-group remediation investigation

> **Status:** private verification foundation. The read-only investigator,
> minimal candidate contract, private evidence ledger and cache, deterministic
> verifier, and frozen benchmark exist, but no server endpoint, public
> actionable state, File Issue eligibility, or Fix PR handoff is enabled yet.

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
2. **Structured finalization.** Read tools are removed. The engine supplies a
   private evidence catalog with deterministic IDs. The model may return only a
   cause assessment, concise reason, optional typed candidate target, selected
   evidence IDs, and a typed non-actionable reason when no candidate exists.

If the evidence floor is not met, dashboard code produces a safe private
`insufficient_evidence` result without asking the model to invent a target. No
workspace, shell, branch, issue, pull request, or write tool is available. The
experimental Agent Sandbox analyzer is not used. Agent Sandbox/OpenCode remains
the later patch-generation stage after deterministic target verification.

## Minimal model result

Result version 3 contains exactly:

```text
version
cause_assessment
reason
candidate
engine-issued evidence_ids
non_actionable_reason
```

The model does not author a final lifecycle classification. It also does not
author repository or revision identity, current-source state, failure-revision
state, allowed paths, validation commands, verification requirements, or action
eligibility.

`candidate` is a discriminated union. Each variant contains only relevant
fields:

- `required_call`: `path`, `containing_symbol`, and `required_call`;
- `symbol_addition`: `path` and `symbol`;
- `prow_environment_entry`: `config_path`, `job`, `container`, `name`, and
  `value`; and
- `configuration_field`: `path`, typed `field_path` segments, and `value`.

General configuration fields are represented without a universal bag of empty
fields, but they remain non-actionable until a deterministic field-state
predicate exists.

A result with no candidate must use exactly one typed reason:

- `environment_or_infrastructure`;
- `mitigation_only`;
- `insufficient_evidence`; or
- `dependency_ownership_unverified`.

Candidate and non-actionable reason are mutually exclusive. The model is not
asked to return `already_fixed`. When evidence identifies an exact candidate,
the engine checks that candidate at failure and current revisions and derives
`actionable` or `already_fixed`.

## Engine-issued evidence ledger

The model no longer authors evidence paths, source revisions, line numbers,
quotes, build IDs, or timestamps. Dashboard code reconstructs successful tool
reads into a private versioned catalog:

- source evidence binds repository, revision, path, and content digest;
- analysis evidence binds build ID, analysis timestamp, and root-cause digest;
- artifact evidence binds build ID, artifact path, and content digest.

Each record receives a deterministic ID over its complete identity. The final
model response may cite only those IDs. Before caching and again during
deterministic verification, dashboard code resolves every selected ID, rereads
source and artifacts, rechecks digests, and matches analysis identities to the
frozen input. Unknown, duplicate, or mutated IDs fail closed.

The exact candidate path must have a selected source evidence ID. Every causal
build must have selected analysis or artifact evidence before a terminal result
can pass deterministic verification.

## Deterministic verification

Verification version 3 rechecks one accepted private cache entry before it can
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
- keeps repository-local call resolution within module boundaries;
- reapplies conversion and destructive-remediation policy; and
- rejects mutated, duplicate, unknown, fabricated, unlinked, ambiguous,
  workspace, module-cache, or wrong-repository targets.

The currently actionable deterministic target kinds remain limited to a
`modify_symbol` target with an exact `required_call` and exact Prow job
environment changes. Package-symbol additions and general configuration fields
remain `insufficient_evidence` until they have deterministic behavioral-role and
field-state predicates. Textual mention plus source absence is not sufficient
proof for a new symbol.

The engine derives the terminal classification:

- a target already present in current source becomes `already_fixed` with no
  proposal;
- a target not proven unresolved at every failure revision becomes
  `insufficient_evidence`;
- only a target proven unresolved in current source and all failure sources
  becomes `actionable`;
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
digest. Corrupt, oversized, mutated, or unsupported state fails closed. A failed
refresh records only a bounded category, timestamp, and error digest while
preserving the previous valid result for the same semantic key. A changed
identity creates a cache miss instead of reusing the old result.

The server hides dot-directories under `/data`. The Pages workflow also strips
the remediation-investigation cache before upload. Cached entries contain the
typed private result, digest-only evidence identities, content-free provenance,
evidence counters, usage totals, and latency. They do not contain credentials,
endpoints, raw prompts, raw model responses, transcripts, source bundles, tool
payloads, or source excerpts.

## Frozen benchmark

The committed manifest is:

```text
backend/internal/e2e/testdata/benchmarks/remediation-investigation-v3.json
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
12. fabricated symbol or configuration field.

The external-dependency and wrong-repository cases expect
`insufficient_evidence` until a typed ownership identity can be independently
verified.

Hermetic tests validate the manifest, hashes, exact category coverage, frozen
Prow job identity, two distinct positive target kinds, discriminated candidate
contracts, evidence ID
reconstruction, cache preservation, read floors, deterministic source-state
checks, and scoring controls.

Provider-backed evaluation must use the final merged schema and exact intended
provider. The earlier 0/12 run against result version 2 is evidence that the old
model contract was unsuitable. It is not readiness evidence for result version
3 and is not evidence that source-grounded remediation is infeasible.

Run the provider gate in stages and stop at the first failed stage.

### Stage 1: structural gate

Run these three cases with two cold repetitions each:

```bash
RUN_REMEDIATION_INVESTIGATION_BENCHMARK=1 \
REMEDIATION_BENCH_CASES=actionable-missing-call,target-already-present-at-pinned-revision,environment-or-infrastructure \
REMEDIATION_BENCH_REPETITIONS=2 \
REMEDIATION_BENCH_RESULTS_JSONL=/private/path/stage-1.jsonl \
AI_ENDPOINT=<configured-endpoint> \
AI_MODEL=<configured-model> \
go test ./internal/e2e -run '^TestRemediationInvestigationBenchmark$' -v -timeout 60m
```

Proceed only if at least five of six trials are structurally valid.

### Stage 2: positive-target gate

Run the two positive cases with three cold repetitions each:

```bash
RUN_REMEDIATION_INVESTIGATION_BENCHMARK=1 \
REMEDIATION_BENCH_CASES=actionable-missing-call,actionable-missing-job-environment \
REMEDIATION_BENCH_REPETITIONS=3 \
REMEDIATION_BENCH_RESULTS_JSONL=/private/path/stage-2.jsonl \
AI_ENDPOINT=<configured-endpoint> \
AI_MODEL=<configured-model> \
go test ./internal/e2e -run '^TestRemediationInvestigationBenchmark$' -v -timeout 60m
```

Proceed only if each positive case produces the correct deterministically
verified target in at least two of three repetitions and there are zero unsafe
acceptances.

### Stage 3: full matrix

Only after stages 1 and 2 pass, run all 12 cases with three cold repetitions:

```bash
RUN_REMEDIATION_INVESTIGATION_BENCHMARK=1 \
REMEDIATION_BENCH_REPETITIONS=3 \
REMEDIATION_BENCH_RESULTS_JSONL=/private/path/stage-3.jsonl \
AI_ENDPOINT=<configured-endpoint> \
AI_MODEL=<configured-model> \
go test ./internal/e2e -run '^TestRemediationInvestigationBenchmark$' -v -timeout 120m
```

Set `AI_API` only when the intended provider mode is not the client default.
`AI_TOKEN` is read by the client but is never printed, persisted, compared, or
hashed. Summarize each private JSONL with:

```bash
python3 hack/summarize-remediation-investigation-benchmark.py \
  /private/path/stage-1.jsonl
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

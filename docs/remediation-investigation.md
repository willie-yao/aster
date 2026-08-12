# Causal-group remediation investigation

> **Status:** private foundation. The read-only investigator, typed result,
> private cache, and frozen benchmark exist, but no server endpoint, public
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
- project destination repositories, allowed paths, and exact validation commands;
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

## Private cache

The cache lives at:

```text
<data-dir>/.remediation-investigations/cache.json
```

The directory is `0700`, the cache and lock files are `0600`, and writes use a
cross-process file lock plus durable atomic replacement. Corrupt, oversized, or
unsupported state fails closed. A failed refresh records only a bounded category,
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
SHA-256: e7c41be4da59684652bbdf6a2c6a71b6eec70f5207f5354ae6f89093c4fa6d1d
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
`models.PatternAllowsActions`. The next stage must independently verify every
candidate and convert any failure to a terminal non-actionable result before an
authenticated **Investigate possible fix** control can publish a safe summary.
Fix PR preview remains deferred until the direct-credential and Responses API
runtime work is settled and the exact Fix executor passes a separate smoke.

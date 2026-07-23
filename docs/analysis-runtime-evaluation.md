# Failure analysis runtime evaluation

Status: Current recommendation as of July 23, 2026.

This document summarizes the evaluation of three ways to run failure analysis in
`prow-ai-dashboard`:

1. The dashboard's in-process analyzer.
2. Orka's generic `type: ai` worker with a dashboard compatibility patch.
3. The dashboard-owned analyzer inside an Orka `type: container` Task.

[ADR 0001](architecture-decisions/0001-analysis-runtime-ownership.md) records
the supported mainline architecture. This document explains the engineering
tradeoffs behind that decision and why the containerized Orka design is still
worth preserving as an experimental evaluation and demonstration option.

## Executive summary

The in-process analyzer is the best fit for failure analysis because the
dashboard already owns the complete analysis policy. Keeping execution beside
that policy gives the project one implementation for prompts, tools, evidence,
critique, cache acceptance, traces, and result schemas.

The patched Orka worker was the least sustainable option. It started as a
compatibility layer, but eventually controlled Tool selection, evidence repair,
context retention, finalization sequencing, provider fallback, result
construction, and result acceptance. It became a dashboard-specific agent
runtime maintained as a patch against pinned upstream worker internals.

The Orka container analyzer had a much cleaner ownership boundary. It ran the
same dashboard `FailureAnalyzer`, so Orka owned lifecycle without owning model
policy. That made it architecturally preferable to the patched worker. It was
still mostly a sidegrade from in-process execution. The same analyzer produced
highly variable results in both runtimes, while the container path added bundle,
result, cache, trace, encryption, RBAC, retention, and cleanup transports around
an ordinary Kubernetes Job.

The recommended portfolio is:

- Failure analysis runs in-process in the fetcher or worker for supported
  deployments.
- Orka remains available for fix generation through
  `ai.fix_prs.agent_runtime.type: orka`.
- The patched Orka AI worker stays retired.
- The containerized Orka analyzer remains available as an explicitly
  experimental option for repeated evaluation, lifecycle demonstrations, and
  manager-facing prototypes.

The supported main branch currently ships the in-process analyzer and Orka fix
runtime, not the container adapter. The container option can be kept reproducible
through a named evaluation branch or tag without making it a supported product
mode.

This is not a conclusion that Orka is generally unsuitable. Orka did not provide
enough additional value to become the supported failure-analysis runtime, but
the cleaner container boundary is useful enough to keep as an option while the
team continues evaluating Orka and future lifecycle requirements.

## The core ownership question

The important distinction is between analysis policy and execution lifecycle.

Analysis policy includes:

- Provider and model behavior
- Prompt composition and diagnostic skills
- Tool registration and authorization
- Evidence planning and deterministic repair
- Context retention
- Finalization and validation
- Critique and semantic review
- Cache acceptance and invalidation
- Private traces
- Result schemas

Execution lifecycle includes:

- Scheduling and placement
- Retry and timeout
- Cancellation
- Attempt history
- Workload isolation
- Durable result storage

`prow-ai-dashboard` must own the first list because those behaviors define what a
safe, publishable diagnosis means. Orka can potentially own the second list, but
only if its lifecycle value is greater than the integration and operational
cost.

## Comparison

| Dimension | In-process analyzer | Patched Orka AI worker | Orka container analyzer |
| --- | --- | --- | --- |
| Analysis policy owner | Dashboard | Split between dashboard and worker patch | Dashboard |
| Model loop implementation | Dashboard Go code | Patched upstream Orka worker | Dashboard Go code |
| Execution unit | Goroutine in fetcher or worker | Orka `type: ai` Task | Orka `type: container` Task and Job |
| Cache and traces | Direct local persistence | Reconstructed from Tasks, Tools, events, and ingestion | Encrypted transport between one-shot Tasks and dashboard state |
| Expected diagnosis quality | Canonical | Can diverge from dashboard behavior | Equivalent to in-process, subject to model variance |
| Upgrade surface | Dashboard repository | Dashboard plus pinned Orka source and patch | Dashboard plus Orka controller and transport contracts |
| Main benefit | Simplicity and one implementation | Existing Orka AI Task integration | Per-failure Task lifecycle and isolation |
| Main cost | No per-failure Orka object | Maintained fork of the agent runtime | Second control plane around ordinary Jobs |
| Recommendation | Retain as supported default | Remove | Keep as experimental option |

## In-process analyzer

### Strengths

- One implementation owns all analysis behavior.
- Prompt, Tool, skill, critique, and cache changes are made and tested in the
  dashboard repository.
- No request bundle, result framing, encrypted state channel, or ingestion step
  is needed.
- Cache and private traces are written directly through the existing atomic
  stores.
- Pages and Kubernetes deployments use the same behavior.
- Provider and weak-model improvements apply to every deployment immediately.
- Debugging is simpler because the model loop and acceptance logic are in the
  same process and codebase.

### Weaknesses

- A failure is not represented by a dedicated Kubernetes or Orka object.
- Retry and cancellation happen at the analysis loop or workload level rather
  than through a per-failure Task API.
- Isolation is bounded by the fetcher or worker process instead of one Job per
  failure.
- Operators use dashboard traces and workload logs rather than an Orka Task UI.

### Assessment

These limitations are real, but they are operational rather than diagnostic.
The current product has a small number of consumers, a bounded read-only Tool
surface, private trace support, persistent cache state, and Kubernetes workload
controls. The simpler ownership model is more valuable than per-failure Task
objects today.

## Patched Orka AI worker

### Why it initially looked attractive

The existing Orka AI path already provided:

- One Task per failure
- Provider and model configuration
- Dynamic Tool resources
- Retry, timeout, placement, and Task history
- Execution events and an Orka result API
- A natural place to enforce model-loop behavior

A compatibility patch also had the lowest initial migration cost because it
could preserve the producer, Tool, and ingestor pipeline.

### What the patch eventually owned

Compatibility versions through v6 added or changed:

- Responses API support and Chat Completions fallback
- Copilot compatibility and API-mode telemetry
- Tool aliases and canonical Tool identity
- One-Tool-per-turn selection
- Finalization Tool recognition
- Strict structured result validation
- Timeline verification for transient diagnoses
- Bounded evidence and iteration budgets
- Reserved validation-repair calls
- Context compaction and an evidence ledger
- Ordered missing-evidence groups
- Deterministic candidate selection
- Synthetic guarded `read_artifact` calls
- Repair-queue advancement
- Final-submission turn reservation

This was no longer a small adapter. It was the dashboard's agent runtime embedded
inside a patch to Orka's generic worker.

### Weak-model progression

The Kimi benchmarks repeatedly showed that fixing one protocol failure exposed
the next one:

1. The model did not reliably converge on finalization.
2. It returned final JSON without deterministic validation.
3. It exhausted evidence calls before validation repair.
4. It repeated already-covered evidence during repair.
5. It sometimes emitted no reader call during repair.
6. Compatibility v6 correctly read four validator-selected candidates in order.
7. The final submission still omitted the repaired evidence tokens and was
   rejected.

A post-v5 Flatcar run ended with a rejected 2/5 diagnosis. Compatibility v6
fixed deterministic repair selection, but the next failure moved to repaired
Evidence-token propagation. Another patch could have merged those tokens into
the validation call, but that would have expanded the fork's ownership of result
construction again.

The patch did improve some runs. An earlier DRA comparison reduced Tool failures
and input usage and found a deeper root cause than the baseline. That evidence
shows the deterministic policies were useful. It does not show that maintaining
them inside an upstream worker patch was the right architecture.

### Operational cost

The patch required:

- A pinned upstream Orka commit
- A large patch and checksum
- Compatibility versions
- Fresh patch-application validation
- Focused and full worker tests
- Worker race tests
- Shared LLM and provider tests
- A dedicated worker image
- Immutable image tags and digests
- SBOM and provenance publication
- Upgrade work whenever upstream worker internals changed

### Assessment

The patched worker had an ownership inversion. Dashboard-specific safety and
quality policy lived in the component the dashboard did not own directly. It
also encouraged a sequence of narrow convergence fixes without a stable endpoint
for weak-model behavior.

Of the three options, this was the least suitable long-term design. If Orka were
to support this kind of strict analysis upstream, it would need a first-class
loop-policy or middleware interface rather than downstream patches to worker
internals.

## Orka container analyzer

### Why it was better than the patch

The container design corrected the main ownership problem:

- The dashboard called its normal `FailureAnalyzer`.
- Orka did not own prompts, Tools, evidence policy, critique, or result schemas.
- In-process and container execution shared the same analysis implementation.
- Orka owned only Task and Job lifecycle.

This was the correct direction to test if failure analysis had to run through
Orka.

### Lifecycle behavior that worked

The kind evaluation demonstrated:

- Analyzer retry after failure
- CPU-only placement with a mock GPU pool present
- Persistent cache reuse across one-shot Tasks
- Private trace persistence
- Five concurrent Task identities and safe state merging
- Task result retrieval
- Bundle and workload cleanup

Five-Task waves applied in about 0.5 to 0.7 seconds and completed in about 11.3
to 11.5 seconds with the scripted model.

### Why it was a sidegrade

The container path called the same `FailureAnalyzer`. It therefore should not be
expected to improve reasoning quality. Any single-run quality difference is
primarily model nondeterminism, not an architectural advantage from Orka.

Repeated cold Flatcar trials confirmed this:

| Model | In-process scores | Container scores | In-process median runtime | Container median runtime |
| --- | --- | --- | ---: | ---: |
| `gpt-5-mini` | 2/5, 2/5, 4/5 | 5/5, 2/5, 3/5 | 3m08s | 3m01s |
| `claude-haiku-4.5` | 4/5, 4/5, 0/5 | 3/5, 3/5, 2/5 | 1m55s | 1m39s |

| Model and runtime | Median Tool calls | Median artifact bytes | Median context bytes |
| --- | ---: | ---: | ---: |
| `gpt-5-mini` in-process | 15 | 1,735,770 | 233,708 |
| `gpt-5-mini` container | 15 | 1,463,118 | 140,443 |
| `claude-haiku-4.5` in-process | 26 | 6,662,540 | 301,965 |
| `claude-haiku-4.5` container | 26 | 3,923,119 | 304,552 |

A previous single Kimi comparison scored 0/5 in-process and 2/5 in the container.
Both results were below the acceptance bar. The repeated mid-tier matrix showed
why that one comparison was not enough to claim a container advantage.

Some 4/5 narratives explicitly stated that the Node registered but did not match
the scorer's wording for that signal. The same scorer evaluated both runtimes,
so this does not change the conclusion that model-run variance was larger than
the runtime difference.

The matrix covers one demanding Flatcar failure rather than every possible
analysis workload. The architecture decision does not depend on treating it as a
universal model ranking. It combines the absence of a stable runtime effect with
the measured lifecycle overlap and maintenance cost.

### Transport and persistence cost

Running the dashboard analyzer as a one-shot Task required new contracts for:

- A framed result marker in mixed pod logs
- A content-addressed immutable project and request bundle
- ConfigMap ownership, claims, retention, and Task-UID-bound cleanup
- A 96-KiB environment transport limit
- Secret-backed model and encryption credentials
- AES-GCM cache and private trace state
- Replay protection tied to Task identity
- Concurrent cache and trace merging
- Cross-namespace RBAC
- Analyzer image build and entrypoint behavior
- A pinned Orka controller for integration testing

The retained experiment contained about 1,980 lines of production Go, 2,631
lines of Go tests, and 312 lines of scripts. Most of this code transported state
that the in-process analyzer already accessed directly.

### Lifecycle value compared with Kubernetes Jobs

The container path did provide a Task abstraction, but much of the demonstrated
behavior overlapped with ordinary Job capabilities:

- Job backoff provides retry.
- `activeDeadlineSeconds` and process deadlines provide timeout.
- Node selectors, affinity, tolerations, and resource requests provide placement.
- Job or Pod deletion provides cancellation.
- Job and Pod status provide execution history.

Orka cancellation existed upstream as a Task status transition followed by Job
termination, but the dashboard adapter did not expose it. Durable results also
required a separately persistent Orka store. The kind install used ephemeral
SQLite and warned that results would be lost on controller restart.

### Integration risk observed during the final run

Removing the old generic image path changed the analyzer binary location while
the Task still overrode it with `/app`. Every container Task failed before model
execution. Unit and shell tests passed; the live kind run found the mismatch.

The issue was fixed by allowing the image entrypoint to own the command, but the
incident demonstrated the ongoing integration cost of a path that was not a
supported product mode.

### Assessment

The container version was much better designed than the patched worker. It was
also a sidegrade from in-process analysis rather than a clear product
improvement. It should not replace the in-process analyzer or become a supported
Helm mode today.

It is still useful to keep the containerized design available as an experimental
option:

- It demonstrates a clean way to use Orka without moving dashboard policy into
  Orka internals.
- It provides a concrete manager-facing example of Orka Task lifecycle,
  placement, retry, result history, and isolation.
- It preserves an escape hatch if future consumers need one Job per failure.
- It gives the project a controlled comparison target for new models or Orka
  lifecycle capabilities.

Keeping the option should not imply production support. It should remain off by
default, outside the supported Helm configuration, and free of compatibility
promises. New analysis policy must continue to land in the dashboard-owned
`FailureAnalyzer`, not in the container adapter.

If a future product requirement demands per-failure isolation, cancellation, or
Task history, the container boundary is the Orka approach worth reconsidering.
That evaluation should include a comparison against a simpler dashboard-owned
Kubernetes Job implementation.

## Useful concepts retained from the Orka work

Keeping the container option does not change where analysis policy belongs. The
following concepts from the Orka work now live in the canonical dashboard
analyzer:

- A shared `FailureAnalyzer` contract
- Engine-owned diagnostic profiles
- Ranked exact evidence candidates
- Conditional required-evidence groups
- Deterministic evidence repair
- Bounded artifact-tree seeding
- Context compaction that preserves important evidence
- Strict structured result parsing
- Critique and semantic review
- Responses API support and provider observability
- Private content-free traces
- Cache acceptance tied to current quality floors and skill hashes

These improvements benefit both Pages and Kubernetes deployments without
requiring a second runtime.

## Where Orka still fits

Orka remains a better production fit for fix generation than for failure
analysis. Fix generation has a meaningful runtime boundary:

- It needs an isolated source workspace.
- It can run for multiple turns.
- Cancellation and attempt history are operationally useful.
- It produces files and a diff as a structured artifact.
- The dashboard can independently pin the base, validate the diff, critique the
  change, run verification, and control PR creation.

This uses Orka for a capability it adds rather than wrapping a short read-only
analysis that the dashboard can already run directly. The containerized analyzer
is a secondary fit for evaluation and demonstration because it uses the same
clean lifecycle boundary without making Orka the policy owner.

## Current repository state and evaluation option

The supported main branch no longer ships the container adapter. The last
working mainline prototype, including the analyzer-entrypoint fix, is preserved
in Git history at commit `a34e2ae` and in the benchmark and pull-request history.

If the team wants the containerized Orka option readily demonstrable, keep a
named evaluation branch or tag based on that working prototype. Treat it as a
reproducible lab rather than a product mode:

- Run it only in isolated kind or evaluation namespaces.
- Schedule analyzer and helper workloads on CPU nodes.
- Reserve GPU nodes for model serving.
- Do not add new user-facing configuration or compatibility guarantees.
- Do not let experimental transport changes block in-process improvements.
- Use repeated cold trials rather than a favorable single run.
- Require a concrete lifecycle or operator benefit before proposing
  productization.

This gives the team something tangible to evaluate and show without restoring
the patched worker or committing to a second supported runtime.

## Recommendation

1. Keep the in-process analyzer as the only supported failure-analysis runtime.
2. Keep analysis policy in dashboard-owned Go code.
3. Permanently avoid worker patches for dashboard-specific model-loop policy.
4. Keep the containerized Orka design available as an explicitly experimental
   evaluation and manager-demo option.
5. Retain Orka for `ai.fix_prs.agent_runtime.type: orka` fix generation.
6. Continue improving model quality with repeated benchmarks rather than
   single-run comparisons.
7. Productize the container path only when a concrete lifecycle need justifies
   its additional control plane and transport contracts.

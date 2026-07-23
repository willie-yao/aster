# ADR 0001: Dashboard ownership of analysis policy

- Status: Accepted
- Date: 2026-07-23

## Context

`prow-ai-dashboard` supports two analysis execution paths:

- The in-process analyzer runs the dashboard-owned agentic loop inside the
  fetcher or worker.
- The original Orka preview creates `type: ai` Tasks that run Orka's generic AI
  worker.

The dashboard requires a stricter analysis contract than the generic Orka AI
worker provides for weaker models. Compatibility versions through v6 added
provider fallback, bounded context retention, finalization gates, evidence
repair, Tool selection, and result validation to a patch against pinned Orka
worker internals.

That patch is now a dashboard-specific agent runtime with its own source pin,
checksum, image publication, race suite, and upgrade lifecycle. Continuing to
add analysis policy there would duplicate the first-party in-process engine and
couple dashboard behavior to an upstream worker implementation.

Three changes established a cleaner boundary:

1. `FailureAnalyzer` defines one dashboard-owned single-failure contract.
2. Ranked evidence planning and deterministic repair run in the in-process
   analyzer.
3. The container analyzer prototype runs the same `FailureAnalyzer` inside an
   Orka `type: container` Task without using Orka's AI worker, Provider, or Tool
   resources.

## Decision

### Analysis policy

`prow-ai-dashboard` owns all analysis policy:

- Provider and model behavior
- Prompts and diagnostic skills
- Tool registration and execution
- Evidence planning and repair
- Critique and semantic review
- Cache acceptance and invalidation
- Private analysis traces
- The `FailureAnalysisResult` schema

The in-process analyzer is the canonical implementation and remains the default
production path.

### Orka lifecycle integration

Orka may own execution lifecycle for an optional dashboard-owned analyzer:

- Task and Job lifecycle
- Retry and timeout
- CPU and memory placement
- Cancellation
- Attempt history
- Durable result availability

The dashboard-owned analyzer container must call `FailureAnalyzer`; it must not
reimplement analysis policy.

### Patched Orka AI worker

The patched `type: ai` compatibility worker is frozen at compatibility v6.

Allowed changes are limited to:

- Security fixes
- Critical correctness fixes for existing v6 users
- Build, provenance, or dependency maintenance needed to keep the frozen image
  reproducible

The project will not add compatibility v7 or new weak-model convergence policy
to the patched Orka AI worker.

Compatibility v6 remains available only as an experimental fallback and
comparison baseline while the container lifecycle path is evaluated.

### Container analyzer status

The Orka container analyzer remains experimental until all of these are proven:

1. A first-class structured result contract that does not depend on mixed pod
   log framing.
2. An immutable content-addressed request and consumer project bundle.
3. Persistent cache and private trace transport across one-shot Tasks.
4. Clean in-process and container benchmarks against the same model, request,
   configuration, and artifacts.
5. A bounded multi-failure load test covering scheduling, retries, cancellation,
   ingestion, and cleanup.

No supported Helm analysis mode will be added before these gates pass.

### Placement

Dashboard analyzer and Orka helper workloads use CPU nodes. GPU nodes are
reserved for Ray model serving and required GPU platform workloads.

### Fix generation

Orka-based coding-agent and fix-generation runtimes are independent from the
analysis backend. This decision does not remove or constrain
`ai.fix_prs.agent_runtime.type: orka`.

## Consequences

### Benefits

- One canonical implementation and home for new analysis policy
- No new dashboard policy in patched Orka internals
- In-process and container execution share results, cache rules, skills, and
  quality gates
- Orka upgrades can remain independent from model-loop behavior
- The container experiment has explicit production gates and an exit decision

### Costs

- The container path still needs request, result, cache, and trace transport
- One-shot Jobs add scheduling and image startup overhead
- Orka remains an additional control plane when container execution is enabled
- Compatibility v6 must be maintained until it is retired or removed

## Alternatives considered

### Continue extending the patched AI worker

Rejected as the default direction. It has the lowest immediate migration cost
but maintains a second agent runtime and makes every Orka upgrade a policy
integration project.

### Use AgentRuntime for failure analysis now

Deferred. AgentRuntime provides a richer protocol but adds a long-running
service, authentication, cancellation, and harness versioning for a one-shot
analysis that maps naturally to a Job.

### Remove Orka analysis immediately

Deferred until the container lifecycle evaluation finishes. Per-failure
isolation, retry, placement, Task history, and durable result lifecycle may
justify the control plane.

## Follow-up work

1. Replace mixed-log final-line parsing with a dashboard-owned structured result
   marker or authenticated result channel.
2. Add an immutable content-addressed request and project bundle.
3. Persist cache and private traces across container Tasks.
4. Run clean in-process and container Kimi benchmarks.
5. Run a small multi-failure load test.
6. Decide whether to productize the container path or remove Orka analysis.
7. Retire the compatibility worker after no supported deployment depends on it.

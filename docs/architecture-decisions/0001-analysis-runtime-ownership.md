# ADR 0001: Dashboard ownership of analysis policy

- Status: Accepted
- Date: 2026-07-23
- Amended: 2026-07-23

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

The patched `type: ai` compatibility worker will be removed without a
compatibility period. It duplicates dashboard analysis policy and is not part of
the retained Orka lifecycle experiment.

The project will not add compatibility v7, preserve the v6 Helm mode, or carry
configuration fields that exist only for the patched worker.

### Container analyzer status

The Orka container analyzer remains an internal experiment. Result framing,
immutable bundles, encrypted state transport, repeated cache use, CPU placement,
retry, cleanup, and bounded concurrent execution have been demonstrated.

The experiment will remain separate from supported Helm analysis configuration
while repeated parity tests run against one or two mid-tier models. Those tests
measure whether the container preserves dashboard behavior. A quality difference
between two stochastic model runs does not establish Orka lifecycle value.

The retention decision will instead weigh Task retry, cancellation, history,
placement, and durable results against the maintenance cost of the bundle, state,
RBAC, retention, and ingestion contracts.

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
- The experimental container transport remains a significant maintenance surface

## Alternatives considered

### Continue extending the patched AI worker

Rejected as the default direction. It has the lowest immediate migration cost
but maintains a second agent runtime and makes every Orka upgrade a policy
integration project.

### Use AgentRuntime for failure analysis now

Deferred. AgentRuntime provides a richer protocol but adds a long-running
service, authentication, cancellation, and harness versioning for a one-shot
analysis that maps naturally to a Job.

### Remove all Orka analysis immediately

Rejected for the container experiment until the additional model and lifecycle
evaluation is complete. The patched AI worker does not share that deferral and
will be removed.

## Follow-up work

1. Decouple the container experiment from patched-worker assets.
2. Remove the patched `type: ai` analysis mode and its configuration.
3. Run repeated in-process and container parity tests with one or two mid-tier
   models.
4. Verify cancellation and operator-visible history against an ordinary
   Kubernetes Job baseline.
5. Decide whether lifecycle value justifies retaining the container experiment.

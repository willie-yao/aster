# ADR 0001: Dashboard ownership of analysis policy

- Status: Accepted
- Date: 2026-07-23
- Amended: 2026-07-23

## Context

`prow-ai-dashboard` originally supported two analysis execution paths:

- The in-process analyzer ran the dashboard-owned agentic loop inside the
  fetcher or worker.
- The Orka preview created `type: ai` Tasks that ran Orka's generic AI worker.

The dashboard requires a stricter analysis contract than the generic Orka AI
worker provides for weaker models. Compatibility versions through v6 added
provider fallback, bounded context retention, finalization gates, evidence
repair, Tool selection, and result validation to a patch against pinned Orka
worker internals.

That patch had become a dashboard-specific agent runtime with its own source
pin, checksum, image publication, race suite, and upgrade lifecycle. Continuing
to add analysis policy there would have duplicated the first-party in-process
engine and coupled dashboard behavior to an upstream worker implementation.

Three changes established a cleaner boundary:

1. `FailureAnalyzer` defines one dashboard-owned single-failure contract.
2. Ranked evidence planning and deterministic repair run in the in-process
   analyzer.
3. The container analyzer prototype ran the same `FailureAnalyzer` inside an
   Orka `type: container` Task without using Orka's AI worker, Provider, or Tool
   resources. The final evaluation below led to its removal.

## Decision

### Analysis policy and execution

`prow-ai-dashboard` owns all analysis policy and executes it in-process inside the
fetcher or worker. This is the only failure-analysis runtime. It owns provider
behavior, prompts, tools, evidence planning and repair, critique, cache
acceptance, private traces, and result schemas.

Both experimental Orka analysis paths are removed:

- The patched `type: ai` worker duplicated dashboard policy inside upstream
  worker internals.
- The dashboard-owned `type: container` adapter duplicated lifecycle, input,
  result, cache, trace, encryption, RBAC, retention, and cleanup contracts around
  an ordinary Kubernetes Job.

### Fix generation

Orka fix generation remains supported through
`ai.fix_prs.agent_runtime.type: orka`. It is an independent runtime boundary and
satisfies the project's Orka integration requirement without moving failure
analysis policy or lifecycle into Orka.

### Placement

Dashboard and Orka fix-generation helpers use CPU nodes. GPU nodes remain
reserved for model serving and required GPU platform workloads.

## Final container evaluation

On July 23, 2026, two mid-tier models ran three cold Flatcar trials through each
runtime. Every trial used the same request, fixture, prompt, skills, 15
iterations, five-Tool floor, 500,000-byte artifact floor, and two critique
retries.

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

Some 4/5 narratives explicitly said that the Node registered but did not match
the scorer's wording for that signal. The same scorer evaluated both paths, and
this does not change the main result: neither runtime produced a stable quality
advantage. Variation between model runs was larger than the difference between
runtimes.

### Lifecycle value

All six container trials also ran the scripted retry, persistent-cache,
five-Task load, CPU-placement, and cleanup cases. Those lifecycle checks passed.
Task waves applied in about 0.5 to 0.7 seconds and completed in about 11.3 to
11.5 seconds.

The demonstrated benefits did not justify the extra control plane:

- Retry, timeout, placement, deletion, and execution history already exist on
  Kubernetes Jobs.
- Cancellation existed upstream as a Task status transition and Job termination,
  but the dashboard adapter did not expose it.
- Durable results required a separately persistent Orka store. The test install
  used ephemeral SQLite and warned that results would be lost on restart.
- Operator-visible Task history was not integrated into a supported dashboard
  workflow.

### Maintenance cost

The retained experiment contained about 1,980 lines of production Go, 2,631
lines of Go tests, and 312 lines of scripts. Most of that code implemented
transport and persistence rather than analysis behavior. It also carried a
96-KiB ConfigMap environment limit, encrypted state replay protection, concurrent
state merging, Task-bound cleanup, cross-namespace RBAC, and a pinned Orka
controller lifecycle.

During the final evaluation, cleanup of the old generic image path changed the
analyzer binary location while the Task still overrode it with `/app`. Every
container Task failed before model execution until the adapter was changed to use
the image entrypoint. This regression passed unit and shell checks and was found
only by the live kind run, demonstrating the integration surface's maintenance
risk.

## Consequences

### Benefits

- One failure-analysis implementation and configuration surface
- No dashboard policy or lifecycle transport in Orka worker or Task adapters
- Smaller build, test, Helm, RBAC, trace, and documentation surfaces
- Model quality work applies directly to Pages and Kubernetes deployments
- Orka remains useful for the independent fix-generation boundary

### Costs

- Failure analysis no longer has per-failure Orka Task objects
- Operators use the existing worker, trace console, and Kubernetes workload
  lifecycle rather than an additional Orka result API

## Alternatives considered

### Keep the container adapter as an internal experiment

Rejected after the repeated model and lifecycle evaluation. Keeping thousands of
lines of transport and lifecycle code for a non-productized path would continue
to impose maintenance cost without a stable quality or operational advantage.

### Productize the container adapter

Rejected. The required result, bundle, persistence, encryption, retention, RBAC,
and cleanup contracts made Orka a second control plane around ordinary Jobs.

### Use AgentRuntime for failure analysis

Rejected for failure analysis. AgentRuntime is a better fit for interactive code
generation and remains available for Orka fix generation.

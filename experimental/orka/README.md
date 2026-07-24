# Orka integrations

The dashboard has two independent Orka integrations:

- `ai.fix_prs.agent_runtime.type: orka` delegates fix generation to an Orka
  Agent workspace. This is the supported Orka integration.
- `analysisRuntime.type: orka-container` is an explicitly experimental Helm
  cron option. It runs the dashboard-owned `FailureAnalyzer` in an Orka
  `type: container` Task without using Orka Providers, Tools, or the generic AI
  worker.

The in-process analyzer remains the default and only recommended production
mode. The former patched `type: ai` analysis mode remains removed.

## Container lifecycle experiment

The experiment evaluates whether Task retry, cancellation, placement, attempt
history, and durable results justify the additional control plane. It does not
move prompts, tools, evidence policy, critique, cache acceptance, or result
schemas into Orka.

The isolated harness also applies a supplemental Role for the pinned Orka
controller because that upstream chart does not grant permissions for two
controllers it registers. This does not expand the dashboard analysis
ServiceAccount, which remains Task and ConfigMap only.

The implementation includes:

- a content-addressed immutable request and project bundle
- framed dashboard result output
- encrypted cache and private trace state
- CPU-only placement and bounded Task execution
- retry, cache reuse, concurrent merge, and cleanup checks

See [the runtime evaluation](../../docs/analysis-runtime-evaluation.md) and
[ADR 0001](../../docs/architecture-decisions/0001-analysis-runtime-ownership.md)
for the decision boundary.

Run the isolated kind test from the repository root:

```bash
experimental/orka/run-container-analyzer-kind.sh
```

The default run uses a scripted model and does not touch Ray or GPU nodes. Set
`ORKA_CONTAINER_LIVE_ENDPOINT`, `ORKA_CONTAINER_LIVE_MODEL`, and optionally
`ORKA_CONTAINER_LIVE_TOKEN` to add a live Flatcar benchmark.

The local ownership and cleanup regression check is:

```bash
experimental/orka/test-container-analyzer-kind.sh
```

The experiment has no compatibility guarantee, supports only Helm `mode: cron`,
and is not available on Pages. Analyzer, controller, and helper workloads must
remain on CPU nodes.

## Fix generation

`ai.fix_prs.agent_runtime.type: orka` moves only coding-agent generation into an
Orka Agent workspace. The dashboard still pins the base SHA, validates the
returned files and diff, runs critique and verification, and opens the pull
request.

Set `orka.fixRuntime.enabled: true` in Helm values to mount the Orka Task
ServiceAccount and use the git-capable fixer image. Configure the Agent reference,
namespace, API, and retry policy in `project.yaml`. Private repositories should
use a separate read-only repository credential for the Orka workspace.

# Orka integrations

The dashboard has two independent Orka integrations:

- `ai.fix_prs.agent_runtime.type: orka` delegates fix generation to an Orka
  Agent workspace. This is the supported Orka integration.
- The container analyzer is an internal lifecycle experiment. It runs the
  dashboard-owned `FailureAnalyzer` in an Orka `type: container` Task without
  using Orka Providers, Tools, or the generic AI worker.

Failure analysis in supported deployments always uses the in-process analyzer.
The former patched `type: ai` analysis mode has been removed.

## Container lifecycle experiment

The experiment evaluates whether Task retry, cancellation, placement, attempt
history, and durable results justify the additional control plane. It does not
move prompts, tools, evidence policy, critique, cache acceptance, or result
schemas into Orka.

The implementation includes:

- a content-addressed immutable request and project bundle
- framed dashboard result output
- encrypted cache and private trace state
- CPU-only placement and bounded Task execution
- retry, cache reuse, concurrent merge, and cleanup checks

See [CONTAINER_ANALYZER_ASSESSMENT.md](CONTAINER_ANALYZER_ASSESSMENT.md) for the
evidence and current decision boundary.

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

The experiment is not a Helm analysis mode and has no compatibility guarantee.
Analyzer and helper workloads must remain on CPU nodes.

## Fix generation

`ai.fix_prs.agent_runtime.type: orka` moves only coding-agent generation into an
Orka Agent workspace. The dashboard still pins the base SHA, validates the
returned files and diff, runs critique and verification, and opens the pull
request.

Set `orka.fixRuntime.enabled: true` in Helm values to mount the Orka Task
ServiceAccount and use the git-capable fixer image. Configure the Agent reference,
namespace, API, and retry policy in `project.yaml`. Private repositories should
use a separate read-only repository credential for the Orka workspace.

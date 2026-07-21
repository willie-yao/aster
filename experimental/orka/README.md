# Orka backend (experimental)

Runs the dashboard's AI failure analysis through
[Orka](https://github.com/orka-agents/orka), a Kubernetes-native agent
orchestration platform, instead of the engine's in-process agentic loop. Discovery
and output are unchanged; only the per-failure analysis step moves to Orka Tasks
that run alongside your inference stack.

> Opt-in and experimental. The engine's default analysis backend is the
> in-process agentic loop; select this path explicitly with `analysis: orka` in
> the Helm chart (see [QUICKSTART.md](QUICKSTART.md)). It requires the Orka
> control plane, a Provider, and the pinned compatibility AI worker. The dashboard chart owns
> its release-scoped artifact Tool service and base Tool ConfigMap by default.

## Docs

- **[QUICKSTART.md](QUICKSTART.md)** - how to deploy and run the Orka path, the config it
  reads, and the knobs. Start here.
- **[ARCHITECTURE.md](ARCHITECTURE.md)** - how it works: where each Orka resource
  is created, the CRD shapes, and how the engine's harness (cache, convergence,
  critique, skills) is reconstructed out of Kubernetes objects.
- **[worker-patches/](worker-patches/)** - the pinned, tested Orka AI-worker
  compatibility image, patch, version matrix, and measured convergence results.
- **[`orka-ops.sh`](orka-ops.sh)** - installation preflight, a disposable
  Provider smoke Task, batch status, and a project-scoped Task and Tool GC preview.

## Headline finding

Orka + a strong model (claude-sonnet-4.5) produces excellent, artifact-grounded
analyses that match or beat the engine's reference labels on the hardest CAPZ
cases. But that edge is the MODEL, not the harness: on the engine's own cheap model
(gemini-3.5-flash) Orka is no better at classification, and it needed the worker
patches just to converge. After those patches Orka is at process parity with the
engine on a cheap model (converges 15/15, grounds every transient verdict), and the
residual quality gap is pure model capability. Recommendation: adopt Orka as an
optional, co-located, strong-model backend; keep the engine as the default
cheap-model path; do not decommission. Convergence and discipline numbers are in
[worker-patches/README.md](worker-patches/README.md).

## Fix-PR generation runtime

`ai.fix_prs.agent_runtime.type: orka` moves only coding-agent generation into an
Orka Agent workspace. The engine still pins the base SHA, reconstructs and
validates the diff, runs critique and independent verification, and opens the
PR. Enable chart RBAC with `orka.fixRuntime.enabled: true`; private repositories
should use a separate read-only `git_secret` for the Orka workspace. The
referenced Agent runtime must support workspace diff capture and structured
results, such as the OpenCode runtime implementation maintained in Orka. Failed
or cancelled content-addressed Tasks are deleted and recreated, while each Task
also carries the configured Orka retry policy.

## Known constraints

- **Execution events are required.** The ingestor reads each Task's event stream
  to enforce the tool-call floor, terminal outcome, successful required quality tools,
  consumer recipe lookup, a `submit_analysis` token bound to the exact final
  JSON, and transient timeline evidence before publishing a result. Successful
  artifact content reads return scoped evidence tokens, and `submit_analysis`
  requires those tokens for every cited path and recipe evidence group.
  Recurrence, diff, and transient-signature checks are advisory; failures remain
  visible in telemetry without discarding a validated analysis.
- **Scheduled side effects run in batch mode.** The skeleton fetch disables
  side effects. The ingestor runs the same notifications, issue reconciliation,
  and fix-PR reconciliation as the in-process fetcher after job-level pattern
  finalization, or directly after ingestion when pattern analysis is disabled.
  Finalization and side-effect errors fail the batch so the CronJob retries.
  Mount the consumer project config and provide the same side-effect credentials
  to the ingestor. Webhook mode patches per-test results only.
- **Copilot needs the de-streaming proxy.** Copilot's non-streaming endpoint
  returns null tool_calls for Claude (the calls only arrive over streaming SSE).
  manifests/50-copilot-proxy.yaml de-streams so the worker sees real tool calls.
  Standard OpenAI-compatible endpoints (vLLM, Ray Serve, Dynamo/NIM, Ollama) do NOT
  need it - point a Provider straight at them.
- **The compatibility worker is required.** Small models otherwise fail to
  converge and skip the verify_timeline discipline. Pin the published worker tag
  or digest from the [compatibility matrix](worker-patches/COMPATIBILITY.md).
- **Artifact-tool authentication is required.** The Helm chart creates and
  preserves a release-scoped bearer Secret by default. External shim deployments
  must provide `orka.artifactTool.auth.existingSecret`. The service accepts
  routing only from producer-owned headers and the included NetworkPolicy limits
  ingress to Orka AI-worker pods.
- **SSRF guard.** Orka marks tools whose URL resolves to a private/loopback IP as
  Available=false. In-cluster ClusterIPs are private, so the tools show unavailable
  but still work (the worker does not gate on availability).
- **Cluster mapping (CAPZ).** A CAPZ e2e build runs many specs, each with its own
  cluster. The k8s tool group (35-k8s-tools.yaml, discover-clusters /
  find-my-cluster) lets the agent map a failing test to its cluster; enable it via
  ai.tools: [filesystem, k8s]. Filesystem-only consumers (e.g. non-CAPI projects)
  set ai.tools: [filesystem].

## Teardown

```bash
kind delete cluster --name orka-spike
```

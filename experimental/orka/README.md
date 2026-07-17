# Orka backend (experimental)

Runs the dashboard's AI failure analysis through
[Orka](https://github.com/orka-agents/orka), a Kubernetes-native agent
orchestration platform, instead of the engine's in-process agentic loop. Discovery
and output are unchanged; only the per-failure analysis step moves to Orka Tasks
that run alongside your inference stack.

> Opt-in and experimental. The engine's default analysis backend is the
> in-process agentic loop; select this path explicitly with `analysis: orka` in
> the Helm chart (see [QUICKSTART.md](QUICKSTART.md)). It requires Orka, the tool shim, a
> Provider, and the ai-worker patches installed in the cluster.

## Docs

- **[QUICKSTART.md](QUICKSTART.md)** - how to deploy and run the Orka path, the config it
  reads, and the knobs. Start here.
- **[ARCHITECTURE.md](ARCHITECTURE.md)** - how it works: where each Orka resource
  is created, the CRD shapes, and how the engine's harness (cache, convergence,
  critique, skills) is reconstructed out of Kubernetes objects.
- **[worker-patches/](worker-patches/)** - the required Orka ai-worker changes
  (convergence + transient-critique) and the numbers behind them.

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

## Known constraints

- **Execution events are required.** The ingestor reads each Task's event stream
  to enforce the tool-call floor, `validate_analysis`, and transient timeline
  evidence before publishing a result.
- **Scheduled pattern issues and fix PRs are not finalized yet.** The batch
  ingestor now writes job-level recurring patterns for the dashboard and
  interactive server actions, but the fetcher's unattended issue/fix-PR side
  effects run before Orka results exist. Keep those scheduled automations on the
  in-process backend until a post-finalization runner is wired.
- **Copilot needs the de-streaming proxy.** Copilot's non-streaming endpoint
  returns null tool_calls for Claude (the calls only arrive over streaming SSE).
  manifests/50-copilot-proxy.yaml de-streams so the worker sees real tool calls.
  Standard OpenAI-compatible endpoints (vLLM, Ray Serve, Dynamo/NIM, Ollama) do NOT
  need it - point a Provider straight at them.
- **Worker patches are required.** Small models otherwise fail to converge (submit
  empty results) and skip the verify_timeline discipline. Apply
  worker-patches/ai-worker-convergence.patch (iteration cap, forced finalization,
  empty-final re-prompt, transient-critique gate) and rebuild the ai-worker image.
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

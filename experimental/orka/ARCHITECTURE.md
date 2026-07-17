# Orka backend: architecture

How the Orka analysis path works at the code level: where each Orka resource is
created, the CRD shapes, and how the engine's in-process harness is reconstructed
out of Kubernetes objects. For how to run it, see [QUICKSTART.md](QUICKSTART.md). For the
evaluation and the productization call, see the "Headline finding" in
[README.md](README.md).

## The pipeline

The engine's flow is: discover jobs/builds/failing tests, analyze each failure
with the in-process agentic loop, write `dashboard.json` + `jobs/*.json`, serve.
Only the **analysis** step moves to Orka; discovery and output are unchanged.

```
fetcher -ai=false        writes the dashboard skeleton (jobs/*.json, no AI)
   |
orka-producer            one content-addressed Task per failing test
   |                     + per-build, header-routed Tool clones  -> kube-apply
   v
Orka ai-worker           runs the agentic loop per Task, calling the shim tools
   |
orka-ingestor            reads each Task's result, patches it into jobs/*.json
   |
server / Pages           unchanged
```

Three of these are code in this repo (`backend/cmd/orka-producer`,
`orka-ingestor`, `orka-artifact-tool`); the ai-worker is Orka's own binary,
carrying the patches in [worker-patches/](worker-patches/).

## Where Orka resources are created

Everything the producer emits is an unstructured `map[string]any` built in Go and
server-side applied through a dynamic client, so the pipeline links no Orka Go
types. The two CRDs it creates:

### Task (one per failing test)

Built by `buildTask` (`cmd/orka-producer/main.go:284`). Shape:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: az-<buildID>-<failureHash>-<version>   # content address
  labels:
    app.kubernetes.io/managed-by: orka-producer
    orka.dashboard/build: <buildID>
spec:
  type: ai
  timeout: <ai.timeout>
  retryPolicy: { maxRetries: <n> }             # when -retries > 0
  webhookURL: <ingestor>                        # when -webhook-url set
  ai:
    providerRef: { name: <provider> }
    model: <model>
    tools: [<tool-b<buildID>>, ...]             # the per-build Tool clones
    systemPrompt: <composed engine prompt + tool addendum>
    prompt: <per-failure user prompt>
```

The producer walks `jobs/*.json`, and for every `status: "failed"` test case
(`main.go:136-156`) emits one Task named by
`orka.TaskName(buildID, FailureHash(testName, failureMessage), version)`
(`internal/orka/naming.go`).

### Tool (one clone per distinct build x tool group)

A single artifact tool shim serves every build and bucket. The producer does
**not** create a tool per Task; it clones each base Tool CRD once per distinct
build (`main.go:159-169`) via `cloneToolForBuild` (`main.go:351`), injecting
static routing headers:

```yaml
spec:
  http:
    headers:
      X-Build-Prefix: <bucket-relative build dir>
      X-Bucket: <bucket>
      X-Storage-Provider: <gcs | gcsweb>   # from the consumer's project.yaml
      X-Storage-Base: <gcsweb gateway root>  # only for gcsweb (e.g. S3)
```

The shim resolves its backend per request from those headers
(`orka-artifact-tool/toolenv.go`), so a tool always reads the Task's own build
in the right bucket **and on the right storage provider** regardless of what the
model passes. Storage is provider-agnostic: the shim reuses the engine's
`internal/storage` (gcs, or gcsweb over an S3 gateway), defaulting from its
`STORAGE_*` env and overriding per request from the `X-Storage-*` headers the
producer derives from `project.yaml`. Base Tool CRDs are loaded from
`experimental/orka/manifests/` by `loadBaseTools` (`main.go:319`).

### The apply

`applyAll` (`main.go:187`) applies Tools before Tasks (Tasks reference them),
using `KubeClient.Apply` (`internal/orka/kube.go`), a server-side-apply Patch
with `FieldManager: orka-producer` and `Force: true`. `RESTConfig`
(`kube.go`) prefers in-cluster config and falls back to kubeconfig + a
`-context` override for local runs. Running without `-apply` only writes the YAML
to `-tasks-out` / `-tools-out` for inspection.

## How the result comes back

`orka-ingestor` re-derives each Task name from the same skeleton
(`FailureHash` is shared through `orka`, so both sides agree) and patches the
result in place. `applyResult` (`cmd/orka-ingestor/main.go:225`) fetches the
Task's result, parses the analysis JSON, and writes `tc.AISummary` +
`tc.AIAnalysis` with `Mode: "agentic"`, the same wire shape the in-process path
produces. Failing/absent results get the engine's `unavailable` placeholder via
`setUnavailable` (`main.go:251`), mirroring `internal/ai/service.go`.

Two ingest modes:

- **Batch** (`-wait`): poll Task phases until terminal, then patch. `markUnavailable`
  (`main.go:265`) reads `TaskPhase` to explain deadline/failure.
- **Event-driven** (`-serve`): a `webhookServer` (`main.go:335`) patches a single
  result as each Task fires its `webhookURL`, serialized so concurrent deliveries
  never corrupt a `jobs/*.json` file. `/status` exposes coverage via
  `skeletonStatus` (`main.go:114`).

Per-build Tool + Task garbage collection is `gcTools` (`main.go:284`), selecting
by the `orka.dashboard/build` label.

## How the harness is replicated

The engine's in-process analysis is more than "call the model in a loop": it has a
cache, convergence machinery, an always-on critique gate, cross-build pattern
correlation, and skill-driven evidence requirements. Each of those is
reconstructed out of Kubernetes objects and deterministic tool endpoints:

| Engine harness piece | Orka reconstruction | Where |
|---|---|---|
| On-disk analysis cache (keyed by mode+hash) | Content-addressed Task name; re-applying an existing Task is a no-op, so the K8s object store *is* the cache. Bump `-version` to force re-analysis. | `orka.TaskName` / `FailureHash`; `Apply` is idempotent |
| Per-failure build isolation (fetcher scopes each analysis to one build) | Per-build Tool clones with static `X-Build-Prefix` / `X-Bucket` headers; the model cannot read the wrong build. | `cloneToolForBuild` + shim `toolenv.go` |
| Prompt composition (BasePrompt + system.md + footer) | The producer calls the same `ai.ComposeSystemPrompt` and appends a tool-usage/self-critique addendum. | `main.go:117`, `toolUsageAddendum` |
| Convergence (loop always yields a final verdict) | Worker patches: forced tools-free finalization near the budget + re-prompt on an empty final message. | `worker-patches/` (2,3) |
| Critique gate: hallucinated-citation guard | `validate_analysis` tool deterministically 1-byte-reads every cited path against the build tree. | `orka-artifact-tool/validate.go` |
| Critique gate: transient discipline | Worker re-prompts an `is_transient=true` that never called `verify_timeline`, per the engine's confirm-or-default-to-bug contract. | `worker-patches/` (4) + `timeline.go` |
| Cross-build pattern correlation | `check_recurrence` tool correlates a failure across recent builds. | `orka-artifact-tool/recurrence.go` |
| Skill-driven required evidence | `required_evidence` tool returns the must-read artifacts for a failure class. | `orka-artifact-tool/requiredevidence.go` |
| Transient-signature background-noise filter | `check_transient_signatures` tool tails build logs for known-noise patterns. | `orka-artifact-tool/transient.go` |

The engine enforces these *in code*; Orka enforces them as *tools the agent must
call* plus *worker re-prompts*. The consequence, quantified in
[worker-patches/README.md](worker-patches/README.md): the tools/gates guarantee
the model **looks** (process parity), but on a weak model they do not guarantee it
**reasons correctly** (classification stays model-bound). That split is why the
recommendation keeps both paths rather than replacing the engine.

## Provider seam

An Orka `Provider` is just `type: openai` + a `baseURL` + a token secret
(`manifests/1x-*-provider.yaml`), so any OpenAI-compatible endpoint works with no
code change: in-cluster Kimi via Ray Serve (no proxy), vLLM, Dynamo/NIM, Ollama.
Copilot is the one exception; its non-streaming endpoint returns null tool_calls
for Claude, so `manifests/50-copilot-proxy.yaml` de-streams and injects the
`Copilot-Integration-Id` header the Provider cannot set itself.

## Consumer-driven, multi-consumer

The producer reads the tool, storage, and id fields from a consumer's
`project.yaml`: `ai.tools` (via `resolveTools`, `main.go:59`), the `storage`
block (`bucket` + `provider`/`base`/`prow_base`), and the display id. Tool
selection, bucket + provider routing, and the prompt all follow from those, so
the same binaries serve CAPZ (`kubernetes-ci-logs` on GCS, cluster-per-test,
`k8s` tools) and a project on an S3-backed Prow behind gcsweb (`filesystem` only)
unchanged. Every other `ai.*` knob (`max_iters`, `min_tool_calls`, `critique.*`,
`evidence.*`) is engine-only and inert on this path: it lives in the worker
patches and the shim tools, not in `project.yaml`.

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
   |                     + per-build, header-routed Tool clones
   |                     + bounded Task apply waves              -> kube-apply
   v
Orka ai-worker           runs the agentic loop per Task, calling the shim tools
   |
orka-ingestor            reads each Task's result, patches it into jobs/*.json,
                         then runs one job-level pattern Task for eligible jobs
   |
server / Pages           unchanged
```

Three of these are code in this repo (`backend/cmd/orka-producer`,
`orka-ingestor`, `orka-artifact-tool`); the ai-worker is built from a pinned
Orka commit plus the tested compatibility patch described in
[worker-patches/COMPATIBILITY.md](worker-patches/COMPATIBILITY.md).

## Where Orka resources are created

Everything the producer emits is an unstructured `map[string]any` built in Go and
server-side applied through a dynamic client, so the pipeline links no Orka Go
types. The producer can apply per-test Tasks in bounded waves, waiting for each
intermediate wave to become terminal before submitting the next. Task placement
is emitted under `spec.execution` for both per-test and pattern Tasks. Placement
is excluded from semantic identity so successful results remain cached. When
placement changes, non-successful Tasks are deleted and recreated only after
Orka's finalizer clears their worker, result, and event state. The two CRDs it
creates:

### Task (one per failing test)

Built by `orka.BuildAITask` (`internal/orka/task.go`). Shape:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: az-analysis-<identityHash>              # content address
  labels:
    app.kubernetes.io/managed-by: orka-producer
    orka.dashboard/build: <build + analysis-contract Tool scope hash>
spec:
  type: ai
  timeout: <ai.timeout>
  retryPolicy: { maxRetries: <n> }             # when -retries > 0
  webhookURL: <ingestor>                        # when -webhook-url set
  ai:
    providerRef: { name: <provider> }
    model: <model>
    tools: [<tool-b<toolScope>>, ...]           # contract-versioned Tool clones
    systemPrompt: <composed engine prompt + tool addendum>
    prompt: <per-failure user prompt>
```

The producer walks the jobs in the current `dashboard.json`. For every
`status: "failed"` test case it hashes the project/storage scope, job/build
scope, exact test index and rendered prompt, plus the provider, model, timeout,
retry, composed system prompt, selected Tool definitions, and manual version.
This prevents cross-consumer and same-build shard collisions and automatically
invalidates results when the model-visible analysis contract changes.

### Tool (one clone per project x job x build x analysis contract x tool group)

A release-scoped artifact Tool shim serves every build and bucket. The producer
clones content and quality Tool CRDs once per distinct scoped build and analysis
contract via `cloneToolForBuild`. It creates a task-specific validation Tool so
the final attestation cannot be replayed across failures. Every clone receives
static routing headers:

```yaml
spec:
  http:
    headers:
      X-Build-Prefix: <bucket-relative build dir>
      X-Prow-AI-Scope: <contract-scoped Tool identity>
      X-Bucket: <bucket>
      X-Storage-Provider: <gcs | gcsweb>   # from the consumer's project.yaml
      X-Storage-Base: <gcsweb gateway root>  # only for gcsweb (e.g. S3)
```

The shim requires an Orka-injected bearer token and resolves its backend per
request from producer-owned headers (`orka-artifact-tool/toolenv.go`), so a tool
always reads the Task's own build
in the right bucket **and on the right storage provider** regardless of what the
model passes. Storage is provider-agnostic: the shim reuses the engine's
`internal/storage` (gcs, or gcsweb over an S3 gateway), defaulting from its
`STORAGE_*` env and overriding per request from the `X-Storage-*` headers the
producer derives from `project.yaml`. Invalid explicit routes fail closed.
Contract-scoped entries use bounded LRU eviction and bounded tool-result caches.
The long-running shim disables each Browser's internal file cache. Each Tool call
has explicit model-response and artifact-read ceilings, including storage reads
performed directly by cross-build quality tools. Responses are buffered and
rejected before headers are committed when they exceed the model-byte ceiling.
For Helm deployments, synchronized base Tool definitions are packaged under the
chart's `files/orka-tools/` directory and rendered into a release ConfigMap. Raw
deployments load the source copies from `experimental/orka/manifests/`.

### The apply

`applyAll` applies Tools before Tasks (Tasks reference them),
using `KubeClient.Apply` (`internal/orka/kube.go`), a server-side-apply Patch
with `FieldManager: orka-producer` and `Force: true`. `RESTConfig`
(`kube.go`) prefers in-cluster config and falls back to kubeconfig + a
`-context` override for local runs. Running without `-apply` only writes the YAML
to `-tasks-out` / `-tools-out` for inspection.

## How the result comes back

The producer writes a private `orka_analysis.json` manifest beside the dashboard
data. It records the project scope, analysis-contract hash, active jobs, and
per-build routing and Tool scopes. `orka-ingestor` loads that manifest and re-derives
each exact Task name from the same job/test index and shared prompt renderer,
then patches the result in place. `applyResult` fetches the
Task's result, parses the analysis JSON, and writes `tc.AISummary` +
`tc.AIAnalysis` with `Mode: "agentic"`, the same wire shape the in-process path
produces. Each analysis stores the contract hash, so a cached result is reused
only while it matches the current producer manifest. The ingestor also reads the
Task's durable execution-event stream. It rejects incomplete response schemas,
analyses below `ai.min_tool_calls` or `ai.min_gcs_bytes`, results without a successful terminal Task
event, required quality tools whose last attempt failed, results without a completed
`submit_analysis` call whose token binds the exact final result and verifies
scoped evidence tokens from successful artifact content reads, and transient
verdicts without a successful `verify_timeline` call. Quality-tool error paths
return non-success HTTP statuses so Orka records failed calls rather than
successful error payloads. The producer keeps a private validation key in the
non-published analysis manifest, fingerprints its hash in Task identity, and
creates one submit Tool per analysis Task, injects the key and expected Task name only into its hidden HTTP headers, and has the ingestor verify both the Task identity and final result in the HMAC token. Accepted results carry Tool/model failures, retries,
context truncations, elapsed time, tokens, stop reason, and quality-tool
telemetry. Failing/absent results get the engine's `unavailable`
placeholder via
`setUnavailable`, mirroring `internal/ai/service.go`.

Two ingest modes:

- **Batch** (`-wait`): poll Task phases until terminal, then patch. `markUnavailable`
  reads `TaskPhase` to explain deadline/failure.
- **Event-driven** (`-serve`): a `webhookServer` patches a single
  result as each Task fires its `webhookURL`, serialized so concurrent deliveries
  never corrupt a `jobs/*.json` file. `/status` exposes coverage via
  `skeletonStatus`.

The skeleton fetch explicitly skips side effects. Job-level pattern finalization
runs in batch mode after the per-test wait completes. A successful finalization
then runs the shared notification, issue, and fix-PR reconciliation stage against
the finalized files under the same finalization deadline. When pattern analysis
is disabled, batch mode still runs the shared side effects directly. Finalization,
cluster setup, and side-effect errors make the batch exit non-zero. The optional
webhook receiver patches per-test results only.

The ingestor's immediate `gcTools` pass deletes scoped Tools after every Task
for a build becomes terminal. The producer also labels per-test Tasks, pattern
Tasks, and Tools with `orka.dashboard/project`; per-test resources carry
`orka.dashboard/build`, and Tasks carry `orka.dashboard/task-type`. The operator
helper uses those labels for project and batch status plus an age-bounded,
read-only garbage-collection preview.

After per-test ingestion, the ingestor runs the same bounded cross-build pattern
contract as the in-process backend. It applies one content-addressed, tool-free
AI Task per eligible job, ingests its `PatternAnalysis`, assigns stable pattern
IDs, and folds systemic verdicts into `flakiness.json`. This makes recurring
patterns available to the existing dashboard and interactive actions. Because
the correlation Task has no source-repository tools, any file path it introduces
in `suggested_fix` is marked as unverified. Its Task identity fingerprints the
project scope, provider/model, timeout/retry policy, prompts, and manual version.

## How the harness is replicated

The engine's in-process analysis is more than "call the model in a loop": it has a
cache, convergence machinery, an always-on critique gate, cross-build pattern
correlation, and skill-driven evidence requirements. Each of those is
reconstructed out of Kubernetes objects and deterministic tool endpoints:

| Engine harness piece | Orka reconstruction | Where |
|---|---|---|
| On-disk analysis cache (keyed by mode+hash) | The Task name fingerprints project/build/test identity plus prompt, provider/model, timeout/retry, and Tool definitions. Re-applying it is a no-op, so the K8s object store is the cache. `-version` remains a manual override for external semantic changes. | `AnalysisTaskName` / `AnalysisContractHash`; `Apply` is idempotent |
| Per-failure build isolation (fetcher scopes each analysis to one build) | Contract-versioned Tool clones with static `X-Build-Prefix` / `X-Bucket` headers; old and new Task contracts cannot share mutable Tool objects. | `ToolScopeID`, `cloneToolForBuild`, shim `toolenv.go` |
| Prompt composition (BasePrompt + system.md + footer) | The producer calls the same `ai.ComposeSystemPrompt` and appends a tool-usage/self-critique addendum. | `toolUsageAddendum` |
| Convergence (loop always yields a final verdict) | Worker patches: forced tools-free finalization near the budget + re-prompt on an empty final message. | `worker-patches/` (2,3) |
| Critique gate: hallucinated-citation guard | Successful `read_artifact`, `tail_artifact`, and `grep_artifact` calls return scoped HMAC evidence tokens. `submit_analysis` requires those tokens for every cited path and matching recipe group, pruning groups absent from a complete 5,000-path build listing. Its final validation token is keyed and bound to the current Task so the model cannot recompute it for a changed answer or replay it across failures. | `orka-artifact-tool/evidence.go`, `validate.go` |
| Critique gate: transient discipline | The worker re-prompts an unsupported transient verdict, and the ingestor independently rejects any final transient result without a completed `verify_timeline` event. | `worker-patches/` (4), `timeline.go`, ingestor event acceptance |
| Investigation floor | The producer fingerprints `ai.min_tool_calls`; the ingestor counts `ToolCallStarted` events and rejects shallower results. | `AnalysisManifest.MinToolCalls`, Task events API |
| Per-test recurrence evidence | `recurrence` reports whether one test recurs across recent builds. | `orka-artifact-tool/recurrence.go` |
| Job-level cross-build correlation | After per-test ingestion, one content-addressed pattern Task correlates representative failures and writes `PatternAnalysis` + recurring patterns. | `orka-ingestor` + `orka.FinalizePatterns` |
| Skill-driven required evidence | The producer compiles consumer `skills/*.yaml` recipes into the scoped `required_evidence` Tool; the ingestor requires that lookup when recipes exist. | `ai/skills`, `orka-producer`, `orka-artifact-tool/requiredevidence.go` |
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
`project.yaml`: `ai.tools` and `ai.min_tool_calls` (via `resolveTools` and the
acceptance manifest), the `storage`
block (`bucket` + `provider`/`base`/`prow_base`), and the display id. Tool
selection, bucket + provider routing, and the prompt all follow from those, so
the same binaries serve CAPZ (`kubernetes-ci-logs` on GCS, cluster-per-test,
`k8s` tools) and a project on an S3-backed Prow behind gcsweb (`filesystem` only)
unchanged. Every other `ai.*` knob (`max_iters`, `min_gcs_bytes`, `critique.*`,
`evidence.*`) is engine-only and inert on this path: it lives in the worker
patches and the shim tools, not in `project.yaml`.

# Quickstart: run the Orka backend preview

The Orka path runs the dashboard's AI failure analysis as Kubernetes-native Orka
Tasks instead of the engine's in-process agentic loop, so analysis runs alongside
your inference stack in the cluster. Discovery and output are unchanged: the
fetcher still writes the dashboard skeleton and the server still serves it; only
the per-failure analysis step moves to Orka.

```
fetcher -ai=false   ->  dashboard.json + jobs/*.json   (skeleton, no AI)
orka-producer       ->  one content-addressed Orka Task per failing test
                        + per-build, header-routed Tool CRDs
Orka ai-worker      ->  runs the agentic loop per Task, calling the shim tools
orka-ingestor       ->  patches each Task's result into jobs/*.json
server              ->  unchanged
```

> Opt-in path: the engine defaults to in-process analysis. Select this with
> `analysis: orka` in the Helm chart. See [ARCHITECTURE.md](ARCHITECTURE.md) for
> how it works and the README "Headline finding" for the evaluation.

## Prerequisites

- A Kubernetes cluster. A local `kind` cluster works for evaluation; a cluster
  co-located with your model is the intended production target.
- `kubectl`, `helm`, and `git`.
- A consumer directory holding `project.yaml` and `prompts/system.md` (the same
  files the Pages path uses).
- A model endpoint and its credentials: any OpenAI-compatible endpoint, or GitHub
  Copilot.
- All commands below assume the `orka-system` namespace. Adjust if you use
  another.

Run every step from the root of this engine repo.

## Container images

The pipeline images are published to GHCR alongside the engine image by
`.github/workflows/image.yml`, tagged `:main`, `:sha-<short>`, and semver on
release:

```
ghcr.io/willie-yao/prow-ai-dashboard/orka-producer:main
ghcr.io/willie-yao/prow-ai-dashboard/orka-ingestor:main
ghcr.io/willie-yao/prow-ai-dashboard/orka-artifact-tool:main
ghcr.io/willie-yao/prow-ai-dashboard/orka-copilot-proxy:main
```

The Helm chart uses the specialized repositories above and inherits the engine
`image.tag`, so one SHA or release tag pins fetcher, producer, ingestor, and
artifact tool together. The standalone manifests use `:main`. Ensure the GHCR
packages are pullable from your cluster: make them public, use the chart's
`imagePullSecrets` for pipeline images, and use
`orka.artifactTool.imagePullSecrets` for the cross-namespace artifact Tool.

The Orka AI worker is published separately because it is built from pinned Orka
source rather than this repository's Go module:

```text
ghcr.io/willie-yao/prow-ai-dashboard/orka-ai-worker:
  v4-orka-<full-orka-commit>-dashboard-<full-dashboard-commit>
```

Use the exact tag or digest from the latest [successful main-branch
compatibility run](https://github.com/willie-yao/prow-ai-dashboard/actions/workflows/orka-compat-image.yml?query=branch%3Amain+is%3Asuccess); the
[compatibility matrix](worker-patches/COMPATIBILITY.md) explains the artifact and
deployment formats. New GHCR packages may need to be made public before Orka can
pull the dynamic worker. To build locally instead, see
[Local build](#local-build-for-kind).

## Step 1: install Orka and pin the compatibility worker

Use the Orka source commit and compatibility worker recorded in
[worker-patches/COMPATIBILITY.md](worker-patches/COMPATIBILITY.md). The workflow
publishes a tag containing both full commits and records the registry digest.
Create an Orka values file with that exact tag:

```yaml
workers:
  ai:
    image:
      repository: ghcr.io/willie-yao/prow-ai-dashboard/orka-ai-worker
      tag: v4-orka-1b6f6f74c8cdf5e3ccfe92d0a7ed03a571670254-dashboard-<dashboard-commit>
      pullPolicy: IfNotPresent
```

Install the Orka control plane from the pinned source commit. Current chart
revisions do not yet package their generated CRDs and may omit RBAC for newer
controllers, so install the CRDs and scoped compatibility RBAC before Helm.
Remove these manual steps after the corresponding upstream fixes land:

```bash
# In your Orka checkout:
git checkout 1b6f6f74c8cdf5e3ccfe92d0a7ed03a571670254
kubectl create namespace orka-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f config/crd/bases/

# In this dashboard checkout:
kubectl apply -f experimental/orka/manifests/00-rbac.yaml

# Back in the pinned Orka checkout:
helm upgrade --install orka charts/orka \
  --namespace orka-system \
  -f /path/to/orka-worker-values.yaml
```

For strict digest pinning and local build commands, see the compatibility
matrix. The image includes the convergence and transient-discipline behavior
smaller models need.

## Step 2: configure a model Provider

Pick one path.

### OpenAI or an OpenAI-compatible endpoint

No proxy is needed. Create the token Secret, point the Provider at the endpoint's
`/v1` base, and apply it. The pinned worker tries Responses first and falls back
to Chat Completions when the endpoint does not support Responses. It sends
`store: false` on Responses requests.

```bash
kubectl create secret generic model-secret -n orka-system \
  --from-literal=api-key=<TOKEN>            # any non-empty value if the endpoint needs none

# Edit manifests/12-kimi-provider.yaml: set spec.baseURL to your endpoint (must
# end in /v1), spec.defaultModel to your model id, and spec.secretRef.name to
# model-secret. Then:
kubectl apply -f experimental/orka/manifests/12-kimi-provider.yaml
```

### GitHub Copilot

Copilot's non-streaming endpoint returns null tool calls for Claude, so it needs
the de-streaming proxy. The proxy also normalizes bare `/responses` errors so a
model reported as `model_not_supported` can fall back to Chat Completions. Create
the Secret from a `copilot_chat` PAT, deploy the proxy, then the Provider:

```bash
kubectl create secret generic copilot-secret -n orka-system \
  --from-literal=api-key=<COPILOT_CHAT_PAT>
kubectl apply -f experimental/orka/manifests/50-copilot-proxy.yaml
kubectl apply -f experimental/orka/manifests/11-copilot-provider.yaml
```

Validate the installed CRDs, controller, API Service, Provider, and configured
compatibility worker before installing the dashboard. Use the exact image
reference from the compatibility workflow summary:

```bash
WORKER_IMAGE='ghcr.io/willie-yao/prow-ai-dashboard/orka-ai-worker:<immutable-compatibility-tag>'

experimental/orka/orka-ops.sh --namespace orka-system preflight \
  --provider copilot \
  --worker-image "$WORKER_IMAGE"

# Exercise the complete controller, worker, and Provider path. The Task is
# deleted after a successful result unless --keep is passed.
# A current Claude model exercises Copilot's Chat Completions fallback.
experimental/orka/orka-ops.sh --namespace orka-system smoke \
  --provider copilot \
  --model claude-sonnet-4.6 \
  --expect-api chat_completions

# If your plan exposes a Responses-capable model, verify that path separately.
experimental/orka/orka-ops.sh --namespace orka-system smoke \
  --provider copilot \
  --model gpt-5.4-mini \
  --expect-api responses
```

For a non-Copilot Provider, substitute its name and model. Use
`--expect-api responses` for an OpenAI Provider when Responses is required, or
leave the default `auto` to accept either API. The smoke command requires
`status.phase=Succeeded`, an available result, and worker API-mode telemetry. A
controller that is merely running is not enough. The command does not call the
dashboard artifact Tools. The first complete dashboard pipeline Job remains the
end-to-end check for artifact routing, Tool authentication, result acceptance,
and ingestion.

## Step 3: choose artifact Tool ownership

The shim exposes the engine's artifact tools over HTTP for the Orka Tasks to call.
The dashboard Helm chart creates a release-scoped authenticated shim, Service,
NetworkPolicy, Secret, and base Tool ConfigMap by default. Configure placement
and resources under `orka.artifactTool`; no manual resources are required for
the normal Helm path.

For a non-Helm deployment, or when intentionally sharing one external shim,
create the bearer Secret and apply the standalone manifest:

```bash
kubectl create secret generic artifact-tool-auth -n orka-system \
  --from-literal=token="$(openssl rand -hex 32)"
kubectl apply -f experimental/orka/manifests/20-artifact-tool.yaml
```

The producer adds `authSecretRef` to every scoped Tool, so Orka injects the same
bearer token that the shim requires. The included NetworkPolicy admits only Orka
AI-worker pods. Requests cannot select a bucket or build through model arguments;
only producer-owned headers control routing.

The shim is storage-provider agnostic. It defaults to GCS; for an S3 bucket behind
a gcsweb gateway, set `STORAGE_PROVIDER: gcsweb` and `STORAGE_BASE` in
`20-artifact-tool.yaml`, or rely on the `X-Storage-*` headers the producer derives
from `project.yaml`.

The base Tool CRDs under `manifests/30-*` through `manifests/41-*` are clone
templates, not cluster resources. Helm packages synchronized copies and writes
them to the producer ConfigMap in the dashboard release namespace.

## Step 4: deploy the pipeline with Helm

The `deploy/helm/prow-ai-dashboard` chart runs the whole flow when
`analysis: orka`, which requires `mode: cron`. The fetcher CronJob becomes
fetch(`-ai=false`) then orka-producer then orka-ingestor, and the chart creates
the pipeline RBAC.

The chart deploys the per-dashboard pipeline and artifact Tool service, not the
Orka control plane. Install Orka and the Provider first, then install the
dashboard:

```bash
helm install dash deploy/helm/prow-ai-dashboard \
  --namespace dashboards --create-namespace \
  --set mode=cron --set analysis=orka \
  --set orka.provider=copilot --set orka.model=claude-sonnet-4.6 \
  --set orka.apiMode=chat_completions \
  --set-file project.config=<consumer>/project.yaml \
  --set-file project.systemPrompt=<consumer>/prompts/system.md
```

After the release exists, repeat the preflight with its pipeline ServiceAccount.
This verifies the cross-namespace Task and Tool permissions that Helm created:

```bash
experimental/orka/orka-ops.sh --namespace orka-system preflight \
  --provider copilot \
  --worker-image "$WORKER_IMAGE" \
  --service-account dashboards/dash-prow-ai-dashboard-orka
```

If `fullnameOverride` or `orka.rbac.serviceAccountName` changes the generated
name, pass the actual ServiceAccount shown by
`kubectl get serviceaccounts -n dashboards`.

On heterogeneous clusters, configure the dashboard components and dynamic Orka
workers independently. `orka.artifactTool.nodeSelector` places the shim, while
`orka.taskExecution` is copied to every per-test and pattern Task:

```yaml
orka:
  artifactTool:
    nodeSelector:
      agentpool: cpu
  producer:
    maxConcurrentTasks: 2
  taskExecution:
    nodeSelector:
      agentpool: cpu
    tolerations:
      - key: dedicated
        operator: Equal
        value: orka
        effect: NoSchedule
    affinity: {}
```

Do not copy these selectors or tolerations onto clusters that do not define the
matching labels and taints. `fetcher.nodeSelector` separately places the
producer and ingestor CronJob pod.

`maxConcurrentTasks` accepts `0` through `1000` and limits per-test submissions
from one producer invocation.
The producer applies a wave, waits for its Tasks to become terminal, and then
applies the next wave. It does not cap unrelated Tasks already running in the
namespace, and job-level pattern Tasks still start concurrently during
finalization. Set it to `0` to restore immediate submission of every per-test
Task.

Placement is not part of semantic Task identity. A placement change keeps a
successful Task and its cached result. If an existing per-test or pattern Task is
pending, running, failed, or cancelled under different placement, the pipeline
deletes it, waits for Orka's finalizer to clear the old worker, result, and event
state, and recreates the same Task name with the new placement.

Producer, ingestor, and artifact-tool tags inherit the engine `image.tag`, so one
immutable SHA pins the complete dashboard pipeline. Set `orka.provider` to your
Provider name and `orka.model` to your model id. Set `orka.apiMode` to the
required protocol when a fallback would hide a deployment mistake. See the chart
values for external artifact Tool and existing ConfigMap overrides.

For an evaluation, also set `fetcher.suspend=true` and
`orka.sideEffects.enabled=false`, then trigger one uniquely named Job after the
release is ready. A new PVC clears dashboard data and generates a new private
validation key, so per-test Tasks are cold. An exact matching pattern Task may
still be reused while `orka.version` is unchanged. Bump the version when pattern
Tasks must also be cold. The complete blue-green procedure is in
[Evaluating Orka safely](EVALUATION.md).

`analysis: inprocess`, the default, is unchanged: the engine runs the in-process
loop and none of the Orka path is deployed.

## Step 5: run it and view the dashboard

```bash
# Trigger a run now instead of waiting for the schedule.
kubectl create job -n dashboards --from=cronjob/dash-prow-ai-dashboard-fetcher orka-run-1

# Watch the steps: fetch skeleton, produce Tasks, ingest results.
kubectl logs -n dashboards job/orka-run-1 -c produce
kubectl logs -n dashboards job/orka-run-1 -c ingest | tail

# Per-project and per-build Task phase and Tool counts.
experimental/orka/orka-ops.sh --namespace orka-system status
```

The server serves the dashboard from the shared volume at
`dash-prow-ai-dashboard-server`. Port-forward it, or expose it via the chart's
`ingress` values:

```bash
kubectl port-forward -n dashboards svc/dash-prow-ai-dashboard-server 8080:80
# open http://localhost:8080
```

## Alternative deploys

### Local run against kind

`experimental/orka/run-demo.sh <consumer-dir>` runs the whole flow against a local
kind cluster with locally built binaries: fetcher(`-ai=false`) then producer then
apply then poll then ingestor. It writes a renderable data dir and prints how to
serve it. Use it to evaluate without a Helm install.

### Raw manifests

Instead of Helm, apply the pipeline manifests directly. This expects a
`ReadWriteMany` PVC named `prow-ai-dashboard-data` shared with the server, plus the
base-tools and consumer-config ConfigMaps:

```bash
# Pipeline RBAC: an SA that can create/patch Tasks and Tools, read status, GC Tools.
kubectl apply -f experimental/orka/manifests/60-pipeline-rbac.yaml

# Base Tool CRDs the producer clones per build (non-Tool docs are ignored).
kubectl create configmap orka-base-tools -n orka-system \
  --from-file=experimental/orka/manifests/

# Consumer config: project.yaml at the root, system.md under prompts/.
kubectl create configmap orka-consumer-config -n orka-system \
  --from-file=project.yaml=<consumer>/project.yaml \
  --from-file=system.md=<consumer>/prompts/system.md

# The analysis CronJob: fetch -> produce -> -wait ingest.
kubectl apply -f experimental/orka/manifests/70-pipeline-job.yaml

# Optional event-driven ingestion: apply the receiver, then uncomment -webhook-url
# in the 70 produce step so each Task notifies it on completion.
# The receiver patches per-test results only; the batch ingestor performs
# job-level pattern finalization.
kubectl apply -f experimental/orka/manifests/71-ingestor-webhook.yaml
```

The Orka pipeline is the sole writer to the data volume. Do not also run the engine
fetcher CronJob or watch worker against the same PVC.

### Local build (for kind)

The published GHCR images cover most clusters. For a local kind cluster or unmerged
changes, build the images and load them, then override the image references to the
local tags:

```bash
for cmd in orka-producer orka-ingestor orka-artifact-tool orka-copilot-proxy; do
  docker build -f experimental/orka/Dockerfile --build-arg CMD=$cmd -t $cmd:latest backend/
  kind load docker-image $cmd:latest --name <cluster>
done
```

Build the pinned patched Orka worker separately:

```bash
make orka-compat-image ORKA_COMPAT_IMAGE=orka-ai-worker:local
kind load docker-image orka-ai-worker:local --name <cluster>
```

Point the Orka controller at that loaded worker in the pinned Orka checkout:

```yaml
# /tmp/orka-local-worker-values.yaml
workers:
  ai:
    image:
      repository: orka-ai-worker
      tag: local
```

```bash
helm upgrade --install orka charts/orka \
  --namespace orka-system \
  -f /tmp/orka-local-worker-values.yaml
```

The pinned Orka controller creates dynamic AI-worker Jobs with
`imagePullPolicy: IfNotPresent`, so the kind-loaded `orka-ai-worker:local` image
is used without a registry pull. The chart's `workers.ai.image.pullPolicy` value
does not control those dynamic Jobs at this pinned revision.

For the dashboard Helm release, override its structured pipeline image values:

```bash
--set orka.producer.image.repository=orka-producer \
--set orka.producer.image.tag=latest \
--set orka.ingestor.image.repository=orka-ingestor \
--set orka.ingestor.image.tag=latest \
--set orka.artifactTool.image.repository=orka-artifact-tool \
--set orka.artifactTool.image.tag=latest
```

For the standalone manifests, edit the image fields in
`20-artifact-tool.yaml`, `50-copilot-proxy.yaml`, `70-pipeline-job.yaml`, and
`71-ingestor-webhook.yaml`.

## Configuration the Orka path reads

The Orka path reads the storage, tool, investigation-floor, and id fields from the consumer's
`project.yaml`:

| Field | Used for |
|---|---|
| `ai.tools` | which tool groups to enable (`filesystem`, `k8s`); default `[filesystem, k8s]` |
| `ai.min_tool_calls` | minimum recorded Orka tool calls before a result is accepted; default `2` |
| `ai.min_gcs_bytes` | minimum bytes of validated artifact evidence before a result is accepted; default `0` |
| `skills/*.yaml` | consumer evidence recipes exposed through `required_evidence` and enforced by `submit_analysis` |
| `storage.bucket` | the bucket, routed to the shim via the `X-Bucket` header |
| `storage.provider` / `storage.base` / `storage.web_base` / `storage.prow_base` | storage and link routing passed to the scoped artifact tools |
| `id` / `short_name` | the project label in the prompt |

The in-process loop settings `ai.concurrency`, `ai.timeout`, `ai.max_iters`,
`ai.single_tool_call`, and `ai.critique.*` do not configure Orka Tasks.
`prompts/system.md` is composed into the Task's system prompt exactly as in the
engine.

The equivalent Orka knobs are producer flags, surfaced as Helm `orka.*` values:

| Knob | Helm value | Producer flag | Default |
|---|---|---|---|
| model | `orka.model` | `-model` | `claude-sonnet-4.6` |
| provider | `orka.provider` | `-provider` | `copilot` |
| expected API | `orka.apiMode` | `-api-mode` | `auto` |
| per-Task timeout | `orka.taskTimeout` | `-timeout` | `10m` |
| retries | `orka.retries` | `-retries` | `1` |
| per-test wave size | `orka.producer.maxConcurrentTasks` | `-max-concurrent-tasks` | `2` |
| wave poll interval | `orka.producer.taskPoll` | `-task-poll` | `5s` |
| placement recovery and intermediate-wave deadline | `orka.producer.waveTimeout` | `-wave-timeout` | `30m` |
| worker placement | `orka.taskExecution` | `-task-execution` | empty |
| manual cache-bust version | `orka.version` | `-version` | `v1` |
| job-pattern finalization wait | `orka.patternWait` | ingestor `-pattern-wait` | `25m` |

Iteration budget, forced finalization, and the transient-critique gate live in the
pinned Orka compatibility worker, not in project config. See
[worker-patches/COMPATIBILITY.md](worker-patches/COMPATIBILITY.md).

## Operate

The operator helper reports both a project summary and one row per build-scoped
batch. The project value is the producer's collision-resistant project scope,
not the display name from `project.yaml`:

```bash
# Show every dashboard project sharing the Orka namespace.
experimental/orka/orka-ops.sh --namespace orka-system status

# Limit the report after copying one project scope from the first table.
experimental/orka/orka-ops.sh --namespace orka-system status \
  --project <32-character-project-scope>
```

New per-test Tasks, pattern Tasks, and scoped Tools carry the project label.
Resources produced before this labeling contract appear as `<unlabeled>` and are
never selected by project-scoped garbage collection.

The garbage-collection preview is age-bounded and read-only. It reports only
terminal Tasks for one exact project scope. A Tool is eligible only when it is
older than the retention window and no Task for its build is active. Duration
flags use one base-10 integer followed by `s`, `m`, `h`, or `d`; leading zeros
remain decimal, so `08h` is the same as `8h`.

```bash
# Preview the default 168-hour retention plan.
experimental/orka/orka-ops.sh --namespace orka-system gc \
  --project <32-character-project-scope>

# Use another retention window, still without deleting anything.
experimental/orka/orka-ops.sh --namespace orka-system gc \
  --project <32-character-project-scope> \
  --older-than 30d

```

The command never deletes resources. Its Task candidates are Kubernetes-backed
analysis cache entries, so deleting them through another process would force a
later producer run to perform the analysis again. Review the preview through the
cluster's normal change-control process before any manual deletion.

The ingestor still performs immediate per-build Tool cleanup after completed
batches. The preview identifies abandoned Tools and old Task cache entries when
a batch failed before that cleanup ran.

```bash
# Coverage: the ingestor logs failing-test coverage, then the number of
# job-level pattern analyses and systemic recurring patterns it finalized.
kubectl logs -n dashboards job/orka-run-1 -c ingest | tail

# Prompt, provider/model/API mode, timeout/retry, and Tool-definition changes create new
# content-addressed Tasks automatically. Bump orka.version (Helm) or -version
# (manifests) only for external semantic changes the producer cannot fingerprint,
# such as a shim implementation change. Re-applying an unchanged Task is a no-op.
```

## Troubleshoot

- **Results stay `pending`.** Tasks are not finishing: check `kubectl get tasks`
  for `Running`/`Failed` and the ai-worker logs. Confirm the pinned compatibility worker is
  configured.
- **`AI analysis unavailable: analysis Task ...`.** Honest surfacing of a
  `Failed`/`Cancelled`/missing Task, not a silent blank. Inspect that Task, fix the
  cause. Fix the dependency or bump the version when retrying an external
  semantic change.
- **`AI analysis unavailable: analysis Task telemetry unavailable`.** The
  ingestor requires Orka execution-event storage to enforce tool-call, API-mode,
  and timeline gates. Verify the controller event store and Task events API are
  enabled and reachable by the pipeline ServiceAccount.
- **`model request telemetry did not report an API mode`.** The Orka installation
  is not using the pinned compatibility v4 worker. Install the exact published
  image tag or digest before retrying.
- **`model requests used ... expected ...`.** The Provider negotiated a different
  API than `orka.apiMode`. Correct the expectation or endpoint. Changing the mode
  creates new content-addressed Tasks automatically.
- **`ImagePullBackOff` on an orka-* image.** The GHCR package is not pullable from
  the cluster. Make it public, configure top-level `imagePullSecrets` for the
  pipeline, or configure `orka.artifactTool.imagePullSecrets` for the artifact
  Tool. For local images, see [Local build](#local-build-for-kind).
- **Webhook never fires.** Orka's SSRF guard requires a same-namespace ClusterIP
  Service with a selector; `71-ingestor-webhook.yaml` satisfies it. The receiver
  version must match the producer's.
- **AI fields vanished after a run.** A fresh skeleton has no AI fields; the
  `-wait` ingest re-applies them from the result store. Keep exactly one writer to
  the PVC.
- **Copilot returns no tool calls.** Use the de-streaming proxy (Step 2). Standard
  OpenAI-compatible endpoints do not need it.

## Manifests reference

```
manifests/
  00-rbac.yaml              Orka SA access to core.orka.ai (chart gap).
  11-copilot-provider.yaml  GitHub Copilot Provider (via the proxy).
  12-kimi-provider.yaml     OpenAI-compatible Provider template (no proxy).
  20-artifact-tool.yaml     Standalone artifact Tool shim for non-Helm deployments.
  30-tools.yaml             Filesystem Tool CRD templates (list/find/grep/read/tail).
  35-k8s-tools.yaml         k8s discovery Tool CRD templates (CAPZ cluster navigation).
  36-41-*.yaml              Deterministic quality Tool CRD templates (validate/verify/...).
  50-copilot-proxy.yaml     De-streaming proxy for Copilot.
  60-pipeline-rbac.yaml     Pipeline SA + Role/RoleBinding.
  70-pipeline-job.yaml      The analysis CronJob (fetch -> produce -> ingest).
  71-ingestor-webhook.yaml  Event-driven ingestor Deployment + Service.
worker-patches/             Pinned worker build, compatibility matrix, patch, and tests.
```

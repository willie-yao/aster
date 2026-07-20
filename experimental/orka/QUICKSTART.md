# Quickstart: run the dashboard on the Orka backend

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
`orka.artifactTool.imagePullSecrets` for the cross-namespace artifact Tool. To
build locally instead, see [Local build](#local-build-for-kind).

## Step 1: install Orka and apply the worker patches

Install the Orka control plane per its own docs
([orka-agents/orka](https://github.com/orka-agents/orka)); its images are private,
so build them from source. Current upstream chart revisions do not yet package
their generated CRDs and may omit RBAC for newer controllers. Install the CRDs
and scoped compatibility RBAC before the Helm release. Remove these manual
steps after the corresponding upstream chart fixes land:

```bash
# In your Orka checkout:
kubectl create namespace orka-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f config/crd/bases/

# In this dashboard checkout:
kubectl apply -f experimental/orka/manifests/00-rbac.yaml

# Back in the Orka checkout:
helm install orka charts/orka --namespace orka-system
```

Then apply this repo's ai-worker patches. They add the convergence and
transient-discipline behavior smaller models need, and are required for the Orka
path to produce reliable results:

```bash
# In your Orka checkout:
git apply /path/to/prow-ai-dashboard/experimental/orka/worker-patches/ai-worker-convergence.patch
# Rebuild the ai-worker image per Orka's build docs, then load or push it so the
# cluster runs the patched worker.
```

See [worker-patches/README.md](worker-patches/README.md) for what the patches
change and the measured impact.

## Step 2: configure a model Provider

Pick one path.

### OpenAI-compatible endpoint (vLLM, Ray Serve, Dynamo/NIM, Ollama)

No proxy needed. Create the token Secret, point the Provider at your endpoint's
`/v1` base, and apply it:

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
the de-streaming proxy. Create the Secret from a `copilot_chat` PAT, deploy the
proxy, then the Provider:

```bash
kubectl create secret generic copilot-secret -n orka-system \
  --from-literal=api-key=<COPILOT_CHAT_PAT>
kubectl apply -f experimental/orka/manifests/50-copilot-proxy.yaml
kubectl apply -f experimental/orka/manifests/11-copilot-provider.yaml
```

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
  --set orka.provider=copilot --set orka.model=claude-sonnet-4.5 \
  --set-file project.config=<consumer>/project.yaml \
  --set-file project.systemPrompt=<consumer>/prompts/system.md
```

On heterogeneous clusters, place the artifact Tool on an appropriate CPU node
pool with a cluster-specific selector, for example:

```bash
--set orka.artifactTool.nodeSelector.agentpool=nodepool1
```

Do not copy this selector onto clusters that do not define that label.

Producer, ingestor, and artifact-tool tags inherit the engine `image.tag`, so one
immutable SHA pins the complete dashboard pipeline. Set `orka.provider` to your
Provider name and `orka.model` to your model id. See the chart values for
external artifact Tool and existing ConfigMap overrides.

For an evaluation, also set `fetcher.suspend=true` and
`orka.sideEffects.enabled=false`, then trigger one uniquely named Job after the
release is ready. A new PVC clears dashboard data and generates a new private
validation key, so per-test Tasks are cold. An exact matching pattern Task may
still be reused while `orka.version` is unchanged. Bump the version when pattern
Tasks must also be cold. The complete blue-green procedure is in
[Evaluating Orka safely](../../docs/orka-evaluation.md).

`analysis: inprocess`, the default, is unchanged: the engine runs the in-process
loop and none of the Orka path is deployed.

## Step 5: run it and view the dashboard

```bash
# Trigger a run now instead of waiting for the schedule.
kubectl create job -n dashboards --from=cronjob/dash-prow-ai-dashboard-fetcher orka-run-1

# Watch the steps: fetch skeleton, produce Tasks, ingest results.
kubectl logs -n dashboards job/orka-run-1 -c produce
kubectl logs -n dashboards job/orka-run-1 -c ingest | tail

# Per-Task state.
kubectl get tasks -n orka-system -l app.kubernetes.io/managed-by=orka-producer
```

The server serves the dashboard from the shared volume at
`dash-prow-ai-dashboard-server`. Port-forward it, or expose it via the chart's
`ingress` values:

```bash
kubectl port-forward -n orka-system svc/dash-prow-ai-dashboard-server 8080:80
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

For Helm, override the structured image values:

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
| `storage.bucket` | the bucket, routed to the shim via the `X-Bucket` header |
| `storage.provider` / `storage.base` / `storage.prow_base` | the storage backend (gcs or gcsweb/S3), routed via `X-Storage-*` headers so the shim reads the right provider |
| `id` / `short_name` | the project label in the prompt |

Everything else under `ai:` is engine-only and has no effect on the Orka path:
`ai.concurrency`, `ai.timeout`, `ai.max_iters`,
`ai.min_gcs_bytes`, `ai.single_tool_call`, and `ai.critique.*`.
`prompts/system.md` is
composed into the Task's system prompt exactly as in the engine.

The equivalent Orka knobs are producer flags, surfaced as Helm `orka.*` values:

| Knob | Helm value | Producer flag | Default |
|---|---|---|---|
| model | `orka.model` | `-model` | `claude-sonnet-4.5` |
| provider | `orka.provider` | `-provider` | `copilot` |
| per-Task timeout | `orka.taskTimeout` | `-timeout` | `10m` |
| retries | `orka.retries` | `-retries` | `1` |
| manual cache-bust version | `orka.version` | `-version` | `v1` |
| job-pattern finalization wait | `orka.patternWait` | ingestor `-pattern-wait` | `25m` |

Iteration budget, forced finalization, and the transient-critique gate live in the
Orka ai-worker (see [worker-patches/](worker-patches/)), not in config.

## Operate

```bash
# Coverage: the ingestor logs failing-test coverage, then the number of
# job-level pattern analyses and systemic recurring patterns it finalized.
kubectl logs -n orka-system job/orka-run-1 -c ingest | tail

# Prompt, provider/model, timeout/retry, and Tool-definition changes create new
# content-addressed Tasks automatically. Bump orka.version (Helm) or -version
# (manifests) only for external semantic changes the producer cannot fingerprint,
# such as a shim implementation change. Re-applying an unchanged Task is a no-op.
```

## Troubleshoot

- **Results stay `pending`.** Tasks are not finishing: check `kubectl get tasks`
  for `Running`/`Failed` and the ai-worker logs. Confirm the worker patches are
  applied.
- **`AI analysis unavailable: analysis Task ...`.** Honest surfacing of a
  `Failed`/`Cancelled`/missing Task, not a silent blank. Inspect that Task, fix the
  cause. Fix the dependency or bump the version when retrying an external
  semantic change.
- **`AI analysis unavailable: analysis Task telemetry unavailable`.** The
  ingestor requires Orka execution-event storage to enforce tool-call and
  timeline gates. Verify the controller event store and Task events API are
  enabled and reachable by the pipeline ServiceAccount.
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
worker-patches/             Required Orka ai-worker changes + why.
```

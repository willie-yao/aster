# Using the Orka analysis backend

The Orka path runs the dashboard's AI failure analysis as Kubernetes-native Orka
Tasks instead of the engine's in-process agentic loop, so analysis runs alongside
your inference stack in the cluster. Discovery and output are unchanged: the
fetcher still writes the dashboard skeleton and the server still serves it; only
the per-failure analysis step moves to Orka.

Pipeline:

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

## What you provide vs what Orka provides

You bring: a consumer `project.yaml` + `prompts/system.md`, a bucket of Prow
builds (GCS, or an S3 bucket behind a gcsweb gateway), and an OpenAI-compatible
model endpoint (a `Provider`). Orka provides the Task CRD, the agentic worker
loop, retries, and the result store.

## Configuration the Orka path reads

The Orka path reads only the storage + tool + id fields from the consumer's
`project.yaml`:

| Field | Used for |
|---|---|
| `ai.tools` | which tool groups to enable (`filesystem`, `k8s`); default `[filesystem, k8s]` |
| `storage.bucket` | the bucket, routed to the shim via the `X-Bucket` header |
| `storage.provider` / `storage.base` / `storage.prow_base` | the storage backend (gcs or gcsweb/S3), routed via `X-Storage-*` headers so the shim reads the right provider |
| `id` / `short_name` | the project label in the prompt |

Everything else under `ai:` is ENGINE-ONLY and has NO effect on the Orka path:
`ai.concurrency`, `ai.timeout`, `ai.max_iters`, `ai.min_tool_calls`,
`ai.min_gcs_bytes`, `ai.critique.*`, `ai.evidence.*`. The equivalent Orka knobs are
producer flags and the worker (below). `prompts/system.md` is used verbatim (it is
composed into the Task's system prompt exactly as in the engine).

### Orka knobs (producer / ingestor flags)

| Knob | Where | Default |
|---|---|---|
| model | `orka-producer -model` | `claude-sonnet-4.5` |
| provider | `orka-producer -provider` | `copilot` |
| per-Task timeout | `orka-producer -timeout` | `10m` |
| retries | `orka-producer -retries` (Task `retryPolicy.maxRetries`) | `1` |
| re-analysis version | `orka-producer -version` (bump to force re-analysis) | `v1` |
| bucket override | `orka-producer -bucket` | consumer `storage.bucket` |
| tool override | `orka-producer -tools` | derived from `ai.tools` |

Iteration budget, forced finalization, and the transient-critique gate live in the
Orka ai-worker (see `worker-patches/`), not in config.

## Container images

The pipeline's images (`orka-producer`, `orka-ingestor`, `orka-artifact-tool`,
`orka-copilot-proxy`) are published to GHCR alongside the engine image by
`.github/workflows/image.yml`, tagged the same way (`:main`, `:sha-<short>`, and
semver on release):

```
ghcr.io/willie-yao/prow-ai-dashboard/orka-producer:main
ghcr.io/willie-yao/prow-ai-dashboard/orka-ingestor:main
ghcr.io/willie-yao/prow-ai-dashboard/orka-artifact-tool:main
ghcr.io/willie-yao/prow-ai-dashboard/orka-copilot-proxy:main
```

Reference these directly (via `--set orka.producerImage=...` for Helm, or the
`image:` field in the manifests) to skip the local `docker build` steps below.
Building locally with `docker build --build-arg CMD=<cmd>` + `kind load` remains
the path for a local kind cluster or unmerged changes.

## One-time cluster setup

1. **Orka.** Install the Orka control plane (Orka's images are private; build from
   source). Apply the RBAC gap fix + your `Provider` (see `manifests/00-rbac.yaml`,
   `manifests/1x-*-provider.yaml`).
2. **Worker patches.** Apply `worker-patches/ai-worker-convergence.patch` to Orka's
   worker, rebuild the ai-worker image, and load it. These are required for
   reliable convergence and transient discipline (especially on smaller models).
3. **Provider.** Point a `Provider` at your model endpoint:
   - Any standard OpenAI-compatible endpoint (vLLM, Ray Serve, Dynamo/NIM, Ollama):
     `manifests/12-kimi-provider.yaml` is the template - just `baseURL` + a token
     Secret, no proxy.
   - GitHub Copilot: needs the de-streaming proxy (`manifests/50-copilot-proxy.yaml`
     + `manifests/11-copilot-provider.yaml`); Copilot is the only endpoint that
     needs it.
4. **Tool shim.** Build and deploy the multi-bucket artifact tool shim:
   ```bash
   docker build -f experimental/orka/Dockerfile --build-arg CMD=orka-artifact-tool \
     -t orka-artifact-tool:latest backend/
   kind load docker-image orka-artifact-tool:latest --name orka-spike
   kubectl apply -f experimental/orka/manifests/20-artifact-tool.yaml
   ```
   One shim serves every bucket and build (routed per request by the `X-Bucket` /
   `X-Build-Prefix` headers the producer stamps).

## Deploy the pipeline

Two options: the Helm chart's `analysis: orka` mode (recommended), or the raw
manifests.

### Via the Helm chart (recommended)

The `deploy/helm/prow-ai-dashboard` chart runs the whole flow when
`analysis: orka` (requires `mode: cron`). The fetcher CronJob becomes
fetch(-ai=false) -> orka-producer -> orka-ingestor, and the chart creates the
pipeline RBAC. Prerequisites (the chart deploys the pipeline, not Orka): Orka + the
tool shim + a Provider + the ai-worker patches installed in the Orka namespace, and
the base Tool CRDs as a ConfigMap. Then:

```bash
kubectl create configmap orka-base-tools -n orka-system \
  --from-file=experimental/orka/manifests/

helm install dash deploy/helm/prow-ai-dashboard \
  --set mode=cron --set analysis=orka \
  --set orka.provider=copilot --set orka.model=claude-sonnet-4.5 \
  --set orka.producerImage=orka-producer:latest \
  --set orka.ingestorImage=orka-ingestor:latest \
  --set-file project.config=<consumer>/project.yaml \
  --set-file project.systemPrompt=<consumer>/prompts/system.md
```

`analysis: inprocess` (the default) is unchanged: the engine runs the in-process
loop and the Orka path is not deployed. See `deploy/helm/prow-ai-dashboard/values.yaml`
for the full `orka:` block (namespace, apiBase, version, timeouts, RBAC).

### Via the raw manifests


docker build -f experimental/orka/Dockerfile --build-arg CMD=orka-producer -t orka-producer:latest backend/
docker build -f experimental/orka/Dockerfile --build-arg CMD=orka-ingestor -t orka-ingestor:latest backend/
kind load docker-image orka-producer:latest orka-ingestor:latest --name orka-spike

# Pipeline RBAC (SA that can create/patch Tasks+Tools, read status, GC Tools).
kubectl apply -f experimental/orka/manifests/60-pipeline-rbac.yaml

# Base Tool CRDs the producer clones per build (non-Tool docs are ignored).
kubectl create configmap orka-base-tools -n orka-system \
  --from-file=experimental/orka/manifests/

# Consumer config: project.yaml at the root, system.md under prompts/.
kubectl create configmap orka-consumer-config -n orka-system \
  --from-file=project.yaml=<consumer>/project.yaml \
  --from-file=system.md=<consumer>/prompts/system.md

# A ReadWriteMany PVC named prow-ai-dashboard-data, shared with the server, then
# the CronJob (fetch -> produce -> -wait ingest).
kubectl apply -f experimental/orka/manifests/70-pipeline-job.yaml

# Optional: event-driven ingestion. Apply the receiver, then uncomment
# -webhook-url in the 70 produce step so each Task notifies it on completion.
kubectl apply -f experimental/orka/manifests/71-ingestor-webhook.yaml
```

The Orka pipeline is the SOLE writer to the data volume: do not also run the engine
fetcher CronJob or watch worker against the same PVC.

### Local run (no in-cluster deploy)

`experimental/orka/run-demo.sh <consumer-dir>` runs the whole flow against a local
kind cluster: fetcher(-ai=false) -> producer -> apply -> poll -> ingestor.

## Operate

```bash
# Trigger a run now.
kubectl create job -n orka-system --from=cronjob/orka-pipeline orka-run-1

# Coverage: the ingestor logs "📊 N failing: analyzed/unavailable/pending", and the
# -serve receiver exposes it as JSON.
kubectl logs -n orka-system job/orka-run-1 -c ingest | tail
kubectl exec -n orka-system deploy/orka-ingestor -- wget -qO- localhost:8080/status

# Per-Task state.
kubectl get tasks -n orka-system -l app.kubernetes.io/managed-by=orka-producer

# Force re-analysis after a prompt/tool change: bump -version on the produce step
# (and the receiver Deployment, if used). Task names embed the version, so old
# Tasks are ignored and new ones created. Re-applying an unchanged Task is a no-op
# (the K8s object store is the run-once cache).
```

## Troubleshoot

- **Results stay `pending`.** Tasks not finishing: check `kubectl get tasks` for
  `Running`/`Failed` and the ai-worker logs. Ensure the worker patches are applied.
- **`AI analysis unavailable: analysis Task ...`.** Honest surfacing of a
  `Failed`/`Cancelled`/missing Task, not a silent blank. Inspect that Task, fix the
  cause, bump `-version`, re-run.
- **Webhook never fires.** Orka's SSRF guard requires a same-namespace ClusterIP
  Service with a selector (`71` satisfies it). Delivery failures don't fail the Task
  (Orka retries). The receiver `-version` must match the producer's.
- **AI fields vanished after a run.** A fresh skeleton has no AI fields; the
  `-wait` ingest re-applies them from the result store. Keep exactly one writer to
  the PVC.
- **Tool CRDs pile up.** The ingestor `-gc` deletes a build's per-build Tools once
  its Tasks are terminal; unlabeled Tools are never touched.
- **Copilot returns no tool calls.** Copilot's non-streaming endpoint returns null
  `tool_calls` for Claude; use the de-streaming proxy. Standard OpenAI endpoints do
  not need it.

## Manifests reference

```
manifests/
  00-rbac.yaml              Orka SA access to core.orka.ai (chart gap).
  11-copilot-provider.yaml  GitHub Copilot Provider (via the proxy).
  12-kimi-provider.yaml     In-cluster OpenAI-compatible Provider (no proxy).
  20-artifact-tool.yaml          The multi-bucket artifact tool shim Deployment + Service.
  30-tools.yaml             Filesystem Tool CRDs (list/find/grep/read/tail).
  35-k8s-tools.yaml         k8s discovery Tool CRDs (CAPZ cluster navigation).
  36-41-*.yaml              Deterministic quality Tool CRDs (validate/verify/...).
  50-copilot-proxy.yaml     De-streaming proxy for Copilot.
  60-pipeline-rbac.yaml     Pipeline SA + Role/RoleBinding.
  70-pipeline-job.yaml      The analysis CronJob (fetch -> produce -> ingest).
  71-ingestor-webhook.yaml  Event-driven ingestor Deployment + Service.
worker-patches/             Required Orka ai-worker changes + why.
```

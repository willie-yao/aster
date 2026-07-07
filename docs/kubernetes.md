# Running the dashboard Kubernetes-native

The engine ships two deploy modes from one codebase. The default is the static
[GitHub Actions + Pages](../README.md) path: the fetcher writes JSON, Actions
builds the SPA, and Pages serves it. This guide covers the second mode, running
in-cluster next to your inference stack, where the fetcher is a CronJob and a
small server serves the dashboard from a shared volume.

Server mode is a strict superset of the static contract. The server exposes the
same `/data/*.json` files the SPA already reads, adds `/api/capabilities` so the
frontend can discover server-only features, and serves the SPA itself. See
[server.md](server.md) for the endpoint reference and the capability seam. The
static Pages path keeps working unchanged.

## Why run in-cluster

- The fetcher's model calls stay inside the cluster: low latency, no egress, and
  no need to expose a private endpoint publicly.
- The AI cache and output live on a shared volume, so warm caches survive across
  fetch runs and the server always serves the latest completed fetch.
- It is the foundation for stateful, interactive features added later.

## Architecture

```
CronJob (fetcher)  --writes-->  RWX volume  <--reads--  Deployment (server)
   -project-dir=/config           /data                   -data-dir=/data
   -out=/data                  data + ai_cache.json        -static-dir=/app/web
                                                                |
                                                             Service / Ingress
```

One image carries both binaries and the built SPA. The fetcher and the server
mount the same `ReadWriteMany` volume: the fetcher writes `dashboard.json`,
`jobs/*.json`, and the rest (plus its `ai_cache.json`), and the server reads
them. `ReadWriteMany` is required so both pods can mount the claim at once.

## Fetch modes: cron vs watch

The chart produces data in one of two modes, set by `mode`. Both keep exactly
one writer to the shared volume.

- `mode: watch` (default): a continuous worker Deployment refreshes data on a
  short interval, reusing a cached job list so it skips job rediscovery, and does
  a full pass (rediscover jobs, run notifications and issue and PR side effects)
  on a longer interval. Newly finished builds are analyzed within the watch
  interval instead of waiting for the next cron tick. The worker uses a
  `Recreate` rollout so an update never runs two writers at once.
- `mode: cron`: the fetcher runs as a scheduled CronJob. Portable, and the same
  binary the GitHub Actions + Pages path uses.

Watch mode detects new builds by listing each job's builds in the artifact
store and reusing the on-disk cache, the same mechanism a normal fetch uses. It
needs no TestGrid API, no Prow or bucket ownership, and no pub/sub.

The worker must be the only writer to the shared volume. Do not run the CronJob
or a manual `fetch-now` Job alongside it, and do not point a second release at
the same `existingClaim`. A `Recreate` rollout keeps a single worker across
updates, and Helm-managed config or secret changes trigger a rollout
automatically.

## Build and push the image

```bash
make image IMAGE=ghcr.io/you/prow-ai-dashboard VERSION=v1.0.0
docker push ghcr.io/you/prow-ai-dashboard:v1.0.0
```

Pushes to `main` and `vX.Y.Z` tags publish the image automatically via
`.github/workflows/image.yml` to `ghcr.io/<owner>/prow-ai-dashboard`.

## Install with Helm

The chart lives at `deploy/helm/prow-ai-dashboard`. Supply your consumer-owned
`project.yaml` and `prompts/system.md` at install time; they are never checked
into the engine repo.

```bash
helm install capz deploy/helm/prow-ai-dashboard \
  --namespace dashboards --create-namespace \
  --set image.tag=v1.0.0 \
  --set persistence.storageClass=<your-rwx-class> \
  --set-file project.config=../capz-prow-ai-dashboard/project.yaml \
  --set-file project.systemPrompt=../capz-prow-ai-dashboard/prompts/system.md \
  --set ai.enabled=true \
  --set ai.endpoint=http://vllm.inference.svc.cluster.local/v1/chat/completions \
  --set ai.model=<model-id> \
  --set ai.token=<token>
```

For production, provide the token via `ai.existingSecret` (see [Reusing
existing config](#reusing-existing-config)) rather than `--set ai.token`, which
lands in shell history and Helm release metadata.

To populate data immediately rather than waiting for the schedule, run the
fetcher once:

```bash
kubectl -n dashboards create job --from=cronjob/capz-prow-ai-dashboard-fetcher fetch-now
```

Then reach the server:

```bash
kubectl -n dashboards port-forward svc/capz-prow-ai-dashboard-server 8080:80
open http://localhost:8080
```

## Configuration reference

Key values (see `deploy/helm/prow-ai-dashboard/values.yaml` for the full set):

| Value | Purpose |
| --- | --- |
| `image.repository`, `image.tag` | Engine image; tag defaults to the chart `appVersion`. |
| `mode` | `watch` (continuous worker Deployment, default) or `cron` (scheduled CronJob). |
| `persistence.accessMode` | Must be `ReadWriteMany`. |
| `persistence.storageClass`, `persistence.size` | The shared volume's class and size. |
| `persistence.existingClaim` | Reuse a pre-provisioned PVC instead of creating one. |
| `project.config`, `project.systemPrompt` | Consumer config, via `--set-file`. |
| `project.existingConfigMap` | Reuse a ConfigMap with keys `project.yaml` and `system.md`. |
| `ai.enabled`, `ai.endpoint`, `ai.model`, `ai.token` | AI analysis and its OpenAI-compatible endpoint. |
| `ai.existingSecret`, `ai.tokenSecretKey` | Reuse a Secret holding the token. |
| `fetcher.schedule` | Cron schedule (default every 6 hours). `mode: cron`. |
| `fetcher.watchInterval`, `fetcher.reconcileInterval` | Refresh and full-pass cadence. `mode: watch`. |
| `fetcher.buildsPerJob`, `fetcher.workers`, `fetcher.timeout` | Fetch depth and budget. |
| `fetcher.extraEnv` | Extra env such as `GITHUB_TOKEN` or `SLACK_WEBHOOK_URL`. |
| `ingress.enabled`, `ingress.hosts`, `ingress.tls` | Public read path. |

The public read endpoints (`/data/*`, `/api/capabilities`, `/healthz`) are
unauthenticated. Interactive write actions and their auth model are a later
phase; until then the server is read-only.

`/data/*` serves everything the fetcher writes to the shared volume, matching
the static Pages path exactly. That includes the AI cache and the fetcher's
state files (issue, skill, and fix-PR tracking). None hold credentials, but if
you want those kept off a public ingress, keep the server on an internal
Service or split the fetcher's state onto a separate volume in a follow-up.

## Reusing existing config

If you manage the project config or credentials outside the chart, point the
chart at them and it will not create its own:

```bash
kubectl -n dashboards create configmap capz-project \
  --from-file=project.yaml=project.yaml \
  --from-file=system.md=prompts/system.md
kubectl -n dashboards create secret generic capz-ai --from-literal=AI_TOKEN=<token>

helm install capz deploy/helm/prow-ai-dashboard \
  --set project.existingConfigMap=capz-project \
  --set ai.enabled=true --set ai.existingSecret=capz-ai \
  --set ai.endpoint=... --set ai.model=...
```

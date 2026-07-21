# Running the dashboard Kubernetes-native

The engine ships two coequal deploy paths from one codebase. This guide covers
the **Kubernetes-native** path: the dashboard runs in-cluster next to your
inference stack, with the fetch as a worker (or CronJob) and a small server
serving the dashboard from a shared volume. The other path is the static
[GitHub Actions + Pages](../README.md) deploy, where the fetcher writes JSON,
Actions builds the SPA, and Pages serves it.

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
- It supports stateful, admin-gated actions such as filing issues and marking
  recurring failures resolved.

## Architecture

```
Worker Deployment (default) or CronJob
   -project-dir=/config  --writes--> RWX volume <--reads-- Deployment (server)
   -out=/data                         /data                 -data-dir=/data
                                                            -static-dir=/app/web
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

## Analysis backend: in-process or Orka

The `analysis` value selects how failing tests are analyzed. Both backends write
the same `jobs/*.json`, so the server, the SPA, and the `/data` contract are
identical either way.

- `analysis: inprocess` (default): the worker or CronJob runs the engine's
  in-process agentic loop, governed by `ai.enabled`. It is self-contained, so a
  fresh `helm install` works with no extra components.
- `analysis: orka` (advanced and experimental): the fetch step
  writes the dashboard skeleton only, and the [Orka](../experimental/orka/)
  pipeline runs the analysis as Kubernetes-native Tasks alongside your inference
  stack, with native retries, per-Task observability, and a path to agent-runtime
  remediation. It is useful when you want one observable Task per failure and
  are prepared to operate the additional Orka components and compatibility worker.

Orka mode requires `mode: cron` and assumes the Orka control plane, a Provider,
and the pinned [compatibility AI worker](../experimental/orka/worker-patches/COMPATIBILITY.md)
are installed. The dashboard chart creates a
release-scoped artifact Tool Deployment, Service, authentication Secret,
NetworkPolicy, and base Tool ConfigMap by default. Configure an external shared
shim only when the operator intentionally owns those resources separately.
The Orka skeleton fetch uses `-skip-side-effects`; notifications and GitHub
reconciliation run once, after final analysis and pattern output exist. Set
`orka.sideEffects.enabled=false` to suppress that final external reconciliation
for an evaluation run. Per-test Tasks are applied in bounded producer waves by
default. `orka.taskExecution` copies node selectors, tolerations, and affinity to
both per-test and pattern Task worker pods.

The chart deploys the analysis pipeline, not Orka itself, so the default stays
`inprocess` and a fresh install always works. Opt into Orka only after those
prerequisites are in place. See
[experimental/orka/QUICKSTART.md](../experimental/orka/QUICKSTART.md) for the full setup
and [experimental/orka/ARCHITECTURE.md](../experimental/orka/ARCHITECTURE.md) for
how it works. For a fresh, side-effect-free comparison and a reversible PVC
cutover, follow [Evaluating Orka safely](orka-evaluation.md).

Orka can also be used only for fix generation while analysis remains
`inprocess`. Set `orka.fixRuntime.enabled=true`, then configure
`ai.fix_prs.agent_runtime.type: orka` in the consumer project. This selects the
git-capable fixer image and enables Task RBAC for scheduled generation. The
server receives the ServiceAccount token only when interactive actions are also
enabled.
When the release namespace differs from `orka.namespace`, provide an
`ORKA_API_TOKEN` authorized for the Orka namespace if the API's namespace policy
does not accept the release ServiceAccount token.

## Build and push the image

```bash
make image IMAGE=ghcr.io/you/prow-ai-dashboard VERSION=v1.0.0
docker push ghcr.io/you/prow-ai-dashboard:v1.0.0
```

Pushes to `main` and `vX.Y.Z` tags publish the image automatically via
`.github/workflows/image.yml` to `ghcr.io/<owner>/prow-ai-dashboard`. A `vX.Y.Z`
tag additionally publishes the Helm chart to
`oci://ghcr.io/<owner>/charts/prow-ai-dashboard` and attaches the packaged
`.tgz` to the GitHub release (see `.github/workflows/release.yml`).

## Install with Helm

The chart is published to GHCR as an OCI artifact on each release, and its
source lives at `deploy/helm/prow-ai-dashboard`. Supply your consumer-owned
`project.yaml` and `prompts/system.md` at install time; they are never checked
into the engine repo. The `onboard -mode k8s` subcommand scaffolds a project
plus a `deploy/values.yaml` ready to pass here with `-f`; see
[onboarding-a-new-project.md](onboarding-a-new-project.md#step-3a-kubernetes-native).

Install the released chart straight from GHCR (no repo checkout needed). The
chart pins its image to the matching release, so `image.tag` is optional:

```bash
helm install capz oci://ghcr.io/willie-yao/charts/prow-ai-dashboard \
  --version 1.0.0-beta.5 \
  --namespace dashboards --create-namespace \
  --set persistence.storageClass=<your-rwx-class> \
  --set-file project.config=../capz-prow-ai-dashboard/project.yaml \
  --set-file project.systemPrompt=../capz-prow-ai-dashboard/prompts/system.md \
  --set ai.enabled=true \
  --set ai.endpoint=http://vllm.inference.svc.cluster.local/v1/chat/completions \
  --set ai.model=<model-id> \
  --set ai.token=<token>
```

> GHCR packages are private by default. If the pull fails with an auth error,
> make the `charts/prow-ai-dashboard` package public once in the repo's package
> settings, or `helm registry login ghcr.io` first. As a no-auth alternative,
> every release also attaches the packaged chart `.tgz`: download it from the
> release page and `helm install capz ./prow-ai-dashboard-<version>.tgz ...`.

To install from a local checkout instead (e.g. an unreleased change), point Helm
at the chart directory and set `image.tag` to a published image tag:

```bash
helm install capz deploy/helm/prow-ai-dashboard \
  --namespace dashboards --create-namespace \
  --set image.tag=v1.0.0-beta.5 \
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
kubectl -n dashboards create job \
  --from=cronjob/capz-prow-ai-dashboard-fetcher \
  fetch-now-$(date -u +%Y%m%d%H%M%S)
```

For a suspended evaluation CronJob, `run-cronjob-now.sh` checks for active
scheduled or manual Jobs and can wait for completion. The check is not a
distributed lock, so do not invoke the helper concurrently.

For the Orka backend, `experimental/orka/orka-ops.sh` validates the control
plane and Provider, runs a disposable smoke Task, reports Task and Tool state by
project and build, and previews age-bounded garbage collection. The preview is
project-scoped and never deletes resources. See the
[Orka quickstart](../experimental/orka/QUICKSTART.md#operate).

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
| `analysis` | `inprocess` (default; in-cluster agentic loop) or `orka` (advanced experimental pipeline; requires `mode: cron`, the Orka control plane, a Provider, and the compatibility worker). |
| `orka.artifactTool.*` | Release-scoped artifact Tool image, authentication, network policy, resources, and scheduling. |
| `orka.baseTools.*` | Create the synchronized producer ConfigMap or reference an existing ConfigMap in the release namespace. |
| `orka.producer.maxConcurrentTasks`, `taskPoll`, `waveTimeout` | Apply per-test Tasks in bounded waves (`0` through `1000`) and bound placement recovery and intermediate-wave polling. |
| `orka.taskExecution.*` | Copy node selectors, tolerations, and affinity to Orka per-test and pattern worker pods. |
| `orka.sideEffects.enabled` | Run post-analysis notifications and GitHub reconciliation. Disable for an Orka evaluation. |
| `orka.fixRuntime.enabled` | Mount a ServiceAccount token and grant Orka Task RBAC for `agent_runtime.type: orka` fix generation. |
| `persistence.accessMode` | Must be `ReadWriteMany`. |
| `persistence.storageClass`, `persistence.size` | The shared volume's class and size. |
| `persistence.existingClaim` | Reuse a pre-provisioned PVC instead of creating one. |
| `persistence.retain` | Preserve a chart-managed PVC when it leaves the release. Defaults to `true`. |
| `project.config`, `project.systemPrompt` | Consumer config, via `--set-file`. |
| `project.existingConfigMap` | Reuse a ConfigMap with keys `project.yaml` and `system.md`. |
| `ai.enabled`, `ai.endpoint`, `ai.model`, `ai.token` | AI analysis and its OpenAI-compatible endpoint. |
| `ai.existingSecret`, `ai.tokenSecretKey` | Reuse a Secret holding the token. |
| `fetcher.schedule` | Cron schedule (default every 6 hours). `mode: cron`. |
| `fetcher.suspend` | Suspend scheduled CronJob starts while allowing manual Jobs. `mode: cron`. |
| `fetcher.watchInterval`, `fetcher.reconcileInterval` | Refresh and full-pass cadence. `mode: watch`. |
| `fetcher.buildsPerJob`, `fetcher.workers`, `fetcher.timeout` | Fetch depth and budget. |
| `fetcher.extraEnv` | Extra env such as `GITHUB_TOKEN`, `EMAIL_SMTP_PASSWORD`, or the `ISSUE_TOKEN` / `FIX_TOKEN` write tokens (see [Automatic issues and fix PRs](#automatic-issues-and-fix-prs)). |
| `ingress.enabled`, `ingress.hosts`, `ingress.tls` | Public read path. |
| `server.actions.enabled`, `server.actions.mode` | Turn on write actions; `oauth` (GitHub sign-in) or `proxy` (SSO proxy + bot token). |
| `server.actions.admins` | Required allowlist for write actions. An empty list fails closed. |

The public read endpoints (`/data/*`, `/api/capabilities`, `/healthz`) are
unauthenticated. Admin write actions are opt-in: set `server.actions.enabled`
and choose `server.actions.mode` (`oauth` for GitHub sign-in with per-user
attribution, or `proxy` for an upstream SSO proxy plus a bot token), then list
the allowed GitHub logins in `server.actions.admins` (see [server.md](server.md)).

### Enabling actions with Helm

OAuth mode (per-user attribution). Register a GitHub OAuth App first (see
[server.md](server.md#setting-up-oauth-mode)); its callback URL is your
dashboard URL plus `/api/auth/callback`.

```bash
helm upgrade --install capz deploy/helm/prow-ai-dashboard \
  ... \
  --set server.actions.enabled=true \
  --set server.actions.mode=oauth \
  --set 'server.actions.admins={alice,bob}' \
  --set server.actions.oauth.clientId=<client-id> \
  --set server.actions.oauth.clientSecret=<client-secret> \
  --set server.actions.oauth.redirectUrl=https://dashboard.example.com/api/auth/callback \
  --set server.actions.oauth.sessionKey="$(openssl rand -base64 32)"
```

Proxy mode (an SSO proxy fronts the server; a bot token writes):

```bash
helm upgrade --install capz deploy/helm/prow-ai-dashboard \
  ... \
  --set server.actions.enabled=true \
  --set server.actions.mode=proxy \
  --set server.actions.proxy.header=X-Auth-Request-Email \
  --set server.actions.proxy.botToken=<bot-pat> \
  --set 'server.actions.admins={alice,bob}'
```

Provide the OAuth secret/session key or bot token via a pre-made Secret instead
with `server.actions.oauth.existingSecret` (keys `OAUTH_CLIENT_SECRET`,
`SESSION_KEY`) or `server.actions.proxy.existingSecret` (key `BOT_TOKEN`).

`/data/*` serves the public dashboard files that the static Pages path exposes.
The server rejects operational files such as `ai_cache.json`, issue state,
fix-PR state, previews, notification state, remediation state, and the private
Prow coverage catalog. Pages strips the same files before publication.
`resolved.json` and the redacted `remediations.json` remain public because the
frontend uses them.

## Email notifications

Enable email delivery in the consumer `project.yaml`, then source the SMTP
password from a Secret when the relay uses authentication:

```bash
kubectl -n dashboards create secret generic capz-smtp \
  --from-literal=password=<smtp-password>
```

```yaml
fetcher:
  extraEnv:
    - name: EMAIL_SMTP_PASSWORD
      valueFrom:
        secretKeyRef: { name: capz-smtp, key: password }
```

The SMTP host in `notifications.email.smtp.host` must be reachable from the
worker or CronJob. Set `notifications.email.action_links: true` after server
actions are enabled to add authenticated issue and fix review links to systemic
pattern emails. Also expose `EMAIL_SMTP_PASSWORD` through `server.extraEnv` so
the server can send draft-ready review emails. See [Email notifications](notifications.md) for TLS modes,
message behavior, and unauthenticated relay configuration.

## Automatic issues and fix PRs

Both features are off by default. When enabled, the fetcher files GitHub issues
for the highest-signal failures and drafts fix PRs for recurring ones on every
pass: each cron run in `mode: cron`, or each reconcile pass in `mode: watch`.
Each needs the feature turned on in `project.yaml` and a write-scoped token in
the fetcher's environment.

Fix PR generation also needs `opencode` and git in the writer container. The
standard distroless engine image does not contain them, and the chart does not
install them. Use a custom writer deployment before enabling scheduled fix PRs.
The same runtime requirement applies to the interactive Propose fix action;
File issue and Mark resolved work in the standard server image.

Turn them on in `project.yaml`:

```yaml
issues:
  enabled: true          # repo defaults to branding.source_repo
ai:
  fix_prs:
    enabled: true        # repo defaults to branding.source_repo
```

Supply the tokens through `fetcher.extraEnv`, which lands on both the worker and
the CronJob. The engine reads `ISSUE_TOKEN` for issues and `FIX_TOKEN` for fix
PRs. `ISSUE_TOKEN` wants `issues: write` on the target repo; `FIX_TOKEN` is a
real contributor's PAT with `Contents: write` and `Pull requests: write`. See
[fix-prs.md](fix-prs.md#identity-cla-and-the-token-read-this-first) for the
fork-versus-branch token rules. Source both from a Secret you manage:

```bash
kubectl -n dashboards create secret generic capz-write-tokens \
  --from-literal=ISSUE_TOKEN=<pat> \
  --from-literal=FIX_TOKEN=<pat>
```

```yaml
# values.yaml
fetcher:
  extraEnv:
    - name: ISSUE_TOKEN
      valueFrom:
        secretKeyRef: { name: capz-write-tokens, key: ISSUE_TOKEN }
    - name: FIX_TOKEN
      valueFrom:
        secretKeyRef: { name: capz-write-tokens, key: FIX_TOKEN }
```

If a feature is enabled but its token is missing, the fetcher logs a skip and
continues, so a misconfigured token never fails the pass. See
[github-issues.md](github-issues.md) and [fix-prs.md](fix-prs.md) for the
triggers, guardrails, and the rest of the per-feature `project.yaml` fields.

Remediation verification observes existing Prow jobs. It does not run repository
E2E commands inside the dashboard cluster. Project tests keep using their normal
Prow build cluster, credentials, quotas, artifact upload, and cleanup behavior.

This is the scheduled, unattended path. To let an admin file one issue or draft
one fix PR on demand from the dashboard UI, enable the interactive server
actions instead (see [Enabling actions with Helm](#enabling-actions-with-helm)).

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

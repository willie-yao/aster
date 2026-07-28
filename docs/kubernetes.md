# Running the dashboard Kubernetes-native

Use the Helm chart when the dashboard needs a private in-cluster model endpoint,
persistent shared data, or authenticated server actions. For a public read-only
site without a cluster, use [GitHub Actions and Pages](github-pages.md).

The chart runs a worker or CronJob beside a small server that serves the SPA and
the same `/data/*.json` contract as Pages. The server also exposes
`/api/capabilities` for server-only features. See [Server mode](server.md) for
the endpoint and authentication reference.

Start with `fetcher onboard -mode k8s`. Its generated values contain only the
storage class, model connection, and a safe fetch timeout. The rest of this guide
is an operator reference for production settings and optional features.

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

## Analysis runtimes and Orka fix generation

See [Orka architecture in prow-ai-dashboard](orka-architecture.md) for the
component, credential, state, and ownership boundaries shared by the analysis,
fix-generation, and source-investigation paths.

### In-process analysis

`analysisRuntime.type: inprocess` is the default and the only recommended
production mode. It works with both `mode: watch` and `mode: cron`. Pages also
uses this runtime.

### Experimental Orka container analysis

`analysisRuntime.type: orka-container` is an opt-in, Helm-only, cron-only
sidegrade. It submits one content-addressed Orka `type: container` Task per
failure. The analyzer image runs the current dashboard `FailureAnalyzer`, so
prompts, Tool schemas, skills loaded through `LoadForTools`, ranked evidence
planning, evidence coverage, critique, semantic review, cache acceptance, traces,
and `FailureAnalysisResult` stay dashboard-owned. The patched Orka AI worker,
Provider resources, dynamic Tool resources, and analysis worker patches remain
removed.

Use this mode only for a concrete lifecycle requirement such as per-failure Task
isolation or Task retry history. It has no Pages support, no watch-mode support,
no backward compatibility guarantee, and is not recommended over in-process
analysis.

```yaml
mode: cron

ai:
  enabled: true
  endpoint: http://model.model-system.svc.cluster.local/v1/chat/completions
  model: model-id
  existingSecret: dashboard-model

analysisRuntime:
  type: orka-container
  orkaContainer:
    namespace: "" # chart creates a retained release-scoped namespace
    api: http://orka.orka-system.svc.cluster.local:8080
    apiAuth:
      existingSecret: ""
      tokenKey: token
    maxConcurrentTasks: 2
    pollInterval: 2s
    taskTimeout: 20m
    retries: 1
    image:
      repository: ghcr.io/willie-yao/prow-ai-dashboard/analyzer
      tag: sha-deadbeef
      pullPolicy: IfNotPresent
    modelAuth:
      existingSecret: orka-model
      tokenKey: token
    state:
      existingSecret: ""
      key: state-key
    nodeSelector:
      agentpool: nodepool1
    tolerations: []
    affinity: {}
```

Set `orkaContainer.api` to the REST Service of the installed Orka release; the
Service name is not derived from the namespace.
With an empty `apiAuth.existingSecret`, the fetcher uses its projected
ServiceAccount token and reloads the file for every result request. Set
`apiAuth.existingSecret` to retain static-token compatibility when the Orka API
does not accept that ServiceAccount identity. This credential is separate from
the model token stored in the analysis namespace.

`taskTimeout` must be at least the project `ai.timeout` plus two minutes for
Task startup and encrypted result finalization. The fetcher rejects a shorter
outer timeout at startup instead of allowing Orka to kill the analyzer before
it can emit recoverable state.

Because the pinned Orka controller uses `IfNotPresent`, the chart rejects mutable analyzer tags such as `main`, `latest`, `dev`, and moving major tags. Use a `sha-<hex>` tag or a full semantic version.

The normal fetcher still needs its `AI_TOKEN` in the dashboard namespace for the
cross-build pattern pass. Create `analysisRuntime.orkaContainer.modelAuth.existingSecret`
in the analysis namespace for per-failure Tasks. The chart never copies provider
credentials across namespaces. For example:

```bash
kubectl -n dashboards create secret generic dashboard-model \
  --from-literal=AI_TOKEN='<token>'
ANALYSIS_NS=$(kubectl get namespace \
  -l app.kubernetes.io/instance=capz,app.kubernetes.io/component=orka-container-analysis \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n "$ANALYSIS_NS" create secret generic orka-model \
  --from-literal=token='<token>'
```

Only when `apiAuth.existingSecret` is set, create that static credential in the
dashboard namespace:

```bash
kubectl -n dashboards create secret generic orka-analysis-api \
  --from-literal=token='<Orka API token authorized for the analysis namespace>'
```

When `state.existingSecret` is empty, Helm creates matching release-scoped
AES-256 state key Secrets in the dashboard and analysis namespaces and marks them to
be retained. If you supply `state.existingSecret`, create the same Secret name
and key in both namespaces. The mounted value must itself be standard base64 for
exactly 32 random bytes. `kubectl --from-literal` performs the outer Kubernetes
Secret encoding, so generate one shared literal and use it in both namespaces:

```bash
STATE_KEY=$(openssl rand -base64 32)
kubectl -n dashboards create secret generic shared-analysis-state \
  --from-literal=state-key="$STATE_KEY"
kubectl -n "$ANALYSIS_NS" create secret generic shared-analysis-state \
  --from-literal=state-key="$STATE_KEY"
```

The encrypted state preserves raw cache entries, including evidence coverage
fields added by newer engine versions, without enumerating their schema. Task
identity remains in the encrypted wrapper and Orka Task, not the private
analysis trace schema.

Consumed input bundles are removed immediately. Terminal analyzer Tasks remain
available for identical in-flight callers and are removed by a bounded retention
pass using exact UID and resource version. Failed Tasks transport authenticated
private traces, but their cache entries are never merged.

The chart creates and retains a namespace dedicated to each Helm release when
`orkaContainer.namespace` is empty. A custom namespace must end in the chart's
release-scope hash, which prevents releases from sharing Task RBAC, maintenance,
or admission policy. It must not be the Orka controller, fix-runtime, or
dashboard release namespace. Keep only the analyzer model and state Secrets
there. Container mode also installs a fail-closed
`ValidatingAdmissionPolicy` that pins the analyzer image, arguments, model
coordinates, CPU placement, bundle reference, and exact model/state Secret
references. Installing this experimental mode therefore requires permission to
create cluster-scoped admission policies.

The immutable ConfigMap bundle contains the sanitized project policy, prompt,
skill files, request, and a bounded raw cache seed. It never contains model
credentials. Projects using `ai.headers` are rejected for this experimental
runtime because the adapter has no secure cross-namespace header transport. Use
bearer-token authentication or a trusted proxy.

Analyzer Tasks default to `agentpool: nodepool1`. Helm requires an explicit
`agentpool` CPU pool and rejects accelerator selectors, affinity, and tolerations,
including vendor accelerator labels. Install the Orka controller and helper
workloads on CPU nodes as well. The pinned Orka controller already applies a non-root, read-only-root
container security context. Only the model-serving workload may select GPU
nodes.

### Orka fix generation

Orka fix generation remains independent. Set `orka.fixRuntime.enabled=true`,
then configure `ai.fix_prs.agent_runtime.type: orka` in the consumer project.
This selects the git-capable fixer image and enables a separate Task-only Role.
Enabling container analysis does not enable, configure, or change the fix
runtime.

The dashboard type and the Orka Agent runtime are separate settings:

- `ai.fix_prs.agent_runtime.type: orka` selects Orka as the generation backend.
- `Agent.spec.runtime.type: opencode` selects OpenCode inside Orka.

The operator owns the Agent and its model Secret. The Secret must contain
`OPENAI_BASE_URL`; `OPENAI_API_KEY` is optional for endpoints that do not require
authentication. The Agent's `model.name` is the endpoint-specific model ID. See
[`configs/example/orka-opencode-agent.yaml`](../configs/example/orka-opencode-agent.yaml)
for a complete manifest and [Agent-proposed fix PRs](fix-prs.md#orka-in-cluster)
for the matching `project.yaml` configuration.

Do not place model settings or model credentials in `project.yaml`. A private
repository may use `git_secret`, but that Secret must contain only a read-only
clone credential. `FIX_TOKEN` remains in the dashboard workload and is never
passed to the Orka Agent, Task, workspace, or model Secret.

OpenCode requires an Orka build containing upstream PR #289. No tagged Orka
release contained that change as of July 24, 2026, so verify and pin the Orka
source and harness image before enabling it. Orka labels the entire project
experimental.

At Orka merge commit `d03acb99`, the Helm controller ClusterRole omits
`agentruntimes` and `substrateactorpools`. Use source manifests or a later chart
with the corrected Orka controller RBAC. Do not add those permissions to the
dashboard ServiceAccount; it needs Orka Task access only.

When the release namespace differs from `orka.namespace`, grant the dashboard
ServiceAccount access to the Orka result API. Use a static `ORKA_API_TOKEN` only
when the API namespace policy cannot accept that ServiceAccount identity.

## Build and push the image

```bash
make image IMAGE=ghcr.io/you/prow-ai-dashboard VERSION=v1.0.0
make analyzer-image IMAGE=ghcr.io/you/prow-ai-dashboard VERSION=v1.0.0
make fixer-image IMAGE=ghcr.io/you/prow-ai-dashboard VERSION=v1.0.0
docker push ghcr.io/you/prow-ai-dashboard:v1.0.0
docker push ghcr.io/you/prow-ai-dashboard/analyzer:v1.0.0
docker push ghcr.io/you/prow-ai-dashboard/fixer:v1.0.0
```

Pushes to `main` and `vX.Y.Z` tags publish the engine, analyzer, and fixer images automatically via
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
[Onboarding a project](onboarding-a-new-project.md).

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

When the provider's total context window is independently known, add
`--set ai.contextWindowTokens=<tokens>` with at least `9217` tokens. For the current Copilot GPT-5 mini
deployment, use `--set ai.contextWindowTokens=128000`. Leave it unset for a
generic endpoint so provider metadata or the bounded fallback remains active.

To populate data immediately rather than waiting for the schedule, run the
fetcher once:

```bash
kubectl -n dashboards create job \
  --from=cronjob/capz-prow-ai-dashboard-fetcher \
  fetch-now-$(date -u +%Y%m%d%H%M%S)
```

For a suspended evaluation CronJob, `run-cronjob-now.sh` checks for active
scheduled or manual Jobs and can wait for completion. When its wait timeout
expires, it deletes the still-running Job by default. Use `--keep-on-timeout`
only when another operator will continue monitoring it. The check is not a
distributed lock, so do not invoke the helper concurrently.

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
| `analysisRuntime.type` | `inprocess` by default; `orka-container` is experimental and requires `mode: cron`. |
| `analysisRuntime.orkaContainer.*` | Orka result API, analyzer image, namespace, bounded Task lifecycle, Secret references, encrypted state key, and CPU placement. |
| `fetcher.restartPolicy`, `fetcher.backoffLimit`, `fetcher.activeDeadlineSeconds` | Bound CronJob container restarts, Job retries, and total wall time. Empty restart policy selects `OnFailure` for in-process and `Never` for Orka container analysis; the default deadline is 10 hours. |
| `orka.fixRuntime.enabled` | Mount a ServiceAccount token and grant Orka Task RBAC for `agent_runtime.type: orka` fix generation. |
| `persistence.accessMode` | Must be `ReadWriteMany`. |
| `persistence.storageClass`, `persistence.size` | The shared volume's class and size. |
| `persistence.existingClaim` | Reuse a pre-provisioned PVC instead of creating one. |
| `persistence.retain` | Preserve a chart-managed PVC when it leaves the release. Defaults to `true`. |
| `project.config`, `project.systemPrompt` | Consumer config, via `--set-file`. |
| `project.existingConfigMap` | Reuse a ConfigMap with keys `project.yaml` and `system.md`. |
| `project.materializer.image.*` | Small pinned image used by Orka container analysis to copy ConfigMap-backed project files into a regular-file runtime directory. |
| `ai.enabled`, `ai.endpoint`, `ai.model`, `ai.token` | AI analysis and its OpenAI-compatible endpoint. |
| `ai.contextWindowTokens` | Optional operator-provided total provider context window. Set only with endpoint evidence. Values must be at least `9217`; use `128000` for the current Copilot GPT-5 mini deployment. |
| `ai.existingSecret`, `ai.tokenSecretKey` | Reuse a Secret holding the token. |
| `fetcher.schedule` | Cron schedule (default every 6 hours). `mode: cron`. |
| `fetcher.suspend` | Suspend scheduled CronJob starts while allowing manual Jobs. `mode: cron`. |
| `fetcher.watchInterval`, `fetcher.reconcileInterval` | Refresh and full-pass cadence. `mode: watch`. |
| `fetcher.buildsPerJob`, `fetcher.workers`, `fetcher.timeout` | Fetch depth and discovery/artifact budget. Orka Task waves use `taskTimeout` and the CronJob deadline. |
| `fetcher.extraEnv` | Extra env such as `GITHUB_TOKEN`, `EMAIL_SMTP_PASSWORD`, or the `ISSUE_TOKEN` / `FIX_TOKEN` write tokens (see [Automatic issues and fix PRs](#automatic-issues-and-fix-prs)). |
| `ingress.enabled`, `ingress.hosts`, `ingress.tls` | Public read path. |
| `server.chat.enabled` | Enable authenticated analysis conversations. Requires `ai.enabled`. |
| `server.chat.timeout` | Per-turn model timeout. Defaults to `2m`; slow local providers may use up to `30m`. |
| `server.chat.correctionsEnabled` | Enable explicit promotion and revocation of evidence-backed correction overlays. |
| `server.chat.sourceInvestigation.enabled` | Enable owner-bound read-only source investigation controls and Orka agent Tasks. |
| `server.chat.sourceInvestigation.serviceAccountName` | Operator-managed dedicated ServiceAccount name when `orka.rbac.create=false`. |
| `server.chat.sourceInvestigation.maxPerSession` | Persisted source requests per session. Defaults to `8`. |
| `server.chat.sourceInvestigation.maxActivePerOwner` | Concurrent source Tasks per login. Defaults to `1`. |
| `server.chat.sessionTTL` | Persisted conversation retention. Defaults to `2h`. |
| `server.chat.maxSessions`, `server.chat.maxSessionsPerOwner` | Deployment-wide and per-login live-session caps. |
| `server.chat.maxActiveTurnsPerOwner` | Concurrent background turns per login. Defaults to `2`. |
| `server.chat.requestsPerMinute` | Newly admitted turns per login in a rolling minute. Defaults to `10`. |
| `server.replicaCount` | Server replicas. Chat sessions are shared through the RWX volume. |
| `server.security.hsts.enabled` | Send a one-year HSTS policy. Defaults to `true` for Helm deployments. |
| `server.development.allowInsecureCookies` | Allow OAuth cookies over local HTTP. Requires HSTS to be disabled and must not be used for a deployed dashboard. |
| `server.actions.enabled`, `server.actions.mode` | Turn on admin authentication, write actions, and private trace access; `oauth` (GitHub sign-in) or `proxy` (SSO proxy + bot token). |
| `server.actions.admins` | Required allowlist for admin actions, chat, and trace access. An empty list fails closed. |

The public read endpoints (`/data/*`, `/api/capabilities`, `/healthz`) are
unauthenticated. Admin features are opt-in. Set `server.chat.enabled` for
read-only conversations or `server.actions.enabled` for GitHub writes, choose
`server.actions.mode` (`oauth` for GitHub sign-in or `proxy` for upstream SSO),
and list the allowed logins in `server.actions.admins` (see
[server.md](server.md)). Proxy mode needs a bot token only when write actions
are enabled. The same authenticated session protects write actions, analysis
chat, and the private analysis trace page.

The chart reserves `COOKIE_INSECURE` and rejects it in `server.extraEnv`.
For local OAuth testing over HTTP, set
`server.security.hsts.enabled=false` and
`server.development.allowInsecureCookies=true`. Deployed OAuth dashboards
should keep the defaults.

### Enabling analysis chat with Helm

Analysis chat is Kubernetes-native and uses the shared RWX volume for private,
owner-bound session state. Multiple server replicas can serve the same session.
The storage class must support advisory file locking, atomic rename, and file and directory synchronization. The
volume contains private transcripts and selected failure context, so restrict
PVC access and backups to dashboard operators. Authentication reuses
`server.actions` settings, but chat alone does not enable
GitHub writes or require `BOT_TOKEN`.

```bash
helm upgrade --install capz deploy/helm/prow-ai-dashboard \
  ... \
  --set ai.enabled=true \
  --set server.replicaCount=2 \
  --set server.chat.enabled=true \
  --set server.chat.timeout=10m \
  --set server.chat.correctionsEnabled=true \
  --set server.chat.sourceInvestigation.enabled=true \
  --set server.actions.mode=oauth \
  --set 'server.actions.admins={alice,bob}' \
  --set server.actions.oauth.clientId=<client-id> \
  --set server.actions.oauth.clientSecret=<client-secret> \
  --set server.actions.oauth.redirectUrl=https://dashboard.example.com/api/auth/callback \
  --set server.actions.oauth.sessionKey="$(openssl rand -base64 32)"
```

The chart stores chat state at `<persistence.mountPath>/.analysis-chat`, mounts
the shared volume read-write in the server, and keeps the directory unavailable
through `/data/*`. Turns continue when a browser stream disconnects, persist
non-sensitive investigation phases, and can be cancelled from any replica.
Tune the provider turn bound with `server.chat.timeout`. Tune retention and
capacity with `server.chat.sessionTTL`,
`server.chat.maxSessions`, `server.chat.maxSessionsPerOwner`,
`server.chat.maxActiveTurnsPerOwner`, and `server.chat.requestsPerMinute`.
Correction promotion is disabled by default. When enabled, the server writes a
private audit ledger and the public `analysis_corrections.json` overlay to the
same shared volume; it never rewrites fetched job JSON.

When `server.chat.enabled=true` and `server.actions.enabled=true`, the server also
advertises the chat-to-fix bridge. Eligible completed responses expose **Use
this finding in a fix proposal**, followed by an explicit context review, the
existing fix preview, and final confirmation before any GitHub write. Preview
and confirmation state is persisted on the shared private volume so retries can
recover across server replicas and restarts.

Source investigation is also disabled by default. Configure its independent
read-only runtime in `project.yaml`:

```yaml
ai:
  source_investigation:
    agent_ref: guarded-source-reader
    api: http://orka.orka-system.svc.cluster.local:8080
    namespace: orka-system
    git_secret: source-repo-readonly
    max_turns: 30
    timeout: 10m
    retries: 1
```

The Agent named by `agent_ref` must use a runtime supported by Orka's enforced
`orka.ai/agent-read-only` contract. Orka releases that reject OpenCode in guarded
mode cannot use an OpenCode Agent for this feature. Do not remove the guard to
make an unsupported runtime start. Create `git_secret` in the Orka namespace with
read-only repository credentials. The chart gives the web-facing server a
dedicated ServiceAccount with only Task create, get, patch, and delete
permissions. Source investigation alone does not require the git-capable fixer
image. When write actions and `orka.fixRuntime.enabled=true` are also enabled,
the existing fixer image selection still applies. If the source repository is
private, put a read-only GitHub token in the AI Secret under `GITHUB_READ_TOKEN`
so the server can verify returned quotes against the pinned commit.

`retries` must be between `0` and `2`. A nonzero `max_turns` must be between `1`
and `1000`, matching the Orka Task CRD.

With chart-managed RBAC, `ai.source_investigation.namespace` must match the
chart's `orka.namespace` value. If Orka-backed write actions are also enabled,
the chart binds the server's source investigation ServiceAccount to both
Task-only Roles. With operator-managed RBAC, bind that ServiceAccount in every
namespace used by source investigation or fix Tasks.

If the source runtime uses another namespace, disable `orka.rbac.create` and
provide the same Task-only permissions there.

Completed assistant responses expose an **Investigate source** control. The
dashboard streams persisted progress, reconnects with the same request ID, allows
cancellation, and renders only independently verified source citations.

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
The server rejects operational files such as `ai_cache.json`, `ai_traces.json`,
issue state, fix-PR state, previews, notification state, remediation state,
chat sessions, and the private Prow coverage catalog. Static Pages deployments
do not create chat session state; the other operational files are stripped before
publication.
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

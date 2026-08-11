# Kubernetes operator reference

This page contains the architecture, deployment behavior, upgrade rules, and
advanced chart configuration that do not belong in the first-run path. Start
with the [Kubernetes quickstart](kubernetes.md) or the generated
`deploy/README.md`.

Orka is documented separately in the
[experimental Orka maintainer reference](orka.md). It is not required for the
default in-process deployment.

## Why run in-cluster

- Model calls can stay inside the cluster without exposing a private endpoint.
- Output and AI cache state persist on shared storage across fetch passes.
- The server always reads the latest completed dashboard data.
- Kubernetes mode can add authenticated chat and guarded GitHub actions.

## Architecture

```text
Worker Deployment (watch) or CronJob (cron)
   -project-dir=/config  --writes--> RWX volume <--reads-- Server Deployment
   -out=/data                         /data                 -data-dir=/data
                                                            -static-dir=/app/web
                                                                   |
                                                            Service / Ingress
```

One image carries the worker or fetcher, server, and built SPA. The writer and
server mount the same `ReadWriteMany` volume. The writer publishes
`dashboard.json`, `jobs/*.json`, aggregate files, and private cache state. The
server serves only the allowed public data contract plus server APIs.

The server exposes the same `/data/*.json` contract as Pages and adds
`/api/capabilities`. See [Server mode](server.md) for endpoint and authentication
details.

## Fetch modes

Set the writer with `mode`.

### Watch mode

`mode: watch` is the default. A continuous worker Deployment:

- Checks known jobs for new builds on `fetcher.watchInterval`.
- Reuses the cached job list during short refreshes.
- Rediscovers jobs and runs configured side effects on
  `fetcher.reconcileInterval`.
- Uses a `Recreate` rollout so two writers do not overlap during an update.

Watch mode needs no TestGrid API, Prow ownership, or pub/sub integration. It
uses normal artifact-store listing and the on-disk cache.

### Cron mode

`mode: cron` runs the fetcher as a scheduled CronJob. It uses the same fetcher
binary as the Pages path. Configure `fetcher.schedule`, concurrency, deadline,
retry, and restart behavior in Helm values.

### Single-writer rule

Both modes require exactly one writer for the shared volume. Do not:

- Run a CronJob or manual fetch Job while the watch worker exists.
- Point two dashboard releases at the same `persistence.existingClaim`.
- Start a second writer during an upgrade.

The chart rolls managed config and Secret changes into the watch Deployment so
the worker restarts with the current configuration.

## Consumer bundle ownership

A Kubernetes consumer normally contains:

```text
project.yaml
prompts/system.md
skills/*.yaml
deploy/values.yaml
deploy/README.md
```

The `skills/` directory is optional unless the project requires a consumer skill
bundle.

`project.yaml` owns portable discovery, branding, analysis policy, and optional
feature policy. `deploy/values.yaml` owns infrastructure, credentials, image
selection, persistence, and runtime tuning. Do not duplicate the project schema
inside Helm values.

The supported deployment wrapper is part of the `fetcher` binary:

```text
fetcher kubernetes install
fetcher kubernetes upgrade
```

Every operation validates:

- `project.yaml` with the strict loader.
- A non-empty `prompts/system.md`.
- Enabled engine tools and profiles.
- Consumer skill files and required skill counts.
- The supplied Helm values.

The wrapper passes the current bundle with `--set-file`. It clears stale
chart-managed project maps and creates one release-managed ConfigMap containing
`project.yaml`, `system.md`, and each consumer skill. Checksums roll the writer
and interactive server when the bundle changes.

Relative `--values` paths are resolved from `--project-dir`. Install refuses an
existing release. Upgrade requires an existing release. The current kubectl or
Helm context is never selected implicitly.

## Local chart development

The wrapper defaults to the published OCI chart. Use the local chart only when
testing engine chart changes:

```bash
export PROJECT_DIR="<consumer-directory>"
export RELEASE="<dashboard-release>"
export NAMESPACE="<dashboard-namespace>"
export CONTEXT="<explicit-context>"

./bin/fetcher kubernetes install \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart deploy/helm/prow-ai-dashboard \
  --dry-run
```

`--dry-run` runs `helm template` locally. It does not contact the context or
print rendered Secrets.

## Building and publishing images

For a fork or local registry:

```bash
export IMAGE="ghcr.io/<owner>/prow-ai-dashboard"
export VERSION="vX.Y.Z"

make image IMAGE="$IMAGE" VERSION="$VERSION"
make analyzer-image IMAGE="$IMAGE" VERSION="$VERSION"
make fixer-image IMAGE="$IMAGE" VERSION="$VERSION"
make remote-fixer-image IMAGE="$IMAGE" VERSION="$VERSION"
make agent-sandbox-fix-executor-image IMAGE="$IMAGE" VERSION="$VERSION"

docker push "${IMAGE}:${VERSION}"
docker push "${IMAGE}/analyzer:${VERSION}"
docker push "${IMAGE}/fixer:${VERSION}"
docker push "${IMAGE}/remote-fixer:${VERSION}"
docker push "${IMAGE}/agent-sandbox-fix-executor:${VERSION}"
```

Pushes to `main` and `vX.Y.Z` tags in the engine repository publish images
through `.github/workflows/image.yml`. A release tag also publishes the Helm
chart and attaches the packaged chart to the GitHub release. See
[Releasing](releasing.md).

Do not use a mutable tag for a production rollback target. Agent Sandbox
consumers must resolve the executor tag to its OCI digest and configure
`agentSandbox.fixRuntime.image.digest`; a tag alone is rejected. Record the
chart version and resolved image references before changing a live release.

## Upgrade behavior

Every wrapper upgrade includes the current project, prompt, skills, and consumer
values. It reuses deployed values first, applies the current bundle, waits for
readiness, and rolls back on failure.

For a stable image-only upgrade, update only `--chart-version`. The packaged
chart uses its `appVersion` as the default engine image tag. Keep
`global.imageTag` and image-specific tags empty unless intentionally testing a
snapshot or splitting image versions.

Image tags resolve in this order:

1. Image-specific tag.
2. `global.imageTag`.
3. Chart `appVersion`.

The generated values reset image tags to empty so an earlier snapshot does not
remain pinned through reused values.

### Guarded snapshot upgrade

The engine repository includes a helper for a published `sha-<commit>` snapshot:

```bash
./deploy/helm/upgrade.sh \
  --context "$CONTEXT" \
  --namespace "$NAMESPACE" \
  --release "$RELEASE" \
  --version "sha-<commit>" \
  --values "$PROJECT_DIR/deploy/values.yaml"
```

The helper requires an existing release, preserves
`analysisCache.generation`, validates the chart, shows image changes, and uses
Helm rollback support. It asks Helm to merge the installed values with every
explicit `--values` overlay into one private temporary candidate, removes only
the known deprecated OAuth controls, and then uses that same candidate for
lint, render, and `helm upgrade --reset-values`. This prevents stale
`scope`, `chatScope`, `privateRepositories`, `OAUTH_SCOPE`, or
`OAUTH_PRIVATE_REPOSITORIES` settings from blocking a guarded upgrade while
preserving all other installed and consumer-owned values. When an image
inspection tool is available, it also checks the rendered image manifests.

Do not use the snapshot helper for a stable release upgrade. Prefer the bundle
wrapper and a published chart version.

### Analysis cache generation

`analysisCache.generation` is a reversible namespace for analysis and recurring
pattern cache keys. Empty preserves the historical key shape. A new value asks
for a non-destructive full AI rebaseline. Returning to an older value can reuse
its unexpired cache entries.

Treat a generation change as an analysis policy operation, not a required image
upgrade step.

## Manual Helm equivalent

The wrapper is preferred because it validates the consumer bundle and protects
install versus upgrade intent. A manual local-chart render follows this shape:

```bash
helm template "$RELEASE" deploy/helm/prow-ai-dashboard \
  --namespace "$NAMESPACE" \
  --values "$PROJECT_DIR/deploy/values.yaml" \
  --set-string project.existingConfigMap= \
  --set-json 'project.skills={}' \
  --set-file "project.config=$PROJECT_DIR/project.yaml" \
  --set-file "project.systemPrompt=$PROJECT_DIR/prompts/system.md"
```

Add each consumer skill with a separate `--set-file project.skills.<key>=<path>`
argument. Escape dots in ConfigMap keys.

A live manual Helm operation must also provide an explicit context, namespace,
release, published chart version, wait behavior, and rollback behavior. Do not
place Secret values in `--set` arguments because they can enter shell history
and Helm release state.

## Configuration reference

The complete commented defaults live in
`deploy/helm/prow-ai-dashboard/values.yaml`. The table below highlights the
main operator controls.

| Value | Purpose |
| --- | --- |
| `global.imageTag` | Shared image override. Empty falls back to chart `appVersion`. |
| `image.*` | Main worker and server image repository, tag, and pull policy. |
| `imagePullSecrets` | Registry pull Secrets. |
| `mode` | `watch` or `cron`. |
| `analysisCache.generation` | Reversible cache-key namespace for a full analysis rebaseline. |
| `persistence.enabled` | Create or mount shared persistent data. |
| `persistence.existingClaim` | Reuse a pre-provisioned RWX PVC. |
| `persistence.retain` | Retain chart-managed data after removal or claim changes. |
| `persistence.storageClass`, `accessMode`, `size` | Shared-volume provisioning. The access mode must be `ReadWriteMany`. |
| `project.config`, `project.systemPrompt`, `project.skills` | Inline or `--set-file` project bundle. The wrapper manages these. |
| `project.existingConfigMap` | Reuse an external ConfigMap instead of the chart-managed bundle. |
| `ai.enabled`, `ai.api`, `ai.endpoint`, `ai.model` | AI provider coordinates. |
| `ai.contextWindowTokens` | Optional operator-provided provider context window. Set only with endpoint evidence. |
| `ai.existingSecret`, `ai.tokenSecretKey` | Existing provider token Secret and key. |
| `ai.githubReadTokenSecretName`, `ai.githubReadTokenSecretKey` | Optional separate read-only GitHub token for source grounding. |
| `orka.agentAnalysisShadow.*` | Disabled private Agent comparison, exact admission identity, limits, and private ledger claim. |
| `fetcher.schedule` | Cron schedule. Used only in cron mode. |
| `fetcher.suspend` | Suspend CronJob starts. Keep true when preserving a safe cron rollback from watch mode. |
| `fetcher.watchInterval`, `fetcher.reconcileInterval` | Watch refresh and full reconciliation cadence. |
| `fetcher.buildsPerJob`, `fetcher.workers`, `fetcher.timeout` | Fetch depth, concurrency, and discovery or artifact budget. |
| `fetcher.extraEnv` | Additional environment variables, preferably through `secretKeyRef`. |
| `server.replicaCount` | Server replicas. Persistent private state requires a suitable shared filesystem. |
| `server.chat.*` | Authenticated analysis conversation settings. |
| `server.security.hsts.enabled` | Helm HSTS behavior. Keep enabled for deployed HTTPS origins. |
| `server.development.allowInsecureHTTP` | Explicit local HTTP acknowledgement required to disable HSTS outside OAuth cookie testing. |
| `server.development.allowInsecureCookies` | Local HTTP OAuth testing only. Never enable on a deployed dashboard. |
| `server.actions.*` | OAuth or proxy authentication and guarded GitHub writes. |
| `server.service.*` | ClusterIP, NodePort, or LoadBalancer exposure. |
| `ingress.*` | Optional ingress resources. |
| `networkPolicy.enabled`, `networkPolicy.ingress` | Complete server ingress rules. Empty ingress denies all traffic when enabled. |
| `podSecurityContext`, `securityContext` | Chart-owned non-root and restricted container defaults. |

Use the full chart values that match the installed chart version before adding a
field not present in the generated consumer file.

## Persistent storage requirements

The writer and server mount the same claim. The storage backend must support:

- `ReadWriteMany` mounts.
- Atomic rename.
- File and directory synchronization.
- Advisory file locking when analysis chat is enabled.

The volume may contain private AI cache, traces, chat transcripts, usage files,
and action state in addition to public dashboard JSON. Restrict PVC access and
backups to dashboard operators.

Do not point multiple releases at the same claim. Do not delete a retained PVC
until rollback and data-retention requirements are resolved.

Agent analysis shadowing uses a second claim, defaulting to `ReadWriteOnce`,
mounted only into the single writer at `/private/agent-analysis-shadow`. The
chart rejects reuse of the public dashboard claim. The server never mounts the
shadow claim, and Helm retains chart-managed shadow data by default.

## Secure server origin topologies

Authenticated chat and actions should not use an unrestricted public origin.
Prefer these topologies in order:

1. ClusterIP behind an in-cluster ingress or SSO proxy.
2. An internal LoadBalancer with provider-specific private annotations.
3. A private-link origin configured outside the chart.
4. A source-restricted public LoadBalancer as a last resort.

For a public LoadBalancer, configure source ranges and NetworkPolicy. If the
chart cannot establish an origin restriction, authenticated features require
`server.service.publicOriginAcknowledged=true`. That acknowledgement is not
proof of runtime isolation.

Example restricted origin:

```yaml
server:
  actions:
    enabled: true
  service:
    type: LoadBalancer
    loadBalancerSourceRanges:
      - 10.0.0.0/8
    externalTrafficPolicy: Local

networkPolicy:
  enabled: true
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ingress-system
      ports:
        - protocol: TCP
          port: 8080
```

Service annotations are passed through but are not accepted as proof of origin
restriction. Verify that the expected proxy can reach the Service and that a
path that should be denied cannot.

The public read endpoints `/data/*`, `/api/capabilities`, and `/healthz` remain
unauthenticated. Authentication protects chat, private traces, and write
actions.

The chart rejects `HSTS_ENABLED` and `COOKIE_INSECURE` in `server.extraEnv`.
HSTS is enabled by default. Disabling it requires explicit local HTTP
acknowledgement with `server.development.allowInsecureHTTP=true`; local OAuth
may instead use `server.development.allowInsecureCookies=true`. Deployed
dashboards should keep both development values false. After deployment, verify
that the public reverse proxy preserves the header:

```bash
curl -fsSI https://dashboard.example.com/ | grep -i '^strict-transport-security:'
```

## Analysis chat

Analysis chat is Kubernetes-native and persists owner-bound session state on the
shared volume. Multiple server replicas can serve the same session when the
storage backend provides the required filesystem semantics.

Chat reuses `server.actions` authentication settings but does not enable GitHub
writes or require a bot token. Chat-only OAuth requests `read:user`. Configure
session TTL, deployment-wide limits, per-owner limits, and per-turn timeouts
under `server.chat`.

Correction promotion is disabled by default. When enabled, it writes a private
audit ledger and a public correction overlay without rewriting fetched job
JSON.

See [Server mode](server.md) for OAuth, proxy mode, chat, correction review, and
trusted-origin configuration.

## Authenticated write actions

Write actions are disabled by default. Enable them only after configuring:

- `server.actions.enabled=true`.
- `server.actions.mode` as `oauth` or `proxy`.
- A non-empty `server.actions.admins` allowlist.
- Existing Secrets for OAuth or proxy credentials.
- A secure origin topology.

OAuth and proxy modes authenticate the initiating admin separately from the
GitHub write credential. OAuth uses `read:user`; `BOT_TOKEN` performs enabled
writes in both modes. Keep credentials in existing Kubernetes Secrets rather
than inline values.

See [Server mode](server.md) for setup and threat boundaries.

The bot-token split changes `action_preview_state.json` to version 5. During an
upgrade, every version 1 through 4 synchronous preview is invalidated because
its owner was bound to a credential hash rather than the initiating login.
Admins must generate a new preview. A rollback to a pre-v5 server fails closed
on v5 preview state; roll forward, or move the private preview state aside and
regenerate previews after confirming no action is in progress. The separate
`.action-write-audit/state.json` ledger remains private and must be retained
across upgrades and rollbacks. Its dot-prefixed directory is rejected by both
pre-v5 and current data-serving rules, so rollback does not expose the ledger.

## Email notification integration

Enable email delivery in `project.yaml`. If the SMTP relay requires a password,
store it in a Secret and reference it through `fetcher.extraEnv`:

```yaml
fetcher:
  extraEnv:
    - name: EMAIL_SMTP_PASSWORD
      valueFrom:
        secretKeyRef:
          name: dashboard-smtp
          key: password
```

The SMTP host must be reachable from the worker or CronJob. When authenticated
review links are enabled, also provide the password to the server through
`server.extraEnv`. See [Email notifications](notifications.md).

## Scheduled issues and fix PRs

Automatic issues and fix PRs are disabled by default. Enable the feature in
`project.yaml` and provide write-scoped tokens through Secret-backed
`fetcher.extraEnv` entries.

The standard distroless engine image does not contain git or the fix-generation
runtime. Use a reviewed writer image before enabling scheduled fix PRs. Filing
issues and marking failures resolved do not require that writer image.

A missing token causes the optional action to be skipped without failing the
fetch pass. See [GitHub issues](github-issues.md) and
[Agent-proposed fix PRs](fix-prs.md) for permissions, identity, and guardrails.

## Reusing external project configuration

If an operator manages the project ConfigMap outside the chart, set
`project.existingConfigMap`. The ConfigMap must include `project.yaml`,
`system.md`, and every configured skill key.

If an operator manages provider credentials outside the chart, set
`ai.existingSecret` and `ai.tokenSecretKey`. Do not combine external Secret
management with inline token values.

The bundle wrapper intentionally clears `project.existingConfigMap` because it
makes the local consumer bundle authoritative. Use manual Helm when external
ConfigMap ownership is required.

## Private operational files

The shared volume contains public dashboard data and private operational state.
The server blocks private files from `/data`, including:

- AI cache and trace files.
- Notification, issue, fix, and remediation state.
- Chat sessions and private investigation state.
- Private Prow coverage catalogs.
- AI usage ledgers.

Pages publication strips these files. Kubernetes operators must protect the PVC,
backups, and server access.

The optional Agent shadow ledger is stricter: it lives on a separate private PVC
outside the server data directory. It stores bounded comparison and cleanup
telemetry and is never served through `/data` or an authenticated API.

When AI usage accounting is enabled, the worker writes
`ai_usage_fetcher.json` and the server writes `ai_usage_server.json`. Both stay
private. Review ownership before scaling a component that writes one of these
ledgers.

## Related references

- [Kubernetes quickstart](kubernetes.md)
- [Experimental Orka maintainer reference](orka.md)
- [Server mode](server.md)
- [Project configuration](project-configuration.md)
- [Troubleshooting](troubleshooting.md)
- [Releasing](releasing.md)

## Agent Sandbox Fix runtime

The `agentSandbox.fixRuntime` chart section is disabled by default. It wires the
dashboard to a consumer-installed Kubernetes SIG Agent Sandbox controller but
never installs that controller or its CRD.

The Sandbox returns a patch rather than writing GitHub directly. The dashboard
independently reapplies it to the pinned source revision, so the server and any
scheduled fix reconciler use `agentSandbox.fixRuntime.dashboardImage`. The
published `remote-fixer` image contains the normal engine binaries, SPA, CA
certificates, and git. It intentionally omits OpenCode, srt, and model
credentials.

When enabled, the chart can create:

- one dashboard client ServiceAccount in the release namespace;
- one namespace-scoped Role and RoleBinding in the consumer-owned execution
  namespace;
- one tokenless workload ServiceAccount in that namespace; and
- one fail-closed ValidatingAdmissionPolicy and binding for dashboard-created
  v1beta1 Sandboxes.

The Role permits only Sandbox create/get/list/watch/delete, Pod
get/list/watch, and Pod-log get. It grants no Secret, direct Pod creation, exec,
attach, port-forward, Service, PVC, node, or cluster-admin access.

The admission policy pins the requester, namespace, content-addressed identity,
immutable executor image, RuntimeClass, workload ServiceAccount, Pod and
container security contexts, `RuntimeDefault` AppArmor and seccomp, resource
bounds, request-only environment, emptyDir-only storage, disabled Service/PVC
behavior, and Delete policy and Pod deadline. AppArmor has no chart override and cannot be
set to `Unconfined`.
The request payload remains opaque base64 data to Kubernetes admission, so the
engine separately validates its version, immutable SHA, gateway, commands,
bounds, and result contract before creation and after retrieval.

A deployed configuration requires a consumer-operated HTTPS model gateway, a
secure Kata, gVisor, or equivalent RuntimeClass, and nodes that support the
requested AppArmor policy. Internal service certificates must chain to a CA in
the immutable executor image. Alternatively, a privately resolved public FQDN
with a publicly trusted certificate can be explicitly acknowledged with
`modelGateway.publicCAPrivateDNS: true`. Standard `runc` is supported only by
the disposable local
lifecycle evaluation and is not a hostile-code boundary. Docker Desktop kind
omits AppArmor through test-only code in both the canonical preflight and
Sandbox shapes; it does not validate production AppArmor enforcement.
The consumer separately owns the execution namespace, Agent Sandbox release,
RuntimeClass, node pools, model gateway, image publication, registry access,
egress enforcement, quotas, LimitRanges, and NetworkPolicies.

## Agent Sandbox OpenCode analyzer

The `agentSandbox.analyzer` chart section is a private, disabled-by-default
deployment boundary for the thin OpenCode analyzer experiment. It does not wire
the fetcher, worker, or server to create analyzer Sandboxes. The in-process
analyzer remains authoritative.

When enabled, the chart can create a dedicated analyzer client ServiceAccount,
narrow Sandbox and Pod-log RBAC in a dedicated existing execution namespace, a
tokenless workload ServiceAccount, a fail-closed admission policy, a
deny-by-default network policy, and a one-Sandbox, one-Pod ResourceQuota. The
quota does not duplicate cluster-owned RuntimeClass overhead. Admission pins the
container resource bounds instead. The chart never creates the namespace,
controller, RuntimeClass, pre-populated input PVC, internal model gateway, or
runtime images.

The analyzer executor and stager images must use immutable SHA-256 digests. The
admission policy pins both images, the exact read-only input PVC, the secure
RuntimeClass and workload identity, resource bounds, AppArmor and seccomp, the
single stager and executor shape, read-only source and artifact mounts, and the
separate result-only writable volume. The executor never mounts the input PVC
directly.

Network policy denies ingress and public egress. Kubernetes policy mode selects
the gateway with namespace and Pod labels. Cilium mode permits only DNS and the
configured Kubernetes gateway Service and port, so the gateway must
independently authenticate the analyzer ServiceAccount. The quota assumes the
namespace is dedicated to this experiment.

See [Agent Sandbox OpenCode analyzer](agent-sandbox-opencode-analyzer.md) for
the workspace, result, authority, and benchmark boundaries.

## Agent Sandbox causal critic

The `agentSandbox.causalCritic` chart section is independent from Fix PR
execution and defaults to disabled. It runs a private sampled review only after
in-process authoritative analysis. It does not publish a replacement diagnosis
or participate in any write action.

The chart creates separate critic RBAC and a separate fail-closed admission
policy. Critic Sandboxes use an immutable purpose-built image, a tokenless
workload ServiceAccount, a secure RuntimeClass, no volumes, one request
environment value, one container, and no public repository access. A required network policy denies ingress and public egress. Standard Kubernetes
peer selection is the default. `mode: cilium` is available for AKS Cilium/Kata
and limits egress to cluster DNS plus the configured cluster-internal gateway
port; the gateway must separately authorize the critic ServiceAccount.

The consumer must provide a separate private ledger PVC. The worker or fetcher
mounts this claim, but the server and public data path do not. The gateway must
authorize the critic workload through an infrastructure identity outside the
executor process. Network reachability alone is not authentication.

See [Agent Sandbox causal critic](agent-sandbox-causal-critic.md) for the result
contract, finalization rules, cold benchmark workflow, and remaining promotion
gates.

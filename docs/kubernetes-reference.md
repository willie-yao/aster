# Kubernetes operator reference

This page contains the architecture, deployment behavior, upgrade rules, and
advanced chart configuration that do not belong in the first-run path. Start
with the [Kubernetes quickstart](kubernetes.md) or the generated
`deploy/README.md`.

Authoritative failure analysis always runs in-process next to the worker or
CronJob. The chart has no failure-analysis runtime selector.

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

Published engine releases attach checksum-listed `aster-<tag>-<target>` CLI
binaries for Linux and macOS on amd64 and arm64. A normal contributor downloads
the matching `aster` artifact and does not clone or build the engine repository.

The supported deployment wrapper is part of the `aster` CLI:

```text
aster kubernetes doctor
aster kubernetes install
aster kubernetes upgrade
```

`doctor` renders the selected chart locally and performs live read-only checks
against an explicit Kubernetes context. It uses only Kubernetes `GET` and
`LIST`, metadata-only Secret existence and Helm release-label requests, and
local Helm `template`. It does not read Secret payloads, Helm values, or Helm
manifests from the cluster. Pass `-action install` or `-action upgrade` to detect
release-state conflicts before the write command. See
[Kubernetes platform setup](kubernetes-platform.md) for the
resource boundary and verification limits.

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

## Development and release references

Use a local chart only while testing engine chart changes. `--dry-run` invokes
local `helm template`, does not contact the cluster, and does not print rendered
Secret values. See [Local development](development.md) for build and chart
commands.

Published releases provide paired application and platform charts, CLI assets,
and immutable image identities. Production rollbacks must use a recorded chart
version and immutable image references. Agent Sandbox executor images require
OCI digests. See [Releasing](releasing.md) for publication and provenance.

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

New generated values omit image tags so the first install follows the chart
`appVersion`. Keep any image tags already present in reused values empty unless
the deployment intentionally pins a snapshot or split version.

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
helm template "$RELEASE" deploy/helm/aster \
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
`deploy/helm/aster/values.yaml`. Generated consumer values contain only reviewed
storage and provider decisions plus the intentional `fetcher.timeout: 120m` and
`fetcher.suspend: true` divergences. All other settings inherit chart defaults.
The table below highlights the main operator controls.

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
| `ai.reasoningEffort` | Optional provider reasoning effort. Empty uses the provider default. |
| `ai.contextWindowTokens` | Optional operator-provided provider context window. Set only with endpoint evidence. |
| `ai.existingSecret`, `ai.tokenSecretKey` | Existing provider token Secret and key. |
| `ai.githubReadTokenSecretName`, `ai.githubReadTokenSecretKey` | Read-only GitHub token for analysis source grounding and pull request triage. Rendered whether or not `ai.enabled` is set. Required for triage, which otherwise reads GitHub anonymously at 60 requests per hour. |
| `fetcher.schedule` | Cron schedule. Used only in cron mode. |
| `fetcher.suspend` | Suspend CronJob starts. Keep true when preserving a safe cron rollback from watch mode. |
| `fetcher.watchInterval`, `fetcher.reconcileInterval` | Watch refresh and full reconciliation cadence. |
| `fetcher.buildsPerJob`, `fetcher.workers`, `fetcher.timeout` | Fetch depth, concurrency, and discovery or artifact budget. Fetch depth also sets the window every aggregation, correlation, and AI pass reads. The run history strip plots a longer arc than this: builds that age out of the window are kept for display, without their test results, up to 40 runs per job. |
| `fetcher.extraEnv` | Additional environment variables, preferably through `secretKeyRef`. Carries `ISSUE_TOKEN` for issue recovery and `ASTER_APP_ID` / `ASTER_APP_PRIVATE_KEY` for the optional bot comment on new pull requests. |
| `server.replicaCount` | Server replicas. Persistent private state requires a suitable shared filesystem. |
| `server.chat.*` | Authenticated analysis conversation settings. Each model turn defaults to `10m`; `server.chat.timeout` accepts values up to `30m`. |
| `server.pullRequestEscalation.enabled` | Authenticated on-demand analysis of one unexplained pull request failure. Requires `ai.enabled` and `pull_requests.enabled` in `project.yaml`. Does not enable writes. |
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

## Private operational files

The shared volume contains public dashboard data and private operational state.
The server blocks private files from `/data`, including AI cache and traces,
notifications and action state, chat transcripts, private coverage catalogs,
and usage ledgers. Pages publication strips them.
Protect the PVC, backups, and server access.

## Agent Sandbox Fix runtime

`agentSandbox.fixRuntime` is disabled by default. It connects Aster to a
consumer-installed Agent Sandbox controller and never installs the controller,
CRD, execution namespace, secure RuntimeClass, node infrastructure, provider
Secret or gateway, or runtime image.

The project owns generation limits and exact validators under
`ai.fix_prs.agent_runtime`. Helm owns the runtime namespace, immutable images,
ServiceAccounts, network policy, provider Secret reference, CA trust, and
platform resources. Those two configurations must agree. The schema rejects
stale duplicate execution bounds under `agentSandbox.fixRuntime`.

The Sandbox receives public pinned source and returns a patch plus ordered command
results. It receives no GitHub credential or dashboard PVC. The dashboard uses
the immutable `remote-fixer` image only to reapply and verify the patch at the
pinned revision; it never runs target validation commands.

Required platform properties:

- a dedicated existing execution namespace;
- a non-empty secure RuntimeClass accepted for hostile repository code;
- separate client and tokenless workload ServiceAccounts;
- immutable executor and dashboard image digests;
- deny-by-default ingress and reviewed provider egress;
- a dedicated inference credential or authenticated tokenless gateway;
- exact admission identity, resource, mount, environment, and command policy.

The Fix client ServiceAccount is used only by the server. Its default name derives
from the first 32 characters of the release fullname, so two releases sharing a
namespace must have fullnames that differ within that prefix. Override it with
`agentSandbox.rbac.fixClientServiceAccountName` when necessary.

Direct bearer mode references one existing Secret key. The chart and dashboard
do not read or print the value. Gateway mode keeps the executor tokenless but
still requires gateway-side workload authorization. Optional CA bundles use the
Fix-only ConfigMap, digest, RBAC, and mount contract. See
[Kubernetes platform setup](kubernetes-platform.md#secure-runtime-contract) for
the provider-neutral isolation boundary and [Fix PR generation](fix-prs.md) for
the user workflow and configuration example.

## Related references

- [Kubernetes quickstart](kubernetes.md)
- [Kubernetes platform setup](kubernetes-platform.md)
- [Flux GitOps deployment](kubernetes-gitops.md)
- [Platform chart README](../deploy/helm/aster-platform/README.md)
- [Server mode](server.md)
- [Project configuration](project-configuration.md)
- [Troubleshooting](troubleshooting.md)
- [Releasing](releasing.md)

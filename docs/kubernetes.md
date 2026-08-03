# Running the dashboard Kubernetes-native

Use the Helm chart when the dashboard needs a private in-cluster model endpoint,
persistent shared data, or authenticated server actions. For a public read-only
site without a cluster, use [GitHub Actions and Pages](github-pages.md).

The chart runs a worker or CronJob beside a small server that serves the SPA and
the same `/data/*.json` contract as Pages. The server also exposes
`/api/capabilities` for server-only features. See [Server mode](server.md) for
the endpoint and authentication reference.

Start with the guided `fetcher onboard` flow and select **Kubernetes with Helm**,
or use `fetcher onboard -mode k8s` with complete flags for automation. The
command runs the real job sweep, renders and validates the scaffold in memory,
and writes nothing before interactive confirmation. `-dry-run` performs the same
checks without writing files.

The generated values contain only the storage class, model connection, AI
enablement, and a safe fetch timeout. The wizard does not install Helm releases,
write Kubernetes Secrets, inspect a cluster, or configure ingress and DNS. The
rest of this guide is an operator reference for production settings and optional
features.

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

## Install Orka as a separate release

The dashboard chart does not install Orka and does not list Orka as a chart
dependency. Install Orka once as a separate cluster-level release before enabling
Orka container analysis, fix generation, or source investigation. Multiple
dashboard releases may share one compatible Orka installation.

The maintained consumer example is
[CAPZ Prow AI Dashboard Orka Demo](https://github.com/willie-yao/capz-prow-ai-dashboard-orka-demo/tree/main/deploy/orka).
It provides an explicit installer, readiness validator, guarded CRD upgrade,
uninstall guidance, OpenCode Agent setup, immutable version fields, and a
fresh-install kind test.

### Release selection

Prefer a published Orka release that contains merge commit
`fde3b7925c367784570fcc36d7a5b3a51747bf10` or later. Verify the tag contains
that commit and publishes the controller, AI worker, general worker, agent
harness-wrapper, and Helm chart. Pin the exact chart version, chart digest, and
all four image digests in the consumer repository before installation. The
reference installer renders every controller and worker image as
`tag@sha256:digest`, and its release upgrade writes all four digest-pinned
references into the target Helm revision.

As of July 28, 2026, `orka-agents/orka` has no tag or GitHub release. The
`0.1.1` version in the chart source is not evidence of a published release. Do
not invent a chart repository URL, publish an unofficial release, or use mutable
`main` or `latest` image tags. The source-commit path is maintainer-only and
non-production: it can package the chart from the exact commit for lint, render,
and temporary kind validation, but it does not provide matching released runtime
images and must not be used for a cloud-cluster installation.

When a verified release exists, prefer the consumer reference's
`deploy/orka/install.sh`, which verifies the chart digest and supplies all four
image digests. A direct Helm equivalent must include the same digest-pinned image
references:

```bash
chart_dir=$(mktemp -d)
helm pull <published-orka-chart> \
  --version <exact-version> \
  --destination "$chart_dir"
chart_package="$chart_dir/orka-<exact-version>.tgz"
actual_chart_digest=$(shasum -a 256 "$chart_package" | awk '{print $1}')
test "$actual_chart_digest" = <verified-chart-sha256>

helm upgrade --install orka "$chart_package" \
  --namespace orka-system \
  --create-namespace \
  -f deploy/orka/values.yaml \
  --set-string 'controller.image.tag=<exact-version>@sha256:<controller-digest>' \
  --set-string 'workers.ai.image.tag=<exact-version>@sha256:<ai-worker-digest>' \
  --set-string 'workers.general.image.tag=<exact-version>@sha256:<general-worker-digest>' \
  --set-string 'workers.harnessWrapper.image.tag=<exact-version>@sha256:<harness-wrapper-digest>' \
  --wait
```

Use the actual publication reference and verified digests from the release. The
consumer installer supports both `sha256sum` and `shasum`; the example above uses
`shasum` only to show the required comparison before Helm reads the local
package. The dashboard Helm command remains separate and must never invoke this
command implicitly.

### Fresh install and readiness

A fresh chart install creates all 12 cluster-scoped Orka CRDs from the chart's
`crds/` directory before the templated release resources. It also manages the
controller, controller Service and RBAC, worker ServiceAccounts and RBAC,
harness-wrapper Deployment and Service, its release-local authentication Secret,
and the persistent store.

After installation, wait for every CRD to become `Established`, then validate the
controller, harness wrapper, REST Service, store PVC, controller permissions for
`agentruntimes` and `substrateactorpools`, worker identities, and pinned runtime
images. Do not broaden a dashboard ServiceAccount to compensate for an Orka
controller installation error.

The chart does not create model credentials or an Agent definition. Create the
Agent model Secret separately, apply a reviewed Agent with the endpoint-specific
model ID, and wait for its `Ready` condition. An OpenCode Agent uses
`spec.runtime.type: opencode`, `OPENAI_BASE_URL`, and `OPENAI_API_KEY` only when
the endpoint requires it. Keep any private-repository clone credential separate
and read-only. Never give the Agent a dashboard GitHub write token.

### CRD-first upgrades

Helm installs files from `crds/` on fresh install, but `helm upgrade` does not
update them. Schedule a maintenance window, stop every client that can create
Orka Tasks, and wait for active Tasks to finish before starting. This includes
all dashboards and any other Orka client sharing the release. The reference
upgrade script serializes CRD lifecycle writers, but it does not block clients
from creating new Tasks, so the operator must keep every producer stopped until
validation finishes.

Upgrade CRDs from the exact target chart before upgrading the Orka controller:

1. Download or locate the exact target chart package and run `helm show crds`.
2. Apply those CRDs server-side with the dedicated `orka-crd-lifecycle` field
   manager.
3. For each CRD, read its current `resourceVersion` and use a JSON Patch that
   tests that version before replacing the complete target `spec`. This removes
   schema fields deleted by the target chart without deleting custom resources.
4. Wait for all 12 CRDs to become `Established`.
5. Only then run `helm upgrade`, wait for the controller and harness wrapper,
   and run the full readiness validation.

Serialize this cluster-wide operation. Do not run a competing CRD apply workflow
and do not replace the guarded spec update with a plain `kubectl apply` that can
retain fields removed from the target schema. The consumer reference implements
this flow in `deploy/orka/upgrade.sh` and records a pre-upgrade resource
inventory.

### Uninstall and multi-release topology

`helm uninstall` removes release-scoped Orka resources, including the
chart-managed store PVC, but Helm retains CRDs installed from `crds/`. The Orka
custom resources also remain in the Kubernetes API. Back up store data before
uninstalling when Task results or sessions must survive.

Deleting a CRD deletes every custom resource for that kind across the cluster.
Treat CRD deletion as a separate destructive operation. Do not automate it as
part of dashboard or Orka uninstall.

One cluster-wide Orka release may serve multiple dashboards. If a cluster needs
multiple Orka releases, every release needs a unique release name or
`fullnameOverride`, an isolated controller namespace, and a distinct non-empty
`controller.watchNamespace`. Do not combine a cluster-wide watcher with
namespace-scoped releases because their admission and reconciliation scopes can
overlap.

## Analysis runtimes and Orka fix generation

See [Orka architecture in prow-ai-dashboard](orka-architecture.md) for the
component, credential, state, and ownership boundaries shared by the analysis,
fix-generation, and source-investigation paths.

### In-process analysis

`analysisRuntime.type: inprocess` is the default and the only recommended
production mode. It works with both `mode: watch` and `mode: cron`. Pages also
uses this runtime.

### Experimental Orka container analysis

`analysisRuntime.type: orka-container` is an opt-in Helm sidegrade for both
`mode: cron` and `mode: watch`. It submits one content-addressed Orka
`type: container` Task per
failure. The analyzer image runs the current dashboard `FailureAnalyzer`, so
prompts, Tool schemas, skills loaded through `LoadForTools`, ranked evidence
planning, evidence coverage, critique, semantic review, cache acceptance, traces,
and `FailureAnalysisResult` stay dashboard-owned. The patched Orka AI worker,
Provider resources, dynamic Tool resources, and analysis worker patches remain
removed.

Use this mode only for a concrete lifecycle requirement such as per-failure Task
isolation or Task retry history. It has no Pages support or backward
compatibility guarantee and is not recommended over in-process analysis.

Set `analysisCache.generation` to request a non-destructive full AI rebaseline.
The chart passes it to the worker or CronJob and to analyzer Tasks. Empty keeps
existing keys unchanged; returning to an older value reuses its unexpired cache.

```bash
helm upgrade capz deploy/helm/prow-ai-dashboard \
  --kube-context h100 \
  --namespace capz-dynamo \
  --reuse-values \
  --set analysisCache.generation=2
```

Both analysis runtimes commit private cache and trace checkpoints after the
individual-analysis phase. The checkpoint uses the existing cache entry schema
and acceptance gates. It does not publish dashboard JSON or side effects. A
post-checkpoint failure reloads state from disk on the next pass, and successful
pattern cache entries from a partially failed correlation pass remain reusable.
Failures before the checkpoint, including project-bundle, state identity,
decryption, cancellation, and systemic result authorization errors, restore the
prior private generation.

```yaml
mode: watch

fetcher:
  watchInterval: 5m
  reconcileInterval: 1h
  suspend: true
  schedule: "0 */6 * * *"

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

Watch mode runs one continuously active worker Deployment, not a CronJob.
`watchInterval` checks the known job list for new builds. `reconcileInterval`
rediscovers jobs and runs notifications, issue filing, and fix generation.
Passes never overlap, so long Task waves delay the next refresh. Never create a
manual fetch Job while the worker exists. `fetcher.suspend` and
`fetcher.schedule` do not render a CronJob in watch mode. Keep them set for a
safe rollback to suspended cron mode.

Inspect the worker with:

```bash
kubectl -n dashboards get deploy,pod -l app.kubernetes.io/component=worker
kubectl -n dashboards logs deploy/capz-prow-ai-dashboard-worker -f
```

Authenticated `/api/fetch-status` responses include the current pass ID and an
identity-free summary of the latest 10 passes. The private history file retains
20 terminal passes. Newly created analyzer Tasks carry safe run, pass,
pass-type, and work-item digest labels. Use the current pass ID from the status
response without adding another CLI dependency:

```bash
kubectl -n <analysis-namespace> get tasks \
  -l prow-ai-dashboard/pass-id=<pass-id> \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,ATTEMPTS:.status.attempts
```

A content-addressed Task adopted from an earlier pass keeps its original labels.
The private `.fetch-status/status.json` mapping and aggregate
`existing_tasks_adopted` count retain that current-pass correlation without
changing the Task, its canonical request hash, or its name.

Before creating analyzer resources, the worker applies private cache entries
that pass the current key, age, investigation-floor, critique, and
malformed-state checks. Model, endpoint, prompt, skill, and transient-streak
changes do not make an otherwise reusable entry stale. `logical_total` still
counts every subject, while `queued`, `new_work`, and `stale_work` describe only
the remaining Task workload.

If private cache is missing, planning first computes the canonical
content-addressed Task identity and checks that exact managed Task in the
release-scoped analysis namespace. Reuse requires a non-deleting succeeded Task,
a durable result reference, the exact current bundle digest and state-key
fingerprint, the current analyzer contract, authenticated encrypted state,
exact agreement between the encrypted cache entry and published result, and the
current investigation and critique gates. Exact planning reuse increments
`exact_results_reused` and creates no Task.

If exact reuse misses, the worker checks up to five newest succeeded managed
Tasks for the same work item. Compatible reuse preserves the same result,
authentication, state, and quality gates while allowing a prior Task identity.
It increments `compatible_results_reused`. Only work that misses every reuse
path is queued. `new_tasks_created` counts Tasks actually created after
planning, `fresh_analyses_completed` counts their accepted results, and
`existing_tasks_adopted` remains the late-adoption fallback for a Task that
appears after planning. An image-only analyzer update keeps the cache key stable
but changes the exact content-addressed Task identity when new execution is
required. Succeeded Tasks are retained for seven days, with at most five newest
compatible candidates per work item.

To return to cron mode without starting a fetch, upgrade with the same chart
and values while changing only the mode and suspension setting:

```bash
helm upgrade capz ./deploy/helm/prow-ai-dashboard -n dashboards \
  --reuse-values \
  --set mode=cron \
  --set fetcher.suspend=true
kubectl -n dashboards get deploy,cronjob \
  -l app.kubernetes.io/instance=capz
```

Verify the worker is gone and the restored CronJob reports `SUSPEND=true`
before considering any manual run.

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

OpenCode requires upstream PR #289, and the complete chart and controller RBAC
require PR #295 at `fde3b792` or later. As of July 28, 2026, Orka has no tagged
release containing those changes. Follow
[Install Orka as a separate release](#install-orka-as-a-separate-release); do not
install older raw manifests or add a supplemental controller RBAC patch. The
dashboard ServiceAccount needs Orka Task access only. Orka labels the project
experimental.

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

## Install and upgrade a consumer bundle

`project.yaml` owns portable project behavior and analysis policy. Workflow
inputs and Helm values own infrastructure, credentials, and execution tuning.
The same project bundle works with Pages, local development, and Kubernetes.
Do not reproduce the `project.yaml` schema in `deploy/values.yaml`.

A Kubernetes consumer repository has this layout:

```text
project.yaml
prompts/system.md
skills/*.yaml
deploy/values.yaml
```

The `skills/` directory is optional unless `ai.consumer_skills` requires it.
The `onboard -mode k8s` subcommand scaffolds this layout and a focused deployment
guide. See [Onboarding a project](onboarding-a-new-project.md).

The supported wrapper is part of the `fetcher` binary. From an engine checkout,
run `make build` to create `bin/fetcher`. The wrapper defaults to the published
OCI chart. Use `--chart deploy/helm/prow-ai-dashboard` when testing a local chart
change. Live installs and upgrades require Helm 4 so failed changes can use
`--rollback-on-failure`.

Before deploying, run `fetcher onboard doctor -project-dir <dir>` when you also
want the persistence, provider, credential-source, and Prow discovery checks.
The bundle wrapper independently runs strict project, prompt, and skill
validation on every install and upgrade.

Keep provider tokens and other credentials in Kubernetes Secrets. Reference
them from `deploy/values.yaml`, for example:

```yaml
ai:
  enabled: true
  endpoint: http://vllm.inference.svc.cluster.local/v1/chat/completions
  model: <model-id>
  existingSecret: capz-dashboard-ai
  tokenSecretKey: AI_TOKEN
```

Do not put Secret values in `project.yaml`, `deploy/values.yaml`, command-line
`--set` arguments, or committed documentation.

### Validate and render without cluster writes

`--dry-run` validates `project.yaml`, the required prompt, selected engine
profiles, consumer skill recipes, and required skill counts. It then runs
`helm template` locally. Rendered manifests are not printed, which avoids
exposing inline values. The command still requires an explicit context so the
same invocation can be used for the later write, but dry-run does not contact
that context.

```bash
./bin/fetcher kubernetes install \
  --project-dir ../capz-prow-ai-dashboard \
  --values deploy/values.yaml \
  --release capz \
  --namespace capz-dynamo \
  --kube-context h100 \
  --chart-version 1.0.0-beta.5 \
  --dry-run
```

### Fresh install

```bash
./bin/fetcher kubernetes install \
  --project-dir ../capz-prow-ai-dashboard \
  --values deploy/values.yaml \
  --release capz \
  --namespace capz-dynamo \
  --kube-context h100 \
  --chart-version 1.0.0-beta.5
```

The command uses `helm upgrade --install --wait --rollback-on-failure` after a
read-only release-state check. `install` refuses an existing release, while
`upgrade` requires one. Release, namespace, and context are required. The
current kubectl or Helm context is never selected implicitly. Relative
`--values` paths are resolved from `--project-dir`.

### Bundle-aware upgrade

Every upgrade includes the current `project.yaml`, `prompts/system.md`, and
`skills/*` automatically. The upgrade reuses deployed values before applying
the consumer values and current bundle:

```bash
./bin/fetcher kubernetes upgrade \
  --project-dir ../capz-prow-ai-dashboard \
  --values deploy/values.yaml \
  --release capz \
  --namespace capz-dynamo \
  --kube-context h100 \
  --chart-version 1.0.0-beta.6
```

For a stable image-only upgrade, change only `--chart-version` to the new
release. The packaged chart sets `appVersion` to the matching image tag, so
`project.yaml` does not need an image-only edit.

The wrapper makes the local bundle authoritative for chart-managed project
configuration. It clears `project.existingConfigMap`, clears any stale
`project.skills` map from values, and passes the current files with `--set-file`.
The chart creates one release-managed ConfigMap with keys `project.yaml`,
`system.md`, and one key per consumer skill. Workload volumes map those keys to
`<project.mountPath>/project.yaml`, `prompts/system.md`, and `skills/<name>` in
the worker, CronJob, and interactive server pods that need the project bundle.
A managed ConfigMap checksum rolls the watch worker and any interactive server
so they load the new bundle. Because the ConfigMap is part of the Helm release,
a separate ConfigMap update
cannot get ahead of a failed Helm operation.

### Guarded unreleased image upgrade

Image tags resolve in this order for the engine, analyzer, and fixer images:

1. The image-specific tag
2. `global.imageTag`
3. The chart `appVersion`

Keep image-specific tags empty unless one image intentionally needs a different
version. For an image-only upgrade to a published `sha-<commit>` snapshot, use
the guarded helper:

```bash
./deploy/helm/upgrade.sh \
  --context h100 \
  --namespace capz-dynamo \
  --release capz \
  --version sha-<commit> \
  --values ../capz-prow-ai-dashboard/deploy/values.yaml
```

The helper requires an existing release. It reuses its values, preserves
`analysisCache.generation`, applies the requested tag through
`global.imageTag`, runs Helm lint and template validation, and shows the image
changes before the upgrade. When `crane`, `docker`, or `skopeo` is available, it
also verifies the rendered image manifests. It then runs Helm with `--wait` and
`--rollback-on-failure` and reports the resulting revision and image references.
It does not clear the cache, update the project bundle, or create a fetch Job.
Use the bundle-aware wrapper when project files change in the same release.

`--reuse-values` also preserves explicit old `image.tag`,
`analysisRuntime.orkaContainer.image.tag`, and `orka.fixRuntime.image.tag`
values. Those values take precedence over both `global.imageTag` and the chart
`appVersion`, so they can prevent an intended snapshot upgrade. Keep the
consumer-owned values file explicit and empty at those paths, then pass it on
every upgrade:

```yaml
global:
  imageTag: ""
image:
  tag: ""
analysisRuntime:
  orkaContainer:
    image:
      tag: ""
orka:
  fixRuntime:
    image:
      tag: ""
```

### Manual Helm equivalent

Manual Helm usage remains supported. Repeat the final `--set-file` argument for
each skill. `--set-json 'project.skills={}'` makes deletion of a local skill
remove a stale map entry from a values file.

```bash
helm upgrade --install capz \
  oci://ghcr.io/willie-yao/charts/prow-ai-dashboard \
  --version 1.0.0-beta.5 \
  --namespace capz-dynamo \
  --create-namespace \
  --kube-context h100 \
  --values ../capz-prow-ai-dashboard/deploy/values.yaml \
  --set-string project.existingConfigMap= \
  --set-json 'project.skills={}' \
  --set-file project.config=../capz-prow-ai-dashboard/project.yaml \
  --set-file project.systemPrompt=../capz-prow-ai-dashboard/prompts/system.md \
  --set-file 'project.skills.cluster\.yaml=../capz-prow-ai-dashboard/skills/cluster.yaml' \
  --wait \
  --rollback-on-failure
```

If you intentionally manage the project ConfigMap outside Helm, continue using
`project.existingConfigMap` and manual Helm. The external ConfigMap must contain
`project.yaml`, `system.md`, and skill keys that match the chart volume items.
The bundle wrapper intentionally selects chart-managed configuration instead.

GHCR packages may require `helm registry login ghcr.io`. Every release also
attaches the packaged chart `.tgz`, which can be supplied with `--chart`.

Source grounding uses a separate optional Secret. Public repositories work
anonymously. For reliable rate limits or private repositories, create a
read-only token Secret and reference it independently from the model Secret:

```yaml
ai:
  githubReadTokenSecretName: dashboard-github-read
  githubReadTokenSecretKey: GITHUB_READ_TOKEN
```

In `orka-container` mode, create the same Secret name and key in the dedicated
analysis namespace. Do not use inline token values in production because Helm
stores them in release metadata.

When the provider's total context window is independently known, set
`ai.contextWindowTokens` in `deploy/values.yaml` to at least `9217`. Use
`128000` for the current Copilot GPT-5 mini deployment. Leave it unset for a
generic endpoint so provider metadata or the bounded fallback remains active.

### Equivalent Pages and Helm controls

Pages workflow inputs and Helm values configure separate deployment paths. They
do not override each other. The equivalent controls and runtime precedence are:

| Concern | Pages workflow | Helm values | Precedence |
| --- | --- | --- | --- |
| Recent builds | `builds` | `fetcher.buildsPerJob` | Selected deployment path only. |
| Fetch concurrency | `workers` | `fetcher.workers` | Selected deployment path only. |
| Whole-fetch timeout | `fetch-timeout` | `fetcher.timeout` | Selected deployment path only. This is distinct from project `ai.timeout`. |
| Provider API, endpoint, model | `ai-api`, `ai-endpoint`, `ai-model` | `ai.api`, `ai.endpoint`, `ai.model` | Non-empty `project.yaml` provider fields win; deployment values are fallbacks. |
| Context window | `ai-context-window-tokens` | `ai.contextWindowTokens` | Deployment value overrides provider metadata. It has no `project.yaml` field. |
| Cache generation | `ai-cache-generation` | `analysisCache.generation` | Non-empty deployment value overrides `project.yaml` `ai.cache_generation`. |
| Presubmits | `include-presubmits` | `fetcher.includePresubmits` | ORed with `project.yaml` `source.include_presubmits`. |

In cron mode, populate data immediately rather than waiting for the schedule by
running the fetcher once. Do not run this command while a watch worker exists:

```bash
kubectl --context h100 -n capz-dynamo create job \
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
kubectl --context h100 -n capz-dynamo port-forward svc/capz-prow-ai-dashboard-server 8080:80
open http://localhost:8080
```

## Configuration reference

Key values (see `deploy/helm/prow-ai-dashboard/values.yaml` for the full set):

| Value | Purpose |
| --- | --- |
| `global.imageTag` | Shared engine, analyzer, and fixer snapshot tag. Each image-specific tag overrides it; empty falls back to the chart `appVersion`. |
| `image.repository`, `image.tag` | Engine image and optional image-specific tag. |
| `mode` | `watch` (continuous worker Deployment, default) or `cron` (scheduled CronJob). |
| `analysisRuntime.type` | `inprocess` by default; `orka-container` is experimental and supports cron or watch mode. |
| `analysisRuntime.orkaContainer.*` | Orka result API, analyzer image, namespace, bounded Task lifecycle, Secret references, encrypted state key, and CPU placement. |
| `fetcher.restartPolicy`, `fetcher.backoffLimit`, `fetcher.activeDeadlineSeconds` | Bound CronJob container restarts, Job retries, and total wall time. Empty restart policy selects `OnFailure` for in-process and `Never` for Orka container analysis; the default deadline is 10 hours. |
| `orka.fixRuntime.enabled` | Mount a ServiceAccount token and grant Orka Task RBAC for `agent_runtime.type: orka` fix generation. |
| `persistence.accessMode` | Must be `ReadWriteMany`. |
| `persistence.storageClass`, `persistence.size` | The shared volume's class and size. |
| `persistence.existingClaim` | Reuse a pre-provisioned PVC instead of creating one. |
| `persistence.retain` | Preserve a chart-managed PVC when it leaves the release. Defaults to `true`. |
| `project.config`, `project.systemPrompt`, `project.skills` | Chart-managed consumer bundle, normally supplied by the wrapper with `--set-file`. |
| `project.existingConfigMap` | Manual path for an external ConfigMap with `project.yaml`, `system.md`, and configured skill keys. |
| `project.materializer.image.*` | Small pinned image used by Orka container analysis to copy ConfigMap-backed project files into a regular-file runtime directory. |
| `ai.enabled`, `ai.endpoint`, `ai.model` | AI analysis and its OpenAI-compatible endpoint. Use `ai.existingSecret` for credentials. |
| `ai.contextWindowTokens` | Optional operator-provided total provider context window. Set only with endpoint evidence. Values must be at least `9217`; use `128000` for the current Copilot GPT-5 mini deployment. |
| `ai.existingSecret`, `ai.tokenSecretKey` | Reuse a Secret holding the token. |
| `ai.githubReadTokenSecretName`, `ai.githubReadTokenSecretKey` | Reuse a separate read-only GitHub token Secret. Omit it for anonymous public-repository grounding. |
| `fetcher.schedule` | Cron schedule (default every 6 hours). `mode: cron`. |
| `fetcher.suspend` | Suspend scheduled CronJob starts while allowing manual Jobs. Retain `true` for rollback when watch mode is active. |
| `fetcher.watchInterval`, `fetcher.reconcileInterval` | Refresh and full-pass cadence. `mode: watch`. |
| `fetcher.buildsPerJob`, `fetcher.workers`, `fetcher.timeout` | Fetch depth and discovery/artifact budget. Orka Task waves use `taskTimeout`; cron mode also uses the Job deadline. |
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
| `server.actions.oauth.privateRepositories` | Request the broad GitHub `repo` scope for private action targets. Defaults to `false`, which uses `public_repo` for actions. Chat-only OAuth always uses `read:user`. |

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
`server.actions` settings, but chat alone does not enable GitHub writes or
require `BOT_TOKEN`. Chat-only OAuth uses only `read:user`; setting
`server.actions.oauth.privateRepositories=true` requires actions to be enabled.

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
  --set server.actions.oauth.sessionKey="$(openssl rand -base64 32)" \
  --set server.actions.oauth.privateRepositories=false
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
private, configure `ai.githubReadTokenSecretName` so the server can verify
returned quotes against the pinned commit.

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
  --set server.actions.oauth.sessionKey="$(openssl rand -base64 32)" \
  --set server.actions.oauth.privateRepositories=false
```

The public-only default requests `public_repo`, which is sufficient to file
issues and create fix branches or pull requests in public repositories. Set
`server.actions.oauth.privateRepositories=true` only when an issue or fix target
is private. That choice requests GitHub's broad `repo` scope for every signed-in
admin.

The old `server.actions.oauth.scope` and
`server.actions.oauth.chatScope` values are rejected. Remove both keys during
upgrade. If the deployment previously requested `repo` and now uses the
public-only default, every admin must sign out and authorize the OAuth App
again. For certainty that GitHub discards the earlier broad grant, revoke the
old OAuth authorization before signing in again. Existing OAuth client, secret,
callback, and allowlist settings stay the same. Proxy mode is unaffected.

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

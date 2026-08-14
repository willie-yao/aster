# Kubernetes deployment quickstart

Use Kubernetes when the model endpoint is private to the cluster, dashboard data
must persist on shared storage, or you need authenticated server features. For a
public read-only dashboard without a cluster, use
[GitHub Actions and Pages](github-pages.md).

Start with `fetcher onboard` and select **Kubernetes with Helm**. The generated
`deploy/README.md` contains project-specific commands. Platform administrators
should first follow the [platform administrator guide](kubernetes-platform-administrator.md).
CAPZ contributors should use the [contributor deployment guide](kubernetes-contributor-deployment.md).
This page remains the combined quickstart reference.

The default deployment uses the supported in-process analysis runtime.
Experimental external runtimes are outside normal onboarding. Add optional
features only after the basic dashboard works.

The generated values are a curated starting configuration. Common settings are
active, while cron, authentication, ingress, NetworkPolicy, resources, and
placement remain commented examples. Comments do not pin chart defaults. The
header also links compatible YAML editors to the schema at the selected engine
reference. If the installed chart version differs, use the schema and complete
values that ship with that chart version.

## Prerequisites

You need:

- A generated Kubernetes consumer scaffold.
- The published `fetcher` CLI matching the chart release.
- `kubectl`, Helm 4, `curl`, `awk`, `install`, `python3`, and either
  `sha256sum` or `shasum`.
- An explicit Kubernetes context and application namespace supplied by the
  platform administrator.
- A cluster with the platform bundle, supported Agent Sandbox release, secure
  RuntimeClass, compatible nodes, and RWX storage already prepared.
- Existing Secret names provisioned through the organization Secret manager.

Run the commands below from the consumer repository root:

```bash
export PROJECT_DIR="$PWD"
export CLI_VERSION="<published-engine-tag>"
export RELEASE="<dashboard-release>"
export NAMESPACE="<dashboard-namespace>"
export CONTEXT="<your-kubernetes-context>"
export EXECUTION_NAMESPACE="<release-dedicated-execution-namespace>"
export PUBLIC_URL="<https-public-dashboard-url>"
export EXPECTED_JOB="<expected-capz-job-name>"
export CHART_VERSION="${CLI_VERSION#v}"
```

Keep the context explicit in every cluster command. Use an engine tag and both
chart versions from the same release.

Download and verify the published CLI rather than cloning and building the
engine repository:

```bash
export CLI_DIR="$HOME/.local/share/prow-ai-dashboard/$CLI_VERSION"
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) CLI_TARGET=linux-amd64 ;;
  Linux-aarch64|Linux-arm64) CLI_TARGET=linux-arm64 ;;
  Darwin-x86_64) CLI_TARGET=darwin-amd64 ;;
  Darwin-arm64) CLI_TARGET=darwin-arm64 ;;
  *) printf 'Unsupported CLI platform\n' >&2; exit 1 ;;
esac
CLI_ASSET="prow-ai-dashboard-fetcher-${CLI_VERSION}-${CLI_TARGET}"
RELEASE_URL="https://github.com/willie-yao/prow-ai-dashboard/releases/download/${CLI_VERSION}"
install -d -m 755 "$CLI_DIR"
if (
  set -euo pipefail
  DOWNLOAD_DIR=$(mktemp -d "${TMPDIR:-/tmp}/prow-cli-download.XXXXXX")
  cleanup_download() {
    find "$DOWNLOAD_DIR" -type f -delete 2>/dev/null || true
    rmdir "$DOWNLOAD_DIR" 2>/dev/null || true
  }
  trap cleanup_download EXIT
  curl --fail --location "$RELEASE_URL/$CLI_ASSET" --output "$DOWNLOAD_DIR/$CLI_ASSET"
  curl --fail --location "$RELEASE_URL/SHA256SUMS" --output "$DOWNLOAD_DIR/SHA256SUMS"
  cd "$DOWNLOAD_DIR"
  CHECKSUM_LINE=$(awk -v asset="$CLI_ASSET" '$2 == asset {print}' SHA256SUMS)
  test -n "$CHECKSUM_LINE"
  if command -v sha256sum >/dev/null && \
    printf '%s\n' "$CHECKSUM_LINE" | sha256sum -c - 2>/dev/null; then
    :
  elif command -v shasum >/dev/null; then
    printf '%s\n' "$CHECKSUM_LINE" | shasum -a 256 --check
  else
    printf 'sha256sum or shasum is required\n' >&2
    exit 1
  fi
  install -m 0755 "$CLI_ASSET" "$CLI_DIR/$CLI_ASSET"
); then
  export FETCHER="$CLI_DIR/$CLI_ASSET"
else
  printf 'CLI download or verification failed\n' >&2
  exit 1
fi
```

No engine source checkout or local chart is required for a published release.

## 1. Inspect and edit `deploy/values.yaml`

The generated file is the main deployment configuration. Active values are
common settings owned by the consumer. Commented values document optional
features without pinning their current chart defaults.

At minimum, review:

```yaml
mode: watch

persistence:
  storageClass: "<your-rwx-storage-class>"
  accessMode: ReadWriteMany
  size: 1Gi

ai:
  enabled: true
  api: chat_completions
  endpoint: "<provider-endpoint>"
  model: "<model-id>"
  reasoningEffort: "" # optional: none, low, medium, high, xhigh, or max
  existingSecret: "<ai-secret-name>"
  tokenSecretKey: AI_TOKEN
```

For the first install:

- Keep `mode: watch` unless you specifically need a scheduled CronJob.
- Set `persistence.storageClass`, or configure `persistence.existingClaim`.
- Confirm the AI API, endpoint, model, Secret name, and Secret key.
- Leave chat, write actions, ingress, and NetworkPolicy disabled.

Never put provider tokens in `project.yaml`, `deploy/values.yaml`, command-line
`--set` arguments, or committed documentation.

Experimental Agent Sandbox OpenCode runtimes remain disabled by default. If one
is explicitly enabled, direct mode is the provider default. Direct bearer mode
must reference a dedicated inference-only Secret that already exists in the
Agent Sandbox execution namespace. Helm stores only the Secret name and key and
never copies, reads, or prints the value. Gateway mode remains available for a
tokenless Chat Completions workload. Native Responses requires direct bearer
auth with pinned OpenCode 1.18.2. Do not reuse the dashboard AI Secret, bot
token, Fix token, OAuth credentials, repository credentials, or a general
GitHub PAT.

Inspect every chart value available in the selected release:

```bash
helm show values \
  oci://ghcr.io/willie-yao/charts/prow-ai-dashboard \
  --version "$CHART_VERSION"
```

## 2. Validate the consumer bundle and live platform

Run the static consumer doctor first:

```bash
"$FETCHER" onboard doctor \
  --project-dir "$PROJECT_DIR"
```

Then run the live read-only Kubernetes doctor with the exact chart and intended
operation. It never uses the current context implicitly:

```bash
"$FETCHER" kubernetes doctor \
  --action install \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  -chart oci://ghcr.io/willie-yao/charts/prow-ai-dashboard \
  --chart-version "$CHART_VERSION"
```

For an existing release, use `-action upgrade`. The doctor validates the local
bundle and chart render, reads Helm release status from metadata-only release labels, and performs
Kubernetes `GET` and `LIST` checks. Secret checks use metadata-only requests. Secret key
names and values are intentionally not inspected. Fix deterministic blockers
before install or upgrade. Warnings and unverified assumptions identify facts
that require platform or real-AKS acceptance.

See [Kubernetes platform ownership](kubernetes-platform-ownership.md) for the
cluster-admin, platform-bundle, application-chart, consumer, and Secret-manager
boundaries.

Doctor validates the project, prompt, deployment values, credential source, and
Prow discovery. It does not contact the model endpoint or inspect the cluster.

Fix all failures before continuing. Review warnings that depend on external
configuration.

## 3. Render locally without cluster writes

The deployment wrapper validates `project.yaml`, `prompts/system.md`, consumer
skills, and Helm values, then runs `helm template` locally:

```bash
"$FETCHER" kubernetes install \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION" \
  --dry-run
```

The command requires a non-empty context so the same command can be used for the
live install, but `--dry-run` does not contact that context and does not print
rendered Secrets.

## 4. Select the context and storage class

List local contexts:

```bash
kubectl config get-contexts
```

Confirm that the selected context reaches the intended cluster:

```bash
kubectl --context "$CONTEXT" cluster-info
```

List available storage classes:

```bash
kubectl --context "$CONTEXT" get storageclass
```

Kubernetes does not expose access-mode support directly on a StorageClass.
Confirm with the storage driver documentation that the selected class supports
`ReadWriteMany`, then update `persistence.storageClass`. Common examples include
Azure Files, EFS, and Filestore CSI classes, but names vary by cluster.

If using an existing claim, confirm it is RWX and in the dashboard namespace:

```bash
kubectl --context "$CONTEXT" \
  --namespace "$NAMESPACE" \
  get pvc <existing-claim>
```

## 5. Confirm the platform handoff

The platform administrator has already created the application namespace,
installed the platform prerequisites, and provisioned the existing Secret names
through the organization Secret manager. The contributor does not create or copy
Secret values as part of application deployment.

Rerun the live doctor immediately before the write command. It validates
namespace, platform binding, storage, Secret metadata, release state, and the
configured application topology without creating resources.

### Install the dashboard

```bash
"$FETCHER" kubernetes install \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION"
```

The wrapper refuses to install over an existing release. It includes the current
project, prompt, and skills in the chart-managed bundle, waits for readiness,
and rolls back a failed install.

## 6. Verify the first fetch

Check the dashboard workloads and persistent claim:

```bash
kubectl --context "$CONTEXT" \
  --namespace "$NAMESPACE" \
  get deploy,pod,pvc
```

Follow the watch worker through its stable release and component labels:

```bash
WORKER_DEPLOYMENT=$(kubectl --context "$CONTEXT" \
  --namespace "$NAMESPACE" \
  get deployment \
  -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=worker" \
  -o jsonpath='{.items[0].metadata.name}')

test -n "$WORKER_DEPLOYMENT"
kubectl --context "$CONTEXT" \
  --namespace "$NAMESPACE" \
  logs -f "deployment/$WORKER_DEPLOYMENT"
```

A successful first run has:

- A `Bound` RWX PVC.
- Ready worker and server Deployments.
- A completed fetch without a systemic error.
- A published `/data/manifest.json`.
- At least one expected job in the dashboard.

If a pod is not ready, inspect events before changing timeouts:

```bash
kubectl --context "$CONTEXT" \
  --namespace "$NAMESPACE" \
  describe pod <pod-name>
```

## 7. Open the dashboard

Resolve and forward the ClusterIP Service through its stable labels:

```bash
SERVER_SERVICE=$(kubectl --context "$CONTEXT" \
  --namespace "$NAMESPACE" \
  get service \
  -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=server" \
  -o jsonpath='{.items[0].metadata.name}')

test -n "$SERVER_SERVICE"
kubectl --context "$CONTEXT" \
  --namespace "$NAMESPACE" \
  port-forward "service/$SERVER_SERVICE" 8080:80
```

Open <http://localhost:8080> in a browser. You can also verify the data endpoint:

```bash
curl --fail http://localhost:8080/data/manifest.json
```

## 8. Upgrade

Review the target release notes and set the new published chart version:

```bash
export CHART_VERSION="<new-published-chart-version>"
```

Validate first:

```bash
"$FETCHER" kubernetes upgrade \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION" \
  --dry-run
```

Then perform the live upgrade:

```bash
"$FETCHER" kubernetes upgrade \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION"
```

Every upgrade includes the current project, prompt, and skills. The wrapper
starts from the new chart defaults, reapplies the prior user-supplied values,
then applies the current consumer values and bundle. It waits for readiness and
rolls back on failure.

## Optional next steps

Keep the first deployment small. Add optional features one at a time:

- [Optional features](optional-features.md) for the supported order to add chat,
  File Issue, Mark Resolved, notifications, and issue automation.
- [Kubernetes operator reference](kubernetes-reference.md) for architecture,
  watch and cron behavior, manual Helm, configuration, and upgrade internals.
- [Server mode](server.md) for authentication, chat, and write-action details.
- [Troubleshooting](troubleshooting.md).
- [Complete documentation map](README.md) for experimental and contributor
  references.

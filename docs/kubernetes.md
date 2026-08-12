# Kubernetes deployment quickstart

Use Kubernetes when the model endpoint is private to the cluster, dashboard data
must persist on shared storage, or you need authenticated server features. For a
public read-only dashboard without a cluster, use
[GitHub Actions and Pages](github-pages.md).

Start with `fetcher onboard` and select **Kubernetes with Helm**. The generated
`deploy/README.md` contains project-specific commands. This page shows the same
first-run path with generic variables.

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
- Go 1.25 or a built `fetcher` binary.
- `kubectl`.
- Helm 4 for live install and rollback support.
- `curl` for the optional local data-endpoint check.
- A Kubernetes cluster with a `ReadWriteMany` storage class or an existing RWX
  claim.
- A published dashboard chart version.
- An AI endpoint reachable from the dashboard namespace when AI is enabled.

Run the commands below from the consumer repository root. Set the deployment
values once:

```bash
export PROJECT_DIR="$PWD"
export ENGINE_DIR="$HOME/src/prow-ai-dashboard"
export ENGINE_REF="<engine-ref-used-by-the-scaffold>"
export RELEASE="<dashboard-release>"
export NAMESPACE="<dashboard-namespace>"
export CONTEXT="<your-kubernetes-context>"
export CHART_VERSION="<published-chart-version>"
```

Release and namespace can use the same DNS-safe project ID. Keep the context
explicit in every cluster command. Choose a reviewed release from the project
release page. For a stable install, use an engine tag and chart version from the
same release. Chart versions omit the leading `v` used by Git tags.

Prepare a dedicated, clean engine checkout. Root release tags work with this
Git checkout even though the Go module lives under `backend/`:

```bash
if [ ! -d "$ENGINE_DIR/.git" ]; then
  git clone https://github.com/willie-yao/prow-ai-dashboard.git "$ENGINE_DIR"
fi

if [ -n "$(git -C "$ENGINE_DIR" status --porcelain)" ]; then
  printf 'Engine checkout has local changes: %s\n' "$ENGINE_DIR" >&2
else
  if git -C "$ENGINE_DIR" fetch --tags origin "$ENGINE_REF" &&
    git -C "$ENGINE_DIR" checkout --detach FETCH_HEAD &&
    make -C "$ENGINE_DIR" build; then
    export FETCHER="$ENGINE_DIR/bin/fetcher"
    printf 'Fetcher ready: %s\n' "$FETCHER"
  fi
fi
```

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

## 2. Validate the consumer bundle

Run the read-only doctor:

```bash
"$FETCHER" onboard doctor \
  -project-dir "$PROJECT_DIR"
```

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

## 5. Live setup and install

The commands in this section write to the selected cluster.

### Create the namespace

```bash
kubectl --context "$CONTEXT" create namespace "$NAMESPACE" \
  --dry-run=client \
  -o yaml |
kubectl --context "$CONTEXT" apply -f -
```

### Create the AI Secret

Skip this step when `ai.enabled` is false. Set the Secret name to the exact value
in `ai.existingSecret` and point to a token file outside the repository:

```bash
export AI_SECRET="<ai.existingSecret>"
export AI_TOKEN_FILE="$HOME/.config/prow-ai-dashboard/ai-token"

install -d -m 700 "$(dirname "$AI_TOKEN_FILE")"
if [ ! -s "$AI_TOKEN_FILE" ]; then
  printf 'AI token: '
  IFS= read -r -s AI_TOKEN
  printf '\n'
  if [ -n "${AI_TOKEN:-}" ]; then
    printf '%s' "$AI_TOKEN" >"$AI_TOKEN_FILE"
    chmod 600 "$AI_TOKEN_FILE"
  fi
  unset AI_TOKEN
fi

if [ -s "$AI_TOKEN_FILE" ]; then
  kubectl --context "$CONTEXT" \
    --namespace "$NAMESPACE" \
    create secret generic "$AI_SECRET" \
    --from-file=AI_TOKEN="$AI_TOKEN_FILE" \
    --dry-run=client \
    -o yaml |
  kubectl --context "$CONTEXT" \
    --namespace "$NAMESPACE" \
    apply -f -
else
  printf 'AI Secret was not created because the token file is empty.\n' >&2
fi
```

The token value is read from the file and is not placed in the command line.
Use your normal Secret manager for production rotation and audit controls.

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

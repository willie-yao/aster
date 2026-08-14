# Kubernetes deployment quickstart

Use Kubernetes when the dashboard needs a private in-cluster model endpoint,
shared persistent data, authenticated server features, or cluster-local
integration. Use [GitHub Actions and Pages](github-pages.md) for a public,
read-only dashboard that does not need a cluster.

Start with `aster onboard` and select **Kubernetes with Helm**. The generated
consumer repository contains `project.yaml`, `prompts/system.md`, optional
skills, `deploy/values.yaml`, and a project-specific `deploy/README.md`.

This page is the canonical contributor install, verification, upgrade, and
rollback procedure. Cluster administrators should prepare prerequisites with
[Kubernetes platform setup](kubernetes-platform.md). Detailed chart behavior is
in the [Kubernetes operator reference](kubernetes-reference.md).

## Prerequisites

Obtain these reviewed inputs from the platform administrator:

- an explicit Kubernetes context;
- the application namespace and Helm release name;
- a release-dedicated execution namespace when Agent Sandbox Fix is enabled;
- a published engine tag and matching chart version;
- an RWX StorageClass or existing claim;
- existing Secret names and non-secret key names;
- the public origin and OAuth callback, when enabled;
- one expected project job for first-deployment verification.

Install Helm 4, `kubectl`, `curl`, `awk`, `python3`, `install`, and either
`sha256sum` or `shasum`. Do not place credentials in consumer files, Helm
arguments, or shell history.

Set project-specific variables from the consumer repository root:

```bash
export PROJECT_DIR="$PWD"
export CLI_VERSION="<published-engine-tag>"
export CHART_VERSION="${CLI_VERSION#v}"
export RELEASE="<application-release>"
export NAMESPACE="<application-namespace>"
export EXECUTION_NAMESPACE="" # set when agentSandbox.fixRuntime.enabled is true
export CONTEXT="<explicit-kubernetes-context>"
export PUBLIC_URL="" # set when an external public origin is configured
export EXPECTED_JOB="<expected-job-name>"
```

Keep `CONTEXT` explicit in every Kubernetes and Helm command. Use the CLI and
charts from the same release.

## Download and verify the CLI

A normal contributor uses the published binary and does not need an engine
source checkout or local build.

```bash
export CLI_DIR="$HOME/.local/share/aster/$CLI_VERSION"
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) CLI_TARGET=linux-amd64 ;;
  Linux-aarch64|Linux-arm64) CLI_TARGET=linux-arm64 ;;
  Darwin-x86_64) CLI_TARGET=darwin-amd64 ;;
  Darwin-arm64) CLI_TARGET=darwin-arm64 ;;
  *) printf 'Unsupported CLI platform\n' >&2; exit 1 ;;
esac

CLI_ASSET="aster-${CLI_VERSION}-${CLI_TARGET}"
RELEASE_URL="https://github.com/willie-yao/aster/releases/download/${CLI_VERSION}"
install -d -m 755 "$CLI_DIR"
DOWNLOAD_DIR=$(mktemp -d "${TMPDIR:-/tmp}/aster-cli-download.XXXXXX")
trap 'find "$DOWNLOAD_DIR" -type f -delete 2>/dev/null || true; rmdir "$DOWNLOAD_DIR" 2>/dev/null || true' EXIT
curl --fail --location "$RELEASE_URL/$CLI_ASSET" --output "$DOWNLOAD_DIR/$CLI_ASSET"
curl --fail --location "$RELEASE_URL/SHA256SUMS" --output "$DOWNLOAD_DIR/SHA256SUMS"
CHECKSUM_LINE=$(awk -v asset="$CLI_ASSET" '$2 == asset {print}' "$DOWNLOAD_DIR/SHA256SUMS")
test -n "$CHECKSUM_LINE"
(
  cd "$DOWNLOAD_DIR"
  if command -v sha256sum >/dev/null; then
    printf '%s\n' "$CHECKSUM_LINE" | sha256sum --check
  else
    printf '%s\n' "$CHECKSUM_LINE" | shasum -a 256 --check
  fi
)
install -m 0755 "$DOWNLOAD_DIR/$CLI_ASSET" "$CLI_DIR/$CLI_ASSET"
export ASTER="$CLI_DIR/$CLI_ASSET"
```

A missing asset, missing checksum entry, checksum mismatch, or unavailable
checksum tool stops the procedure.

## Review the consumer bundle

Review before every install or upgrade:

- discovery, branding, source identity, and expected jobs in `project.yaml`;
- every claim and TODO in `prompts/system.md`;
- each consumer diagnostic skill;
- storage, provider coordinates, Secret references, and optional features in
  `deploy/values.yaml`;
- immutable application, remote-fixer, and executor image identities.

Keep chat, GitHub writes, public ingress, and experimental runtimes disabled
until the baseline dashboard is healthy.

Inspect the complete values for the selected chart version when needed:

```bash
helm show values \
  oci://ghcr.io/willie-yao/charts/aster \
  --version "$CHART_VERSION"
```

## Validate before installation

Run the static consumer doctor:

```bash
"$ASTER" onboard doctor --project-dir "$PROJECT_DIR"
```

Render locally without contacting the cluster:

```bash
"$ASTER" kubernetes install \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION" \
  --dry-run
```

Run the live read-only doctor before the write command:

```bash
"$ASTER" kubernetes doctor \
  --action install \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION"
```

The live doctor uses Kubernetes `GET` and `LIST`, metadata-only Secret checks,
Helm release metadata, and local chart rendering. It does not read Secret
payloads, create a Sandbox, call a provider, or write cluster resources. Resolve
blocking checks and review unverified external facts with the platform owner.

## Install

```bash
"$ASTER" kubernetes install \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION"
```

Install refuses an existing release. The wrapper validates the current bundle,
waits for readiness, and requests Helm rollback on failure.

## Verify the first deployment

Resolve release-owned objects through stable labels and wait for the writer and
server:

```bash
SERVER=$(kubectl --context "$CONTEXT" -n "$NAMESPACE" get deployment \
  -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=server" \
  -o jsonpath='{.items[0].metadata.name}')
WRITER=$(kubectl --context "$CONTEXT" -n "$NAMESPACE" get deployment \
  -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=worker" \
  -o jsonpath='{.items[0].metadata.name}')
SERVICE=$(kubectl --context "$CONTEXT" -n "$NAMESPACE" get service \
  -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=server" \
  -o jsonpath='{.items[0].metadata.name}')
test -n "$SERVER" && test -n "$WRITER" && test -n "$SERVICE"
kubectl --context "$CONTEXT" -n "$NAMESPACE" rollout status "deployment/$SERVER" --timeout=5m
kubectl --context "$CONTEXT" -n "$NAMESPACE" rollout status "deployment/$WRITER" --timeout=10m
```

Port-forward the Service in a second terminal, then verify published and private
paths:

```bash
kubectl --context "$CONTEXT" -n "$NAMESPACE" port-forward "service/$SERVICE" 18080:80
```

```bash
curl --fail --retry 60 --retry-delay 10 --retry-connrefused \
  http://127.0.0.1:18080/data/manifest.json | python3 -m json.tool >/dev/null
curl --fail http://127.0.0.1:18080/data/dashboard.json \
  | grep -F "$EXPECTED_JOB"
test "$(curl --silent --output /dev/null --write-out '%{http_code}' \
  http://127.0.0.1:18080/data/ai_cache.json)" = 404
if [ -n "$PUBLIC_URL" ]; then
  curl --fail "${PUBLIC_URL%/}/data/manifest.json" | python3 -m json.tool >/dev/null
fi
if [ -n "$EXECUTION_NAMESPACE" ]; then
  test -z "$(kubectl --context "$CONTEXT" -n "$EXECUTION_NAMESPACE" \
    get sandboxes.agents.x-k8s.io -o name)"
fi
```

Confirm branding, the expected project job, authentication when enabled, normal
DNS, and the configured public-origin topology. Do not use a direct-IP
kubeconfig, edit `/etc/hosts`, remove the cluster CA, or disable TLS validation.

## Upgrade

Commit the reviewed consumer state and record the rollback revision:

```bash
test -z "$(git status --porcelain)"
export PRIOR_CONSUMER_COMMIT=$(git rev-parse HEAD)
export PRIOR_HELM_REVISION=$(helm --kube-context "$CONTEXT" -n "$NAMESPACE" \
  status "$RELEASE" --output json \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"])')
```

After selecting and verifying the new CLI and matching chart version, run:

```bash
"$ASTER" onboard doctor --project-dir "$PROJECT_DIR"
"$ASTER" kubernetes doctor \
  --action upgrade \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION"
"$ASTER" kubernetes upgrade \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION"
```

Upgrade requires an existing release, reuses deployed values, applies the
current consumer bundle, waits for readiness, and requests rollback on failure.
Repeat first-deployment verification after the upgrade.

## Roll back

```bash
helm --kube-context "$CONTEXT" -n "$NAMESPACE" \
  rollback "$RELEASE" "$PRIOR_HELM_REVISION" --wait
git restore --source="$PRIOR_CONSUMER_COMMIT" -- \
  project.yaml prompts/system.md deploy/values.yaml
git restore --source="$PRIOR_CONSUMER_COMMIT" -- skills 2>/dev/null || true
```

Restore the CLI and chart version matching the prior consumer state, rerun the
live doctor with `--action upgrade`, and repeat verification. Rollback does not
delete retained PVC data or externally owned platform resources.

## Next references

- [Kubernetes platform setup](kubernetes-platform.md)
- [Kubernetes operator reference](kubernetes-reference.md)
- [Server, authentication, chat, and actions](server.md)
- [Troubleshooting](troubleshooting.md)

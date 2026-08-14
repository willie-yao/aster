# Kubernetes contributor deployment guide

This is the normal application-deployment path for a CAPZ contributor. The
platform administrator has already installed the cluster prerequisites, supplied
an explicit kube context and namespace, and provisioned the referenced Secret
names.

You do not need an engine source checkout, local chart, custom executor, private
CA image, copied platform manifest, direct-IP kubeconfig, or disabled TLS
verification.

## 1. Required inputs

You need:

- a generated consumer repository containing `project.yaml`,
  `prompts/system.md`, optional `skills/`, `deploy/values.yaml`, and
  `deploy/README.md`;
- an explicit Kubernetes context, application namespace, and release-dedicated
  execution namespace;
- application release name and one expected CAPZ job name;
- one published engine tag and matching chart version;
- existing Secret names and non-secret key names supplied by the platform
  administrator;
- `curl`, `kubectl`, Helm 4, `awk`, `install`, `python3`, and either
  `sha256sum` or `shasum`.

## 2. Download the published CLI

Release assets include macOS and Linux binaries plus `SHA256SUMS`. This exact
path becomes downloadable with the first release published after this contract
lands. Until then, maintainers validate it with `make cleanroom-check`; do not
substitute an engine source build in the contributor procedure.

```bash
export CLI_VERSION="<published-engine-tag>" # for example v1.2.3
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

The commands will become `aster kubernetes ...` during the later repository
migration. The current pre-migration commands remain supported here.

## 3. Review the consumer

From the consumer repository root:

```bash
export PROJECT_DIR="$PWD"
export RELEASE="<application-release-from-platform-handoff>"
export NAMESPACE="<application-namespace>"
export EXECUTION_NAMESPACE="<release-dedicated-execution-namespace>"
export CONTEXT="<explicit-kube-context>"
export PUBLIC_URL="<https-public-dashboard-url>"
export EXPECTED_JOB="<expected-capz-job-name>"
export CHART_VERSION="${CLI_VERSION#v}"
```

Review:

- strict project identity, source repository, TestGrid discovery, and branding;
- every project-specific claim in `prompts/system.md`;
- consumer diagnostic skills;
- RWX storage selection;
- provider API, endpoint, model, and existing Secret references;
- public URL, OAuth callback, and optional features;
- immutable application, remote-fixer, and executor image contracts.

Do not place tokens, passwords, private keys, or certificate data in consumer
files or Helm arguments.

## 4. Run the static and live doctors

Run the static consumer doctor:

```bash
"$FETCHER" onboard doctor --project-dir "$PROJECT_DIR"
```

For a new release, run the live read-only doctor before installation:

```bash
"$FETCHER" kubernetes doctor \
  --action install \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION"
```

The current context is never used implicitly. Fix every blocking check. Review
warnings and unverified assumptions with the platform administrator. The doctor
does not create a Sandbox or call the model provider.

## 5. Install

Render locally without cluster writes:

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

Then run the guarded installation:

```bash
"$FETCHER" kubernetes install \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION"
```

The wrapper validates the bundle, refuses an existing release, includes the
current project, prompt, and skills, waits for readiness, and rolls back a failed
install.

## 6. Verify

Resolve and wait for the default watch-mode workloads using stable labels:

```bash
SERVER_DEPLOYMENT=$(kubectl --context "$CONTEXT" --namespace "$NAMESPACE" \
  get deployment \
  -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=server" \
  -o jsonpath='{.items[0].metadata.name}')
WORKER_DEPLOYMENT=$(kubectl --context "$CONTEXT" --namespace "$NAMESPACE" \
  get deployment \
  -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=worker" \
  -o jsonpath='{.items[0].metadata.name}')
SERVER_SERVICE=$(kubectl --context "$CONTEXT" --namespace "$NAMESPACE" \
  get service \
  -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=server" \
  -o jsonpath='{.items[0].metadata.name}')

test -n "$SERVER_DEPLOYMENT"
test -n "$WORKER_DEPLOYMENT"
test -n "$SERVER_SERVICE"
kubectl --context "$CONTEXT" --namespace "$NAMESPACE" \
  rollout status "deployment/$SERVER_DEPLOYMENT" --timeout=5m
kubectl --context "$CONTEXT" --namespace "$NAMESPACE" \
  rollout status "deployment/$WORKER_DEPLOYMENT" --timeout=10m
```

Wait for the first fetch and validate local data, public origin, private-file
filtering, and execution cleanup in one fail-closed subshell:

```bash
(
  set -euo pipefail
  VERIFY_DIR=$(mktemp -d "${TMPDIR:-/tmp}/prow-dashboard-verify.XXXXXX")
  PF_PID=""
  cleanup() {
    if [ -n "$PF_PID" ]; then
      kill "$PF_PID" 2>/dev/null || true
      wait "$PF_PID" 2>/dev/null || true
    fi
    find "$VERIFY_DIR" -type f -delete 2>/dev/null || true
    rmdir "$VERIFY_DIR" 2>/dev/null || true
  }
  trap cleanup EXIT

  kubectl --context "$CONTEXT" --namespace "$NAMESPACE" \
    port-forward "service/$SERVER_SERVICE" 18080:80 \
    >"$VERIFY_DIR/port-forward.log" 2>&1 &
  PF_PID=$!
  manifest_ready=false
  for _ in $(seq 1 60); do
    if curl --fail --silent http://127.0.0.1:18080/data/manifest.json \
      --output "$VERIFY_DIR/manifest.json"; then
      manifest_ready=true
      break
    fi
    sleep 10
  done
  if [ "$manifest_ready" != true ]; then
    printf 'manifest did not become available within 10 minutes\n' >&2
    exit 1
  fi

  curl --fail --silent http://127.0.0.1:18080/data/dashboard.json \
    --output "$VERIFY_DIR/dashboard.json"
  python3 -m json.tool "$VERIFY_DIR/manifest.json" >/dev/null
  python3 -m json.tool "$VERIFY_DIR/dashboard.json" >/dev/null
  grep -F "$EXPECTED_JOB" "$VERIFY_DIR/dashboard.json"
  PRIVATE_STATUS=$(curl --silent --output /dev/null --write-out '%{http_code}' \
    http://127.0.0.1:18080/data/ai_cache.json)
  test "$PRIVATE_STATUS" = 404
  curl --fail --silent "${PUBLIC_URL%/}/data/manifest.json" \
    | python3 -m json.tool >/dev/null
  SANDBOXES=$(kubectl --context "$CONTEXT" --namespace "$EXECUTION_NAMESPACE" \
    get sandboxes.agents.x-k8s.io -o name)
  test -z "$SANDBOXES"
)
```

Open `PUBLIC_URL` and confirm the expected branding, CAPZ job, and OAuth sign-in
when authentication is enabled. The private-file check must remain HTTP 404.
Use normal DNS for production verification. Do not use a direct-IP kubeconfig,
remove the CA, edit `/etc/hosts`, or set `insecure-skip-tls-verify`.

For an intentional cron-mode deployment, inspect the release-labeled CronJob and
its latest Job instead of `WORKER_DEPLOYMENT`.

## 7. Upgrade

Commit the reviewed consumer state and record the rollback inputs before changing
versions:

```bash
 test -z "$(git status --porcelain)"
export PRIOR_CONSUMER_COMMIT=$(git rev-parse HEAD)
export PRIOR_FETCHER="$FETCHER"
export PRIOR_CLI_VERSION="$CLI_VERSION"
export PRIOR_CHART_VERSION="$CHART_VERSION"
export PRIOR_HELM_REVISION=$(helm --kube-context "$CONTEXT" \
  --namespace "$NAMESPACE" status "$RELEASE" --output json \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"])')
```

Set the new published engine tag and matching chart version, download and verify
the matching CLI, then run:

```bash
"$FETCHER" onboard doctor --project-dir "$PROJECT_DIR"

"$FETCHER" kubernetes doctor \
  --action upgrade \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION"

"$FETCHER" kubernetes upgrade \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION"
```

A watch-to-cron or cron-to-watch transition may produce a doctor warning for the
old single writer. The guarded upgrade replaces it transactionally. Multiple
simultaneous writers remain a blocker.

## 8. Roll back

If post-upgrade verification fails, roll back the Helm release to the recorded
revision:

```bash
helm --kube-context "$CONTEXT" --namespace "$NAMESPACE" \
  rollback "$RELEASE" "$PRIOR_HELM_REVISION" --wait
```

Restore the matching prior consumer bundle and tooling before validating the
restored deployment:

```bash
git restore --source="$PRIOR_CONSUMER_COMMIT" -- \
  project.yaml prompts/system.md deploy/values.yaml
git restore --source="$PRIOR_CONSUMER_COMMIT" -- skills 2>/dev/null || true
export FETCHER="$PRIOR_FETCHER"
export CLI_VERSION="$PRIOR_CLI_VERSION"
export CHART_VERSION="$PRIOR_CHART_VERSION"

"$FETCHER" kubernetes doctor \
  --action upgrade \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version "$CHART_VERSION"
```

Repeat the verification sequence using the restored bundle. Rollback does not
delete retained PVC data or externally owned platform resources.

## Remaining platform acceptance

Provider-free render and kind tests do not prove real AKS Cilium behavior,
secure-runtime isolation, RWX semantics, Front Door, DNS, certificates, OAuth,
or provider compatibility. Those remain release-candidate acceptance owned with
the platform administrator.

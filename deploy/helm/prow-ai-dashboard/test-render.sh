#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
chart="$root/deploy/helm/prow-ai-dashboard"
tmp="${TMPDIR:-/tmp}/prow-ai-dashboard-helm-$$"
mkdir -p "$tmp"
trap 'rm -rf "$tmp"' EXIT

container_command() {
  local name=$1 file=$2
  awk -v target="$name" '
    $1 == "-" && $2 == "name:" { current = $3 }
    current == target && $1 == "command:" {
      getline
      sub(/^[[:space:]]*-[[:space:]]*/, "")
      print
      exit
    }
  ' "$file"
}

cat > "$tmp/values.yaml" <<'VALUES'
image:
  tag: sha-test
project:
  config: |
    id: test
    name: Test
    testgrid:
      dashboard: test
    storage:
      provider: local
      base: /tmp
    branding:
      title: Test
      base_path: /
      site_url: https://example.test
  systemPrompt: test prompt
VALUES

helm lint "$chart" -f "$tmp/values.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" > "$tmp/default.yaml"
for removed in orka-producer orka-ingestor orka-artifact-tool submit-analysis 'type: ai' 'kind: RoleBinding' 'kind: ValidatingAdmissionPolicy'; do
  if grep -Fq "$removed" "$tmp/default.yaml"; then
    echo "default render contains removed Orka analysis resource: $removed" >&2
    exit 1
  fi
done

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --show-only templates/fetcher-cronjob.yaml > "$tmp/cron.yaml"
grep -Fq 'activeDeadlineSeconds: 36000' "$tmp/cron.yaml"
grep -Fq 'restartPolicy: OnFailure' "$tmp/cron.yaml"
if grep -Fq 'backoffLimit:' "$tmp/cron.yaml"; then
  echo 'default CronJob unexpectedly set a backoff limit' >&2
  exit 1
fi
if [[ $(container_command fetcher "$tmp/cron.yaml") != /usr/local/bin/fetcher ]]; then
  echo 'CronJob does not run the in-process fetcher' >&2
  exit 1
fi

# The experimental selector is Helm-only and leaves existing consumers on the
# in-process path until explicitly selected.
if grep -Fq -- '-analysis-runtime=orka-container' "$tmp/default.yaml"; then
  echo 'default render enabled Orka container analysis' >&2
  exit 1
fi

container_args=(
  --set mode=cron
  --set ai.enabled=true
  --set ai.endpoint=http://model.orka-system.svc.cluster.local/v1/chat/completions
  --set ai.model=script-model
  --set ai.token=dashboard-token
  --set analysisRuntime.type=orka-container
  --set analysisRuntime.orkaContainer.image.tag=sha-deadbeef
  --set analysisRuntime.orkaContainer.apiAuth.existingSecret=orka-api
  --set analysisRuntime.orkaContainer.modelAuth.existingSecret=orka-model
)
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "${container_args[@]}" > "$tmp/container-analysis.yaml"
grep -Fq -- '-analysis-runtime=orka-container' "$tmp/container-analysis.yaml"
grep -Fq -- '-orka-analysis-api=http://orka.orka-system.svc.cluster.local:8080' "$tmp/container-analysis.yaml"
grep -Fq -- '-orka-analysis-image=ghcr.io/willie-yao/prow-ai-dashboard/analyzer:sha-deadbeef' "$tmp/container-analysis.yaml"
grep -Fq 'restartPolicy: Never' "$tmp/container-analysis.yaml"
grep -Fq 'backoffLimit: 0' "$tmp/container-analysis.yaml"
grep -Fq 'resources: ["tasks"]' "$tmp/container-analysis.yaml"
grep -Fq 'verbs: ["create", "get", "list", "watch", "patch", "delete"]' "$tmp/container-analysis.yaml"
grep -Fq 'resources: ["configmaps"]' "$tmp/container-analysis.yaml"
grep -Fq 'kind: ValidatingAdmissionPolicy' "$tmp/container-analysis.yaml"
grep -Fq 'object.spec.image ==' "$tmp/container-analysis.yaml"
grep -Fq 'analysis Tasks must use only the configured model Secret' "$tmp/container-analysis.yaml"
grep -Fq 'kind: Namespace' "$tmp/container-analysis.yaml"
grep -Eq 'namespace: test-prow-ai-dashboard-analysis-[0-9a-f]{8}' "$tmp/container-analysis.yaml"
grep -Fq 'name: PROW_AI_STATE_KEY' "$tmp/container-analysis.yaml"
grep -Fq 'name: ORKA_API_TOKEN' "$tmp/container-analysis.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "${container_args[@]}" \
  --set ai.contextWindowTokens=128000 --show-only templates/orka-analysis-admission.yaml > "$tmp/container-context-window.yaml"
grep -Fq "AI_CONTEXT_WINDOW_TOKENS" "$tmp/container-context-window.yaml"
grep -Fq 'e.value == \"128000\"' "$tmp/container-context-window.yaml"
if grep -Eq 'resources: \["(tools|providers|agents|agentruntimes)"\]|type: ai|orka-producer|orka-ingestor|orka-artifact-tool' "$tmp/container-analysis.yaml"; then
  echo 'container analysis render contains a forbidden patched-worker resource' >&2
  exit 1
fi
if [[ $(grep -Fc 'app.kubernetes.io/component: orka-container-analysis-state' "$tmp/container-analysis.yaml") -ne 2 ]]; then
  echo 'chart-managed state key was not rendered in both namespaces' >&2
  exit 1
fi

for namespace in dashboard-a dashboard-b; do
  helm template test "$chart" -n "$namespace" -f "$tmp/values.yaml" "${container_args[@]}" \
    --show-only templates/orka-analysis-state-secret.yaml > "$tmp/state-$namespace.yaml"
done
state_name_a=$(awk '$1 == "name:" { name=$2 } $1 == "namespace:" && $2 != "dashboard-a" { print name; exit }' "$tmp/state-dashboard-a.yaml")
state_name_b=$(awk '$1 == "name:" { name=$2 } $1 == "namespace:" && $2 != "dashboard-b" { print name; exit }' "$tmp/state-dashboard-b.yaml")
if [[ -z $state_name_a || -z $state_name_b || $state_name_a == "$state_name_b" ]]; then
  echo 'chart-managed cross-namespace state Secret names are not release-scoped' >&2
  exit 1
fi

# A supplied state Secret is referenced in both namespaces but never copied.
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "${container_args[@]}" \
  --set analysisRuntime.orkaContainer.state.existingSecret=shared-state > "$tmp/container-existing-state.yaml"
if grep -Fq 'orka-container-analysis-state' "$tmp/container-existing-state.yaml"; then
  echo 'existing state Secret unexpectedly rendered chart-managed state data' >&2
  exit 1
fi
grep -Fq -- '-orka-analysis-state-secret=shared-state' "$tmp/container-existing-state.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "${container_args[@]}" \
  --set-string analysisRuntime.orkaContainer.pollInterval=1.5s \
  --set-string analysisRuntime.orkaContainer.taskTimeout=1m30s > "$tmp/container-compound-duration.yaml"
grep -Fq -- '-orka-analysis-poll-interval=1.5s' "$tmp/container-compound-duration.yaml"
grep -Fq -- '-orka-analysis-task-timeout=1m30s' "$tmp/container-compound-duration.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "${container_args[@]}" \
  --set-string analysisRuntime.orkaContainer.pollInterval=500us \
  --set-string analysisRuntime.orkaContainer.taskTimeout=1h > "$tmp/container-microsecond-duration.yaml"
grep -Fq -- '-orka-analysis-poll-interval=500us' "$tmp/container-microsecond-duration.yaml"

for invalid in type watch endpoint model custom-namespace shared-namespace release-namespace api api-secret api-token-key image mutable-image build-metadata model-secret token-key state-key concurrency poll slow-poll timeout retries cpu-selector gpu accelerator; do
  case $invalid in
    type) invalid_args=(--set analysisRuntime.type=remote); want='analysisRuntime.type must be inprocess or orka-container' ;;
    watch) invalid_args=("${container_args[@]}" --set mode=watch); want='analysisRuntime.type=orka-container requires mode=cron' ;;
    endpoint) invalid_args=("${container_args[@]}" --set-string ai.endpoint=); want='analysisRuntime.type=orka-container requires ai.endpoint' ;;
    model) invalid_args=("${container_args[@]}" --set-string ai.model=); want='analysisRuntime.type=orka-container requires ai.model' ;;
    custom-namespace) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.namespace=custom-analysis); want='analysisRuntime.orkaContainer.namespace must be dedicated to this release and end with its release scope' ;;
    shared-namespace) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.namespace=orka-system); want='analysisRuntime.orkaContainer.namespace must be dedicated and differ from orka.namespace' ;;
    release-namespace) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.namespace=dashboard-test); want='analysisRuntime.orkaContainer.namespace must differ from the dashboard release namespace' ;;
    api) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.api='http://user:secret@orka'); want='analysisRuntime.orkaContainer.api must be an absolute http or https URL without credentials' ;;
    api-secret) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.apiAuth.existingSecret=); want='analysisRuntime.orkaContainer.apiAuth.existingSecret is required' ;;
    api-token-key) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.apiAuth.tokenKey=); want='analysisRuntime.orkaContainer.apiAuth.tokenKey is required' ;;
    image) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.image.repository=); want='analysisRuntime.orkaContainer.image.repository is required' ;;
    mutable-image) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.image.tag=main); want='analysisRuntime.orkaContainer.image tag must be an immutable sha-<hex> or full semantic version' ;;
    build-metadata) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.image.tag=v1.2.3+build.4); want='analysisRuntime.orkaContainer.image tag must be an immutable sha-<hex> or full semantic version' ;;
    model-secret) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.modelAuth.existingSecret=); want='analysisRuntime.orkaContainer.modelAuth.existingSecret is required' ;;
    token-key) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.modelAuth.tokenKey=); want='analysisRuntime.orkaContainer.modelAuth.tokenKey is required' ;;
    state-key) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.state.key=); want='analysisRuntime.orkaContainer.state.key is required' ;;
    concurrency) invalid_args=("${container_args[@]}" --set analysisRuntime.orkaContainer.maxConcurrentTasks=0); want='analysisRuntime.orkaContainer.maxConcurrentTasks must be an integer from 1 to 999' ;;
    poll) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.pollInterval=soon); want='analysisRuntime.orkaContainer.pollInterval must be a positive Go duration' ;;
    slow-poll) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.pollInterval=30s); want='analysisRuntime.orkaContainer.pollInterval must be less than 30s' ;;
    timeout) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.taskTimeout=0s); want='analysisRuntime.orkaContainer.taskTimeout must be a positive Go duration' ;;
    retries) invalid_args=("${container_args[@]}" --set analysisRuntime.orkaContainer.retries=-1); want='analysisRuntime.orkaContainer.retries must be an integer from 0 to 99' ;;
    cpu-selector) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.nodeSelector.agentpool=); want='analysisRuntime.orkaContainer.nodeSelector.agentpool must select an explicit CPU pool' ;;
    gpu) invalid_args=("${container_args[@]}" --set analysisRuntime.orkaContainer.nodeSelector.agentpool=h100); want='analysisRuntime.orkaContainer placement must not select or tolerate GPU nodes' ;;
    accelerator) invalid_args=("${container_args[@]}" --set analysisRuntime.orkaContainer.nodeSelector.cloud\.google\.com/gke-accelerator=nvidia-tesla-t4); want='analysisRuntime.orkaContainer placement must not select or tolerate GPU nodes' ;;
  esac
  if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "${invalid_args[@]}" > "$tmp/invalid-analysis-$invalid.yaml" 2>&1; then
    echo "$invalid analysis runtime value was accepted" >&2
    exit 1
  fi
  grep -Fq "$want" "$tmp/invalid-analysis-$invalid.yaml"
done

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set fetcher.restartPolicy=Never \
  --set fetcher.backoffLimit=2 \
  --set fetcher.activeDeadlineSeconds=7200 \
  --show-only templates/fetcher-cronjob.yaml > "$tmp/custom-job-lifecycle.yaml"
grep -Fq 'backoffLimit: 2' "$tmp/custom-job-lifecycle.yaml"
grep -Fq 'activeDeadlineSeconds: 7200' "$tmp/custom-job-lifecycle.yaml"
grep -Fq 'restartPolicy: Never' "$tmp/custom-job-lifecycle.yaml"

for invalid in restart backoff negative-backoff oversized-backoff deadline negative-deadline; do
  case $invalid in
    restart) lifecycle_args=(--set-string fetcher.restartPolicy=Always); want='fetcher.restartPolicy must be Never or OnFailure' ;;
    backoff) lifecycle_args=(--set-string fetcher.backoffLimit=many); want='fetcher.backoffLimit must be -1 or a non-negative integer' ;;
    negative-backoff) lifecycle_args=(--set fetcher.backoffLimit=-2); want='fetcher.backoffLimit must be -1 or a non-negative integer' ;;
    oversized-backoff) lifecycle_args=(--set-string fetcher.backoffLimit=2147483648); want='fetcher.backoffLimit must not exceed 2147483647' ;;
    deadline) lifecycle_args=(--set-string fetcher.activeDeadlineSeconds=soon); want='fetcher.activeDeadlineSeconds must be a non-negative integer' ;;
    negative-deadline) lifecycle_args=(--set fetcher.activeDeadlineSeconds=-1); want='fetcher.activeDeadlineSeconds must be a non-negative integer' ;;
  esac
  if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
    --set mode=cron "${lifecycle_args[@]}" > "$tmp/invalid-$invalid.yaml" 2>&1; then
    echo "$invalid lifecycle value was accepted" >&2
    exit 1
  fi
  grep -Fq "$want" "$tmp/invalid-$invalid.yaml"
done


helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true \
  --set ai.endpoint=https://ai.example.test/v1/chat/completions \
  --set ai.model=test-model \
  --set ai.token=test-token \
  --set ai.contextWindowTokens=128000 > "$tmp/context-window.yaml"
grep -Fq 'name: AI_CONTEXT_WINDOW_TOKENS' "$tmp/context-window.yaml"
grep -Fq 'value: "128000"' "$tmp/context-window.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set ai.enabled=true \
  --set ai.endpoint=https://ai.example.test/v1/chat/completions \
  --set ai.model=test-model \
  --set ai.token=test-token \
  --set ai.contextWindowTokens=128000 \
  --show-only templates/fetcher-cronjob.yaml > "$tmp/context-window-cron.yaml"
grep -Fq 'name: AI_CONTEXT_WINDOW_TOKENS' "$tmp/context-window-cron.yaml"
grep -Fq 'value: "128000"' "$tmp/context-window-cron.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.actions.enabled=true \
  --set server.actions.mode=proxy \
  --set server.actions.proxy.botToken=test-token \
  --set ai.enabled=true \
  --set ai.endpoint=https://ai.example.test/v1/chat/completions \
  --set ai.model=test-model \
  --set ai.token=test-token \
  --set ai.contextWindowTokens=128000 \
  --show-only templates/server-deployment.yaml > "$tmp/context-window-server.yaml"
grep -Fq 'name: AI_CONTEXT_WINDOW_TOKENS' "$tmp/context-window-server.yaml"
grep -Fq 'value: "128000"' "$tmp/context-window-server.yaml"

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set-string ai.contextWindowTokens=many > "$tmp/invalid-context-window.yaml" 2>&1; then
  echo 'chart accepted an invalid AI context window' >&2
  exit 1
fi
grep -Fq 'ai.contextWindowTokens must be 0 or an integer from 9217 to 1000000000' "$tmp/invalid-context-window.yaml"

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.contextWindowTokens=9216 > "$tmp/too-small-context-window.yaml" 2>&1; then
  echo 'chart accepted an unusable AI context window' >&2
  exit 1
fi
grep -Fq 'ai.contextWindowTokens must be 0 or an integer from 9217 to 1000000000' "$tmp/too-small-context-window.yaml"

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set-string ai.api=legacy > "$tmp/invalid-ai-api.yaml" 2>&1; then
  echo 'chart accepted an invalid AI API' >&2
  exit 1
fi
grep -Fq 'ai.api must be chat_completions or responses' "$tmp/invalid-ai-api.yaml"

for namespace in dashboard-a dashboard-b; do
  helm template test "$chart" -n "$namespace" -f "$tmp/values.yaml" \
    --set orka.fixRuntime.enabled=true \
    --set orka.namespace=orka-test \
    --show-only templates/orka-fix-runtime-rbac.yaml > "$tmp/rbac-$namespace.yaml"
  grep -Fq 'namespace: orka-test' "$tmp/rbac-$namespace.yaml"
  grep -Fq 'resources: ["tasks"]' "$tmp/rbac-$namespace.yaml"
  grep -Fq 'verbs: ["create", "get", "patch", "delete"]' "$tmp/rbac-$namespace.yaml"
  if [[ $(grep -Ec '^[[:space:]]+resources:' "$tmp/rbac-$namespace.yaml") -ne 1 ]]; then
    echo 'fix runtime RBAC includes a resource rule beyond Tasks' >&2
    exit 1
  fi
done
rbac_name_a=$(awk '$1 == "kind:" { kind=$2 } kind == "Role" && $1 == "name:" { print $2; exit }' "$tmp/rbac-dashboard-a.yaml")
rbac_name_b=$(awk '$1 == "kind:" { kind=$2 } kind == "Role" && $1 == "name:" { print $2; exit }' "$tmp/rbac-dashboard-b.yaml")
if [[ -z "$rbac_name_a" || -z "$rbac_name_b" || "$rbac_name_a" == "$rbac_name_b" ]]; then
  echo 'Orka fix-runtime RBAC names are not isolated by release namespace' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=watch \
  --set orka.fixRuntime.enabled=true \
  --set orka.fixRuntime.image.tag=sha-test \
  --show-only templates/worker-deployment.yaml > "$tmp/fix-watch.yaml"
grep -Fq 'serviceAccountName: test-prow-ai-dashboard-orka' "$tmp/fix-watch.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/fixer:sha-test' "$tmp/fix-watch.yaml"
if [[ $(container_command worker "$tmp/fix-watch.yaml") != /usr/local/bin/worker ]]; then
  echo 'fix-enabled worker does not run the in-process analyzer' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set orka.fixRuntime.enabled=true \
  --set orka.fixRuntime.image.tag=sha-test \
  --show-only templates/fetcher-cronjob.yaml > "$tmp/fix-cron.yaml"
grep -Fq 'serviceAccountName: test-prow-ai-dashboard-orka' "$tmp/fix-cron.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/fixer:sha-test' "$tmp/fix-cron.yaml"
if [[ $(container_command fetcher "$tmp/fix-cron.yaml") != /usr/local/bin/fetcher ]]; then
  echo 'fix-enabled CronJob does not run the in-process fetcher' >&2
  exit 1
fi

# Container analysis and Orka fix generation are independent options that may
# share the runtime ServiceAccount while retaining separate Roles.
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  "${container_args[@]}" \
  --set orka.fixRuntime.enabled=true \
  --set orka.fixRuntime.image.tag=sha-test > "$tmp/combined-orka-runtimes.yaml"
grep -Fq 'app.kubernetes.io/component: orka-container-analysis' "$tmp/combined-orka-runtimes.yaml"
grep -Fq 'app.kubernetes.io/component: orka-fix-runtime' "$tmp/combined-orka-runtimes.yaml"
grep -Fq 'app.kubernetes.io/component: orka-runtime' "$tmp/combined-orka-runtimes.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/fixer:sha-test' "$tmp/combined-orka-runtimes.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --show-only templates/pvc.yaml > "$tmp/pvc-retained.yaml"
grep -Fq 'helm.sh/resource-policy: keep' "$tmp/pvc-retained.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set persistence.retain=false \
  --show-only templates/pvc.yaml > "$tmp/pvc-deletable.yaml"
if grep -Fq 'helm.sh/resource-policy: keep' "$tmp/pvc-deletable.yaml"; then
  echo 'persistence.retain=false still rendered the keep policy' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.actions.enabled=true \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --set server.actions.proxy.botToken=test-token \
  --show-only templates/server-deployment.yaml > "$tmp/actions-server.yaml"
grep -A1 -Fq 'name: ACTIONS_ENABLED' "$tmp/actions-server.yaml"
grep -Fq 'value: "true"' "$tmp/actions-server.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --set ai.enabled=true \
  --set ai.token=test-token \
  --set ai.endpoint=http://model.test/v1/chat/completions \
  --set ai.model=test-model \
  --show-only templates/server-deployment.yaml > "$tmp/chat-server.yaml"
grep -A1 -Fq 'name: ANALYSIS_CHAT_ENABLED' "$tmp/chat-server.yaml"
grep -Fq 'value: "true"' "$tmp/chat-server.yaml"
grep -Fq 'name: ANALYSIS_CHAT_STATE_DIR' "$tmp/chat-server.yaml"
grep -Fq 'value: "/data/.analysis-chat"' "$tmp/chat-server.yaml"
grep -Fq 'name: ANALYSIS_CHAT_SESSION_TTL' "$tmp/chat-server.yaml"
grep -Fq 'value: "2h"' "$tmp/chat-server.yaml"
grep -Fq 'readOnly: false' "$tmp/chat-server.yaml"
grep -Fq -- '- -project-dir=/config' "$tmp/chat-server.yaml"
grep -Fq 'name: project' "$tmp/chat-server.yaml"
if grep -Fq 'name: ACTIONS_ENABLED' "$tmp/chat-server.yaml" || grep -Fq 'name: BOT_TOKEN' "$tmp/chat-server.yaml"; then
  echo 'chat-only server rendered write-action credentials' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.actions.mode=oauth \
  --set server.actions.admins[0]=alice \
  --set server.actions.oauth.clientId=client \
  --set server.actions.oauth.clientSecret=secret \
  --set server.actions.oauth.sessionKey=session-key \
  --set server.actions.oauth.redirectUrl=https://dashboard.test/api/auth/callback \
  --set ai.enabled=true \
  --set ai.token=test-token \
  --set ai.endpoint=http://model.test/v1/chat/completions \
  --set ai.model=test-model \
  --show-only templates/server-deployment.yaml > "$tmp/chat-oauth.yaml"
grep -A1 -Fq 'name: OAUTH_SCOPE' "$tmp/chat-oauth.yaml"
grep -Fq 'value: "read:user"' "$tmp/chat-oauth.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --set server.actions.proxy.existingSecret=proxy-auth \
  --set ai.enabled=true \
  --set ai.token=test-token \
  --set ai.endpoint=http://model.test/v1/chat/completions \
  --set ai.model=test-model \
  --show-only templates/server-deployment.yaml > "$tmp/chat-existing-auth.yaml"
grep -A5 -Fq 'name: AUTH_PROXY_SECRET' "$tmp/chat-existing-auth.yaml"
grep -Fq 'name: proxy-auth' "$tmp/chat-existing-auth.yaml"
grep -Fq 'optional: true' "$tmp/chat-existing-auth.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.replicaCount=2 \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --set ai.enabled=true \
  --set ai.token=test-token \
  --set ai.endpoint=http://model.test/v1/chat/completions \
  --set ai.model=test-model > "$tmp/chat-multiple-replicas.yaml"
grep -Fq 'replicas: 2' "$tmp/chat-multiple-replicas.yaml"
grep -Fq 'name: ANALYSIS_CHAT_STATE_DIR' "$tmp/chat-multiple-replicas.yaml"

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.chat.maxSessions=2 \
  --set server.chat.maxSessionsPerOwner=3 \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --set ai.enabled=true \
  --set ai.token=test-token > "$tmp/chat-invalid-capacity.yaml" 2>&1; then
  echo 'chat accepted a per-owner session limit above the total' >&2
  exit 1
fi
grep -Fq 'server.chat.maxSessionsPerOwner cannot exceed server.chat.maxSessions' "$tmp/chat-invalid-capacity.yaml"

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true > "$tmp/chat-without-ai.yaml" 2>&1; then
  echo 'server.chat.enabled was accepted without ai.enabled' >&2
  exit 1
fi
grep -Fq 'server.chat.enabled requires ai.enabled' "$tmp/chat-without-ai.yaml"

echo 'Helm render checks passed.'

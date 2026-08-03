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
if grep -Eq '^kind: (CronJob|Job)$' "$tmp/default.yaml"; then
  echo 'watch mode rendered a CronJob or manual Job' >&2
  exit 1
fi

# The bundle wrapper clears skill entries inherited from values, then adds the
# current files. Helm applies --set-json before --set-file.
cat > "$tmp/stale-skills.yaml" <<'VALUES'
project:
  skills:
    stale.yaml: stale
VALUES
cat > "$tmp/current-skill.yaml" <<'SKILL'
id: current
triggers: [failure]
SKILL
helm template test "$chart" -n dashboard-test \
  -f "$tmp/values.yaml" -f "$tmp/stale-skills.yaml" \
  --set-json 'project.skills={}' \
  --set-file "project.skills.current\.yaml=$tmp/current-skill.yaml" \
  --show-only templates/configmap-project.yaml > "$tmp/bundle-skills.yaml"
grep -Fq 'current.yaml: |' "$tmp/bundle-skills.yaml"
if grep -Fq 'stale.yaml:' "$tmp/bundle-skills.yaml"; then
  echo 'bundle skill override retained a stale values entry' >&2
  exit 1
fi
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
if grep -Fq 'name: prepare-project' "$tmp/cron.yaml" || grep -Fq 'name: project-runtime' "$tmp/cron.yaml"; then
  echo 'in-process CronJob unexpectedly materialized the project ConfigMap' >&2
  exit 1
fi
grep -A3 -F 'name: project' "$tmp/cron.yaml" | grep -Fq 'mountPath: /config'
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" --set mode=cron > "$tmp/cron-all.yaml"
if grep -Fq 'app.kubernetes.io/component: worker' "$tmp/cron-all.yaml"; then
  echo 'cron mode rendered a worker Deployment' >&2
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

# Image-specific tags override the shared snapshot tag. Empty image-specific
# tags resolve through global.imageTag, then Chart.appVersion.
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set-string image.tag= \
  --set-string global.imageTag=sha-abcdef0 > "$tmp/global-engine.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard:sha-abcdef0' "$tmp/global-engine.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  "${container_args[@]}" \
  --set-string analysisRuntime.orkaContainer.image.tag= \
  --set-string global.imageTag=sha-abcdef0 > "$tmp/global-analyzer.yaml"
grep -Fq -- '-orka-analysis-image=ghcr.io/willie-yao/prow-ai-dashboard/analyzer:sha-abcdef0' "$tmp/global-analyzer.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set-string global.imageTag=sha-abcdef0 \
  --set orka.fixRuntime.enabled=true \
  --set server.actions.enabled=true \
  --set server.actions.mode=proxy \
  --set 'server.actions.admins[0]=alice' \
  --set server.actions.proxy.botToken=test-token > "$tmp/global-fixer.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/fixer:sha-abcdef0' "$tmp/global-fixer.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set-string global.imageTag=sha-abcdef0 > "$tmp/specific-engine.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard:sha-test' "$tmp/specific-engine.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  "${container_args[@]}" \
  --set-string global.imageTag=sha-abcdef0 > "$tmp/specific-analyzer.yaml"
grep -Fq -- '-orka-analysis-image=ghcr.io/willie-yao/prow-ai-dashboard/analyzer:sha-deadbeef' "$tmp/specific-analyzer.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set-string global.imageTag=sha-abcdef0 \
  --set-string orka.fixRuntime.image.tag=sha-cafebabe \
  --set orka.fixRuntime.enabled=true \
  --set server.actions.enabled=true \
  --set server.actions.mode=proxy \
  --set 'server.actions.admins[0]=alice' \
  --set server.actions.proxy.botToken=test-token > "$tmp/specific-fixer.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/fixer:sha-cafebabe' "$tmp/specific-fixer.yaml"

helm package "$chart" --destination "$tmp" --version 9.8.7 --app-version v9.8.7 >/dev/null
fallback_chart="$tmp/prow-ai-dashboard-9.8.7.tgz"
helm template test "$fallback_chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set-string image.tag= \
  --set-string global.imageTag= > "$tmp/app-version-engine.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard:v9.8.7' "$tmp/app-version-engine.yaml"
helm template test "$fallback_chart" -n dashboard-test -f "$tmp/values.yaml" \
  "${container_args[@]}" \
  --set-string analysisRuntime.orkaContainer.image.tag= \
  --set-string global.imageTag= > "$tmp/app-version-analyzer.yaml"
grep -Fq -- '-orka-analysis-image=ghcr.io/willie-yao/prow-ai-dashboard/analyzer:v9.8.7' "$tmp/app-version-analyzer.yaml"
helm template test "$fallback_chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set-string global.imageTag= \
  --set orka.fixRuntime.enabled=true \
  --set server.actions.enabled=true \
  --set server.actions.mode=proxy \
  --set 'server.actions.admins[0]=alice' \
  --set server.actions.proxy.botToken=test-token > "$tmp/app-version-fixer.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/fixer:v9.8.7' "$tmp/app-version-fixer.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "${container_args[@]}" > "$tmp/container-analysis.yaml"
awk '/^          initContainers:/{copy=1} /^          containers:/{copy=0} copy' \
  "$tmp/container-analysis.yaml" > "$tmp/prepare-project.yaml"
awk '/^          containers:/{copy=1} /^          (nodeSelector|affinity|tolerations|volumes):/{if (copy) exit} copy' \
  "$tmp/container-analysis.yaml" > "$tmp/fetcher-container.yaml"
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
grep -Fq 'name: orka-api' "$tmp/container-analysis.yaml"
if grep -Fq 'name: ORKA_API_TOKEN_FILE' "$tmp/container-analysis.yaml"; then
  echo 'Secret-backed container analysis also rendered a token file' >&2
  exit 1
fi
grep -Fq 'name: prepare-project' "$tmp/prepare-project.yaml"
grep -Fq 'image: busybox:1.36.1' "$tmp/prepare-project.yaml"
grep -Fq 'imagePullPolicy: IfNotPresent' "$tmp/prepare-project.yaml"
grep -Fq 'set -eu' "$tmp/prepare-project.yaml"
grep -Fq 'cp -L /source/project.yaml /project/project.yaml' "$tmp/prepare-project.yaml"
grep -Fq 'cp -L /source/prompts/system.md /project/prompts/system.md' "$tmp/prepare-project.yaml"
grep -Fq 'for file in /source/skills/*; do' "$tmp/prepare-project.yaml"
grep -Fq "tr '[:upper:]' '[:lower:]'" "$tmp/prepare-project.yaml"
grep -Fq 'yaml|yml) cp -L "$file" /project/skills/' "$tmp/prepare-project.yaml"
grep -A3 -F 'name: project' "$tmp/prepare-project.yaml" | grep -Fq 'mountPath: /source'
grep -A3 -F 'name: project' "$tmp/prepare-project.yaml" | grep -Fq 'readOnly: true'
grep -A2 -F 'name: project-runtime' "$tmp/prepare-project.yaml" | grep -Fq 'mountPath: /project'
if grep -Eq 'secret|token|chmod[[:space:]]+-R' "$tmp/prepare-project.yaml"; then
  echo 'project materializer contains a Secret mount, token reference, or recursive chmod' >&2
  exit 1
fi
grep -A3 -F 'name: project-runtime' "$tmp/fetcher-container.yaml" | grep -Fq 'mountPath: /config'
grep -A3 -F 'name: project-runtime' "$tmp/fetcher-container.yaml" | grep -Fq 'readOnly: true'
if grep -Eq '^[[:space:]]+- name: project$' "$tmp/fetcher-container.yaml"; then
  echo 'Orka fetcher still mounts the ConfigMap directly at the project path' >&2
  exit 1
fi
grep -A2 -F 'name: project-runtime' "$tmp/container-analysis.yaml" | grep -Fq 'emptyDir: {}'

cat > "$tmp/skills-values.yaml" <<'VALUES'
project:
  skills:
    first.yaml: |
      id: first
    Second.YmL: |
      id: second
VALUES
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" -f "$tmp/skills-values.yaml" \
  "${container_args[@]}" --show-only templates/fetcher-cronjob.yaml > "$tmp/container-skills.yaml"
grep -Fq 'path: skills/first.yaml' "$tmp/container-skills.yaml"
grep -Fq 'path: skills/Second.YmL' "$tmp/container-skills.yaml"
grep -Fq 'for file in /source/skills/*; do' "$tmp/container-skills.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "${container_args[@]}" \
  --set project.existingConfigMap=existing-project \
  --show-only templates/fetcher-cronjob.yaml > "$tmp/container-existing-project.yaml"
grep -Fq 'name: existing-project' "$tmp/container-existing-project.yaml"
grep -Fq 'name: prepare-project' "$tmp/container-existing-project.yaml"

container_service_account_args=(
  --set mode=cron
  --set ai.enabled=true
  --set ai.endpoint=http://model.orka-system.svc.cluster.local/v1/chat/completions
  --set ai.model=script-model
  --set ai.token=dashboard-token
  --set analysisRuntime.type=orka-container
  --set analysisRuntime.orkaContainer.image.tag=sha-deadbeef
  --set analysisRuntime.orkaContainer.modelAuth.existingSecret=orka-model
)
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  "${container_service_account_args[@]}" > "$tmp/container-service-account.yaml"
grep -Fq 'serviceAccountName: test-prow-ai-dashboard-orka' "$tmp/container-service-account.yaml"
grep -Fq 'automountServiceAccountToken: false' "$tmp/container-service-account.yaml"
grep -Fq 'name: ORKA_API_TOKEN_FILE' "$tmp/container-service-account.yaml"
grep -Fq 'value: /var/run/secrets/kubernetes.io/serviceaccount/token' "$tmp/container-service-account.yaml"
grep -A3 -F 'name: orka-api-token' "$tmp/container-service-account.yaml" | grep -Fq 'mountPath: /var/run/secrets/kubernetes.io/serviceaccount'
grep -A18 -F 'name: orka-api-token' "$tmp/container-service-account.yaml" | grep -Fq 'serviceAccountToken:'
grep -A18 -F 'name: orka-api-token' "$tmp/container-service-account.yaml" | grep -Fq 'name: kube-root-ca.crt'
grep -A18 -F 'name: orka-api-token' "$tmp/container-service-account.yaml" | grep -Fq 'path: ca.crt'
grep -A18 -F 'name: orka-api-token' "$tmp/container-service-account.yaml" | grep -Fq 'fieldPath: metadata.namespace'
if grep -Fq 'name: orka-api-token' "$tmp/prepare-project.yaml"; then
  echo 'project materializer received the projected Orka API token' >&2
  exit 1
fi
if grep -Eq '^[[:space:]]*- name: ORKA_API_TOKEN$' "$tmp/container-service-account.yaml"; then
  echo 'ServiceAccount container analysis rendered a static Orka token reference' >&2
  exit 1
fi
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "${container_args[@]}" \
  --set ai.contextWindowTokens=128000 --show-only templates/orka-analysis-admission.yaml > "$tmp/container-context-window.yaml"
grep -Fq "AI_CONTEXT_WINDOW_TOKENS" "$tmp/container-context-window.yaml"
grep -Fq 'e.value == \"128000\"' "$tmp/container-context-window.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "${container_args[@]}" \
  --set analysisCache.generation=0 --show-only templates/orka-analysis-admission.yaml > "$tmp/container-cache-generation.yaml"
grep -Fq 'AI_CACHE_GENERATION' "$tmp/container-cache-generation.yaml"
grep -Fq "size(object.spec.env) == 10 + (object.spec.env.exists(e, e.name == 'AI_CACHE_GENERATION') ? 1 : 0)" "$tmp/container-cache-generation.yaml"
grep -Fq 'e.value == \"0\"' "$tmp/container-cache-generation.yaml"
grep -Fq "e.value.matches('^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$')" "$tmp/container-analysis.yaml"
if grep -Eq 'resources: \["(tools|providers|agents|agentruntimes)"\]|type: ai|orka-producer|orka-ingestor|orka-artifact-tool' "$tmp/container-analysis.yaml"; then
  echo 'container analysis render contains a forbidden patched-worker resource' >&2
  exit 1
fi
if [[ $(grep -Fc 'app.kubernetes.io/component: orka-container-analysis-state' "$tmp/container-analysis.yaml") -ne 2 ]]; then
  echo 'chart-managed state key was not rendered in both namespaces' >&2
  exit 1
fi

container_watch_args=(
  --set mode=watch
  --set ai.enabled=true
  --set ai.endpoint=http://model.orka-system.svc.cluster.local/v1/chat/completions
  --set ai.model=script-model
  --set ai.token=dashboard-token
  --set analysisRuntime.type=orka-container
  --set analysisRuntime.orkaContainer.image.tag=sha-deadbeef
  --set analysisRuntime.orkaContainer.apiAuth.existingSecret=orka-api
  --set analysisRuntime.orkaContainer.modelAuth.existingSecret=orka-model
)
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  "${container_watch_args[@]}" > "$tmp/container-watch.yaml"
grep -Fq 'kind: Deployment' "$tmp/container-watch.yaml"
grep -Fq 'app.kubernetes.io/component: worker' "$tmp/container-watch.yaml"
grep -Fq 'type: Recreate' "$tmp/container-watch.yaml"
grep -Fq 'replicas: 1' "$tmp/container-watch.yaml"
grep -Fq -- '- /usr/local/bin/worker' "$tmp/container-watch.yaml"
grep -Fq -- '- -watch-interval=5m' "$tmp/container-watch.yaml"
grep -Fq -- '- -reconcile-interval=1h' "$tmp/container-watch.yaml"
for arg in \
  '-analysis-runtime=orka-container' \
  '-orka-analysis-namespace=test-prow-ai-dashboard-analysis-' \
  '-orka-analysis-api=http://orka.orka-system.svc.cluster.local:8080' \
  '-orka-analysis-image=ghcr.io/willie-yao/prow-ai-dashboard/analyzer:sha-deadbeef' \
  '-orka-analysis-model-secret=orka-model' \
  '-orka-analysis-model-token-key=token' \
  '-orka-analysis-state-secret=test-prow-ai-dashboard-analysis-state-' \
  '-orka-analysis-state-key=state-key' \
  '-orka-analysis-max-concurrent-tasks=2' \
  '-orka-analysis-poll-interval=2s' \
  '-orka-analysis-task-timeout=20m' \
  '-orka-analysis-retries=1' \
  '-orka-analysis-node-selector-json=' \
  '-orka-analysis-tolerations-json=' \
  '-orka-analysis-affinity-json='; do
  grep -Fq -- "$arg" "$tmp/container-watch.yaml"
done
grep -Fq 'name: prepare-project' "$tmp/container-watch.yaml"
grep -Fq 'image: busybox:1.36.1' "$tmp/container-watch.yaml"
grep -Fq 'cp -L /source/project.yaml /project/project.yaml' "$tmp/container-watch.yaml"
grep -Fq 'cp -L /source/prompts/system.md /project/prompts/system.md' "$tmp/container-watch.yaml"
grep -Fq 'for file in /source/skills/*; do' "$tmp/container-watch.yaml"
grep -Fq 'name: project-runtime' "$tmp/container-watch.yaml"
grep -Fq 'emptyDir: {}' "$tmp/container-watch.yaml"
grep -Fq 'name: PROW_AI_STATE_KEY' "$tmp/container-watch.yaml"
grep -Fq 'name: ORKA_API_TOKEN' "$tmp/container-watch.yaml"
grep -Fq 'name: orka-api' "$tmp/container-watch.yaml"
grep -Fq 'serviceAccountName: test-prow-ai-dashboard-orka' "$tmp/container-watch.yaml"
grep -Fq 'automountServiceAccountToken: false' "$tmp/container-watch.yaml"
grep -A18 -F 'name: orka-api-token' "$tmp/container-watch.yaml" | grep -Fq 'serviceAccountToken:'
if grep -Fq 'kind: CronJob' "$tmp/container-watch.yaml" || grep -Fq 'name: ORKA_API_TOKEN_FILE' "$tmp/container-watch.yaml"; then
  echo 'static-token Orka watch mode rendered a CronJob or result token file' >&2
  exit 1
fi

container_watch_service_account_args=(
  --set mode=watch
  --set ai.enabled=true
  --set ai.endpoint=http://model.orka-system.svc.cluster.local/v1/chat/completions
  --set ai.model=script-model
  --set ai.token=dashboard-token
  --set analysisRuntime.type=orka-container
  --set analysisRuntime.orkaContainer.image.tag=sha-deadbeef
  --set analysisRuntime.orkaContainer.modelAuth.existingSecret=orka-model
)
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  "${container_watch_service_account_args[@]}" --show-only templates/worker-deployment.yaml > "$tmp/container-watch-service-account.yaml"
grep -Fq 'name: ORKA_API_TOKEN_FILE' "$tmp/container-watch-service-account.yaml"
grep -Fq 'value: /var/run/secrets/kubernetes.io/serviceaccount/token' "$tmp/container-watch-service-account.yaml"
if grep -Eq '^[[:space:]]*- name: ORKA_API_TOKEN$|^[[:space:]]*- name: ORKA_ANALYSIS_API_TOKEN$' "$tmp/container-watch-service-account.yaml"; then
  echo 'ServiceAccount Orka watch mode rendered a static result token' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=watch \
  --set orka.fixRuntime.enabled=true \
  --set orka.fixRuntime.image.tag=sha-fixer \
  --show-only templates/worker-deployment.yaml > "$tmp/watch-fix-only.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/fixer:sha-fixer' "$tmp/watch-fix-only.yaml"
grep -Fq 'name: ORKA_API_TOKEN_FILE' "$tmp/watch-fix-only.yaml"
grep -Fq 'serviceAccountName: test-prow-ai-dashboard-orka' "$tmp/watch-fix-only.yaml"
if grep -Fq -- '-analysis-runtime=orka-container' "$tmp/watch-fix-only.yaml" || grep -Fq 'name: prepare-project' "$tmp/watch-fix-only.yaml"; then
  echo 'fix-only watch mode rendered container analysis wiring' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  "${container_watch_args[@]}" \
  --set orka.fixRuntime.enabled=true \
  --set orka.fixRuntime.image.tag=sha-fixer \
  --show-only templates/worker-deployment.yaml > "$tmp/watch-analysis-and-fix.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/fixer:sha-fixer' "$tmp/watch-analysis-and-fix.yaml"
grep -Fq 'name: ORKA_ANALYSIS_API_TOKEN' "$tmp/watch-analysis-and-fix.yaml"
grep -Fq 'name: ORKA_API_TOKEN_FILE' "$tmp/watch-analysis-and-fix.yaml"
if grep -Eq '^[[:space:]]*- name: ORKA_API_TOKEN$' "$tmp/watch-analysis-and-fix.yaml"; then
  echo 'combined Orka watch mode shared the analysis token with fix generation' >&2
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

for invalid in type endpoint model materializer-repository materializer-tag materializer-mutable materializer-policy custom-namespace shared-namespace release-namespace api api-token-key image mutable-image mutable-global build-metadata model-secret token-key state-key concurrency poll slow-poll timeout retries cpu-selector gpu accelerator; do
  case $invalid in
    type) invalid_args=(--set analysisRuntime.type=remote); want='analysisRuntime.type must be inprocess or orka-container' ;;
    endpoint) invalid_args=("${container_args[@]}" --set-string ai.endpoint=); want='analysisRuntime.type=orka-container requires ai.endpoint' ;;
    model) invalid_args=("${container_args[@]}" --set-string ai.model=); want='analysisRuntime.type=orka-container requires ai.model' ;;
    materializer-repository) invalid_args=("${container_args[@]}" --set-string project.materializer.image.repository=); want='project.materializer.image.repository is required for Orka container analysis' ;;
    materializer-tag) invalid_args=("${container_args[@]}" --set-string project.materializer.image.tag=); want='project.materializer.image.tag is required for Orka container analysis' ;;
    materializer-mutable) invalid_args=("${container_args[@]}" --set-string project.materializer.image.tag=latest); want='project.materializer.image.tag must be an immutable sha-<hex> or full semantic version' ;;
    materializer-policy) invalid_args=("${container_args[@]}" --set project.materializer.image.pullPolicy=Always); want='project.materializer.image.pullPolicy must be IfNotPresent' ;;
    custom-namespace) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.namespace=custom-analysis); want='analysisRuntime.orkaContainer.namespace must be dedicated to this release and end with its release scope' ;;
    shared-namespace) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.namespace=orka-system); want='analysisRuntime.orkaContainer.namespace must be dedicated and differ from orka.namespace' ;;
    release-namespace) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.namespace=dashboard-test); want='analysisRuntime.orkaContainer.namespace must differ from the dashboard release namespace' ;;
    api) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.api='http://user:secret@orka'); want='analysisRuntime.orkaContainer.api must be an absolute http or https URL without credentials' ;;
    api-token-key) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.apiAuth.tokenKey=); want='analysisRuntime.orkaContainer.apiAuth.tokenKey is required when apiAuth.existingSecret is set' ;;
    image) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.image.repository=); want='analysisRuntime.orkaContainer.image.repository is required' ;;
    mutable-image) invalid_args=("${container_args[@]}" --set-string analysisRuntime.orkaContainer.image.tag=main); want='analysisRuntime.orkaContainer.image tag must be an immutable sha-<hex> or full semantic version' ;;
    mutable-global) invalid_args=(--set-string global.imageTag=latest); want='global.imageTag must be an immutable sha-<hex> or full semantic version' ;;
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

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=continuous > "$tmp/invalid-mode.yaml" 2>&1; then
  echo 'chart accepted an unknown mode' >&2
  exit 1
fi
grep -Fq 'mode must be "cron" or "watch"' "$tmp/invalid-mode.yaml"

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
grep -Fq 'automountServiceAccountToken: false' "$tmp/fix-watch.yaml"
grep -Fq 'name: ORKA_API_TOKEN_FILE' "$tmp/fix-watch.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/fixer:sha-test' "$tmp/fix-watch.yaml"
grep -A3 -F 'name: orka-api-token' "$tmp/fix-watch.yaml" | grep -Fq 'mountPath: /var/run/secrets/kubernetes.io/serviceaccount'
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
grep -Fq 'automountServiceAccountToken: false' "$tmp/fix-cron.yaml"
grep -Fq 'name: ORKA_API_TOKEN_FILE' "$tmp/fix-cron.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/fixer:sha-test' "$tmp/fix-cron.yaml"
grep -A3 -F 'name: orka-api-token' "$tmp/fix-cron.yaml" | grep -Fq 'mountPath: /var/run/secrets/kubernetes.io/serviceaccount'
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
grep -Fq 'name: ORKA_ANALYSIS_API_TOKEN' "$tmp/combined-orka-runtimes.yaml"
grep -Fq 'name: ORKA_API_TOKEN_FILE' "$tmp/combined-orka-runtimes.yaml"
if grep -Eq '^[[:space:]]*- name: ORKA_API_TOKEN$' "$tmp/combined-orka-runtimes.yaml"; then
  echo 'combined Orka runtimes share the analysis static token' >&2
  exit 1
fi

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
grep -Fq 'checksum/project-config:' "$tmp/chat-server.yaml"
grep -Fq 'value: "/data/.analysis-chat"' "$tmp/chat-server.yaml"
grep -A1 -Fq 'name: ANALYSIS_CHAT_TIMEOUT' "$tmp/chat-server.yaml"
grep -Fq 'value: "2m"' "$tmp/chat-server.yaml"
grep -Fq 'name: ANALYSIS_CHAT_SESSION_TTL' "$tmp/chat-server.yaml"
grep -Fq 'value: "2h"' "$tmp/chat-server.yaml"
grep -Fq 'name: ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER' "$tmp/chat-server.yaml"
grep -Fq 'name: ANALYSIS_CHAT_REQUESTS_PER_MINUTE' "$tmp/chat-server.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.chat.timeout=10m \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --set ai.enabled=true \
  --set ai.token=test-token \
  --set ai.endpoint=http://model.test/v1/chat/completions \
  --set ai.model=test-model \
  --show-only templates/server-deployment.yaml > "$tmp/chat-timeout.yaml"
grep -A1 -Fq 'name: ANALYSIS_CHAT_TIMEOUT' "$tmp/chat-timeout.yaml"
grep -Fq 'value: "10m"' "$tmp/chat-timeout.yaml"
if grep -Fq 'name: ANALYSIS_CORRECTIONS_ENABLED' "$tmp/chat-server.yaml"; then
  echo 'analysis corrections enabled without explicit opt-in' >&2
  exit 1
fi
if grep -Fq 'name: ANALYSIS_SOURCE_INVESTIGATION_ENABLED' "$tmp/chat-server.yaml"; then
  echo 'source investigation enabled without explicit opt-in' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.chat.sourceInvestigation.enabled=true \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --set ai.enabled=true \
  --set ai.token=test-token \
  --set ai.githubReadToken=read-token \
  --set ai.endpoint=http://model.test/v1/chat/completions \
  --set ai.model=test-model > "$tmp/source-investigation.yaml"
grep -A1 -Fq 'name: ANALYSIS_SOURCE_INVESTIGATION_ENABLED' "$tmp/source-investigation.yaml"
grep -Fq 'name: ANALYSIS_SOURCE_INVESTIGATION_MAX_PER_SESSION' "$tmp/source-investigation.yaml"
grep -Fq 'name: ANALYSIS_SOURCE_INVESTIGATION_MAX_ACTIVE_PER_OWNER' "$tmp/source-investigation.yaml"
grep -A6 -Fq 'name: SOURCE_INVESTIGATION_GITHUB_TOKEN' "$tmp/source-investigation.yaml"
grep -Fq 'name: test-prow-ai-dashboard-github-read' "$tmp/source-investigation.yaml"
grep -Fq 'automountServiceAccountToken: true' "$tmp/source-investigation.yaml"
grep -Fq 'name: ORKA_API_TOKEN_FILE' "$tmp/source-investigation.yaml"
grep -Fq 'value: /var/run/secrets/kubernetes.io/serviceaccount/token' "$tmp/source-investigation.yaml"
grep -Fq 'app.kubernetes.io/component: orka-source-investigation' "$tmp/source-investigation.yaml"
grep -Fq 'app.kubernetes.io/component: orka-source-investigation-runtime' "$tmp/source-investigation.yaml"
grep -Fq 'serviceAccountName: test-prow-ai-dashboard-source' "$tmp/source-investigation.yaml"
grep -Fq 'resources: ["tasks"]' "$tmp/source-investigation.yaml"
grep -Fq 'verbs: ["create", "get", "patch", "delete"]' "$tmp/source-investigation.yaml"
if grep -Eq 'resources: \["(secrets|pods|agents|agentruntimes)"\]|name: BOT_TOKEN|image: .*fixer' "$tmp/source-investigation.yaml"; then
  echo 'source investigation render contains write credentials or broad runtime access' >&2
  exit 1
fi
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.chat.sourceInvestigation.enabled=true \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --set ai.enabled=true \
  --set ai.token=test-token \
  --show-only templates/orka-source-investigation-rbac.yaml > "$tmp/source-investigation-rbac.yaml"
if [[ $(grep -Fc '  - apiGroups:' "$tmp/source-investigation-rbac.yaml") -ne 1 ]] ||
   [[ $(grep -Fc '    resources: ["tasks"]' "$tmp/source-investigation-rbac.yaml") -ne 1 ]] ||
   [[ $(grep -Fc '    verbs: ["create", "get", "patch", "delete"]' "$tmp/source-investigation-rbac.yaml") -ne 1 ]]; then
  echo 'source investigation Role is not exactly Task-only create/get/patch/delete' >&2
  exit 1
fi
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "${container_args[@]}" \
  --set server.chat.enabled=true \
  --set server.chat.sourceInvestigation.enabled=true \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --show-only templates/server-deployment.yaml > "$tmp/source-with-container-analysis-server.yaml"
grep -Fq 'serviceAccountName: test-prow-ai-dashboard-source' "$tmp/source-with-container-analysis-server.yaml"
if grep -Fq 'serviceAccountName: test-prow-ai-dashboard-orka' "$tmp/source-with-container-analysis-server.yaml"; then
  echo 'source server shares the broader Orka analysis ServiceAccount' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.chat.sourceInvestigation.enabled=true \
  --set server.actions.enabled=true \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --set server.actions.proxy.botToken=test-token \
  --set orka.fixRuntime.enabled=true \
  --set orka.fixRuntime.image.tag=sha-test \
  --set ai.enabled=true \
  --set ai.token=test-token > "$tmp/source-with-fix-actions.yaml"
grep -Fq 'serviceAccountName: test-prow-ai-dashboard-source' "$tmp/source-with-fix-actions.yaml"
grep -Fq 'name: ORKA_API_TOKEN_FILE' "$tmp/source-with-fix-actions.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/fixer:sha-test' "$tmp/source-with-fix-actions.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.chat.sourceInvestigation.enabled=true \
  --set server.actions.enabled=true \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --set server.actions.proxy.botToken=test-token \
  --set orka.fixRuntime.enabled=true \
  --set ai.enabled=true \
  --set ai.token=test-token \
  --show-only templates/orka-fix-runtime-rbac.yaml > "$tmp/source-with-fix-rbac.yaml"
grep -Fq 'name: test-prow-ai-dashboard-orka' "$tmp/source-with-fix-rbac.yaml"
grep -Fq 'name: test-prow-ai-dashboard-source' "$tmp/source-with-fix-rbac.yaml"

long_fullname=$(printf 'a%.0s' {1..63})
long_source_name="${long_fullname:0:56}-source"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set fullnameOverride="$long_fullname" \
  --set server.chat.enabled=true \
  --set server.chat.sourceInvestigation.enabled=true \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --set ai.enabled=true \
  --set ai.token=test-token > "$tmp/source-investigation-long-name.yaml"
grep -Fq "serviceAccountName: $long_source_name" "$tmp/source-investigation-long-name.yaml"
grep -Fq "name: $long_source_name" "$tmp/source-investigation-long-name.yaml"
if [[ ${#long_source_name} -ne 63 ]]; then
  echo 'generated source ServiceAccount name is not a 63-character DNS label' >&2
  exit 1
fi

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.sourceInvestigation.enabled=true > "$tmp/source-without-chat.yaml" 2>&1; then
  echo 'source investigation accepted without analysis chat' >&2
  exit 1
fi
grep -Fq 'server.chat.sourceInvestigation.enabled requires server.chat.enabled' "$tmp/source-without-chat.yaml"

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.chat.sourceInvestigation.enabled=true \
  --set server.chat.sourceInvestigation.maxActivePerOwner=0 \
  --set ai.enabled=true \
  --set ai.token=test-token > "$tmp/source-invalid-limit.yaml" 2>&1; then
  echo 'source investigation accepted a non-positive active limit' >&2
  exit 1
fi
grep -Fq 'server.chat.sourceInvestigation.maxActivePerOwner must be positive' "$tmp/source-invalid-limit.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.chat.correctionsEnabled=true \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --set ai.enabled=true \
  --set ai.token=test-token \
  --show-only templates/server-deployment.yaml > "$tmp/chat-corrections.yaml"
grep -A1 -Fq 'name: ANALYSIS_CORRECTIONS_ENABLED' "$tmp/chat-corrections.yaml"
grep -Fq 'value: "true"' "$tmp/chat-corrections.yaml"

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.correctionsEnabled=true > "$tmp/corrections-without-chat.yaml" 2>&1; then
  echo 'analysis corrections accepted without analysis chat' >&2
  exit 1
fi
grep -Fq 'server.chat.correctionsEnabled requires server.chat.enabled' "$tmp/corrections-without-chat.yaml"
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
grep -A1 -F 'name: OAUTH_PRIVATE_REPOSITORIES' "$tmp/chat-oauth.yaml" | grep -Fq 'value: "false"'
if grep -Fq 'name: OAUTH_SCOPE' "$tmp/chat-oauth.yaml"; then
  echo 'chat-only OAuth rendered the removed OAUTH_SCOPE variable' >&2
  exit 1
fi
grep -A1 -Fq 'name: HSTS_ENABLED' "$tmp/chat-oauth.yaml"
grep -Fq 'value: "true"' "$tmp/chat-oauth.yaml"
if grep -Fq 'name: COOKIE_INSECURE' "$tmp/chat-oauth.yaml"; then
  echo 'OAuth deployment rendered insecure cookies by default' >&2
  exit 1
fi

oauth_action_args=(
  --set server.actions.enabled=true
  --set server.actions.mode=oauth
  --set server.actions.admins[0]=alice
  --set server.actions.oauth.clientId=client
  --set server.actions.oauth.clientSecret=secret
  --set server.actions.oauth.sessionKey=session-key
  --set server.actions.oauth.redirectUrl=https://dashboard.test/api/auth/callback
)
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  "${oauth_action_args[@]}" \
  --show-only templates/server-deployment.yaml > "$tmp/actions-oauth-public.yaml"
grep -A1 -F 'name: OAUTH_PRIVATE_REPOSITORIES' "$tmp/actions-oauth-public.yaml" | grep -Fq 'value: "false"'
if grep -Fq 'name: OAUTH_SCOPE' "$tmp/actions-oauth-public.yaml"; then
  echo 'public OAuth actions rendered the removed OAUTH_SCOPE variable' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  "${oauth_action_args[@]}" \
  --set server.actions.oauth.privateRepositories=true \
  --show-only templates/server-deployment.yaml > "$tmp/actions-oauth-private.yaml"
grep -A1 -F 'name: OAUTH_PRIVATE_REPOSITORIES' "$tmp/actions-oauth-private.yaml" | grep -Fq 'value: "true"'

for legacy_scope_key in scope chatScope; do
  if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
    "${oauth_action_args[@]}" \
    --set-string "server.actions.oauth.${legacy_scope_key}=repo" > "$tmp/oauth-legacy-${legacy_scope_key}.yaml" 2>&1; then
    echo "legacy OAuth ${legacy_scope_key} value was accepted" >&2
    exit 1
  fi
  grep -Fq 'server.actions.oauth.scope and server.actions.oauth.chatScope are no longer supported' "$tmp/oauth-legacy-${legacy_scope_key}.yaml"
  grep -Fq 'server.actions.oauth.privateRepositories=true' "$tmp/oauth-legacy-${legacy_scope_key}.yaml"
done

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  "${oauth_action_args[@]}" \
  --set 'server.extraEnv[0].name=OAUTH_SCOPE' \
  --set 'server.extraEnv[0].value=repo' > "$tmp/oauth-extra-env-scope.yaml" 2>&1; then
  echo 'legacy OAUTH_SCOPE extra environment variable was accepted' >&2
  exit 1
fi
grep -Fq 'server.extraEnv must not set OAUTH_SCOPE' "$tmp/oauth-extra-env-scope.yaml"

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.actions.mode=oauth \
  --set server.actions.admins[0]=alice \
  --set server.actions.oauth.clientId=client \
  --set server.actions.oauth.clientSecret=secret \
  --set server.actions.oauth.sessionKey=session-key \
  --set server.actions.oauth.redirectUrl=https://dashboard.test/api/auth/callback \
  --set server.actions.oauth.privateRepositories=true \
  --set ai.enabled=true \
  --set ai.token=test-token > "$tmp/chat-oauth-private.yaml" 2>&1; then
  echo 'chat-only OAuth accepted private-repository access' >&2
  exit 1
fi
grep -Fq 'server.actions.oauth.privateRepositories=true requires server.actions.enabled=true; chat-only OAuth uses read:user' "$tmp/chat-oauth-private.yaml"

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.actions.mode=oauth \
  --set server.actions.admins[0]=alice \
  --set server.actions.oauth.clientId=client \
  --set server.actions.oauth.clientSecret=secret \
  --set server.actions.oauth.sessionKey=session-key \
  --set server.actions.oauth.redirectUrl=http://localhost:8080/api/auth/callback \
  --set ai.enabled=true \
  --set ai.token=test-token \
  --set server.development.allowInsecureCookies=true > "$tmp/insecure-with-hsts.yaml" 2>&1; then
  echo 'insecure OAuth cookies rendered while HSTS was enabled' >&2
  exit 1
fi
grep -Fq 'server.development.allowInsecureCookies requires server.security.hsts.enabled=false' "$tmp/insecure-with-hsts.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true \
  --set server.actions.mode=oauth \
  --set server.actions.admins[0]=alice \
  --set server.actions.oauth.clientId=client \
  --set server.actions.oauth.clientSecret=secret \
  --set server.actions.oauth.sessionKey=session-key \
  --set server.actions.oauth.redirectUrl=http://localhost:8080/api/auth/callback \
  --set ai.enabled=true \
  --set ai.token=test-token \
  --set server.security.hsts.enabled=false \
  --set server.development.allowInsecureCookies=true \
  --show-only templates/server-deployment.yaml > "$tmp/insecure-local-oauth.yaml"
grep -A1 -Fq 'name: COOKIE_INSECURE' "$tmp/insecure-local-oauth.yaml"
grep -Fq 'value: "1"' "$tmp/insecure-local-oauth.yaml"
if grep -Fq 'name: HSTS_ENABLED' "$tmp/insecure-local-oauth.yaml"; then
  echo 'local HTTP OAuth rendered HSTS' >&2
  exit 1
fi

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.extraEnv[0].name=COOKIE_INSECURE \
  --set-string server.extraEnv[0].value=1 > "$tmp/insecure-extra-env.yaml" 2>&1; then
  echo 'server.extraEnv accepted COOKIE_INSECURE' >&2
  exit 1
fi
grep -Fq 'server.extraEnv must not set COOKIE_INSECURE' "$tmp/insecure-extra-env.yaml"

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
  --set server.chat.enabled=true \
  --set server.chat.maxActiveTurnsPerOwner=0 \
  --set server.actions.mode=proxy \
  --set server.actions.admins[0]=alice \
  --set ai.enabled=true \
  --set ai.token=test-token > "$tmp/chat-invalid-active-limit.yaml" 2>&1; then
  echo 'chat accepted a non-positive active turn limit' >&2
  exit 1
fi
grep -Fq 'server.chat.maxActiveTurnsPerOwner must be positive' "$tmp/chat-invalid-active-limit.yaml"

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set server.chat.enabled=true > "$tmp/chat-without-ai.yaml" 2>&1; then
  echo 'server.chat.enabled was accepted without ai.enabled' >&2
  exit 1
fi
grep -Fq 'server.chat.enabled requires ai.enabled' "$tmp/chat-without-ai.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true --set ai.token=test-token \
  --set analysisCache.generation=2 \
  --show-only templates/worker-deployment.yaml > "$tmp/cache-generation-worker.yaml"
grep -A1 -F 'name: AI_CACHE_GENERATION' "$tmp/cache-generation-worker.yaml" | grep -Fq 'value: "2"'

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron --set ai.enabled=true --set ai.token=test-token \
  --set analysisCache.generation=2 \
  --show-only templates/fetcher-cronjob.yaml > "$tmp/cache-generation-cron.yaml"
grep -A1 -F 'name: AI_CACHE_GENERATION' "$tmp/cache-generation-cron.yaml" | grep -Fq 'value: "2"'
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true --set ai.token=test-token \
  --set analysisCache.generation=0 \
  --show-only templates/worker-deployment.yaml > "$tmp/cache-generation-zero.yaml"
grep -A1 -F 'name: AI_CACHE_GENERATION' "$tmp/cache-generation-zero.yaml" | grep -Fq 'value: "0"'

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true --set ai.token=test-token \
  --set ai.githubReadToken=read-token \
  --show-only templates/secret-github-read.yaml > "$tmp/github-read-inline-secret.yaml"
grep -Fq 'name: test-prow-ai-dashboard-github-read' "$tmp/github-read-inline-secret.yaml"
grep -Fq 'GITHUB_READ_TOKEN: "read-token"' "$tmp/github-read-inline-secret.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true --set ai.token=test-token \
  --set ai.githubReadToken=read-token \
  --show-only templates/secret-ai.yaml > "$tmp/github-read-ai-secret.yaml"
if grep -Fq 'GITHUB_READ_TOKEN' "$tmp/github-read-ai-secret.yaml"; then
  echo 'GitHub read token was stored in the model Secret' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true --set ai.token=test-token \
  --set ai.githubReadTokenSecretName=github-read \
  --show-only templates/worker-deployment.yaml > "$tmp/github-read-worker.yaml"
grep -A5 -Fq 'name: GITHUB_READ_TOKEN' "$tmp/github-read-worker.yaml"
grep -Fq 'name: github-read' "$tmp/github-read-worker.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron --set ai.enabled=true --set ai.token=test-token \
  --set ai.githubReadTokenSecretName=github-read \
  --show-only templates/fetcher-cronjob.yaml > "$tmp/github-read-cron.yaml"
grep -A5 -Fq 'name: GITHUB_READ_TOKEN' "$tmp/github-read-cron.yaml"
grep -Fq 'name: github-read' "$tmp/github-read-cron.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "${container_args[@]}" \
  --set ai.githubReadTokenSecretName=github-read > "$tmp/github-read-container.yaml"
grep -Fq -- '-orka-analysis-github-secret=github-read' "$tmp/github-read-container.yaml"
grep -Fq -- '-orka-analysis-github-token-key=GITHUB_READ_TOKEN' "$tmp/github-read-container.yaml"
grep -Fq "e.name == 'GITHUB_READ_TOKEN'" "$tmp/github-read-container.yaml"
grep -Fq 'GitHub read Secret' "$tmp/github-read-container.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "${container_watch_args[@]}" \
  --set ai.githubReadTokenSecretName=github-read \
  --show-only templates/worker-deployment.yaml > "$tmp/github-read-container-watch.yaml"
grep -Fq -- '-orka-analysis-github-secret=github-read' "$tmp/github-read-container-watch.yaml"
grep -Fq -- '-orka-analysis-github-token-key=GITHUB_READ_TOKEN' "$tmp/github-read-container-watch.yaml"
grep -A5 -Fq 'name: GITHUB_READ_TOKEN' "$tmp/github-read-container-watch.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true --set ai.token=test-token \
  --set ai.githubReadToken=read-token \
  --show-only templates/configmap-project.yaml > "$tmp/github-read-configmap.yaml"
if grep -Fq 'read-token' "$tmp/github-read-configmap.yaml"; then
  echo 'GitHub read token leaked into project ConfigMap' >&2
  exit 1
fi

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true --set ai.token=test-token \
  --set ai.githubReadToken=read-token \
  --set ai.githubReadTokenSecretName=github-read > "$tmp/github-read-conflict.yaml" 2>&1; then
  echo 'GitHub read token accepted inline and Secret name together' >&2
  exit 1
fi
grep -Fq 'ai.githubReadToken and ai.githubReadTokenSecretName are mutually exclusive' "$tmp/github-read-conflict.yaml"

echo 'Helm render checks passed.'

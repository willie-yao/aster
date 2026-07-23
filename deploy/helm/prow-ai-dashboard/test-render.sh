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
for removed in orka-producer orka-ingestor orka-artifact-tool submit-analysis 'type: ai' 'kind: RoleBinding'; do
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
  if grep -Eq 'resources: \["(tools|configmaps)"\]' "$tmp/rbac-$namespace.yaml"; then
    echo 'fix runtime RBAC includes analysis resources' >&2
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

echo 'Helm render checks passed.'

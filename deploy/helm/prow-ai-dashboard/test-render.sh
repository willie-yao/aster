#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
chart="$root/deploy/helm/prow-ai-dashboard"
tmp="${TMPDIR:-/tmp}/prow-ai-dashboard-helm-$$"
mkdir -p "$tmp"

container_command() {
  local name=$1
  local file=$2
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

for file in 30-tools.yaml 35-k8s-tools.yaml 36-submit-tool.yaml \
  37-verify-timeline.yaml 38-transient-signatures.yaml 39-recurrence.yaml \
  40-diff-last-passing.yaml 41-required-evidence.yaml; do
  cmp "$root/experimental/orka/manifests/$file" "$chart/files/orka-tools/$file"
done

helm lint "$chart" -f "$tmp/values.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" > "$tmp/inprocess.yaml"
if grep -Fq 'test-prow-ai-dashboard-artifact-tool' "$tmp/inprocess.yaml"; then
  echo 'in-process render unexpectedly created Orka artifact Tool resources' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set analysis=orka \
  --set orka.namespace=orka-test \
  --set orka.artifactTool.nodeSelector.agentpool=nodepool1 \
  > "$tmp/owned.yaml"

grep -Eq 'name: test-prow-ai-dashboard-artifact-tool-[0-9a-f]{8}' "$tmp/owned.yaml"
grep -Fq 'namespace: orka-test' "$tmp/owned.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/orka-artifact-tool:sha-test' "$tmp/owned.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/orka-producer:sha-test' "$tmp/owned.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/orka-ingestor:sha-test' "$tmp/owned.yaml"
grep -Eq 'http://test-prow-ai-dashboard-artifact-tool-[0-9a-f]{8}\.orka-test\.svc:8080/tool/read_artifact' "$tmp/owned.yaml"
grep -Eq -- '-tool-auth-secret=test-prow-ai-dashboard-artifact-tool-[0-9a-f]{8}-auth' "$tmp/owned.yaml"
grep -Fq 'name: test-prow-ai-dashboard-orka-tools' "$tmp/owned.yaml"
grep -Fq 'name: submit-analysis' "$tmp/owned.yaml"
grep -Fq '/tool/submit_analysis' "$tmp/owned.yaml"
grep -Fq 'agentpool: nodepool1' "$tmp/owned.yaml"
grep -Fq 'suspend: false' "$tmp/owned.yaml"
grep -Fq -- '- -max-concurrent-tasks=2' "$tmp/owned.yaml"
grep -Fq -- '- -task-poll=5s' "$tmp/owned.yaml"
grep -Fq -- '- -wave-timeout=30m' "$tmp/owned.yaml"
helm install test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set analysis=orka \
  --set orka.namespace=orka-test \
  --dry-run=client > "$tmp/owned-notes.txt"
grep -Fq 'orka-ops.sh --namespace orka-test preflight' "$tmp/owned-notes.txt"
grep -Fq -- '--service-account dashboard-test/test-prow-ai-dashboard-orka' "$tmp/owned-notes.txt"
grep -Fq 'orka-ops.sh --namespace orka-test status' "$tmp/owned-notes.txt"
if [[ $(container_command produce "$tmp/owned.yaml") != /app ]]; then
  echo 'specialized Orka producer command is not /app' >&2
  exit 1
fi
if [[ $(container_command ingest "$tmp/owned.yaml") != /app ]]; then
  echo 'specialized Orka ingestor command is not /app' >&2
  exit 1
fi
grep -Fq -- '- -api-mode=auto' "$tmp/owned.yaml"
if grep -Fq -- '-task-execution=' "$tmp/owned.yaml"; then
  echo 'default Orka render unexpectedly added Task placement' >&2
  exit 1
fi
skip_count=$(grep -Fc -- '- -skip-side-effects' "$tmp/owned.yaml")
if [[ $skip_count -ne 1 ]]; then
  echo "default Orka render has $skip_count skip-side-effects flags, want skeleton fetch only" >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set analysis=orka \
  --set orka.fixRuntime.enabled=true \
  --show-only templates/fetcher-cronjob.yaml \
  > "$tmp/fixer-runtime.yaml"
if [[ $(container_command produce "$tmp/fixer-runtime.yaml") != /app ]]; then
  echo 'fix-runtime Orka producer command is not /app' >&2
  exit 1
fi
if [[ $(container_command ingest "$tmp/fixer-runtime.yaml") != /usr/local/bin/orka-ingestor ]]; then
  echo 'fix-runtime Orka ingestor command is not /usr/local/bin/orka-ingestor' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set analysis=orka \
  --set fetcher.suspend=true \
  --set orka.sideEffects.enabled=false \
  --show-only templates/fetcher-cronjob.yaml \
  > "$tmp/evaluation.yaml"
grep -Fq 'suspend: true' "$tmp/evaluation.yaml"
skip_count=$(grep -Fc -- '- -skip-side-effects' "$tmp/evaluation.yaml")
if [[ $skip_count -ne 2 ]]; then
  echo "evaluation render has $skip_count skip-side-effects flags, want fetch and ingest" >&2
  exit 1
fi

cat > "$tmp/task-execution-values.yaml" <<'VALUES'
orka:
  producer:
    maxConcurrentTasks: 3
    taskPoll: 2s
    waveTimeout: 20m
  taskExecution:
    nodeSelector:
      agentpool: cpu
    tolerations:
      - key: dedicated
        operator: Equal
        value: orka
        effect: NoSchedule
    affinity:
      nodeAffinity:
        preferredDuringSchedulingIgnoredDuringExecution:
          - weight: 100
            preference:
              matchExpressions:
                - key: agentpool
                  operator: In
                  values: [cpu]
VALUES
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  -f "$tmp/task-execution-values.yaml" \
  --set mode=cron --set analysis=orka \
  --show-only templates/fetcher-cronjob.yaml > "$tmp/task-execution.yaml"
grep -Fq -- '- -max-concurrent-tasks=3' "$tmp/task-execution.yaml"
grep -Fq -- '- -task-poll=2s' "$tmp/task-execution.yaml"
grep -Fq -- '- -wave-timeout=20m' "$tmp/task-execution.yaml"
if [[ $(grep -Fc -- '-task-execution=' "$tmp/task-execution.yaml") -ne 2 ]]; then
  echo 'Task placement was not passed to both producer and ingestor' >&2
  exit 1
fi
grep -Fq '\"nodeSelector\":{\"agentpool\":\"cpu\"}' "$tmp/task-execution.yaml"
grep -Fq '\"tolerations\":[{\"effect\":\"NoSchedule\",\"key\":\"dedicated\",\"operator\":\"Equal\",\"value\":\"orka\"}]' "$tmp/task-execution.yaml"
grep -Fq '\"affinity\":{\"nodeAffinity\"' "$tmp/task-execution.yaml"


if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron --set analysis=orka --set orka.apiMode=chat \
  > "$tmp/invalid-api-mode.yaml" 2>&1; then
  echo 'chart accepted an invalid Orka API mode' >&2
  exit 1
fi
grep -Fq 'orka.apiMode must be auto, responses, or chat_completions' "$tmp/invalid-api-mode.yaml"

for invalid in negative nonnumeric overlimit overflow; do
  case $invalid in
    negative) concurrency_args=(--set orka.producer.maxConcurrentTasks=-1) ;;
    nonnumeric) concurrency_args=(--set-string orka.producer.maxConcurrentTasks=two) ;;
    overlimit) concurrency_args=(--set orka.producer.maxConcurrentTasks=1001) ;;
    overflow) concurrency_args=(--set-string orka.producer.maxConcurrentTasks=999999999999999999999999999999999999) ;;
  esac
  if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
    --set mode=cron --set analysis=orka "${concurrency_args[@]}" \
    > "$tmp/invalid-concurrency-$invalid.yaml" 2>&1; then
    echo "$invalid producer concurrency was accepted" >&2
    exit 1
  fi
  grep -Fq 'must be an integer between 0 and 1000' "$tmp/invalid-concurrency-$invalid.yaml"
done

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

artifact_name_a=$(helm template test "$chart" -n dashboard-a -f "$tmp/values.yaml" \
  --set mode=cron --set analysis=orka \
  --show-only templates/orka-artifact-tool-service.yaml |
  awk '$1 == "name:" { print $2; exit }')
artifact_name_b=$(helm template test "$chart" -n dashboard-b -f "$tmp/values.yaml" \
  --set mode=cron --set analysis=orka \
  --show-only templates/orka-artifact-tool-service.yaml |
  awk '$1 == "name:" { print $2; exit }')
if [[ -z "$artifact_name_a" || -z "$artifact_name_b" || "$artifact_name_a" == "$artifact_name_b" ]]; then
  echo 'artifact Tool names are not isolated by source release namespace' >&2
  exit 1
fi

for namespace in dashboard-a dashboard-b; do
  helm template test "$chart" -n "$namespace" -f "$tmp/values.yaml" \
    --set mode=cron --set analysis=orka \
    --show-only templates/orka-pipeline-rbac.yaml \
    > "$tmp/rbac-$namespace.yaml"
done
rbac_name_a=$(awk '$1 == "kind:" { kind=$2 } kind == "Role" && $1 == "name:" { print $2; exit }' "$tmp/rbac-dashboard-a.yaml")
rbac_name_b=$(awk '$1 == "kind:" { kind=$2 } kind == "Role" && $1 == "name:" { print $2; exit }' "$tmp/rbac-dashboard-b.yaml")
if [[ -z "$rbac_name_a" || -z "$rbac_name_b" || "$rbac_name_a" == "$rbac_name_b" ]]; then
  echo 'Orka RBAC names are not isolated by source release namespace' >&2
  exit 1
fi

for token in alpha beta; do
  helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
    --set mode=cron --set analysis=orka \
    --set orka.artifactTool.auth.token="$token" \
    --show-only templates/orka-artifact-tool-deployment.yaml \
    > "$tmp/token-$token.yaml"
done
checksum_alpha=$(awk '/checksum\/artifact-tool-auth:/ { print $2; exit }' "$tmp/token-alpha.yaml")
checksum_beta=$(awk '/checksum\/artifact-tool-auth:/ { print $2; exit }' "$tmp/token-beta.yaml")
if [[ -z "$checksum_alpha" || -z "$checksum_beta" || "$checksum_alpha" == "$checksum_beta" ]]; then
  echo 'artifact Tool token changes do not update the pod template checksum' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set analysis=orka \
  --set orka.namespace=orka-test \
  --set orka.artifactTool.enabled=false \
  --set orka.artifactTool.baseURL=http://shared-tools.orka-test.svc:9090 \
  --set orka.artifactTool.auth.existingSecret=shared-tool-auth \
  > "$tmp/external.yaml"

if grep -Fq 'name: test-prow-ai-dashboard-artifact-tool' "$tmp/external.yaml"; then
  echo 'external artifact Tool render unexpectedly created release-owned resources' >&2
  exit 1
fi
grep -Fq 'http://shared-tools.orka-test.svc:9090/tool/read_artifact' "$tmp/external.yaml"
grep -Fq -- '-tool-auth-secret=shared-tool-auth' "$tmp/external.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set analysis=orka \
  --set orka.baseTools.create=false \
  --set orka.baseTools.existingConfigMap=shared-tools \
  > "$tmp/existing-tools.yaml"
grep -Fq 'name: shared-tools' "$tmp/existing-tools.yaml"

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron --set analysis=orka \
  --set orka.artifactTool.enabled=false > "$tmp/invalid-artifact.yaml" 2>&1; then
  echo 'missing external artifact Tool values were accepted' >&2
  exit 1
fi

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron --set analysis=orka \
  --set orka.baseTools.create=false > "$tmp/invalid-tools.yaml" 2>&1; then
  echo 'missing base Tool ConfigMap values were accepted' >&2
  exit 1
fi

echo 'Helm render checks passed.'

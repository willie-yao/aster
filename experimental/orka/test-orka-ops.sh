#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
script="$root/experimental/orka/orka-ops.sh"
tmp="${TMPDIR:-/tmp}/prow-ai-dashboard-orka-ops-$$"
mkdir -p "$tmp/bin"
trap 'rm -rf "$tmp"' EXIT

bash -n "$script"

cat > "$tmp/bin/kubectl" <<'EOF_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$CALLS"
args="$*"

if [[ $args == *' get namespace '* ]]; then
  printf 'resource/test\n'
  exit 0
fi
if [[ $args == *' get crd '* ]]; then
  printf '%s' "${FAKE_CRD_ESTABLISHED:-True}"
  exit 0
fi
if [[ $args == *' get deployments '* && $args == *'status.readyReplicas'* ]]; then
  if [[ -n ${FAKE_CONTROLLER_ROWS:-} ]]; then
    printf '%s' "$FAKE_CONTROLLER_ROWS"
  else
    printf 'orka-controller\t1\t1\t1\torka\torka\t--api-port=8080,--ai-worker-image=%s,\n' "${FAKE_WORKER_IMAGE:-compat-image}"
  fi
  exit 0
fi
if [[ $args == *' get services '* ]]; then
  printf 'orka\tapi,metrics,\n'
  exit 0
fi
if [[ $args == *' get endpoints '* ]]; then
  printf '10.0.0.1\n'
  exit 0
fi
if [[ $args == *' get provider.core.orka.ai '* && $args == *'status.message'* ]]; then
  printf '%s' "${FAKE_PROVIDER_STATE:-true|Ready}"
  exit 0
fi
if [[ $args == *' get provider.core.orka.ai '* ]]; then
  printf '%s' "${FAKE_PROVIDER_READY:-true}"
  exit 0
fi
if [[ $args == *' get serviceaccount '* ]]; then
  if [[ ${FAKE_SERVICE_ACCOUNT_EXISTS:-true} == true ]]; then
    printf 'serviceaccount/test\n'
    exit 0
  fi
  exit 1
fi
if [[ $args == *' auth can-i '* ]]; then
  if [[ ${FAKE_RBAC_DENY:-} == "$2 $3" ]]; then
    printf 'no\n'
  else
    printf 'yes\n'
  fi
  exit 0
fi
if [[ $args == *' create -f -'* ]]; then
  cat > "$SMOKE_MANIFEST"
  if [[ ${FAKE_CREATE_ALREADY_EXISTS:-false} == true ]]; then
    echo 'Error from server (AlreadyExists): tasks.core.orka.ai already exists' >&2
    exit 1
  fi
  if [[ ${FAKE_CREATE_ERROR_AFTER_ACCEPT:-false} == true ]]; then
    exit 1
  fi
  exit 0
fi
if [[ $args == *' get task.core.orka.ai prow-ai-dashboard-smoke-'* ]]; then
  if [[ -n ${FAKE_SMOKE_PHASE:-} ]]; then
    printf '%s|false|provider failed|smoke-job' "$FAKE_SMOKE_PHASE"
    exit 0
  fi
  count=0
  [[ -f $SMOKE_COUNT ]] && count=$(cat "$SMOKE_COUNT")
  count=$((count + 1))
  printf '%d' "$count" > "$SMOKE_COUNT"
  if (( count == 1 )); then
    printf 'Pending|||'
  else
    printf 'Succeeded|true||smoke-job'
  fi
  exit 0
fi
if [[ $args == *' get tasks.core.orka.ai '* && $args == *'go-template='* ]]; then
  printf '%s' "${FAKE_TASKS:-}"
  exit 0
fi
if [[ $args == *' get tools.core.orka.ai '* && $args == *'go-template='* ]]; then
  printf '%s' "${FAKE_TOOLS:-}"
  exit 0
fi
if [[ $args == *' delete task.core.orka.ai '* ]]; then
  exit 0
fi
if [[ $args == *' logs job/'* ]]; then
  if [[ -n ${FAKE_SMOKE_LOGS:-} ]]; then
    printf '%s\n' "$FAKE_SMOKE_LOGS"
  else
    printf 'Model request completed iteration=1 api_mode=%s response_id=%s\n' "${FAKE_SMOKE_API_MODE:-responses}" "${FAKE_SMOKE_RESPONSE_ID:-resp-smoke}"
  fi
  exit 0
fi
if [[ $args == *' describe task.core.orka.ai '* ]]; then
  exit 0
fi

echo "unexpected kubectl invocation: $args" >&2
exit 1
EOF_KUBECTL
chmod +x "$tmp/bin/kubectl"

export PATH="$tmp/bin:$PATH"
export KUBECTL="$tmp/bin/kubectl"
export CALLS="$tmp/calls"
export SMOKE_MANIFEST="$tmp/smoke.yaml"
export SMOKE_COUNT="$tmp/smoke-count"
export ORKA_OPS_POLL_SECONDS=0
export ORKA_OPS_RANDOM_TOKEN=0123456789abcdef
: > "$CALLS"

"$script" --context test --namespace orka-system preflight \
  --provider copilot --worker-image compat-image \
  --service-account dashboards/dashboard-orka > "$tmp/preflight.txt"
grep -Fq 'PASS  Provider copilot is ready' "$tmp/preflight.txt"
grep -Fq 'PASS  AI worker image is compat-image' "$tmp/preflight.txt"
grep -Fq 'Preflight passed.' "$tmp/preflight.txt"
grep -Fq -- '--context test auth can-i delete tools.core.orka.ai -n orka-system --as=system:serviceaccount:dashboards:dashboard-orka' "$CALLS"
grep -Fq -- '--context test -n orka-system get services -l app.kubernetes.io/instance=orka,app.kubernetes.io/name=orka' "$CALLS"

if FAKE_SERVICE_ACCOUNT_EXISTS=false "$script" --namespace orka-system preflight \
  --provider copilot --service-account dashboards/missing > "$tmp/preflight-missing-sa.txt" 2>&1; then
  echo 'preflight accepted a missing ServiceAccount' >&2
  exit 1
fi
grep -Fq 'ServiceAccount dashboards/missing is missing or unreadable' "$tmp/preflight-missing-sa.txt"

if FAKE_WORKER_IMAGE=wrong-image "$script" --namespace orka-system preflight \
  --provider copilot --worker-image compat-image > "$tmp/preflight-fail.txt" 2>&1; then
  echo 'preflight accepted the wrong compatibility worker image' >&2
  exit 1
fi
grep -Fq 'AI worker image is wrong-image, expected compat-image' "$tmp/preflight-fail.txt"

unrelated_row=$'other-controller\t1\t1\t1\tother\tother\t--metrics-bind-address=:8080,\n'
if FAKE_CONTROLLER_ROWS="$unrelated_row" "$script" --namespace orka-system preflight \
  --provider copilot > "$tmp/preflight-unrelated-controller.txt" 2>&1; then
  echo 'preflight accepted an unrelated controller API Service' >&2
  exit 1
fi
grep -Fq 'no Orka controller Deployment was found' "$tmp/preflight-unrelated-controller.txt"

: > "$CALLS"
rm -f "$SMOKE_COUNT" "$SMOKE_MANIFEST"
"$script" --namespace orka-system smoke --provider copilot --model claude-test --expect-api responses --timeout 5s \
  > "$tmp/smoke.txt"
grep -Fq 'Smoke Task succeeded with an available result.' "$tmp/smoke.txt"
grep -Fq 'Smoke Task API mode: responses (response resp-smoke)' "$tmp/smoke.txt"
grep -Fq 'Deleted smoke Task orka-system/' "$tmp/smoke.txt"
grep -Fq 'model: "claude-test"' "$SMOKE_MANIFEST"
grep -Fq 'Reply with exactly: PROW_AI_DASHBOARD_ORKA_OK_' "$SMOKE_MANIFEST"
grep -Fq 'delete task.core.orka.ai prow-ai-dashboard-smoke-' "$CALLS"

: > "$CALLS"
rm -f "$SMOKE_COUNT" "$SMOKE_MANIFEST"
if FAKE_SMOKE_API_MODE=chat_completions "$script" --namespace orka-system smoke \
  --provider copilot --expect-api responses --timeout 5s > "$tmp/smoke-api-mismatch.txt" 2>&1; then
  echo 'smoke accepted an API mode mismatch' >&2
  exit 1
fi
grep -Fq 'Smoke Task used chat_completions, expected responses' "$tmp/smoke-api-mismatch.txt"
grep -Fq 'delete task.core.orka.ai prow-ai-dashboard-smoke-' "$CALLS"

: > "$CALLS"
rm -f "$SMOKE_COUNT" "$SMOKE_MANIFEST"
if FAKE_SMOKE_LOGS=$'Model request completed iteration=1 api_mode=responses response_id=resp-1\nModel request completed iteration=2 api_mode=chat_completions response_id=chat-2' \
  "$script" --namespace orka-system smoke --provider copilot --timeout 5s \
  > "$tmp/smoke-api-mixed.txt" 2>&1; then
  echo 'smoke accepted mixed API modes' >&2
  exit 1
fi
grep -Fq 'Smoke Task used multiple API modes: chat_completions,responses' "$tmp/smoke-api-mixed.txt"
grep -Fq 'delete task.core.orka.ai prow-ai-dashboard-smoke-' "$CALLS"

: > "$CALLS"
rm -f "$SMOKE_COUNT" "$SMOKE_MANIFEST"
if FAKE_SMOKE_API_MODE=responses_v2 "$script" --namespace orka-system smoke \
  --provider copilot --timeout 5s > "$tmp/smoke-api-unsupported.txt" 2>&1; then
  echo 'smoke accepted an unsupported API mode' >&2
  exit 1
fi
grep -Fq 'Smoke Task reported unsupported API mode telemetry: responses_v2' "$tmp/smoke-api-unsupported.txt"
grep -Fq 'delete task.core.orka.ai prow-ai-dashboard-smoke-' "$CALLS"

: > "$CALLS"
rm -f "$SMOKE_COUNT" "$SMOKE_MANIFEST"
if FAKE_CREATE_ERROR_AFTER_ACCEPT=true "$script" --namespace orka-system smoke \
  --provider copilot --timeout 5s > "$tmp/smoke-create-ambiguous.txt" 2>&1; then
  echo 'smoke accepted an ambiguous create failure' >&2
  exit 1
fi
grep -Fq 'Creating smoke Task orka-system/' "$tmp/smoke-create-ambiguous.txt"
grep -Fq 'cleanup was attempted' "$tmp/smoke-create-ambiguous.txt"
grep -Fq 'delete task.core.orka.ai prow-ai-dashboard-smoke-' "$CALLS"

: > "$CALLS"
rm -f "$SMOKE_COUNT" "$SMOKE_MANIFEST"
if FAKE_CREATE_ALREADY_EXISTS=true "$script" --namespace orka-system smoke \
  --provider copilot --timeout 5s > "$tmp/smoke-name-collision.txt" 2>&1; then
  echo 'smoke accepted a Task name collision' >&2
  exit 1
fi
grep -Fq 'Smoke Task name collision for orka-system/prow-ai-dashboard-smoke-' "$tmp/smoke-name-collision.txt"
grep -Fq 'the existing Task was not deleted' "$tmp/smoke-name-collision.txt"
if grep -Fq 'delete task.core.orka.ai prow-ai-dashboard-smoke-' "$CALLS"; then
  echo 'smoke deleted another invocation after AlreadyExists' >&2
  exit 1
fi

: > "$CALLS"
rm -f "$SMOKE_COUNT" "$SMOKE_MANIFEST"
if FAKE_SMOKE_PHASE=Failed "$script" --namespace orka-system smoke \
  --provider copilot --timeout 5s > "$tmp/smoke-failed.txt" 2>&1; then
  echo 'smoke accepted a failed Task' >&2
  exit 1
fi
grep -Fq 'Smoke Task Failed: provider failed' "$tmp/smoke-failed.txt"
grep -Fq 'delete task.core.orka.ai prow-ai-dashboard-smoke-' "$CALLS"

project=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
build_old=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
build_active=cccccccccccccccccccccccccccccccc
export FAKE_TASKS
FAKE_TASKS=$(cat <<EOF_TASKS
$project	$build_old	analysis	task-old	Succeeded	2019-01-01T00:00:00Z	2020-01-01T00:00:00Z	<no value>
$project	$build_active	analysis	task-active	Running	2020-01-01T00:00:00Z	<no value>	<no value>
$project	<no value>	pattern	task-new	Succeeded	2020-01-01T00:00:00Z	2099-01-01T00:00:00Z	<no value>
<no value>	<no value>	<no value>	legacy-task	Failed	2020-01-01T00:00:00Z	<no value>	<no value>
EOF_TASKS
)
export FAKE_TOOLS
FAKE_TOOLS=$(cat <<EOF_TOOLS
$project	$build_old	tool-old	2020-01-01T00:00:00Z	<no value>
$project	$build_active	tool-active	2020-01-01T00:00:00Z	<no value>
$project	$build_old	tool-new	2099-01-01T00:00:00Z	<no value>
EOF_TOOLS
)

"$script" --namespace orka-system status > "$tmp/status.txt"
grep -Fq $'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t3\t1\t2\t0\t0\t0\t3' "$tmp/status.txt"
grep -Fq $'<unlabeled>\t1\t0\t0\t1\t0\t0\t0' "$tmp/status.txt"
grep -Fq "$build_active" "$tmp/status.txt"
grep -Fq $'<none>\tpattern\t1' "$tmp/status.txt"

FAKE_TOOLS='' FAKE_TASKS="$project	$build_active	analysis	task-pending	<none>	2020-01-01T00:00:00Z	<no value>	<no value>
" \
  "$script" --namespace orka-system status > "$tmp/status-tasks-only.txt"
grep -Fq "$project" "$tmp/status-tasks-only.txt"
grep -Fq $'1\t1\t0\t0\t0\t0\t0' "$tmp/status-tasks-only.txt"

: > "$CALLS"
"$script" --namespace orka-system gc --project "$project" --older-than 7d > "$tmp/gc-dry-run.txt"
grep -Fq 'Tasks eligible: 1' "$tmp/gc-dry-run.txt"
grep -Fq 'Task task-old phase=Succeeded' "$tmp/gc-dry-run.txt"
grep -Fq 'Tools eligible: 1' "$tmp/gc-dry-run.txt"
grep -Fq 'Tool tool-old' "$tmp/gc-dry-run.txt"
grep -Fq 'Dry-run only.' "$tmp/gc-dry-run.txt"
if grep -Fq ' delete ' "$CALLS"; then
  echo 'GC dry-run deleted a resource' >&2
  exit 1
fi

"$script" --namespace orka-system gc --project "$project" --older-than 09m \
  > "$tmp/gc-leading-zero.txt"
grep -Fq 'Retention: older than 09m' "$tmp/gc-leading-zero.txt"

if "$script" --namespace orka-system gc --project "$project" --older-than 7d --delete \
  > "$tmp/gc-delete-option.txt" 2>&1; then
  echo 'GC accepted the removed --delete option' >&2
  exit 1
fi
grep -Fq 'unknown gc option: --delete' "$tmp/gc-delete-option.txt"

if "$script" gc --project not-a-scope > "$tmp/gc-invalid.txt" 2>&1; then
  echo 'GC accepted an invalid project scope' >&2
  exit 1
fi
grep -Fq '32-character project scope' "$tmp/gc-invalid.txt"

echo 'Orka operation helper checks passed.'

#!/usr/bin/env bash
set -euo pipefail

managed_by_label="app.kubernetes.io/managed-by"
managed_by_value="orka-producer"
project_label="orka.dashboard/project"
smoke_managed_by="prow-ai-dashboard-orka-ops"

usage() {
  cat <<'USAGE'
Usage:
  orka-ops.sh [--context <name>] [--namespace <name>] preflight \
    --provider <name> [--worker-image <image>] [--service-account <namespace/name>]
  orka-ops.sh [--context <name>] [--namespace <name>] smoke \
    --provider <name> [--model <name>] [--timeout <duration>] [--keep]
  orka-ops.sh [--context <name>] [--namespace <name>] status \
    [--project <scope>]
  orka-ops.sh [--context <name>] [--namespace <name>] gc \
    --project <scope> [--older-than <duration>]

Commands:
  preflight  Check Orka CRDs, controller readiness, API endpoints, Provider
             readiness, compatibility worker configuration, and optional
             dashboard ServiceAccount permissions.
  smoke      Create a disposable AI Task and require a successful result.
  status     Report Task phases and Tool counts by project and build batch.
  gc         Preview terminal Tasks and inactive Tools older than the retention
             window. This command never deletes resources.

Durations use one base-10 integer and unit, for example 30m, 24h, or 7d.
Leading zeros remain decimal, so 08h is the same as 8h.
USAGE
}

namespace="orka-system"
kube_context=""
kubectl_bin=${KUBECTL:-kubectl}
date_bin=${DATE:-date}

while [[ $# -gt 0 ]]; do
  case $1 in
    --context)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      kube_context=$2
      shift 2
      ;;
    --namespace)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      namespace=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    preflight|smoke|status|gc)
      command=$1
      shift
      break
      ;;
    *)
      echo "unknown option or command: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z ${command:-} ]]; then
  usage >&2
  exit 2
fi

kube() {
  local args=()
  if [[ -n $kube_context ]]; then
    args+=(--context "$kube_context")
  fi
  "$kubectl_bin" "${args[@]}" "$@"
}

smoke_cleanup_task=""
smoke_cleanup_keep=false

cleanup_smoke() {
  if [[ -n $smoke_cleanup_task && $smoke_cleanup_keep != true ]]; then
    kube -n "$namespace" delete task.core.orka.ai "$smoke_cleanup_task" --wait=false >/dev/null 2>&1 || true
  fi
}

parse_duration_seconds() {
  local value=$1 number unit
  if [[ $value =~ ^([0-9]+)([smhd])$ ]]; then
    number=$((10#${BASH_REMATCH[1]}))
    unit=${BASH_REMATCH[2]}
  else
    echo "invalid duration $value: use one integer unit such as 30m, 24h, or 7d" >&2
    return 1
  fi
  case $unit in
    s) printf '%d\n' "$number" ;;
    m) printf '%d\n' "$((number * 60))" ;;
    h) printf '%d\n' "$((number * 60 * 60))" ;;
    d) printf '%d\n' "$((number * 60 * 60 * 24))" ;;
  esac
}

normalize_label() {
  local value=$1
  if [[ -z $value || $value == "<no value>" ]]; then
    printf '%s' "<unlabeled>"
  else
    printf '%s' "$value"
  fi
}

inventory() {
  local selector=$1 task_file=$2 tool_file=$3
  local task_template tool_template
  task_template='{{range .items}}'
  task_template+='{{index .metadata.labels "orka.dashboard/project"}}{{"\t"}}'
  task_template+='{{index .metadata.labels "orka.dashboard/build"}}{{"\t"}}'
  task_template+='{{index .metadata.labels "orka.dashboard/task-type"}}{{"\t"}}'
  task_template+='{{.metadata.name}}{{"\t"}}'
  task_template+='{{if .status.phase}}{{.status.phase}}{{else}}<none>{{end}}{{"\t"}}'
  task_template+='{{.metadata.creationTimestamp}}{{"\t"}}'
  task_template+='{{.status.completionTime}}{{"\t"}}'
  task_template+='{{.metadata.deletionTimestamp}}{{"\n"}}{{end}}'
  tool_template='{{range .items}}'
  tool_template+='{{index .metadata.labels "orka.dashboard/project"}}{{"\t"}}'
  tool_template+='{{index .metadata.labels "orka.dashboard/build"}}{{"\t"}}'
  tool_template+='{{.metadata.name}}{{"\t"}}'
  tool_template+='{{.metadata.creationTimestamp}}{{"\t"}}'
  tool_template+='{{.metadata.deletionTimestamp}}{{"\n"}}{{end}}'
  kube -n "$namespace" get tasks.core.orka.ai -l "$selector" -o "go-template=$task_template" > "$task_file"
  kube -n "$namespace" get tools.core.orka.ai -l "$selector" -o "go-template=$tool_template" > "$tool_file"
}

preflight() {
  local provider="" expected_worker="" service_account=""
  while [[ $# -gt 0 ]]; do
    case $1 in
      --provider)
        [[ $# -ge 2 ]] || { usage >&2; exit 2; }
        provider=$2
        shift 2
        ;;
      --worker-image)
        [[ $# -ge 2 ]] || { usage >&2; exit 2; }
        expected_worker=$2
        shift 2
        ;;
      --service-account)
        [[ $# -ge 2 ]] || { usage >&2; exit 2; }
        service_account=$2
        shift 2
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        echo "unknown preflight option: $1" >&2
        exit 2
        ;;
    esac
  done
  if [[ -z $provider ]]; then
    echo "preflight requires --provider" >&2
    exit 2
  fi
  if [[ -n $service_account && $service_account != */* ]]; then
    echo "service account must be namespace/name" >&2
    exit 2
  fi

  local failures=0
  pass() { printf 'PASS  %s\n' "$1"; }
  fail() { printf 'FAIL  %s\n' "$1" >&2; failures=$((failures + 1)); }

  if kube get namespace "$namespace" -o name >/dev/null 2>&1; then
    pass "namespace $namespace exists"
  else
    fail "namespace $namespace is not readable"
  fi

  local crd established
  for crd in tasks.core.orka.ai tools.core.orka.ai providers.core.orka.ai; do
    established=$(kube get crd "$crd" \
      -o jsonpath='{range .status.conditions[?(@.type=="Established")]}{.status}{end}' 2>/dev/null || true)
    if [[ $established == "True" ]]; then
      pass "CRD $crd is established"
    elif [[ -z $established ]]; then
      fail "CRD $crd is missing or unreadable"
    else
      fail "CRD $crd is not established"
    fi
  done

  local controllers controller_ready=false controller_rows controller_selectors
  local worker_image controller_args
  controller_rows=$(kube -n "$namespace" get deployments -l app.kubernetes.io/component=controller \
    -o go-template='{{range .items}}{{.metadata.name}}{{"\t"}}{{.spec.replicas}}{{"\t"}}{{.status.readyReplicas}}{{"\t"}}{{.status.availableReplicas}}{{"\t"}}{{index .metadata.labels "app.kubernetes.io/instance"}}{{"\t"}}{{index .metadata.labels "app.kubernetes.io/name"}}{{"\t"}}{{range .spec.template.spec.containers}}{{range .args}}{{.}}{{","}}{{end}}{{end}}{{"\n"}}{{end}}' 2>/dev/null || true)
  controllers=0
  controller_selectors=""
  while IFS=$'\t' read -r name desired ready available instance app_name controller_args; do
    [[ -z $name ]] && continue
    if [[ $controller_args != *"--api-port="* || $controller_args != *"--ai-worker-image="* ]]; then
      continue
    fi
    controllers=$((controllers + 1))
    [[ $desired =~ ^[0-9]+$ ]] || desired=0
    [[ $ready =~ ^[0-9]+$ ]] || ready=0
    [[ $available =~ ^[0-9]+$ ]] || available=0
    if [[ -n $instance && $instance != "<no value>" && -n $app_name && $app_name != "<no value>" ]]; then
      controller_selectors+="${controller_selectors:+$'\n'}$instance"$'\t'"$app_name"
    else
      fail "controller Deployment $name is missing release identity labels"
    fi
    if (( desired > 0 && ready == desired && available > 0 )); then
      pass "controller Deployment $name is ready ($ready/$desired)"
      controller_ready=true
    else
      fail "controller Deployment $name is not ready ($ready/$desired, available=$available)"
    fi
    worker_image=$(tr ',' '\n' <<< "$controller_args" | sed -n 's/^--ai-worker-image=//p' | head -1)
    if [[ -z $worker_image ]]; then
      fail "controller Deployment $name does not expose --ai-worker-image"
    elif [[ -n $expected_worker && $worker_image != "$expected_worker" ]]; then
      fail "AI worker image is $worker_image, expected $expected_worker"
    else
      pass "AI worker image is $worker_image"
    fi
  done <<< "$controller_rows"
  if (( controllers == 0 )); then
    fail "no Orka controller Deployment was found in $namespace"
  elif [[ $controller_ready != true ]]; then
    fail "no Orka controller Deployment is available"
  fi

  local service_rows services="" service_matches service endpoint_addresses instance app_name
  while IFS=$'\t' read -r instance app_name; do
    [[ -z $instance || -z $app_name ]] && continue
    service_rows=$(kube -n "$namespace" get services \
      -l "app.kubernetes.io/instance=$instance,app.kubernetes.io/name=$app_name" \
      -o go-template='{{range .items}}{{.metadata.name}}{{"\t"}}{{range .spec.ports}}{{.name}}{{","}}{{end}}{{"\n"}}{{end}}' 2>/dev/null || true)
    service_matches=$(awk -F '\t' '{count = split($2, ports, ","); for (i = 1; i <= count; i++) if (ports[i] == "api") print $1}' <<< "$service_rows")
    if [[ -n $service_matches ]]; then
      services+="${services:+$'\n'}$service_matches"
    fi
  done <<< "$(printf '%s\n' "$controller_selectors" | sort -u)"
  services=$(printf '%s\n' "$services" | sed '/^$/d' | sort -u)
  if [[ -z $services ]]; then
    fail "no Orka controller Service exposes the API port"
  else
    while IFS= read -r service; do
      [[ -z $service ]] && continue
      endpoint_addresses=$(kube -n "$namespace" get endpoints "$service" \
        -o jsonpath='{range .subsets[*].addresses[*]}{.ip}{"\n"}{end}' 2>/dev/null || true)
      if [[ -n $endpoint_addresses ]]; then
        pass "API Service $service has ready endpoints"
      else
        fail "API Service $service has no ready endpoints"
      fi
    done <<< "$services"
  fi

  local provider_state provider_ready provider_message
  provider_state=$(kube -n "$namespace" get provider.core.orka.ai "$provider" \
    -o jsonpath='{.status.ready}{"|"}{.status.message}' 2>/dev/null || true)
  provider_ready=${provider_state%%|*}
  provider_message=${provider_state#*|}
  if [[ $provider_ready == "true" ]]; then
    pass "Provider $provider is ready"
  elif [[ -z $provider_state ]]; then
    fail "Provider $provider is missing or unreadable"
  else
    fail "Provider $provider is not ready${provider_message:+: $provider_message}"
  fi

  if [[ -n $service_account ]]; then
    local sa_namespace=${service_account%%/*}
    local sa_name=${service_account#*/}
    if ! kube -n "$sa_namespace" get serviceaccount "$sa_name" -o name >/dev/null 2>&1; then
      fail "ServiceAccount $service_account is missing or unreadable"
    else
      pass "ServiceAccount $service_account exists"
      local identity="system:serviceaccount:$sa_namespace:$sa_name"
      local resource verb allowed
      for resource in tasks.core.orka.ai tools.core.orka.ai; do
        for verb in create get list watch patch update delete; do
          allowed=$(kube auth can-i "$verb" "$resource" -n "$namespace" --as="$identity" 2>/dev/null || true)
          if [[ $allowed == "yes" ]]; then
            pass "$service_account can $verb $resource in $namespace"
          else
            fail "$service_account cannot $verb $resource in $namespace"
          fi
        done
      done
    fi
  fi

  if (( failures > 0 )); then
    printf '\nPreflight failed with %d check(s).\n' "$failures" >&2
    return 1
  fi
  printf '\nPreflight passed.\n'
}

smoke() {
  local provider="" model="" timeout="5m" keep=false
  while [[ $# -gt 0 ]]; do
    case $1 in
      --provider)
        [[ $# -ge 2 ]] || { usage >&2; exit 2; }
        provider=$2
        shift 2
        ;;
      --model)
        [[ $# -ge 2 ]] || { usage >&2; exit 2; }
        model=$2
        shift 2
        ;;
      --timeout)
        [[ $# -ge 2 ]] || { usage >&2; exit 2; }
        timeout=$2
        shift 2
        ;;
      --keep)
        keep=true
        shift
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        echo "unknown smoke option: $1" >&2
        exit 2
        ;;
    esac
  done
  if [[ -z $provider ]]; then
    echo "smoke requires --provider" >&2
    exit 2
  fi
  local timeout_seconds
  timeout_seconds=$(parse_duration_seconds "$timeout")

  local provider_ready
  provider_ready=$(kube -n "$namespace" get provider.core.orka.ai "$provider" \
    -o jsonpath='{.status.ready}' 2>/dev/null || true)
  if [[ $provider_ready != "true" ]]; then
    echo "Provider $namespace/$provider is not ready" >&2
    return 1
  fi

  local suffix name expected model_yaml random_token create_error
  random_token=${ORKA_OPS_RANDOM_TOKEN:-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')}
  if [[ ! $random_token =~ ^[0-9a-f]{16}$ ]]; then
    echo "could not generate a valid smoke Task operation token" >&2
    return 1
  fi
  suffix="$($date_bin -u +%Y%m%d%H%M%S)-$random_token"
  name="prow-ai-dashboard-smoke-$suffix"
  expected="PROW_AI_DASHBOARD_ORKA_OK_$suffix"
  model_yaml=""
  if [[ -n $model ]]; then
    model_yaml="    model: \"$model\""
  fi

  smoke_cleanup_task=$name
  smoke_cleanup_keep=false
  trap cleanup_smoke EXIT
  if ! create_error=$(cat <<EOF_TASK | kube -n "$namespace" create -f - 2>&1 >/dev/null
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: $name
  labels:
    $managed_by_label: $smoke_managed_by
spec:
  type: ai
  timeout: $timeout
  ai:
    providerRef:
      name: $provider
$model_yaml
    prompt: "Reply with exactly: $expected"
EOF_TASK
  ); then
    if grep -Eqi 'alreadyexists|already exists' <<< "$create_error"; then
      smoke_cleanup_task=""
      trap - EXIT
      echo "Smoke Task name collision for $namespace/$name; the existing Task was not deleted" >&2
      return 1
    fi
    [[ -n $create_error ]] && printf '%s\n' "$create_error" >&2
    echo "Creating smoke Task $namespace/$name failed; cleanup was attempted" >&2
    return 1
  fi
  smoke_cleanup_keep=$keep

  printf 'Created smoke Task %s/%s\n' "$namespace" "$name"
  local deadline=$((SECONDS + timeout_seconds))
  local poll_seconds=${ORKA_OPS_POLL_SECONDS:-5}
  local state phase available message job_name
  while true; do
    state=$(kube -n "$namespace" get task.core.orka.ai "$name" \
      -o jsonpath='{.status.phase}{"|"}{.status.resultRef.available}{"|"}{.status.message}{"|"}{.status.jobName}' 2>/dev/null || true)
    IFS='|' read -r phase available message job_name <<< "$state"
    printf 'Task %s: %s\n' "$name" "${phase:-Pending}"
    case $phase in
      Succeeded)
        if [[ $available != "true" ]]; then
          echo "Task succeeded without an available result" >&2
          return 1
        fi
        printf 'Smoke Task succeeded with an available result.\n'
        if [[ $keep == true ]]; then
          printf 'Kept Task %s/%s\n' "$namespace" "$name"
          smoke_cleanup_task=""
        else
          if ! kube -n "$namespace" delete task.core.orka.ai "$name" --wait=false >/dev/null; then
            echo "Smoke Task succeeded, but deleting $namespace/$name failed" >&2
            return 1
          fi
          smoke_cleanup_task=""
          printf 'Deleted smoke Task %s/%s\n' "$namespace" "$name"
        fi
        trap - EXIT
        return 0
        ;;
      Failed|Cancelled)
        echo "Smoke Task $phase${message:+: $message}" >&2
        kube -n "$namespace" describe task.core.orka.ai "$name" >&2 || true
        if [[ -n $job_name ]]; then
          kube -n "$namespace" logs "job/$job_name" --all-containers=true --prefix=true >&2 || true
        fi
        return 1
        ;;
    esac
    if (( SECONDS >= deadline )); then
      echo "Smoke Task did not finish within $timeout" >&2
      kube -n "$namespace" describe task.core.orka.ai "$name" >&2 || true
      return 1
    fi
    sleep "$poll_seconds"
  done
}

status_report() {
  local project=""
  while [[ $# -gt 0 ]]; do
    case $1 in
      --project)
        [[ $# -ge 2 ]] || { usage >&2; exit 2; }
        project=$2
        shift 2
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        echo "unknown status option: $1" >&2
        exit 2
        ;;
    esac
  done

  local selector="$managed_by_label=$managed_by_value"
  if [[ -n $project ]]; then
    selector+=",$project_label=$project"
  fi
  local task_file tool_file
  tmp=$(mktemp -d)
  task_file="$tmp/tasks.tsv"
  tool_file="$tmp/tools.tsv"
  trap 'rm -rf "$tmp"' EXIT
  inventory "$selector" "$task_file" "$tool_file"

  if [[ ! -s $task_file && ! -s $tool_file ]]; then
    echo "No dashboard-managed Orka Tasks or Tools matched $selector."
    return 0
  fi

  printf 'PROJECT\tTASKS\tACTIVE\tSUCCEEDED\tFAILED\tCANCELLED\tUNKNOWN\tTOOLS\n'
  awk -F '\t' '
    FILENAME == ARGV[1] {
      p = ($1 == "" || $1 == "<no value>") ? "<unlabeled>" : $1
      ptools[p]++
      next
    }
    {
      p = ($1 == "" || $1 == "<no value>") ? "<unlabeled>" : $1
      phase = $5
      tasks[p]++
      if (phase == "Succeeded") succeeded[p]++
      else if (phase == "Failed") failed[p]++
      else if (phase == "Cancelled") cancelled[p]++
      else if (phase == "Pending" || phase == "Scheduled" || phase == "Running" || phase == "" || phase == "<none>") active[p]++
      else unknown[p]++
      projects[p] = 1
    }
    END {
      for (p in ptools) projects[p] = 1
      for (p in projects)
        printf "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", p, tasks[p], active[p], succeeded[p], failed[p], cancelled[p], unknown[p], ptools[p]
    }
  ' "$tool_file" "$task_file" | sort

  printf '\nPROJECT\tBATCH\tTYPE\tTASKS\tACTIVE\tSUCCEEDED\tFAILED\tCANCELLED\tUNKNOWN\tTOOLS\n'
  awk -F '\t' '
    FILENAME == ARGV[1] {
      p = ($1 == "" || $1 == "<no value>") ? "<unlabeled>" : $1
      b = ($2 == "" || $2 == "<no value>") ? "<none>" : $2
      tools[p SUBSEP b]++
      keys[p SUBSEP b] = 1
      next
    }
    {
      p = ($1 == "" || $1 == "<no value>") ? "<unlabeled>" : $1
      b = ($2 == "" || $2 == "<no value>") ? "<none>" : $2
      type = ($3 == "" || $3 == "<no value>") ? ((b == "<none>") ? "pattern" : "analysis") : $3
      key = p SUBSEP b
      types[key] = type
      tasks[key]++
      phase = $5
      if (phase == "Succeeded") succeeded[key]++
      else if (phase == "Failed") failed[key]++
      else if (phase == "Cancelled") cancelled[key]++
      else if (phase == "Pending" || phase == "Scheduled" || phase == "Running" || phase == "" || phase == "<none>") active[key]++
      else unknown[key]++
      keys[key] = 1
    }
    END {
      for (key in keys) {
        split(key, parts, SUBSEP)
        type = types[key]
        if (type == "") type = "tools"
        printf "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", parts[1], parts[2], type, tasks[key], active[key], succeeded[key], failed[key], cancelled[key], unknown[key], tools[key]
      }
    }
  ' "$tool_file" "$task_file" | sort
}

timestamp_epoch() {
  local timestamp=$1 normalized
  normalized=${timestamp%%.*}
  normalized=${normalized%Z}Z
  if "$date_bin" -u -d "$normalized" +%s >/dev/null 2>&1; then
    "$date_bin" -u -d "$normalized" +%s
  else
    "$date_bin" -j -u -f '%Y-%m-%dT%H:%M:%SZ' "$normalized" +%s
  fi
}

gc_resources() {
  local project="" older_than="168h"
  while [[ $# -gt 0 ]]; do
    case $1 in
      --project)
        [[ $# -ge 2 ]] || { usage >&2; exit 2; }
        project=$2
        shift 2
        ;;
      --older-than)
        [[ $# -ge 2 ]] || { usage >&2; exit 2; }
        older_than=$2
        shift 2
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        echo "unknown gc option: $1" >&2
        exit 2
        ;;
    esac
  done
  if [[ ! $project =~ ^[0-9a-f]{32}$ ]]; then
    echo "gc requires --project with the 32-character project scope from the status command" >&2
    exit 2
  fi

  local retention_seconds now cutoff
  retention_seconds=$(parse_duration_seconds "$older_than")
  now=$($date_bin -u +%s)
  cutoff=$((now - retention_seconds))

  local selector="$managed_by_label=$managed_by_value,$project_label=$project"
  local task_file tool_file active_builds task_candidates tool_candidates
  tmp=$(mktemp -d)
  task_file="$tmp/tasks.tsv"
  tool_file="$tmp/tools.tsv"
  active_builds="$tmp/active-builds.txt"
  task_candidates="$tmp/task-candidates.tsv"
  tool_candidates="$tmp/tool-candidates.tsv"
  trap 'rm -rf "$tmp"' EXIT
  inventory "$selector" "$task_file" "$tool_file"
  : > "$active_builds"
  : > "$task_candidates"
  : > "$tool_candidates"

  local _project build task_type name phase created completed deleting age_timestamp created_epoch
  while IFS=$'\t' read -r _project build task_type name phase created completed deleting; do
    [[ -z $name ]] && continue
    if [[ $phase != "Succeeded" && $phase != "Failed" && $phase != "Cancelled" ]]; then
      if [[ -n $build && $build != "<no value>" ]]; then
        printf '%s\n' "$build" >> "$active_builds"
      fi
      continue
    fi
    [[ -n $deleting && $deleting != "<no value>" ]] && continue
    age_timestamp=$completed
    if [[ -z $age_timestamp || $age_timestamp == "<no value>" ]]; then
      age_timestamp=$created
    fi
    if ! created_epoch=$(timestamp_epoch "$age_timestamp" 2>/dev/null); then
      echo "Skipping Task $name with unreadable completion timestamp $age_timestamp" >&2
      continue
    fi
    if (( created_epoch <= cutoff )); then
      printf '%s\t%s\t%s\t%s\n' "$name" "$phase" "$age_timestamp" "$(normalize_label "$task_type")" >> "$task_candidates"
    fi
  done < "$task_file"
  sort -u -o "$active_builds" "$active_builds"

  local tool_name
  while IFS=$'\t' read -r _project build tool_name created deleting; do
    [[ -z $tool_name ]] && continue
    [[ -n $deleting && $deleting != "<no value>" ]] && continue
    if [[ -z $build || $build == "<no value>" ]]; then
      echo "Skipping Tool $tool_name without a build scope" >&2
      continue
    fi
    if grep -Fxq "$build" "$active_builds"; then
      continue
    fi
    if ! created_epoch=$(timestamp_epoch "$created" 2>/dev/null); then
      echo "Skipping Tool $tool_name with unreadable creation timestamp $created" >&2
      continue
    fi
    if (( created_epoch <= cutoff )); then
      printf '%s\t%s\t%s\n' "$tool_name" "$build" "$created" >> "$tool_candidates"
    fi
  done < "$tool_file"

  local task_count tool_count
  task_count=$(wc -l < "$task_candidates" | tr -d ' ')
  tool_count=$(wc -l < "$tool_candidates" | tr -d ' ')
  printf 'GC scope: %s\n' "$project"
  printf 'Retention: older than %s\n' "$older_than"
  printf 'Tasks eligible: %d\n' "$task_count"
  if [[ -s $task_candidates ]]; then
    awk -F '\t' '{printf "  Task %s phase=%s completed=%s type=%s\n", $1, $2, $3, $4}' "$task_candidates"
  fi
  printf 'Tools eligible: %d\n' "$tool_count"
  if [[ -s $tool_candidates ]]; then
    awk -F '\t' '{printf "  Tool %s build=%s created=%s\n", $1, $2, $3}' "$tool_candidates"
  fi

  printf 'Dry-run only. This command never deletes resources.\n'

}

case $command in
  preflight) preflight "$@" ;;
  smoke) smoke "$@" ;;
  status) status_report "$@" ;;
  gc) gc_resources "$@" ;;
esac

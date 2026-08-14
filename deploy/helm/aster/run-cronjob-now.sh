#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: run-cronjob-now.sh [--wait] [--timeout <duration>] [--keep-on-timeout] <namespace> <cronjob>

Create a uniquely named Job from a suspended dashboard CronJob. The command
checks for active scheduled and manual Jobs before creating another one. This is
a preflight check, not a distributed lock; do not invoke the helper concurrently.

Timeout uses one base-10 integer and unit, for example 90m, 2h, or 300s.
Leading zeros remain decimal, so 08h is the same as 8h. A timed-out Job is
deleted by default. Use --keep-on-timeout to leave it running.
USAGE
}

parse_duration_seconds() {
  local value=$1 number unit
  if [[ $value =~ ^([0-9]+)([smh])$ ]]; then
    number=$((10#${BASH_REMATCH[1]}))
    unit=${BASH_REMATCH[2]}
  else
    echo "invalid timeout $value: use one integer unit such as 90m, 2h, or 300s" >&2
    return 1
  fi
  case $unit in
    s) printf '%d\n' "$number" ;;
    m) printf '%d\n' "$((number * 60))" ;;
    h) printf '%d\n' "$((number * 60 * 60))" ;;
  esac
}

wait_for_job=false
keep_on_timeout=false
timeout=90m
while [[ $# -gt 0 ]]; do
  case $1 in
    --wait)
      wait_for_job=true
      shift
      ;;
    --timeout)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      timeout=$2
      shift 2
      ;;
    --keep-on-timeout)
      keep_on_timeout=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      break
      ;;
    -*)
      echo "unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
    *)
      break
      ;;
  esac
done
if [[ $# -ne 2 ]]; then
  usage >&2
  exit 2
fi

namespace=$1
cronjob=$2
timeout_seconds=0
if [[ $wait_for_job == true ]]; then
  timeout_seconds=$(parse_duration_seconds "$timeout")
fi
kubectl_bin=${KUBECTL:-kubectl}
suspended=$($kubectl_bin -n "$namespace" get cronjob "$cronjob" -o jsonpath='{.spec.suspend}')
if [[ $suspended != "true" ]]; then
  echo "CronJob $namespace/$cronjob is not suspended; set fetcher.suspend=true before a manual evaluation" >&2
  exit 1
fi
active=$($kubectl_bin -n "$namespace" get cronjob "$cronjob" -o jsonpath='{range .status.active[*]}{.name}{"\n"}{end}')
if [[ -n $active ]]; then
  echo "CronJob $namespace/$cronjob already has an active Job: $active" >&2
  exit 1
fi
label_key=prow-ai-dashboard.willieyao.dev/source-cronjob
manual_jobs=$($kubectl_bin -n "$namespace" get jobs -l "$label_key=$cronjob" -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .status.conditions[*]}{.type}{"="}{.status}{","}{end}{"\n"}{end}')
manual_active=""
while IFS=$'\t' read -r name conditions; do
  [[ -z $name ]] && continue
  if [[ $conditions != *"Complete=True"* && $conditions != *"Failed=True"* ]]; then
    manual_active+="${manual_active:+, }$name"
  fi
done <<< "$manual_jobs"
if [[ -n $manual_active ]]; then
  echo "CronJob $namespace/$cronjob already has an active manual Job: $manual_active" >&2
  exit 1
fi

suffix=$(date -u +%Y%m%d%H%M%S)
max_prefix=$((63 - 1 - ${#suffix}))
prefix=${cronjob:0:max_prefix}
prefix=${prefix%-}
job_name="$prefix-$suffix"

job_manifest=$($kubectl_bin -n "$namespace" create job --from="cronjob/$cronjob" "$job_name" --dry-run=client -o yaml)
labeled_manifest=$(printf '%s\n' "$job_manifest" | $kubectl_bin label --local -f - "$label_key=$cronjob" -o yaml)
printf '%s\n' "$labeled_manifest" | $kubectl_bin -n "$namespace" create -f -

echo "Created Job $namespace/$job_name"
echo "Follow: kubectl -n $namespace logs -f job/$job_name --all-containers=true --prefix=true"
echo "Status: kubectl -n $namespace get job/$job_name"

if [[ $wait_for_job == true ]]; then
  deadline=$((SECONDS + timeout_seconds))
  while true; do
    conditions=$($kubectl_bin -n "$namespace" get "job/$job_name" -o jsonpath='{range .status.conditions[*]}{.type}{"="}{.status}{"\n"}{end}')
    if [[ $conditions == *"Complete=True"* ]]; then
      $kubectl_bin -n "$namespace" logs "job/$job_name" --all-containers=true --prefix=true
      break
    fi
    if [[ $conditions == *"Failed=True"* ]]; then
      echo "Job $namespace/$job_name failed" >&2
      $kubectl_bin -n "$namespace" describe "job/$job_name" >&2 || true
      $kubectl_bin -n "$namespace" logs "job/$job_name" --all-containers=true --prefix=true >&2 || true
      exit 1
    fi
    if (( SECONDS >= deadline )); then
      echo "Job $namespace/$job_name did not finish within $timeout" >&2
      $kubectl_bin -n "$namespace" describe "job/$job_name" >&2 || true
      if [[ $keep_on_timeout == true ]]; then
        echo "Leaving timed-out Job $namespace/$job_name running" >&2
      else
        echo "Deleting timed-out Job $namespace/$job_name" >&2
        $kubectl_bin -n "$namespace" delete "job/$job_name" --wait=true >&2 || true
      fi
      exit 1
    fi
    sleep 5
  done
fi

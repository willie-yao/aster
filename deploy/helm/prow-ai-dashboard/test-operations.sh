#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
chart="$root/deploy/helm/prow-ai-dashboard"
tmp="${TMPDIR:-/tmp}/prow-ai-dashboard-operations-$$"
mkdir -p "$tmp"
trap 'rm -rf "$tmp"' EXIT

bash -n "$chart/create-fresh-pvc.sh"
bash -n "$chart/run-cronjob-now.sh"

cat > "$tmp/kubectl-pvc" <<'EOF_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *'.spec.resources.requests.storage'*) printf '5Gi' ;;
  *'go-template='*'storageClassName'*) printf '%s' "${FAKE_STORAGE_CLASS_STATE:-present=azurefile-csi-premium}" ;;
  *'.spec.volumeMode'*) printf 'Filesystem' ;;
  *'.spec.accessModes'*) printf 'ReadWriteMany\n' ;;
  *) echo "unexpected kubectl invocation: $*" >&2; exit 1 ;;
esac
EOF_KUBECTL
chmod +x "$tmp/kubectl-pvc"

KUBECTL="$tmp/kubectl-pvc" "$chart/create-fresh-pvc.sh" dashboards old-data new-data > "$tmp/pvc.yaml"
grep -Fq 'name: new-data' "$tmp/pvc.yaml"
grep -Fq 'namespace: dashboards' "$tmp/pvc.yaml"
grep -Fq -- '- ReadWriteMany' "$tmp/pvc.yaml"
grep -Fq 'storage: 5Gi' "$tmp/pvc.yaml"
grep -Fq 'storageClassName: "azurefile-csi-premium"' "$tmp/pvc.yaml"
grep -Fq 'volumeMode: Filesystem' "$tmp/pvc.yaml"

FAKE_STORAGE_CLASS_STATE=present= KUBECTL="$tmp/kubectl-pvc" \
  "$chart/create-fresh-pvc.sh" dashboards old-data classless-data > "$tmp/pvc-classless.yaml"
grep -Fq 'storageClassName: ""' "$tmp/pvc-classless.yaml"

FAKE_STORAGE_CLASS_STATE=absent KUBECTL="$tmp/kubectl-pvc" \
  "$chart/create-fresh-pvc.sh" dashboards old-data defaulted-data > "$tmp/pvc-defaulted.yaml"
if grep -Fq 'storageClassName:' "$tmp/pvc-defaulted.yaml"; then
  echo 'source PVC without storageClassName rendered the field' >&2
  exit 1
fi

cat > "$tmp/kubectl-job" <<EOF_KUBECTL
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$*" >> "$tmp/job-calls"
if [[ "\$*" == *' get cronjob '* && "\$*" == *'.spec.suspend'* ]]; then
  printf '%s' "\${FAKE_SUSPEND:-true}"
  exit 0
fi
if [[ "\$*" == *' get cronjob '* ]]; then
  printf '%s' "\${FAKE_SCHEDULED_JOBS:-}"
  exit 0
fi
if [[ "\$*" == *' get jobs '* ]]; then
  printf '%s' "\${FAKE_MANUAL_JOBS:-}"
  exit 0
fi
if [[ "\$*" == *' get job/'* ]]; then
  printf '%s' "\${FAKE_JOB_CONDITIONS:-Complete=True}"
  exit 0
fi
if [[ "\$*" == *' create job '* && "\$*" == *' --dry-run=client '* ]]; then
  cat <<'MANIFEST'
apiVersion: batch/v1
kind: Job
metadata:
  name: test
MANIFEST
  exit 0
fi
if [[ "\$*" == *'label --local '* ]]; then
  cat
  exit 0
fi
if [[ "\$*" == *' create -f -'* ]]; then
  cat >/dev/null
  printf 'job.batch/test created\n'
  exit 0
fi
if [[ "\$*" == *' describe job/'* ]]; then
  printf 'job description\n'
  exit 0
fi
if [[ "\$*" == *' logs job/'* ]]; then
  printf 'job logs\n'
  exit 0
fi
echo "unexpected kubectl invocation: \$*" >&2
exit 1
EOF_KUBECTL
chmod +x "$tmp/kubectl-job"

long_cronjob=dashboard-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx-fetcher
KUBECTL="$tmp/kubectl-job" "$chart/run-cronjob-now.sh" dashboards "$long_cronjob" > "$tmp/run-now.txt"
job_name=$(sed -n 's/^Created Job dashboards\///p' "$tmp/run-now.txt")
if [[ -z $job_name || ${#job_name} -gt 63 ]]; then
  echo "generated Job name is empty or exceeds 63 characters: $job_name" >&2
  exit 1
fi
grep -Fq -- "--from=cronjob/$long_cronjob $job_name --dry-run=client -o yaml" "$tmp/job-calls"
grep -Fq "label --local -f - prow-ai-dashboard.willieyao.dev/source-cronjob=$long_cronjob -o yaml" "$tmp/job-calls"
grep -Fq 'Created Job dashboards/' "$tmp/run-now.txt"

KUBECTL="$tmp/kubectl-job" "$chart/run-cronjob-now.sh" --wait --timeout 5s \
  dashboards "$long_cronjob" > "$tmp/completed-job.txt"
grep -Fq 'job logs' "$tmp/completed-job.txt"

if FAKE_JOB_CONDITIONS=Failed=True KUBECTL="$tmp/kubectl-job" \
  "$chart/run-cronjob-now.sh" --wait --timeout 5s dashboards "$long_cronjob" \
  > "$tmp/failed-job.txt" 2>&1; then
  echo 'run helper accepted a failed Job' >&2
  exit 1
fi
grep -Fq 'failed' "$tmp/failed-job.txt"
grep -Fq 'job description' "$tmp/failed-job.txt"

create_calls_before=$(grep -Fc ' create job ' "$tmp/job-calls")
if KUBECTL="$tmp/kubectl-job" "$chart/run-cronjob-now.sh" --wait --timeout invalid \
  dashboards "$long_cronjob" > "$tmp/invalid-timeout.txt" 2>&1; then
  echo 'run helper accepted an invalid timeout' >&2
  exit 1
fi
create_calls_after=$(grep -Fc ' create job ' "$tmp/job-calls")
if [[ $create_calls_after -ne $create_calls_before ]]; then
  echo 'run helper created a Job before rejecting an invalid timeout' >&2
  exit 1
fi
grep -Fq 'invalid timeout' "$tmp/invalid-timeout.txt"

if FAKE_SUSPEND=false KUBECTL="$tmp/kubectl-job" \
  "$chart/run-cronjob-now.sh" dashboards "$long_cronjob" \
  > "$tmp/unsuspended.txt" 2>&1; then
  echo 'run helper accepted an unsuspended CronJob' >&2
  exit 1
fi
grep -Fq 'is not suspended' "$tmp/unsuspended.txt"

if FAKE_MANUAL_JOBS=$'pending-job\t' KUBECTL="$tmp/kubectl-job" \
  "$chart/run-cronjob-now.sh" dashboards "$long_cronjob" \
  > "$tmp/active-job.txt" 2>&1; then
  echo 'run helper accepted an active manual Job' >&2
  exit 1
fi
grep -Fq 'already has an active manual Job: pending-job' "$tmp/active-job.txt"

echo 'Helm operation helper checks passed.'

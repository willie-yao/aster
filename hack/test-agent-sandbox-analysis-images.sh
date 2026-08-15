#!/usr/bin/env bash
set -euo pipefail
executor=${1:?executor image required}
stager=${2:?stager image required}
for image in "$executor" "$stager"; do
  user=$(docker image inspect "$image" --format '{{.Config.User}}')
  [[ "$user" == "65532:65532" ]] || { echo "$image user=$user" >&2; exit 1; }
done
if docker run --rm "$executor" >/tmp/analysis-executor.out 2>/tmp/analysis-executor.err; then
  echo 'analysis executor accepted a missing request' >&2; exit 1
fi
grep -q 'PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64 is required' /tmp/analysis-executor.out
if docker run --rm "$stager" >/tmp/analysis-stager.out 2>/tmp/analysis-stager.err; then
  echo 'analysis stager accepted a missing request' >&2; exit 1
fi
grep -q 'PROW_AI_ANALYSIS_STAGE_REQUEST_B64 is required' /tmp/analysis-stager.err

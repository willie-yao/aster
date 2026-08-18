#!/usr/bin/env bash
set -euo pipefail
executor=${1:?executor image required}
stager=${2:?stager image required}
expected_commit=${3:-}
expected_image_tag=${4:-}
contract_output=${5:-}
for image in "$executor" "$stager"; do
  user=$(docker image inspect "$image" --format '{{.Config.User}}')
  [[ "$user" == "65532:65532" ]] || { echo "$image user=$user" >&2; exit 1; }
  if [[ -n "$expected_commit" ]]; then
    revision=$(docker image inspect "$image" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
    [[ "$revision" == "$expected_commit" ]] || { echo "$image revision=$revision" >&2; exit 1; }
  fi
  if [[ -n "$expected_image_tag" ]]; then
    image_tag=$(docker image inspect "$image" --format '{{index .Config.Labels "io.prow-ai-dashboard.image-tag"}}')
    [[ "$image_tag" == "$expected_image_tag" ]] || { echo "$image image-tag=$image_tag" >&2; exit 1; }
  fi
done
executor_version=$(docker run --rm "$executor" --version)
stager_version=$(docker run --rm "$stager" --version)
opencode_version=$(docker run --rm --entrypoint opencode "$executor" --version)
if [[ -n "$expected_commit" ]]; then
  [[ "$executor_version" == *"commit=$expected_commit"* ]] || { echo "executor version=$executor_version" >&2; exit 1; }
  [[ "$stager_version" == *"commit=$expected_commit"* ]] || { echo "stager version=$stager_version" >&2; exit 1; }
fi
if docker run --rm "$executor" >/tmp/analysis-executor.out 2>/tmp/analysis-executor.err; then
  echo 'analysis executor accepted a missing request' >&2; exit 1
fi
grep -q 'PROW_AI_ANALYSIS_EXECUTION_REQUEST_B64 is required' /tmp/analysis-executor.out
if docker run --rm "$stager" >/tmp/analysis-stager.out 2>/tmp/analysis-stager.err; then
  echo 'analysis stager accepted a missing request' >&2; exit 1
fi
grep -q 'PROW_AI_ANALYSIS_STAGE_REQUEST_B64 is required' /tmp/analysis-stager.err

if [[ -n "$contract_output" ]]; then
  [[ -n "$expected_commit" && -n "$expected_image_tag" ]] || { echo 'image contract requires expected commit and image tag' >&2; exit 1; }
  [[ "$executor" =~ @sha256:[0-9a-f]{64}$ && "$stager" =~ @sha256:[0-9a-f]{64}$ ]] || { echo 'image contract requires immutable digest references' >&2; exit 1; }
  executor_revision=$(docker image inspect "$executor" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
  stager_revision=$(docker image inspect "$stager" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
  executor_user=$(docker image inspect "$executor" --format '{{.Config.User}}')
  stager_user=$(docker image inspect "$stager" --format '{{.Config.User}}')
  mkdir -p "$(dirname "$contract_output")"
  python3 - "$contract_output" "$executor" "$stager" "$expected_commit" "$expected_image_tag" "$executor_revision" "$stager_revision" "$executor_user" "$stager_user" "$executor_version" "$stager_version" "$opencode_version" <<'PY'
import hashlib
import json
import os
from pathlib import Path
import sys

(output, executor, stager, commit, image_tag, executor_revision, stager_revision,
 executor_user, stager_user, executor_version, stager_version, opencode_version) = sys.argv[1:]
payload = {
    "version": 1,
    "engine_commit": commit,
    "image_tag": image_tag,
    "executor_image": executor,
    "stager_image": stager,
    "executor_revision": executor_revision,
    "stager_revision": stager_revision,
    "executor_user": executor_user,
    "stager_user": stager_user,
    "executor_version": executor_version,
    "stager_version": stager_version,
    "opencode_version": opencode_version,
}
encoded = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()
payload["contract_sha256"] = hashlib.sha256(encoded).hexdigest()
target = Path(output)
target.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")
os.chmod(target, 0o600)
PY
fi

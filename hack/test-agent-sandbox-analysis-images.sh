#!/usr/bin/env bash
set -euo pipefail
executor=${1:?executor image required}
stager=${2:?stager image required}
expected_commit=${3:-}
expected_image_tag=${4:-}
contract_output=${5:-}
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/aster-analysis-images.XXXXXX")
trap 'rm -rf "$tmpdir"' EXIT

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
docker run --rm --entrypoint cat "$executor" /usr/local/share/aster/opencode-runtime.json > "$tmpdir/opencode-runtime.json"
opencode_binary_sha256=$(docker run --rm --entrypoint sha256sum "$executor" /usr/local/bin/opencode | awk '{print $1}')
python3 - "$tmpdir/opencode-runtime.json" "$opencode_binary_sha256" <<'PY'
import json
from pathlib import Path
import sys

path, observed_binary_sha256 = sys.argv[1:]
manifest = json.loads(Path(path).read_text())
expected = {
    "version": 1,
    "upstream_version": "1.18.2",
    "upstream_revision": "70b56a0a93d366889cae950379cc9d2537148fa2",
    "source_archive_sha256": "13d277b405def808734be8ce4c6f68d3b40df866358556aefb48b5be90ea53c1",
    "models_dev_sha256": "2f6a5a4ab4d450e3ddabdbf0313e51bd76d51577ec1d7936326c484aded33b51",
    "web_builder_image": "docker.io/library/node:24-bookworm@sha256:934240a162082fd8b8a2f90cd5114446443f1eba1c5378f6687167ca405e6584",
    "web_builder_node_version": "v24.19.0",
    "web_builder_bun_image": "docker.io/oven/bun:1.3.14@sha256:e10577f0db68676a7024391c6e5cb4b879ebd17188ab750cf10024a6d700e5c4",
    "web_builder_bun_sha256": "a8f9ebd1770ddc8e55dab7a68d4ec1ec1eebf374bb97cc65cf2c3cb373fc6791",
    "builder_image": "docker.io/oven/bun:1.3.14-alpine@sha256:5acc90a93e91ff07bf72aa90a7c9f0fa189765aec90b47bdbf2152d2196383c0",
    "builder_bun_sha256": "500e6edbf321ddf490adcc55a6a01639993a07924616ab67492e1256c15557e2",
    "bun_version": "1.3.14",
    "embedded_web_ui": True,
    "patch_version": "aster-disable-project-instructions-v1",
    "patch_sha256": "48031f5d9a3c675406c43697682291efba78feb208c9f5dc2a977645aa41e6a3",
    "build_patch_version": "aster-single-target-build-v1",
    "build_patch_sha256": "49c2e7435fb59199df817b36bd7f8c7bfbac5622b86fbadef6a3ea3f1095605d",
}
for key, value in expected.items():
    if manifest.get(key) != value:
        raise SystemExit(f"OpenCode runtime {key}={manifest.get(key)!r}, want {value!r}")
if not isinstance(manifest.get("web_ui_sha256"), str) or len(manifest["web_ui_sha256"]) != 64:
    raise SystemExit("OpenCode Web UI digest is invalid")
if manifest.get("binary_sha256") != observed_binary_sha256:
    raise SystemExit("OpenCode runtime binary digest differs")
if set(manifest) != set(expected) | {"web_ui_sha256", "binary_sha256"}:
    raise SystemExit("OpenCode runtime manifest fields differ")
PY

for label in \
  "io.aster.opencode.upstream.version=1.18.2" \
  "io.aster.opencode.upstream.revision=70b56a0a93d366889cae950379cc9d2537148fa2" \
  "io.aster.opencode.source-archive.sha256=13d277b405def808734be8ce4c6f68d3b40df866358556aefb48b5be90ea53c1" \
  "io.aster.opencode.models-dev.sha256=2f6a5a4ab4d450e3ddabdbf0313e51bd76d51577ec1d7936326c484aded33b51" \
  "io.aster.opencode.patch.version=aster-disable-project-instructions-v1" \
  "io.aster.opencode.patch.sha256=48031f5d9a3c675406c43697682291efba78feb208c9f5dc2a977645aa41e6a3" \
  "io.aster.opencode.build-patch.version=aster-single-target-build-v1" \
  "io.aster.opencode.build-patch.sha256=49c2e7435fb59199df817b36bd7f8c7bfbac5622b86fbadef6a3ea3f1095605d" \
  "io.aster.opencode.builder-bun.sha256=500e6edbf321ddf490adcc55a6a01639993a07924616ab67492e1256c15557e2"; do
  key=${label%%=*}
  want=${label#*=}
  got=$(docker image inspect "$executor" --format "{{index .Config.Labels \"$key\"}}")
  [[ "$got" == "$want" ]] || { echo "$executor $key=$got" >&2; exit 1; }
done

[[ "$opencode_version" == "1.18.2" ]] || { echo "opencode version=$opencode_version" >&2; exit 1; }
if [[ -n "$expected_commit" ]]; then
  [[ "$executor_version" == *"commit=$expected_commit"* ]] || { echo "executor version=$executor_version" >&2; exit 1; }
  [[ "$stager_version" == *"commit=$expected_commit"* ]] || { echo "stager version=$stager_version" >&2; exit 1; }
fi
if docker run --rm "$executor" >"$tmpdir/analysis-executor.out" 2>"$tmpdir/analysis-executor.err"; then
  echo 'analysis executor accepted a missing request' >&2; exit 1
fi
grep -q 'workspace execution request file is missing or unsafe' "$tmpdir/analysis-executor.out"
if docker run --rm "$stager" >"$tmpdir/analysis-stager.out" 2>"$tmpdir/analysis-stager.err"; then
  echo 'analysis stager accepted a missing request' >&2; exit 1
fi
grep -q 'PROW_AI_ANALYSIS_STAGE_REQUEST_B64 is required' "$tmpdir/analysis-stager.err"

image_arch=$(docker image inspect "$executor" --format '{{.Architecture}}')
[[ "$image_arch" == "amd64" ]] || { echo "unsupported executor architecture $image_arch" >&2; exit 1; }
(
  cd "$repo_root/backend"
  CGO_ENABLED=0 GOOS=linux GOARCH="$image_arch" go test -c -o "$tmpdir/analysisexecutor.test" ./internal/analysisexecutor
)
docker run --rm --network none \
  --entrypoint /tmp/analysisexecutor.test \
  -e OPENCODE_1_18_2_BIN=/usr/local/bin/opencode \
  -v "$tmpdir/analysisexecutor.test:/tmp/analysisexecutor.test:ro" \
  "$executor" \
  -test.run '^TestOpenCode1182(InstructionPolicy|BoundedEvidenceExhaustion)Compatibility$' -test.count=1 -test.v

if [[ -n "$contract_output" ]]; then
  [[ -n "$expected_commit" && -n "$expected_image_tag" ]] || { echo 'image contract requires expected commit and image tag' >&2; exit 1; }
  [[ "$executor" =~ @sha256:[0-9a-f]{64}$ && "$stager" =~ @sha256:[0-9a-f]{64}$ ]] || { echo 'image contract requires immutable digest references' >&2; exit 1; }
  executor_revision=$(docker image inspect "$executor" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
  stager_revision=$(docker image inspect "$stager" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
  executor_user=$(docker image inspect "$executor" --format '{{.Config.User}}')
  stager_user=$(docker image inspect "$stager" --format '{{.Config.User}}')
  mkdir -p "$(dirname "$contract_output")"
  python3 - "$contract_output" "$executor" "$stager" "$expected_commit" "$expected_image_tag" "$executor_revision" "$stager_revision" "$executor_user" "$stager_user" "$executor_version" "$stager_version" "$opencode_version" "$tmpdir/opencode-runtime.json" <<'PY'
import hashlib
import json
import os
from pathlib import Path
import sys

(output, executor, stager, commit, image_tag, executor_revision, stager_revision,
 executor_user, stager_user, executor_version, stager_version, opencode_version,
 runtime_path) = sys.argv[1:]
runtime = json.loads(Path(runtime_path).read_text())
payload = {
    "version": 2,
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
    "opencode_upstream_revision": runtime["upstream_revision"],
    "opencode_source_archive_sha256": runtime["source_archive_sha256"],
    "opencode_models_dev_sha256": runtime["models_dev_sha256"],
    "opencode_web_builder_image": runtime["web_builder_image"],
    "opencode_web_builder_node_version": runtime["web_builder_node_version"],
    "opencode_web_builder_bun_image": runtime["web_builder_bun_image"],
    "opencode_web_builder_bun_sha256": runtime["web_builder_bun_sha256"],
    "opencode_web_ui_sha256": runtime["web_ui_sha256"],
    "opencode_builder_image": runtime["builder_image"],
    "opencode_builder_bun_sha256": runtime["builder_bun_sha256"],
    "opencode_bun_version": runtime["bun_version"],
    "opencode_embedded_web_ui": runtime["embedded_web_ui"],
    "opencode_patch_version": runtime["patch_version"],
    "opencode_patch_sha256": runtime["patch_sha256"],
    "opencode_build_patch_version": runtime["build_patch_version"],
    "opencode_build_patch_sha256": runtime["build_patch_sha256"],
    "opencode_binary_sha256": runtime["binary_sha256"],
}
encoded = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()
payload["contract_sha256"] = hashlib.sha256(encoded).hexdigest()
target = Path(output)
target.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")
os.chmod(target, 0o600)
PY
fi

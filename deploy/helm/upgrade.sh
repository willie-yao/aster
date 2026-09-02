#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Upgrade an existing Aster release to one immutable image snapshot.

Usage:
  upgrade.sh --context CONTEXT --namespace NAMESPACE --release RELEASE \
    --version TAG [--values FILE]... [--skip-image-check]

Required:
  --context CONTEXT       Kubernetes context to use
  --namespace NAMESPACE   Existing Helm release namespace
  --release RELEASE       Existing Helm release name
  --version TAG           Immutable sha-<hex> or full semantic-version image tag

Optional:
  --values FILE           Consumer-owned values file; may be repeated
  --skip-image-check      Skip registry manifest checks when local auth is unavailable
  -h, --help              Show this help
USAGE
}

fail() {
  echo "error: $*" >&2
  exit 1
}

valid_version() {
  [[ $1 =~ ^(sha-[0-9a-fA-F]{7,64}|v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)$ ]]
}

extract_images() {
  awk '
    function workload_kind(value) {
      return value == "DaemonSet" || value == "Deployment" || value == "Job" ||
        value == "CronJob" || value == "Pod" || value == "ReplicaSet" ||
        value == "StatefulSet"
    }
    /^---[[:space:]]*$/ {
      kind = ""
      in_containers = 0
      next
    }
    /^kind:[[:space:]]*/ {
      kind = $0
      sub(/^kind:[[:space:]]*/, "", kind)
      next
    }
    workload_kind(kind) {
      match($0, /[^ ]/)
      indent = RSTART ? RSTART - 1 : 0
      if (in_containers && $0 !~ /^[[:space:]]*$/ && indent <= containers_indent) {
        in_containers = 0
      }
      if ($0 ~ /^[[:space:]]*(initContainers|containers):[[:space:]]*$/) {
        in_containers = 1
        containers_indent = indent
        next
      }
      if (in_containers && indent == containers_indent + 4 &&
          $0 ~ /^[[:space:]]*image:[[:space:]]*/) {
        image = $0
        sub(/^[[:space:]]*image:[[:space:]]*/, "", image)
        gsub(/^"|"$/, "", image)
        gsub(/^\047|\047$/, "", image)
        print image
      }
    }
  ' "$1" | sed '/^$/d' | sort -u
}

inspect_image() {
  local image=$1
  case $image_inspector in
    crane)
      crane manifest "$image" >/dev/null
      ;;
    docker)
      docker manifest inspect "$image" >/dev/null
      ;;
    skopeo)
      skopeo inspect "docker://$image" >/dev/null
      ;;
    *)
      return 1
      ;;
  esac
}

context=""
namespace=""
release=""
version=""
skip_image_check=false
values_files=()

while (($#)); do
  case $1 in
    --context)
      (($# >= 2)) || fail "--context requires a value"
      context=$2
      shift 2
      ;;
    --namespace)
      (($# >= 2)) || fail "--namespace requires a value"
      namespace=$2
      shift 2
      ;;
    --release)
      (($# >= 2)) || fail "--release requires a value"
      release=$2
      shift 2
      ;;
    --version)
      (($# >= 2)) || fail "--version requires a value"
      version=$2
      shift 2
      ;;
    --values)
      (($# >= 2)) || fail "--values requires a file"
      values_files+=("$2")
      shift 2
      ;;
    --skip-image-check)
      skip_image_check=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[[ -n $context ]] || fail "--context is required"
[[ -n $namespace ]] || fail "--namespace is required"
[[ -n $release ]] || fail "--release is required"
[[ -n $version ]] || fail "--version is required"
valid_version "$version" || fail "--version must be an immutable sha-<hex> or full semantic-version tag"

for values_file in ${values_files[@]+"${values_files[@]}"}; do
  [[ -f $values_file ]] || fail "values file not found: $values_file"
done

helm_bin=${HELM:-helm}
python_bin=${PYTHON:-python3}
command -v "$helm_bin" >/dev/null 2>&1 || fail "Helm is required"
command -v "$python_bin" >/dev/null 2>&1 || fail "Python 3 is required"
helm_upgrade_help=$("$helm_bin" upgrade --help 2>/dev/null) || fail "could not read Helm upgrade help"
if ! grep -Fq -- '--rollback-on-failure' <<< "$helm_upgrade_help"; then
  fail "Helm 4 with --rollback-on-failure support is required"
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
chart=$script_dir/aster
[[ -f $chart/Chart.yaml ]] || fail "chart not found: $chart"

tmp_root=${ASTER_HELM_TMPDIR:-"$script_dir/../../.test-work"}
mkdir -p "$tmp_root"
tmp=$tmp_root/upgrade-$$
if [[ -e $tmp ]]; then
  fail "scratch path already exists: $tmp"
fi
mkdir "$tmp"
trap 'rm -rf "$tmp"' EXIT
chmod 700 "$tmp"
current_values=$tmp/current-values.json
merged_values=$tmp/merged-values.yaml
candidate_values=$tmp/candidate-values.json
merge_chart=$tmp/value-merge
current_manifest=$tmp/current-manifest.yaml
candidate_manifest=$tmp/candidate-manifest.yaml
deployed_manifest=$tmp/deployed-manifest.yaml
current_images=$tmp/current-images.txt
candidate_images=$tmp/candidate-images.txt
deployed_images=$tmp/deployed-images.txt

"$helm_bin" status "$release" \
  --kube-context "$context" \
  --namespace "$namespace" >/dev/null

umask 077
"$helm_bin" get values "$release" \
  --kube-context "$context" \
  --namespace "$namespace" \
  -o json > "$current_values"

mkdir -p "$merge_chart/templates"
cat > "$merge_chart/Chart.yaml" <<'CHART'
apiVersion: v2
name: value-merge
version: 0.1.0
CHART
cat > "$merge_chart/templates/values.json" <<'TEMPLATE'
{{ toJson .Values }}
TEMPLATE

merge_args=(template value-merge "$merge_chart" --values "$current_values")
for values_file in ${values_files[@]+"${values_files[@]}"}; do
  merge_args+=(--values "$values_file")
done
"$helm_bin" "${merge_args[@]}" > "$merged_values"

"$python_bin" - "$merged_values" "$candidate_values" <<'PY'
import json
import sys

merged_path, candidate_path = sys.argv[1:]
with open(merged_path, encoding="utf-8") as merged_file:
    merged = merged_file.read()
start = merged.find("{")
if start < 0:
    raise SystemExit("Helm value merge did not produce a JSON object")
try:
    values, end = json.JSONDecoder().raw_decode(merged[start:])
except json.JSONDecodeError as err:
    raise SystemExit(f"could not decode merged Helm values: {err}") from err
if not isinstance(values, dict):
    raise SystemExit("merged Helm values must be an object")


with open(candidate_path, "w", encoding="utf-8") as candidate_file:
    json.dump(values, candidate_file, indent=2, sort_keys=True)
    candidate_file.write("\n")

PY
chmod 600 "$candidate_values"

cache_generation=$("$python_bin" - "$current_values" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as values_file:
    values = json.load(values_file)
analysis_cache = values.get("analysisCache") or {}
if not isinstance(analysis_cache, dict):
    raise SystemExit("analysisCache must be an object")
generation = analysis_cache.get("generation", "")
if generation is None:
    generation = ""
if not isinstance(generation, (str, int)) or isinstance(generation, bool):
    raise SystemExit("analysisCache.generation must be a string or integer")
print(generation)
PY
)
if [[ -n $cache_generation && ! $cache_generation =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]]; then
  fail "the deployed analysisCache.generation is invalid; refusing to change it"
fi

"$helm_bin" get manifest "$release" \
  --kube-context "$context" \
  --namespace "$namespace" > "$current_manifest"

value_args=(--values "$candidate_values")
upgrade_args=(
  upgrade "$release" "$chart"
  --kube-context "$context"
  --namespace "$namespace"
  --reset-values
  --values "$candidate_values"
)
set_args=(
  --set-string "global.imageTag=$version"
  --set-string "analysisCache.generation=$cache_generation"
)

echo "Preserving analysis cache generation: ${cache_generation:-<empty>}"
echo "Linting chart..."
"$helm_bin" lint "$chart" "${value_args[@]}" "${set_args[@]}"

echo "Rendering candidate release..."
"$helm_bin" template "$release" "$chart" \
  --namespace "$namespace" \
  "${value_args[@]}" \
  "${set_args[@]}" > "$candidate_manifest"

extract_images "$current_manifest" > "$current_images"
extract_images "$candidate_manifest" > "$candidate_images"
[[ -s $candidate_images ]] || fail "candidate release rendered no container images"

echo "Image changes:"
if cmp -s "$current_images" "$candidate_images"; then
  echo "  (no image changes; check for explicit image-specific tags)"
else
  while IFS= read -r image; do
    grep -Fxq "$image" "$candidate_images" || echo "  - $image"
  done < "$current_images"
  while IFS= read -r image; do
    grep -Fxq "$image" "$current_images" || echo "  + $image"
  done < "$candidate_images"
fi

while IFS= read -r image; do
  image_tag=${image##*:}
  if [[ $image_tag == "$image" ]] || ! valid_version "$image_tag"; then
    fail "candidate image must use an immutable sha-<hex> or full semantic-version tag: $image"
  fi
done < "$candidate_images"

if [[ $skip_image_check == false ]]; then
  image_inspector=""
  if command -v crane >/dev/null 2>&1; then
    image_inspector=crane
  elif command -v docker >/dev/null 2>&1; then
    image_inspector=docker
  elif command -v skopeo >/dev/null 2>&1; then
    image_inspector=skopeo
  fi

  if [[ -z $image_inspector ]]; then
    echo "warning: crane, docker, or skopeo is required to verify image manifests; continuing without registry checks" >&2
  else
    echo "Verifying rendered image manifests with $image_inspector..."
    while IFS= read -r image; do
      inspect_image "$image" || fail "image manifest is unavailable or local registry authentication failed: $image"
    done < "$candidate_images"
  fi
else
  echo "Skipping image manifest checks by explicit request."
fi

echo "Upgrading $namespace/$release on context $context..."
upgrade_args+=("${set_args[@]}" --wait --rollback-on-failure)
"$helm_bin" "${upgrade_args[@]}"

status_json=$tmp/status.json
"$helm_bin" status "$release" \
  --kube-context "$context" \
  --namespace "$namespace" \
  -o json > "$status_json"
revision=$("$python_bin" - "$status_json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as status_file:
    status = json.load(status_file)
revision = status.get("version", status.get("revision"))
if revision is None:
    raise SystemExit("Helm status did not include a revision")
print(revision)
PY
)

"$helm_bin" get manifest "$release" \
  --kube-context "$context" \
  --namespace "$namespace" > "$deployed_manifest"
extract_images "$deployed_manifest" > "$deployed_images"
[[ -s $deployed_images ]] || fail "deployed release manifest contains no container images"

echo "Helm revision: $revision"
echo "Deployed image references:"
sed 's/^/  /' "$deployed_images"

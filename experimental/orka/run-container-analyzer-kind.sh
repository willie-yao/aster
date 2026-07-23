#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# shellcheck source=experimental/orka/orka.env
source "$repo_root/experimental/orka/orka.env"

cluster=${ORKA_CONTAINER_CLUSTER:-orka-container-analyzer}
context="kind-$cluster"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/orka-container-analyzer.XXXXXX")
run_id=$(date -u +%Y%m%d%H%M%S)-$$
controller_repository=orka-controller
controller_tag="container-analyzer-$run_id"
controller_image="$controller_repository:$controller_tag"
base_image="dashboard-analyzer-base:$run_id"
analyzer_image="dashboard-analyzer:$run_id"
model_image="orka-script-model:$run_id"
cluster_owned=false
lock_owned=false
lock_name=${cluster//[^a-zA-Z0-9_.-]/_}
lock_dir="${TMPDIR:-/tmp}/prow-ai-dashboard-orka-container-$lock_name.lock"
owned_images=()

cleanup() {
  if [[ $cluster_owned == true ]]; then
    kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
  fi
  if (( ${#owned_images[@]} > 0 )); then
    docker image rm -f "${owned_images[@]}" >/dev/null 2>&1 || true
  fi
  if [[ $lock_owned == true ]]; then
    rmdir "$lock_dir" >/dev/null 2>&1 || true
  fi
  rm -rf "$tmp"
}
trap cleanup EXIT

for command in docker kind kubectl helm git curl tar go; do
  command -v "$command" >/dev/null || { echo "$command is required" >&2; exit 1; }
done

if ! mkdir "$lock_dir" 2>/dev/null; then
  echo "another container analyzer run owns cluster name $cluster" >&2
  exit 1
fi
lock_owned=true

if kind get clusters | grep -Fxq "$cluster"; then
  echo "kind cluster $cluster already exists; choose a different ORKA_CONTAINER_CLUSTER" >&2
  exit 1
fi

echo "Creating isolated kind cluster $cluster"
cat > "$tmp/kind.yaml" <<'KIND'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
- role: worker
KIND
# Claim cleanup ownership before creation so a partial failed create is removed.
cluster_owned=true
kind create cluster --name "$cluster" --config "$tmp/kind.yaml"
kubectl --context "$context" label node "$cluster-worker" agentpool=nodepool1 --overwrite
kubectl --context "$context" label node "$cluster-worker2" agentpool=h100 --overwrite
kubectl --context "$context" taint node "$cluster-worker2" nvidia.com/gpu=true:NoSchedule --overwrite

orka_source="$tmp/orka"
git clone --quiet "$ORKA_REPOSITORY" "$orka_source"
git -C "$orka_source" checkout --quiet --detach "$ORKA_COMMIT"
actual=$(git -C "$orka_source" rev-parse HEAD)
[[ $actual == "$ORKA_COMMIT" ]] || { echo "Orka checkout is $actual, want $ORKA_COMMIT" >&2; exit 1; }

echo "Building pinned Orka controller $ORKA_COMMIT"
docker build -q -t "$controller_image" "$orka_source" >/dev/null
owned_images+=("$controller_image")

echo "Building dashboard analyzer"
docker build -q \
  -f "$repo_root/experimental/orka/Dockerfile" \
  -t "$base_image" \
  "$repo_root/backend" >/dev/null
owned_images+=("$base_image")

fixture=flatcar-sysext-dns-providerid.tar.gz
fixture_sha=${ORKA_CONTAINER_FIXTURE_SHA:-8ed886395742d145c014be4b6a2dc38b3ddf3db0ad6e7a5740da10eea80a1945}
mkdir -p "$tmp/image/fixtures"
curl -fsSL "https://github.com/willie-yao/prow-ai-dashboard/releases/download/benchmark-fixtures/$fixture" -o "$tmp/$fixture"
if command -v sha256sum >/dev/null; then
  actual_fixture_sha=$(sha256sum "$tmp/$fixture" | awk '{print $1}')
else
  actual_fixture_sha=$(shasum -a 256 "$tmp/$fixture" | awk '{print $1}')
fi
[[ $actual_fixture_sha == "$fixture_sha" ]] || { echo "fixture checksum $actual_fixture_sha, want $fixture_sha" >&2; exit 1; }
tar -xzf "$tmp/$fixture" -C "$tmp/image/fixtures"
cat > "$tmp/image/Dockerfile" <<EOF_IMAGE
FROM $base_image
COPY fixtures /fixtures
EOF_IMAGE
docker build -q -t "$analyzer_image" "$tmp/image" >/dev/null
owned_images+=("$analyzer_image")

cat > "$tmp/model.Dockerfile" <<'MODEL_IMAGE'
FROM python:3.12-alpine
MODEL_IMAGE
docker build -q -t "$model_image" -f "$tmp/model.Dockerfile" "$tmp" >/dev/null
owned_images+=("$model_image")
kind load docker-image --name "$cluster" "$controller_image" "$analyzer_image" "$model_image"

kubectl --context "$context" create namespace orka-system
kubectl --context "$context" apply -f "$orka_source/config/crd/bases/"
kubectl --context "$context" apply -f "$repo_root/experimental/orka/manifests/00-rbac.yaml"
kubectl --context "$context" apply -f "$repo_root/experimental/orka/manifests/60-pipeline-rbac.yaml"
helm upgrade --install orka "$orka_source/charts/orka" \
  --kube-context "$context" \
  --namespace orka-system \
  --set controller.image.repository="$controller_repository" \
  --set controller.image.tag="$controller_tag" \
  --set controller.image.pullPolicy=Never \
  --set nodeSelector.agentpool=nodepool1 \
  --set crds.install=false
kubectl --context "$context" rollout status -n orka-system deployment/orka-controller --timeout=5m
# The harness wrapper is not part of this container Task path. Keep the unused
# helper off the mock GPU pool during the isolated benchmark.
kubectl --context "$context" scale -n orka-system deployment/orka-agent-harness-wrapper --replicas=0

cd "$repo_root/backend"
RUN_ORKA_CONTAINER_ANALYZER_KIND=1 \
ORKA_CONTAINER_CONTEXT="$context" \
ORKA_CONTAINER_IMAGE="$analyzer_image" \
ORKA_CONTAINER_MODEL_IMAGE="$model_image" \
ORKA_CONTAINER_LIVE_ENDPOINT="${ORKA_CONTAINER_LIVE_ENDPOINT:-}" \
ORKA_CONTAINER_LIVE_MODEL="${ORKA_CONTAINER_LIVE_MODEL:-}" \
ORKA_CONTAINER_LIVE_TOKEN="${ORKA_CONTAINER_LIVE_TOKEN:-}" \
go test ./internal/e2e -run '^TestOrkaContainerAnalyzerKind$' -v -count=1 -timeout 45m

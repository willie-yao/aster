#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$script_dir/../../.." && pwd)
# shellcheck source=experimental/orka/worker-patches/compatibility.env
source "$script_dir/compatibility.env"
patch_file="$script_dir/$ORKA_PATCH"
temp_dir=""

cleanup() {
  if [[ -n $temp_dir ]]; then
    rm -rf "$temp_dir"
  fi
}
trap cleanup EXIT

usage() {
  cat <<'USAGE'
Usage:
  compat-worker.sh verify
  compat-worker.sh metadata [dashboard-commit]
  compat-worker.sh check-source <orka-source>
  compat-worker.sh prepare <orka-source>
  compat-worker.sh test <patched-orka-source>
  compat-worker.sh inspect-published <image> <dashboard-commit>
  compat-worker.sh build <image> [orka-source]

build clones the pinned Orka commit when orka-source is omitted, applies the
compatibility patch, runs focused normal and race tests plus the full worker
package, and builds workers/ai/Dockerfile.
USAGE
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

verify_config() {
  [[ $COMPATIBILITY_VERSION =~ ^v[0-9]+$ ]] || {
    echo "invalid COMPATIBILITY_VERSION: $COMPATIBILITY_VERSION" >&2
    return 1
  }
  [[ $ORKA_COMMIT =~ ^[0-9a-f]{40}$ ]] || {
    echo "invalid ORKA_COMMIT: $ORKA_COMMIT" >&2
    return 1
  }
  [[ $ORKA_PATCH_SHA256 =~ ^[0-9a-f]{64}$ ]] || {
    echo "invalid ORKA_PATCH_SHA256: $ORKA_PATCH_SHA256" >&2
    return 1
  }
  [[ -f $patch_file ]] || {
    echo "missing patch: $patch_file" >&2
    return 1
  }
  actual=$(sha256_file "$patch_file")
  [[ $actual == "$ORKA_PATCH_SHA256" ]] || {
    echo "patch checksum $actual does not match $ORKA_PATCH_SHA256" >&2
    return 1
  }
}

image_metadata() {
  local dashboard_commit=${1:-}
  if [[ -z $dashboard_commit ]]; then
    dashboard_commit=$(git -C "$repo_root" rev-parse HEAD)
  fi
  [[ $dashboard_commit =~ ^[0-9a-f]{40}$ ]] || {
    echo "dashboard commit must be a full 40-character SHA" >&2
    return 1
  }
  local tag="${COMPATIBILITY_VERSION}-orka-${ORKA_COMMIT}-dashboard-${dashboard_commit}"
  (( ${#tag} <= 128 )) || {
    echo "generated image tag exceeds 128 characters" >&2
    return 1
  }
  printf 'compatibility_version=%s\n' "$COMPATIBILITY_VERSION"
  printf 'orka_repository=%s\n' "$ORKA_REPOSITORY"
  printf 'orka_commit=%s\n' "$ORKA_COMMIT"
  printf 'patch_sha256=%s\n' "$ORKA_PATCH_SHA256"
  printf 'dashboard_commit=%s\n' "$dashboard_commit"
  printf 'image_tag=%s\n' "$tag"
}

check_source() {
  local source=$1
  [[ -d $source/.git || -f $source/.git ]] || {
    echo "not a Git checkout: $source" >&2
    return 1
  }
  local actual
  actual=$(git -C "$source" rev-parse HEAD)
  [[ $actual == "$ORKA_COMMIT" ]] || {
    echo "Orka checkout is $actual, want $ORKA_COMMIT" >&2
    return 1
  }
  [[ -z $(git -C "$source" status --porcelain) ]] || {
    echo "Orka checkout is not clean: $source" >&2
    return 1
  }
  git -C "$source" apply --check "$patch_file"
}

prepare_source() {
  local source=$1
  check_source "$source"
  git -C "$source" apply "$patch_file"
  git -C "$source" diff --check
}

test_source() {
  local source=$1
  [[ -f $source/workers/ai/validated_analysis.go && -f $source/workers/ai/analysis_context.go ]] || {
    echo "compatibility patch is not applied in $source" >&2
    return 1
  }
  grep -Fq 'Store: openai.Bool(false)' "$source/internal/llm/openai/provider.go" || {
    echo "Responses storage safeguard is not applied in $source" >&2
    return 1
  }
  (
    cd "$source"
    test -z "$(gofmt -l workers/ai/*.go)"
    focused_tests='Test(Analysis|Validated|ToolAlias|CachedToolResult|PrepareAnalysisRequest|RequestApproval|ExplicitApproval|MalformedAliasedApproval|VerifiedTimeline|SkippedApprovedTimeline|ExecuteAgentLoop(FinalizesValidatedAnalysis|RequiresValidation|StopsRepeatedValidationFailure)|OrdinaryTaskFinalization|TimelineToolEnablesLegacyTransientCritique|ValidationPromptCanBeReappliedAfterCompaction)'
    go test ./workers/ai -run "$focused_tests" -count=1
    go test ./internal/llm ./internal/llm/openai -count=1
    go test -race ./workers/ai -count=1
    go test ./workers/ai -count=1
  )
}

clone_source() {
  local destination=$1
  git init -q "$destination"
  git -C "$destination" remote add origin "$ORKA_REPOSITORY"
  git -C "$destination" fetch -q --depth=1 origin "$ORKA_COMMIT"
  git -C "$destination" checkout -q --detach FETCH_HEAD
}


inspect_published() {
  local image=$1 dashboard_commit=$2 output rc
  [[ $dashboard_commit =~ ^[0-9a-f]{40}$ ]] || {
    echo "dashboard commit must be a full 40-character SHA" >&2
    return 1
  }
  set +e
  output=$(docker buildx imagetools inspect "$image" --format '{{json .}}' 2>&1)
  rc=$?
  set -e
  if [[ $rc -ne 0 ]]; then
    if grep -Eqi '404 Not Found|manifest unknown|MANIFEST_UNKNOWN|: not found$' <<< "$output"; then
      return 10
    fi
    echo "registry inspection failed for $image:" >&2
    echo "$output" >&2
    return 1
  fi

  jq -e     --arg orka "$ORKA_COMMIT"     --arg patch "$ORKA_PATCH_SHA256"     --arg dashboard "$dashboard_commit"     '.image.os == "linux" and
     .image.architecture == "amd64" and
     .image.config.User == "65532:65532" and
     .image.config.Entrypoint == ["/worker"] and
     .image.config.Labels["org.opencontainers.image.revision"] == $dashboard and
     .image.config.Labels["io.orka.compatibility.revision"] == $orka and
     .image.config.Labels["io.orka.compatibility.patch-sha256"] == $patch'     <<< "$output" > /dev/null || {
      echo "existing compatibility image does not match the requested contract: $image" >&2
      return 1
    }

  local attestations
  attestations=$(jq -r '.manifest.manifests[]? | select(.annotations["vnd.docker.reference.type"] == "attestation-manifest") | .digest' <<< "$output")
  [[ -n $attestations ]] || {
    echo "existing compatibility image has no attestation manifest: $image" >&2
    return 1
  }
  local predicates="" attestation raw
  while IFS= read -r attestation; do
    [[ -z $attestation ]] && continue
    raw=$(docker buildx imagetools inspect "$image@$attestation" --raw)
    predicates+=$'\n'$(jq -r '.layers[]?.annotations["in-toto.io/predicate-type"] // empty' <<< "$raw")
  done <<< "$attestations"
  grep -Eq '^https://slsa.dev/provenance/(v0\.2|v1)$' <<< "$predicates" || {
    echo "existing compatibility image has no supported SLSA provenance attestation: $image" >&2
    return 1
  }
  grep -Fqx 'https://spdx.dev/Document' <<< "$predicates" || {
    echo "existing compatibility image has no SPDX SBOM attestation: $image" >&2
    return 1
  }

  jq -n     --arg image "$image"     --arg digest "$(jq -r '.manifest.digest' <<< "$output")"     --arg orkaCommit "$ORKA_COMMIT"     --arg patchSHA256 "$ORKA_PATCH_SHA256"     --arg dashboardCommit "$dashboard_commit"     '{image:$image,digest:$digest,orka_commit:$orkaCommit,patch_sha256:$patchSHA256,dashboard_commit:$dashboardCommit,published:true,recovered:true}'
}


build_image() {
  local image=$1
  local source=${2:-}
  if [[ -z $source ]]; then
    temp_dir=$(mktemp -d)
    source="$temp_dir/orka"
    clone_source "$source"
  fi
  prepare_source "$source"
  test_source "$source"
  docker build --file "$source/workers/ai/Dockerfile" --tag "$image" "$source"
}

verify_config
command=${1:-}
case $command in
  verify)
    [[ $# -eq 1 ]] || { usage >&2; exit 2; }
    ;;
  metadata)
    [[ $# -le 2 ]] || { usage >&2; exit 2; }
    image_metadata "${2:-}"
    ;;
  check-source)
    [[ $# -eq 2 ]] || { usage >&2; exit 2; }
    check_source "$2"
    ;;
  prepare)
    [[ $# -eq 2 ]] || { usage >&2; exit 2; }
    prepare_source "$2"
    ;;
  test)
    [[ $# -eq 2 ]] || { usage >&2; exit 2; }
    test_source "$2"
    ;;
  inspect-published)
    [[ $# -eq 3 ]] || { usage >&2; exit 2; }
    inspect_published "$2" "$3"
    ;;
  build)
    [[ $# -ge 2 && $# -le 3 ]] || { usage >&2; exit 2; }
    build_image "$2" "${3:-}"
    ;;
  -h|--help)
    usage
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

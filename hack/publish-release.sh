#!/usr/bin/env bash
set -euo pipefail

: "${TAG:?TAG is required}"

release_dry_run=${RELEASE_DRY_RUN:-false}
release_tags_only=${RELEASE_TAGS_ONLY:-false}
case $release_dry_run:$release_tags_only in
  true:true | true:false | false:true | false:false) ;;
  *)
    echo "RELEASE_DRY_RUN and RELEASE_TAGS_ONLY must be true or false" >&2
    exit 1
    ;;
esac
if [[ ! $TAG =~ ^v(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)[.](0|[1-9][0-9]*)(-(beta|rc)[.](0|[1-9][0-9]*))?$ ]]; then
  echo "invalid release tag: $TAG" >&2
  exit 1
fi

module_tag="backend/$TAG"
release_ref_prefix="refs/aster-release/$$"
root_remote_ref="$release_ref_prefix/root"
module_remote_ref="$release_ref_prefix/module"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/aster-release.XXXXXX")
cleanup() {
  git update-ref -d "$root_remote_ref" 2>/dev/null || true
  git update-ref -d "$module_remote_ref" 2>/dev/null || true
  find "$tmp" -type f -delete 2>/dev/null || true
  rmdir "$tmp" 2>/dev/null || true
}
trap cleanup EXIT

remote_tag_commit() {
  local tag=$1
  local output=$2
  local remote_ref=$3
  local tag_ref="refs/tags/$tag"
  set +e
  git ls-remote --exit-code --refs --tags origin "$tag_ref" > "$output"
  local status=$?
  set -e
  if [[ $status -eq 2 ]]; then
    return 2
  fi
  if [[ $status -ne 0 ]]; then
    echo "failed to inspect remote release tag: $tag" >&2
    return 1
  fi
  if ! git fetch --quiet --no-tags --force origin "$tag_ref:$remote_ref"; then
    echo "failed to fetch remote release tag: $tag" >&2
    return 1
  fi
  local commit
  if ! commit=$(git rev-parse "$remote_ref^{commit}"); then
    echo "remote release tag does not identify a commit: $tag" >&2
    return 1
  fi
  printf '%s\n' "$commit"
}

ensure_release_tag_pair() {
  local root_commit=""
  local root_status=0
  local module_commit=""
  local module_status=0
  root_commit=$(remote_tag_commit "$TAG" "$tmp/root-tag-remote" "$root_remote_ref") || root_status=$?
  module_commit=$(remote_tag_commit "$module_tag" "$tmp/module-tag-remote" "$module_remote_ref") || module_status=$?

  if [[ $root_status -ne 0 && $root_status -ne 2 ]] ||
    [[ $module_status -ne 0 && $module_status -ne 2 ]]; then
    return 1
  fi
  if [[ $root_status -eq 2 ]]; then
    if [[ $module_status -eq 0 ]]; then
      echo "module tag exists without root release tag: $module_tag" >&2
    else
      echo "remote release tag does not exist: $TAG" >&2
    fi
    return 1
  fi
  if [[ $root_commit != "$reviewed_commit" ]]; then
    echo "remote release tag $TAG does not identify reviewed HEAD $reviewed_commit" >&2
    return 1
  fi
  if [[ $module_status -eq 0 && $module_commit != "$reviewed_commit" ]]; then
    echo "remote module tag $module_tag does not identify reviewed HEAD $reviewed_commit" >&2
    return 1
  fi

  if [[ $module_status -eq 2 ]]; then
    if [[ $release_dry_run == true ]]; then
      echo "would create module tag $module_tag at $reviewed_commit"
      return
    fi
    if git show-ref --verify --quiet "refs/tags/$module_tag"; then
      local local_module_commit
      local_module_commit=$(git rev-parse "$module_tag^{commit}")
      if [[ $local_module_commit != "$reviewed_commit" ]]; then
        echo "local module tag $module_tag does not identify reviewed HEAD $reviewed_commit" >&2
        return 1
      fi
    else
      git -c tag.gpgSign=false tag "$module_tag" "$reviewed_commit"
    fi
    if ! git push --atomic origin \
      "$root_remote_ref:refs/tags/$TAG" \
      "refs/tags/$module_tag:refs/tags/$module_tag"; then
      local root_after=""
      local root_after_status=0
      local module_after=""
      local module_after_status=0
      root_after=$(remote_tag_commit "$TAG" "$tmp/root-tag-remote" "$root_remote_ref") || root_after_status=$?
      module_after=$(remote_tag_commit "$module_tag" "$tmp/module-tag-remote" "$module_remote_ref") || module_after_status=$?
      if [[ $root_after_status -eq 0 && $module_after_status -eq 0 &&
        $root_after == "$reviewed_commit" && $module_after == "$reviewed_commit" ]]; then
        echo "release tags $TAG and $module_tag were created concurrently at $reviewed_commit"
        return
      fi
      echo "failed to create immutable release tag pair for $TAG" >&2
      return 1
    fi
    module_commit=$(remote_tag_commit "$module_tag" "$tmp/module-tag-remote" "$module_remote_ref")
    if [[ $module_commit != "$reviewed_commit" ]]; then
      echo "remote module tag $module_tag does not identify reviewed HEAD $reviewed_commit after push" >&2
      return 1
    fi
    echo "created module tag $module_tag at $reviewed_commit"
    return
  fi

  echo "release tags $TAG and $module_tag identify reviewed HEAD $reviewed_commit"
}

reviewed_commit=$(git rev-parse 'HEAD^{commit}')
ensure_release_tag_pair
if [[ $release_dry_run == true || $release_tags_only == true ]]; then
  exit 0
fi

: "${REPOSITORY_OWNER:?REPOSITORY_OWNER is required}"
: "${IMAGE_REPOSITORY:?IMAGE_REPOSITORY is required}"

export GOTOOLCHAIN=local
if [[ $(go env GOVERSION) != go1.25.12 ]]; then
  echo "release requires Go 1.25.12 with GOTOOLCHAIN=local" >&2
  exit 1
fi

chart_version=${TAG#v}
TAG="$TAG" \
  IMAGE_REPOSITORY="$IMAGE_REPOSITORY" \
  REVIEWED_COMMIT="$reviewed_commit" \
  hack/verify-release-images.sh

major=""
if [[ $TAG != *-* ]]; then
  major=$(printf '%s' "$TAG" | sed -E 's/^(v[0-9]+).*/\1/')
  alias_ref="refs/tags/$major"
  set +e
  git ls-remote --exit-code --refs --tags origin "$alias_ref" > "$tmp/alias-remote"
  alias_status=$?
  set -e
  case $alias_status in
    0)
      git fetch --force origin "$alias_ref:$alias_ref"
      current_commit=$(git rev-parse "$major^{commit}")
      current_versions=$(git tag --points-at "$current_commit" --list "$major.*" | grep -E '^v[0-9]+[.][0-9]+[.][0-9]+$' || true)
      current_version=$(printf '%s\n' "$current_versions" | python3 -c 'import re, sys; values=[line.strip() for line in sys.stdin if line.strip()]; parsed=[(tuple(map(int, re.fullmatch(r"v([0-9]+)\.([0-9]+)\.([0-9]+)", value).groups())), value) for value in values]; print(max(parsed)[1] if parsed else "")')
      if [[ -z $current_version ]]; then
        echo "stable alias $major does not point at a stable semantic-version tag" >&2
        exit 1
      fi
      python3 - "$TAG" "$current_version" <<'PY_VERSION'
import re
import sys

def parse(value):
    match = re.fullmatch(r"v([0-9]+)\.([0-9]+)\.([0-9]+)", value)
    if not match:
        raise SystemExit(f"invalid stable version: {value}")
    return tuple(map(int, match.groups()))

requested = parse(sys.argv[1])
current = parse(sys.argv[2])
if requested < current:
    raise SystemExit(f"refusing to move stable alias backward from {sys.argv[2]} to {sys.argv[1]}")
PY_VERSION
      ;;
    2) ;;
    *)
      echo "failed to inspect stable alias $major" >&2
      exit 1
      ;;
  esac
fi

helm lint deploy/helm/aster
helm lint deploy/helm/aster-platform \
  --set application.releaseName=release \
  --set execution.namespace=release-sandbox \
  --set execution.runtimeClassName=secure-runtime \
  --set-string 'execution.networkPolicy.allowedFQDNs[0]=vcs.example.test'

for chart in aster aster-platform; do
  helm package "deploy/helm/$chart" \
    --destination "$tmp" \
    --version "$chart_version" \
    --app-version "$TAG"
done

app_pkg=$tmp/aster-$chart_version.tgz
platform_pkg=$tmp/aster-platform-$chart_version.tgz
cli_assets=()
cli_asset_names=()
for target in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  goos=${target%-*}
  goarch=${target#*-}
  asset="aster-${TAG}-${target}"
  (
    cd backend
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
      -trimpath \
      -ldflags "-s -w -X main.version=$TAG -X main.commit=$reviewed_commit -X main.imageTag=$TAG" \
      -o "$tmp/$asset" \
      ./cmd/aster
  )
  cli_assets+=("$tmp/$asset")
  cli_asset_names+=("$asset")
done
source_archive="$tmp/aster-${TAG}-source.tar.gz"
release_manifest="$tmp/aster-${TAG}-release-manifest.json"
git archive \
  --format=tar.gz \
  --prefix="aster-${TAG}/" \
  --output="$source_archive" \
  HEAD
python3 - "$release_manifest" "$TAG" "$reviewed_commit" "$REPOSITORY_OWNER" <<'PY_MANIFEST'
import json
import sys

path, tag, revision, owner = sys.argv[1:]
chart_version = tag.removeprefix("v")
assets = [
    f"aster-{chart_version}.tgz",
    f"aster-platform-{chart_version}.tgz",
    *(f"aster-{tag}-{target}" for target in ("linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64")),
    f"aster-{tag}-source.tar.gz",
    "SHA256SUMS",
]
manifest = {
    "schema_version": 1,
    "version": tag,
    "revision": revision,
    "images": {
        "application": f"ghcr.io/{owner}/aster:{tag}",
        "remote_fixer": f"ghcr.io/{owner}/aster/remote-fixer:{tag}",
        "fix_executor": f"ghcr.io/{owner}/aster/agent-sandbox-fix-executor:{tag}",
    },
    "charts": {
        "application": f"oci://ghcr.io/{owner}/charts/aster:{chart_version}",
        "platform": f"oci://ghcr.io/{owner}/charts/aster-platform:{chart_version}",
    },
    "assets": assets,
}
with open(path, "w", encoding="utf-8") as output:
    json.dump(manifest, output, indent=2, sort_keys=True)
    output.write("\n")
PY_MANIFEST
(
  cd "$tmp"
  shasum -a 256 \
    "$(basename "$app_pkg")" \
    "$(basename "$platform_pkg")" \
    "${cli_asset_names[@]}" \
    "$(basename "$source_archive")" \
    "$(basename "$release_manifest")" > SHA256SUMS
)
registry="oci://ghcr.io/$REPOSITORY_OWNER/charts"
# Publish the prerequisite chart first. The application chart is the consumer
# entry point and is published only after its matching platform artifact exists.
ensure_release_tag_pair
helm push "$platform_pkg" "$registry"
ensure_release_tag_pair
helm push "$app_pkg" "$registry"
ensure_release_tag_pair

release_args=("$TAG" "$app_pkg" "$platform_pkg" "$source_archive" "$release_manifest" "$tmp/SHA256SUMS" "${cli_assets[@]}" --title "$TAG" --generate-notes --verify-tag)
if [[ $TAG == *-* ]]; then
  release_args+=(--prerelease)
fi
gh release create "${release_args[@]}"

if [[ $TAG != *-* ]]; then
  git tag -f "$major" "$TAG"
  git push origin -f "refs/tags/$major"
fi

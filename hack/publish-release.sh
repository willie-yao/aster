#!/usr/bin/env bash
set -euo pipefail

: "${TAG:?TAG is required}"
: "${REPOSITORY_OWNER:?REPOSITORY_OWNER is required}"

chart_version=${TAG#v}
tmp=$(mktemp -d "${TMPDIR:-/tmp}/prow-ai-dashboard-release.XXXXXX")
cleanup() {
  find "$tmp" -type f -delete 2>/dev/null || true
  rmdir "$tmp" 2>/dev/null || true
}
trap cleanup EXIT

verify_release_tag() {
  local tag_ref="refs/tags/$TAG"
  set +e
  git ls-remote --exit-code --tags origin "$tag_ref" > "$tmp/release-tag-remote"
  local status=$?
  set -e
  if [[ $status -eq 2 ]]; then
    echo "remote release tag does not exist: $TAG" >&2
    return 1
  fi
  if [[ $status -ne 0 ]]; then
    echo "failed to inspect remote release tag: $TAG" >&2
    return 1
  fi
  git fetch --force origin "$tag_ref:$tag_ref"
  local head_commit tag_commit
  head_commit=$(git rev-parse 'HEAD^{commit}')
  tag_commit=$(git rev-parse "$TAG^{commit}")
  if [[ $tag_commit != "$head_commit" ]]; then
    echo "remote release tag $TAG does not identify reviewed HEAD $head_commit" >&2
    return 1
  fi
}

verify_release_tag


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

helm lint deploy/helm/prow-ai-dashboard
helm lint deploy/helm/prow-ai-dashboard-platform \
  --set application.releaseName=release \
  --set execution.namespace=release-sandbox \
  --set execution.runtimeClassName=secure-runtime

for chart in prow-ai-dashboard prow-ai-dashboard-platform; do
  helm package "deploy/helm/$chart" \
    --destination "$tmp" \
    --version "$chart_version" \
    --app-version "$TAG"
done

app_pkg=$tmp/prow-ai-dashboard-$chart_version.tgz
platform_pkg=$tmp/prow-ai-dashboard-platform-$chart_version.tgz
registry="oci://ghcr.io/$REPOSITORY_OWNER/charts"
# Publish the prerequisite chart first. The application chart is the consumer
# entry point and is published only after its matching platform artifact exists.
verify_release_tag
helm push "$platform_pkg" "$registry"
verify_release_tag
helm push "$app_pkg" "$registry"
verify_release_tag

release_args=("$TAG" "$app_pkg" "$platform_pkg" --title "$TAG" --generate-notes --verify-tag)
if [[ $TAG == *-* ]]; then
  release_args+=(--prerelease)
fi
gh release create "${release_args[@]}"

if [[ $TAG != *-* ]]; then
  git tag -f "$major" "$TAG"
  git push origin -f "refs/tags/$major"
fi

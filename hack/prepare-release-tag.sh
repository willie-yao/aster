#!/usr/bin/env bash
# Create and push a release tag pair after validating everything that can be
# checked before the tags exist. Tagging by hand is how this repository ended up
# with a version line that moved backward and a release whose module tag was
# never created; every check here exists because skipping it caused a real
# problem.
set -euo pipefail

: "${TAG:?TAG is required}"

dry_run=${PREPARE_DRY_RUN:-false}
case $dry_run in
  true | false) ;;
  *)
    echo "PREPARE_DRY_RUN must be true or false" >&2
    exit 1
    ;;
esac

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
# shellcheck source=hack/release-checks.sh
source "$root/hack/release-checks.sh"

module_tag="backend/$TAG"

check_release_tag_format "$TAG"
check_release_notes "$TAG"
check_version_moves_forward "$TAG"

# Neither tag may exist yet. Distinguish absence from an inspection failure so a
# transport or auth error is never read as "the tag is free".
for tag in "$TAG" "$module_tag"; do
  set +e
  git ls-remote --exit-code --refs --tags origin "refs/tags/$tag" > /dev/null
  status=$?
  set -e
  case $status in
    0)
      echo "release tag already exists: $tag" >&2
      exit 1
      ;;
    2) ;;
    *)
      echo "failed to inspect release tag: $tag" >&2
      exit 1
      ;;
  esac
done

# Reusing a version is unsafe once the module mirror is serving it: the mirror
# can keep returning the original content after the tag is gone, and the version
# is identified by a checksum that does not change, so a client that fetches
# rebuilt content fails verification against the recorded sum. Deleting the tags
# does not undo that, and the tag checks above cannot see a version whose tags
# were deleted.
# The cached-only endpoint answers from what the proxy already has, so this
# never triggers an origin fetch that would negatively cache the version we are
# about to create.
module_path=$(awk '/^module /{print $2; exit}' backend/go.mod)
proxy_info="https://proxy.golang.org/cached-only/${module_path}/@v/${TAG}.info"
proxy_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 "$proxy_info") || proxy_status=unreachable
case $proxy_status in
  404) ;;
  200)
    echo "module version already published and immutable on the Go proxy: ${module_path}@${TAG}" >&2
    echo "pick the next version; a deleted tag does not free its version number" >&2
    exit 1
    ;;
  *)
    # Includes 410, which the proxy protocol defines only as "not available"
    # and not as proof the version was never served. Fail closed.
    echo "failed to check the Go module proxy for ${module_path}@${TAG} (status $proxy_status)" >&2
    exit 1
    ;;
esac

# Tag the reviewed tip of the default branch, never a local commit that was
# never pushed or reviewed.
git fetch --quiet origin main
reviewed_commit=$(git rev-parse 'refs/remotes/origin/main^{commit}')
head_commit=$(git rev-parse 'HEAD^{commit}')
if [[ $head_commit != "$reviewed_commit" ]]; then
  echo "HEAD $head_commit is not the tip of origin/main $reviewed_commit" >&2
  exit 1
fi

if [[ $dry_run == true ]]; then
  echo "would create $TAG and $module_tag at $reviewed_commit"
  exit 0
fi

# Push the commit straight to both tag refs. Atomic, so the pair can never be
# half-created the way backend/v0.9.0-rc.1 was, and creating no local tags means
# a rejected push leaves nothing behind to clean up before a retry.
git push --atomic origin \
  "$reviewed_commit:refs/tags/$TAG" \
  "$reviewed_commit:refs/tags/$module_tag"
echo "created release tags $TAG and $module_tag at $reviewed_commit"

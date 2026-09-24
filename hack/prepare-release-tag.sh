#!/usr/bin/env bash
# Create and push a release tag pair after validating everything that can be
# checked before the tags exist. Tagging by hand is how this repository ended up
# with a version line that moved backward and a release whose module tag was
# never created; every check here exists because skipping it caused a real
# problem.
set -euo pipefail

: "${TAG:?TAG is required}"

dry_run=${PREPARE_DRY_RUN:-false}
review_only=${PREPARE_REVIEW_ONLY:-false}
case $dry_run in
  true | false) ;;
  *)
    echo "PREPARE_DRY_RUN must be true or false" >&2
    exit 1
    ;;
esac
case $review_only in
  true | false) ;;
  *)
    echo "PREPARE_REVIEW_ONLY must be true or false" >&2
    exit 1
    ;;
esac

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
# shellcheck source=hack/release-checks.sh
source "$root/hack/release-checks.sh"

module_tag="backend/$TAG"
source_branch=${RELEASE_SOURCE_BRANCH:-$(git symbolic-ref --quiet --short HEAD || true)}
head_commit=$(git rev-parse 'HEAD^{commit}')

check_release_tag_format "$TAG"
check_release_notes "$TAG"
check_version_moves_forward "$TAG" "$source_branch"
check_release_tags_available "$TAG"

# Reusing a version is unsafe once the module mirror is serving it: the mirror
# can keep returning the original content after the tag is gone, and the version
# is identified by a checksum that does not change, so a client that fetches
# rebuilt content fails verification against the recorded sum. Deleting the tags
# does not undo that, and the tag checks above cannot see a version whose tags
# were deleted.
# The cached-only endpoint answers from what the proxy already has, so this
# never triggers an origin fetch that would negatively cache the version we are
# about to create.
check_module_version_unused "$TAG"

if [[ $review_only == true ]]; then
  check_release_source_branch "$TAG" "$source_branch" "$head_commit" pr
  echo "validated release intent $TAG for $source_branch"
  exit 0
fi

# The notes checked above must be in the tagged commit, not only in the
# working tree. The workflow checkout is clean; local dry runs must be too.
if [[ -n $(git status --porcelain) ]]; then
  echo "release tagging requires a clean checkout with committed notes" >&2
  exit 1
fi
check_release_source_branch "$TAG" "$source_branch" "$head_commit" exact

if [[ $dry_run == true ]]; then
  echo "would create $TAG and $module_tag at $head_commit from $source_branch"
  exit 0
fi

# Push the commit straight to both tag refs. Atomic, so the pair can never be
# half-created the way backend/v0.9.0-rc.1 was, and creating no local tags means
# a rejected push leaves nothing behind to clean up before a retry.
git push --atomic origin \
  "$head_commit:refs/tags/$TAG" \
  "$head_commit:refs/tags/$module_tag"
echo "created release tags $TAG and $module_tag at $head_commit from $source_branch"

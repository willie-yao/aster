#!/usr/bin/env bash
# Release preconditions shared by hack/publish-release.sh, which runs after a
# tag is pushed, and hack/prepare-release-tag.sh, which runs before one exists.
# Every check is independent of whether the tag is already on the remote.

RELEASE_TAG_PATTERN='^v(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)[.](0|[1-9][0-9]*)(-(beta|rc)[.](0|[1-9][0-9]*))?$'

release_notes_path() {
  printf 'changelog/%s.md\n' "$1"
}

check_release_tag_format() {
  local tag=$1
  if [[ ! $tag =~ $RELEASE_TAG_PATTERN ]]; then
    echo "invalid release tag: $tag" >&2
    return 1
  fi
}

# The notes file must exist with real content, and must be reachable from the
# CHANGELOG.md index under this exact tag. A prose mention, a commented-out
# line, or an entry labelled with a different version leaves the release
# undiscoverable and does not count.
check_release_notes() {
  local tag=$1 notes pattern
  notes=$(release_notes_path "$tag")
  if [[ ! -s $notes ]] || ! grep -q '[^[:space:]]' "$notes"; then
    echo "missing release notes: $notes" >&2
    return 1
  fi
  pattern=${tag//./\\.}
  if ! grep -Eq "^- \[${pattern}\]\(changelog/${pattern}\.md\)" CHANGELOG.md; then
    echo "release notes are not indexed in CHANGELOG.md: $notes" >&2
    return 1
  fi
}

release_policy() {
  python3 "$(dirname "${BASH_SOURCE[0]}")/release_policy.py" "$@"
}

release_tags() {
  local tags
  if ! tags=$(git ls-remote --refs --tags origin 'refs/tags/v*' | sed 's|.*refs/tags/||'); then
    echo "failed to enumerate existing release tags" >&2
    return 1
  fi
  printf '%s\n' "$tags"
}

check_version_moves_forward() {
  local tag=$1 branch=${2:-main} tags
  tags=$(release_tags) || return 1
  printf '%s\n' "$tags" | release_policy check "$tag" --branch "$branch"
}

release_promotion_flags() {
  local tag=$1 tags
  tags=$(release_tags) || return 1
  printf '%s\n' "$tags" | release_policy promote "$tag"
}

release_branch_for_tag() {
  local tag=$1
  check_release_tag_format "$tag" || return 1
  [[ $tag =~ ^v([0-9]+)[.]([0-9]+)[.] ]] || return 1
  printf 'release/%s.%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
}

remote_release_branch_head() {
  local branch=$1 output sha fetched
  if ! output=$(git ls-remote --exit-code --refs --heads origin "refs/heads/$branch"); then
    echo "failed to inspect release source branch: $branch" >&2
    return 1
  fi
  sha=$(printf '%s\n' "$output" | awk -v ref="refs/heads/$branch" '$2 == ref {print $1}')
  if [[ ! $sha =~ ^[0-9a-f]{40}$ ]] ||
    ! git fetch --quiet --no-tags origin "refs/heads/$branch"; then
    echo "failed to fetch release source branch: $branch" >&2
    return 1
  fi
  fetched=$(git rev-parse 'FETCH_HEAD^{commit}') || return 1
  if [[ $fetched != "$sha" ]]; then
    echo "release source branch moved during inspection: $branch" >&2
    return 1
  fi
  printf '%s\n' "$fetched"
}

check_release_source_branch() {
  local tag=$1 branch=$2 commit=$3 mode=$4 anchor anchor_commit head major minor
  check_release_tag_format "$tag" || return 1
  case $mode in
    pr | exact | ancestor) ;;
    *)
      echo "invalid release source check: $mode" >&2
      return 1
      ;;
  esac
  if [[ $branch != main ]]; then
    if [[ ! $branch =~ ^release/(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)$ ]]; then
      echo "unsupported release source branch: $branch" >&2
      return 1
    fi
    major=${BASH_REMATCH[1]}
    minor=${BASH_REMATCH[2]}
    if [[ ! $tag =~ ^v(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)[.]([1-9][0-9]*)(-(beta|rc)[.](0|[1-9][0-9]*))?$ ]]; then
      echo "$tag is not a patch release on $branch" >&2
      return 1
    fi
    if [[ ${BASH_REMATCH[1]} != "$major" || ${BASH_REMATCH[2]} != "$minor" ]]; then
      echo "$tag is not a patch release on $branch" >&2
      return 1
    fi
    anchor="v${major}.${minor}.0"
    if ! git ls-remote --exit-code --refs --tags origin "refs/tags/$anchor" >/dev/null ||
      ! git fetch --quiet --no-tags origin "refs/tags/$anchor"; then
      echo "missing stable release anchor: $anchor" >&2
      return 1
    fi
    anchor_commit=$(git rev-parse 'FETCH_HEAD^{commit}') || return 1
    if ! git merge-base --is-ancestor "$anchor_commit" "$commit"; then
      echo "$commit does not descend from stable release $anchor" >&2
      return 1
    fi
  fi
  [[ $mode == pr ]] && return 0
  head=$(remote_release_branch_head "$branch") || return 1
  if [[ $mode == exact && $commit != "$head" ]]; then
    echo "HEAD $commit is not the tip of origin/$branch $head" >&2
    return 1
  fi
  if [[ $mode == ancestor ]] && ! git merge-base --is-ancestor "$commit" "$head"; then
    echo "$commit is not reachable from origin/$branch $head" >&2
    return 1
  fi
}

publication_source_branch() {
  local tag=$1 commit=$2 candidate status head
  candidate=$(release_branch_for_tag "$tag") || return 1
  if [[ $tag =~ ^v[0-9]+[.][0-9]+[.]([1-9][0-9]*)(-|$) ]]; then
    if git ls-remote --exit-code --refs --heads origin "refs/heads/$candidate" >/dev/null; then
      head=$(remote_release_branch_head "$candidate") || return 1
      if git merge-base --is-ancestor "$commit" "$head"; then
        printf '%s\n' "$candidate"
        return
      fi
    else
      status=$?
      if [[ $status -ne 2 ]]; then
        echo "failed to inspect release source branch: $candidate" >&2
        return 1
      fi
    fi
  fi
  printf 'main\n'
}

check_release_tags_available() {
  local tag=$1 candidate status
  for candidate in "$tag" "backend/$tag"; do
    if git ls-remote --exit-code --refs --tags origin "refs/tags/$candidate" >/dev/null; then
      echo "release tag already exists: $candidate" >&2
      return 1
    else
      status=$?
      if [[ $status -ne 2 ]]; then
        echo "failed to inspect release tag: $candidate" >&2
        return 1
      fi
    fi
  done
}

check_module_version_unused() {
  local tag=$1 module_path proxy_status
  module_path=$(awk '/^module /{print $2; exit}' backend/go.mod)
  proxy_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 \
    "https://proxy.golang.org/cached-only/${module_path}/@v/${tag}.info") || proxy_status=unreachable
  case $proxy_status in
    404) ;;
    200)
      echo "module version already published and immutable on the Go proxy: ${module_path}@${tag}" >&2
      echo "pick the next version; a deleted tag does not free its version number" >&2
      return 1
      ;;
    *)
      echo "failed to check the Go module proxy for ${module_path}@${tag} (status $proxy_status)" >&2
      return 1
      ;;
  esac
}

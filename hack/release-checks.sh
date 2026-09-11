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

# The release must be the newest version in the repository. Comparison is by
# semantic precedence, so a prerelease sorts below the release it leads to and
# beta sorts below rc. Tags that are not strict release tags, including the
# moving vMAJOR alias and backend/ module tags, are ignored.
check_version_moves_forward() {
  local tag=$1 existing
  if ! existing=$(git ls-remote --refs --tags origin 'refs/tags/v*' | sed 's|.*refs/tags/||'); then
    echo "failed to enumerate existing release tags" >&2
    return 1
  fi
  EXISTING_TAGS="$existing" python3 - "$tag" <<'PY_MONOTONIC'
import os
import re
import sys

RELEASE = re.compile(r"^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(beta|rc)\.(0|[1-9][0-9]*))?$")


def precedence(value):
    match = RELEASE.fullmatch(value)
    if not match:
        return None
    major, minor, patch, phase, number = match.groups()
    stage = (0, phase, int(number)) if phase else (1, "", 0)
    return (int(major), int(minor), int(patch), stage)


requested = sys.argv[1]
requested_precedence = precedence(requested)
newest = None
for line in os.environ.get("EXISTING_TAGS", "").splitlines():
    existing = line.strip()
    if not existing or existing == requested:
        continue
    parsed = precedence(existing)
    if parsed is None:
        continue
    if newest is None or parsed > newest[0]:
        newest = (parsed, existing)
if newest is not None and requested_precedence <= newest[0]:
    raise SystemExit(f"refusing to publish {requested}: {newest[1]} is already released")
PY_MONOTONIC
}

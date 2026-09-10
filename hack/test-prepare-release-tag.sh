#!/usr/bin/env bash
# Drives hack/prepare-release-tag.sh against a self-contained fixture repository
# so the real checkout and the real remote are never touched.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
real_git=$(command -v git)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/aster-prepare-tag-test.XXXXXX")
cleanup() {
  chmod -R u+w "$tmp" 2>/dev/null || true
  rm -rf "$tmp" 2>/dev/null || true
}
trap cleanup EXIT

# A fixture repository with its own remote, holding just the files the checks
# read plus copies of the scripts under test.
fixture=$tmp/work
"$real_git" init --bare --quiet "$tmp/remote.git"
"$real_git" init --quiet -b main "$fixture"
mkdir -p "$fixture/hack" "$fixture/changelog" "$fixture/backend" "$tmp/bin"
cp "$root/hack/prepare-release-tag.sh" "$root/hack/release-checks.sh" "$fixture/hack/"
# Run the fixture's copy: the script resolves its repository from its own path,
# so this is what keeps it away from the real checkout and the real remote.
script=$fixture/hack/prepare-release-tag.sh

# Stub git so tag enumeration still works against the real fixture remote while
# the exact-ref lookup can be forced to fail, which is the only way to reach the
# per-tag inspection-error branch.
cat > "$tmp/bin/git" <<GIT
#!/usr/bin/env bash
if [[ -n \${LS_REMOTE_EXIT:-} ]]; then
  for arg in "\$@"; do
    if [[ \$arg == --exit-code ]]; then
      exit "\$LS_REMOTE_EXIT"
    fi
  done
fi
exec "$real_git" "\$@"
GIT
chmod +x "$tmp/bin/git"

# Stub the Go module proxy probe so the suite stays offline and can assert the
# "already published", HTTP-error, and transport-failure branches.
cat > "$tmp/bin/curl" <<'CURL'
#!/usr/bin/env bash
if [[ -n ${CURL_EXIT:-} && ${CURL_EXIT} != 0 ]]; then
  exit "$CURL_EXIT"
fi
printf '%s' "${PROXY_STATUS:-404}"
CURL
chmod +x "$tmp/bin/curl"
export PATH="$tmp/bin:$PATH"

cd "$fixture"
"$real_git" config user.name 'Prepare Tag Test'
"$real_git" config user.email prepare-tag@example.test
"$real_git" config commit.gpgSign false
"$real_git" config tag.gpgSign false
"$real_git" remote add origin "$tmp/remote.git"

write_release() {
  local tag=$1
  printf 'Fixture notes for %s.\n' "$tag" > "changelog/$tag.md"
  printf -- '- [%s](changelog/%s.md) - fixture\n' "$tag" "$tag" >> CHANGELOG.md
}

printf '# Changelog\n\n## Releases\n\n' > CHANGELOG.md
printf 'module example.test/aster/backend\n\ngo 1.26.8\n' > backend/go.mod
write_release v1.0.0
write_release v1.1.0
write_release v0.9.0
write_release v1.0.0-rc.1
"$real_git" add -A
"$real_git" commit --quiet -m 'fixture'
"$real_git" push --quiet origin main

expect_failure() {
  local description=$1 expected=$2
  shift 2
  local before after output
  before=$("$real_git" ls-remote --refs --tags origin 2>/dev/null | sort || true)
  if output=$(env "$@" "$script" 2>&1); then
    echo "$description: expected failure, got success" >&2
    printf '%s\n' "$output" >&2
    exit 1
  fi
  if ! printf '%s\n' "$output" | grep -Fq "$expected"; then
    echo "$description: missing expected message '$expected'" >&2
    printf '%s\n' "$output" >&2
    exit 1
  fi
  after=$("$real_git" ls-remote --refs --tags origin 2>/dev/null | sort || true)
  if [[ $before != "$after" ]]; then
    echo "$description: remote tags changed despite the failure" >&2
    exit 1
  fi
}

expect_failure 'malformed version' 'invalid release tag: 1.0.0' TAG=1.0.0
expect_failure 'prerelease phase outside beta/rc' 'invalid release tag: v1.0.0-alpha.1' TAG=v1.0.0-alpha.1
expect_failure 'missing notes' 'missing release notes: changelog/v1.2.0.md' TAG=v1.2.0

printf '   \n' > "changelog/v1.2.0.md"
expect_failure 'whitespace-only notes' 'missing release notes: changelog/v1.2.0.md' TAG=v1.2.0
printf 'Real notes.\n' > "changelog/v1.2.0.md"
expect_failure 'unindexed notes' 'release notes are not indexed in CHANGELOG.md' TAG=v1.2.0
printf -- '<!-- - [v1.2.0](changelog/v1.2.0.md) -->\n' >> CHANGELOG.md
expect_failure 'commented-out index entry' 'release notes are not indexed in CHANGELOG.md' TAG=v1.2.0
printf -- '- [v1.2.1](changelog/v1.2.0.md)\n' >> CHANGELOG.md
expect_failure 'mislabelled index entry' 'release notes are not indexed in CHANGELOG.md' TAG=v1.2.0
rm -f "changelog/v1.2.0.md"
"$real_git" checkout --quiet -- CHANGELOG.md

# A version behind the newest release, and a prerelease of a version that
# already shipped, are both rejected before any tag exists.
"$real_git" tag v1.1.0 && "$real_git" push --quiet origin refs/tags/v1.1.0
expect_failure 'older line' 'refusing to publish v0.9.0: v1.1.0 is already released' TAG=v0.9.0
expect_failure 'prerelease after its stable' 'refusing to publish v1.0.0-rc.1: v1.1.0 is already released' TAG=v1.0.0-rc.1
# Re-tagging the newest version is caught by the existence check rather than by
# precedence, since a tag is never compared against itself.
expect_failure 'already released version' 'release tag already exists: v1.1.0' TAG=v1.1.0
"$real_git" push --quiet origin :refs/tags/v1.1.0
"$real_git" tag -d v1.1.0 > /dev/null

# An uncommitted or unpushed HEAD must not be tagged.
printf 'local only\n' > local.txt
"$real_git" add local.txt
"$real_git" commit --quiet -m 'unpushed'
expect_failure 'HEAD ahead of origin/main' 'is not the tip of origin/main' TAG=v1.1.0
"$real_git" reset --quiet --hard origin/main

# A version the module mirror is already serving must not be reused, even when
# its tags were deleted: the mirror can keep serving the original content.
expect_failure 'version already on the module proxy' 'already published and immutable on the Go proxy' \
  TAG=v1.1.0 PROXY_STATUS=200
# An error response and a failed request must both fail closed rather than be
# read as "this version is free".
expect_failure 'proxy error response' 'failed to check the Go module proxy' \
  TAG=v1.1.0 PROXY_STATUS=503
# 410 means "not available", not "never served", so it must fail closed too.
expect_failure 'proxy gone response' 'failed to check the Go module proxy' \
  TAG=v1.1.0 PROXY_STATUS=410
expect_failure 'proxy request failed' 'failed to check the Go module proxy' \
  TAG=v1.1.0 CURL_EXIT=7

# An unreadable remote must fail rather than be treated as "no tags exist". This
# surfaces from the tag enumeration, which is the first command to touch origin.
"$real_git" remote set-url origin "$tmp/does-not-exist.git"
expect_failure 'unreadable remote' 'failed to enumerate existing release tags' TAG=v1.1.0
"$real_git" remote set-url origin "$tmp/remote.git"

# An exact-ref lookup that fails for a reason other than "no match" must be
# reported as an inspection failure, never read as the tag being free. Status 2
# is the real "absent" answer and must still be accepted.
expect_failure 'exact tag lookup failed' 'failed to inspect release tag: v1.1.0' \
  TAG=v1.1.0 LS_REMOTE_EXIT=128

# The happy path validates, then creates both tags atomically at the reviewed tip.
TAG=v1.1.0 PREPARE_DRY_RUN=true "$script" | grep -Fq 'would create v1.1.0 and backend/v1.1.0'
if "$real_git" ls-remote --exit-code --refs --tags origin 'refs/tags/v*' > /dev/null 2>&1; then
  echo 'dry run created a tag' >&2
  exit 1
fi

TAG=v1.1.0 "$script" | grep -Fq 'created release tags v1.1.0 and backend/v1.1.0'
head_commit=$("$real_git" rev-parse 'HEAD^{commit}')
for tag in v1.1.0 backend/v1.1.0; do
  remote_commit=$("$real_git" ls-remote --refs --tags origin "refs/tags/$tag" | cut -f1)
  if [[ $remote_commit != "$head_commit" ]]; then
    echo "tag $tag does not identify the reviewed commit" >&2
    exit 1
  fi
done

# Re-running must not move or duplicate an existing pair. Reusing a version is
# unrecoverable because the Go module proxy keeps serving the original content.
output=$(TAG=v1.1.0 "$script" 2>&1) && { echo 'existing tag pair was re-created' >&2; exit 1; }
printf '%s\n' "$output" | grep -Fq 'release tag already exists: v1.1.0'

# A root tag that exists without its module tag is still refused here; recovery
# is hack/publish-release.sh with RELEASE_TAGS_ONLY=true.
"$real_git" push --quiet origin :refs/tags/backend/v1.1.0
output=$(TAG=v1.1.0 "$script" 2>&1) && { echo 'half-created pair was accepted' >&2; exit 1; }
printf '%s\n' "$output" | grep -Fq 'release tag already exists: v1.1.0'

grep -Fq 'hack/prepare-release-tag.sh' "$root/.github/workflows/release-tag.yml"
# Tag creation shares the publication group so a tag is never created while an
# earlier release is still publishing, and both need queue: max because the
# default cancels the existing pending run.
for workflow in release-tag release; do
  path=$root/.github/workflows/$workflow.yml
  grep -Fq 'group: release-publication' "$path"
  if ! grep -Fq 'queue: max' "$path"; then
    echo "$workflow.yml can cancel a pending release run; it needs queue: max" >&2
    exit 1
  fi
  if grep -Fq 'cancel-in-progress: true' "$path"; then
    echo "$workflow.yml combines queue: max with cancel-in-progress: true, which is rejected" >&2
    exit 1
  fi
done
# Tags pushed with the default GITHUB_TOKEN do not trigger the release and image
# workflows, so the checkout must use a separately minted token.
grep -Fq 'actions/create-github-app-token' "$root/.github/workflows/release-tag.yml"
grep -Fq 'token: ${{ steps.token.outputs.token }}' "$root/.github/workflows/release-tag.yml"
if grep -Fq 'secrets.GITHUB_TOKEN' "$root/.github/workflows/release-tag.yml"; then
  echo 'release tag workflow pushes with the default token, which publishes nothing' >&2
  exit 1
fi

echo 'Release tag preparation checks passed.'

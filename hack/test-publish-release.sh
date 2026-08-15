#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
script=$root/hack/publish-release.sh
real_git=$(command -v git)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/aster-release-test.XXXXXX")
cleanup() {
  find "$tmp" -type f -delete 2>/dev/null || true
  find "$tmp" -type l -delete 2>/dev/null || true
  find "$tmp" -depth -type d -exec rmdir {} \; 2>/dev/null || true
}
trap cleanup EXIT
mkdir -p "$tmp/bin"
log=$tmp/operations.log

cat > "$tmp/bin/helm" <<'HELM'
#!/usr/bin/env bash
set -euo pipefail
printf 'helm %s\n' "$*" >> "$RELEASE_TEST_LOG"
case ${1:-} in
  lint) exit 0 ;;
  package)
    chart=${2##*/}
    shift 2
    destination=.
    version=
    while (($#)); do
      case $1 in
        --destination) destination=$2; shift 2 ;;
        --version) version=$2; shift 2 ;;
        *) shift ;;
      esac
    done
    : > "$destination/$chart-$version.tgz"
    ;;
  push)
    if [[ ${FAIL_APPLICATION_PUSH:-false} == true && $2 == *aster-[0-9]* && $2 != *aster-platform-* ]]; then
      exit 42
    fi
    printf 'published %s\n' "$2" >> "$RELEASE_TEST_LOG"
    ;;
  *) exit 2 ;;
esac
HELM
cat > "$tmp/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
printf 'gh %s\n' "$*" >> "$RELEASE_TEST_LOG"
for arg in "$@"; do
  if [[ ${arg##*/} == SHA256SUMS ]]; then
    cp "$arg" "$RELEASE_TEST_SHA_COPY"
  fi
done
GH
cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
printf 'docker %s\n' "$*" >> "$RELEASE_TEST_LOG"
case ${1:-} in
  pull) exit 0 ;;
  image)
    case $* in
      *org.opencontainers.image.version*) printf '%s\n' "${TAG:-v1.2.3}" ;;
      *) printf '%s\n' "${HEAD_COMMIT:-2222222222222222222222222222222222222222}" ;;
    esac
    ;;
  *) exit 2 ;;
esac
DOCKER
cat > "$tmp/bin/go" <<'GO'
#!/usr/bin/env bash
set -euo pipefail
printf 'go %s\n' "$*" >> "$RELEASE_TEST_LOG"
if [[ ${1:-} == env && ${2:-} == GOVERSION ]]; then
  printf 'go1.25.12\n'
  exit 0
fi
if [[ ${1:-} != build ]]; then exit 2; fi
if [[ -n ${FAIL_CLI_TARGET:-} && ${GOOS:-}-${GOARCH:-} == "$FAIL_CLI_TARGET" ]]; then
  exit 43
fi
out=
while (($#)); do
  if [[ $1 == -o ]]; then out=$2; shift 2; else shift; fi
done
: "${out:?missing -o}"
: > "$out"
GO
cat > "$tmp/bin/git" <<'GIT'
#!/usr/bin/env bash
set -euo pipefail
printf 'git %s\n' "$*" >> "$RELEASE_TEST_LOG"
printf 'git-argv' >> "$RELEASE_TEST_LOG"
printf ' <%s>' "$@" >> "$RELEASE_TEST_LOG"
printf '\n' >> "$RELEASE_TEST_LOG"
case ${1:-} in
  ls-remote)
    ref=${*: -1}
    if [[ $ref == refs/tags/v1 ]]; then
      if [[ -n ${EXISTING_STABLE_VERSION:-} ]]; then
        printf '1111111111111111111111111111111111111111\trefs/tags/v1\n'
        exit 0
      fi
      exit 2
    fi
    if [[ $ref == refs/tags/backend/* ]]; then
      state=${MODULE_TAG_STATE:-present}
      if [[ -n ${MODULE_TAG_STATE_FILE:-} && -f $MODULE_TAG_STATE_FILE ]]; then
        state=present
      fi
      commit=${MODULE_TAG_COMMIT:-${HEAD_COMMIT:-2222222222222222222222222222222222222222}}
    else
      state=${ROOT_TAG_STATE:-present}
      commit=${ROOT_TAG_COMMIT:-${HEAD_COMMIT:-2222222222222222222222222222222222222222}}
    fi
    if [[ $state == missing ]]; then
      exit 2
    fi
    printf '%s\t%s\n' "$commit" "$ref"
    ;;
  fetch)
    printf '%s\n' "${*: -1}" > "$RELEASE_TEST_FETCHED_TAG"
    ;;
  archive)
    output=
    for arg in "$@"; do
      case $arg in --output=*) output=${arg#--output=} ;; esac
    done
    : "${output:?missing archive output}"
    : > "$output"
    ;;
  rev-parse)
    if [[ ${2:-} == 'HEAD^{commit}' ]]; then
      printf '%s\n' "${HEAD_COMMIT:-2222222222222222222222222222222222222222}"
    elif [[ ${2:-} == 'FETCH_HEAD^{commit}' ]]; then
      fetched=$(<"$RELEASE_TEST_FETCHED_TAG")
      if [[ $fetched == refs/tags/backend/* ]]; then
        printf '%s\n' "${MODULE_TAG_COMMIT:-${HEAD_COMMIT:-2222222222222222222222222222222222222222}}"
      else
        printf '%s\n' "${ROOT_TAG_COMMIT:-${HEAD_COMMIT:-2222222222222222222222222222222222222222}}"
      fi
    elif [[ ${2:-} == *'^{commit}' ]]; then
      if [[ ${2:-} == v1'^{commit}' ]]; then
        printf '1111111111111111111111111111111111111111\n'
      else
        printf '%s\n' "${LOCAL_MODULE_TAG_COMMIT:-${HEAD_COMMIT:-2222222222222222222222222222222222222222}}"
      fi
    fi
    ;;
  show-ref)
    [[ ${LOCAL_MODULE_TAG_STATE:-missing} == present ]]
    ;;
  tag)
    if [[ ${2:-} == --points-at ]]; then
      printf '%b\n' "${EXISTING_STABLE_VERSIONS:-${EXISTING_STABLE_VERSION:-}}"
    fi
    ;;
  push)
    if [[ ${3:-} == refs/tags/backend/* ]]; then
      if [[ ${FAIL_MODULE_TAG_PUSH:-false} == true ]]; then
        exit 44
      fi
      if [[ -n ${MODULE_TAG_STATE_FILE:-} ]]; then
        : > "$MODULE_TAG_STATE_FILE"
      fi
    fi
    ;;
esac
GIT
cat > "$tmp/contract.sh" <<'CONTRACT'
#!/usr/bin/env bash
set -euo pipefail
CONTRACT
chmod +x "$tmp/bin/helm" "$tmp/bin/gh" "$tmp/bin/docker" "$tmp/bin/go" "$tmp/bin/git" "$tmp/contract.sh"
export IMAGE_REPOSITORY=ghcr.io/example/aster
export FIX_IMAGE_CONTRACT_SCRIPT="$tmp/contract.sh"
export RELEASE_TEST_SHA_COPY="$tmp/release.SHA256SUMS"
export RELEASE_TEST_FETCHED_TAG="$tmp/fetched-tag"
export MODULE_TAG_STATE_FILE="$tmp/module-tag-created"

if (cd "$root" && RELEASE_TEST_LOG="$log" ROOT_TAG_STATE=missing MODULE_TAG_STATE=missing PATH="$tmp/bin:$PATH" TAG=v1.2.3 REPOSITORY_OWNER=example "$script") >"$tmp/missing-tag.out" 2>&1; then
  echo 'missing remote release tag was accepted' >&2
  exit 1
fi
grep -Fq 'remote release tag does not exist' "$tmp/missing-tag.out"
if grep -Eq '^(helm push|gh release create|git tag -f|git push origin)' "$log"; then
  echo 'missing release tag performed publication side effects' >&2
  exit 1
fi

: > "$log"
if (cd "$root" && RELEASE_TEST_LOG="$log" ROOT_TAG_COMMIT=3333333333333333333333333333333333333333 PATH="$tmp/bin:$PATH" TAG=v1.2.3 REPOSITORY_OWNER=example "$script") >"$tmp/moved-tag.out" 2>&1; then
  echo 'moved remote release tag was accepted' >&2
  exit 1
fi
grep -Fq 'does not identify reviewed HEAD' "$tmp/moved-tag.out"
if grep -Eq '^(helm push|gh release create|git tag -f|git push origin)' "$log"; then
  echo 'moved release tag performed publication side effects' >&2
  exit 1
fi

: > "$log"
if (cd "$root" && RELEASE_TEST_LOG="$log" MODULE_TAG_COMMIT=3333333333333333333333333333333333333333 PATH="$tmp/bin:$PATH" TAG=v1.2.3 REPOSITORY_OWNER=example "$script") >"$tmp/mismatched-module.out" 2>&1; then
  echo 'mismatched module tag was accepted' >&2
  exit 1
fi
grep -Fq 'remote module tag backend/v1.2.3 does not identify reviewed HEAD' "$tmp/mismatched-module.out"
if grep -Eq '^(helm push|gh release create|git tag|git push origin)' "$log"; then
  echo 'mismatched module tag performed publication side effects' >&2
  exit 1
fi

: > "$log"
if (cd "$root" && RELEASE_TEST_LOG="$log" ROOT_TAG_STATE=missing MODULE_TAG_STATE=present PATH="$tmp/bin:$PATH" TAG=v1.2.3 REPOSITORY_OWNER=example "$script") >"$tmp/module-only.out" 2>&1; then
  echo 'module-only tag state was accepted' >&2
  exit 1
fi
grep -Fq 'module tag exists without root release tag' "$tmp/module-only.out"

: > "$log"
if (cd "$root" && RELEASE_TEST_LOG="$log" PATH="$tmp/bin:$PATH" TAG=1.2.3 RELEASE_TAGS_ONLY=true "$script") >"$tmp/invalid-version.out" 2>&1; then
  echo 'invalid release version was accepted' >&2
  exit 1
fi
grep -Fq 'invalid release tag: 1.2.3' "$tmp/invalid-version.out"
if [[ -s $log ]]; then
  echo 'invalid release version invoked git' >&2
  exit 1
fi

: > "$log"
(cd "$root" && RELEASE_TEST_LOG="$log" PATH="$tmp/bin:$PATH" TAG=v1.2.3 RELEASE_TAGS_ONLY=true "$script") >"$tmp/existing-pair.out"
grep -Fq 'release tags v1.2.3 and backend/v1.2.3 identify reviewed HEAD' "$tmp/existing-pair.out"
if grep -Eq '^git (tag backend/|push origin refs/tags/backend/)' "$log"; then
  echo 'existing correct paired tags were changed' >&2
  exit 1
fi

: > "$log"
rm -f "$MODULE_TAG_STATE_FILE"
(cd "$root" && RELEASE_TEST_LOG="$log" MODULE_TAG_STATE=missing PATH="$tmp/bin:$PATH" TAG=v1.2.3 RELEASE_TAGS_ONLY=true "$script") >"$tmp/root-only.out"
grep -Fq 'created module tag backend/v1.2.3' "$tmp/root-only.out"
grep -Fq 'git -c tag.gpgSign=false tag backend/v1.2.3 2222222222222222222222222222222222222222' "$log"
grep -Fq 'git-argv <push> <origin> <refs/tags/backend/v1.2.3:refs/tags/backend/v1.2.3>' "$log"

: > "$log"
rm -f "$MODULE_TAG_STATE_FILE"
(cd "$root" && RELEASE_TEST_LOG="$log" MODULE_TAG_STATE=missing PATH="$tmp/bin:$PATH" TAG=v0.9.0-rc.2 RELEASE_DRY_RUN=true "$script") >"$tmp/rc-dry-run.out"
grep -Fq 'would create module tag backend/v0.9.0-rc.2' "$tmp/rc-dry-run.out"
if grep -Eq '^(helm |gh |go build|git tag|git push)' "$log"; then
  echo 'RC dry run performed publication side effects' >&2
  exit 1
fi

: > "$log"
(cd "$root" && RELEASE_TEST_LOG="$log" MODULE_TAG_STATE=missing PATH="$tmp/bin:$PATH" TAG=v1.2.3 RELEASE_DRY_RUN=true "$script") >"$tmp/stable-dry-run.out"
grep -Fq 'would create module tag backend/v1.2.3' "$tmp/stable-dry-run.out"

: > "$log"
if (cd "$root" && RELEASE_TEST_LOG="$log" FAIL_CLI_TARGET=darwin-arm64 PATH="$tmp/bin:$PATH" TAG=v1.2.3 REPOSITORY_OWNER=example "$script") >"$tmp/cli-build.out" 2>&1; then
  echo 'failed CLI asset build was accepted' >&2
  exit 1
fi
if grep -Eq '^(helm push|gh release create|git tag -f|git push origin)' "$log"; then
  echo 'CLI build failure performed publication side effects' >&2
  exit 1
fi

: > "$log"
if (cd "$root" && RELEASE_TEST_LOG="$log" FAIL_APPLICATION_PUSH=true PATH="$tmp/bin:$PATH" TAG=v1.2.3 REPOSITORY_OWNER=example "$script"); then
  echo 'application chart push failure was accepted' >&2
  exit 1
fi
if grep -Eq '^(gh|git push|git tag -f) ' "$log"; then
  echo 'release or alias was published after an application chart push failure' >&2
  exit 1
fi
if grep -E '^published .*aster-[0-9]' "$log" | grep -vq 'aster-platform-'; then
  echo 'application chart became available without its platform prerequisite' >&2
  exit 1
fi

: > "$log"
if (cd "$root" && RELEASE_TEST_LOG="$log" EXISTING_STABLE_VERSION=v1.3.0 PATH="$tmp/bin:$PATH" TAG=v1.2.3 REPOSITORY_OWNER=example "$script") >"$tmp/backward.out" 2>&1; then
  echo 'stable alias rollback was accepted' >&2
  exit 1
fi
grep -Fq 'refusing to move stable alias backward' "$tmp/backward.out"
if grep -Eq '^(helm push|gh release create|git tag -f|git push origin)' "$log"; then
  echo 'backward stable release performed publication side effects' >&2
  exit 1
fi

: > "$log"
if (cd "$root" && RELEASE_TEST_LOG="$log" EXISTING_STABLE_VERSION=v1.10.0 EXISTING_STABLE_VERSIONS='v1.9.0\nv1.10.0' PATH="$tmp/bin:$PATH" TAG=v1.9.5 REPOSITORY_OWNER=example "$script") >"$tmp/lexical-backward.out" 2>&1; then
  echo 'lexicographic stable alias rollback was accepted' >&2
  exit 1
fi
grep -Fq 'refusing to move stable alias backward from v1.10.0 to v1.9.5' "$tmp/lexical-backward.out"

: > "$log"
(cd "$root" && RELEASE_TEST_LOG="$log" PATH="$tmp/bin:$PATH" TAG=v1.2.3 REPOSITORY_OWNER=example "$script")
python3 - "$log" "$RELEASE_TEST_SHA_COPY" <<'PY'
from pathlib import Path
import sys
lines = Path(sys.argv[1]).read_text().splitlines()
app_push = next(i for i, line in enumerate(lines) if line.startswith('helm push ') and 'aster-1.2.3.tgz' in line and 'platform' not in line)
platform_push = next(i for i, line in enumerate(lines) if line.startswith('helm push ') and 'aster-platform-1.2.3.tgz' in line)
release = next(i for i, line in enumerate(lines) if line.startswith('gh release create '))
alias = next(i for i, line in enumerate(lines) if line.startswith('git push origin '))
assert platform_push < app_push < release < alias, lines
release_line = lines[release]
assert 'aster-1.2.3.tgz' in release_line
assert 'aster-platform-1.2.3.tgz' in release_line
assert '--verify-tag' in release_line
assert 'SHA256SUMS' in release_line
expected = [
    'aster-1.2.3.tgz',
    'aster-platform-1.2.3.tgz',
    'aster-v1.2.3-source.tar.gz',
    'aster-v1.2.3-release-manifest.json',
    'SHA256SUMS',
    *(f'aster-v1.2.3-{target}' for target in ('linux-amd64', 'linux-arm64', 'darwin-amd64', 'darwin-arm64')),
]
for asset in expected:
    assert release_line.split().count(next(value for value in release_line.split() if value.endswith('/' + asset) or value == asset)) == 1, (asset, release_line)
assert sum(1 for line in lines if line.startswith('go build ')) == 4
checksum_lines = Path(sys.argv[2]).read_text().splitlines()
checksum_names = [line.split(None, 1)[1] for line in checksum_lines]
expected_checksums = [asset for asset in expected if asset != 'SHA256SUMS']
assert len(checksum_names) == 8, checksum_names
assert len(checksum_names) == len(set(checksum_names)), checksum_names
assert sorted(checksum_names) == sorted(expected_checksums), (checksum_names, expected_checksums)
PY

: > "$log"
(cd "$root" && RELEASE_TEST_LOG="$log" PATH="$tmp/bin:$PATH" TAG=v1.2.3-rc.1 REPOSITORY_OWNER=example "$script")
grep -F 'gh release create ' "$log" | grep -Fq -- '--prerelease'
if grep -Fq 'git push origin' "$log"; then
  echo 'pre-release moved the stable major alias' >&2
  exit 1
fi

for workflow in "$root/.github/workflows/release.yml" "$root/.github/workflows/image.yml"; do
  grep -Fq -- '- "v*.*.*"' "$workflow"
  if grep -Fq 'backend/v*.*.*' "$workflow"; then
    echo "module tag triggers duplicate workflow publication: $workflow" >&2
    exit 1
  fi
done

fixture=$tmp/tag-fixture
mkdir -p "$fixture"
"$real_git" init --bare --quiet "$fixture/remote.git"
"$real_git" init --quiet "$fixture/work"
(
  cd "$fixture/work"
  "$real_git" config user.name 'Release Test'
  "$real_git" config user.email release-test@example.test
  "$real_git" config commit.gpgSign false
  "$real_git" config tag.gpgSign false
  printf 'fixture\n' > fixture.txt
  "$real_git" add fixture.txt
  "$real_git" commit --quiet -m fixture
  "$real_git" remote add origin "$fixture/remote.git"
  "$real_git" tag v0.9.0-rc.2
  "$real_git" push --quiet origin refs/tags/v0.9.0-rc.2
  PATH="$(dirname "$real_git"):/usr/bin:/bin" TAG=v0.9.0-rc.2 RELEASE_TAGS_ONLY=true "$script" > "$tmp/fixture-recovery.out"
  head_commit=$("$real_git" rev-parse HEAD)
  root_commit=$("$real_git" ls-remote --refs --tags origin refs/tags/v0.9.0-rc.2 | cut -f1)
  module_commit=$("$real_git" ls-remote --refs --tags origin refs/tags/backend/v0.9.0-rc.2 | cut -f1)
  [[ $root_commit == "$head_commit" ]]
  [[ $module_commit == "$head_commit" ]]
)
grep -Fq 'created module tag backend/v0.9.0-rc.2' "$tmp/fixture-recovery.out"

echo 'Release publication ordering checks passed.'

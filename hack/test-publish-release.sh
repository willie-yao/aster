#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
script=$root/hack/publish-release.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/aster-release-test.XXXXXX")
cleanup() {
  find "$tmp" -type f -delete 2>/dev/null || true
  rmdir "$tmp/bin" "$tmp" 2>/dev/null || true
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
    if [[ ${REMOTE_TAG_STATE:-present} == missing ]]; then
      exit 2
    fi
    printf '2222222222222222222222222222222222222222\t%s\n' "$ref"
    ;;
  fetch) exit 0 ;;
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
    elif [[ ${2:-} == *'^{commit}' ]]; then
      if [[ ${2:-} == v1'^{commit}' ]]; then
        printf '1111111111111111111111111111111111111111\n'
      else
        printf '%s\n' "${REMOTE_TAG_COMMIT:-2222222222222222222222222222222222222222}"
      fi
    fi
    ;;
  tag)
    if [[ ${2:-} == --points-at ]]; then
      printf '%b\n' "${EXISTING_STABLE_VERSIONS:-${EXISTING_STABLE_VERSION:-}}"
    fi
    ;;
  push) exit 0 ;;
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

if (cd "$root" && RELEASE_TEST_LOG="$log" REMOTE_TAG_STATE=missing PATH="$tmp/bin:$PATH" TAG=v1.2.3 REPOSITORY_OWNER=example "$script") >"$tmp/missing-tag.out" 2>&1; then
  echo 'missing remote release tag was accepted' >&2
  exit 1
fi
grep -Fq 'remote release tag does not exist' "$tmp/missing-tag.out"
if grep -Eq '^(helm push|gh release create|git tag -f|git push origin)' "$log"; then
  echo 'missing release tag performed publication side effects' >&2
  exit 1
fi

: > "$log"
if (cd "$root" && RELEASE_TEST_LOG="$log" REMOTE_TAG_COMMIT=3333333333333333333333333333333333333333 PATH="$tmp/bin:$PATH" TAG=v1.2.3 REPOSITORY_OWNER=example "$script") >"$tmp/moved-tag.out" 2>&1; then
  echo 'moved remote release tag was accepted' >&2
  exit 1
fi
grep -Fq 'does not identify reviewed HEAD' "$tmp/moved-tag.out"
if grep -Eq '^(helm push|gh release create|git tag -f|git push origin)' "$log"; then
  echo 'moved release tag performed publication side effects' >&2
  exit 1
fi

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

echo 'Release publication ordering checks passed.'

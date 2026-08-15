#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
script=$root/hack/verify-release-images.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/aster-release-images.XXXXXX")
cleanup() {
  find "$tmp" -type f -delete 2>/dev/null || true
  rmdir "$tmp/bin" "$tmp" 2>/dev/null || true
}
trap cleanup EXIT
mkdir -p "$tmp/bin"
log=$tmp/docker.log
contract_log=$tmp/contract.log
cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$IMAGE_TEST_LOG"
case ${1:-} in
  pull)
    image=${*: -1}
    if [[ -n ${FAIL_IMAGE_SUFFIX:-} && $image == *"$FAIL_IMAGE_SUFFIX"* ]]; then
      exit 1
    fi
    ;;
  image)
    [[ ${2:-} == inspect ]]
    case $* in
      *org.opencontainers.image.version*) printf '%s\n' "${IMAGE_VERSION:-v1.2.3}" ;;
      *) printf '%s\n' "${IMAGE_REVISION:-reviewed-commit}" ;;
    esac
    ;;
  *) exit 2 ;;
esac
DOCKER
cat > "$tmp/contract.sh" <<'CONTRACT'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$IMAGE_TEST_CONTRACT_LOG"
CONTRACT
chmod +x "$tmp/bin/docker" "$tmp/contract.sh"

common_env=(
  IMAGE_TEST_LOG="$log"
  IMAGE_TEST_CONTRACT_LOG="$contract_log"
  FIX_IMAGE_CONTRACT_SCRIPT="$tmp/contract.sh"
  PATH="$tmp/bin:$PATH"
  TAG=v1.2.3
  IMAGE_REPOSITORY=ghcr.io/example/aster
  REVIEWED_COMMIT=reviewed-commit
  IMAGE_WAIT_ATTEMPTS=1
  IMAGE_WAIT_DELAY_SECONDS=0
)

env "${common_env[@]}" "$script" >"$tmp/success.out"
[[ $(grep -Fc 'release_image=verified' "$tmp/success.out") == 3 ]]
grep -Fq 'ghcr.io/example/aster/agent-sandbox-fix-executor:v1.2.3 v1.2.3 reviewed-commit v1.2.3' "$contract_log"

if env "${common_env[@]}" IMAGE_REVISION=wrong "$script" >"$tmp/revision.out" 2>&1; then
  echo 'wrong image revision was accepted' >&2
  exit 1
fi
grep -Fq 'release image revision mismatch' "$tmp/revision.out"

if env "${common_env[@]}" IMAGE_VERSION=v1.2.2 "$script" >"$tmp/version.out" 2>&1; then
  echo 'wrong image version was accepted' >&2
  exit 1
fi
grep -Fq 'release image version mismatch' "$tmp/version.out"

if env "${common_env[@]}" FAIL_IMAGE_SUFFIX=remote-fixer IMAGE_WAIT_ATTEMPTS=2 "$script" >"$tmp/missing.out" 2>&1; then
  echo 'missing release image was accepted' >&2
  exit 1
fi
grep -Fq 'did not become available' "$tmp/missing.out"

echo 'Release image availability checks passed.'

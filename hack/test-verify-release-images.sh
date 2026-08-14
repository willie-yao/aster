#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
script=$root/hack/verify-release-images.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/prow-release-images.XXXXXX")
cleanup() {
  find "$tmp" -type f -delete 2>/dev/null || true
  rmdir "$tmp/bin" "$tmp" 2>/dev/null || true
}
trap cleanup EXIT
mkdir -p "$tmp/bin"
log=$tmp/docker.log
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
    printf '%s\n' "${IMAGE_REVISION:-reviewed-commit}"
    ;;
  *) exit 2 ;;
esac
DOCKER
chmod +x "$tmp/bin/docker"

IMAGE_TEST_LOG="$log" PATH="$tmp/bin:$PATH" \
  TAG=v1.2.3 IMAGE_REPOSITORY=ghcr.io/example/prow-ai-dashboard \
  REVIEWED_COMMIT=reviewed-commit IMAGE_WAIT_ATTEMPTS=1 IMAGE_WAIT_DELAY_SECONDS=0 \
  "$script" >"$tmp/success.out"
[[ $(grep -Fc 'release_image=verified' "$tmp/success.out") == 3 ]]

if IMAGE_TEST_LOG="$log" PATH="$tmp/bin:$PATH" \
  TAG=v1.2.3 IMAGE_REPOSITORY=ghcr.io/example/prow-ai-dashboard \
  REVIEWED_COMMIT=reviewed-commit IMAGE_REVISION=wrong \
  IMAGE_WAIT_ATTEMPTS=1 IMAGE_WAIT_DELAY_SECONDS=0 \
  "$script" >"$tmp/revision.out" 2>&1; then
  echo 'wrong image revision was accepted' >&2
  exit 1
fi
grep -Fq 'release image revision mismatch' "$tmp/revision.out"

if IMAGE_TEST_LOG="$log" PATH="$tmp/bin:$PATH" \
  TAG=v1.2.3 IMAGE_REPOSITORY=ghcr.io/example/prow-ai-dashboard \
  REVIEWED_COMMIT=reviewed-commit FAIL_IMAGE_SUFFIX=remote-fixer \
  IMAGE_WAIT_ATTEMPTS=2 IMAGE_WAIT_DELAY_SECONDS=0 \
  "$script" >"$tmp/missing.out" 2>&1; then
  echo 'missing release image was accepted' >&2
  exit 1
fi
grep -Fq 'did not become available' "$tmp/missing.out"

echo 'Release image availability checks passed.'

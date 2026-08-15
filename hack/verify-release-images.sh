#!/usr/bin/env bash
set -euo pipefail

: "${TAG:?TAG is required}"
: "${IMAGE_REPOSITORY:?IMAGE_REPOSITORY is required}"
: "${REVIEWED_COMMIT:?REVIEWED_COMMIT is required}"

fix_image_contract=${FIX_IMAGE_CONTRACT_SCRIPT:-hack/test-agent-sandbox-fix-image.sh}
attempts=${IMAGE_WAIT_ATTEMPTS:-80}
delay=${IMAGE_WAIT_DELAY_SECONDS:-15}
images=(
  "$IMAGE_REPOSITORY:$TAG"
  "$IMAGE_REPOSITORY/remote-fixer:$TAG"
  "$IMAGE_REPOSITORY/agent-sandbox-fix-executor:$TAG"
)

for image in "${images[@]}"; do
  available=false
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if docker pull --platform linux/amd64 --quiet "$image" >/dev/null 2>&1; then
      revision=$(docker image inspect \
        --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' \
        "$image")
      version=$(docker image inspect \
        --format '{{ index .Config.Labels "org.opencontainers.image.version" }}' \
        "$image")
      if [[ $revision != "$REVIEWED_COMMIT" ]]; then
        printf 'release image revision mismatch\nimage=%s\nexpected=%s\nactual=%s\n' \
          "$image" "$REVIEWED_COMMIT" "$revision" >&2
        exit 1
      fi
      if [[ $version != "$TAG" ]]; then
        printf 'release image version mismatch\nimage=%s\nexpected=%s\nactual=%s\n' \
          "$image" "$TAG" "$version" >&2
        exit 1
      fi
      available=true
      printf 'release_image=verified image=%s revision=%s version=%s\n' "$image" "$revision" "$version"
      break
    fi
    if ((attempt < attempts)); then
      sleep "$delay"
    fi
  done
  if [[ $available != true ]]; then
    printf 'release image did not become available: %s\n' "$image" >&2
    exit 1
  fi
done

"$fix_image_contract" \
  "$IMAGE_REPOSITORY/agent-sandbox-fix-executor:$TAG" \
  "$TAG" \
  "$REVIEWED_COMMIT" \
  "$TAG"

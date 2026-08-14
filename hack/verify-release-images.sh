#!/usr/bin/env bash
set -euo pipefail

: "${TAG:?TAG is required}"
: "${IMAGE_REPOSITORY:?IMAGE_REPOSITORY is required}"
: "${REVIEWED_COMMIT:?REVIEWED_COMMIT is required}"

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
      if [[ $revision != "$REVIEWED_COMMIT" ]]; then
        printf 'release image revision mismatch\nimage=%s\nexpected=%s\nactual=%s\n' \
          "$image" "$REVIEWED_COMMIT" "$revision" >&2
        exit 1
      fi
      available=true
      printf 'release_image=verified image=%s revision=%s\n' "$image" "$revision"
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

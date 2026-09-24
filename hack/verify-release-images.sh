#!/usr/bin/env bash
set -euo pipefail

: "${TAG:?TAG is required}"
: "${IMAGE_REPOSITORY:?IMAGE_REPOSITORY is required}"
: "${REVIEWED_COMMIT:?REVIEWED_COMMIT is required}"

fix_image_contract=${FIX_IMAGE_CONTRACT_SCRIPT:-hack/test-agent-sandbox-fix-image.sh}
attempts=${IMAGE_WAIT_ATTEMPTS:-80}
delay=${IMAGE_WAIT_DELAY_SECONDS:-15}
repositories=(
  "$IMAGE_REPOSITORY"
  "$IMAGE_REPOSITORY/remote-fixer"
  "$IMAGE_REPOSITORY/agent-sandbox-fix-executor"
)
verified=()
executor_image=""

for repository in "${repositories[@]}"; do
  image="$repository:$TAG"
  available=false
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if digest=$(docker buildx imagetools inspect "$image" --format '{{.Manifest.Digest}}' 2>/dev/null); then
      if [[ ! $digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
        echo "invalid release image digest: $image" >&2
        exit 1
      fi
      pinned_image="$repository@$digest"
    else
      pinned_image=""
    fi
    if [[ -n $pinned_image ]] && docker pull --platform linux/amd64 --quiet "$pinned_image" >/dev/null 2>&1; then
      revision=$(docker image inspect \
        --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' \
        "$pinned_image")
      version=$(docker image inspect \
        --format '{{ index .Config.Labels "org.opencontainers.image.version" }}' \
        "$pinned_image")
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
      verified+=("$repository $digest")
      if [[ $repository == "$IMAGE_REPOSITORY/agent-sandbox-fix-executor" ]]; then
        executor_image=$pinned_image
      fi
      printf 'release_image=verified image=%s digest=%s revision=%s version=%s\n' "$image" "$digest" "$revision" "$version"
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
  "$executor_image" \
  "$TAG" \
  "$REVIEWED_COMMIT" \
  "$TAG"

if [[ -n ${RELEASE_IMAGE_DIGESTS_OUT:-} ]]; then
  printf '%s\n' "${verified[@]}" > "$RELEASE_IMAGE_DIGESTS_OUT"
fi

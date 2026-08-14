#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/aster-cli-assets.XXXXXX")
cleanup() {
  find "$tmp" -type f -delete 2>/dev/null || true
  rmdir "$tmp" 2>/dev/null || true
}
trap cleanup EXIT

version=v0.0.0-cleanroom
commit=$(git -C "$root" rev-parse HEAD)
for target in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  goos=${target%-*}
  goarch=${target#*-}
  asset="aster-${version}-${target}"
  (
    cd "$root/backend"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
      -trimpath \
      -ldflags "-s -w -X main.version=$version -X main.commit=$commit -X main.imageTag=$version" \
      -o "$tmp/$asset" \
      ./cmd/aster
  )
  [[ -s "$tmp/$asset" ]]
done
(
  cd "$tmp"
  shasum -a 256 aster-${version}-* > SHA256SUMS
  [[ $(wc -l < SHA256SUMS | tr -d ' ') == 4 ]]
  shasum -a 256 --check SHA256SUMS
  asset=aster-${version}-darwin-arm64
  checksum_line=$(awk -v asset="$asset" '$2 == asset {print}' SHA256SUMS)
  test -n "$checksum_line"
  if command -v sha256sum >/dev/null && \
    printf '%s\n' "$checksum_line" | sha256sum -c - 2>/dev/null; then
    :
  else
    printf '%s\n' "$checksum_line" | shasum -a 256 --check
  fi
)

echo 'Release CLI asset build checks passed.'

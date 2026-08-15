#!/usr/bin/env bash
set -euo pipefail
simulate_install() (
  set -euo pipefail
  phase=$1
  download=$(mktemp -d "${TMPDIR:-/tmp}/cli-fail.XXXXXX")
  dest=$download/dest
  mkdir -p "$dest"
  printf old > "$dest/cli"
  # shellcheck disable=SC2329 # invoked by the EXIT trap
  cleanup() { find "$download" -type f -delete 2>/dev/null || true; find "$download" -depth -type d -empty -delete 2>/dev/null || true; }
  trap cleanup EXIT
  case $phase in
    corrupt|missing-entry|missing-tool|download-failure) false ;;
  esac
  install -m 0755 payload "$dest/cli"
)
for phase in corrupt missing-entry missing-tool download-failure; do
  if simulate_install "$phase"; then
    echo "CLI verification failure was masked: $phase" >&2
    exit 1
  fi
done
echo 'CLI download fail-closed checks passed.'

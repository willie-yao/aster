#!/usr/bin/env bash
set -euo pipefail

simulate_verification() (
  set -euo pipefail
  phase=$1
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/prow-verification-failure.XXXXXX")
  # shellcheck disable=SC2329 # invoked by the EXIT trap
  cleanup() {
    printf 'cleanup\n' > "$tmp/cleanup"
    find "$tmp" -type f -delete 2>/dev/null || true
    rmdir "$tmp" 2>/dev/null || true
  }
  trap cleanup EXIT

  case $phase in
    manifest-timeout) false ;;
    missing-job) false ;;
    private-not-404) false ;;
    public-origin) false ;;
    sandbox-read) false ;;
    *) return 2 ;;
  esac
)

for phase in manifest-timeout missing-job private-not-404 public-origin sandbox-read; do
  if simulate_verification "$phase"; then
    echo "verification failure was masked by cleanup: $phase" >&2
    exit 1
  fi
done

echo 'Kubernetes verification fail-closed checks passed.'

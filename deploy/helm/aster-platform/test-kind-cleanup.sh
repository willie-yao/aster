#!/usr/bin/env bash
set -euo pipefail

chart=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/prow-ai-dashboard-platform-kind-cleanup.XXXXXX")
trap 'find "$tmp" -type f -delete 2>/dev/null || true; rmdir "$tmp/bin" "$tmp" 2>/dev/null || true' EXIT
mkdir -p "$tmp/bin"
log=$tmp/kind.log
cat > "$tmp/bin/kind" <<EOF_KIND
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$*" >> "$log"
if [[ \${1:-} == get && \${2:-} == clusters ]]; then
  printf 'existing-platform-cluster\n'
  exit 0
fi
exit 99
EOF_KIND
chmod +x "$tmp/bin/kind"

if PLATFORM_KIND_CLUSTER_NAME=existing-platform-cluster PATH="$tmp/bin:$PATH" "$chart/test-kind.sh" >"$tmp/output" 2>&1; then
  echo 'pre-existing kind cluster was accepted' >&2
  exit 1
fi
grep -Fq 'kind cluster already exists' "$tmp/output"
if grep -Fq 'delete cluster' "$log"; then
  echo 'cleanup attempted to delete a cluster it did not create' >&2
  exit 1
fi

echo 'Platform kind cleanup ownership checks passed.'

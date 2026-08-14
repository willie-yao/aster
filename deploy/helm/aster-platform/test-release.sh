#!/usr/bin/env bash
set -euo pipefail

chart=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script=$chart/verify-agent-sandbox-release.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/prow-ai-dashboard-platform-release.XXXXXX")
trap 'find "$tmp" -type f -delete 2>/dev/null || true; rmdir "$tmp/bin" "$tmp" 2>/dev/null || true' EXIT

contract=$($script --print-contract)
grep -Fxq 'version=v0.5.3' <<<"$contract"
grep -Fxq 'url=https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.5.3/sandbox.yaml' <<<"$contract"
grep -Fxq 'sha256=50f54b0e746376455ae6bb8b90b436bdd8798e1296cff0d72b6267bbeb858e3c' <<<"$contract"

printf 'not the release manifest\n' > "$tmp/bad.yaml"
if $script "$tmp/bad.yaml" >"$tmp/mismatch.out" 2>&1; then
  echo 'checksum mismatch was accepted' >&2
  exit 1
fi
grep -Fq 'checksum mismatch' "$tmp/mismatch.out"

mkdir -p "$tmp/bin"
cat > "$tmp/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
out=
while (($#)); do
  if [[ $1 == --output ]]; then out=$2; shift 2; else shift; fi
done
printf 'tampered download\n' > "$out"
CURL
chmod +x "$tmp/bin/curl"
if PATH="$tmp/bin:$PATH" $script --output "$tmp/downloaded.yaml" >"$tmp/download.out" 2>&1; then
  echo 'tampered download was accepted' >&2
  exit 1
fi
if [[ -e "$tmp/downloaded.yaml" ]]; then
  echo 'checksum failure retained the requested output' >&2
  exit 1
fi
grep -Fq 'checksum mismatch' "$tmp/download.out"

echo 'Agent Sandbox release contract checks passed.'

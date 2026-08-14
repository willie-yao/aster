#!/usr/bin/env bash
set -euo pipefail

version=v0.5.3
url=https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.5.3/sandbox.yaml
sha256=50f54b0e746376455ae6bb8b90b436bdd8798e1296cff0d72b6267bbeb858e3c

usage() {
  cat <<USAGE
Usage:
  $0 --print-contract
  $0 <pre-downloaded-sandbox.yaml>
  $0 --output <new-path>

The script verifies the official Agent Sandbox $version core manifest. It never
executes or applies the downloaded content.
USAGE
}

print_contract() {
  printf 'version=%s\nurl=%s\nsha256=%s\n' "$version" "$url" "$sha256"
}

verify_file() {
  local path=$1
  if [[ ! -f "$path" ]]; then
    printf 'manifest is not a regular file: %s\n' "$path" >&2
    return 1
  fi
  local actual
  actual=$(shasum -a 256 "$path" | awk '{print $1}')
  if [[ "$actual" != "$sha256" ]]; then
    printf 'Agent Sandbox manifest checksum mismatch\nexpected=%s\nactual=%s\nsource=%s\n' "$sha256" "$actual" "$url" >&2
    return 1
  fi
  printf 'agent_sandbox_manifest=verified\nversion=%s\nsource=%s\nsha256=%s\npath=%s\n' "$version" "$url" "$sha256" "$path"
}

case ${1:-} in
  --print-contract)
    [[ $# -eq 1 ]] || { usage >&2; exit 2; }
    print_contract
    ;;
  --output)
    [[ $# -eq 2 && -n ${2:-} ]] || { usage >&2; exit 2; }
    output=$2
    if [[ -e "$output" ]]; then
      printf 'output already exists: %s\n' "$output" >&2
      exit 1
    fi
    mkdir -p "$(dirname "$output")"
    tmp=$(mktemp "${output}.tmp.XXXXXX")
    trap 'test ! -e "$tmp" || unlink "$tmp"' EXIT
    printf 'downloading=%s\n' "$url"
    curl --proto '=https' --tlsv1.2 --fail --location --silent --show-error "$url" --output "$tmp"
    verify_file "$tmp" >/dev/null
    chmod 0644 "$tmp"
    mv "$tmp" "$output"
    trap - EXIT
    verify_file "$output"
    ;;
  -h|--help|'')
    usage
    [[ ${1:-} == '' ]] && exit 2 || exit 0
    ;;
  *)
    [[ $# -eq 1 ]] || { usage >&2; exit 2; }
    verify_file "$1"
    ;;
esac

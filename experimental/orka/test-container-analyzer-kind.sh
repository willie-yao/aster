#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=experimental/orka/orka.env
source "$script_dir/orka.env"
export ORKA_TEST_COMMIT=$ORKA_COMMIT
tmp=$(mktemp -d "${TMPDIR:-/tmp}/test-container-analyzer-kind.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

if grep -Fq "worker-patches" "$script_dir/run-container-analyzer-kind.sh"; then
  echo "container analyzer still depends on worker-patch assets" >&2
  exit 1
fi

stub_dir="$tmp/bin"
runtime_tmp="$tmp/runtime"
mkdir -p "$stub_dir" "$runtime_tmp"
for command in kubectl helm tar go; do
  cat > "$stub_dir/$command" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
  chmod +x "$stub_dir/$command"
done

cat > "$stub_dir/kind" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "get clusters")
    if [[ ${KIND_EXISTING_CLUSTER:-} != "" ]]; then
      printf '%s\n' "$KIND_EXISTING_CLUSTER"
    fi
    ;;
  "create cluster")
    [[ ${KIND_CREATE_SUCCESS:-} == 1 ]]
    ;;
  "delete cluster")
    printf 'delete\n' >> "$KIND_DELETE_MARKER"
    ;;
  "load docker-image")
    ;;
  *)
    exit 1
    ;;
esac
STUB
chmod +x "$stub_dir/kind"

cat > "$stub_dir/docker" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  build)
    shift
    while (( $# > 0 )); do
      if [[ $1 == -t ]]; then
        printf '%s\n' "$2" >> "$DOCKER_BUILD_MARKER"
        exit 0
      fi
      shift
    done
    exit 1
    ;;
  image)
    [[ ${2:-} == rm ]]
    shift 2
    for arg in "$@"; do
      [[ $arg == -f ]] && continue
      printf '%s\n' "$arg" >> "$DOCKER_REMOVE_MARKER"
    done
    ;;
  *)
    exit 1
    ;;
esac
STUB
chmod +x "$stub_dir/docker"

cat > "$stub_dir/git" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${1:-} == clone ]]; then
  mkdir -p "${@: -1}"
  exit 0
fi
if [[ ${1:-} == -C && ${3:-} == rev-parse ]]; then
  printf '%s\n' "$ORKA_TEST_COMMIT"
fi
STUB
chmod +x "$stub_dir/git"

cat > "$stub_dir/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
while (( $# > 0 )); do
  if [[ $1 == -o ]]; then
    : > "$2"
    exit 0
  fi
  shift
done
exit 1
STUB
chmod +x "$stub_dir/curl"

cat > "$stub_dir/sha256sum" <<'STUB'
#!/usr/bin/env bash
printf '%s  %s\n' 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855' "${1:-}"
STUB
chmod +x "$stub_dir/sha256sum"

marker="$tmp/deletes"
locked="$runtime_tmp/prow-ai-dashboard-orka-container-locked-cluster.lock"
mkdir "$locked"
if PATH="$stub_dir:$PATH" \
  TMPDIR="$runtime_tmp" \
  KIND_DELETE_MARKER="$marker" \
  ORKA_CONTAINER_CLUSTER=locked-cluster \
  "$script_dir/run-container-analyzer-kind.sh" >"$tmp/locked.out" 2>&1; then
  echo "locked cluster name was accepted" >&2
  exit 1
fi
grep -Fq 'another container analyzer run owns cluster name' "$tmp/locked.out"
[[ ! -e $marker ]] || { echo "locked cluster was deleted" >&2; exit 1; }
rmdir "$locked"

if PATH="$stub_dir:$PATH" \
  TMPDIR="$runtime_tmp" \
  KIND_EXISTING_CLUSTER=existing-cluster \
  KIND_DELETE_MARKER="$marker" \
  ORKA_CONTAINER_CLUSTER=existing-cluster \
  "$script_dir/run-container-analyzer-kind.sh" >"$tmp/existing.out" 2>&1; then
  echo "existing cluster was accepted" >&2
  exit 1
fi
grep -Fq 'already exists' "$tmp/existing.out"
[[ ! -e $marker ]] || { echo "pre-existing cluster was deleted" >&2; exit 1; }

if PATH="$stub_dir:$PATH" \
  TMPDIR="$runtime_tmp" \
  KIND_DELETE_MARKER="$marker" \
  ORKA_CONTAINER_CLUSTER=partial-cluster \
  "$script_dir/run-container-analyzer-kind.sh" >"$tmp/partial.out" 2>&1; then
  echo "failed kind create unexpectedly succeeded" >&2
  exit 1
fi
if [[ $(wc -l < "$marker") -ne 1 ]]; then
  echo "partial cluster cleanup was not attempted exactly once" >&2
  exit 1
fi

built="$tmp/built-images"
removed="$tmp/removed-images"
: > "$built"
: > "$removed"
PATH="$stub_dir:$PATH" \
TMPDIR="$runtime_tmp" \
KIND_CREATE_SUCCESS=1 \
KIND_DELETE_MARKER="$marker" \
DOCKER_BUILD_MARKER="$built" \
DOCKER_REMOVE_MARKER="$removed" \
ORKA_CONTAINER_FIXTURE_SHA=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 \
ORKA_CONTAINER_CLUSTER=successful-cluster \
"$script_dir/run-container-analyzer-kind.sh" >"$tmp/success.out" 2>&1
if [[ $(wc -l < "$built") -ne 4 ]]; then
  echo "expected four invocation-owned images" >&2
  exit 1
fi
if ! diff -u <(sort "$built") <(sort "$removed"); then
  echo "built images were not removed exactly" >&2
  exit 1
fi

printf 'Container analyzer kind ownership and image cleanup checks passed.\n'

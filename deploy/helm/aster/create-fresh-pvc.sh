#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: create-fresh-pvc.sh [--create] <namespace> <source-pvc> <new-pvc>

Render a new empty PVC with the source claim's access modes, storage request,
storage class, and volume mode. The source claim's data is not copied.

The manifest is printed by default. Pass --create to create the claim.
USAGE
}

create=false
if [[ ${1:-} == "--create" ]]; then
  create=true
  shift
fi
if [[ ${1:-} == "-h" || ${1:-} == "--help" ]]; then
  usage
  exit 0
fi
if [[ $# -ne 3 ]]; then
  usage >&2
  exit 2
fi

namespace=$1
source_pvc=$2
new_pvc=$3
if [[ $source_pvc == "$new_pvc" ]]; then
  echo "source and new PVC names must differ" >&2
  exit 2
fi

kubectl_bin=${KUBECTL:-kubectl}
storage=$($kubectl_bin -n "$namespace" get pvc "$source_pvc" -o jsonpath='{.spec.resources.requests.storage}')
storage_class_state=$($kubectl_bin -n "$namespace" get pvc "$source_pvc" -o go-template='{{if eq (index .spec "storageClassName") nil}}absent{{else}}present={{index .spec "storageClassName"}}{{end}}')
volume_mode=$($kubectl_bin -n "$namespace" get pvc "$source_pvc" -o jsonpath='{.spec.volumeMode}')
access_modes=$($kubectl_bin -n "$namespace" get pvc "$source_pvc" -o jsonpath='{range .spec.accessModes[*]}{.}{"\n"}{end}')

if [[ -z $storage || -z $access_modes ]]; then
  echo "source PVC has no storage request or access modes" >&2
  exit 1
fi

render() {
  cat <<EOF_MANIFEST
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $new_pvc
  namespace: $namespace
spec:
  accessModes:
EOF_MANIFEST
  while IFS= read -r mode; do
    [[ -n $mode ]] && printf '    - %s\n' "$mode"
  done <<< "$access_modes"
  cat <<EOF_MANIFEST
  resources:
    requests:
      storage: $storage
EOF_MANIFEST
  case $storage_class_state in
    absent) ;;
    present=*) printf '  storageClassName: "%s"\n' "${storage_class_state#present=}" ;;
    *) echo "could not determine source PVC storageClassName" >&2; return 1 ;;
  esac
  if [[ -n $volume_mode ]]; then
    printf '  volumeMode: %s\n' "$volume_mode"
  fi
}

if [[ $create == true ]]; then
  render | $kubectl_bin create -f -
else
  render
fi

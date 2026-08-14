#!/usr/bin/env bash
set -euo pipefail

for tool in kind kubectl helm; do
  command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 1; }
done

chart=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
name=${PLATFORM_KIND_CLUSTER_NAME:-prow-platform-$RANDOM-$$}
tmp=$(mktemp -d "${TMPDIR:-/tmp}/prow-ai-dashboard-platform-kind.XXXXXX")
created=false
cleanup() {
  if [[ $created == true ]]; then
    kind delete cluster --name "$name" >/dev/null 2>&1 || true
  fi
  find "$tmp" -type f -delete 2>/dev/null || true
  rmdir "$tmp" 2>/dev/null || true
}
trap cleanup EXIT

if kind get clusters | grep -Fxq "$name"; then
  echo "kind cluster already exists: $name" >&2
  exit 1
fi

cat > "$tmp/values.yaml" <<'VALUES'
application:
  releaseName: sample
execution:
  namespace: sample-sandbox
  runtimeClassName: kata-vm-isolation
  networkPolicy:
    allowedFQDNs:
      - vcs.example.test
      - registry.example.test
      - artifacts.example.test
VALUES

cat > "$tmp/cilium-crd.yaml" <<'CRD'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: ciliumnetworkpolicies.cilium.io
spec:
  group: cilium.io
  names:
    kind: CiliumNetworkPolicy
    listKind: CiliumNetworkPolicyList
    plural: ciliumnetworkpolicies
    singular: ciliumnetworkpolicy
  scope: Namespaced
  versions:
    - name: v2
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
CRD

kind create cluster --name "$name" --wait 60s
created=true
context=kind-$name
kubectl --context "$context" apply -f "$tmp/cilium-crd.yaml"
helm upgrade --install platform "$chart" \
  --namespace sample \
  --create-namespace \
  --kube-context "$context" \
  --values "$tmp/values.yaml" \
  --wait \
  --rollback-on-failure

[[ $(kubectl --context "$context" get namespace sample-sandbox -o jsonpath='{.metadata.labels.prow-ai-dashboard/release}') == sample ]]
[[ $(kubectl --context "$context" get namespace sample-sandbox -o jsonpath='{.metadata.annotations.prow-ai-dashboard/runtime-class}') == kata-vm-isolation ]]
[[ $(kubectl --context "$context" get namespace sample-sandbox -o jsonpath='{.metadata.annotations.prow-ai-dashboard/network-policy-mode}') == cilium ]]
kubectl --context "$context" -n sample-sandbox get resourcequota platform-prow-ai-dashboard-platform-execution >/dev/null
kubectl --context "$context" -n sample-sandbox get limitrange platform-prow-ai-dashboard-platform-execution >/dev/null
kubectl --context "$context" -n sample-sandbox get serviceaccount fix-workload >/dev/null
kubectl --context "$context" -n sample-sandbox get networkpolicy platform-prow-ai-dashboard-platform-execution-default-deny >/dev/null
kubectl --context "$context" -n sample-sandbox get ciliumnetworkpolicy platform-prow-ai-dashboard-platform-execution-egress >/dev/null

helm upgrade platform "$chart" \
  --namespace sample \
  --kube-context "$context" \
  --values "$tmp/values.yaml" \
  --set-string execution.quota.pods=7 \
  --wait \
  --rollback-on-failure
[[ $(kubectl --context "$context" -n sample-sandbox get resourcequota platform-prow-ai-dashboard-platform-execution -o jsonpath='{.spec.hard.pods}') == 7 ]]

if helm upgrade platform "$chart" --namespace sample --kube-context "$context" --values "$tmp/values.yaml" --set execution.namespace=other-sandbox >"$tmp/rename-namespace.out" 2>&1; then
  echo 'execution namespace rename was accepted' >&2
  exit 1
fi
grep -Fq 'execution.namespace is immutable' "$tmp/rename-namespace.out"
kubectl --context "$context" get namespace sample-sandbox >/dev/null
if kubectl --context "$context" get namespace other-sandbox >/dev/null 2>&1; then
  echo 'failed upgrade created the replacement execution namespace' >&2
  exit 1
fi

if helm upgrade platform "$chart" --namespace sample --kube-context "$context" --values "$tmp/values.yaml" --set application.releaseName=other >"$tmp/rename-release.out" 2>&1; then
  echo 'application release rebinding was accepted' >&2
  exit 1
fi
grep -Fq 'application.releaseName is immutable' "$tmp/rename-release.out"
[[ $(kubectl --context "$context" get namespace sample-sandbox -o jsonpath='{.metadata.labels.prow-ai-dashboard/release}') == sample ]]

if helm upgrade --install other-platform "$chart" --namespace sample --kube-context "$context" --values "$tmp/values.yaml" >"$tmp/second-platform.out" 2>&1; then
  echo 'second platform release claimed the retained execution namespace' >&2
  exit 1
fi
grep -Fq 'already bound to another platform release' "$tmp/second-platform.out"

if kubectl --context "$context" get runtimeclass kata-vm-isolation >/dev/null 2>&1; then
  echo 'platform chart unexpectedly created the RuntimeClass' >&2
  exit 1
fi

helm uninstall platform --namespace sample --kube-context "$context"
kubectl --context "$context" get namespace sample-sandbox >/dev/null
kubectl --context "$context" -n sample-sandbox get resourcequota platform-prow-ai-dashboard-platform-execution >/dev/null
kubectl --context "$context" -n sample-sandbox get limitrange platform-prow-ai-dashboard-platform-execution >/dev/null
kubectl --context "$context" -n sample-sandbox get serviceaccount fix-workload >/dev/null
kubectl --context "$context" -n sample-sandbox get networkpolicy platform-prow-ai-dashboard-platform-execution-default-deny >/dev/null
kubectl --context "$context" -n sample-sandbox get ciliumnetworkpolicy platform-prow-ai-dashboard-platform-execution-egress >/dev/null
kubectl --context "$context" -n sample get configmap platform-prow-ai-dashboard-platform-binding >/dev/null

if helm upgrade --install platform "$chart" --namespace sample --kube-context "$context" --values "$tmp/values.yaml" --set execution.namespace=other-sandbox >"$tmp/reinstall-rename.out" 2>&1; then
  echo 'reinstall changed the retained execution binding' >&2
  exit 1
fi
grep -Fq 'execution.namespace is immutable' "$tmp/reinstall-rename.out"

if helm upgrade --install platform "$chart" --namespace sample --kube-context "$context" --values "$tmp/values.yaml" --set fullnameOverride=rebound --set execution.namespace=other-sandbox >"$tmp/reinstall-override.out" 2>&1; then
  echo 'name override bypassed the retained execution binding' >&2
  exit 1
fi
grep -Fq '/fullnameOverride' "$tmp/reinstall-override.out"

helm upgrade --install platform "$chart" --namespace sample --kube-context "$context" --values "$tmp/values.yaml" --wait --rollback-on-failure

echo 'Platform kind lifecycle checks passed. Cilium behavior and secure runtime enforcement were not tested.'

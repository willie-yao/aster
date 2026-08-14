#!/usr/bin/env bash
set -euo pipefail

chart=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/prow-ai-dashboard-platform-schema.XXXXXX")
trap 'find "$tmp" -type f -delete 2>/dev/null || true; rmdir "$tmp" 2>/dev/null || true' EXIT

cat > "$tmp/values.yaml" <<'VALUES'
application:
  releaseName: capz
execution:
  namespace: capz-sandbox
  runtimeClassName: kata-vm-isolation
VALUES

helm lint "$chart" -f "$tmp/values.yaml"
helm template platform "$chart" -n capz -f "$tmp/values.yaml" > "$tmp/render.yaml"

expect_fail() {
  local name=$1 want=$2
  shift 2
  if helm template platform "$chart" -n capz -f "$tmp/values.yaml" "$@" >"$tmp/$name.out" 2>&1; then
    echo "invalid platform values were accepted: $name" >&2
    exit 1
  fi
  grep -Fq "$want" "$tmp/$name.out"
}

gateway_args=(
  --set modelGateway.enabled=true
  --set modelGateway.publicHost=gateway.platform.example.com
  --set modelGateway.upstreamURL=https://provider.example/v1/chat/completions
  --set modelGateway.upstreamHost=provider.example
  --set modelGateway.image.repository=registry.example/gateway
  --set modelGateway.image.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  --set modelGateway.providerAuth.existingSecret=provider
  --set modelGateway.tls.existingSecret=gateway-tls
)

expect_fail missing-release '/application/releaseName' --set-string application.releaseName=
expect_fail unknown-field "additional properties 'unexpected' not allowed" --set unexpected=true
expect_fail name-override '/nameOverride' --set nameOverride=rebound
expect_fail fullname-override '/fullnameOverride' --set fullnameOverride=rebound
expect_fail controller-version '/agentSandbox/requiredVersion' --set agentSandbox.requiredVersion=v0.5.4
expect_fail controller-checksum '/agentSandbox/manifestSHA256' --set agentSandbox.manifestSHA256=deadbeef
expect_fail runtimeclass-create '/runtimeClass/create' --set runtimeClass.create=true --set runtimeClass.handler=kata
expect_fail unsupported-policy-mode '/execution/networkPolicy/mode' --set execution.networkPolicy.mode=kubernetes
expect_fail global-fqdn '/execution/networkPolicy/allowedFQDNs/0' --set-string 'execution.networkPolicy.allowedFQDNs[0]=*'
expect_fail wildcard-fqdn '/execution/networkPolicy/allowedFQDNs/0' --set-string 'execution.networkPolicy.allowedFQDNs[0]=*.github.com'
expect_fail public-suffix-wildcard '/execution/networkPolicy/allowedFQDNs/0' --set-string 'execution.networkPolicy.allowedFQDNs[0]=*.co.uk'
expect_fail multitenant-wildcard '/execution/networkPolicy/allowedFQDNs/0' --set-string 'execution.networkPolicy.allowedFQDNs[0]=*.github.io'
expect_fail ip-fqdn '/execution/networkPolicy/allowedFQDNs/0' --set-string 'execution.networkPolicy.allowedFQDNs[0]=192.0.2.10'
expect_fail gateway-mutable-image 'modelGateway.image.digest must be a sha256 digest' "${gateway_args[@]}" --set modelGateway.image.digest=latest
expect_fail gateway-missing-tls 'modelGateway.tls.existingSecret is required' \
  --set modelGateway.enabled=true \
  --set modelGateway.publicHost=gateway.platform.example.com \
  --set modelGateway.upstreamURL=https://provider.example/v1/chat/completions \
  --set modelGateway.upstreamHost=provider.example \
  --set modelGateway.image.repository=registry.example/gateway \
  --set modelGateway.image.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  --set modelGateway.providerAuth.existingSecret=provider
expect_fail internal-public-host 'must not use an internal or local hostname' "${gateway_args[@]}" --set modelGateway.publicHost=model-gateway.gateway.svc
expect_fail ip-public-host '/modelGateway/publicHost' "${gateway_args[@]}" --set modelGateway.publicHost=192.0.2.10
expect_fail upstream-credentials '/modelGateway/upstreamURL' "${gateway_args[@]}" --set modelGateway.upstreamURL=https://secret@provider.example/v1/chat/completions
expect_fail upstream-query '/modelGateway/upstreamURL' "${gateway_args[@]}" --set modelGateway.upstreamURL=https://provider.example/v1/chat/completions?token=secret
expect_fail upstream-host-mismatch 'modelGateway.upstreamURL host must match modelGateway.upstreamHost' "${gateway_args[@]}" --set modelGateway.upstreamHost=other.example
expect_fail gateway-broad-fqdn '/modelGateway/networkPolicy/allowedFQDNs/0' "${gateway_args[@]}" --set-string 'modelGateway.networkPolicy.allowedFQDNs[0]=*'

echo 'Platform chart schema checks passed.'

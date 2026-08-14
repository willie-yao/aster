#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
chart=$root/deploy/helm/prow-ai-dashboard-platform
app_chart=$root/deploy/helm/prow-ai-dashboard
tmp=$(mktemp -d "${TMPDIR:-/tmp}/prow-ai-dashboard-platform-render.XXXXXX")
trap 'find "$tmp" -type f -delete 2>/dev/null || true; rmdir "$tmp" 2>/dev/null || true' EXIT

cat > "$tmp/platform.yaml" <<'VALUES'
application:
  releaseName: capz
execution:
  namespace: capz-sandbox
  runtimeClassName: kata-vm-isolation
  workloadServiceAccountName: fix-workload
VALUES

helm lint "$chart" -f "$tmp/platform.yaml"
helm template platform "$chart" -n capz -f "$tmp/platform.yaml" > "$tmp/base.yaml"
helm template platform "$chart" -n capz -f "$tmp/platform.yaml" > "$tmp/base-second.yaml"
cmp "$tmp/base.yaml" "$tmp/base-second.yaml"

grep -Fq 'kind: Namespace' "$tmp/base.yaml"
grep -Fq 'name: capz-sandbox' "$tmp/base.yaml"
grep -Fq 'prow-ai-dashboard/release: "capz"' "$tmp/base.yaml"
grep -Fq 'prow-ai-dashboard/runtime-class: "kata-vm-isolation"' "$tmp/base.yaml"
grep -Fq 'prow-ai-dashboard/agent-sandbox-version: "v0.5.3"' "$tmp/base.yaml"
if [ "$(grep -Fc 'helm.sh/resource-policy: keep' "$tmp/base.yaml")" -lt 7 ]; then
  echo 'platform render did not retain the complete execution security boundary' >&2
  exit 1
fi
grep -Fq 'prow-ai-dashboard/network-policy-mode: "cilium"' "$tmp/base.yaml"
grep -Fq 'app.kubernetes.io/component: platform-binding' "$tmp/base.yaml"
grep -Fq 'applicationReleaseName: "capz"' "$tmp/base.yaml"
grep -Fq 'modelGatewayEnabled: "false"' "$tmp/base.yaml"
grep -Fq 'kind: ResourceQuota' "$tmp/base.yaml"
grep -Fq 'count/sandboxes.agents.x-k8s.io: "4"' "$tmp/base.yaml"
grep -Fq 'kind: LimitRange' "$tmp/base.yaml"
grep -Fq 'name: fix-workload' "$tmp/base.yaml"
grep -Fq 'automountServiceAccountToken: false' "$tmp/base.yaml"
grep -Fq 'kind: CiliumNetworkPolicy' "$tmp/base.yaml"
grep -Fq 'matchName: "github.com"' "$tmp/base.yaml"
if grep -Eq '^kind: (Secret|RuntimeClass|CustomResourceDefinition)$' "$tmp/base.yaml" || grep -Fq 'agent-sandbox-controller' "$tmp/base.yaml"; then
  echo 'platform base render claimed an external resource' >&2
  exit 1
fi

cat > "$tmp/gateway.yaml" <<'VALUES'
modelGateway:
  enabled: true
  publicHost: gateway.platform.example.com
  image:
    repository: registry.example/model-gateway
    digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  upstreamURL: https://provider.example/v1/chat/completions
  upstreamHost: provider.example
  providerAuth:
    existingSecret: provider-auth
    tokenKey: AI_TOKEN
  tls:
    existingSecret: gateway-tls
VALUES

helm template platform "$chart" -n capz -f "$tmp/platform.yaml" -f "$tmp/gateway.yaml" > "$tmp/gateway-render.yaml"
grep -Fq 'image: registry.example/model-gateway@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' "$tmp/gateway-render.yaml"
grep -Fq 'prow-ai-dashboard/model-gateway-host: "gateway.platform.example.com"' "$tmp/gateway-render.yaml"
grep -Fq 'prow-ai-dashboard/model-gateway-tls-secret: "gateway-tls"' "$tmp/gateway-render.yaml"
grep -Fq 'name: provider-auth' "$tmp/gateway-render.yaml"
grep -Fq 'secretName: gateway-tls' "$tmp/gateway-render.yaml"
grep -Fq 'mountPath: /tls' "$tmp/gateway-render.yaml"
grep -Fq 'readOnly: true' "$tmp/gateway-render.yaml"
grep -Fq 'type: ClusterIP' "$tmp/gateway-render.yaml"
grep -Fq 'app.kubernetes.io/component: model-gateway' "$tmp/gateway-render.yaml"
grep -Fq 'modelGatewayEnabled: "true"' "$tmp/gateway-render.yaml"
grep -Fq 'modelGatewayPublicHost: "gateway.platform.example.com"' "$tmp/gateway-render.yaml"
grep -Fq 'modelGatewayUpstreamHost: "provider.example"' "$tmp/gateway-render.yaml"
grep -Fq 'modelGatewayExecutionNamespace: "capz-sandbox"' "$tmp/gateway-render.yaml"
grep -Fq 'modelGatewayTargetPort: "8443"' "$tmp/gateway-render.yaml"
if grep -Eq '^kind: Secret$' "$tmp/gateway-render.yaml" || grep -Fq 'value: actual-provider-token' "$tmp/gateway-render.yaml"; then
  echo 'platform gateway render contained a Secret value' >&2
  exit 1
fi

cat > "$tmp/application.yaml" <<'VALUES'
global:
  imageTag: sha-abcdef0
mode: watch
project:
  config: |
    id: test
    name: Test
    testgrid:
      dashboard: test
    storage:
      provider: local
      base: /tmp
    branding:
      title: Test
      base_path: /
      site_url: https://example.test
      source_repo:
        owner: octocat
        name: Hello-World
    ai:
      fix_prs:
        enabled: true
        author_name: Fixture
        author_email: fixture@example.test
        max_files: 3
        critique_retries: 0
        agent_runtime:
          type: agent-sandbox
          max_turns: 30
          allow_bash: false
          timeout: 10m
          output_limit_bytes: 1048576
          allowed_commands:
            - argv: [git, diff, --cached, --check]
              timeout: 30s
          model_provider:
            credential_mode: direct
            api: chat_completions
            endpoint: https://model.example.test/v1/chat/completions
            model: fixture-model
            auth:
              type: none
  systemPrompt: test prompt
agentSandbox:
  fixRuntime:
    enabled: true
    namespace: capz-sandbox
    runtimeClassName: kata-vm-isolation
    image:
      repository: registry.example/fix-executor
      digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    dashboardImage:
      repository: registry.example/remote-fixer
      tag: sha-abcdef0
    workloadServiceAccount:
      create: false
      name: fix-workload
    modelProvider:
      credentialMode: direct
      api: chat_completions
      endpoint: https://model.example.test/v1/chat/completions
      model: fixture-model
      auth:
        type: none
    maxSteps: 30
    maxFiles: 3
    timeout: 10m
    outputLimitBytes: 1048576
    allowedCommands:
      - argv: [git, diff, --cached, --check]
        timeout: 30s
  rbac:
    create: true
server:
  actions:
    enabled: true
    mode: proxy
    admins: [fixture]
    proxy:
      existingSecret: app-auth
VALUES

helm template capz "$app_chart" -n capz -f "$tmp/application.yaml" > "$tmp/application-render.yaml"
python3 - "$tmp/base.yaml" "$tmp/application-render.yaml" <<'PY'
from pathlib import Path
import sys

def identities(path):
    result = set()
    for document in Path(path).read_text().split("\n---\n"):
        api = kind = name = namespace = ""
        in_metadata = False
        for line in document.splitlines():
            if line.startswith("apiVersion:"):
                api = line.split(":", 1)[1].strip()
            elif line.startswith("kind:"):
                kind = line.split(":", 1)[1].strip()
            elif line == "metadata:":
                in_metadata = True
            elif in_metadata and line.startswith("  name:") and not name:
                name = line.split(":", 1)[1].strip().strip('"')
            elif in_metadata and line.startswith("  namespace:") and not namespace:
                namespace = line.split(":", 1)[1].strip().strip('"')
            elif in_metadata and line and not line.startswith(" "):
                in_metadata = False
        if api and kind and name:
            result.add((api, kind, namespace, name))
    return result

platform = identities(sys.argv[1])
application = identities(sys.argv[2])
overlap = sorted(platform & application)
if overlap:
    raise SystemExit(f"platform and application ownership overlap: {overlap}")
PY

if grep -Eq '^kind: (Deployment|CronJob|PersistentVolumeClaim|Ingress)$' "$tmp/base.yaml"; then
  echo 'platform chart rendered application-owned workloads' >&2
  exit 1
fi

echo 'Platform chart render checks passed.'

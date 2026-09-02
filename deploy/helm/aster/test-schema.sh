#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
chart="$root/deploy/helm/aster"
tmp="$root/.test-work/aster-schema-$$"
mkdir -p "$tmp"
trap 'rm -rf "$tmp"' EXIT

cat > "$tmp/base.yaml" <<'VALUES'
project:
  config: |
    id: schema-test
    name: Schema Test
    testgrid:
      dashboard: schema-test
    storage:
      provider: local
      base: .test-work/storage
    branding:
      title: Schema Test
      base_path: /
      site_url: https://schema.example.test
      source_repo:
        owner: example
        name: project
  systemPrompt: schema test prompt
VALUES

expect_pass() {
  local name=$1 values=$2
  if ! helm lint "$chart" -f "$tmp/base.yaml" -f "$values" > "$tmp/$name.out" 2>&1; then
    cat "$tmp/$name.out" >&2
    echo "schema rejected valid values: $name" >&2
    exit 1
  fi
}

expect_fail() {
  local name=$1 values=$2 path=$3
  if helm lint "$chart" -f "$tmp/base.yaml" -f "$values" > "$tmp/$name.out" 2>&1; then
    echo "schema accepted invalid values: $name" >&2
    exit 1
  fi
  grep -Fq "values don't meet the specifications of the schema" "$tmp/$name.out"
  grep -Fq "$path" "$tmp/$name.out"
}

# The complete chart defaults must satisfy the schema before template validation.
helm lint "$chart" > "$tmp/default.out" 2>&1

cat > "$tmp/watch.yaml" <<'VALUES'
mode: watch
fetcher:
  watchInterval: 2m
  reconcileInterval: 30m
  suspend: true
VALUES
expect_pass watch "$tmp/watch.yaml"

cat > "$tmp/cron.yaml" <<'VALUES'
mode: cron
fetcher:
  schedule: "15 */4 * * *"
  concurrencyPolicy: Forbid
  suspend: false
  activeDeadlineSeconds: 7200
  backoffLimit: 0
  restartPolicy: Never
VALUES
expect_pass cron "$tmp/cron.yaml"

cat > "$tmp/oauth.yaml" <<'VALUES'
ai:
  enabled: true
  endpoint: https://model.example.test/v1/chat/completions
  model: fixture-model
  existingSecret: fixture-model-auth
server:
  chat:
    enabled: true
  actions:
    enabled: true
    mode: oauth
    admins:
      - fixture-admin
    oauth:
      clientId: fixture-client-id
      redirectUrl: https://dashboard.example.test/api/auth/callback
      existingSecret: fixture-oauth-auth
  service:
    type: ClusterIP
networkPolicy:
  enabled: true
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ingress-system
      ports:
        - protocol: TCP
          port: 8080
VALUES
expect_pass oauth "$tmp/oauth.yaml"

for property in privateRepositories scope chatScope; do
  cat > "$tmp/invalid-oauth-$property.yaml" <<VALUES
server:
  actions:
    oauth:
      $property: legacy
VALUES
  expect_fail "invalid-oauth-$property" "$tmp/invalid-oauth-$property.yaml" "$property"
done

cat > "$tmp/invalid-network-policy-peers.yaml" <<'VALUES'
networkPolicyPeers: []
VALUES
expect_fail invalid-network-policy-peers "$tmp/invalid-network-policy-peers.yaml" networkPolicyPeers

cat > "$tmp/flexible.yaml" <<'VALUES'
imagePullSecrets:
  - name: fixture-pull
project:
  skills:
    fixture.yaml: |
      id: fixture
      triggers: [failure]
fetcher:
  extraEnv:
    - name: FIXTURE_ENV
      valueFrom:
        secretKeyRef:
          name: fixture-env
          key: value
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
  nodeSelector:
    example.com/pool: cpu
  tolerations:
    - key: dedicated
      operator: Equal
      value: dashboard
      effect: NoSchedule
  affinity:
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution: []
server:
  resources:
    limits:
      cpu: "1"
  nodeSelector:
    example.com/pool: web
  tolerations:
    - operator: Exists
  affinity:
    nodeAffinity:
      preferredDuringSchedulingIgnoredDuringExecution: []
  service:
    annotations:
      example.com/exposure: internal
    internal:
      annotations:
        example.com/load-balancer: private
ingress:
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: 4m
networkPolicy:
  enabled: true
  ingress:
    - from:
        - namespaceSelector:
            matchExpressions:
              - key: kubernetes.io/metadata.name
                operator: In
                values: [ingress-system]
      ports:
        - protocol: TCP
          port: 8080
podSecurityContext:
  seccompProfile:
    type: RuntimeDefault
securityContext:
  seccompProfile:
    type: RuntimeDefault
VALUES
expect_pass flexible "$tmp/flexible.yaml"

cat > "$tmp/invalid-mode.yaml" <<'VALUES'
mode: continuous
VALUES
expect_fail invalid-mode "$tmp/invalid-mode.yaml" /mode

cat > "$tmp/removed-network-policy-from.yaml" <<'VALUES'
networkPolicy:
  enabled: true
  from:
    - namespaceSelector: {}
VALUES
expect_fail removed-network-policy-from "$tmp/removed-network-policy-from.yaml" "additional properties 'from' not allowed"

cat > "$tmp/invalid-type.yaml" <<'VALUES'
fetcher:
  workers: "five"
VALUES
expect_fail invalid-type "$tmp/invalid-type.yaml" /fetcher/workers

cat > "$tmp/invalid-top-level.yaml" <<'VALUES'
fetching:
  workers: 5
VALUES
expect_fail invalid-top-level "$tmp/invalid-top-level.yaml" fetching

cat > "$tmp/invalid-fixed-key.yaml" <<'VALUES'
server:
  service:
    exposure: private
VALUES
expect_fail invalid-fixed-key "$tmp/invalid-fixed-key.yaml" /server/service


cat > "$tmp/agent-sandbox.yaml" <<'VALUES'
project:
  config: |
    id: schema-test
    name: Schema Test
    testgrid:
      dashboard: schema-test
    storage:
      provider: local
      base: .test-work/storage
    branding:
      title: Schema Test
      base_path: /
      site_url: https://schema.example.test
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
          output_limit_bytes: 524288
          allowed_commands:
            - argv: [git, diff, --cached, --check]
              timeout: 30s
          model_provider:
            credential_mode: direct
            api: chat_completions
            endpoint: https://api.githubcopilot.com/chat/completions
            model: fixture-model
            auth:
              type: bearer
  systemPrompt: schema test prompt
agentSandbox:
  fixRuntime:
    enabled: true
    namespace: fix-eval
    runtimeClassName: kata-vm-isolation
    image:
      repository: local/fixexecutor
      digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      pullPolicy: IfNotPresent
    dashboardImage:
      repository: local/remote-fixer
      tag: sha-abcdef0
      pullPolicy: IfNotPresent
    workloadServiceAccount:
      create: true
      name: fix-workload
    modelProvider:
      credentialMode: direct
      api: chat_completions
      endpoint: https://api.githubcopilot.com/chat/completions
      model: fixture-model
      auth:
        type: bearer
        existingSecret: agent-sandbox-model
        tokenKey: AI_TOKEN
      publicCAPrivateDNS: false
    pollInterval: 250ms
    resources:
      requests: {cpu: 100m, memory: 128Mi, ephemeral-storage: 256Mi}
      limits: {cpu: "1", memory: 512Mi, ephemeral-storage: 256Mi}
  rbac:
    create: true
    fixClientServiceAccountName: ""
VALUES
expect_pass agent-sandbox "$tmp/agent-sandbox.yaml"

cat > "$tmp/invalid-agent-sandbox-pull.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    image:
      pullPolicy: Always
VALUES
expect_fail invalid-agent-sandbox-pull "$tmp/invalid-agent-sandbox-pull.yaml" /agentSandbox/fixRuntime/image/pullPolicy

cat > "$tmp/invalid-agent-sandbox-ca-hash.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    caBundle:
      existingConfigMap: model-provider-ca
      key: ca-bundle.pem
      sha256: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
VALUES
expect_fail invalid-agent-sandbox-ca-hash "$tmp/invalid-agent-sandbox-ca-hash.yaml" /agentSandbox/fixRuntime/caBundle/sha256

cat > "$tmp/agent-sandbox-ca.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    caBundle:
      existingConfigMap: model-provider-ca
      key: ca-bundle.pem
      sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
VALUES
expect_pass agent-sandbox-ca "$tmp/agent-sandbox-ca.yaml"


cat > "$tmp/agent-sandbox-responses-api.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    modelProvider:
      api: responses
VALUES
expect_pass agent-sandbox-responses-api "$tmp/agent-sandbox-responses-api.yaml"

cat > "$tmp/invalid-agent-sandbox-provider-api.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    modelProvider:
      api: completions
VALUES
expect_fail invalid-agent-sandbox-provider-api "$tmp/invalid-agent-sandbox-provider-api.yaml" /agentSandbox/fixRuntime/modelProvider/api

cat > "$tmp/invalid-agent-sandbox-max-effort.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    modelProvider:
      reasoningEffort: max
VALUES
expect_fail invalid-agent-sandbox-max-effort "$tmp/invalid-agent-sandbox-max-effort.yaml" /agentSandbox/fixRuntime/modelProvider/reasoningEffort

echo 'Helm values schema checks passed.'

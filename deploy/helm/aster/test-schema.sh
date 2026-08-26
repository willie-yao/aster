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

cat > "$tmp/analysis-shadow.yaml" <<'VALUES'
mode: cron
fetcher:
  resources:
    requests: {cpu: 100m, memory: 128Mi, ephemeral-storage: 3Gi}
    limits: {cpu: "1", memory: 1Gi, ephemeral-storage: 3Gi}
ai:
  enabled: true
  endpoint: https://api.githubcopilot.com/chat/completions
  model: fixture-model
  reasoningEffort: high
  contextWindowTokens: 200000
  maxOutputTokens: 8192
  existingSecret: fixture-model-auth
agentSandbox:
  analysisShadow:
    enabled: true
    namespace: analysis-shadow-eval
    runtimeClassName: kata-vm-isolation
    image:
      repository: local/shadow-executor
      digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      pullPolicy: IfNotPresent
    stagerImage:
      repository: local/shadow-stager
      digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
      pullPolicy: IfNotPresent
    dashboardImage:
      repository: local/remote-fixer
      tag: sha-1234567
      pullPolicy: IfNotPresent
    input:
      existingClaim: shadow-input
      localRoot: /analysis-shadow-input
      localSizeLimit: 3Gi
    workloadServiceAccount:
      create: true
      name: shadow-workload
    modelProvider:
      credentialMode: direct
      api: chat_completions
      endpoint: https://api.githubcopilot.com/chat/completions
      model: fixture-model
      reasoningEffort: high
      auth:
        type: bearer
        existingSecret: shadow-provider
        tokenKey: AI_TOKEN
      publicCAPrivateDNS: false
    timeout: 10m
    outputLimitBytes: 262144
    maxPerRun: 1
    maxSteps: 20
    modelContextTokens: 200000
    modelOutputTokens: 8192
    requireSourceEvidence: true
    pollInterval: 250ms
    ledger:
      existingClaim: shadow-ledger
      mountPath: /private/analysis-shadow-ledger
    networkPolicy:
      mode: cilium
      enabled: true
      gatewayNamespaceSelector: {}
      gatewayPodSelector: {}
      gatewayPort: 443
      gatewayTargetPort: null
      stagingFQDNs: [github.com, api.github.com, storage.googleapis.com]
      dnsNamespaceSelector: {kubernetes.io/metadata.name: kube-system}
      dnsPodSelector: {k8s-app: kube-dns}
    quota:
      enabled: true
    resources:
      requests: {cpu: 250m, memory: 512Mi, ephemeral-storage: 3Gi}
      limits: {cpu: "2", memory: 2Gi, ephemeral-storage: 3Gi}
VALUES
expect_pass analysis-shadow "$tmp/analysis-shadow.yaml"

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

cat > "$tmp/removed-analysis-runtime.yaml" <<'VALUES'
analysisRuntime:
  type: inprocess
VALUES
expect_fail removed-analysis-runtime "$tmp/removed-analysis-runtime.yaml" "additional properties 'analysisRuntime' not allowed"

cat > "$tmp/invalid-api.yaml" <<'VALUES'
ai:
  api: completions
VALUES
expect_fail invalid-api "$tmp/invalid-api.yaml" /ai/api

cat > "$tmp/invalid-analysis-shadow-api.yaml" <<'VALUES'
agentSandbox:
  analysisShadow:
    modelProvider:
      api: completions
VALUES
expect_fail invalid-analysis-shadow-api "$tmp/invalid-analysis-shadow-api.yaml" /agentSandbox/analysisShadow/modelProvider/api

cat > "$tmp/invalid-analysis-shadow-bound.yaml" <<'VALUES'
agentSandbox:
  analysisShadow:
    maxPerRun: 0
VALUES
expect_fail invalid-analysis-shadow-bound "$tmp/invalid-analysis-shadow-bound.yaml" /agentSandbox/analysisShadow/maxPerRun

cat > "$tmp/invalid-analysis-shadow-key.yaml" <<'VALUES'
agentSandbox:
  analysisShadow:
    modelSecret: forbidden
VALUES
expect_fail invalid-analysis-shadow-key "$tmp/invalid-analysis-shadow-key.yaml" /agentSandbox/analysisShadow

cat > "$tmp/invalid-actions.yaml" <<'VALUES'
server:
  actions:
    mode: basic
VALUES
expect_fail invalid-actions "$tmp/invalid-actions.yaml" /server/actions/mode

cat > "$tmp/invalid-escalation-key.yaml" <<'VALUES'
server:
  pullRequestEscalation:
    mode: always
VALUES
expect_fail invalid-escalation-key "$tmp/invalid-escalation-key.yaml" /server/pullRequestEscalation

cat > "$tmp/invalid-escalation-type.yaml" <<'VALUES'
server:
  pullRequestEscalation:
    enabled: "yes"
VALUES
expect_fail invalid-escalation-type "$tmp/invalid-escalation-type.yaml" /server/pullRequestEscalation/enabled

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

cat > "$tmp/invalid-oauth-key.yaml" <<'VALUES'
server:
  actions:
    oauth:
      audience: fixture
VALUES
expect_fail invalid-oauth-key "$tmp/invalid-oauth-key.yaml" /server/actions/oauth

cat > "$tmp/invalid-service.yaml" <<'VALUES'
server:
  service:
    type: ExternalName
VALUES
expect_fail invalid-service "$tmp/invalid-service.yaml" /server/service/type

cat > "$tmp/invalid-access-mode.yaml" <<'VALUES'
persistence:
  accessMode: ReadWriteOnce
VALUES
expect_fail invalid-access-mode "$tmp/invalid-access-mode.yaml" /persistence/accessMode


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
    scheduledClientServiceAccountName: ""
VALUES
expect_pass agent-sandbox "$tmp/agent-sandbox.yaml"

cat > "$tmp/agent-sandbox-analyzer.yaml" <<'VALUES'
agentSandbox:
  analyzer:
    enabled: true
    namespace: analyzer-eval
    runtimeClassName: kata-vm-isolation
    executorImage:
      repository: local/analyzer
      digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      pullPolicy: IfNotPresent
    stagerImage:
      repository: local/analyzer-stager
      digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
      pullPolicy: IfNotPresent
    input:
      existingClaim: analyzer-input
    clientServiceAccount:
      create: true
      name: ""
    workloadServiceAccount:
      create: true
      name: analyzer-workload
    modelProvider:
      credentialMode: gateway
      api: chat_completions
      endpoint: https://model-gateway.platform.svc.cluster.local:8443/v1/chat/completions
      model: fixture-model
      auth:
        type: none
        existingSecret: ""
        tokenKey: ""
      publicCAPrivateDNS: false
    timeout: 15m
    outputLimitBytes: 262144
    pollInterval: 250ms
    networkPolicy:
      mode: kubernetes
      enabled: true
      gatewayNamespaceSelector: {kubernetes.io/metadata.name: platform}
      gatewayPodSelector: {app: model-gateway}
      gatewayPort: 8443
      dnsNamespaceSelector: {kubernetes.io/metadata.name: kube-system}
      dnsPodSelector: {k8s-app: kube-dns}
    quota:
      enabled: true
    resources:
      requests: {cpu: 250m, memory: 512Mi, ephemeral-storage: 3Gi}
      limits: {cpu: "2", memory: 2Gi, ephemeral-storage: 3Gi}
VALUES
expect_pass agent-sandbox-analyzer "$tmp/agent-sandbox-analyzer.yaml"

# Execution bounds now live only in project.yaml, so a stale copy in Helm values
# must fail closed rather than be silently ignored.
cat > "$tmp/invalid-agent-sandbox-output.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    outputLimitBytes: 1024
VALUES
expect_fail invalid-agent-sandbox-output "$tmp/invalid-agent-sandbox-output.yaml" "additional properties 'outputLimitBytes' not allowed"

cat > "$tmp/invalid-agent-sandbox-stale-bounds.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    maxSteps: 30
VALUES
expect_fail invalid-agent-sandbox-stale-bounds "$tmp/invalid-agent-sandbox-stale-bounds.yaml" "additional properties 'maxSteps' not allowed"

cat > "$tmp/invalid-agent-sandbox-pull.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    image:
      pullPolicy: Always
VALUES
expect_fail invalid-agent-sandbox-pull "$tmp/invalid-agent-sandbox-pull.yaml" /agentSandbox/fixRuntime/image/pullPolicy

cat > "$tmp/invalid-agent-sandbox-dashboard-pull.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    dashboardImage:
      pullPolicy: Always
VALUES
expect_fail invalid-agent-sandbox-dashboard-pull "$tmp/invalid-agent-sandbox-dashboard-pull.yaml" /agentSandbox/fixRuntime/dashboardImage/pullPolicy

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

cat > "$tmp/invalid-agent-sandbox-analyzer-pull.yaml" <<'VALUES'
agentSandbox:
  analyzer:
    executorImage:
      pullPolicy: Always
VALUES
expect_fail invalid-agent-sandbox-analyzer-pull "$tmp/invalid-agent-sandbox-analyzer-pull.yaml" /agentSandbox/analyzer/executorImage/pullPolicy

cat > "$tmp/invalid-agent-sandbox-apparmor.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    appArmorProfile: Unconfined
VALUES
expect_fail invalid-agent-sandbox-apparmor "$tmp/invalid-agent-sandbox-apparmor.yaml" /agentSandbox/fixRuntime

cat > "$tmp/invalid-agent-sandbox-key.yaml" <<'VALUES'
agentSandbox:
  controller:
    install: true
VALUES
expect_fail invalid-agent-sandbox-key "$tmp/invalid-agent-sandbox-key.yaml" /agentSandbox


cat > "$tmp/agent-sandbox-responses-api.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    modelProvider:
      api: responses
  analyzer:
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

cat > "$tmp/invalid-reasoning-effort.yaml" <<'VALUES'
ai:
  reasoningEffort: ultra
agentSandbox:
  fixRuntime:
    modelProvider:
      reasoningEffort: ultra
VALUES
expect_fail invalid-reasoning-effort "$tmp/invalid-reasoning-effort.yaml" /ai/reasoningEffort

cat > "$tmp/invalid-agent-sandbox-max-effort.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    modelProvider:
      reasoningEffort: max
VALUES
expect_fail invalid-agent-sandbox-max-effort "$tmp/invalid-agent-sandbox-max-effort.yaml" /agentSandbox/fixRuntime/modelProvider/reasoningEffort

cat > "$tmp/invalid-agent-sandbox-auth-type.yaml" <<'VALUES'
agentSandbox:
  analyzer:
    modelProvider:
      auth:
        type: ambient
VALUES
expect_fail invalid-agent-sandbox-auth-type "$tmp/invalid-agent-sandbox-auth-type.yaml" /agentSandbox/analyzer/modelProvider/auth/type

echo 'Helm values schema checks passed.'

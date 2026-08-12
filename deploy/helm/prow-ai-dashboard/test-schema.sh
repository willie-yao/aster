#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
chart="$root/deploy/helm/prow-ai-dashboard"
generated="$root/backend/internal/onboard/testdata/k8s-values.golden.yaml"
tmp="${TMPDIR:-/tmp}/prow-ai-dashboard-schema-$$"
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
      base: /tmp
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

# The onboarding scaffold is a supported chart values subset.
expect_pass generated "$generated"

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

cat > "$tmp/orka.yaml" <<'VALUES'
mode: cron
ai:
  enabled: true
  endpoint: https://model.example.test/v1/chat/completions
  model: fixture-model
  existingSecret: fixture-model-auth
analysisRuntime:
  type: orka-container
  orkaContainer:
    apiAuth:
      existingSecret: fixture-orka-api
    image:
      tag: sha-deadbeef
    modelAuth:
      existingSecret: fixture-model-auth
    nodeSelector:
      agentpool: cpu-pool
VALUES
expect_pass orka "$tmp/orka.yaml"

cat > "$tmp/shadow.yaml" <<'VALUES'
ai:
  enabled: true
  endpoint: https://model.example.test/v1/chat/completions
  model: fixture-model
  existingSecret: fixture-model-auth
orka:
  agentAnalysisShadow:
    enabled: true
    agentVersion: v1
    admission:
      agentRef: analysis-agent
      repository:
        owner: example
        name: repo
VALUES
expect_pass shadow "$tmp/shadow.yaml"

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

cat > "$tmp/invalid-runtime.yaml" <<'VALUES'
analysisRuntime:
  type: remote
VALUES
expect_fail invalid-runtime "$tmp/invalid-runtime.yaml" /analysisRuntime/type

cat > "$tmp/invalid-api.yaml" <<'VALUES'
ai:
  api: completions
VALUES
expect_fail invalid-api "$tmp/invalid-api.yaml" /ai/api

cat > "$tmp/invalid-shadow-access.yaml" <<'VALUES'
orka:
  agentAnalysisShadow:
    ledger:
      accessMode: ReadOnlyMany
VALUES
expect_fail invalid-shadow-access "$tmp/invalid-shadow-access.yaml" /orka/agentAnalysisShadow/ledger/accessMode

cat > "$tmp/invalid-shadow-bound.yaml" <<'VALUES'
orka:
  agentAnalysisShadow:
    maxPerRun: 0
VALUES
expect_fail invalid-shadow-bound "$tmp/invalid-shadow-bound.yaml" /orka/agentAnalysisShadow/maxPerRun

cat > "$tmp/invalid-shadow-key.yaml" <<'VALUES'
orka:
  agentAnalysisShadow:
    modelSecret: forbidden
VALUES
expect_fail invalid-shadow-key "$tmp/invalid-shadow-key.yaml" /orka/agentAnalysisShadow

cat > "$tmp/invalid-actions.yaml" <<'VALUES'
server:
  actions:
    mode: basic
VALUES
expect_fail invalid-actions "$tmp/invalid-actions.yaml" /server/actions/mode

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
      base: /tmp
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
    maxSteps: 30
    maxFiles: 3
    timeout: 10m
    outputLimitBytes: 524288
    allowedCommands:
      - argv: [git, diff, --cached, --check]
        timeout: 30s
    pollInterval: 250ms
    resources:
      requests: {cpu: 100m, memory: 128Mi, ephemeral-storage: 256Mi}
      limits: {cpu: "1", memory: 512Mi, ephemeral-storage: 256Mi}
  rbac:
    create: true
    clientServiceAccountName: ""
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
      requests: {cpu: 250m, memory: 512Mi, ephemeral-storage: 2Gi}
      limits: {cpu: "2", memory: 2Gi, ephemeral-storage: 2Gi}
VALUES
expect_pass agent-sandbox-analyzer "$tmp/agent-sandbox-analyzer.yaml"

cat > "$tmp/invalid-agent-sandbox-legacy-command.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    allowedCommands:
      - git diff --cached --check
VALUES
expect_fail invalid-agent-sandbox-legacy-command "$tmp/invalid-agent-sandbox-legacy-command.yaml" /agentSandbox/fixRuntime/allowedCommands/0

cat > "$tmp/invalid-agent-sandbox-command-timeout.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    allowedCommands:
      - argv: [git, diff, --cached, --check]
        timeout: 999999999999999999999999999999s
VALUES
expect_fail invalid-agent-sandbox-command-timeout "$tmp/invalid-agent-sandbox-command-timeout.yaml" /agentSandbox/fixRuntime/allowedCommands/0/timeout

cat > "$tmp/invalid-agent-sandbox-output.yaml" <<'VALUES'
agentSandbox:
  fixRuntime:
    outputLimitBytes: 1024
VALUES
expect_fail invalid-agent-sandbox-output "$tmp/invalid-agent-sandbox-output.yaml" /agentSandbox/fixRuntime/outputLimitBytes

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

cat > "$tmp/invalid-agent-sandbox-auth-type.yaml" <<'VALUES'
agentSandbox:
  analyzer:
    modelProvider:
      auth:
        type: ambient
VALUES
expect_fail invalid-agent-sandbox-auth-type "$tmp/invalid-agent-sandbox-auth-type.yaml" /agentSandbox/analyzer/modelProvider/auth/type

echo 'Helm values schema checks passed.'

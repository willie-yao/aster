#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
chart="$root/deploy/helm/aster"
tmp="$root/.test-work/aster-helm-$$"
mkdir -p "$tmp"
trap 'rm -rf "$tmp"' EXIT

container_command() {
  local name=$1 file=$2
  awk -v target="$name" '
    $1 == "-" && $2 == "name:" { current = $3 }
    current == target && $1 == "command:" {
      getline
      sub(/^[[:space:]]*-[[:space:]]*/, "")
      print
      exit
    }
  ' "$file"
}

expect_fail() {
  local name=$1 expected=$2
  shift 2
  if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" "$@" > "$tmp/$name.out" 2>&1; then
    echo "invalid render accepted: $name" >&2
    exit 1
  fi
  grep -Fq "$expected" "$tmp/$name.out" || grep -Fq "values don't meet the specifications of the schema" "$tmp/$name.out"
}

cat > "$tmp/values.yaml" <<'VALUES'
image:
  tag: sha-test
project:
  config: |
    id: test
    name: Test
    testgrid:
      dashboard: test
    storage:
      provider: local
      base: .test-work/storage
    branding:
      title: Test
      base_path: /
      site_url: https://example.test
      source_repo:
        owner: octocat
        name: Hello-World
  systemPrompt: test prompt
VALUES

helm lint "$chart" -f "$tmp/values.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" > "$tmp/default.yaml"
if grep -Eq '^kind: (CronJob|Job)$' "$tmp/default.yaml"; then
  echo 'watch mode rendered a CronJob or manual Job' >&2
  exit 1
fi
if grep -Fq 'AI_REASONING_EFFORT' "$tmp/default.yaml"; then
  echo 'default render included an unset reasoning effort' >&2
  exit 1
fi
if grep -Fq -- '-analysis-runtime=' "$tmp/default.yaml"; then
  echo 'default render selected a non-default analysis runtime' >&2
  exit 1
fi
if grep -Fq -- '-agent-analysis-shadow' "$tmp/default.yaml" || grep -Fq 'agent-sandbox-analysis-shadow' "$tmp/default.yaml"; then
  echo 'default render enabled analysis shadow resources' >&2
  exit 1
fi
if grep -Fq 'kind: CustomResourceDefinition' "$tmp/default.yaml" || grep -Fq 'agent-sandbox-controller' "$tmp/default.yaml"; then
  echo 'dashboard chart attempted to install Agent Sandbox itself' >&2
  exit 1
fi

cat > "$tmp/stale-skills.yaml" <<'VALUES'
project:
  skills:
    stale.yaml: stale
VALUES
cat > "$tmp/current-skill.yaml" <<'SKILL'
id: current
triggers: [failure]
SKILL
helm template test "$chart" -n dashboard-test \
  -f "$tmp/values.yaml" -f "$tmp/stale-skills.yaml" \
  --set-json 'project.skills={}' \
  --set-file "project.skills.current\.yaml=$tmp/current-skill.yaml" \
  --show-only templates/configmap-project.yaml > "$tmp/bundle-skills.yaml"
grep -Fq 'current.yaml: |' "$tmp/bundle-skills.yaml"
if grep -Fq 'stale.yaml:' "$tmp/bundle-skills.yaml"; then
  echo 'bundle skill override retained a stale values entry' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron --show-only templates/fetcher-cronjob.yaml > "$tmp/cron.yaml"
grep -Fq 'activeDeadlineSeconds: 36000' "$tmp/cron.yaml"
grep -Fq 'restartPolicy: OnFailure' "$tmp/cron.yaml"
if grep -Fq 'backoffLimit:' "$tmp/cron.yaml"; then
  echo 'default CronJob unexpectedly set a backoff limit' >&2
  exit 1
fi
if [[ $(container_command fetcher "$tmp/cron.yaml") != /usr/local/bin/fetcher ]]; then
  echo 'CronJob does not run the in-process fetcher' >&2
  exit 1
fi
if grep -Fq 'name: prepare-project' "$tmp/cron.yaml" || grep -Fq 'name: project-runtime' "$tmp/cron.yaml"; then
  echo 'in-process CronJob unexpectedly materialized the project ConfigMap' >&2
  exit 1
fi
grep -A3 -F 'name: project' "$tmp/cron.yaml" | grep -Fq 'mountPath: /config'
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" --set mode=cron > "$tmp/cron-all.yaml"
if grep -Fq 'app.kubernetes.io/component: worker' "$tmp/cron-all.yaml"; then
  echo 'cron mode rendered a worker Deployment' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set-string image.tag= --set-string global.imageTag=sha-abcdef0 > "$tmp/global-engine.yaml"
grep -Fq 'image: ghcr.io/willie-yao/aster:sha-abcdef0' "$tmp/global-engine.yaml"
helm package "$chart" --destination "$tmp" --version 9.8.7 --app-version v9.8.7 >/dev/null
fallback_chart="$tmp/aster-9.8.7.tgz"
helm template test "$fallback_chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set-string image.tag= --set-string global.imageTag= > "$tmp/app-version-engine.yaml"
grep -Fq 'image: ghcr.io/willie-yao/aster:v9.8.7' "$tmp/app-version-engine.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true \
  --set ai.endpoint=https://model.example.test/v1/chat/completions \
  --set ai.model=fixture-model \
  --set ai.token=test-token \
  --set server.chat.enabled=true \
  --set server.actions.mode=oauth \
  --set server.actions.admins[0]=fixture \
  --set server.actions.oauth.clientId=client-id \
  --set server.actions.oauth.redirectUrl=https://dashboard.example.test/api/auth/callback \
  --set server.actions.oauth.clientSecret=client-secret \
  --set server.actions.oauth.sessionKey=session-key \
  --show-only templates/server-deployment.yaml > "$tmp/chat.yaml"
grep -A1 -F 'name: ANALYSIS_CHAT_ENABLED' "$tmp/chat.yaml" | grep -Fq 'value: "true"'
grep -A1 -F 'name: AUTH_MODE' "$tmp/chat.yaml" | grep -Fq 'value: "oauth"'
if grep -Fq 'name: BOT_TOKEN' "$tmp/chat.yaml" || grep -Fq 'name: ACTIONS_ENABLED' "$tmp/chat.yaml"; then
  echo 'chat-only OAuth rendered write-action credentials' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true \
  --set ai.endpoint=https://model.example.test/v1/chat/completions \
  --set ai.model=fixture-model \
  --set ai.token=test-token \
  --set server.remediationInvestigation.enabled=true \
  --set server.actions.mode=proxy \
  --show-only templates/server-deployment.yaml > "$tmp/remediation.yaml"
grep -A1 -F 'name: CAUSAL_REMEDIATION_INVESTIGATION_ENABLED' "$tmp/remediation.yaml" | grep -Fq 'value: "true"'
if grep -Fq 'name: ACTIONS_ENABLED' "$tmp/remediation.yaml" || grep -Fq 'name: BOT_TOKEN' "$tmp/remediation.yaml"; then
  echo 'remediation rendered write-action credentials' >&2
  exit 1
fi
expect_fail remediation-without-ai 'server.remediationInvestigation.enabled requires ai.enabled' --set server.remediationInvestigation.enabled=true --set server.actions.mode=proxy
expect_fail invalid-runtime 'analysisRuntime.type must be inprocess' --set analysisRuntime.type=remote

cat > "$tmp/agent-sandbox.yaml" <<'VALUES'
mode: watch
project:
  config: |
    id: test
    name: Test
    testgrid:
      dashboard: test
    storage:
      provider: local
      base: .test-work/storage
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
            endpoint: https://api.githubcopilot.com/chat/completions
            model: fixture-model
            reasoning_effort: high
            auth:
              type: bearer
  systemPrompt: test prompt
agentSandbox:
  fixRuntime:
    enabled: true
    namespace: fix-eval
    runtimeClassName: kata-vm-isolation
    image:
      repository: local/agent-sandbox-fix-executor
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
      reasoningEffort: high
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
    clientServiceAccountName: ""
server:
  actions:
    enabled: true
    mode: proxy
    admins: [fixture]
    proxy:
      header: X-Authenticated-User
      botToken: test-token
VALUES

helm template test "$chart" -n dashboard-test -f "$tmp/agent-sandbox.yaml" > "$tmp/agent-sandbox-render.yaml"
grep -Fq 'kind: ValidatingAdmissionPolicy' "$tmp/agent-sandbox-render.yaml"
grep -Fq 'apiGroups: ["agents.x-k8s.io"]' "$tmp/agent-sandbox-render.yaml"
grep -Fq 'resources: ["sandboxes"]' "$tmp/agent-sandbox-render.yaml"
grep -Fq 'resources: ["pods/log"]' "$tmp/agent-sandbox-render.yaml"
grep -Fq 'serviceAccountName: test-prow-ai-dashboard-agent-sandbox-client' "$tmp/agent-sandbox-render.yaml"
grep -A1 -F 'name: AGENT_SANDBOX_MODEL_PROVIDER_REASONING_EFFORT' "$tmp/agent-sandbox-render.yaml" | grep -Fq 'value: "high"'
grep -Fq "variables.container.env[1].name == 'PROW_AI_MODEL_PROVIDER_TOKEN'" "$tmp/agent-sandbox-render.yaml"
grep -Fq 'local/agent-sandbox-fix-executor@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' "$tmp/agent-sandbox-render.yaml"
# Fix generation is a maintainer-initiated server action, so only the server runs
# the remote fixer. A scheduled pod picking it up would carry unused authority.
if [ "$(grep -Fc 'image: local/remote-fixer:sha-abcdef0' "$tmp/agent-sandbox-render.yaml")" -ne 1 ]; then
  echo 'Agent Sandbox did not select the remote fixer for the server alone' >&2
  exit 1
fi
if grep -Eq 'resources: \["(secrets|services|persistentvolumeclaims|pods/exec|pods/attach|nodes)"\]' "$tmp/agent-sandbox-render.yaml"; then
  echo 'Agent Sandbox RBAC rendered a forbidden resource' >&2
  exit 1
fi
expect_fail agent-sandbox-reserved-env 'must not override reserved Agent Sandbox variable' \
  -f "$tmp/agent-sandbox.yaml" --set server.extraEnv[0].name=AGENT_SANDBOX_IMAGE --set server.extraEnv[0].value=attacker
expect_fail agent-sandbox-project-runtime 'project ai.fix_prs.agent_runtime.type=agent-sandbox' \
  -f "$tmp/agent-sandbox.yaml" --set project.config='ai: {fix_prs: {agent_runtime: {type: legacy}}}'
cat > "$tmp/project-agent-command.yaml" <<'PROJECT'
id: test
name: Test
testgrid:
  dashboard: test
storage:
  provider: local
  base: .test-work/storage
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
        - argv: [opencode, diff, --cached, --check]
          timeout: 30s
      model_provider:
        credential_mode: direct
        api: chat_completions
        endpoint: https://api.githubcopilot.com/chat/completions
        model: fixture-model
        reasoning_effort: high
        auth:
          type: bearer
PROJECT

expect_fail agent-sandbox-command-agent 'must not invoke a coding agent or executor' \
  -f "$tmp/agent-sandbox.yaml" --set-file project.config="$tmp/project-agent-command.yaml"

cat > "$tmp/analysis-shadow.yaml" <<'VALUES'
mode: cron
ai:
  enabled: true
  endpoint: https://model.example.test/v1/chat/completions
  model: fixture-model
  token: test-token
agentSandbox:
  analysisShadow:
    enabled: true
    namespace: analysis-shadow-eval
    runtimeClassName: kata-vm-isolation
    image:
      repository: local/shadow-executor
      digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
      pullPolicy: IfNotPresent
    workloadServiceAccount:
      create: true
      name: shadow-workload
    agentVersion: v1
    modelProvider:
      credentialMode: direct
      api: chat_completions
      endpoint: https://api.githubcopilot.com/chat/completions
      model: fixture-model
    timeout: 10m
    retries: 1
    outputLimitBytes: 65536
    maxPerRun: 2
    maxTurns: 20
    pollInterval: 250ms
    ledger:
      existingClaim: shadow-ledger
      mountPath: /private/analysis-shadow
    networkPolicy:
      mode: kubernetes
      enabled: true
      gatewayNamespaceSelector: {kubernetes.io/metadata.name: platform}
      gatewayPodSelector: {app: model-gateway}
      gatewayPort: 443
      dnsNamespaceSelector: {kubernetes.io/metadata.name: kube-system}
      dnsPodSelector: {k8s-app: kube-dns}
    resources:
      requests: {cpu: 100m, memory: 128Mi, ephemeral-storage: 256Mi}
      limits: {cpu: "1", memory: 512Mi, ephemeral-storage: 256Mi}
VALUES
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" -f "$tmp/analysis-shadow.yaml" > "$tmp/analysis-shadow-render.yaml"
for expected in \
  '-agent-analysis-shadow' \
  '-agent-analysis-shadow-agent-version=v1' \
  '-agent-analysis-shadow-ledger=/private/analysis-shadow/analysis_shadow.json' \
  '-agent-analysis-shadow-max-turns=20' \
  '-agent-analysis-shadow-provider-endpoint=https://api.githubcopilot.com/chat/completions' \
  'name: AGENT_SANDBOX_ANALYSIS_SHADOW_NAMESPACE' \
  'value: "analysis-shadow-eval"' \
  'name: agent-analysis-shadow-ledger' \
  'claimName: shadow-ledger' \
  'agent-sandbox-analysis-shadow'; do
  grep -Fq -- "$expected" "$tmp/analysis-shadow-render.yaml"
done
grep -Fq 'serviceAccountName: test-prow-ai-dashboard-agent-sandbox-client' "$tmp/analysis-shadow-render.yaml"
grep -Fq 'automountServiceAccountToken: true' "$tmp/analysis-shadow-render.yaml"
expect_fail analysis-shadow-with-critic 'cannot run with agentSandbox.analysisShadow' \
  -f "$tmp/analysis-shadow.yaml" --set agentSandbox.causalCritic.enabled=true
expect_fail analysis-shadow-reserved-env 'reserved analysis shadow variable' \
  -f "$tmp/analysis-shadow.yaml" --set fetcher.extraEnv[0].name=AGENT_SANDBOX_ANALYSIS_SHADOW_IMAGE --set fetcher.extraEnv[0].value=attacker

cat > "$tmp/causal-critic.yaml" <<'VALUES'
ai:
  enabled: true
  endpoint: https://model.example.test/v1/chat/completions
  model: fixture-model
  token: test-token
agentSandbox:
  causalCritic:
    enabled: true
    namespace: critic-eval
    runtimeClassName: kata-vm-isolation
    image:
      repository: local/critic-executor
      digest: sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
      pullPolicy: IfNotPresent
    workloadServiceAccount:
      create: true
      name: critic-workload
    modelGateway:
      endpoint: https://critic-gateway.platform.svc.cluster.local:8443/v1/chat/completions
      model: critic-model
      protocolVersion: openai-chat-completions-v1
    timeout: 5m
    outputLimitBytes: 65536
    maxPerRun: 1
    pollInterval: 250ms
    ledger:
      existingClaim: critic-ledger
      mountPath: /private/causal-critic
    networkPolicy:
      mode: kubernetes
      enabled: true
      gatewayNamespaceSelector: {kubernetes.io/metadata.name: platform}
      gatewayPodSelector: {app: critic-gateway}
      gatewayPort: 8443
      dnsNamespaceSelector: {kubernetes.io/metadata.name: kube-system}
      dnsPodSelector: {k8s-app: kube-dns}
    resources:
      requests: {cpu: 50m, memory: 64Mi, ephemeral-storage: 32Mi}
      limits: {cpu: 500m, memory: 256Mi, ephemeral-storage: 32Mi}
VALUES
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" -f "$tmp/causal-critic.yaml" > "$tmp/causal-critic-render.yaml"
grep -Fq -- '-causal-critic-shadow' "$tmp/causal-critic-render.yaml"
grep -Fq 'name: AGENT_SANDBOX_CRITIC_NAMESPACE' "$tmp/causal-critic-render.yaml"
grep -Fq 'claimName: critic-ledger' "$tmp/causal-critic-render.yaml"
grep -Fq 'kind: ValidatingAdmissionPolicy' "$tmp/causal-critic-render.yaml"
expect_fail causal-critic-with-shadow 'cannot run with agentSandbox.analysisShadow' \
  -f "$tmp/causal-critic.yaml" --set agentSandbox.analysisShadow.enabled=true

cat > "$tmp/analyzer.yaml" <<'VALUES'
agentSandbox:
  analyzer:
    enabled: true
    namespace: analyzer-eval
    runtimeClassName: kata-vm-isolation
    executorImage:
      repository: local/analyzer
      digest: sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
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
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" -f "$tmp/analyzer.yaml" > "$tmp/analyzer-render.yaml"
grep -Fq 'agent-sandbox-analyzer' "$tmp/analyzer-render.yaml"
grep -Fq 'kind: ResourceQuota' "$tmp/analyzer-render.yaml"
grep -Fq 'analyzer-input' "$tmp/analyzer-render.yaml"
expect_fail analyzer-with-shadow 'agentSandbox.analyzer cannot run with agentSandbox.analysisShadow' \
  -f "$tmp/analyzer.yaml" -f "$tmp/analysis-shadow.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true \
  --set ai.endpoint=https://model.example.test/v1/chat/completions \
  --set ai.model=fixture-model \
  --set ai.token=test-token \
  --set ai.githubReadToken=github-read-token \
  --show-only templates/secret-github-read.yaml > "$tmp/github-read-secret.yaml"
if [[ $(grep -Fc 'kind: Secret' "$tmp/github-read-secret.yaml") -ne 1 ]]; then
  echo 'GitHub read token secret rendered more than once' >&2
  exit 1
fi

echo 'Helm render checks passed.'

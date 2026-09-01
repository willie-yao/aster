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
grep -A1 -F 'strategy:' "$tmp/chat.yaml" | grep -Fq 'type: Recreate'
test "$(grep -Fc 'name: ANALYSIS_CHAT_TIMEOUT' "$tmp/chat.yaml")" -eq 1
grep -A1 -F 'name: ANALYSIS_CHAT_TIMEOUT' "$tmp/chat.yaml" | grep -Fq 'value: "10m"'
grep -A1 -F 'name: AUTH_MODE' "$tmp/chat.yaml" | grep -Fq 'value: "oauth"'
if grep -Fq 'name: BOT_TOKEN' "$tmp/chat.yaml" || grep -Fq 'name: ACTIONS_ENABLED' "$tmp/chat.yaml"; then
  echo 'chat-only OAuth rendered write-action credentials' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set ai.enabled=true \
  --set ai.endpoint=https://model.example.test/v1/chat/completions \
  --set ai.model=fixture-model \
  --set ai.token=test-token \
  --set server.chat.enabled=true \
  --set server.actions.mode=proxy \
  --show-only templates/fetcher-cronjob.yaml > "$tmp/chat-fetcher.yaml"
grep -Fq -- '- -prepare-cause-findings' "$tmp/chat-fetcher.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=watch \
  --set ai.enabled=true \
  --set ai.endpoint=https://model.example.test/v1/chat/completions \
  --set ai.model=fixture-model \
  --set ai.token=test-token \
  --set server.chat.enabled=true \
  --set server.actions.mode=proxy \
  --show-only templates/worker-deployment.yaml > "$tmp/chat-worker.yaml"
grep -Fq -- '- -prepare-cause-findings' "$tmp/chat-worker.yaml"

# Escalation alone must render every prerequisite the feature needs: the project
# mount, an auth mode, the model credential, and an authenticated GitHub read.
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true \
  --set ai.endpoint=https://model.example.test/v1/chat/completions \
  --set ai.model=fixture-model \
  --set ai.token=test-token \
  --set ai.githubReadTokenSecretName=read-token \
  --set server.pullRequestEscalation.enabled=true \
  --set server.actions.mode=proxy \
  --show-only templates/server-deployment.yaml > "$tmp/escalation.yaml"
grep -A1 -F 'name: PULL_REQUEST_ESCALATION_ENABLED' "$tmp/escalation.yaml" | grep -Fq 'value: "true"'
grep -A1 -F 'name: AUTH_MODE' "$tmp/escalation.yaml" | grep -Fq 'value: "proxy"'
grep -Fq 'name: AI_TOKEN' "$tmp/escalation.yaml"
grep -Fq 'name: GITHUB_READ_TOKEN' "$tmp/escalation.yaml"
grep -Fq -- '-project-dir=/config' "$tmp/escalation.yaml"
if grep -Fq 'name: ACTIONS_ENABLED' "$tmp/escalation.yaml" || grep -Fq 'name: BOT_TOKEN' "$tmp/escalation.yaml"; then
  echo 'escalation rendered write-action credentials' >&2
  exit 1
fi
# Escalation persists its own state, so the shared volume must be writable.
grep -A3 -F 'mountPath: /data' "$tmp/escalation.yaml" | grep -Fq 'readOnly: false'

# Escalation must not leak a GitHub read token into a server that cannot use it.
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true \
  --set ai.endpoint=https://model.example.test/v1/chat/completions \
  --set ai.model=fixture-model \
  --set ai.token=test-token \
  --set ai.githubReadTokenSecretName=read-token \
  --set server.chat.enabled=true \
  --set server.actions.mode=proxy \
  --show-only templates/server-deployment.yaml > "$tmp/escalation-off.yaml"
if grep -Fq 'name: PULL_REQUEST_ESCALATION_ENABLED' "$tmp/escalation-off.yaml" || grep -Fq 'name: GITHUB_READ_TOKEN' "$tmp/escalation-off.yaml"; then
  echo 'chat-only render leaked escalation environment' >&2
  exit 1
fi

expect_fail escalation-without-ai 'server.pullRequestEscalation.enabled requires ai.enabled' --set server.pullRequestEscalation.enabled=true --set server.actions.mode=proxy
expect_fail escalation-raw-env 'server.extraEnv must not set PULL_REQUEST_ESCALATION_ENABLED' \
  --set server.extraEnv[0].name=PULL_REQUEST_ESCALATION_ENABLED --set server.extraEnv[0].value=true
expect_fail chat-timeout-raw-env 'server.extraEnv must not set ANALYSIS_CHAT_TIMEOUT' \
  --set server.extraEnv[0].name=ANALYSIS_CHAT_TIMEOUT --set server.extraEnv[0].value=1m
# Escalation is an authenticated origin, so the public LoadBalancer guardrail
# must cover it exactly as it covers actions, chat, and remediation.
expect_fail escalation-public-load-balancer 'authenticated server features with a LoadBalancer require' \
  --set ai.enabled=true \
  --set ai.endpoint=https://model.example.test/v1/chat/completions \
  --set ai.model=fixture-model \
  --set ai.token=test-token \
  --set server.pullRequestEscalation.enabled=true \
  --set server.actions.mode=proxy \
  --set server.service.type=LoadBalancer

# The whole chart must install for escalation alone: the Deployment references
# the OAuth auth Secret, so secret-auth.yaml has to render it too.
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true \
  --set ai.endpoint=https://model.example.test/v1/chat/completions \
  --set ai.model=fixture-model \
  --set ai.token=test-token \
  --set server.pullRequestEscalation.enabled=true \
  --set server.actions.mode=oauth \
  --set server.actions.admins[0]=fixture \
  --set server.actions.oauth.clientId=client-id \
  --set server.actions.oauth.redirectUrl=https://dashboard.example.test/api/auth/callback \
  --set server.actions.oauth.clientSecret=client-secret \
  --set server.actions.oauth.sessionKey=session-key > "$tmp/escalation-oauth.yaml"
grep -Fq 'name: test-prow-ai-dashboard-auth' "$tmp/escalation-oauth.yaml"
grep -Fq 'SESSION_KEY: "session-key"' "$tmp/escalation-oauth.yaml"
if grep -Fq 'BOT_TOKEN:' "$tmp/escalation-oauth.yaml"; then
  echo 'escalation-only OAuth rendered a write credential' >&2
  exit 1
fi

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
    fixClientServiceAccountName: ""
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
fix_client=test-prow-ai-dashboard-agent-sandbox-fix-client
# Fix authority stays on the dedicated server client identity.
grep -Fq "serviceAccountName: $fix_client" "$tmp/agent-sandbox-render.yaml"
grep -Fq "system:serviceaccount:dashboard-test:$fix_client" "$tmp/agent-sandbox-render.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/agent-sandbox.yaml" \
  -s templates/agent-sandbox-fix-runtime-rbac.yaml > "$tmp/agent-sandbox-fix-rbac.yaml"
grep -A3 -F 'subjects:' "$tmp/agent-sandbox-fix-rbac.yaml" | grep -Fq "name: $fix_client"
helm template test "$chart" -n dashboard-test -f "$tmp/agent-sandbox.yaml" \
  -s templates/worker-deployment.yaml > "$tmp/agent-sandbox-worker.yaml"
grep -Fq 'automountServiceAccountToken: false' "$tmp/agent-sandbox-worker.yaml"
if grep -Fq 'serviceAccountName:' "$tmp/agent-sandbox-worker.yaml"; then
  echo 'Fix release gave the worker an Agent Sandbox client ServiceAccount' >&2
  exit 1
fi
# A long release name must retain the Fix client suffix.
long_release=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
helm template "$long_release" "$chart" -n dashboard-test -f "$tmp/agent-sandbox.yaml" \
  > "$tmp/agent-sandbox-long-name.yaml"
long_base=$(printf '%s' "$long_release-prow-ai-dashboard" | cut -c1-32)
grep -Fq "serviceAccountName: $long_base-agent-sandbox-fix-client" "$tmp/agent-sandbox-long-name.yaml"
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

# Pull request triage reads GitHub with no model calls, so the read token must
# render independently of ai.enabled.
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=false \
  --set ai.githubReadTokenSecretName=external-github-read \
  --show-only templates/worker-deployment.yaml > "$tmp/triage-worker.yaml"
grep -Fq 'GITHUB_READ_TOKEN' "$tmp/triage-worker.yaml"
grep -Fq 'name: external-github-read' "$tmp/triage-worker.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set ai.enabled=false \
  --set ai.githubReadTokenSecretName=external-github-read \
  --show-only templates/fetcher-cronjob.yaml > "$tmp/triage-cronjob.yaml"
grep -Fq 'GITHUB_READ_TOKEN' "$tmp/triage-cronjob.yaml"
grep -Fq 'name: external-github-read' "$tmp/triage-cronjob.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=false \
  --set ai.githubReadToken=github-read-token \
  --show-only templates/secret-github-read.yaml > "$tmp/triage-secret.yaml"
if [[ $(grep -Fc 'kind: Secret' "$tmp/triage-secret.yaml") -ne 1 ]]; then
  echo 'GitHub read token secret not rendered exactly once with ai.enabled=false' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=false \
  --show-only templates/worker-deployment.yaml > "$tmp/triage-worker-unset.yaml"
if grep -Fq 'GITHUB_READ_TOKEN' "$tmp/triage-worker-unset.yaml"; then
  echo 'GITHUB_READ_TOKEN rendered without a configured read token' >&2
  exit 1
fi

# ai.existingSecret may or may not carry the read key, so the chart mounts it
# optional. An explicitly named Secret must stay required.
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=true \
  --set ai.endpoint=https://model.example.test/v1/chat/completions \
  --set ai.model=fixture-model \
  --set ai.existingSecret=shared-ai \
  --show-only templates/worker-deployment.yaml > "$tmp/triage-worker-existing.yaml"
grep -Fq 'optional: true' "$tmp/triage-worker-existing.yaml"
if grep -Fq 'optional: true' "$tmp/triage-worker.yaml"; then
  echo 'an explicitly named read-token Secret was rendered optional' >&2
  exit 1
fi

# With AI off the AI Secret is never deployed, so it must not be referenced.
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set ai.enabled=false \
  --set ai.existingSecret=shared-ai \
  --show-only templates/worker-deployment.yaml > "$tmp/triage-worker-existing-off.yaml"
if grep -Fq 'GITHUB_READ_TOKEN' "$tmp/triage-worker-existing-off.yaml"; then
  echo 'ai.existingSecret was referenced for the read token while AI is disabled' >&2
  exit 1
fi

echo 'Helm render checks passed.'

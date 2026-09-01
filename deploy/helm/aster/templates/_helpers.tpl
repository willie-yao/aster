{{/*
Chart name, optionally overridden.
*/}}
{{- define "aster.name" -}}
{{- default "prow-ai-dashboard" .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name.
*/}}
{{- define "aster.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default "prow-ai-dashboard" .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "aster.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "aster.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels.
*/}}
{{- define "aster.selectorLabels" -}}
app.kubernetes.io/name: {{ include "aster.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Resolve an image-specific tag, then the shared tag, then appVersion. */}}
{{- define "aster.resolvedImageTag" -}}
{{- $root := index . 0 -}}
{{- $specificTag := index . 1 -}}
{{- $global := $root.Values.global | default dict -}}
{{- $globalTag := $global.imageTag | default "" -}}
{{- if and $globalTag (not (regexMatch "^(sha-[0-9a-fA-F]{7,64}|v?[0-9]+[.][0-9]+[.][0-9]+(-[0-9A-Za-z.-]+)?)$" $globalTag)) -}}
{{- fail "global.imageTag must be an immutable sha-<hex> or full semantic version" -}}
{{- end -}}
{{- $specificTag | default $globalTag | default $root.Chart.AppVersion -}}
{{- end -}}

{{/* Engine image reference. */}}
{{- define "aster.image" -}}
{{- $tag := include "aster.resolvedImageTag" (list . .Values.image.tag) -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/* Minimal git-capable engine image used with Agent Sandbox. */}}
{{- define "aster.agentSandboxDashboardImage" -}}
{{- $image := .Values.agentSandbox.fixRuntime.dashboardImage -}}
{{- $tag := include "aster.resolvedImageTag" (list . $image.tag) -}}
{{- printf "%s:%s" $image.repository $tag -}}
{{- end -}}

{{/* Small image used by tests that materialize ConfigMap project files. */}}
{{- define "aster.projectMaterializerImage" -}}
{{- printf "%s:%s" .Values.project.materializer.image.repository .Values.project.materializer.image.tag -}}
{{- end -}}

{{/* Release scope for cross-namespace resource names. */}}
{{- define "aster.releaseScope" -}}
{{- printf "%s/%s" .Release.Namespace .Release.Name | sha256sum | trunc 8 -}}
{{- end -}}

{{/*
Name of the PVC the fetcher and server share.
*/}}
{{- define "aster.pvcName" -}}
{{- if .Values.persistence.existingClaim -}}
{{- .Values.persistence.existingClaim -}}
{{- else -}}
{{- printf "%s-data" (include "aster.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Name of the ConfigMap holding the consumer project config.
*/}}
{{- define "aster.projectConfigMap" -}}
{{- if .Values.project.existingConfigMap -}}
{{- .Values.project.existingConfigMap -}}
{{- else -}}
{{- printf "%s-project" (include "aster.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
ConfigMap volume items for the project config: project.yaml, system.md (mapped
to prompts/system.md), and each consumer skill recipe mapped to skills/<name> so
the engine loads them from <project_dir>/skills/. Include with nindent at the
call site.
*/}}
{{- define "aster.projectVolumeItems" -}}
- key: project.yaml
  path: project.yaml
- key: system.md
  path: prompts/system.md
{{- range $name, $content := .Values.project.skills }}
- key: {{ $name }}
  path: skills/{{ $name }}
{{- end }}
{{- end -}}

{{/* Validate Service origin and NetworkPolicy configuration. */}}
{{- define "aster.validateNetworkSecurity" -}}
{{- $service := .Values.server.service -}}
{{- $serviceType := default "ClusterIP" $service.type -}}
{{- if not (has $serviceType (list "ClusterIP" "LoadBalancer" "NodePort")) -}}
{{- fail "server.service.type must be ClusterIP, LoadBalancer, or NodePort" -}}
{{- end -}}
{{- $ranges := $service.loadBalancerSourceRanges | default list -}}
{{- range $range := $ranges -}}
{{- $range = trim $range -}}
{{- if not $range -}}{{- fail "server.service.loadBalancerSourceRanges must not contain empty entries" -}}{{- end -}}
{{- if regexMatch "/0+$" $range -}}
{{- fail "server.service.loadBalancerSourceRanges must not contain universal CIDRs; remove them and set publicOriginAcknowledged=true for an intentional public origin" -}}
{{- end -}}
{{- end -}}
{{- $externalTrafficPolicy := default "" $service.externalTrafficPolicy -}}
{{- $internal := $service.internal | default dict -}}
{{- $internalEnabled := $internal.enabled | default false -}}
{{- $internalAnnotations := $internal.annotations | default dict -}}
{{- $publicAcknowledged := $service.publicOriginAcknowledged | default false -}}
{{- $interactive := include "aster.serverInteractive" . -}}
{{- if and (gt (len $ranges) 0) (ne $serviceType "LoadBalancer") -}}
{{- fail "server.service.loadBalancerSourceRanges requires server.service.type=LoadBalancer" -}}
{{- end -}}
{{- if not (has $externalTrafficPolicy (list "" "Cluster" "Local")) -}}
{{- fail "server.service.externalTrafficPolicy must be empty, Cluster, or Local" -}}
{{- end -}}
{{- if and $externalTrafficPolicy (eq $serviceType "ClusterIP") -}}
{{- fail "server.service.externalTrafficPolicy requires LoadBalancer or NodePort" -}}
{{- end -}}
{{- if and $internalEnabled (ne $serviceType "LoadBalancer") -}}
{{- fail "server.service.internal.enabled requires server.service.type=LoadBalancer" -}}
{{- end -}}
{{- if and $internalEnabled (eq (len $internalAnnotations) 0) -}}
{{- fail "server.service.internal.annotations is required when internal.enabled=true" -}}
{{- end -}}
{{- if and $publicAcknowledged (ne $serviceType "LoadBalancer") -}}
{{- fail "server.service.publicOriginAcknowledged applies only to LoadBalancer Services" -}}
{{- end -}}
{{- if and (not .Values.networkPolicy.enabled) (gt (len (.Values.networkPolicy.ingress | default list)) 0) -}}
{{- fail "networkPolicy.ingress requires networkPolicy.enabled=true" -}}
{{- end -}}
{{- if and $interactive (eq $serviceType "LoadBalancer") (not $internalEnabled) (gt (len $ranges) 1) (not $publicAcknowledged) -}}
{{- fail "authenticated server features with multiple loadBalancerSourceRanges require publicOriginAcknowledged=true because the chart cannot prove their union is restricted" -}}
{{- end -}}
{{- if and $interactive (eq $serviceType "LoadBalancer") (not $internalEnabled) (eq (len $ranges) 0) (not $publicAcknowledged) -}}
{{- fail "authenticated server features with a LoadBalancer require loadBalancerSourceRanges, internal.enabled, or publicOriginAcknowledged=true" -}}
{{- end -}}
{{- end -}}

{{/*
Whether any admin-gated server feature is enabled. Every one of them mounts the
project config, selects an auth mode, and writes private state, so the server
templates gate those on this single answer. Emits "true" or the empty string.
*/}}
{{- define "aster.serverInteractive" -}}
{{- if or .Values.server.actions.enabled .Values.server.chat.enabled .Values.server.pullRequestEscalation.enabled -}}
true
{{- end -}}
{{- end -}}

{{/*
Name of the Secret holding the AI token.
*/}}
{{- define "aster.aiSecret" -}}
{{- if .Values.ai.existingSecret -}}
{{- .Values.ai.existingSecret -}}
{{- else -}}
{{- printf "%s-ai" (include "aster.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* Name of the Secret holding the read-only GitHub source token. */}}
{{- define "aster.githubReadSecret" -}}
{{- if .Values.ai.githubReadTokenSecretName -}}
{{- .Values.ai.githubReadTokenSecretName -}}
{{- else if and (not .Values.ai.githubReadToken) .Values.ai.existingSecret -}}
{{- .Values.ai.existingSecret -}}
{{- else -}}
{{- printf "%s-github-read" (include "aster.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "aster.validateAI" -}}
{{- if not (has .Values.ai.api (list "chat_completions" "responses")) -}}
{{- fail "ai.api must be chat_completions or responses" -}}
{{- end -}}
{{- if not (has (printf "%v" .Values.ai.reasoningEffort) (list "" "none" "low" "medium" "high" "xhigh" "max")) -}}
{{- fail "ai.reasoningEffort must be empty, none, low, medium, high, xhigh, or max" -}}
{{- end -}}
{{- if and .Values.ai.githubReadToken .Values.ai.githubReadTokenSecretName -}}
{{- fail "ai.githubReadToken and ai.githubReadTokenSecretName are mutually exclusive" -}}
{{- end -}}
{{- if and (or .Values.ai.githubReadToken .Values.ai.githubReadTokenSecretName .Values.ai.existingSecret) (not .Values.ai.githubReadTokenSecretKey) -}}
{{- fail "ai.githubReadTokenSecretKey is required when a GitHub read-token Secret is configured" -}}
{{- end -}}
{{- $contextWindow := printf "%v" .Values.ai.contextWindowTokens -}}
{{- if not (regexMatch "^(0|[1-9][0-9]{0,9})$" $contextWindow) -}}
{{- fail "ai.contextWindowTokens must be 0 or an integer from 9217 to 1000000000" -}}
{{- end -}}
{{- $contextWindowInt := int64 $contextWindow -}}
{{- if or (gt $contextWindowInt 1000000000) (and (gt $contextWindowInt 0) (lt $contextWindowInt 9217)) -}}
{{- fail "ai.contextWindowTokens must be 0 or an integer from 9217 to 1000000000" -}}
{{- end -}}
{{- end -}}

{{/*
Name of the Secret holding the server auth credentials (OAuth secret + session
key, or bot token).
*/}}
{{- define "aster.authSecret" -}}
{{- $a := .Values.server.actions -}}
{{- if eq $a.mode "oauth" -}}
{{- if $a.oauth.existingSecret -}}{{ $a.oauth.existingSecret }}{{- else -}}{{ printf "%s-auth" (include "aster.fullname" .) }}{{- end -}}
{{- else -}}
{{- if $a.proxy.existingSecret -}}{{ $a.proxy.existingSecret }}{{- else -}}{{ printf "%s-auth" (include "aster.fullname" .) }}{{- end -}}
{{- end -}}
{{- end -}}

{{/* Immutable Agent Sandbox executor image reference. */}}
{{- define "aster.agentSandboxExecutorImage" -}}
{{- printf "%s@%s" .Values.agentSandbox.fixRuntime.image.repository .Values.agentSandbox.fixRuntime.image.digest -}}
{{- end -}}

{{/* Server ServiceAccount allowed to manage only Fix Sandboxes. */}}
{{- define "aster.agentSandboxFixClientServiceAccountName" -}}
{{- if .Values.agentSandbox.rbac.fixClientServiceAccountName -}}
{{- .Values.agentSandbox.rbac.fixClientServiceAccountName -}}
{{- else -}}
{{- printf "%s-agent-sandbox-fix-client" (include "aster.agentSandboxClientNameBase" .) -}}
{{- end -}}
{{- end -}}

{{/* Stable prefix for the default Agent Sandbox client ServiceAccount name. */}}
{{- define "aster.agentSandboxClientNameBase" -}}
{{- include "aster.fullname" . | trunc 32 | trimSuffix "-" -}}
{{- end -}}

{{/* Tokenless ServiceAccount used inside the executor Sandbox. */}}
{{- define "aster.agentSandboxWorkloadServiceAccountName" -}}
{{- .Values.agentSandbox.fixRuntime.workloadServiceAccount.name -}}
{{- end -}}

{{/* Cluster-scoped admission policy name, unique to the release. */}}
{{- define "aster.agentSandboxAdmissionName" -}}
{{- printf "%s-agent-sandbox-%s" (include "aster.fullname" .) (include "aster.releaseScope" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fix-runtime execution bounds come from the consumer's project.yaml so they are
configured exactly once. agentSandboxFixRuntime validation requires inline
project.config whenever the fix runtime is enabled, so these always resolve.
*/}}
{{- define "aster.fixRuntimeProjectAgent" -}}
{{- $project := fromYaml (default "" .Values.project.config) -}}
{{- $fix := get (get $project "ai" | default dict) "fix_prs" | default dict -}}
{{- get $fix "agent_runtime" | default dict | toYaml -}}
{{- end -}}

{{- define "aster.fixRuntimeTimeout" -}}
{{- $agent := fromYaml (include "aster.fixRuntimeProjectAgent" .) -}}
{{- default "10m" (get $agent "timeout") -}}
{{- end -}}

{{- define "aster.fixRuntimeOutputLimitBytes" -}}
{{- $agent := fromYaml (include "aster.fixRuntimeProjectAgent" .) -}}
{{- printf "%d" (int64 (default 524288 (get $agent "output_limit_bytes"))) -}}
{{- end -}}

{{/* Non-secret Agent Sandbox runtime environment shared by server and fetcher. */}}
{{- define "aster.agentSandboxEnv" -}}
- name: AGENT_SANDBOX_NAMESPACE
  value: {{ .Values.agentSandbox.fixRuntime.namespace | quote }}
- name: AGENT_SANDBOX_IMAGE
  value: {{ include "aster.agentSandboxExecutorImage" . | quote }}
- name: AGENT_SANDBOX_SERVICE_ACCOUNT
  value: {{ include "aster.agentSandboxWorkloadServiceAccountName" . | quote }}
- name: AGENT_SANDBOX_RUNTIME_CLASS
  value: {{ .Values.agentSandbox.fixRuntime.runtimeClassName | quote }}
- name: AGENT_SANDBOX_MODEL_PROVIDER_CREDENTIAL_MODE
  value: {{ .Values.agentSandbox.fixRuntime.modelProvider.credentialMode | quote }}
- name: AGENT_SANDBOX_MODEL_PROVIDER_API
  value: {{ .Values.agentSandbox.fixRuntime.modelProvider.api | quote }}
- name: AGENT_SANDBOX_MODEL_PROVIDER_ENDPOINT
  value: {{ .Values.agentSandbox.fixRuntime.modelProvider.endpoint | quote }}
- name: AGENT_SANDBOX_MODEL_PROVIDER_MODEL
  value: {{ .Values.agentSandbox.fixRuntime.modelProvider.model | quote }}
{{- if .Values.agentSandbox.fixRuntime.modelProvider.reasoningEffort }}
- name: AGENT_SANDBOX_MODEL_PROVIDER_REASONING_EFFORT
  value: {{ .Values.agentSandbox.fixRuntime.modelProvider.reasoningEffort | quote }}
{{- end }}
- name: AGENT_SANDBOX_MODEL_PROVIDER_AUTH_TYPE
  value: {{ .Values.agentSandbox.fixRuntime.modelProvider.auth.type | quote }}
- name: AGENT_SANDBOX_MODEL_PROVIDER_AUTH_SECRET_NAME
  value: {{ .Values.agentSandbox.fixRuntime.modelProvider.auth.existingSecret | quote }}
- name: AGENT_SANDBOX_MODEL_PROVIDER_AUTH_SECRET_KEY
  value: {{ .Values.agentSandbox.fixRuntime.modelProvider.auth.tokenKey | quote }}
- name: AGENT_SANDBOX_MODEL_PROVIDER_PUBLIC_CA_PRIVATE_DNS
  value: {{ ternary "true" "false" .Values.agentSandbox.fixRuntime.modelProvider.publicCAPrivateDNS | quote }}
{{- if .Values.agentSandbox.fixRuntime.caBundle.existingConfigMap }}
- name: AGENT_SANDBOX_MODEL_PROVIDER_CA_CONFIG_MAP
  value: {{ .Values.agentSandbox.fixRuntime.caBundle.existingConfigMap | quote }}
- name: AGENT_SANDBOX_MODEL_PROVIDER_CA_KEY
  value: {{ .Values.agentSandbox.fixRuntime.caBundle.key | quote }}
- name: AGENT_SANDBOX_MODEL_PROVIDER_CA_SHA256
  value: {{ .Values.agentSandbox.fixRuntime.caBundle.sha256 | quote }}
{{- end }}
- name: AGENT_SANDBOX_TIMEOUT
  value: {{ include "aster.fixRuntimeTimeout" . | quote }}
- name: AGENT_SANDBOX_OUTPUT_LIMIT_BYTES
  value: {{ include "aster.fixRuntimeOutputLimitBytes" . | quote }}
- name: AGENT_SANDBOX_POLL_INTERVAL
  value: {{ .Values.agentSandbox.fixRuntime.pollInterval | quote }}
- name: AGENT_SANDBOX_CPU_REQUEST
  value: {{ index .Values.agentSandbox.fixRuntime.resources.requests "cpu" | quote }}
- name: AGENT_SANDBOX_CPU_LIMIT
  value: {{ index .Values.agentSandbox.fixRuntime.resources.limits "cpu" | quote }}
- name: AGENT_SANDBOX_MEMORY_REQUEST
  value: {{ index .Values.agentSandbox.fixRuntime.resources.requests "memory" | quote }}
- name: AGENT_SANDBOX_MEMORY_LIMIT
  value: {{ index .Values.agentSandbox.fixRuntime.resources.limits "memory" | quote }}
- name: AGENT_SANDBOX_EPHEMERAL_STORAGE_LIMIT
  value: {{ index .Values.agentSandbox.fixRuntime.resources.limits "ephemeral-storage" | quote }}
{{- end -}}

{{/* Validate the disabled-by-default consumer-installed Agent Sandbox runtime. */}}
{{- define "aster.validateAgentSandboxFixRuntime" -}}
{{- $cfg := .Values.agentSandbox.fixRuntime -}}
{{- $ca := $cfg.caBundle -}}
{{- $caCount := add (ternary 1 0 (ne $ca.existingConfigMap "")) (ternary 1 0 (ne $ca.key "")) (ternary 1 0 (ne $ca.sha256 "")) -}}
{{- if and (not $cfg.enabled) (gt $caCount 0) -}}{{- fail "agentSandbox.fixRuntime.caBundle requires agentSandbox.fixRuntime.enabled=true" -}}{{- end -}}
{{- if and (ne $caCount 0) (ne $caCount 3) -}}{{- fail "agentSandbox.fixRuntime.caBundle requires existingConfigMap, key, and sha256 together" -}}{{- end -}}
{{- if eq $caCount 3 -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $ca.existingConfigMap) -}}{{- fail "agentSandbox.fixRuntime.caBundle.existingConfigMap must be a lowercase DNS subdomain" -}}{{- end -}}
  {{- if not (regexMatch "^[A-Za-z0-9._-]+$" $ca.key) -}}{{- fail "agentSandbox.fixRuntime.caBundle.key must be a valid ConfigMap key" -}}{{- end -}}
  {{- if not (regexMatch "^[0-9a-f]{64}$" $ca.sha256) -}}{{- fail "agentSandbox.fixRuntime.caBundle.sha256 must be 64 lowercase hexadecimal characters" -}}{{- end -}}
{{- end -}}
{{- if $cfg.enabled -}}
  {{- if .Values.project.existingConfigMap -}}{{- fail "agentSandbox.fixRuntime requires inline project.config so security-sensitive values can be compared" -}}{{- end -}}
  {{- if not .Values.project.config -}}{{- fail "agentSandbox.fixRuntime requires project.config" -}}{{- end -}}
  {{- $project := fromYaml .Values.project.config -}}
  {{- $projectAI := get $project "ai" | default dict -}}
  {{- $projectFix := get $projectAI "fix_prs" | default dict -}}
  {{- $projectRuntime := get $projectFix "agent_runtime" | default dict -}}
  {{- $projectProvider := get $projectRuntime "model_provider" | default dict -}}
  {{- $maxSteps := int (default 30 (get $projectRuntime "max_turns")) -}}
  {{- $maxFiles := int (default 3 (get $projectFix "max_files")) -}}
  {{- $fixTimeout := default "10m" (get $projectRuntime "timeout") -}}
  {{- $outputLimitBytes := int64 (default 524288 (get $projectRuntime "output_limit_bytes")) -}}
  {{- $allowedCommands := get $projectRuntime "allowed_commands" | default list -}}
  {{- if ne (default "agent-sandbox" (get $projectRuntime "type")) "agent-sandbox" -}}{{- fail "agentSandbox.fixRuntime requires project ai.fix_prs.agent_runtime.type=agent-sandbox" -}}{{- end -}}
  {{- if not .Values.server.actions.enabled -}}{{- fail "agentSandbox.fixRuntime requires server.actions.enabled=true; Fix generation is a maintainer-initiated server action" -}}{{- end -}}
  {{- if .Values.server.actions.oauth.privateRepositories -}}{{- fail "agentSandbox.fixRuntime supports public repositories only; OAuth privateRepositories must be false" -}}{{- end -}}
  {{- if not $cfg.namespace -}}{{- fail "agentSandbox.fixRuntime.namespace is required" -}}{{- end -}}
  {{- if eq $cfg.namespace .Release.Namespace -}}{{- fail "agentSandbox.fixRuntime.namespace must differ from the dashboard release namespace" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $cfg.namespace) -}}{{- fail "agentSandbox.fixRuntime.namespace must be a lowercase DNS label" -}}{{- end -}}
  {{- if not $cfg.runtimeClassName -}}{{- fail "agentSandbox.fixRuntime.runtimeClassName is required" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $cfg.runtimeClassName) -}}{{- fail "agentSandbox.fixRuntime.runtimeClassName must be a lowercase RuntimeClass name" -}}{{- end -}}
  {{- if not $cfg.image.repository -}}{{- fail "agentSandbox.fixRuntime.image.repository is required" -}}{{- end -}}
  {{- if not (regexMatch "^[^[:space:]@]+$" $cfg.image.repository) -}}{{- fail "agentSandbox.fixRuntime.image.repository must not contain whitespace, credentials, or a digest" -}}{{- end -}}
  {{- if not (regexMatch "^sha256:[0-9a-f]{64}$" $cfg.image.digest) -}}{{- fail "agentSandbox.fixRuntime.image.digest must be an immutable sha256 digest" -}}{{- end -}}
  {{- if ne $cfg.image.pullPolicy "IfNotPresent" -}}{{- fail "agentSandbox.fixRuntime.image.pullPolicy must be IfNotPresent" -}}{{- end -}}
  {{- $dashboardImage := $cfg.dashboardImage -}}
  {{- if not $dashboardImage.repository -}}{{- fail "agentSandbox.fixRuntime.dashboardImage.repository is required" -}}{{- end -}}
  {{- if not (regexMatch "^[^[:space:]@]+$" $dashboardImage.repository) -}}{{- fail "agentSandbox.fixRuntime.dashboardImage.repository must not contain whitespace, credentials, or a digest" -}}{{- end -}}
  {{- $dashboardTag := include "aster.resolvedImageTag" (list . $dashboardImage.tag) -}}
  {{- if not (regexMatch "^(sha-[0-9a-fA-F]{7,64}|v?[0-9]+[.][0-9]+[.][0-9]+(-[0-9A-Za-z.-]+)?)$" $dashboardTag) -}}{{- fail "agentSandbox.fixRuntime.dashboardImage tag must be an immutable sha-<hex> or full semantic version" -}}{{- end -}}
  {{- if ne $dashboardImage.pullPolicy "IfNotPresent" -}}{{- fail "agentSandbox.fixRuntime.dashboardImage.pullPolicy must be IfNotPresent" -}}{{- end -}}
  {{- $workloadSA := include "aster.agentSandboxWorkloadServiceAccountName" . -}}
  {{- if not $workloadSA -}}{{- fail "agentSandbox.fixRuntime.workloadServiceAccount.name is required" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $workloadSA) -}}{{- fail "agentSandbox.fixRuntime.workloadServiceAccount.name must be a lowercase Kubernetes object name" -}}{{- end -}}
  {{- $clientSA := include "aster.agentSandboxFixClientServiceAccountName" . -}}
  {{- if and (not .Values.agentSandbox.rbac.create) (not .Values.agentSandbox.rbac.fixClientServiceAccountName) -}}{{- fail "agentSandbox.rbac.fixClientServiceAccountName is required when chart-managed RBAC is disabled" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $clientSA) -}}{{- fail "agentSandbox.rbac.fixClientServiceAccountName must be a lowercase Kubernetes object name" -}}{{- end -}}
  {{- $provider := $cfg.modelProvider -}}
  {{- $credentialMode := default "direct" $provider.credentialMode -}}
  {{- $providerAPI := default "chat_completions" $provider.api -}}
  {{- $providerAuth := $provider.auth -}}
  {{- $authType := default "none" $providerAuth.type -}}
  {{- $publicCAPrivateDNS := default false $provider.publicCAPrivateDNS -}}
  {{- if not (has $credentialMode (list "direct" "gateway")) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.credentialMode must be direct or gateway" -}}{{- end -}}
  {{- if not (has $providerAPI (list "chat_completions" "responses")) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.api must be chat_completions or responses" -}}{{- end -}}
  {{- if not (has (default "" $provider.reasoningEffort) (list "" "none" "low" "medium" "high" "xhigh")) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.reasoningEffort must be empty, none, low, medium, high, or xhigh with pinned OpenCode 1.18.2" -}}{{- end -}}
  {{- if not (regexMatch "^https://[^/@?#]+(:[0-9]+)?(/[A-Za-z0-9._~!$&()*+,;=:@%/-]*)?$" $provider.endpoint) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.endpoint must be an absolute credential-free HTTPS URL" -}}{{- end -}}
  {{- if and (eq $providerAPI "chat_completions") (not (hasSuffix "/chat/completions" (trimSuffix "/" $provider.endpoint))) -}}{{- fail "agentSandbox.fixRuntime.modelProvider chat_completions endpoint must end with /chat/completions" -}}{{- end -}}
  {{- if and (eq $providerAPI "responses") (not (hasSuffix "/responses" (trimSuffix "/" $provider.endpoint))) -}}{{- fail "agentSandbox.fixRuntime.modelProvider responses endpoint must end with /responses" -}}{{- end -}}
  {{- if or (not $provider.model) (gt (len $provider.model) 256) (contains "\n" $provider.model) (contains "\r" $provider.model) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.model must be non-empty, at most 256 bytes, and single-line" -}}{{- end -}}
  {{- if not (has $authType (list "none" "bearer")) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.auth.type must be none or bearer" -}}{{- end -}}
  {{- if and (eq $providerAPI "responses") (or (ne $credentialMode "direct") (ne $authType "bearer")) -}}{{- fail "agentSandbox.fixRuntime.modelProvider responses requires direct bearer auth with the pinned OpenCode provider" -}}{{- end -}}
  {{- if eq $credentialMode "gateway" -}}
    {{- if ne $authType "none" -}}{{- fail "agentSandbox.fixRuntime.modelProvider gateway mode requires auth.type=none" -}}{{- end -}}
    {{- if or $providerAuth.existingSecret $providerAuth.tokenKey -}}{{- fail "agentSandbox.fixRuntime.modelProvider gateway mode must not set Secret fields" -}}{{- end -}}
    {{- if $publicCAPrivateDNS -}}
      {{- if not (regexMatch "^https://([A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?[.])+[A-Za-z]{2,}(:[0-9]+)?(/[^?#]*)?$" $provider.endpoint) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.endpoint must be an HTTPS DNS FQDN when publicCAPrivateDNS=true" -}}{{- end -}}
      {{- if regexMatch "^https://([^/@?#]+[.])?(openai[.]com|openai[.]azure[.]com|services[.]ai[.]azure[.]com|anthropic[.]com|githubcopilot[.]com|copilot[.]microsoft[.]com|moonshot[.]cn|kimi[.]com|generativelanguage[.]googleapis[.]com|api[.]nvidia[.]com|mistral[.]ai|cohere[.]ai|groq[.]com|together[.]xyz|deepseek[.]com|x[.]ai)(:[0-9]+)?(/|$)" (lower $provider.endpoint) -}}{{- fail "agentSandbox.fixRuntime.modelProvider gateway endpoint must not be a direct model-provider endpoint" -}}{{- end -}}
      {{- if regexMatch "[.](svc([.]cluster[.]local)?|internal)(:[0-9]+)?(/|$)" (lower $provider.endpoint) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.publicCAPrivateDNS applies only to a privately resolved public FQDN" -}}{{- end -}}
    {{- else -}}
      {{- if not (regexMatch "^https://[^/@?#]+[.](svc([.]cluster[.]local)?|internal)(:[0-9]+)?(/[^?#]*)?$" $provider.endpoint) -}}{{- fail "agentSandbox.fixRuntime.modelProvider gateway endpoint must be an internal HTTPS service URL or publicCAPrivateDNS must be true" -}}{{- end -}}
    {{- end -}}
  {{- else -}}
    {{- if $publicCAPrivateDNS -}}{{- fail "agentSandbox.fixRuntime.modelProvider.publicCAPrivateDNS applies only to gateway mode" -}}{{- end -}}
    {{- if eq $authType "none" -}}
      {{- if or $providerAuth.existingSecret $providerAuth.tokenKey -}}{{- fail "agentSandbox.fixRuntime.modelProvider auth.type=none must not set Secret fields" -}}{{- end -}}
    {{- else -}}
      {{- if or (not $providerAuth.existingSecret) (gt (len $providerAuth.existingSecret) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $providerAuth.existingSecret)) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.auth.existingSecret is required for bearer auth and must be a valid Secret name" -}}{{- end -}}
      {{- if or (not $providerAuth.tokenKey) (gt (len $providerAuth.tokenKey) 253) (not (regexMatch "^[A-Za-z0-9._-]+$" $providerAuth.tokenKey)) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.auth.tokenKey is required for bearer auth and must be a valid Secret key" -}}{{- end -}}
    {{- end -}}
  {{- end -}}
  {{- if not (regexMatch "^([1-9]|[12][0-9]|30)m$" (printf "%v" $fixTimeout)) -}}{{- fail "project ai.fix_prs.agent_runtime.timeout must use whole minutes from 1m through 30m" -}}{{- end -}}
  {{- $poll := printf "%v" $cfg.pollInterval -}}
  {{- if or (not (regexMatch "^(([0-9]+([.][0-9]+)?)|([.][0-9]+))(ms|s)$" $poll)) (not (regexMatch "[1-9]" $poll)) -}}{{- fail "agentSandbox.fixRuntime.pollInterval must be a positive duration below 30s" -}}{{- end -}}
  {{- if regexMatch "^([3-9][0-9]|[1-9][0-9]{2,})s$" (durationRound $poll) -}}{{- fail "agentSandbox.fixRuntime.pollInterval must be below 30s" -}}{{- end -}}
  {{- if or (lt $outputLimitBytes 4096) (gt $outputLimitBytes 1048576) -}}{{- fail "project ai.fix_prs.agent_runtime.output_limit_bytes must be between 4096 and 1048576" -}}{{- end -}}
  {{- $overallSeconds := mul (trimSuffix "m" (printf "%v" $fixTimeout) | int) 60 -}}
  {{- if ge (len $allowedCommands) $maxSteps -}}{{- fail "project ai.fix_prs.agent_runtime.max_turns must reserve at least one coding-agent step after allowed_commands" -}}{{- end -}}
  {{- range $index, $command := $allowedCommands -}}
    {{- $argv := get $command "argv" | default list -}}
    {{- if eq (len $argv) 0 -}}{{- fail (printf "project ai.fix_prs.agent_runtime.allowed_commands[%d].argv must be non-empty" $index) -}}{{- end -}}
    {{- $totalBytes := 0 -}}
    {{- range $argIndex, $arg := $argv -}}
      {{- if or (eq $arg "") (gt (len $arg) 1024) (regexMatch "[\r\n]" $arg) -}}{{- fail (printf "project ai.fix_prs.agent_runtime.allowed_commands[%d].argv[%d] must be a bounded non-empty single-line string" $index $argIndex) -}}{{- end -}}
      {{- $totalBytes = add $totalBytes (len $arg) -}}
    {{- end -}}
    {{- if gt $totalBytes 4096 -}}{{- fail (printf "project ai.fix_prs.agent_runtime.allowed_commands[%d].argv exceeds 4096 bytes" $index) -}}{{- end -}}
    {{- $executable := lower (trim (first $argv)) -}}
    {{- if or (contains "/" (first $argv)) (contains "\\" (first $argv)) -}}{{- fail (printf "project ai.fix_prs.agent_runtime.allowed_commands[%d] must use a PATH-resolved executable" $index) -}}{{- end -}}
    {{- if has $executable (list "sh" "bash" "dash" "zsh" "ksh" "fish" "cmd" "cmd.exe" "powershell" "pwsh") -}}{{- fail (printf "project ai.fix_prs.agent_runtime.allowed_commands[%d] must not invoke a shell" $index) -}}{{- end -}}
    {{- if has $executable (list "env" "busybox" "toybox") -}}{{- fail (printf "project ai.fix_prs.agent_runtime.allowed_commands[%d] must not use a command dispatcher" $index) -}}{{- end -}}
    {{- if has $executable (list "opencode" "fixexecutor") -}}{{- fail (printf "project ai.fix_prs.agent_runtime.allowed_commands[%d] must not invoke a coding agent or executor" $index) -}}{{- end -}}
    {{- if and (eq $executable "git") (ne (toJson $argv) (toJson (list "git" "diff" "--cached" "--check"))) -}}{{- fail (printf "project ai.fix_prs.agent_runtime.allowed_commands[%d] git is reserved for the exact final diff check" $index) -}}{{- end -}}
    {{- $commandTimeout := printf "%v" (get $command "timeout") -}}
    {{- $commandSeconds := 0 -}}
    {{- if regexMatch "^[1-9][0-9]*s$" $commandTimeout -}}
      {{- if gt (len $commandTimeout) 5 -}}{{- fail (printf "project ai.fix_prs.agent_runtime.allowed_commands[%d].timeout exceeds the execution timeout" $index) -}}{{- end -}}
      {{- $commandSeconds = trimSuffix "s" $commandTimeout | int -}}
    {{- else if regexMatch "^[1-9][0-9]*m$" $commandTimeout -}}
      {{- if gt (len $commandTimeout) 3 -}}{{- fail (printf "project ai.fix_prs.agent_runtime.allowed_commands[%d].timeout exceeds the execution timeout" $index) -}}{{- end -}}
      {{- $commandSeconds = mul (trimSuffix "m" $commandTimeout | int) 60 -}}
    {{- else -}}
      {{- fail (printf "project ai.fix_prs.agent_runtime.allowed_commands[%d].timeout must use positive whole seconds or minutes" $index) -}}
    {{- end -}}
    {{- if gt $commandSeconds $overallSeconds -}}{{- fail (printf "project ai.fix_prs.agent_runtime.allowed_commands[%d].timeout exceeds the execution timeout" $index) -}}{{- end -}}
  {{- end -}}
  {{- if or (eq (len $allowedCommands) 0) (ne (toJson (get (last $allowedCommands) "argv")) (toJson (list "git" "diff" "--cached" "--check"))) -}}{{- fail "project ai.fix_prs.agent_runtime.allowed_commands must end with argv [git diff --cached --check]" -}}{{- end -}}
  {{- $projectProviderAuth := get $projectProvider "auth" | default dict -}}
  {{- if ne $credentialMode (default "direct" (get $projectProvider "credential_mode")) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.credentialMode must match project agent_runtime.model_provider.credential_mode" -}}{{- end -}}
  {{- if ne $providerAPI (default "chat_completions" (get $projectProvider "api")) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.api must match project agent_runtime.model_provider.api" -}}{{- end -}}
  {{- if ne $provider.endpoint (default "" (get $projectProvider "endpoint")) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.endpoint must match project agent_runtime.model_provider.endpoint" -}}{{- end -}}
  {{- if ne $provider.model (default "" (get $projectProvider "model")) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.model must match project agent_runtime.model_provider.model" -}}{{- end -}}
  {{- if ne (default "" $provider.reasoningEffort) (default "" (get $projectProvider "reasoning_effort")) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.reasoningEffort must match project agent_runtime.model_provider.reasoning_effort" -}}{{- end -}}
  {{- if ne $authType (default "none" (get $projectProviderAuth "type")) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.auth.type must match project agent_runtime.model_provider.auth.type" -}}{{- end -}}
  {{- if ne $publicCAPrivateDNS (default false (get $projectProvider "public_ca_private_dns")) -}}{{- fail "agentSandbox.fixRuntime.modelProvider.publicCAPrivateDNS must match project agent_runtime.model_provider.public_ca_private_dns" -}}{{- end -}}
  {{- if (default false (get $projectRuntime "allow_bash")) -}}{{- fail "agentSandbox.fixRuntime requires project agent_runtime.allow_bash=false" -}}{{- end -}}
  {{- range $env := concat .Values.server.extraEnv .Values.fetcher.extraEnv -}}
    {{- if hasPrefix "AGENT_SANDBOX_" (default "" $env.name) -}}{{- fail (printf "extraEnv must not override reserved Agent Sandbox variable %s" $env.name) -}}{{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}

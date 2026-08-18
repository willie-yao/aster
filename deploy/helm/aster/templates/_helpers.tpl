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
{{- $interactive := or .Values.server.actions.enabled .Values.server.chat.enabled .Values.server.remediationInvestigation.enabled -}}
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
{{- fail "authenticated actions or chat with multiple loadBalancerSourceRanges require publicOriginAcknowledged=true because the chart cannot prove their union is restricted" -}}
{{- end -}}
{{- if and $interactive (eq $serviceType "LoadBalancer") (not $internalEnabled) (eq (len $ranges) 0) (not $publicAcknowledged) -}}
{{- fail "authenticated actions or chat with a LoadBalancer require loadBalancerSourceRanges, internal.enabled, or publicOriginAcknowledged=true" -}}
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

{{/* Immutable analysis shadow executor image. */}}
{{- define "aster.agentAnalysisShadowExecutorImage" -}}
{{- printf "%s@%s" .Values.agentSandbox.analysisShadow.image.repository .Values.agentSandbox.analysisShadow.image.digest -}}
{{- end -}}

{{/* Tokenless ServiceAccount used inside analysis shadow Sandboxes. */}}
{{- define "aster.agentAnalysisShadowWorkloadServiceAccountName" -}}
{{- .Values.agentSandbox.analysisShadow.workloadServiceAccount.name -}}
{{- end -}}

{{/* Cluster-scoped analysis shadow admission policy name. */}}
{{- define "aster.agentAnalysisShadowAdmissionName" -}}
{{- printf "%s-agent-sandbox-shadow-%s" (include "aster.fullname" .) (include "aster.releaseScope" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "aster.agentAnalysisShadowLedgerPath" -}}
{{- printf "%s/analysis_shadow.json" (trimSuffix "/" .Values.agentSandbox.analysisShadow.ledger.mountPath) -}}
{{- end -}}

{{/* Non-secret analysis shadow runtime environment for scheduled fetcher or worker. */}}
{{- define "aster.agentAnalysisShadowEnv" -}}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_NAMESPACE
  value: {{ .Values.agentSandbox.analysisShadow.namespace | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_IMAGE
  value: {{ include "aster.agentAnalysisShadowExecutorImage" . | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_SERVICE_ACCOUNT
  value: {{ include "aster.agentAnalysisShadowWorkloadServiceAccountName" . | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_RUNTIME_CLASS
  value: {{ .Values.agentSandbox.analysisShadow.runtimeClassName | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_MODEL_PROVIDER_CREDENTIAL_MODE
  value: {{ .Values.agentSandbox.analysisShadow.modelProvider.credentialMode | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_MODEL_PROVIDER_API
  value: {{ .Values.agentSandbox.analysisShadow.modelProvider.api | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_MODEL_PROVIDER_ENDPOINT
  value: {{ .Values.agentSandbox.analysisShadow.modelProvider.endpoint | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_MODEL_PROVIDER_MODEL
  value: {{ .Values.agentSandbox.analysisShadow.modelProvider.model | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_MODEL_PROVIDER_AUTH_TYPE
  value: "none"
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_MODEL_PROVIDER_PUBLIC_CA_PRIVATE_DNS
  value: "false"
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_TIMEOUT
  value: {{ .Values.agentSandbox.analysisShadow.timeout | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_OUTPUT_LIMIT_BYTES
  value: {{ printf "%d" (int64 .Values.agentSandbox.analysisShadow.outputLimitBytes) | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_POLL_INTERVAL
  value: {{ .Values.agentSandbox.analysisShadow.pollInterval | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_CPU_REQUEST
  value: {{ index .Values.agentSandbox.analysisShadow.resources.requests "cpu" | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_CPU_LIMIT
  value: {{ index .Values.agentSandbox.analysisShadow.resources.limits "cpu" | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_MEMORY_REQUEST
  value: {{ index .Values.agentSandbox.analysisShadow.resources.requests "memory" | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_MEMORY_LIMIT
  value: {{ index .Values.agentSandbox.analysisShadow.resources.limits "memory" | quote }}
- name: AGENT_SANDBOX_ANALYSIS_SHADOW_EPHEMERAL_STORAGE_LIMIT
  value: {{ index .Values.agentSandbox.analysisShadow.resources.limits "ephemeral-storage" | quote }}
{{- end -}}

{{/* Validate the disabled-by-default private Agent analysis shadow. */}}
{{- define "aster.validateAgentAnalysisShadow" -}}
{{- if .Values.agentSandbox.analysisShadow.enabled -}}
  {{- $cfg := .Values.agentSandbox.analysisShadow -}}
  {{- if not .Values.ai.enabled -}}{{- fail "agentSandbox.analysisShadow requires ai.enabled=true" -}}{{- end -}}
  {{- if ne .Values.analysisRuntime.type "inprocess" -}}{{- fail "agentSandbox.analysisShadow requires analysisRuntime.type=inprocess" -}}{{- end -}}
  {{- if .Values.agentSandbox.fixRuntime.enabled -}}{{- fail "agentSandbox.analysisShadow cannot run with agentSandbox.fixRuntime" -}}{{- end -}}
  {{- if .Values.agentSandbox.causalCritic.enabled -}}{{- fail "agentSandbox.analysisShadow cannot run with agentSandbox.causalCritic" -}}{{- end -}}
  {{- if and (eq .Values.mode "cron") (ne .Values.fetcher.concurrencyPolicy "Forbid") -}}{{- fail "agentSandbox.analysisShadow requires fetcher.concurrencyPolicy=Forbid in cron mode" -}}{{- end -}}
  {{- if not $cfg.namespace -}}{{- fail "agentSandbox.analysisShadow.namespace is required" -}}{{- end -}}
  {{- if eq $cfg.namespace .Release.Namespace -}}{{- fail "agentSandbox.analysisShadow.namespace must differ from the dashboard release namespace" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $cfg.namespace) -}}{{- fail "agentSandbox.analysisShadow.namespace must be a lowercase DNS label" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $cfg.runtimeClassName) -}}{{- fail "agentSandbox.analysisShadow.runtimeClassName is required and must be a lowercase RuntimeClass name" -}}{{- end -}}
  {{- if or (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $cfg.agentVersion)) (gt (len $cfg.agentVersion) 30) -}}{{- fail "agentSandbox.analysisShadow.agentVersion must be a lowercase DNS label of at most 30 characters" -}}{{- end -}}
  {{- if not (regexMatch "^[^[:space:]@]+$" $cfg.image.repository) -}}{{- fail "agentSandbox.analysisShadow.image.repository is required without whitespace, credentials, or a digest" -}}{{- end -}}
  {{- if not (regexMatch "^sha256:[0-9a-f]{64}$" $cfg.image.digest) -}}{{- fail "agentSandbox.analysisShadow.image.digest must be an immutable sha256 digest" -}}{{- end -}}
  {{- if ne $cfg.image.pullPolicy "IfNotPresent" -}}{{- fail "agentSandbox.analysisShadow.image.pullPolicy must be IfNotPresent" -}}{{- end -}}
  {{- $workloadSA := include "aster.agentAnalysisShadowWorkloadServiceAccountName" . -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $workloadSA) -}}{{- fail "agentSandbox.analysisShadow.workloadServiceAccount.name is required and must be a lowercase object name" -}}{{- end -}}
  {{- $clientSA := include "aster.agentSandboxClientServiceAccountName" . -}}
  {{- if and (not .Values.agentSandbox.rbac.create) (not .Values.agentSandbox.rbac.clientServiceAccountName) -}}{{- fail "agentSandbox.rbac.clientServiceAccountName is required when chart-managed RBAC is disabled" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $clientSA) -}}{{- fail "agentSandbox.rbac.clientServiceAccountName must be a lowercase Kubernetes object name" -}}{{- end -}}
  {{- if not $cfg.ledger.existingClaim -}}{{- fail "agentSandbox.analysisShadow.ledger.existingClaim is required" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $cfg.ledger.existingClaim) -}}{{- fail "agentSandbox.analysisShadow.ledger.existingClaim must be a lowercase object name" -}}{{- end -}}
  {{- if eq $cfg.ledger.existingClaim (include "aster.pvcName" .) -}}{{- fail "agentSandbox.analysisShadow must use a PVC distinct from public dashboard data" -}}{{- end -}}
  {{- if not (hasPrefix "/private/" $cfg.ledger.mountPath) -}}{{- fail "agentSandbox.analysisShadow.ledger.mountPath must be under /private" -}}{{- end -}}
  {{- if or (contains ".." $cfg.ledger.mountPath) (contains "//" $cfg.ledger.mountPath) -}}{{- fail "agentSandbox.analysisShadow.ledger.mountPath must be canonical" -}}{{- end -}}
  {{- if or (hasPrefix .Values.persistence.mountPath $cfg.ledger.mountPath) (hasPrefix $cfg.ledger.mountPath .Values.persistence.mountPath) -}}{{- fail "agentSandbox.analysisShadow ledger must be separate from public dashboard persistence" -}}{{- end -}}
  {{- $provider := $cfg.modelProvider -}}
  {{- $credentialMode := default "direct" $provider.credentialMode -}}
  {{- $providerAPI := default "chat_completions" $provider.api -}}
  {{- if not (has $credentialMode (list "direct" "gateway")) -}}{{- fail "agentSandbox.analysisShadow.modelProvider.credentialMode must be direct or gateway" -}}{{- end -}}
  {{- if ne $providerAPI "chat_completions" -}}{{- fail "agentSandbox.analysisShadow.modelProvider.api must be chat_completions" -}}{{- end -}}
  {{- if not (regexMatch "^https://[^/@?#]+(:[0-9]+)?(/[A-Za-z0-9._~!$&()*+,;=:@%/-]*)?$" $provider.endpoint) -}}{{- fail "agentSandbox.analysisShadow.modelProvider.endpoint must be an absolute credential-free HTTPS URL" -}}{{- end -}}
  {{- if and (eq $providerAPI "chat_completions") (not (hasSuffix "/chat/completions" (trimSuffix "/" $provider.endpoint))) -}}{{- fail "agentSandbox.analysisShadow.modelProvider chat_completions endpoint must end with /chat/completions" -}}{{- end -}}
  {{- if or (not $provider.model) (gt (len $provider.model) 256) (contains "\n" $provider.model) (contains "\r" $provider.model) -}}{{- fail "agentSandbox.analysisShadow.modelProvider.model must be non-empty, at most 256 bytes, and single-line" -}}{{- end -}}
  {{- if eq $credentialMode "gateway" -}}
    {{- if not (regexMatch "^https://[^/@?#]+[.](svc([.]cluster[.]local)?|internal)(:[0-9]+)?(/[^?#]*)?$" $provider.endpoint) -}}{{- fail "agentSandbox.analysisShadow.modelProvider gateway endpoint must be an internal HTTPS service URL" -}}{{- end -}}
  {{- end -}}
  {{- $timeoutText := printf "%v" $cfg.timeout -}}
  {{- $timeoutSeconds := 0 -}}
  {{- if regexMatch "^[1-9][0-9]*s$" $timeoutText -}}
    {{- if gt (len $timeoutText) 5 -}}{{- fail "agentSandbox.analysisShadow.timeout must be at most 30m" -}}{{- end -}}
    {{- $timeoutSeconds = trimSuffix "s" $timeoutText | int -}}
  {{- else if regexMatch "^[1-9][0-9]*m$" $timeoutText -}}
    {{- if gt (len $timeoutText) 3 -}}{{- fail "agentSandbox.analysisShadow.timeout must be at most 30m" -}}{{- end -}}
    {{- $timeoutSeconds = mul (trimSuffix "m" $timeoutText | int) 60 -}}
  {{- else -}}
    {{- fail "agentSandbox.analysisShadow.timeout must use positive whole seconds or minutes" -}}
  {{- end -}}
  {{- if gt $timeoutSeconds 1800 -}}{{- fail "agentSandbox.analysisShadow.timeout must be at most 30m" -}}{{- end -}}
  {{- $poll := printf "%v" $cfg.pollInterval -}}
  {{- if or (not (regexMatch "^(([0-9]+([.][0-9]+)?)|([.][0-9]+))(ms|s)$" $poll)) (not (regexMatch "[1-9]" $poll)) -}}{{- fail "agentSandbox.analysisShadow.pollInterval must be a positive duration below 30s" -}}{{- end -}}
  {{- if regexMatch "^([3-9][0-9]|[1-9][0-9]{2,})s$" (durationRound $poll) -}}{{- fail "agentSandbox.analysisShadow.pollInterval must be below 30s" -}}{{- end -}}
  {{- if or (lt (int64 $cfg.outputLimitBytes) 4096) (gt (int64 $cfg.outputLimitBytes) 1048576) -}}{{- fail "agentSandbox.analysisShadow.outputLimitBytes must be between 4096 and 1048576" -}}{{- end -}}
  {{- if or (lt (int $cfg.maxPerRun) 1) (gt (int $cfg.maxPerRun) 10) -}}{{- fail "agentSandbox.analysisShadow.maxPerRun must be between 1 and 10" -}}{{- end -}}
  {{- if or (lt (int $cfg.maxTurns) 1) (gt (int $cfg.maxTurns) 1000) -}}{{- fail "agentSandbox.analysisShadow.maxTurns must be between 1 and 1000" -}}{{- end -}}
  {{- if or (lt (int $cfg.retries) 0) (gt (int $cfg.retries) 2) -}}{{- fail "agentSandbox.analysisShadow.retries must be between 0 and 2" -}}{{- end -}}
  {{- if ne (index $cfg.resources.requests "ephemeral-storage") (index $cfg.resources.limits "ephemeral-storage") -}}{{- fail "agentSandbox.analysisShadow ephemeral-storage request must equal its limit" -}}{{- end -}}
  {{- if not $cfg.networkPolicy.enabled -}}{{- fail "agentSandbox.analysisShadow.networkPolicy.enabled must be true" -}}{{- end -}}
  {{- if not (has $cfg.networkPolicy.mode (list "kubernetes" "cilium")) -}}{{- fail "agentSandbox.analysisShadow.networkPolicy.mode must be kubernetes or cilium" -}}{{- end -}}
  {{- if eq $credentialMode "gateway" -}}
    {{- if eq (len $cfg.networkPolicy.gatewayNamespaceSelector) 0 -}}{{- fail "agentSandbox.analysisShadow.networkPolicy.gatewayNamespaceSelector is required for gateway mode" -}}{{- end -}}
    {{- if eq (len $cfg.networkPolicy.gatewayPodSelector) 0 -}}{{- fail "agentSandbox.analysisShadow.networkPolicy.gatewayPodSelector is required for gateway mode" -}}{{- end -}}
  {{- end -}}
  {{- if or (lt (int $cfg.networkPolicy.gatewayPort) 1) (gt (int $cfg.networkPolicy.gatewayPort) 65535) -}}{{- fail "agentSandbox.analysisShadow.networkPolicy.gatewayPort is invalid" -}}{{- end -}}
  {{- if or (eq (len $cfg.networkPolicy.dnsNamespaceSelector) 0) (eq (len $cfg.networkPolicy.dnsPodSelector) 0) -}}{{- fail "agentSandbox.analysisShadow DNS network selectors are required" -}}{{- end -}}
  {{- if and (eq $cfg.networkPolicy.mode "cilium") (or (not (hasKey $cfg.networkPolicy.dnsNamespaceSelector "kubernetes.io/metadata.name")) (not (get $cfg.networkPolicy.dnsNamespaceSelector "kubernetes.io/metadata.name"))) -}}{{- fail "agentSandbox.analysisShadow cilium mode requires dnsNamespaceSelector.kubernetes.io/metadata.name" -}}{{- end -}}
  {{- range $env := concat .Values.server.extraEnv .Values.fetcher.extraEnv -}}
    {{- if hasPrefix "AGENT_SANDBOX_ANALYSIS_SHADOW_" (default "" $env.name) -}}{{- fail (printf "extraEnv must not override reserved analysis shadow variable %s" $env.name) -}}{{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/* Validate AI provider configuration. */}}
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

{{/* Validate the failure analysis runtime. */}}
{{- define "aster.validateAnalysisRuntime" -}}
{{- if ne .Values.analysisRuntime.type "inprocess" -}}
{{- fail "analysisRuntime.type must be inprocess" -}}
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

{{/* Dashboard ServiceAccount allowed to manage only Fix Sandboxes. */}}
{{- define "aster.agentSandboxClientServiceAccountName" -}}
{{- if .Values.agentSandbox.rbac.clientServiceAccountName -}}
{{- .Values.agentSandbox.rbac.clientServiceAccountName -}}
{{- else -}}
{{- printf "%s-agent-sandbox-client" (include "aster.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
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
  {{- if ne .Values.analysisRuntime.type "inprocess" -}}{{- fail "agentSandbox.fixRuntime requires analysisRuntime.type=inprocess" -}}{{- end -}}
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
  {{- $clientSA := include "aster.agentSandboxClientServiceAccountName" . -}}
  {{- if and (not .Values.agentSandbox.rbac.create) (not .Values.agentSandbox.rbac.clientServiceAccountName) -}}{{- fail "agentSandbox.rbac.clientServiceAccountName is required when chart-managed RBAC is disabled" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $clientSA) -}}{{- fail "agentSandbox.rbac.clientServiceAccountName must be a lowercase Kubernetes object name" -}}{{- end -}}
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


{{/* Immutable analyzer executor image. */}}
{{- define "aster.agentSandboxAnalyzerExecutorImage" -}}
{{- printf "%s@%s" .Values.agentSandbox.analyzer.executorImage.repository .Values.agentSandbox.analyzer.executorImage.digest -}}
{{- end -}}

{{/* Dedicated ServiceAccount allowed to manage only analyzer Sandboxes. */}}
{{- define "aster.agentSandboxAnalyzerClientServiceAccountName" -}}
{{- if .Values.agentSandbox.analyzer.clientServiceAccount.name -}}
{{- .Values.agentSandbox.analyzer.clientServiceAccount.name -}}
{{- else -}}
{{- printf "%s-agent-sandbox-analyzer-client" (include "aster.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* Tokenless ServiceAccount used inside analyzer Sandboxes. */}}
{{- define "aster.agentSandboxAnalyzerWorkloadServiceAccountName" -}}
{{- .Values.agentSandbox.analyzer.workloadServiceAccount.name -}}
{{- end -}}

{{/* Cluster-scoped analyzer admission policy name. */}}
{{- define "aster.agentSandboxAnalyzerAdmissionName" -}}
{{- printf "%s-agent-sandbox-analyzer-%s" (include "aster.fullname" .) (include "aster.releaseScope" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Validate the disabled-by-default private Agent Sandbox analyzer. */}}
{{- define "aster.validateAgentSandboxAnalyzer" -}}
{{- if .Values.agentSandbox.analyzer.enabled -}}
  {{- $cfg := .Values.agentSandbox.analyzer -}}
  {{- if ne .Values.analysisRuntime.type "inprocess" -}}{{- fail "agentSandbox.analyzer requires analysisRuntime.type=inprocess" -}}{{- end -}}
  {{- if .Values.agentSandbox.analysisShadow.enabled -}}{{- fail "agentSandbox.analyzer cannot run with agentSandbox.analysisShadow" -}}{{- end -}}
  {{- if not $cfg.namespace -}}{{- fail "agentSandbox.analyzer.namespace is required" -}}{{- end -}}
  {{- if eq $cfg.namespace .Release.Namespace -}}{{- fail "agentSandbox.analyzer.namespace must differ from the dashboard release namespace" -}}{{- end -}}
  {{- if and .Values.agentSandbox.fixRuntime.enabled (eq $cfg.namespace .Values.agentSandbox.fixRuntime.namespace) -}}{{- fail "agentSandbox.analyzer.namespace must differ from agentSandbox.fixRuntime.namespace" -}}{{- end -}}
  {{- if and .Values.agentSandbox.causalCritic.enabled (eq $cfg.namespace .Values.agentSandbox.causalCritic.namespace) -}}{{- fail "agentSandbox.analyzer.namespace must differ from agentSandbox.causalCritic.namespace" -}}{{- end -}}
  {{- if or (gt (len $cfg.namespace) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $cfg.namespace)) -}}{{- fail "agentSandbox.analyzer.namespace must be a lowercase DNS label" -}}{{- end -}}
  {{- if or (gt (len $cfg.runtimeClassName) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $cfg.runtimeClassName)) -}}{{- fail "agentSandbox.analyzer.runtimeClassName is required and must be a lowercase RuntimeClass name" -}}{{- end -}}
  {{- if not (regexMatch "^[^[:space:]@]+$" $cfg.executorImage.repository) -}}{{- fail "agentSandbox.analyzer.executorImage.repository is required without whitespace, credentials, or a digest" -}}{{- end -}}
  {{- if not (regexMatch "^sha256:[0-9a-f]{64}$" $cfg.executorImage.digest) -}}{{- fail "agentSandbox.analyzer.executorImage.digest must be an immutable sha256 digest" -}}{{- end -}}
  {{- if or (gt (len $cfg.input.existingClaim) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $cfg.input.existingClaim)) -}}{{- fail "agentSandbox.analyzer.input.existingClaim is required and must be a lowercase PVC name" -}}{{- end -}}
  {{- if and .Values.persistence.existingClaim (eq $cfg.input.existingClaim .Values.persistence.existingClaim) -}}{{- fail "agentSandbox.analyzer.input.existingClaim must differ from the public dashboard data PVC" -}}{{- end -}}
  {{- $workloadSA := include "aster.agentSandboxAnalyzerWorkloadServiceAccountName" . -}}
  {{- if or (gt (len $workloadSA) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $workloadSA)) -}}{{- fail "agentSandbox.analyzer.workloadServiceAccount.name is required and must be a lowercase object name" -}}{{- end -}}
  {{- if and (not .Values.agentSandbox.rbac.create) $cfg.clientServiceAccount.create -}}{{- fail "agentSandbox.analyzer.clientServiceAccount.create requires agentSandbox.rbac.create=true" -}}{{- end -}}
  {{- if and (not $cfg.clientServiceAccount.create) (not $cfg.clientServiceAccount.name) -}}{{- fail "agentSandbox.analyzer.clientServiceAccount.name is required when create=false" -}}{{- end -}}
  {{- $clientSA := include "aster.agentSandboxAnalyzerClientServiceAccountName" . -}}
  {{- if or (gt (len $clientSA) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $clientSA)) -}}{{- fail "agentSandbox.analyzer.clientServiceAccount.name must be a lowercase Kubernetes object name" -}}{{- end -}}
  {{- $provider := $cfg.modelProvider -}}
  {{- $credentialMode := default "direct" $provider.credentialMode -}}
  {{- $providerAPI := default "chat_completions" $provider.api -}}
  {{- $providerAuth := $provider.auth -}}
  {{- $authType := default "none" $providerAuth.type -}}
  {{- if not (has $credentialMode (list "direct" "gateway")) -}}{{- fail "agentSandbox.analyzer.modelProvider.credentialMode must be direct or gateway" -}}{{- end -}}
  {{- if not (has $providerAPI (list "chat_completions" "responses")) -}}{{- fail "agentSandbox.analyzer.modelProvider.api must be chat_completions or responses" -}}{{- end -}}
  {{- if not (has (default "" $provider.reasoningEffort) (list "" "none" "low" "medium" "high" "xhigh")) -}}{{- fail "agentSandbox.analyzer.modelProvider.reasoningEffort must be empty, none, low, medium, high, or xhigh with pinned OpenCode 1.18.2" -}}{{- end -}}
  {{- if not (regexMatch "^https://[^/@?#]+(:[0-9]+)?(/[A-Za-z0-9._~!$&()*+,;=:@%/-]*)?$" $provider.endpoint) -}}{{- fail "agentSandbox.analyzer.modelProvider.endpoint must be an absolute credential-free HTTPS URL" -}}{{- end -}}
  {{- if and (eq $providerAPI "chat_completions") (not (hasSuffix "/chat/completions" (trimSuffix "/" $provider.endpoint))) -}}{{- fail "agentSandbox.analyzer.modelProvider chat_completions endpoint must end with /chat/completions" -}}{{- end -}}
  {{- if and (eq $providerAPI "responses") (not (hasSuffix "/responses" (trimSuffix "/" $provider.endpoint))) -}}{{- fail "agentSandbox.analyzer.modelProvider responses endpoint must end with /responses" -}}{{- end -}}
  {{- if or (not $provider.model) (gt (len $provider.model) 256) (contains "\n" $provider.model) (contains "\r" $provider.model) -}}{{- fail "agentSandbox.analyzer.modelProvider.model must be non-empty, at most 256 bytes, and single-line" -}}{{- end -}}
  {{- if not (has $authType (list "none" "bearer")) -}}{{- fail "agentSandbox.analyzer.modelProvider.auth.type must be none or bearer" -}}{{- end -}}
  {{- if and (eq $providerAPI "responses") (or (ne $credentialMode "direct") (ne $authType "bearer")) -}}{{- fail "agentSandbox.analyzer.modelProvider responses requires direct bearer auth with the pinned OpenCode provider" -}}{{- end -}}
  {{- if $provider.publicCAPrivateDNS -}}{{- fail "agentSandbox.analyzer.modelProvider.publicCAPrivateDNS is not supported" -}}{{- end -}}
  {{- if eq $credentialMode "gateway" -}}
    {{- if ne $authType "none" -}}{{- fail "agentSandbox.analyzer.modelProvider gateway mode requires auth.type=none" -}}{{- end -}}
    {{- if or $providerAuth.existingSecret $providerAuth.tokenKey -}}{{- fail "agentSandbox.analyzer.modelProvider gateway mode must not set Secret fields" -}}{{- end -}}
    {{- if not (regexMatch "^https://[^/]+[.](svc|svc[.]cluster[.]local|internal)(:[0-9]+)?(/[A-Za-z0-9._~!$&()*+,;=:@%/-]*)?$" $provider.endpoint) -}}{{- fail "agentSandbox.analyzer.modelProvider gateway endpoint must use internal service DNS" -}}{{- end -}}
  {{- else if eq $authType "none" -}}
    {{- if or $providerAuth.existingSecret $providerAuth.tokenKey -}}{{- fail "agentSandbox.analyzer.modelProvider auth.type=none must not set Secret fields" -}}{{- end -}}
  {{- else -}}
    {{- if or (not $providerAuth.existingSecret) (gt (len $providerAuth.existingSecret) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $providerAuth.existingSecret)) -}}{{- fail "agentSandbox.analyzer.modelProvider.auth.existingSecret is required for bearer auth and must be a valid Secret name" -}}{{- end -}}
    {{- if or (not $providerAuth.tokenKey) (gt (len $providerAuth.tokenKey) 253) (not (regexMatch "^[A-Za-z0-9._-]+$" $providerAuth.tokenKey)) -}}{{- fail "agentSandbox.analyzer.modelProvider.auth.tokenKey is required for bearer auth and must be a valid Secret key" -}}{{- end -}}
  {{- end -}}
  {{- $timeoutText := printf "%v" $cfg.timeout -}}
  {{- $timeoutSeconds := 0 -}}
  {{- if regexMatch "^[1-9][0-9]*s$" $timeoutText -}}
    {{- if gt (len $timeoutText) 5 -}}{{- fail "agentSandbox.analyzer.timeout must be at most 30m" -}}{{- end -}}
    {{- $timeoutSeconds = trimSuffix "s" $timeoutText | int -}}
  {{- else if regexMatch "^[1-9][0-9]*m$" $timeoutText -}}
    {{- if gt (len $timeoutText) 3 -}}{{- fail "agentSandbox.analyzer.timeout must be at most 30m" -}}{{- end -}}
    {{- $timeoutSeconds = mul (trimSuffix "m" $timeoutText | int) 60 -}}
  {{- else -}}
    {{- fail "agentSandbox.analyzer.timeout must use positive whole seconds or minutes" -}}
  {{- end -}}
  {{- if gt $timeoutSeconds 1800 -}}{{- fail "agentSandbox.analyzer.timeout must be at most 30m" -}}{{- end -}}
  {{- $poll := printf "%v" $cfg.pollInterval -}}
  {{- if or (not (regexMatch "^(([0-9]+([.][0-9]+)?)|([.][0-9]+))(ms|s)$" $poll)) (not (regexMatch "[1-9]" $poll)) -}}{{- fail "agentSandbox.analyzer.pollInterval must be a positive duration below 30s" -}}{{- end -}}
  {{- if regexMatch "^([3-9][0-9]|[1-9][0-9]{2,})s$" (durationRound $poll) -}}{{- fail "agentSandbox.analyzer.pollInterval must be below 30s" -}}{{- end -}}
  {{- if or (lt (int64 $cfg.outputLimitBytes) 4096) (gt (int64 $cfg.outputLimitBytes) 1048576) -}}{{- fail "agentSandbox.analyzer.outputLimitBytes must be between 4096 and 1048576" -}}{{- end -}}
  {{- range $scope, $resources := $cfg.resources -}}
    {{- range $resource := list "cpu" "memory" "ephemeral-storage" -}}
      {{- if not (index $resources $resource) -}}{{- fail (printf "agentSandbox.analyzer.resources.%s.%s is required" $scope $resource) -}}{{- end -}}
    {{- end -}}
  {{- end -}}
  {{- if ne (index $cfg.resources.requests "ephemeral-storage") (index $cfg.resources.limits "ephemeral-storage") -}}{{- fail "agentSandbox.analyzer ephemeral-storage request must equal its limit" -}}{{- end -}}
  {{- if not $cfg.networkPolicy.enabled -}}{{- fail "agentSandbox.analyzer.networkPolicy.enabled must be true" -}}{{- end -}}
  {{- if not (has $cfg.networkPolicy.mode (list "kubernetes" "cilium")) -}}{{- fail "agentSandbox.analyzer.networkPolicy.mode must be kubernetes or cilium" -}}{{- end -}}
  {{- if or (lt (int $cfg.networkPolicy.gatewayPort) 1) (gt (int $cfg.networkPolicy.gatewayPort) 65535) -}}{{- fail "agentSandbox.analyzer.networkPolicy.gatewayPort is invalid" -}}{{- end -}}
  {{- $gatewayTargetPort := int $cfg.networkPolicy.gatewayPort -}}
  {{- if and (hasKey $cfg.networkPolicy "gatewayTargetPort") (ne (index $cfg.networkPolicy "gatewayTargetPort") nil) -}}
    {{- $gatewayTargetPort = int (index $cfg.networkPolicy "gatewayTargetPort") -}}
  {{- end -}}
  {{- if or (lt $gatewayTargetPort 1) (gt $gatewayTargetPort 65535) -}}{{- fail "agentSandbox.analyzer.networkPolicy.gatewayTargetPort is invalid" -}}{{- end -}}
  {{- $providerAuthority := regexFind "^https://[^/]+" $provider.endpoint -}}
  {{- $explicitProviderPort := regexFind ":[0-9]+$" $providerAuthority -}}
  {{- $endpointProviderPort := 443 -}}
  {{- if $explicitProviderPort -}}{{- $endpointProviderPort = trimPrefix ":" $explicitProviderPort | int -}}{{- end -}}
  {{- if ne (int $cfg.networkPolicy.gatewayPort) $endpointProviderPort -}}{{- fail "agentSandbox.analyzer.networkPolicy.gatewayPort must match modelProvider.endpoint" -}}{{- end -}}
  {{- if or (eq (len $cfg.networkPolicy.dnsNamespaceSelector) 0) (eq (len $cfg.networkPolicy.dnsPodSelector) 0) -}}{{- fail "agentSandbox.analyzer DNS network selectors are required" -}}{{- end -}}
  {{- if and (eq $cfg.networkPolicy.mode "cilium") (or (not (hasKey $cfg.networkPolicy.dnsNamespaceSelector "kubernetes.io/metadata.name")) (not (get $cfg.networkPolicy.dnsNamespaceSelector "kubernetes.io/metadata.name"))) -}}{{- fail "agentSandbox.analyzer cilium mode requires dnsNamespaceSelector.kubernetes.io/metadata.name" -}}{{- end -}}
  {{- $providerInternal := regexMatch "^https://[^/]+[.](svc|svc[.]cluster[.]local|internal)(:[0-9]+)?/" $provider.endpoint -}}
  {{- if $providerInternal -}}
    {{- if eq (len $cfg.networkPolicy.gatewayNamespaceSelector) 0 -}}{{- fail "agentSandbox.analyzer.networkPolicy.gatewayNamespaceSelector is required for an internal provider" -}}{{- end -}}
    {{- if eq (len $cfg.networkPolicy.gatewayPodSelector) 0 -}}{{- fail "agentSandbox.analyzer.networkPolicy.gatewayPodSelector is required for an internal provider" -}}{{- end -}}
    {{- if and (eq $cfg.networkPolicy.mode "cilium") (not (regexMatch "^https://[a-z0-9]([-a-z0-9]*[a-z0-9])?[.][a-z0-9]([-a-z0-9]*[a-z0-9])?[.]svc([.]cluster[.]local)?(:[0-9]+)?/" $provider.endpoint)) -}}{{- fail "agentSandbox.analyzer cilium mode requires a Kubernetes Service internal provider endpoint" -}}{{- end -}}
  {{- else -}}
    {{- if ne $credentialMode "direct" -}}{{- fail "agentSandbox.analyzer external providers require direct credential mode" -}}{{- end -}}
    {{- if not (regexMatch "^https://([A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?[.])+[A-Za-z]{2,}(:[0-9]+)?/" $provider.endpoint) -}}{{- fail "agentSandbox.analyzer external direct provider endpoint must use a DNS FQDN" -}}{{- end -}}
    {{- if ne $cfg.networkPolicy.mode "cilium" -}}{{- fail "agentSandbox.analyzer external direct providers require networkPolicy.mode=cilium" -}}{{- end -}}
  {{- end -}}
  {{- if not $cfg.quota.enabled -}}{{- fail "agentSandbox.analyzer.quota.enabled must be true" -}}{{- end -}}
  {{- range $env := concat .Values.server.extraEnv .Values.fetcher.extraEnv -}}
    {{- if hasPrefix "AGENT_SANDBOX_ANALYSIS_" (default "" $env.name) -}}{{- fail (printf "extraEnv must not override reserved analyzer variable %s" $env.name) -}}{{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}


{{/* Immutable causal critic executor image. */}}
{{- define "aster.agentSandboxCriticExecutorImage" -}}
{{- printf "%s@%s" .Values.agentSandbox.causalCritic.image.repository .Values.agentSandbox.causalCritic.image.digest -}}
{{- end -}}

{{/* Tokenless ServiceAccount used inside critic Sandboxes. */}}
{{- define "aster.agentSandboxCriticWorkloadServiceAccountName" -}}
{{- .Values.agentSandbox.causalCritic.workloadServiceAccount.name -}}
{{- end -}}

{{/* Cluster-scoped critic admission policy name. */}}
{{- define "aster.agentSandboxCriticAdmissionName" -}}
{{- printf "%s-agent-sandbox-critic-%s" (include "aster.fullname" .) (include "aster.releaseScope" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "aster.causalCriticLedgerPath" -}}
{{- printf "%s/causal_critic.json" (trimSuffix "/" .Values.agentSandbox.causalCritic.ledger.mountPath) -}}
{{- end -}}

{{/* Non-secret critic runtime environment for scheduled fetcher or worker. */}}
{{- define "aster.agentSandboxCriticEnv" -}}
- name: AGENT_SANDBOX_CRITIC_NAMESPACE
  value: {{ .Values.agentSandbox.causalCritic.namespace | quote }}
- name: AGENT_SANDBOX_CRITIC_IMAGE
  value: {{ include "aster.agentSandboxCriticExecutorImage" . | quote }}
- name: AGENT_SANDBOX_CRITIC_SERVICE_ACCOUNT
  value: {{ include "aster.agentSandboxCriticWorkloadServiceAccountName" . | quote }}
- name: AGENT_SANDBOX_CRITIC_RUNTIME_CLASS
  value: {{ .Values.agentSandbox.causalCritic.runtimeClassName | quote }}
- name: AGENT_SANDBOX_CRITIC_MODEL_GATEWAY_ENDPOINT
  value: {{ .Values.agentSandbox.causalCritic.modelGateway.endpoint | quote }}
- name: AGENT_SANDBOX_CRITIC_MODEL_GATEWAY_MODEL
  value: {{ .Values.agentSandbox.causalCritic.modelGateway.model | quote }}
- name: AGENT_SANDBOX_CRITIC_MODEL_GATEWAY_PROTOCOL
  value: {{ .Values.agentSandbox.causalCritic.modelGateway.protocolVersion | quote }}
- name: AGENT_SANDBOX_CRITIC_MODEL_GATEWAY_PUBLIC_CA_PRIVATE_DNS
  value: "false"
- name: AGENT_SANDBOX_CRITIC_TIMEOUT
  value: {{ .Values.agentSandbox.causalCritic.timeout | quote }}
- name: AGENT_SANDBOX_CRITIC_OUTPUT_LIMIT_BYTES
  value: {{ printf "%d" (int64 .Values.agentSandbox.causalCritic.outputLimitBytes) | quote }}
- name: AGENT_SANDBOX_CRITIC_POLL_INTERVAL
  value: {{ .Values.agentSandbox.causalCritic.pollInterval | quote }}
- name: AGENT_SANDBOX_CRITIC_CPU_REQUEST
  value: {{ index .Values.agentSandbox.causalCritic.resources.requests "cpu" | quote }}
- name: AGENT_SANDBOX_CRITIC_CPU_LIMIT
  value: {{ index .Values.agentSandbox.causalCritic.resources.limits "cpu" | quote }}
- name: AGENT_SANDBOX_CRITIC_MEMORY_REQUEST
  value: {{ index .Values.agentSandbox.causalCritic.resources.requests "memory" | quote }}
- name: AGENT_SANDBOX_CRITIC_MEMORY_LIMIT
  value: {{ index .Values.agentSandbox.causalCritic.resources.limits "memory" | quote }}
- name: AGENT_SANDBOX_CRITIC_EPHEMERAL_STORAGE_LIMIT
  value: {{ index .Values.agentSandbox.causalCritic.resources.limits "ephemeral-storage" | quote }}
{{- end -}}

{{/* Validate the disabled-by-default private Agent Sandbox critic. */}}
{{- define "aster.validateAgentSandboxCausalCritic" -}}
{{- if .Values.agentSandbox.causalCritic.enabled -}}
  {{- $cfg := .Values.agentSandbox.causalCritic -}}
  {{- if not .Values.ai.enabled -}}{{- fail "agentSandbox.causalCritic requires ai.enabled=true" -}}{{- end -}}
  {{- if ne .Values.analysisRuntime.type "inprocess" -}}{{- fail "agentSandbox.causalCritic requires analysisRuntime.type=inprocess" -}}{{- end -}}
  {{- if .Values.agentSandbox.analysisShadow.enabled -}}{{- fail "agentSandbox.causalCritic cannot run with agentSandbox.analysisShadow" -}}{{- end -}}
  {{- if .Values.agentSandbox.fixRuntime.enabled -}}{{- fail "agentSandbox.causalCritic cannot run with agentSandbox.fixRuntime" -}}{{- end -}}
  {{- if not $cfg.namespace -}}{{- fail "agentSandbox.causalCritic.namespace is required" -}}{{- end -}}
  {{- if eq $cfg.namespace .Release.Namespace -}}{{- fail "agentSandbox.causalCritic.namespace must differ from the dashboard release namespace" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $cfg.namespace) -}}{{- fail "agentSandbox.causalCritic.namespace must be a lowercase DNS label" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $cfg.runtimeClassName) -}}{{- fail "agentSandbox.causalCritic.runtimeClassName is required and must be a lowercase RuntimeClass name" -}}{{- end -}}
  {{- if not (regexMatch "^[^[:space:]@]+$" $cfg.image.repository) -}}{{- fail "agentSandbox.causalCritic.image.repository is required without whitespace, credentials, or a digest" -}}{{- end -}}
  {{- if not (regexMatch "^sha256:[0-9a-f]{64}$" $cfg.image.digest) -}}{{- fail "agentSandbox.causalCritic.image.digest must be an immutable sha256 digest" -}}{{- end -}}
  {{- if ne $cfg.image.pullPolicy "IfNotPresent" -}}{{- fail "agentSandbox.causalCritic.image.pullPolicy must be IfNotPresent" -}}{{- end -}}
  {{- $workloadSA := include "aster.agentSandboxCriticWorkloadServiceAccountName" . -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $workloadSA) -}}{{- fail "agentSandbox.causalCritic.workloadServiceAccount.name is required and must be a lowercase object name" -}}{{- end -}}
  {{- $clientSA := include "aster.agentSandboxClientServiceAccountName" . -}}
  {{- if and (not .Values.agentSandbox.rbac.create) (not .Values.agentSandbox.rbac.clientServiceAccountName) -}}{{- fail "agentSandbox.rbac.clientServiceAccountName is required when chart-managed RBAC is disabled" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $clientSA) -}}{{- fail "agentSandbox.rbac.clientServiceAccountName must be a lowercase Kubernetes object name" -}}{{- end -}}
  {{- if not $cfg.ledger.existingClaim -}}{{- fail "agentSandbox.causalCritic.ledger.existingClaim is required" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $cfg.ledger.existingClaim) -}}{{- fail "agentSandbox.causalCritic.ledger.existingClaim must be a lowercase object name" -}}{{- end -}}
  {{- if eq $cfg.ledger.existingClaim (include "aster.pvcName" .) -}}{{- fail "agentSandbox.causalCritic must use a PVC distinct from public dashboard data" -}}{{- end -}}
  {{- if not (hasPrefix "/private/" $cfg.ledger.mountPath) -}}{{- fail "agentSandbox.causalCritic.ledger.mountPath must be under /private" -}}{{- end -}}
  {{- if or (contains ".." $cfg.ledger.mountPath) (contains "//" $cfg.ledger.mountPath) -}}{{- fail "agentSandbox.causalCritic.ledger.mountPath must be canonical" -}}{{- end -}}
  {{- if or (hasPrefix .Values.persistence.mountPath $cfg.ledger.mountPath) (hasPrefix $cfg.ledger.mountPath .Values.persistence.mountPath) -}}{{- fail "agentSandbox.causalCritic ledger must be separate from public dashboard persistence" -}}{{- end -}}
  {{- $gateway := $cfg.modelGateway -}}
  {{- if not (regexMatch "^https://[a-zA-Z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~!$&()*+,;=:@%/-]*)?$" $gateway.endpoint) -}}{{- fail "agentSandbox.causalCritic.modelGateway.endpoint must be an absolute credential-free HTTPS URL" -}}{{- end -}}
  {{- if not (regexMatch "^https://[^/]+[.](svc|svc[.]cluster[.]local|internal)(:[0-9]+)?(/[A-Za-z0-9._~!$&()*+,;=:@%/-]*)?$" $gateway.endpoint) -}}{{- fail "agentSandbox.causalCritic.modelGateway.endpoint must use internal service DNS" -}}{{- end -}}
  {{- if or (not $gateway.model) (gt (len $gateway.model) 256) (contains "\n" $gateway.model) (contains "\r" $gateway.model) -}}{{- fail "agentSandbox.causalCritic.modelGateway.model must be non-empty, at most 256 bytes, and single-line" -}}{{- end -}}
  {{- if ne $gateway.protocolVersion "openai-chat-completions-v1" -}}{{- fail "agentSandbox.causalCritic.modelGateway.protocolVersion must be openai-chat-completions-v1" -}}{{- end -}}
  {{- $timeoutText := printf "%v" $cfg.timeout -}}
  {{- $timeoutSeconds := 0 -}}
  {{- if regexMatch "^[1-9][0-9]*s$" $timeoutText -}}
    {{- if gt (len $timeoutText) 5 -}}{{- fail "agentSandbox.causalCritic.timeout must be at most 30m" -}}{{- end -}}
    {{- $timeoutSeconds = trimSuffix "s" $timeoutText | int -}}
  {{- else if regexMatch "^[1-9][0-9]*m$" $timeoutText -}}
    {{- if gt (len $timeoutText) 3 -}}{{- fail "agentSandbox.causalCritic.timeout must be at most 30m" -}}{{- end -}}
    {{- $timeoutSeconds = mul (trimSuffix "m" $timeoutText | int) 60 -}}
  {{- else -}}
    {{- fail "agentSandbox.causalCritic.timeout must use positive whole seconds or minutes" -}}
  {{- end -}}
  {{- if gt $timeoutSeconds 1800 -}}{{- fail "agentSandbox.causalCritic.timeout must be at most 30m" -}}{{- end -}}
  {{- $poll := printf "%v" $cfg.pollInterval -}}
  {{- if or (not (regexMatch "^(([0-9]+([.][0-9]+)?)|([.][0-9]+))(ms|s)$" $poll)) (not (regexMatch "[1-9]" $poll)) -}}{{- fail "agentSandbox.causalCritic.pollInterval must be a positive duration below 30s" -}}{{- end -}}
  {{- if regexMatch "^([3-9][0-9]|[1-9][0-9]{2,})s$" (durationRound $poll) -}}{{- fail "agentSandbox.causalCritic.pollInterval must be below 30s" -}}{{- end -}}
  {{- if or (lt (int64 $cfg.outputLimitBytes) 4096) (gt (int64 $cfg.outputLimitBytes) 1048576) -}}{{- fail "agentSandbox.causalCritic.outputLimitBytes must be between 4096 and 1048576" -}}{{- end -}}
  {{- if or (lt (int $cfg.maxPerRun) 1) (gt (int $cfg.maxPerRun) 10) -}}{{- fail "agentSandbox.causalCritic.maxPerRun must be between 1 and 10" -}}{{- end -}}
  {{- if ne (index $cfg.resources.requests "ephemeral-storage") (index $cfg.resources.limits "ephemeral-storage") -}}{{- fail "agentSandbox.causalCritic ephemeral-storage request must equal its limit" -}}{{- end -}}
  {{- if not $cfg.networkPolicy.enabled -}}{{- fail "agentSandbox.causalCritic.networkPolicy.enabled must be true" -}}{{- end -}}
  {{- if not (has $cfg.networkPolicy.mode (list "kubernetes" "cilium")) -}}{{- fail "agentSandbox.causalCritic.networkPolicy.mode must be kubernetes or cilium" -}}{{- end -}}
  {{- if eq (len $cfg.networkPolicy.gatewayNamespaceSelector) 0 -}}{{- fail "agentSandbox.causalCritic.networkPolicy.gatewayNamespaceSelector is required" -}}{{- end -}}
  {{- if eq (len $cfg.networkPolicy.gatewayPodSelector) 0 -}}{{- fail "agentSandbox.causalCritic.networkPolicy.gatewayPodSelector is required" -}}{{- end -}}
  {{- if or (lt (int $cfg.networkPolicy.gatewayPort) 1) (gt (int $cfg.networkPolicy.gatewayPort) 65535) -}}{{- fail "agentSandbox.causalCritic.networkPolicy.gatewayPort is invalid" -}}{{- end -}}
  {{- $gatewayAuthority := regexFind "^https://[^/]+" $gateway.endpoint -}}
  {{- $explicitGatewayPort := regexFind ":[0-9]+$" $gatewayAuthority -}}
  {{- $endpointGatewayPort := 443 -}}
  {{- if $explicitGatewayPort -}}{{- $endpointGatewayPort = trimPrefix ":" $explicitGatewayPort | int -}}{{- end -}}
  {{- if ne (int $cfg.networkPolicy.gatewayPort) $endpointGatewayPort -}}{{- fail "agentSandbox.causalCritic.networkPolicy.gatewayPort must match modelGateway.endpoint" -}}{{- end -}}
  {{- if or (eq (len $cfg.networkPolicy.dnsNamespaceSelector) 0) (eq (len $cfg.networkPolicy.dnsPodSelector) 0) -}}{{- fail "agentSandbox.causalCritic DNS network selectors are required" -}}{{- end -}}
  {{- if and (eq $cfg.networkPolicy.mode "cilium") (or (not (hasKey $cfg.networkPolicy.dnsNamespaceSelector "kubernetes.io/metadata.name")) (not (get $cfg.networkPolicy.dnsNamespaceSelector "kubernetes.io/metadata.name"))) -}}{{- fail "agentSandbox.causalCritic cilium mode requires dnsNamespaceSelector.kubernetes.io/metadata.name" -}}{{- end -}}
  {{- range $env := concat .Values.server.extraEnv .Values.fetcher.extraEnv -}}
    {{- if hasPrefix "AGENT_SANDBOX_CRITIC_" (default "" $env.name) -}}{{- fail (printf "extraEnv must not override reserved critic variable %s" $env.name) -}}{{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/* Whether any scheduled Agent Sandbox lifecycle needs the dashboard client identity.
Fix generation is server-only and maintainer-initiated, so it never applies here. */}}
{{- define "aster.agentSandboxScheduledEnabled" -}}
{{- if or .Values.agentSandbox.causalCritic.enabled .Values.agentSandbox.analysisShadow.enabled -}}true
{{- else -}}false{{- end -}}
{{- end -}}

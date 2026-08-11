{{/*
Chart name, optionally overridden.
*/}}
{{- define "prow-ai-dashboard.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name.
*/}}
{{- define "prow-ai-dashboard.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
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
{{- define "prow-ai-dashboard.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "prow-ai-dashboard.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels.
*/}}
{{- define "prow-ai-dashboard.selectorLabels" -}}
app.kubernetes.io/name: {{ include "prow-ai-dashboard.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Resolve an image-specific tag, then the shared tag, then appVersion. */}}
{{- define "prow-ai-dashboard.resolvedImageTag" -}}
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
{{- define "prow-ai-dashboard.image" -}}
{{- $tag := include "prow-ai-dashboard.resolvedImageTag" (list . .Values.image.tag) -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/* Analyzer image used only by the experimental Orka container runtime. */}}
{{- define "prow-ai-dashboard.analyzerImage" -}}
{{- $tag := include "prow-ai-dashboard.resolvedImageTag" (list . .Values.analysisRuntime.orkaContainer.image.tag) -}}
{{- printf "%s:%s" .Values.analysisRuntime.orkaContainer.image.repository $tag -}}
{{- end -}}

{{/* Git-capable engine image used by the opt-in fix runtime. */}}
{{- define "prow-ai-dashboard.fixerImage" -}}
{{- $tag := include "prow-ai-dashboard.resolvedImageTag" (list . .Values.orka.fixRuntime.image.tag) -}}
{{- printf "%s:%s" .Values.orka.fixRuntime.image.repository $tag -}}
{{- end -}}

{{/* Minimal git-capable engine image used with Agent Sandbox. */}}
{{- define "prow-ai-dashboard.agentSandboxDashboardImage" -}}
{{- $image := .Values.agentSandbox.fixRuntime.dashboardImage -}}
{{- $tag := include "prow-ai-dashboard.resolvedImageTag" (list . $image.tag) -}}
{{- printf "%s:%s" $image.repository $tag -}}
{{- end -}}

{{/*
Small image used to materialize ConfigMap project files for container analysis.
*/}}
{{- define "prow-ai-dashboard.projectMaterializerImage" -}}
{{- printf "%s:%s" .Values.project.materializer.image.repository .Values.project.materializer.image.tag -}}
{{- end -}}

{{/*
Release scope for cross-namespace Orka RBAC names.
*/}}
{{- define "prow-ai-dashboard.orkaReleaseScope" -}}
{{- printf "%s/%s" .Release.Namespace .Release.Name | sha256sum | trunc 8 -}}
{{- end -}}

{{- define "prow-ai-dashboard.orkaAnalysisNamespace" -}}
{{- if .Values.analysisRuntime.orkaContainer.namespace -}}
{{- .Values.analysisRuntime.orkaContainer.namespace -}}
{{- else -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 44 | trimSuffix "-" -}}
{{- printf "%s-analysis-%s" $base (include "prow-ai-dashboard.orkaReleaseScope" .) -}}
{{- end -}}
{{- end -}}

{{/*
Name of the PVC the fetcher and server share.
*/}}
{{- define "prow-ai-dashboard.pvcName" -}}
{{- if .Values.persistence.existingClaim -}}
{{- .Values.persistence.existingClaim -}}
{{- else -}}
{{- printf "%s-data" (include "prow-ai-dashboard.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Name of the ConfigMap holding the consumer project config.
*/}}
{{- define "prow-ai-dashboard.projectConfigMap" -}}
{{- if .Values.project.existingConfigMap -}}
{{- .Values.project.existingConfigMap -}}
{{- else -}}
{{- printf "%s-project" (include "prow-ai-dashboard.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
ConfigMap volume items for the project config: project.yaml, system.md (mapped
to prompts/system.md), and each consumer skill recipe mapped to skills/<name> so
the engine loads them from <project_dir>/skills/. Include with nindent at the
call site.
*/}}
{{- define "prow-ai-dashboard.projectVolumeItems" -}}
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
{{- define "prow-ai-dashboard.validateNetworkSecurity" -}}
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
{{- $interactive := or .Values.server.actions.enabled .Values.server.chat.enabled -}}
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
{{- define "prow-ai-dashboard.aiSecret" -}}
{{- if .Values.ai.existingSecret -}}
{{- .Values.ai.existingSecret -}}
{{- else -}}
{{- printf "%s-ai" (include "prow-ai-dashboard.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* Name of the Secret holding the read-only GitHub source token. */}}
{{- define "prow-ai-dashboard.githubReadSecret" -}}
{{- if .Values.ai.githubReadTokenSecretName -}}
{{- .Values.ai.githubReadTokenSecretName -}}
{{- else if and (not .Values.ai.githubReadToken) .Values.ai.existingSecret -}}
{{- .Values.ai.existingSecret -}}
{{- else -}}
{{- printf "%s-github-read" (include "prow-ai-dashboard.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Name of the ServiceAccount used by Orka fix generation.
*/}}
{{- define "prow-ai-dashboard.orkaServiceAccountName" -}}
{{- if .Values.orka.rbac.serviceAccountName -}}
{{- .Values.orka.rbac.serviceAccountName -}}
{{- else -}}
{{- printf "%s-orka" (include "prow-ai-dashboard.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Name of cross-namespace Orka RBAC resources. Include the source release scope
because Helm release names are unique only within their own namespace.
*/}}
{{- define "prow-ai-dashboard.orkaRBACName" -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 49 | trimSuffix "-" -}}
{{- printf "%s-orka-%s" $base (include "prow-ai-dashboard.orkaReleaseScope" .) -}}
{{- end -}}

{{/* TokenRequest RBAC used to mint the isolated fix identity. */}}
{{- define "prow-ai-dashboard.orkaFixTokenRBACName" -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 40 | trimSuffix "-" -}}
{{- printf "%s-fix-token-%s" $base (include "prow-ai-dashboard.orkaReleaseScope" .) -}}
{{- end -}}

{{/* Fix Task admission policy name. */}}
{{- define "prow-ai-dashboard.orkaFixAdmissionName" -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 39 | trimSuffix "-" -}}
{{- printf "%s-fix-guard-%s" $base (include "prow-ai-dashboard.orkaReleaseScope" .) -}}
{{- end -}}

{{/* Source investigation Task admission policy name. */}}
{{- define "prow-ai-dashboard.orkaSourceAdmissionName" -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 36 | trimSuffix "-" -}}
{{- printf "%s-source-guard-%s" $base (include "prow-ai-dashboard.orkaReleaseScope" .) -}}
{{- end -}}

{{/* Source investigation RBAC stays separate from fix-generation RBAC. */}}
{{- define "prow-ai-dashboard.orkaSourceRBACName" -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 39 | trimSuffix "-" -}}
{{- printf "%s-source-%s" $base (include "prow-ai-dashboard.orkaReleaseScope" .) -}}
{{- end -}}

{{/* ServiceAccount used only by the web-facing source investigation runtime. */}}
{{- define "prow-ai-dashboard.orkaSourceServiceAccountName" -}}
{{- if .Values.server.chat.sourceInvestigation.serviceAccountName -}}
{{- .Values.server.chat.sourceInvestigation.serviceAccountName -}}
{{- else -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 56 | trimSuffix "-" -}}
{{- printf "%s-source" $base -}}
{{- end -}}
{{- end -}}

{{/* Analysis RBAC stays separate from fix-generation RBAC. */}}
{{- define "prow-ai-dashboard.orkaAnalysisRBACName" -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 40 | trimSuffix "-" -}}
{{- printf "%s-analysis-%s" $base (include "prow-ai-dashboard.orkaReleaseScope" .) -}}
{{- end -}}

{{- define "prow-ai-dashboard.orkaAnalysisAdmissionName" -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 34 | trimSuffix "-" -}}
{{- printf "%s-analysis-guard-%s" $base (include "prow-ai-dashboard.orkaReleaseScope" .) -}}
{{- end -}}


{{/* Dedicated ServiceAccount used only by Agent analysis shadow Tasks. */}}
{{- define "prow-ai-dashboard.agentAnalysisShadowServiceAccountName" -}}
{{- if .Values.orka.agentAnalysisShadow.serviceAccountName -}}
{{- .Values.orka.agentAnalysisShadow.serviceAccountName -}}
{{- else -}}
{{- printf "%s-shadow" (include "prow-ai-dashboard.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* Private shadow ledger PVC, never mounted by the server. */}}
{{- define "prow-ai-dashboard.agentAnalysisShadowPVCName" -}}
{{- if .Values.orka.agentAnalysisShadow.ledger.existingClaim -}}
{{- .Values.orka.agentAnalysisShadow.ledger.existingClaim -}}
{{- else -}}
{{- printf "%s-shadow-ledger" (include "prow-ai-dashboard.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "prow-ai-dashboard.agentAnalysisShadowRBACName" -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 37 | trimSuffix "-" -}}
{{- printf "%s-shadow-%s" $base (include "prow-ai-dashboard.orkaReleaseScope" .) -}}
{{- end -}}

{{- define "prow-ai-dashboard.agentAnalysisShadowAdmissionName" -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 31 | trimSuffix "-" -}}
{{- printf "%s-shadow-guard-%s" $base (include "prow-ai-dashboard.orkaReleaseScope" .) -}}
{{- end -}}

{{- define "prow-ai-dashboard.agentAnalysisShadowLedgerMountPath" -}}/private/agent-analysis-shadow{{- end -}}
{{- define "prow-ai-dashboard.agentAnalysisShadowLedgerPath" -}}/private/agent-analysis-shadow/analysis_shadow.json{{- end -}}

{{/* Validate the fail-closed Agent analysis shadow deployment contract. */}}
{{- define "prow-ai-dashboard.validateAgentAnalysisShadow" -}}
{{- if .Values.orka.agentAnalysisShadow.enabled -}}
  {{- $cfg := .Values.orka.agentAnalysisShadow -}}
  {{- $admission := $cfg.admission -}}
  {{- if not .Values.ai.enabled -}}{{- fail "orka.agentAnalysisShadow.enabled requires ai.enabled=true" -}}{{- end -}}
  {{- if ne .Values.analysisRuntime.type "inprocess" -}}{{- fail "orka.agentAnalysisShadow.enabled requires analysisRuntime.type=inprocess" -}}{{- end -}}
  {{- if .Values.orka.fixRuntime.enabled -}}{{- fail "orka.agentAnalysisShadow and orka.fixRuntime cannot be enabled together" -}}{{- end -}}
  {{- if and (eq .Values.mode "cron") (ne .Values.fetcher.concurrencyPolicy "Forbid") -}}{{- fail "orka.agentAnalysisShadow requires fetcher.concurrencyPolicy=Forbid in cron mode" -}}{{- end -}}
  {{- if not .Values.orka.namespace -}}{{- fail "orka.namespace is required when agentAnalysisShadow is enabled" -}}{{- end -}}
  {{- if or (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" .Values.orka.namespace)) (gt (len .Values.orka.namespace) 63) -}}{{- fail "orka.namespace must be a lowercase DNS label of at most 63 characters" -}}{{- end -}}
  {{- if not (regexMatch "^https?://[^/@?#]+(/[^?#]*)?$" $cfg.api) -}}{{- fail "orka.agentAnalysisShadow.api must be an absolute http or https URL without credentials" -}}{{- end -}}
  {{- if or (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $cfg.agentVersion)) (gt (len $cfg.agentVersion) 30) -}}{{- fail "orka.agentAnalysisShadow.agentVersion must be a lowercase DNS label of at most 30 characters" -}}{{- end -}}
  {{- if not $admission.agentRef -}}{{- fail "orka.agentAnalysisShadow.admission.agentRef is required" -}}{{- end -}}
  {{- if or (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $admission.agentRef)) (gt (len $admission.agentRef) 253) -}}{{- fail "orka.agentAnalysisShadow.admission.agentRef must be a lowercase DNS name of at most 253 characters" -}}{{- end -}}
  {{- if not $admission.repository.owner -}}{{- fail "orka.agentAnalysisShadow.admission.repository.owner is required" -}}{{- end -}}
  {{- if not $admission.repository.name -}}{{- fail "orka.agentAnalysisShadow.admission.repository.name is required" -}}{{- end -}}
  {{- if not (regexMatch "^[A-Za-z0-9][A-Za-z0-9-]{0,38}$" $admission.repository.owner) -}}{{- fail "orka.agentAnalysisShadow.admission.repository.owner must be a GitHub owner name" -}}{{- end -}}
  {{- if or (not (regexMatch "^[A-Za-z0-9_.-]+$" $admission.repository.name)) (hasSuffix ".git" $admission.repository.name) -}}{{- fail "orka.agentAnalysisShadow.admission.repository.name must be a GitHub repository name without .git" -}}{{- end -}}
  {{- if and $admission.gitSecret (or (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $admission.gitSecret)) (gt (len $admission.gitSecret) 253)) -}}{{- fail "orka.agentAnalysisShadow.admission.gitSecret must be a lowercase Kubernetes object name of at most 253 characters" -}}{{- end -}}
  {{- if and $admission.gitSecret (not (or .Values.ai.githubReadToken .Values.ai.githubReadTokenSecretName)) -}}{{- fail "orka.agentAnalysisShadow.admission.gitSecret requires a dashboard-side GITHUB_READ_TOKEN Secret" -}}{{- end -}}
  {{- $maxPerRun := printf "%v" $cfg.maxPerRun -}}
  {{- if not (regexMatch "^([1-9]|10)$" $maxPerRun) -}}{{- fail "orka.agentAnalysisShadow.maxPerRun must be an integer from 1 to 10" -}}{{- end -}}
  {{- $maxTurns := printf "%v" $admission.maxTurns -}}
  {{- if not (regexMatch "^([1-9][0-9]{0,2}|1000)$" $maxTurns) -}}{{- fail "orka.agentAnalysisShadow.admission.maxTurns must be an integer from 1 to 1000" -}}{{- end -}}
  {{- $retries := printf "%v" $admission.retries -}}
  {{- if not (regexMatch "^[0-2]$" $retries) -}}{{- fail "orka.agentAnalysisShadow.admission.retries must be an integer from 0 to 2" -}}{{- end -}}
  {{- if not (regexMatch "^([1-9]|[12][0-9]|30)m$" (printf "%v" $admission.timeout)) -}}{{- fail "orka.agentAnalysisShadow.admission.timeout must be whole minutes from 1m through 30m" -}}{{- end -}}
  {{- if not (has $cfg.ledger.accessMode (list "ReadWriteOnce" "ReadWriteMany")) -}}{{- fail "orka.agentAnalysisShadow.ledger.accessMode must be ReadWriteOnce or ReadWriteMany" -}}{{- end -}}
  {{- if not $cfg.ledger.size -}}{{- fail "orka.agentAnalysisShadow.ledger.size is required" -}}{{- end -}}
  {{- if eq (include "prow-ai-dashboard.agentAnalysisShadowPVCName" .) (include "prow-ai-dashboard.pvcName" .) -}}{{- fail "orka.agentAnalysisShadow must use a PVC distinct from public dashboard data" -}}{{- end -}}
  {{- if and (not .Values.orka.rbac.create) (not $cfg.serviceAccountName) -}}{{- fail "orka.agentAnalysisShadow.serviceAccountName is required when chart-managed Orka RBAC is disabled" -}}{{- end -}}
  {{- if and $cfg.serviceAccountName (or (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $cfg.serviceAccountName)) (gt (len $cfg.serviceAccountName) 253)) -}}{{- fail "orka.agentAnalysisShadow.serviceAccountName must be a lowercase Kubernetes object name of at most 253 characters" -}}{{- end -}}
  {{- if and .Values.server.chat.sourceInvestigation.enabled (eq (include "prow-ai-dashboard.agentAnalysisShadowServiceAccountName" .) (include "prow-ai-dashboard.orkaSourceServiceAccountName" .)) -}}{{- fail "Agent analysis shadow and source investigation require distinct ServiceAccounts" -}}{{- end -}}
  {{- range .Values.fetcher.extraEnv -}}
    {{- if has .name (list "ORKA_API_TOKEN" "ORKA_API_TOKEN_FILE" "GITHUB_READ_TOKEN" "AGENT_ANALYSIS_SHADOW_API_TOKEN") -}}{{- fail (printf "fetcher.extraEnv must not override reserved shadow credential variable %s" .name) -}}{{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/* State key Secret shared by the dashboard and Orka namespaces. */}}
{{- define "prow-ai-dashboard.orkaAnalysisStateSecret" -}}
{{- if .Values.analysisRuntime.orkaContainer.state.existingSecret -}}
{{- .Values.analysisRuntime.orkaContainer.state.existingSecret -}}
{{- else -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 39 | trimSuffix "-" -}}
{{- printf "%s-analysis-state-%s" $base (include "prow-ai-dashboard.orkaReleaseScope" .) -}}
{{- end -}}
{{- end -}}

{{/*
Validate AI provider configuration.
*/}}
{{- define "prow-ai-dashboard.validateAI" -}}
{{- if not (has .Values.ai.api (list "chat_completions" "responses")) -}}
{{- fail "ai.api must be chat_completions or responses" -}}
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

{{/* Validate the fail-closed source investigation Task contract. */}}
{{- define "prow-ai-dashboard.validateSourceInvestigation" -}}
{{- if .Values.server.chat.sourceInvestigation.enabled -}}
  {{- $cfg := .Values.server.chat.sourceInvestigation.admission -}}
  {{- if not $cfg.agentRef -}}{{- fail "server.chat.sourceInvestigation.admission.agentRef is required" -}}{{- end -}}
  {{- if not $cfg.repository.owner -}}{{- fail "server.chat.sourceInvestigation.admission.repository.owner is required" -}}{{- end -}}
  {{- if not $cfg.repository.name -}}{{- fail "server.chat.sourceInvestigation.admission.repository.name is required" -}}{{- end -}}
  {{- if not $cfg.gitSecret -}}{{- fail "server.chat.sourceInvestigation.admission.gitSecret is required and must be read-only" -}}{{- end -}}
  {{- $goDurationPattern := "^(([0-9]+([.][0-9]+)?)|([.][0-9]+))(ns|us|µs|μs|ms|s|m|h)((([0-9]+([.][0-9]+)?)|([.][0-9]+))(ns|us|µs|μs|ms|s|m|h))*$" -}}
  {{- $timeout := printf "%v" $cfg.timeout -}}
  {{- if or (not (regexMatch $goDurationPattern $timeout)) (not (regexMatch "[1-9]" $timeout)) -}}{{- fail "server.chat.sourceInvestigation.admission.timeout must be a positive Go duration" -}}{{- end -}}
  {{- if and (not .Values.orka.rbac.create) (not .Values.server.chat.sourceInvestigation.serviceAccountName) -}}{{- fail "server.chat.sourceInvestigation.serviceAccountName is required when chart-managed Orka RBAC is disabled" -}}{{- end -}}
{{- end -}}
{{- end -}}

{{/* Validate the fail-closed Orka fix Task contract. */}}
{{- define "prow-ai-dashboard.validateFixRuntime" -}}
{{- if and .Values.orka.fixRuntime.enabled .Values.agentSandbox.fixRuntime.enabled -}}{{- fail "agentSandbox.fixRuntime cannot be combined with Orka runtimes or source investigation" -}}{{- end -}}
{{- if .Values.orka.fixRuntime.enabled -}}
  {{- $cfg := .Values.orka.fixRuntime.admission -}}
  {{- if not .Values.orka.namespace -}}{{- fail "orka.namespace is required when orka.fixRuntime.enabled=true" -}}{{- end -}}
  {{- if not $cfg.agentRef -}}{{- fail "orka.fixRuntime.admission.agentRef is required when fixRuntime is enabled" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $cfg.agentRef) -}}{{- fail "orka.fixRuntime.admission.agentRef must be a lowercase DNS name" -}}{{- end -}}
  {{- if not $cfg.repository.owner -}}{{- fail "orka.fixRuntime.admission.repository.owner is required when fixRuntime is enabled" -}}{{- end -}}
  {{- if not $cfg.repository.name -}}{{- fail "orka.fixRuntime.admission.repository.name is required when fixRuntime is enabled" -}}{{- end -}}
  {{- if not (regexMatch "^[A-Za-z0-9][A-Za-z0-9-]{0,38}$" $cfg.repository.owner) -}}{{- fail "orka.fixRuntime.admission.repository.owner must be a GitHub owner name" -}}{{- end -}}
  {{- if not (regexMatch "^[A-Za-z0-9_.-]+$" $cfg.repository.name) -}}{{- fail "orka.fixRuntime.admission.repository.name must be a GitHub repository name" -}}{{- end -}}
  {{- if hasSuffix ".git" $cfg.repository.name -}}{{- fail "orka.fixRuntime.admission.repository.name must not include .git" -}}{{- end -}}
  {{- $maxTurns := printf "%v" $cfg.maxTurns -}}
  {{- if not (regexMatch "^([1-9][0-9]{0,2}|1000)$" $maxTurns) -}}{{- fail "orka.fixRuntime.admission.maxTurns must be an integer from 1 to 1000" -}}{{- end -}}
  {{- $retries := printf "%v" $cfg.retries -}}
  {{- if not (regexMatch "^[0-2]$" $retries) -}}{{- fail "orka.fixRuntime.admission.retries must be an integer from 0 to 2" -}}{{- end -}}
  {{- if not (regexMatch "^([1-9]|[12][0-9]|30)m$" (printf "%v" $cfg.timeout)) -}}{{- fail "orka.fixRuntime.admission.timeout must be whole minutes from 1m through 30m" -}}{{- end -}}
  {{- if and .Values.server.actions.enabled .Values.server.chat.sourceInvestigation.enabled -}}
    {{- $fixServiceAccount := include "prow-ai-dashboard.orkaServiceAccountName" . -}}
    {{- $sourceServiceAccount := include "prow-ai-dashboard.orkaSourceServiceAccountName" . -}}
    {{- if eq $fixServiceAccount $sourceServiceAccount -}}{{- fail "Orka fix generation and source investigation require distinct ServiceAccounts" -}}{{- end -}}
  {{- end -}}
  {{- if and (not .Values.orka.rbac.create) (not .Values.orka.rbac.serviceAccountName) -}}{{- fail "orka.rbac.serviceAccountName is required when chart-managed Orka RBAC is disabled" -}}{{- end -}}
{{- end -}}
{{- end -}}

{{/* Validate the Helm-only failure analysis runtime. */}}
{{- define "prow-ai-dashboard.validateAnalysisRuntime" -}}
{{- $runtime := .Values.analysisRuntime.type -}}
{{- if not (has $runtime (list "inprocess" "orka-container")) -}}
{{- fail "analysisRuntime.type must be inprocess or orka-container" -}}
{{- end -}}
{{- if eq $runtime "orka-container" -}}
  {{- $cfg := .Values.analysisRuntime.orkaContainer -}}
  {{- $materializer := .Values.project.materializer.image -}}
  {{- if not .Values.ai.enabled -}}{{- fail "analysisRuntime.type=orka-container requires ai.enabled=true" -}}{{- end -}}
  {{- if not .Values.ai.endpoint -}}{{- fail "analysisRuntime.type=orka-container requires ai.endpoint" -}}{{- end -}}
  {{- if not .Values.ai.model -}}{{- fail "analysisRuntime.type=orka-container requires ai.model" -}}{{- end -}}
  {{- if not $materializer.repository -}}{{- fail "project.materializer.image.repository is required for Orka container analysis" -}}{{- end -}}
  {{- if not $materializer.tag -}}{{- fail "project.materializer.image.tag is required for Orka container analysis" -}}{{- end -}}
  {{- if not (regexMatch "^(sha-[0-9a-fA-F]{7,64}|v?[0-9]+[.][0-9]+[.][0-9]+(-[0-9A-Za-z.-]+)?)$" $materializer.tag) -}}{{- fail "project.materializer.image.tag must be an immutable sha-<hex> or full semantic version" -}}{{- end -}}
  {{- if ne $materializer.pullPolicy "IfNotPresent" -}}{{- fail "project.materializer.image.pullPolicy must be IfNotPresent" -}}{{- end -}}
  {{- $analysisNamespace := include "prow-ai-dashboard.orkaAnalysisNamespace" . -}}
  {{- if eq $analysisNamespace .Values.orka.namespace -}}{{- fail "analysisRuntime.orkaContainer.namespace must be dedicated and differ from orka.namespace" -}}{{- end -}}
  {{- if eq $analysisNamespace .Release.Namespace -}}{{- fail "analysisRuntime.orkaContainer.namespace must differ from the dashboard release namespace" -}}{{- end -}}
  {{- if and $cfg.namespace (not (hasSuffix (printf "-%s" (include "prow-ai-dashboard.orkaReleaseScope" .)) $cfg.namespace)) -}}{{- fail "analysisRuntime.orkaContainer.namespace must be dedicated to this release and end with its release scope" -}}{{- end -}}
  {{- if not (regexMatch "^https?://[^/@?#]+(/[^?#]*)?$" $cfg.api) -}}{{- fail "analysisRuntime.orkaContainer.api must be an absolute http or https URL without credentials" -}}{{- end -}}
  {{- if and $cfg.apiAuth.existingSecret (not $cfg.apiAuth.tokenKey) -}}{{- fail "analysisRuntime.orkaContainer.apiAuth.tokenKey is required when apiAuth.existingSecret is set" -}}{{- end -}}
  {{- if not $cfg.image.repository -}}{{- fail "analysisRuntime.orkaContainer.image.repository is required" -}}{{- end -}}
  {{- $imageTag := include "prow-ai-dashboard.resolvedImageTag" (list . $cfg.image.tag) -}}
  {{- if not $imageTag -}}{{- fail "analysisRuntime.orkaContainer.image.tag, global.imageTag, or Chart.appVersion is required" -}}{{- end -}}
  {{- if not (regexMatch "^(sha-[0-9a-fA-F]{7,64}|v?[0-9]+[.][0-9]+[.][0-9]+(-[0-9A-Za-z.-]+)?)$" $imageTag) -}}{{- fail "analysisRuntime.orkaContainer.image tag must be an immutable sha-<hex> or full semantic version" -}}{{- end -}}
  {{- if ne $cfg.image.pullPolicy "IfNotPresent" -}}{{- fail "analysisRuntime.orkaContainer.image.pullPolicy must be IfNotPresent for the pinned Orka controller" -}}{{- end -}}
  {{- if not $cfg.modelAuth.existingSecret -}}{{- fail "analysisRuntime.orkaContainer.modelAuth.existingSecret is required" -}}{{- end -}}
  {{- if not $cfg.modelAuth.tokenKey -}}{{- fail "analysisRuntime.orkaContainer.modelAuth.tokenKey is required" -}}{{- end -}}
  {{- if not $cfg.state.key -}}{{- fail "analysisRuntime.orkaContainer.state.key is required" -}}{{- end -}}
  {{- $maxConcurrent := printf "%v" $cfg.maxConcurrentTasks -}}
  {{- if not (regexMatch "^[1-9][0-9]{0,2}$" $maxConcurrent) -}}{{- fail "analysisRuntime.orkaContainer.maxConcurrentTasks must be an integer from 1 to 999" -}}{{- end -}}
  {{- $retries := printf "%v" $cfg.retries -}}
  {{- if not (regexMatch "^(0|[1-9][0-9]?)$" $retries) -}}{{- fail "analysisRuntime.orkaContainer.retries must be an integer from 0 to 99" -}}{{- end -}}
  {{- $goDurationPattern := "^(([0-9]+([.][0-9]+)?)|([.][0-9]+))(ns|us|µs|μs|ms|s|m|h)((([0-9]+([.][0-9]+)?)|([.][0-9]+))(ns|us|µs|μs|ms|s|m|h))*$" -}}
  {{- $pollInterval := printf "%v" $cfg.pollInterval -}}
  {{- if or (not (regexMatch $goDurationPattern $pollInterval)) (not (regexMatch "[1-9]" $pollInterval)) -}}{{- fail "analysisRuntime.orkaContainer.pollInterval must be a positive Go duration" -}}{{- end -}}
  {{- $roundedPoll := durationRound $pollInterval -}}
  {{- if regexMatch "(^([3-9][0-9]|[1-9][0-9]{2,})s$|[mh]$)" $roundedPoll -}}{{- fail "analysisRuntime.orkaContainer.pollInterval must be less than 30s" -}}{{- end -}}
  {{- $taskTimeout := printf "%v" $cfg.taskTimeout -}}
  {{- if or (not (regexMatch $goDurationPattern $taskTimeout)) (not (regexMatch "[1-9]" $taskTimeout)) -}}{{- fail "analysisRuntime.orkaContainer.taskTimeout must be a positive Go duration" -}}{{- end -}}
  {{- if not (index $cfg.nodeSelector "agentpool") -}}{{- fail "analysisRuntime.orkaContainer.nodeSelector.agentpool must select an explicit CPU pool" -}}{{- end -}}
  {{- $placement := printf "%s %s %s" (toJson $cfg.nodeSelector) (toJson $cfg.tolerations) (toJson $cfg.affinity) -}}
  {{- if regexMatch "(?i)(accelerator|nvidia|tesla|radeon|(^|[^a-z0-9])(gpu|a10|a100|h100|v100|p100|t4|l4|mi250|mi300)([^a-z0-9]|$))" $placement -}}{{- fail "analysisRuntime.orkaContainer placement must not select or tolerate GPU nodes" -}}{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Name of the Secret holding the server auth credentials (OAuth secret + session
key, or bot token).
*/}}
{{- define "prow-ai-dashboard.authSecret" -}}
{{- $a := .Values.server.actions -}}
{{- if eq $a.mode "oauth" -}}
{{- if $a.oauth.existingSecret -}}{{ $a.oauth.existingSecret }}{{- else -}}{{ printf "%s-auth" (include "prow-ai-dashboard.fullname" .) }}{{- end -}}
{{- else -}}
{{- if $a.proxy.existingSecret -}}{{ $a.proxy.existingSecret }}{{- else -}}{{ printf "%s-auth" (include "prow-ai-dashboard.fullname" .) }}{{- end -}}
{{- end -}}
{{- end -}}

{{/* Immutable Agent Sandbox executor image reference. */}}
{{- define "prow-ai-dashboard.agentSandboxExecutorImage" -}}
{{- printf "%s@%s" .Values.agentSandbox.fixRuntime.image.repository .Values.agentSandbox.fixRuntime.image.digest -}}
{{- end -}}

{{/* Dashboard ServiceAccount allowed to manage only Fix Sandboxes. */}}
{{- define "prow-ai-dashboard.agentSandboxClientServiceAccountName" -}}
{{- if .Values.agentSandbox.rbac.clientServiceAccountName -}}
{{- .Values.agentSandbox.rbac.clientServiceAccountName -}}
{{- else -}}
{{- printf "%s-agent-sandbox-client" (include "prow-ai-dashboard.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* Tokenless ServiceAccount used inside the executor Sandbox. */}}
{{- define "prow-ai-dashboard.agentSandboxWorkloadServiceAccountName" -}}
{{- .Values.agentSandbox.fixRuntime.workloadServiceAccount.name -}}
{{- end -}}

{{/* Cluster-scoped admission policy name, unique to the release. */}}
{{- define "prow-ai-dashboard.agentSandboxAdmissionName" -}}
{{- printf "%s-agent-sandbox-%s" (include "prow-ai-dashboard.fullname" .) (include "prow-ai-dashboard.orkaReleaseScope" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Non-secret Agent Sandbox runtime environment shared by server and fetcher. */}}
{{- define "prow-ai-dashboard.agentSandboxEnv" -}}
- name: AGENT_SANDBOX_NAMESPACE
  value: {{ .Values.agentSandbox.fixRuntime.namespace | quote }}
- name: AGENT_SANDBOX_IMAGE
  value: {{ include "prow-ai-dashboard.agentSandboxExecutorImage" . | quote }}
- name: AGENT_SANDBOX_SERVICE_ACCOUNT
  value: {{ include "prow-ai-dashboard.agentSandboxWorkloadServiceAccountName" . | quote }}
- name: AGENT_SANDBOX_RUNTIME_CLASS
  value: {{ .Values.agentSandbox.fixRuntime.runtimeClassName | quote }}
- name: AGENT_SANDBOX_MODEL_GATEWAY_ENDPOINT
  value: {{ .Values.agentSandbox.fixRuntime.modelGateway.endpoint | quote }}
- name: AGENT_SANDBOX_MODEL_GATEWAY_MODEL
  value: {{ .Values.agentSandbox.fixRuntime.modelGateway.model | quote }}
- name: AGENT_SANDBOX_MODEL_GATEWAY_PROTOCOL
  value: {{ .Values.agentSandbox.fixRuntime.modelGateway.protocolVersion | quote }}
- name: AGENT_SANDBOX_MODEL_GATEWAY_PUBLIC_CA_PRIVATE_DNS
  value: {{ ternary "true" "false" .Values.agentSandbox.fixRuntime.modelGateway.publicCAPrivateDNS | quote }}
- name: AGENT_SANDBOX_TIMEOUT
  value: {{ .Values.agentSandbox.fixRuntime.timeout | quote }}
- name: AGENT_SANDBOX_OUTPUT_LIMIT_BYTES
  value: {{ printf "%d" (int64 .Values.agentSandbox.fixRuntime.outputLimitBytes) | quote }}
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
{{- define "prow-ai-dashboard.validateAgentSandboxFixRuntime" -}}
{{- if .Values.agentSandbox.fixRuntime.enabled -}}
  {{- $cfg := .Values.agentSandbox.fixRuntime -}}
  {{- if .Values.project.existingConfigMap -}}{{- fail "agentSandbox.fixRuntime requires inline project.config so security-sensitive values can be compared" -}}{{- end -}}
  {{- if not .Values.project.config -}}{{- fail "agentSandbox.fixRuntime requires project.config" -}}{{- end -}}
  {{- $project := fromYaml .Values.project.config -}}
  {{- $projectAI := get $project "ai" | default dict -}}
  {{- $projectFix := get $projectAI "fix_prs" | default dict -}}
  {{- $projectRuntime := get $projectFix "agent_runtime" | default dict -}}
  {{- $projectGateway := get $projectRuntime "model_gateway" | default dict -}}
  {{- if ne (default "opencode" (get $projectRuntime "type")) "agent-sandbox" -}}{{- fail "agentSandbox.fixRuntime requires project ai.fix_prs.agent_runtime.type=agent-sandbox" -}}{{- end -}}
  {{- if not (or .Values.server.actions.enabled (default false (get $projectFix "enabled"))) -}}{{- fail "agentSandbox.fixRuntime requires server.actions.enabled=true or project ai.fix_prs.enabled=true" -}}{{- end -}}
  {{- if .Values.server.actions.oauth.privateRepositories -}}{{- fail "agentSandbox.fixRuntime supports public repositories only; OAuth privateRepositories must be false" -}}{{- end -}}
  {{- if or .Values.orka.fixRuntime.enabled .Values.orka.agentAnalysisShadow.enabled .Values.server.chat.sourceInvestigation.enabled (eq .Values.analysisRuntime.type "orka-container") -}}{{- fail "agentSandbox.fixRuntime cannot be combined with Orka runtimes or source investigation" -}}{{- end -}}
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
  {{- $dashboardTag := include "prow-ai-dashboard.resolvedImageTag" (list . $dashboardImage.tag) -}}
  {{- if not (regexMatch "^(sha-[0-9a-fA-F]{7,64}|v?[0-9]+[.][0-9]+[.][0-9]+(-[0-9A-Za-z.-]+)?)$" $dashboardTag) -}}{{- fail "agentSandbox.fixRuntime.dashboardImage tag must be an immutable sha-<hex> or full semantic version" -}}{{- end -}}
  {{- if ne $dashboardImage.pullPolicy "IfNotPresent" -}}{{- fail "agentSandbox.fixRuntime.dashboardImage.pullPolicy must be IfNotPresent" -}}{{- end -}}
  {{- $workloadSA := include "prow-ai-dashboard.agentSandboxWorkloadServiceAccountName" . -}}
  {{- if not $workloadSA -}}{{- fail "agentSandbox.fixRuntime.workloadServiceAccount.name is required" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $workloadSA) -}}{{- fail "agentSandbox.fixRuntime.workloadServiceAccount.name must be a lowercase Kubernetes object name" -}}{{- end -}}
  {{- $clientSA := include "prow-ai-dashboard.agentSandboxClientServiceAccountName" . -}}
  {{- if and (not .Values.agentSandbox.rbac.create) (not .Values.agentSandbox.rbac.clientServiceAccountName) -}}{{- fail "agentSandbox.rbac.clientServiceAccountName is required when chart-managed RBAC is disabled" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $clientSA) -}}{{- fail "agentSandbox.rbac.clientServiceAccountName must be a lowercase Kubernetes object name" -}}{{- end -}}
  {{- $publicCAPrivateDNS := default false $cfg.modelGateway.publicCAPrivateDNS -}}
  {{- if $publicCAPrivateDNS -}}
    {{- if not (regexMatch "^https://([A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?[.])+[A-Za-z]{2,}(:[0-9]+)?(/[^?#]*)?$" $cfg.modelGateway.endpoint) -}}{{- fail "agentSandbox.fixRuntime.modelGateway.endpoint must be a credential-free HTTPS DNS FQDN when publicCAPrivateDNS=true" -}}{{- end -}}
    {{- if regexMatch "^https://([^/@?#]+[.])?(openai[.]com|openai[.]azure[.]com|services[.]ai[.]azure[.]com|anthropic[.]com|githubcopilot[.]com|copilot[.]microsoft[.]com|moonshot[.]cn|kimi[.]com|generativelanguage[.]googleapis[.]com|api[.]nvidia[.]com|mistral[.]ai|cohere[.]ai|groq[.]com|together[.]xyz|deepseek[.]com|x[.]ai)(:[0-9]+)?(/|$)" (lower $cfg.modelGateway.endpoint) -}}{{- fail "agentSandbox.fixRuntime.modelGateway.endpoint must not be a direct model-provider endpoint" -}}{{- end -}}
    {{- if regexMatch "[.](svc([.]cluster[.]local)?|internal)(:[0-9]+)?(/|$)" (lower $cfg.modelGateway.endpoint) -}}{{- fail "agentSandbox.fixRuntime.modelGateway.publicCAPrivateDNS applies only to a privately resolved public FQDN" -}}{{- end -}}
  {{- else -}}
    {{- if not (regexMatch "^https://[^/@?#]+[.](svc([.]cluster[.]local)?|internal)(:[0-9]+)?(/[^?#]*)?$" $cfg.modelGateway.endpoint) -}}{{- fail "agentSandbox.fixRuntime.modelGateway.endpoint must be an internal HTTPS service URL or publicCAPrivateDNS must be true" -}}{{- end -}}
  {{- end -}}
  {{- if not $cfg.modelGateway.model -}}{{- fail "agentSandbox.fixRuntime.modelGateway.model is required" -}}{{- end -}}
  {{- if ne $cfg.modelGateway.protocolVersion "openai-chat-completions-v1" -}}{{- fail "agentSandbox.fixRuntime.modelGateway.protocolVersion must be openai-chat-completions-v1" -}}{{- end -}}
  {{- if not (regexMatch "^([1-9]|[12][0-9]|30)m$" (printf "%v" $cfg.timeout)) -}}{{- fail "agentSandbox.fixRuntime.timeout must use whole minutes from 1m through 30m" -}}{{- end -}}
  {{- $poll := printf "%v" $cfg.pollInterval -}}
  {{- if or (not (regexMatch "^(([0-9]+([.][0-9]+)?)|([.][0-9]+))(ms|s)$" $poll)) (not (regexMatch "[1-9]" $poll)) -}}{{- fail "agentSandbox.fixRuntime.pollInterval must be a positive duration below 30s" -}}{{- end -}}
  {{- if regexMatch "^([3-9][0-9]|[1-9][0-9]{2,})s$" (durationRound $poll) -}}{{- fail "agentSandbox.fixRuntime.pollInterval must be below 30s" -}}{{- end -}}
  {{- if or (lt (int64 $cfg.outputLimitBytes) 4096) (gt (int64 $cfg.outputLimitBytes) 1048576) -}}{{- fail "agentSandbox.fixRuntime.outputLimitBytes must be between 4096 and 1048576" -}}{{- end -}}
  {{- $overallSeconds := mul (trimSuffix "m" (printf "%v" $cfg.timeout) | int) 60 -}}
  {{- if ge (len $cfg.allowedCommands) (int $cfg.maxSteps) -}}{{- fail "agentSandbox.fixRuntime.maxSteps must reserve at least one coding-agent step after allowedCommands" -}}{{- end -}}
  {{- range $index, $command := $cfg.allowedCommands -}}
    {{- $argv := get $command "argv" | default list -}}
    {{- if eq (len $argv) 0 -}}{{- fail (printf "agentSandbox.fixRuntime.allowedCommands[%d].argv must be non-empty" $index) -}}{{- end -}}
    {{- $totalBytes := 0 -}}
    {{- range $argIndex, $arg := $argv -}}
      {{- if or (eq $arg "") (gt (len $arg) 1024) (regexMatch "[\r\n]" $arg) -}}{{- fail (printf "agentSandbox.fixRuntime.allowedCommands[%d].argv[%d] must be a bounded non-empty single-line string" $index $argIndex) -}}{{- end -}}
      {{- $totalBytes = add $totalBytes (len $arg) -}}
    {{- end -}}
    {{- if gt $totalBytes 4096 -}}{{- fail (printf "agentSandbox.fixRuntime.allowedCommands[%d].argv exceeds 4096 bytes" $index) -}}{{- end -}}
    {{- $executable := lower (trim (first $argv)) -}}
    {{- if or (contains "/" (first $argv)) (contains "\\" (first $argv)) -}}{{- fail (printf "agentSandbox.fixRuntime.allowedCommands[%d] must use a PATH-resolved executable" $index) -}}{{- end -}}
    {{- if has $executable (list "sh" "bash" "dash" "zsh" "ksh" "fish" "cmd" "cmd.exe" "powershell" "pwsh") -}}{{- fail (printf "agentSandbox.fixRuntime.allowedCommands[%d] must not invoke a shell" $index) -}}{{- end -}}
    {{- if has $executable (list "env" "busybox" "toybox") -}}{{- fail (printf "agentSandbox.fixRuntime.allowedCommands[%d] must not use a command dispatcher" $index) -}}{{- end -}}
    {{- if has $executable (list "opencode" "fixexecutor") -}}{{- fail (printf "agentSandbox.fixRuntime.allowedCommands[%d] must not invoke a coding agent or executor" $index) -}}{{- end -}}
    {{- if and (eq $executable "git") (ne (toJson $argv) (toJson (list "git" "diff" "--cached" "--check"))) -}}{{- fail (printf "agentSandbox.fixRuntime.allowedCommands[%d] git is reserved for the exact final diff check" $index) -}}{{- end -}}
    {{- $commandTimeout := printf "%v" (get $command "timeout") -}}
    {{- $commandSeconds := 0 -}}
    {{- if regexMatch "^[1-9][0-9]*s$" $commandTimeout -}}
      {{- if gt (len $commandTimeout) 5 -}}{{- fail (printf "agentSandbox.fixRuntime.allowedCommands[%d].timeout exceeds the execution timeout" $index) -}}{{- end -}}
      {{- $commandSeconds = trimSuffix "s" $commandTimeout | int -}}
    {{- else if regexMatch "^[1-9][0-9]*m$" $commandTimeout -}}
      {{- if gt (len $commandTimeout) 3 -}}{{- fail (printf "agentSandbox.fixRuntime.allowedCommands[%d].timeout exceeds the execution timeout" $index) -}}{{- end -}}
      {{- $commandSeconds = mul (trimSuffix "m" $commandTimeout | int) 60 -}}
    {{- else -}}
      {{- fail (printf "agentSandbox.fixRuntime.allowedCommands[%d].timeout must use positive whole seconds or minutes" $index) -}}
    {{- end -}}
    {{- if gt $commandSeconds $overallSeconds -}}{{- fail (printf "agentSandbox.fixRuntime.allowedCommands[%d].timeout exceeds the execution timeout" $index) -}}{{- end -}}
  {{- end -}}
  {{- if ne (toJson (get (last $cfg.allowedCommands) "argv")) (toJson (list "git" "diff" "--cached" "--check")) -}}{{- fail "agentSandbox.fixRuntime.allowedCommands must end with argv [git diff --cached --check]" -}}{{- end -}}
  {{- if ne (toJson $cfg.allowedCommands) (toJson (get $projectRuntime "allowed_commands" | default list)) -}}{{- fail "agentSandbox.fixRuntime.allowedCommands must exactly match project agent_runtime.allowed_commands" -}}{{- end -}}
  {{- if ne (int $cfg.maxSteps) (int (default 30 (get $projectRuntime "max_turns"))) -}}{{- fail "agentSandbox.fixRuntime.maxSteps must match project agent_runtime.max_turns" -}}{{- end -}}
  {{- if ne (int $cfg.maxFiles) (int (default 3 (get $projectFix "max_files"))) -}}{{- fail "agentSandbox.fixRuntime.maxFiles must match project ai.fix_prs.max_files" -}}{{- end -}}
  {{- if ne (printf "%v" $cfg.timeout) (printf "%v" (default "10m" (get $projectRuntime "timeout"))) -}}{{- fail "agentSandbox.fixRuntime.timeout must match project agent_runtime.timeout" -}}{{- end -}}
  {{- if ne (int64 $cfg.outputLimitBytes) (int64 (default 524288 (get $projectRuntime "output_limit_bytes"))) -}}{{- fail "agentSandbox.fixRuntime.outputLimitBytes must match project agent_runtime.output_limit_bytes" -}}{{- end -}}
  {{- if ne $cfg.modelGateway.endpoint (default "" (get $projectGateway "endpoint")) -}}{{- fail "agentSandbox.fixRuntime.modelGateway.endpoint must match project agent_runtime.model_gateway.endpoint" -}}{{- end -}}
  {{- if ne $cfg.modelGateway.model (default "" (get $projectGateway "model")) -}}{{- fail "agentSandbox.fixRuntime.modelGateway.model must match project agent_runtime.model_gateway.model" -}}{{- end -}}
  {{- if ne $cfg.modelGateway.protocolVersion (default "openai-chat-completions-v1" (get $projectGateway "protocol_version")) -}}{{- fail "agentSandbox.fixRuntime.modelGateway.protocolVersion must match project agent_runtime.model_gateway.protocol_version" -}}{{- end -}}
  {{- if ne $publicCAPrivateDNS (default false (get $projectGateway "public_ca_private_dns")) -}}{{- fail "agentSandbox.fixRuntime.modelGateway.publicCAPrivateDNS must match project agent_runtime.model_gateway.public_ca_private_dns" -}}{{- end -}}
  {{- if (default false (get $projectRuntime "allow_bash")) -}}{{- fail "agentSandbox.fixRuntime requires project agent_runtime.allow_bash=false" -}}{{- end -}}
  {{- range $env := concat .Values.server.extraEnv .Values.fetcher.extraEnv -}}
    {{- if hasPrefix "AGENT_SANDBOX_" (default "" $env.name) -}}{{- fail (printf "extraEnv must not override reserved Agent Sandbox variable %s" $env.name) -}}{{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}


{{/* Immutable analyzer executor image. */}}
{{- define "prow-ai-dashboard.agentSandboxAnalyzerExecutorImage" -}}
{{- printf "%s@%s" .Values.agentSandbox.analyzer.executorImage.repository .Values.agentSandbox.analyzer.executorImage.digest -}}
{{- end -}}

{{/* Immutable analyzer stager image. */}}
{{- define "prow-ai-dashboard.agentSandboxAnalyzerStagerImage" -}}
{{- printf "%s@%s" .Values.agentSandbox.analyzer.stagerImage.repository .Values.agentSandbox.analyzer.stagerImage.digest -}}
{{- end -}}

{{/* Dedicated ServiceAccount allowed to manage only analyzer Sandboxes. */}}
{{- define "prow-ai-dashboard.agentSandboxAnalyzerClientServiceAccountName" -}}
{{- if .Values.agentSandbox.analyzer.clientServiceAccount.name -}}
{{- .Values.agentSandbox.analyzer.clientServiceAccount.name -}}
{{- else -}}
{{- printf "%s-agent-sandbox-analyzer-client" (include "prow-ai-dashboard.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* Tokenless ServiceAccount used inside analyzer Sandboxes. */}}
{{- define "prow-ai-dashboard.agentSandboxAnalyzerWorkloadServiceAccountName" -}}
{{- .Values.agentSandbox.analyzer.workloadServiceAccount.name -}}
{{- end -}}

{{/* Cluster-scoped analyzer admission policy name. */}}
{{- define "prow-ai-dashboard.agentSandboxAnalyzerAdmissionName" -}}
{{- printf "%s-agent-sandbox-analyzer-%s" (include "prow-ai-dashboard.fullname" .) (include "prow-ai-dashboard.orkaReleaseScope" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Validate the disabled-by-default private Agent Sandbox analyzer. */}}
{{- define "prow-ai-dashboard.validateAgentSandboxAnalyzer" -}}
{{- if .Values.agentSandbox.analyzer.enabled -}}
  {{- $cfg := .Values.agentSandbox.analyzer -}}
  {{- if ne .Values.analysisRuntime.type "inprocess" -}}{{- fail "agentSandbox.analyzer requires analysisRuntime.type=inprocess" -}}{{- end -}}
  {{- if .Values.orka.agentAnalysisShadow.enabled -}}{{- fail "agentSandbox.analyzer cannot run with orka.agentAnalysisShadow" -}}{{- end -}}
  {{- if not $cfg.namespace -}}{{- fail "agentSandbox.analyzer.namespace is required" -}}{{- end -}}
  {{- if eq $cfg.namespace .Release.Namespace -}}{{- fail "agentSandbox.analyzer.namespace must differ from the dashboard release namespace" -}}{{- end -}}
  {{- if and .Values.agentSandbox.fixRuntime.enabled (eq $cfg.namespace .Values.agentSandbox.fixRuntime.namespace) -}}{{- fail "agentSandbox.analyzer.namespace must differ from agentSandbox.fixRuntime.namespace" -}}{{- end -}}
  {{- if and .Values.agentSandbox.causalCritic.enabled (eq $cfg.namespace .Values.agentSandbox.causalCritic.namespace) -}}{{- fail "agentSandbox.analyzer.namespace must differ from agentSandbox.causalCritic.namespace" -}}{{- end -}}
  {{- if or (gt (len $cfg.namespace) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $cfg.namespace)) -}}{{- fail "agentSandbox.analyzer.namespace must be a lowercase DNS label" -}}{{- end -}}
  {{- if or (gt (len $cfg.runtimeClassName) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $cfg.runtimeClassName)) -}}{{- fail "agentSandbox.analyzer.runtimeClassName is required and must be a lowercase RuntimeClass name" -}}{{- end -}}
  {{- range $name, $image := dict "executorImage" $cfg.executorImage "stagerImage" $cfg.stagerImage -}}
    {{- if not (regexMatch "^[^[:space:]@]+$" $image.repository) -}}{{- fail (printf "agentSandbox.analyzer.%s.repository is required without whitespace, credentials, or a digest" $name) -}}{{- end -}}
    {{- if not (regexMatch "^sha256:[0-9a-f]{64}$" $image.digest) -}}{{- fail (printf "agentSandbox.analyzer.%s.digest must be an immutable sha256 digest" $name) -}}{{- end -}}
  {{- end -}}
  {{- if eq (include "prow-ai-dashboard.agentSandboxAnalyzerExecutorImage" .) (include "prow-ai-dashboard.agentSandboxAnalyzerStagerImage" .) -}}{{- fail "agentSandbox.analyzer executor and stager images must be distinct" -}}{{- end -}}
  {{- if or (gt (len $cfg.input.existingClaim) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $cfg.input.existingClaim)) -}}{{- fail "agentSandbox.analyzer.input.existingClaim is required and must be a lowercase PVC name" -}}{{- end -}}
  {{- if and .Values.persistence.existingClaim (eq $cfg.input.existingClaim .Values.persistence.existingClaim) -}}{{- fail "agentSandbox.analyzer.input.existingClaim must differ from the public dashboard data PVC" -}}{{- end -}}
  {{- $workloadSA := include "prow-ai-dashboard.agentSandboxAnalyzerWorkloadServiceAccountName" . -}}
  {{- if or (gt (len $workloadSA) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $workloadSA)) -}}{{- fail "agentSandbox.analyzer.workloadServiceAccount.name is required and must be a lowercase object name" -}}{{- end -}}
  {{- if and (not .Values.agentSandbox.rbac.create) $cfg.clientServiceAccount.create -}}{{- fail "agentSandbox.analyzer.clientServiceAccount.create requires agentSandbox.rbac.create=true" -}}{{- end -}}
  {{- if and (not $cfg.clientServiceAccount.create) (not $cfg.clientServiceAccount.name) -}}{{- fail "agentSandbox.analyzer.clientServiceAccount.name is required when create=false" -}}{{- end -}}
  {{- $clientSA := include "prow-ai-dashboard.agentSandboxAnalyzerClientServiceAccountName" . -}}
  {{- if or (gt (len $clientSA) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $clientSA)) -}}{{- fail "agentSandbox.analyzer.clientServiceAccount.name must be a lowercase Kubernetes object name" -}}{{- end -}}
  {{- $gateway := $cfg.modelGateway -}}
  {{- if not (regexMatch "^https://[^/@?#]+(:[0-9]+)?(/[A-Za-z0-9._~!$&()*+,;=:@%/-]*)?$" $gateway.endpoint) -}}{{- fail "agentSandbox.analyzer.modelGateway.endpoint must be an absolute credential-free HTTPS URL" -}}{{- end -}}
  {{- if not (regexMatch "^https://[^/]+[.](svc|svc[.]cluster[.]local|internal)(:[0-9]+)?(/[A-Za-z0-9._~!$&()*+,;=:@%/-]*)?$" $gateway.endpoint) -}}{{- fail "agentSandbox.analyzer.modelGateway.endpoint must use internal service DNS" -}}{{- end -}}
  {{- if or (not $gateway.model) (gt (len $gateway.model) 256) (contains "\n" $gateway.model) (contains "\r" $gateway.model) -}}{{- fail "agentSandbox.analyzer.modelGateway.model must be non-empty, at most 256 bytes, and single-line" -}}{{- end -}}
  {{- if ne $gateway.protocolVersion "openai-chat-completions-v1" -}}{{- fail "agentSandbox.analyzer.modelGateway.protocolVersion must be openai-chat-completions-v1" -}}{{- end -}}
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
  {{- if eq (len $cfg.networkPolicy.gatewayNamespaceSelector) 0 -}}{{- fail "agentSandbox.analyzer.networkPolicy.gatewayNamespaceSelector is required" -}}{{- end -}}
  {{- if eq (len $cfg.networkPolicy.gatewayPodSelector) 0 -}}{{- fail "agentSandbox.analyzer.networkPolicy.gatewayPodSelector is required" -}}{{- end -}}
  {{- if or (lt (int $cfg.networkPolicy.gatewayPort) 1) (gt (int $cfg.networkPolicy.gatewayPort) 65535) -}}{{- fail "agentSandbox.analyzer.networkPolicy.gatewayPort is invalid" -}}{{- end -}}
  {{- $gatewayAuthority := regexFind "^https://[^/]+" $gateway.endpoint -}}
  {{- $explicitGatewayPort := regexFind ":[0-9]+$" $gatewayAuthority -}}
  {{- $endpointGatewayPort := 443 -}}
  {{- if $explicitGatewayPort -}}{{- $endpointGatewayPort = trimPrefix ":" $explicitGatewayPort | int -}}{{- end -}}
  {{- if ne (int $cfg.networkPolicy.gatewayPort) $endpointGatewayPort -}}{{- fail "agentSandbox.analyzer.networkPolicy.gatewayPort must match modelGateway.endpoint" -}}{{- end -}}
  {{- if or (eq (len $cfg.networkPolicy.dnsNamespaceSelector) 0) (eq (len $cfg.networkPolicy.dnsPodSelector) 0) -}}{{- fail "agentSandbox.analyzer DNS network selectors are required" -}}{{- end -}}
  {{- if and (eq $cfg.networkPolicy.mode "cilium") (or (not (hasKey $cfg.networkPolicy.dnsNamespaceSelector "kubernetes.io/metadata.name")) (not (get $cfg.networkPolicy.dnsNamespaceSelector "kubernetes.io/metadata.name"))) -}}{{- fail "agentSandbox.analyzer cilium mode requires dnsNamespaceSelector.kubernetes.io/metadata.name" -}}{{- end -}}
  {{- if and (eq $cfg.networkPolicy.mode "cilium") (not (regexMatch "^https://[a-z0-9]([-a-z0-9]*[a-z0-9])?[.][a-z0-9]([-a-z0-9]*[a-z0-9])?[.]svc([.]cluster[.]local)?(:[0-9]+)?(/[^?#]*)?$" $gateway.endpoint)) -}}{{- fail "agentSandbox.analyzer cilium mode requires a Kubernetes Service gateway endpoint" -}}{{- end -}}
  {{- if not $cfg.quota.enabled -}}{{- fail "agentSandbox.analyzer.quota.enabled must be true" -}}{{- end -}}
  {{- range $env := concat .Values.server.extraEnv .Values.fetcher.extraEnv -}}
    {{- if hasPrefix "AGENT_SANDBOX_ANALYSIS_" (default "" $env.name) -}}{{- fail (printf "extraEnv must not override reserved analyzer variable %s" $env.name) -}}{{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}


{{/* Immutable causal critic executor image. */}}
{{- define "prow-ai-dashboard.agentSandboxCriticExecutorImage" -}}
{{- printf "%s@%s" .Values.agentSandbox.causalCritic.image.repository .Values.agentSandbox.causalCritic.image.digest -}}
{{- end -}}

{{/* Tokenless ServiceAccount used inside critic Sandboxes. */}}
{{- define "prow-ai-dashboard.agentSandboxCriticWorkloadServiceAccountName" -}}
{{- .Values.agentSandbox.causalCritic.workloadServiceAccount.name -}}
{{- end -}}

{{/* Cluster-scoped critic admission policy name. */}}
{{- define "prow-ai-dashboard.agentSandboxCriticAdmissionName" -}}
{{- printf "%s-agent-sandbox-critic-%s" (include "prow-ai-dashboard.fullname" .) (include "prow-ai-dashboard.orkaReleaseScope" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "prow-ai-dashboard.causalCriticLedgerPath" -}}
{{- printf "%s/causal_critic.json" (trimSuffix "/" .Values.agentSandbox.causalCritic.ledger.mountPath) -}}
{{- end -}}

{{/* Non-secret critic runtime environment for scheduled fetcher or worker. */}}
{{- define "prow-ai-dashboard.agentSandboxCriticEnv" -}}
- name: AGENT_SANDBOX_CRITIC_NAMESPACE
  value: {{ .Values.agentSandbox.causalCritic.namespace | quote }}
- name: AGENT_SANDBOX_CRITIC_IMAGE
  value: {{ include "prow-ai-dashboard.agentSandboxCriticExecutorImage" . | quote }}
- name: AGENT_SANDBOX_CRITIC_SERVICE_ACCOUNT
  value: {{ include "prow-ai-dashboard.agentSandboxCriticWorkloadServiceAccountName" . | quote }}
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
{{- define "prow-ai-dashboard.validateAgentSandboxCausalCritic" -}}
{{- if .Values.agentSandbox.causalCritic.enabled -}}
  {{- $cfg := .Values.agentSandbox.causalCritic -}}
  {{- if not .Values.ai.enabled -}}{{- fail "agentSandbox.causalCritic requires ai.enabled=true" -}}{{- end -}}
  {{- if ne .Values.analysisRuntime.type "inprocess" -}}{{- fail "agentSandbox.causalCritic requires analysisRuntime.type=inprocess" -}}{{- end -}}
  {{- if .Values.orka.agentAnalysisShadow.enabled -}}{{- fail "agentSandbox.causalCritic cannot run with orka.agentAnalysisShadow" -}}{{- end -}}
  {{- if .Values.orka.fixRuntime.enabled -}}{{- fail "agentSandbox.causalCritic cannot run with orka.fixRuntime" -}}{{- end -}}
  {{- if .Values.agentSandbox.fixRuntime.enabled -}}{{- fail "agentSandbox.causalCritic cannot run with agentSandbox.fixRuntime" -}}{{- end -}}
  {{- if not $cfg.namespace -}}{{- fail "agentSandbox.causalCritic.namespace is required" -}}{{- end -}}
  {{- if eq $cfg.namespace .Release.Namespace -}}{{- fail "agentSandbox.causalCritic.namespace must differ from the dashboard release namespace" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $cfg.namespace) -}}{{- fail "agentSandbox.causalCritic.namespace must be a lowercase DNS label" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $cfg.runtimeClassName) -}}{{- fail "agentSandbox.causalCritic.runtimeClassName is required and must be a lowercase RuntimeClass name" -}}{{- end -}}
  {{- if not (regexMatch "^[^[:space:]@]+$" $cfg.image.repository) -}}{{- fail "agentSandbox.causalCritic.image.repository is required without whitespace, credentials, or a digest" -}}{{- end -}}
  {{- if not (regexMatch "^sha256:[0-9a-f]{64}$" $cfg.image.digest) -}}{{- fail "agentSandbox.causalCritic.image.digest must be an immutable sha256 digest" -}}{{- end -}}
  {{- if ne $cfg.image.pullPolicy "IfNotPresent" -}}{{- fail "agentSandbox.causalCritic.image.pullPolicy must be IfNotPresent" -}}{{- end -}}
  {{- $workloadSA := include "prow-ai-dashboard.agentSandboxCriticWorkloadServiceAccountName" . -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $workloadSA) -}}{{- fail "agentSandbox.causalCritic.workloadServiceAccount.name is required and must be a lowercase object name" -}}{{- end -}}
  {{- $clientSA := include "prow-ai-dashboard.agentSandboxClientServiceAccountName" . -}}
  {{- if and (not .Values.agentSandbox.rbac.create) (not .Values.agentSandbox.rbac.clientServiceAccountName) -}}{{- fail "agentSandbox.rbac.clientServiceAccountName is required when chart-managed RBAC is disabled" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $clientSA) -}}{{- fail "agentSandbox.rbac.clientServiceAccountName must be a lowercase Kubernetes object name" -}}{{- end -}}
  {{- if not $cfg.ledger.existingClaim -}}{{- fail "agentSandbox.causalCritic.ledger.existingClaim is required" -}}{{- end -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $cfg.ledger.existingClaim) -}}{{- fail "agentSandbox.causalCritic.ledger.existingClaim must be a lowercase object name" -}}{{- end -}}
  {{- if eq $cfg.ledger.existingClaim (include "prow-ai-dashboard.pvcName" .) -}}{{- fail "agentSandbox.causalCritic must use a PVC distinct from public dashboard data" -}}{{- end -}}
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

{{/* Whether scheduled/watch Fix PR reconciliation is enabled in project.yaml. */}}
{{- define "prow-ai-dashboard.agentSandboxFixScheduledEnabled" -}}
{{- if and .Values.agentSandbox.fixRuntime.enabled .Values.project.config -}}
  {{- $project := fromYaml .Values.project.config -}}
  {{- $projectAI := get $project "ai" | default dict -}}
  {{- $projectFix := get $projectAI "fix_prs" | default dict -}}
  {{- if (default false (get $projectFix "enabled")) -}}true{{- else -}}false{{- end -}}
{{- else -}}false{{- end -}}
{{- end -}}

{{/* Whether any scheduled Agent Sandbox lifecycle needs the dashboard client identity. */}}
{{- define "prow-ai-dashboard.agentSandboxScheduledEnabled" -}}
{{- if .Values.agentSandbox.causalCritic.enabled -}}true
{{- else -}}{{ include "prow-ai-dashboard.agentSandboxFixScheduledEnabled" . }}{{- end -}}
{{- end -}}

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

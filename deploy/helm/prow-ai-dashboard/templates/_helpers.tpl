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

{{/*
Image reference, defaulting the tag to the chart appVersion.
*/}}
{{- define "prow-ai-dashboard.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
Analyzer image used only by the experimental Orka container runtime.
*/}}
{{- define "prow-ai-dashboard.analyzerImage" -}}
{{- $tag := .Values.analysisRuntime.orkaContainer.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.analysisRuntime.orkaContainer.image.repository $tag -}}
{{- end -}}

{{/*
Git-capable engine image used by the opt-in fix runtime.
*/}}
{{- define "prow-ai-dashboard.fixerImage" -}}
{{- $tag := .Values.orka.fixRuntime.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.orka.fixRuntime.image.repository $tag -}}
{{- end -}}

{{/*
Release scope for cross-namespace Orka RBAC names.
*/}}
{{- define "prow-ai-dashboard.orkaReleaseScope" -}}
{{- printf "%s/%s" .Release.Namespace .Release.Name | sha256sum | trunc 8 -}}
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

{{/* Analysis RBAC stays separate from fix-generation RBAC. */}}
{{- define "prow-ai-dashboard.orkaAnalysisRBACName" -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 40 | trimSuffix "-" -}}
{{- printf "%s-analysis-%s" $base (include "prow-ai-dashboard.orkaReleaseScope" .) -}}
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
  {{- if ne .Values.mode "cron" -}}{{- fail "analysisRuntime.type=orka-container requires mode=cron" -}}{{- end -}}
  {{- if not .Values.ai.enabled -}}{{- fail "analysisRuntime.type=orka-container requires ai.enabled=true" -}}{{- end -}}
  {{- if not .Values.ai.endpoint -}}{{- fail "analysisRuntime.type=orka-container requires ai.endpoint" -}}{{- end -}}
  {{- if not .Values.ai.model -}}{{- fail "analysisRuntime.type=orka-container requires ai.model" -}}{{- end -}}
  {{- if not $cfg.namespace -}}{{- fail "analysisRuntime.orkaContainer.namespace is required" -}}{{- end -}}
  {{- if not $cfg.image.repository -}}{{- fail "analysisRuntime.orkaContainer.image.repository is required" -}}{{- end -}}
  {{- $imageTag := $cfg.image.tag | default .Chart.AppVersion -}}
  {{- if not $imageTag -}}{{- fail "analysisRuntime.orkaContainer.image.tag or Chart.appVersion is required" -}}{{- end -}}
  {{- if not (regexMatch "^(sha-[0-9a-fA-F]{7,64}|v?[0-9]+[.][0-9]+[.][0-9]+(-[0-9A-Za-z.-]+)?([+][0-9A-Za-z.-]+)?)$" $imageTag) -}}{{- fail "analysisRuntime.orkaContainer.image tag must be an immutable sha-<hex> or full semantic version" -}}{{- end -}}
  {{- if ne $cfg.image.pullPolicy "IfNotPresent" -}}{{- fail "analysisRuntime.orkaContainer.image.pullPolicy must be IfNotPresent for the pinned Orka controller" -}}{{- end -}}
  {{- if not $cfg.modelAuth.existingSecret -}}{{- fail "analysisRuntime.orkaContainer.modelAuth.existingSecret is required" -}}{{- end -}}
  {{- if not $cfg.modelAuth.tokenKey -}}{{- fail "analysisRuntime.orkaContainer.modelAuth.tokenKey is required" -}}{{- end -}}
  {{- if not $cfg.state.key -}}{{- fail "analysisRuntime.orkaContainer.state.key is required" -}}{{- end -}}
  {{- $maxConcurrent := printf "%v" $cfg.maxConcurrentTasks -}}
  {{- if not (regexMatch "^[1-9][0-9]{0,2}$" $maxConcurrent) -}}{{- fail "analysisRuntime.orkaContainer.maxConcurrentTasks must be an integer from 1 to 999" -}}{{- end -}}
  {{- $retries := printf "%v" $cfg.retries -}}
  {{- if not (regexMatch "^(0|[1-9][0-9]?)$" $retries) -}}{{- fail "analysisRuntime.orkaContainer.retries must be an integer from 0 to 99" -}}{{- end -}}
  {{- if not (regexMatch "^[1-9][0-9]*(ms|s|m|h)$" (printf "%v" $cfg.pollInterval)) -}}{{- fail "analysisRuntime.orkaContainer.pollInterval must be a positive Go duration" -}}{{- end -}}
  {{- if not (regexMatch "^[1-9][0-9]*(ms|s|m|h)$" (printf "%v" $cfg.taskTimeout)) -}}{{- fail "analysisRuntime.orkaContainer.taskTimeout must be a positive Go duration" -}}{{- end -}}
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

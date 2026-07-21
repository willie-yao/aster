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
Git-capable engine image used by the opt-in fix runtime.
*/}}
{{- define "prow-ai-dashboard.fixerImage" -}}
{{- $tag := .Values.orka.fixRuntime.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.orka.fixRuntime.image.repository $tag -}}
{{- end -}}

{{/*
Resolve an Orka component image tag through component, engine, and chart defaults.
*/}}
{{- define "prow-ai-dashboard.orkaProducerImage" -}}
{{- $tag := .Values.orka.producer.image.tag | default .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.orka.producer.image.repository $tag -}}
{{- end -}}

{{- define "prow-ai-dashboard.orkaIngestorImage" -}}
{{- $tag := .Values.orka.ingestor.image.tag | default .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.orka.ingestor.image.repository $tag -}}
{{- end -}}

{{- define "prow-ai-dashboard.orkaArtifactToolImage" -}}
{{- $tag := .Values.orka.artifactTool.image.tag | default .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.orka.artifactTool.image.repository $tag -}}
{{- end -}}

{{/*
Release-scoped Orka artifact Tool resources.
*/}}
{{- define "prow-ai-dashboard.orkaReleaseScope" -}}
{{- printf "%s/%s" .Release.Namespace .Release.Name | sha256sum | trunc 8 -}}
{{- end -}}

{{- define "prow-ai-dashboard.orkaArtifactToolName" -}}
{{- $base := include "prow-ai-dashboard.fullname" . | trunc 40 | trimSuffix "-" -}}
{{- printf "%s-artifact-tool-%s" $base (include "prow-ai-dashboard.orkaReleaseScope" .) -}}
{{- end -}}

{{- define "prow-ai-dashboard.orkaArtifactToolSelectorLabels" -}}
{{ include "prow-ai-dashboard.selectorLabels" . }}
prow-ai-dashboard.io/release-scope: {{ include "prow-ai-dashboard.orkaReleaseScope" . }}
{{- end -}}

{{- define "prow-ai-dashboard.orkaArtifactToolSecret" -}}
{{- if .Values.orka.artifactTool.auth.existingSecret -}}
{{- .Values.orka.artifactTool.auth.existingSecret -}}
{{- else -}}
{{- printf "%s-auth" (include "prow-ai-dashboard.orkaArtifactToolName" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "prow-ai-dashboard.orkaArtifactToolBaseURL" -}}
{{- if .Values.orka.artifactTool.enabled -}}
{{- printf "http://%s.%s.svc:%v" (include "prow-ai-dashboard.orkaArtifactToolName" .) .Values.orka.namespace .Values.orka.artifactTool.service.port -}}
{{- else -}}
{{- trimSuffix "/" .Values.orka.artifactTool.baseURL -}}
{{- end -}}
{{- end -}}

{{- define "prow-ai-dashboard.orkaBaseToolsConfigMap" -}}
{{- if .Values.orka.baseTools.existingConfigMap -}}
{{- .Values.orka.baseTools.existingConfigMap -}}
{{- else -}}
{{- printf "%s-orka-tools" (include "prow-ai-dashboard.fullname" .) | trunc 63 | trimSuffix "-" -}}
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
Name of the ServiceAccount the Orka analysis pipeline runs as.
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

{{/*
Validate the analysis backend selection and its constraints.
*/}}
{{- define "prow-ai-dashboard.validateAnalysis" -}}
{{- if not (or (eq .Values.analysis "inprocess") (eq .Values.analysis "orka")) -}}
{{- fail (printf "analysis must be \"inprocess\" or \"orka\", got %q" .Values.analysis) -}}
{{- end -}}
{{- if and (eq .Values.analysis "orka") (ne .Values.mode "cron") -}}
{{- fail "analysis: orka requires mode: cron (the produce->ingest flow is batch-oriented)" -}}
{{- end -}}
{{- if eq .Values.analysis "orka" -}}
{{- if not (has .Values.orka.apiMode (list "auto" "responses" "chat_completions")) -}}
{{- fail "orka.apiMode must be auto, responses, or chat_completions" -}}
{{- end -}}
{{- $maxConcurrentTasks := printf "%v" .Values.orka.producer.maxConcurrentTasks -}}
{{- if not (regexMatch "^(0|[1-9][0-9]{0,2}|1000)$" $maxConcurrentTasks) -}}
{{- fail "orka.producer.maxConcurrentTasks must be an integer between 0 and 1000" -}}
{{- end -}}
{{- if and .Values.orka.baseTools.create .Values.orka.baseTools.existingConfigMap -}}
{{- fail "orka.baseTools.create and orka.baseTools.existingConfigMap are mutually exclusive" -}}
{{- end -}}
{{- if and (not .Values.orka.baseTools.create) (not .Values.orka.baseTools.existingConfigMap) -}}
{{- fail "analysis: orka requires orka.baseTools.create=true or orka.baseTools.existingConfigMap" -}}
{{- end -}}
{{- if and (not .Values.orka.artifactTool.enabled) (not .Values.orka.artifactTool.baseURL) -}}
{{- fail "analysis: orka with artifactTool.enabled=false requires orka.artifactTool.baseURL" -}}
{{- end -}}
{{- if and (not .Values.orka.artifactTool.enabled) (not .Values.orka.artifactTool.auth.existingSecret) -}}
{{- fail "analysis: orka with artifactTool.enabled=false requires orka.artifactTool.auth.existingSecret" -}}
{{- end -}}
{{- if not .Values.orka.artifactTool.auth.tokenKey -}}
{{- fail "analysis: orka requires orka.artifactTool.auth.tokenKey" -}}
{{- end -}}
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

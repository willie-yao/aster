{{- define "prow-ai-dashboard-platform.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "prow-ai-dashboard-platform.fullname" -}}
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

{{- define "prow-ai-dashboard-platform.selectorLabels" -}}
app.kubernetes.io/name: {{ include "prow-ai-dashboard-platform.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "prow-ai-dashboard-platform.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "prow-ai-dashboard-platform.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: prow-ai-dashboard-platform
{{- end -}}

{{- define "prow-ai-dashboard-platform.gatewayName" -}}
{{- printf "%s-%s" (include "prow-ai-dashboard-platform.fullname" .) .Values.modelGateway.name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "prow-ai-dashboard-platform.bindingName" -}}
{{- printf "%s-prow-ai-dashboard-platform-binding" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "prow-ai-dashboard-platform.gatewayImage" -}}
{{- printf "%s@%s" .Values.modelGateway.image.repository .Values.modelGateway.image.digest -}}
{{- end -}}

{{- define "prow-ai-dashboard-platform.executionPolicyHash" -}}
{{- toJson (list .Values.execution.networkPolicy.allowedFQDNs .Values.modelGateway.enabled (ternary .Release.Namespace "" .Values.modelGateway.enabled) (ternary (include "prow-ai-dashboard-platform.name" .) "" .Values.modelGateway.enabled) (ternary .Release.Name "" .Values.modelGateway.enabled) (ternary (toString .Values.modelGateway.service.targetPort) "" .Values.modelGateway.enabled)) | sha256sum -}}
{{- end -}}

{{- define "prow-ai-dashboard-platform.gatewayPolicyHash" -}}
{{- toJson (list .Values.execution.namespace .Values.modelGateway.upstreamHost (toString .Values.modelGateway.service.targetPort)) | sha256sum -}}
{{- end -}}

{{- define "prow-ai-dashboard-platform.validatePublicHost" -}}
{{- $label := index . 0 -}}
{{- $host := lower (index . 1) -}}
{{- if not (regexMatch "^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\\.[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$" $host) -}}
{{- fail (printf "%s must be a public DNS hostname" $label) -}}
{{- end -}}
{{- if or (eq $host "localhost") (hasSuffix ".svc" $host) (hasSuffix ".cluster.local" $host) (hasSuffix ".local" $host) -}}
{{- fail (printf "%s must not use an internal or local hostname" $label) -}}
{{- end -}}
{{- end -}}

{{- define "prow-ai-dashboard-platform.validateFQDNs" -}}
{{- $label := index . 0 -}}
{{- $values := index . 1 -}}
{{- range $values -}}
  {{- $value := lower . -}}
  {{- if not (regexMatch "^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\\.[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$" $value) -}}
    {{- fail (printf "%s contains invalid or non-exact FQDN %q" $label .) -}}
  {{- end -}}
  {{- if or (eq $value "localhost") (hasSuffix ".svc" $value) (hasSuffix ".cluster.local" $value) (hasSuffix ".local" $value) -}}
    {{- fail (printf "%s contains internal or local FQDN %q" $label .) -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{- define "prow-ai-dashboard-platform.validate" -}}
{{- if or .Values.nameOverride .Values.fullnameOverride -}}{{- fail "nameOverride and fullnameOverride are unsupported because platform resource identities are immutable" -}}{{- end -}}
{{- if not .Values.application.releaseName -}}{{- fail "application.releaseName is required" -}}{{- end -}}
{{- if not .Values.execution.namespace -}}{{- fail "execution.namespace is required" -}}{{- end -}}
{{- if eq .Values.execution.namespace .Release.Namespace -}}{{- fail "execution.namespace must differ from the platform and application namespace" -}}{{- end -}}
{{- if not .Values.execution.runtimeClassName -}}{{- fail "execution.runtimeClassName is required" -}}{{- end -}}
{{- if not .Values.execution.workloadServiceAccountName -}}{{- fail "execution.workloadServiceAccountName is required" -}}{{- end -}}
{{- if ne .Values.agentSandbox.requiredVersion "v0.5.3" -}}{{- fail "agentSandbox.requiredVersion must remain v0.5.3" -}}{{- end -}}
{{- if ne .Values.agentSandbox.manifestURL "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.5.3/sandbox.yaml" -}}{{- fail "agentSandbox.manifestURL must remain the official v0.5.3 sandbox.yaml asset" -}}{{- end -}}
{{- if ne .Values.agentSandbox.manifestSHA256 "50f54b0e746376455ae6bb8b90b436bdd8798e1296cff0d72b6267bbeb858e3c" -}}{{- fail "agentSandbox.manifestSHA256 must remain the published v0.5.3 checksum" -}}{{- end -}}
{{- if .Values.runtimeClass.create -}}{{- fail "runtimeClass.create is unsupported; node infrastructure must provide the real handler" -}}{{- end -}}
{{- if ne .Values.execution.networkPolicy.mode "cilium" -}}{{- fail "execution.networkPolicy.mode must remain cilium for the supported FQDN-aware network-policy contract" -}}{{- end -}}
{{- if eq (len .Values.execution.networkPolicy.allowedFQDNs) 0 -}}{{- fail "execution.networkPolicy.allowedFQDNs must not be empty" -}}{{- end -}}
{{- include "prow-ai-dashboard-platform.validateFQDNs" (list "execution.networkPolicy.allowedFQDNs" .Values.execution.networkPolicy.allowedFQDNs) -}}
{{- $bindingName := include "prow-ai-dashboard-platform.bindingName" . -}}
{{- $binding := lookup "v1" "ConfigMap" .Release.Namespace $bindingName -}}
{{- if $binding -}}
  {{- if ne (index $binding.data "applicationReleaseName") .Values.application.releaseName -}}{{- fail "application.releaseName is immutable after platform installation" -}}{{- end -}}
  {{- if ne (index $binding.data "executionNamespace") .Values.execution.namespace -}}{{- fail "execution.namespace is immutable after platform installation" -}}{{- end -}}
{{- end -}}
{{- $existingNamespace := lookup "v1" "Namespace" "" .Values.execution.namespace -}}
{{- if $existingNamespace -}}
  {{- $namespaceLabels := $existingNamespace.metadata.labels | default dict -}}
  {{- if ne (get $namespaceLabels "prow-ai-dashboard/release") .Values.application.releaseName -}}
    {{- fail "execution.namespace is already bound to another application release" -}}
  {{- end -}}
  {{- $namespaceAnnotations := $existingNamespace.metadata.annotations | default dict -}}
  {{- if ne (get $namespaceAnnotations "prow-ai-dashboard/platform-release") .Release.Name -}}
    {{- fail "execution.namespace is already bound to another platform release" -}}
  {{- end -}}
{{- end -}}
{{- if .Values.modelGateway.enabled -}}
  {{- $gateway := .Values.modelGateway -}}
  {{- include "prow-ai-dashboard-platform.validatePublicHost" (list "modelGateway.publicHost" $gateway.publicHost) -}}
  {{- include "prow-ai-dashboard-platform.validatePublicHost" (list "modelGateway.upstreamHost" $gateway.upstreamHost) -}}
  {{- if or (contains "@" $gateway.upstreamURL) (contains "?" $gateway.upstreamURL) (contains "#" $gateway.upstreamURL) -}}{{- fail "modelGateway.upstreamURL must not contain credentials, a query, or a fragment" -}}{{- end -}}
  {{- $expectedURL := printf "https://%s" (lower $gateway.upstreamHost) -}}
  {{- if not (or (eq (lower $gateway.upstreamURL) $expectedURL) (hasPrefix (printf "%s/" $expectedURL) (lower $gateway.upstreamURL))) -}}{{- fail "modelGateway.upstreamURL host must match modelGateway.upstreamHost" -}}{{- end -}}
  {{- if or (not $gateway.image.repository) (contains "@" $gateway.image.repository) -}}{{- fail "modelGateway.image.repository is required and must not contain a digest" -}}{{- end -}}
  {{- if not (regexMatch "^sha256:[0-9a-f]{64}$" $gateway.image.digest) -}}{{- fail "modelGateway.image.digest must be a sha256 digest" -}}{{- end -}}
  {{- if not $gateway.providerAuth.existingSecret -}}{{- fail "modelGateway.providerAuth.existingSecret is required" -}}{{- end -}}
  {{- if not $gateway.providerAuth.tokenKey -}}{{- fail "modelGateway.providerAuth.tokenKey is required" -}}{{- end -}}
  {{- if not $gateway.tls.existingSecret -}}{{- fail "modelGateway.tls.existingSecret is required" -}}{{- end -}}
{{- end -}}
{{- end -}}

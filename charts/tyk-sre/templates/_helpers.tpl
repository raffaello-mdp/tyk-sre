{{/*
Name of the chart. Truncated to 63 characters, the
limit of a Kubernetes label value, and without a trailing "-" left by the cut.
*/}}
{{- define "tyk-sre.name" -}}
{{- default .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels. They must never change after the first install, because the
Deployment selector is immutable.
*/}}
{{- define "tyk-sre.selectorLabels" -}}
app.kubernetes.io/name: {{ include "tyk-sre.name" . }}
{{- end }}

{{/*
Labels set on every resource.
*/}}
{{- define "tyk-sre.labels" -}}
{{ include "tyk-sre.selectorLabels" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Image reference, with the chart appVersion as the default tag.
*/}}
{{- define "tyk-sre.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}

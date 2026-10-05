{{/*
Expand the name of the chart.
*/}}
{{- define "vbilling.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "vbilling.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "vbilling.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
app.kubernetes.io/name: {{ include "vbilling.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "vbilling.selectorLabels" -}}
app.kubernetes.io/name: {{ include "vbilling.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name
*/}}
{{- define "vbilling.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "vbilling.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Adapter list (falls back to the deprecated single `adapter` value).
*/}}
{{- define "vbilling.adapters" -}}
{{- $a := .Values.adapters | default list -}}
{{- if and (eq (len $a) 0) .Values.adapter -}}
{{- $a = list .Values.adapter -}}
{{- end -}}
{{- join "," $a -}}
{{- end }}

{{/*
Secret-backed env var. Uses the component's existingSecret when set,
otherwise the chart-managed secret when an inline value is given.
Args: dict "env" NAME "existing" SECRET "key" KEY "inline" VALUE "chartKey" KEY "root" $
*/}}
{{- define "vbilling.secretEnv" -}}
{{- if .existing }}
- name: {{ .env }}
  valueFrom:
    secretKeyRef:
      name: {{ .existing }}
      key: {{ .key }}
      optional: true
{{- else if .inline }}
- name: {{ .env }}
  valueFrom:
    secretKeyRef:
      name: {{ include "vbilling.fullname" .root }}
      key: {{ .chartKey }}
{{- end }}
{{- end }}

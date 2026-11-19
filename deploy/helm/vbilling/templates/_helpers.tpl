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
otherwise the chart-managed secret when an inline value is given. The
chart's Secret always holds ingest-token (given or generated, see
vbilling.ingestToken), as it always holds api-token.
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
{{- else if or .inline (eq .chartKey "ingest-token") }}
- name: {{ .env }}
  valueFrom:
    secretKeyRef:
      name: {{ include "vbilling.fullname" .root }}
      key: {{ .chartKey }}
{{- end }}
{{- end }}

{{/*
API token stored in the chart's Secret: api.token when set, else the token
the Secret already holds (helm lookup, so upgrades keep it), else a new
random one. Tools that render without cluster access (helm template) get a
new token on every render: set api.token or api.existingSecret with them.
*/}}
{{- define "vbilling.apiToken" -}}
{{- if .Values.api.token -}}
{{- .Values.api.token -}}
{{- else -}}
{{- $prev := lookup "v1" "Secret" .Release.Namespace (include "vbilling.fullname" .) -}}
{{- $data := dict -}}
{{- if $prev }}{{ $data = $prev.data | default dict }}{{ end -}}
{{- if hasKey $data "api-token" -}}
{{- index $data "api-token" | b64dec -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Shared ingest token stored in the chart's Secret: api.ingestToken when set,
else the token the Secret already holds (helm lookup, so upgrades keep it),
else a new random one. It is never the API token: gateways keep it in their
configuration. Tools that render without cluster access (helm template) get
a new token on every render: set api.ingestToken or api.existingSecret with
them.
*/}}
{{- define "vbilling.ingestToken" -}}
{{- if .Values.api.ingestToken -}}
{{- .Values.api.ingestToken -}}
{{- else -}}
{{- $prev := lookup "v1" "Secret" .Release.Namespace (include "vbilling.fullname" .) -}}
{{- $data := dict -}}
{{- if $prev }}{{ $data = $prev.data | default dict }}{{ end -}}
{{- $old := "" -}}
{{- if hasKey $data "ingest-token" }}{{ $old = index $data "ingest-token" | b64dec }}{{ end -}}
{{- $api := .Values.api.token -}}
{{- if and (not $api) (hasKey $data "api-token") }}{{ $api = index $data "api-token" | b64dec }}{{ end -}}
{{- if and $old (ne $old $api) -}}
{{- $old -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Secret that holds the API token (key api-token).
*/}}
{{- define "vbilling.apiTokenSecret" -}}
{{- .Values.api.existingSecret | default (include "vbilling.fullname" .) -}}
{{- end }}

{{/*
Published charts carry an immutable image digest in their annotations. Keep
it outside values so --reuse-values cannot retain an older release's default
digest. Explicit image.digest overrides win; custom repositories or tags
use their own image instead of the published default.
*/}}
{{- define "vbilling.image" -}}
{{- $digest := .Values.image.digest | default "" -}}
{{- $ann := .Chart.Annotations | default dict -}}
{{- $repo := index $ann "vbilling.vcluster.com/image-repository" | default "" -}}
{{- $tag := index $ann "vbilling.vcluster.com/image-tag" | default "" -}}
{{- if and (not $digest) (eq .Values.image.repository $repo) (eq .Values.image.tag $tag) -}}
{{- $digest = index $ann "vbilling.vcluster.com/image-digest" | default "" -}}
{{- end -}}
{{- if $digest -}}
{{- printf "%s@%s" .Values.image.repository $digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- end -}}
{{- end }}

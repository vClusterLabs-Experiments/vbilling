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
With "check" (the configuration check Job) a value from the chart's own
Secret is left out: that Secret does not exist before the first install and
holds the previous release's values before an upgrade, so the check would
judge credentials the new pod never gets. An existingSecret is read as it is.
Args: dict "env" NAME "existing" SECRET "key" KEY "inline" VALUE "chartKey" KEY "root" $ "check" BOOL
*/}}
{{- define "vbilling.secretEnv" -}}
{{- if .existing }}
- name: {{ .env }}
  valueFrom:
    secretKeyRef:
      name: {{ .existing }}
      key: {{ .key }}
      optional: true
{{- else if and (not .check) (or .inline (eq .chartKey "ingest-token")) }}
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

{{/*
Name of a ConfigMap of files the chart renders ("files", "prometheus-metrics",
"lago-price-card"). With "check" it is the copy the configuration check Job
reads: a hook of its own, because the Job runs before the release's
ConfigMaps are created or updated.
Args: dict "root" $ "name" NAME "check" BOOL
*/}}
{{- define "vbilling.configMapName" -}}
{{- printf "%s-%s%s" (include "vbilling.fullname" .root) .name (ternary "-config-check" "" (.check | default false)) -}}
{{- end }}

{{/*
"true" when the chart renders the files ConfigMap: custom metrics given as a
list, storage allowances, records formats and pulls, the Stripe price card.
*/}}
{{- define "vbilling.hasFiles" -}}
{{- $custom := and .Values.metering.customMetrics (not (kindIs "string" .Values.metering.customMetrics)) -}}
{{- $stripeCard := and (has "stripe" (include "vbilling.adapters" . | splitList ",")) .Values.stripe.priceCard -}}
{{- if or .Values.records.formats $custom .Values.records.pull $stripeCard .Values.billing.allowances }}true{{ end -}}
{{- end }}

{{/*
Data of the files ConfigMap, for the release's ConfigMap and the check's copy.
*/}}
{{- define "vbilling.filesData" -}}
{{- $custom := and .Values.metering.customMetrics (not (kindIs "string" .Values.metering.customMetrics)) -}}
{{- $stripeCard := and (has "stripe" (include "vbilling.adapters" . | splitList ",")) .Values.stripe.priceCard -}}
{{- if $custom }}
custom-metrics.yaml: |
  {{- toYaml .Values.metering.customMetrics | nindent 2 }}
{{- end }}
{{- if .Values.billing.allowances }}
allowances.yaml: |
  {{- toYaml .Values.billing.allowances | nindent 2 }}
{{- end }}
{{- if .Values.records.formats }}
records-formats.yaml: |
  {{- toYaml .Values.records.formats | nindent 2 }}
{{- end }}
{{- if .Values.records.pull }}
records-pull.yaml: |
  {{- toYaml .Values.records.pull | nindent 2 }}
{{- end }}
{{- if $stripeCard }}
stripe-price-card.yaml: |
  {{- toYaml .Values.stripe.priceCard | nindent 2 }}
{{- end }}
{{- end }}

{{/*
"true" when the lago adapter reads a price card: lago.priceCard (a ConfigMap
the chart renders) or lago.priceCardConfigMap (one the operator keeps). The
configuration check reads only the first: the operator's ConfigMap is not
part of the release and may not exist yet when the Job runs.
Args: dict "root" $ "check" BOOL
*/}}
{{- define "vbilling.lagoPriceCard" -}}
{{- $lago := .root.Values.lago -}}
{{- if has "lago" (include "vbilling.adapters" .root | splitList ",") -}}
{{- if .check -}}
{{- if and $lago.priceCard (not $lago.priceCardConfigMap) }}true{{ end -}}
{{- else if or $lago.priceCard $lago.priceCardConfigMap }}true{{ end -}}
{{- end -}}
{{- end }}

{{/*
Mounts of the configuration files: Secrets the operator keeps (external
clusters, the Platform CA) and the ConfigMaps of files. The same for
the StatefulSet and the configuration check Job.
Args: dict "root" $ "check" BOOL
*/}}
{{- define "vbilling.configVolumeMounts" -}}
{{- with .root -}}
{{- if .Values.tenantAPI.externalClustersSecret }}
- name: tenant-clusters
  mountPath: /etc/vbilling/tenants
  readOnly: true
{{- end }}
{{- if .Values.platform.caSecret }}
- name: platform-ca
  mountPath: /etc/vbilling/platform-ca
  readOnly: true
{{- end }}
{{- if .Values.prometheus.metrics }}
- name: prometheus-metrics
  mountPath: /etc/vbilling/prometheus
  readOnly: true
{{- end }}
{{- if include "vbilling.hasFiles" . }}
- name: files
  mountPath: /etc/vbilling/files
  readOnly: true
{{- end }}
{{- end }}
{{- if include "vbilling.lagoPriceCard" . }}
- name: lago-price-card
  mountPath: /etc/vbilling/lago
  readOnly: true
{{- end }}
{{- end }}

{{/*
Volumes behind vbilling.configVolumeMounts. With "check": the ConfigMaps are
the check's own copies, Secrets are optional (they may not exist before the
first install), and of the external clusters only clusters.yaml is
mounted, never the kubeconfigs it lists.
Args: dict "root" $ "check" BOOL
*/}}
{{- define "vbilling.configVolumes" -}}
{{- $check := .check | default false -}}
{{- $lagoPriceCard := include "vbilling.lagoPriceCard" . -}}
{{- with .root -}}
{{- if .Values.tenantAPI.externalClustersSecret }}
- name: tenant-clusters
  secret:
    secretName: {{ .Values.tenantAPI.externalClustersSecret }}
    {{- if $check }}
    optional: true
    items: [{key: clusters.yaml, path: clusters.yaml}]
    {{- end }}
{{- end }}
{{- if .Values.platform.caSecret }}
- name: platform-ca
  secret:
    secretName: {{ .Values.platform.caSecret }}
    {{- if $check }}
    optional: true
    {{- end }}
    items: [{key: ca.crt, path: ca.crt}]
{{- end }}
{{- if .Values.prometheus.metrics }}
- name: prometheus-metrics
  configMap:
    name: {{ include "vbilling.configMapName" (dict "root" . "name" "prometheus-metrics" "check" $check) }}
{{- end }}
{{- if include "vbilling.hasFiles" . }}
- name: files
  configMap:
    name: {{ include "vbilling.configMapName" (dict "root" . "name" "files" "check" $check) }}
{{- end }}
{{- if $lagoPriceCard }}
- name: lago-price-card
  configMap:
    name: {{ .Values.lago.priceCardConfigMap | default (include "vbilling.configMapName" (dict "root" . "name" "lago-price-card" "check" $check)) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Security context of the vBilling container, in the StatefulSet and in the
configuration check Job.
*/}}
{{- define "vbilling.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: [ALL]
{{- end }}

{{/*
Where the pod may run: the StatefulSet and the configuration check Job are
scheduled alike, so the check runs where the new pod will.
*/}}
{{- define "vbilling.scheduling" -}}
{{- with .Values.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.topologySpreadConstraints }}
topologySpreadConstraints:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.priorityClassName }}
priorityClassName: {{ . }}
{{- end }}
{{- end }}

{{/*
Name of the configuration check Job. A Job's name is also a label of its
pods, so it stays within 63 characters.
*/}}
{{- define "vbilling.configCheckName" -}}
{{- printf "%s-config-check" (include "vbilling.fullname" . | trunc 50 | trimSuffix "-") -}}
{{- end }}

{{/*
Annotations that make an object part of the configuration check: a Helm hook
that runs before an install and before an upgrade (Argo CD runs both as
PreSync hooks, ordered by the weight). The object is replaced by the next
run and removed once the hooks have succeeded.
Args: dict "weight" STRING
*/}}
{{- define "vbilling.configCheckHook" -}}
helm.sh/hook: pre-install,pre-upgrade
helm.sh/hook-weight: {{ .weight | quote }}
helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
{{- end }}

{{/*
The environment of the vBilling container: every setting the values map to.
The StatefulSet and the configuration check Job (config-check.yaml) both
render it, so the check reads the settings the new pod will get.
With "check", references to Secrets are optional (the Job runs before the
Secrets of a new installation exist) and the chart's own Secret is not read
(see vbilling.secretEnv).
Args: dict "root" $ "check" BOOL
*/}}
{{/*
persistence.archive.mirror as LEDGER_MIRROR: auto, on or off. YAML reads a
bare on or off as a boolean, so both spellings are taken.
*/}}
{{- define "vbilling.ledgerMirror" -}}
{{- $mirror := .Values.persistence.archive.mirror -}}
{{- if kindIs "bool" $mirror -}}
{{- ternary "on" "off" $mirror -}}
{{- else -}}
{{- $mirror | default "auto" -}}
{{- end -}}
{{- end -}}

{{/*
Where the mirror volume (persistence.mirror) is mounted. LEDGER_MIRROR_DIR is
<this path>/<pod name>: a directory of the mirror's own on that volume, which
it can set aside next to itself.
*/}}
{{- define "vbilling.mirrorMountPath" -}}
/var/lib/vbilling-mirror
{{- end -}}

{{- define "vbilling.env" -}}
{{- $root := .root -}}
{{- $check := .check | default false -}}
{{- with $root -}}
{{- $adapters := include "vbilling.adapters" . | splitList "," -}}
{{- $lagoPriceCard := include "vbilling.lagoPriceCard" (dict "root" . "check" $check) -}}
- name: ADAPTERS
  value: {{ include "vbilling.adapters" . | quote }}
- name: REGION
  value: {{ .Values.region | quote }}
- name: CLUSTER_NAME
  value: {{ .Values.clusterName | quote }}
- name: TENANT_SOURCE
  value: {{ .Values.tenantSource | quote }}
- name: DATA_DIR
  value: /var/lib/vbilling
{{- if or .Values.persistence.archive.enabled (.Values.persistence.mirror).enabled }}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
{{- end }}
{{- if .Values.persistence.archive.enabled }}
- name: ARCHIVE_DIR
  value: {{ printf "%s/$(POD_NAME)" (trimSuffix "/" .Values.persistence.archive.mountPath) | quote }}
{{- end }}
{{- if (.Values.persistence.mirror).enabled }}
- name: LEDGER_MIRROR_DIR
  value: {{ printf "%s/$(POD_NAME)" (include "vbilling.mirrorMountPath" .) | quote }}
{{- end }}
{{- if or .Values.persistence.archive.enabled (.Values.persistence.mirror).enabled }}
- name: LEDGER_MIRROR
  value: {{ include "vbilling.ledgerMirror" . | quote }}
{{- end }}
- name: LISTEN_ADDR
  value: ":{{ .Values.api.port }}"
- name: DEFAULT_PLAN_CODE
  value: {{ .Values.billing.defaultPlanCode | quote }}
- name: BILLING_CURRENCY
  value: {{ .Values.billing.currency | quote }}
- name: PRICE_CHECK
  value: {{ (.Values.billing.priceCheck).mode | default "warn" | quote }}
- name: COLLECTION_DELAY
  value: {{ .Values.billing.collectionDelay | default "5s" | quote }}
- name: CPU_MEMORY_BASIS
  value: {{ .Values.metering.cpuMemoryBasis | quote }}
- name: METER_CONTROL_PLANE
  value: {{ .Values.metering.meterControlPlane | quote }}
- name: METER_BY_NAMESPACE
  value: {{ .Values.metering.meterByNamespace | quote }}
- name: GPU_RESOURCES
  value: {{ join "," .Values.metering.gpuResources | default "none" | quote }}
- name: SKU_LABEL
  value: {{ .Values.metering.skuLabel | quote }}
- name: CAPACITY_TYPE_LABEL
  value: {{ .Values.metering.capacityTypeLabel | quote }}
- name: UNHEALTHY_NODE_TAINTS
  value: {{ join "," .Values.metering.unhealthyNodeTaints | default "none" | quote }}
- name: VCLUSTER_NODE_LABEL
  value: {{ .Values.metering.dedicatedNodeLabel | quote }}
{{- if kindIs "string" .Values.metering.customMetrics }}
- name: CUSTOM_METRICS
  value: {{ .Values.metering.customMetrics | quote }}
{{- else if .Values.metering.customMetrics }}
- name: CUSTOM_METRICS_FILE
  value: /etc/vbilling/files/custom-metrics.yaml
{{- end }}
{{- if .Values.billing.allowances }}
- name: ALLOWANCES_FILE
  value: /etc/vbilling/files/allowances.yaml
{{- end }}
{{- if .Values.billing.planByClass }}
- name: PLAN_BY_CLASS
  value: {{ $p := list }}{{ range $k, $v := .Values.billing.planByClass }}{{ $p = append $p (printf "%s=%s" $k $v) }}{{ end }}{{ join "," $p | quote }}
{{- end }}
{{- if .Values.billing.pricingKeys }}
- name: PRICING_KEYS
  value: {{ $pk := list }}{{ range $metric, $keys := .Values.billing.pricingKeys }}{{ if $keys }}{{ $pk = append $pk (printf "%s:%s" $metric (join "|" $keys)) }}{{ end }}{{ end }}{{ join "," $pk | quote }}
{{- end }}
{{- if .Values.crossChecks }}
- name: CROSS_CHECKS
  value: {{ $c := list }}{{ range .Values.crossChecks }}{{ $c = append $c (printf "%s=%s:%v%%" .primary .secondary (.tolerancePercent | default 1)) }}{{ end }}{{ join "," $c | quote }}
{{- end }}
- name: RECORDS_UNKNOWN_TENANTS
  value: {{ .Values.records.unknownTenants | quote }}
- name: RECORDS_LITELLM
  value: {{ .Values.records.litellm | quote }}
{{- with .Values.records.pullLiteLLM }}
{{- if .url }}
- name: LITELLM_URL
  value: {{ .url | quote }}
- name: LITELLM_CLIENT
  value: {{ .client | default "litellm" | quote }}
{{- with .tenant }}
- name: LITELLM_TENANT
  value: {{ . | quote }}
{{- end }}
{{- include "vbilling.secretEnv" (dict "env" "LITELLM_API_KEY" "existing" .existingSecret "key" "litellm-key" "inline" .key "chartKey" "litellm-key" "root" $root "check" $check) | nindent 0 }}
{{- end }}
{{- end }}
{{- if .Values.records.formats }}
- name: RECORDS_MAPPINGS_FILE
  value: /etc/vbilling/files/records-formats.yaml
{{- end }}
{{- if .Values.records.pull }}
- name: RECORDS_PULL_FILE
  value: /etc/vbilling/files/records-pull.yaml
{{- end }}
{{- range .Values.records.pullEnv }}
{{- if not (hasPrefix "RECORDS_PULL_" .name) }}
{{- fail (printf "records.pullEnv: %s must start with RECORDS_PULL_" .name) }}
{{- end }}
- name: {{ .name }}
  valueFrom:
    secretKeyRef: {name: {{ .secret | quote }}, key: {{ .key | quote }}{{ if $check }}, optional: true{{ end }}}
{{- end }}
- name: PRIVATE_NODE_BILLING
  value: {{ .Values.tenantAPI.privateNodeBilling | quote }}
{{- if .Values.tenantAPI.externalClustersSecret }}
- name: TENANT_CLUSTERS_FILE
  value: /etc/vbilling/tenants/clusters.yaml
{{- end }}
- name: PLATFORM_CLUSTER
  value: {{ .Values.platform.cluster | quote }}
{{- if eq .Values.tenantSource "platform" }}
- name: PLATFORM_URL
  value: {{ required "platform.url is required when tenantSource is platform" .Values.platform.url | quote }}
- name: PLATFORM_INSECURE
  value: {{ .Values.platform.insecure | quote }}
{{- if .Values.platform.caSecret }}
- name: PLATFORM_CA_FILE
  value: /etc/vbilling/platform-ca/ca.crt
{{- end }}
{{- include "vbilling.secretEnv" (dict "env" "PLATFORM_ACCESS_KEY" "existing" .Values.platform.existingSecret "key" "access-key" "inline" .Values.platform.accessKey "chartKey" "platform-access-key" "root" $root "check" $check) | nindent 0 }}
{{- end }}
- name: WATCH_NAMESPACES
  value: {{ join "," .Values.watchNamespaces | quote }}
- name: ENFORCEMENT_MODE
  value: {{ .Values.enforcement.mode | quote }}
- name: ENFORCEMENT_RULES
  value: {{ .Values.enforcement.rules | quote }}
{{- if .Values.enforcement.escalations }}
- name: ENFORCEMENT_ESCALATIONS
  value: {{ $e := list }}{{ range .Values.enforcement.escalations }}{{ $e = append $e (printf "%v:%v:%v" .from .after .to) }}{{ end }}{{ join "," $e | quote }}
{{- end }}
- name: ENFORCEMENT_IN_CLUSTER
  value: {{ .Values.enforcement.inCluster | quote }}
- name: ENFORCEMENT_LITELLM
  value: {{ ((.Values.enforcement).gateway).litellm | default false | quote }}
- name: PAUSE_KEEP_BILLING
  value: {{ .Values.enforcement.pause.keepBilling | quote }}
{{- with .Values.enforcement.pause.keepNamespaces }}
- name: PAUSE_KEEP_NAMESPACES
  value: {{ . | quote }}
{{- end }}
{{- if .Values.prometheus.url }}
- name: PROMETHEUS_URL
  value: {{ .Values.prometheus.url | quote }}
- name: EGRESS_QUERY
  value: {{ .Values.prometheus.egressQuery | quote }}
- name: GPU_UTIL_QUERY
  value: {{ .Values.prometheus.gpuUtilQuery | quote }}
- name: GPU_COUNT_QUERY
  value: {{ .Values.prometheus.gpuCountQuery | quote }}
- name: PROMETHEUS_PRESET
  value: {{ .Values.prometheus.preset | quote }}
{{- if .Values.prometheus.bearerTokenFile }}
- name: PROMETHEUS_BEARER_TOKEN_FILE
  value: {{ .Values.prometheus.bearerTokenFile | quote }}
{{- end }}
{{- include "vbilling.secretEnv" (dict "env" "PROMETHEUS_HEADERS" "existing" .Values.prometheus.existingSecret "key" "headers" "inline" .Values.prometheus.headers "chartKey" "prometheus-headers" "root" $root "check" $check) | nindent 0 }}
{{- end }}
{{- if .Values.prometheus.metrics }}
- name: PROMETHEUS_METRICS_FILE
  value: /etc/vbilling/prometheus/metrics.yaml
{{- end }}
- name: PROMETHEUS_SOURCE_LABEL
  value: {{ .Values.prometheus.sourceLabel | quote }}
{{- /* The chart's Secret always holds api-token (given or generated) unless api.existingSecret is set. */}}
{{- include "vbilling.secretEnv" (dict "env" "API_TOKEN" "existing" .Values.api.existingSecret "key" "api-token" "inline" true "chartKey" "api-token" "root" $root "check" $check) | nindent 0 }}
{{- include "vbilling.secretEnv" (dict "env" "INGEST_TOKEN" "existing" .Values.api.existingSecret "key" "ingest-token" "inline" .Values.api.ingestToken "chartKey" "ingest-token" "root" $root "check" $check) | nindent 0 }}
{{- include "vbilling.secretEnv" (dict "env" "INGEST_TOKENS" "existing" .Values.api.existingSecret "key" "ingest-tokens" "inline" .Values.api.tenantIngestTokens "chartKey" "ingest-tokens" "root" $root "check" $check) | nindent 0 }}
{{- if has "lago" $adapters }}
- name: LAGO_API_URL
  value: {{ .Values.lago.apiURL | quote }}
{{- if $lagoPriceCard }}
- name: LAGO_PRICE_CARD
  value: /etc/vbilling/lago/price-card.yaml
{{- end }}
{{- with .Values.lago.managedPlans }}
- name: LAGO_MANAGED_PLANS
  value: {{ join "," . | quote }}
{{- end }}
{{- include "vbilling.secretEnv" (dict "env" "LAGO_API_KEY" "existing" .Values.lago.existingSecret "key" "api-key" "inline" .Values.lago.apiKey "chartKey" "lago-api-key" "root" $root "check" $check) | nindent 0 }}
{{- end }}
{{- $metronomeProvision := and (has "metronome" $adapters) (or .Values.metronome.provision .Values.metronome.stripeLink) }}
{{- if or (has "stripe" $adapters) (and $metronomeProvision (or .Values.stripe.apiKey .Values.stripe.existingSecret)) }}
- name: STRIPE_API_BASE
  value: {{ .Values.stripe.apiBase | quote }}
- name: STRIPE_SPLIT_METERS_BY
  value: {{ join "," .Values.stripe.splitMetersBy | default "none" | quote }}
- name: STRIPE_AUTO_SUBSCRIBE
  value: {{ .Values.stripe.autoSubscribe | quote }}
{{- if .Values.stripe.priceCard }}
- name: STRIPE_PRICE_CARD
  value: /etc/vbilling/files/stripe-price-card.yaml
{{- end }}
{{- include "vbilling.secretEnv" (dict "env" "STRIPE_API_KEY" "existing" .Values.stripe.existingSecret "key" "api-key" "inline" .Values.stripe.apiKey "chartKey" "stripe-api-key" "root" $root "check" $check) | nindent 0 }}
{{- end }}
{{- include "vbilling.secretEnv" (dict "env" "STRIPE_WEBHOOK_SECRET" "existing" .Values.stripe.existingSecret "key" "webhook-secret" "inline" .Values.stripe.webhookSecret "chartKey" "stripe-webhook-secret" "root" $root "check" $check) | nindent 0 }}
{{- if has "metronome" $adapters }}
- name: METRONOME_API_BASE
  value: {{ .Values.metronome.apiBase | quote }}
- name: METRONOME_PROVISION
  value: {{ or .Values.metronome.provision .Values.metronome.stripeLink | quote }}
- name: METRONOME_RATE_CARD
  value: {{ .Values.metronome.rateCard | quote }}
{{- include "vbilling.secretEnv" (dict "env" "METRONOME_API_TOKEN" "existing" .Values.metronome.existingSecret "key" "api-token" "inline" .Values.metronome.apiToken "chartKey" "metronome-api-token" "root" $root "check" $check) | nindent 0 }}
{{- end }}
{{- include "vbilling.secretEnv" (dict "env" "METRONOME_WEBHOOK_SECRET" "existing" .Values.metronome.existingSecret "key" "webhook-secret" "inline" .Values.metronome.webhookSecret "chartKey" "metronome-webhook-secret" "root" $root "check" $check) | nindent 0 }}
{{- if has "webhook" $adapters }}
- name: WEBHOOK_URL
  value: {{ .Values.webhook.url | quote }}
{{- if .Values.webhook.headers }}
- name: WEBHOOK_HEADERS
  value: {{ $h := list }}{{ range $k, $v := .Values.webhook.headers }}{{ $h = append $h (printf "%s=%s" $k $v) }}{{ end }}{{ join "," $h | quote }}
{{- end }}
{{- include "vbilling.secretEnv" (dict "env" "WEBHOOK_SECRET" "existing" .Values.webhook.existingSecret "key" "secret" "inline" .Values.webhook.secret "chartKey" "webhook-secret" "root" $root "check" $check) | nindent 0 }}
{{- end }}
{{- end }}
{{- end }}

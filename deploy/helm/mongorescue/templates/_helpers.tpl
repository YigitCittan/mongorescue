{{/*
Expand the name of the chart.
*/}}
{{- define "mongorescue.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name, truncated to 63 characters (DNS label limit).
*/}}
{{- define "mongorescue.fullname" -}}
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

{{- define "mongorescue.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "mongorescue.labels" -}}
helm.sh/chart: {{ include "mongorescue.chart" . }}
{{ include "mongorescue.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "mongorescue.selectorLabels" -}}
app.kubernetes.io/name: {{ include "mongorescue.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "mongorescue.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "mongorescue.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The container image: repository@digest, or repository:tag (the appVersion by default).
*/}}
{{- define "mongorescue.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}

{{/*
The Secret and key holding MONGORESCUE_SECRET_KEY, or nothing when the key lives in
secret.key (or, with keyWrap, secret.key.wrapped) on the data volume.
*/}}
{{- define "mongorescue.secretKeySecret" -}}
{{- if .Values.secretKey.existingSecret }}
{{- .Values.secretKey.existingSecret }}
{{- else if and .Values.secretKey.generate (not .Values.keyWrap) }}
{{- printf "%s-secret-key" (include "mongorescue.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
MONGORESCUE_SHUTDOWN_GRACE: config.shutdownGrace, or terminationGracePeriodSeconds
minus the 60 seconds MongoRescue needs to cancel the runs left and stop.
*/}}
{{- define "mongorescue.shutdownGrace" -}}
{{- if .Values.config.shutdownGrace }}
{{- .Values.config.shutdownGrace }}
{{- else }}
{{- printf "%ds" (max 0 (sub (int .Values.terminationGracePeriodSeconds) 60)) }}
{{- end }}
{{- end }}

{{/*
The HA cluster label both releases of a cluster put on their pods: the selector of
the shared Service and of the anti-affinity and topology spread examples.
*/}}
{{- define "mongorescue.haClusterLabel" -}}
mongorescue.io/ha-cluster: {{ .Values.ha.cluster | quote }}
{{- end }}

{{/*
The name of the shared Service of the HA cluster.
*/}}
{{- define "mongorescue.haServiceName" -}}
{{- default (printf "mongorescue-%s" .Values.ha.cluster | trunc 63 | trimSuffix "-") .Values.ha.sharedService.name }}
{{- end }}

{{/*
The path of the readiness probe: /api/v1/ready (200 only on the active instance,
not draining, store writable, disk OK) with ha.readinessActiveOnly, otherwise the
liveness check /api/v1/health.
*/}}
{{- define "mongorescue.readinessPath" -}}
{{- if and .Values.ha.enabled .Values.ha.readinessActiveOnly }}/api/v1/ready{{ else }}/api/v1/health{{ end }}
{{- end }}

{{- define "sindri.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "sindri.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "sindri.labels" -}}
{{ include "sindri.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/*
"true" when config.storage is the S3 backend, in either spelling: the short
form s3://bucket/prefix or a mapping with backend: s3.
*/}}
{{- define "sindri.usesS3" -}}
{{- $storage := .Values.config.storage -}}
{{- if kindIs "string" $storage -}}
{{- if hasPrefix "s3://" $storage -}}true{{- end -}}
{{- else if kindIs "map" $storage -}}
{{- if eq (toString $storage.backend) "s3" -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Sindri's defaults for these three are development values, so a chart install
that left them out would quietly run on a published credential.
*/}}
{{- define "sindri.validate" -}}
{{- $config := .Values.config -}}
{{- if not $config.database -}}
{{- fail "config.database is required: Sindri's default is a development database" -}}
{{- end -}}
{{- if not $config.storage -}}
{{- fail "config.storage is required" -}}
{{- end -}}
{{- $auth := default dict $config.auth -}}
{{- if not $auth.static_token -}}
{{- fail "config.auth.static_token is required: Sindri's default is a published development token. Use ${SINDRI_TOKEN} and supply it through envFrom" -}}
{{- end -}}
{{- if and (not (include "sindri.usesS3" .)) (not .Values.persistence.enabled) -}}
{{- fail "config.storage is the filesystem backend, so persistence.enabled must be true: without a volume every blob is lost when the pod restarts" -}}
{{- end -}}
{{- end -}}

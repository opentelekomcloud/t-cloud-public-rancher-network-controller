{{- define "t-cloud-network-controller.name" -}}
t-cloud-network-controller
{{- end }}

{{- define "t-cloud-network-controller.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "t-cloud-network-controller.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "t-cloud-network-controller.labels" -}}
app.kubernetes.io/name: {{ include "t-cloud-network-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}


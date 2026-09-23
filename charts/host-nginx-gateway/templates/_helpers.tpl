{{- define "host-nginx-gateway.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "host-nginx-gateway.fullname" -}}
{{- default (printf "%s" (include "host-nginx-gateway.name" .)) .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "host-nginx-gateway.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "host-nginx-gateway.name" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "kch.name" -}}{{ .Release.Name }}-kubecost-auto-hpa{{- end -}}
{{- define "kch.labels" -}}
app.kubernetes.io/name: kubecost-auto-hpa
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}
{{- define "kch.selector" -}}
app.kubernetes.io/name: kubecost-auto-hpa
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}
{{- define "kch.redisAddr" -}}
{{- if .Values.redis.enabled -}}{{ include "kch.name" . }}-redis:6379{{- else -}}{{ .Values.redis.external.addr }}{{- end -}}
{{- end -}}

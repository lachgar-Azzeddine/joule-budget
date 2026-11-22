{{- define "joule-budget.labels" -}}
app.kubernetes.io/name: joule-budget
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- define "joule-budget.selector" -}}
app.kubernetes.io/name: joule-budget
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

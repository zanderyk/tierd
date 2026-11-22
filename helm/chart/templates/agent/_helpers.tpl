{{/* Labels on every agent object. */}}
{{- define "tierd.agentLabels" -}}
app.kubernetes.io/component: agent
app.kubernetes.io/name: {{ include "tierd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- end }}

{{/* Selector labels of the agent pods. */}}
{{- define "tierd.agentSelectorLabels" -}}
app.kubernetes.io/component: agent
app.kubernetes.io/name: {{ include "tierd.name" . }}
{{- end }}

{{/* The agent image: agent.image, falling back to the manager's. */}}
{{- define "tierd.agentImage" -}}
{{- $repo := .Values.agent.image.repository | default .Values.manager.image.repository -}}
{{- $tag := .Values.agent.image.tag | default .Values.manager.image.tag | default .Chart.AppVersion -}}
{{- if contains "@" $repo -}}{{ $repo }}{{- else -}}{{ $repo }}:{{ $tag }}{{- end -}}
{{- end }}

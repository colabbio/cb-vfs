{{/*
Expand the name of the chart.
*/}}
{{- define "medvfs.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "medvfs.fullname" -}}
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
{{- define "medvfs.labels" -}}
helm.sh/chart: {{ include "medvfs.name" . }}-{{ .Chart.Version | replace "+" "_" }}
{{ include "medvfs.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "medvfs.selectorLabels" -}}
app.kubernetes.io/name: {{ include "medvfs.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name
*/}}
{{- define "medvfs.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "medvfs.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Container spec (shared across DaemonSet, Deployment, and Sidecar)
*/}}
{{- define "medvfs.containerSpec" -}}
- name: medvfs
  image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
  imagePullPolicy: {{ .Values.image.pullPolicy }}
  args:
    - serve
    - --fuse
    - --port={{ .Values.vfs.port }}
    - --mount-point={{ .Values.vfs.mountPoint }}
    - --cache-dir={{ .Values.vfs.cacheDir }}
    {{- if .Values.configInline }}
    - -c=/etc/medvfs/vfs.yaml
    {{- else if .Values.existingConfigMap }}
    - -c=/etc/medvfs/vfs.yaml
    {{- end }}
  ports:
    - name: http
      containerPort: {{ .Values.vfs.port }}
      protocol: TCP
    {{- if .Values.metrics.enabled }}
    - name: metrics
      containerPort: {{ .Values.metrics.port }}
      protocol: TCP
    {{- end }}
  {{- if .Values.healthCheck.enabled }}
  livenessProbe:
    httpGet:
      path: {{ .Values.healthCheck.path }}
      port: http
    initialDelaySeconds: {{ .Values.healthCheck.initialDelaySeconds }}
    periodSeconds: {{ .Values.healthCheck.periodSeconds }}
  readinessProbe:
    httpGet:
      path: {{ .Values.healthCheck.path }}
      port: http
    initialDelaySeconds: 5
    periodSeconds: 10
  {{- end }}
  resources:
    {{- toYaml .Values.resources | nindent 4 }}
  securityContext:
    {{- toYaml .Values.securityContext | nindent 4 }}
  volumeMounts:
    - name: fuse-mount
      mountPath: {{ .Values.vfs.mountPoint }}
      mountPropagation: {{ .Values.hostFuse.mountPropagation }}
    - name: cache
      mountPath: {{ .Values.vfs.cacheDir }}
    - name: dev-fuse
      mountPath: /dev/fuse
    {{- if or .Values.configInline .Values.existingConfigMap }}
    - name: config
      mountPath: /etc/medvfs
      readOnly: true
    {{- end }}
    {{- if .Values.output.sync.enabled }}
    - name: output
      mountPath: {{ .Values.output.localDir }}
    {{- end }}
{{- end }}

{{/*
Volume spec (shared across DaemonSet, Deployment, and Sidecar)
*/}}
{{- define "medvfs.volumeSpec" -}}
- name: dev-fuse
  hostPath:
    path: /dev/fuse
{{- if or .Values.configInline .Values.existingConfigMap }}
- name: config
  configMap:
    {{- if .Values.existingConfigMap }}
    name: {{ .Values.existingConfigMap }}
    {{- else }}
    name: {{ include "medvfs.fullname" . }}-config
    {{- end }}
{{- end }}
{{- if eq .Values.mode "daemonset" }}
- name: fuse-mount
  hostPath:
    path: {{ .Values.hostFuse.mountPath }}
    type: DirectoryOrCreate
- name: cache
  {{- if eq .Values.cache.type "hostPath" }}
  hostPath:
    path: {{ .Values.cache.hostPath }}
    type: DirectoryOrCreate
  {{- else }}
  emptyDir: {}
  {{- end }}
{{- else }}
- name: fuse-mount
  emptyDir: {}
- name: cache
  {{- if eq .Values.cache.type "pvc" }}
  persistentVolumeClaim:
    claimName: {{ include "medvfs.fullname" . }}-cache
  {{- else }}
  emptyDir: {}
  {{- end }}
{{- end }}
{{- if .Values.output.sync.enabled }}
- name: output
  emptyDir: {}
{{- end }}
{{- end }}

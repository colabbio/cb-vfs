{{/*
Sidecar container specs for clinical tools.
Renders container definitions for enabled preset sidecars and custom sidecars.
All sidecars share the FUSE mount at vfs.mountPoint.
*/}}

{{/*
Render all sidecar containers (presets + custom).
Called from deployment.yaml / daemonset.yaml when sidecars are enabled.
*/}}
{{- define "medvfs.sidecarContainers" -}}
{{- if .Values.sidecars.enabled }}

{{/* ── Preset: OpenSlide ── */}}
{{- if .Values.sidecars.openslide.enabled }}
- name: openslide
  image: {{ .Values.sidecars.openslide.image | quote }}
  imagePullPolicy: IfNotPresent
  {{- with .Values.sidecars.openslide.command }}
  command:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.sidecars.openslide.args }}
  args:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- if .Values.sidecars.openslide.port }}
  ports:
    - name: openslide
      containerPort: {{ .Values.sidecars.openslide.port }}
      protocol: TCP
  {{- end }}
  resources:
    {{- toYaml .Values.sidecars.openslide.resources | nindent 4 }}
  {{- with .Values.sidecars.openslide.env }}
  env:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  volumeMounts:
    - name: fuse-mount
      mountPath: {{ .Values.vfs.mountPoint }}
      readOnly: true
{{- end }}

{{/* ── Preset: ClamAV ── */}}
{{- if .Values.sidecars.clamav.enabled }}
- name: clamav
  image: {{ .Values.sidecars.clamav.image | quote }}
  imagePullPolicy: IfNotPresent
  {{- with .Values.sidecars.clamav.command }}
  command:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.sidecars.clamav.args }}
  args:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  resources:
    {{- toYaml .Values.sidecars.clamav.resources | nindent 4 }}
  {{- with .Values.sidecars.clamav.env }}
  env:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  volumeMounts:
    - name: fuse-mount
      mountPath: {{ .Values.vfs.mountPoint }}
      readOnly: true
{{- end }}

{{/* ── Preset: libvips ── */}}
{{- if .Values.sidecars.libvips.enabled }}
- name: libvips
  image: {{ .Values.sidecars.libvips.image | quote }}
  imagePullPolicy: IfNotPresent
  {{- with .Values.sidecars.libvips.command }}
  command:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.sidecars.libvips.args }}
  args:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  resources:
    {{- toYaml .Values.sidecars.libvips.resources | nindent 4 }}
  {{- with .Values.sidecars.libvips.env }}
  env:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  volumeMounts:
    - name: fuse-mount
      mountPath: {{ .Values.vfs.mountPoint }}
      readOnly: true
{{- end }}

{{/* ── Preset: samtools ── */}}
{{- if .Values.sidecars.samtools.enabled }}
- name: samtools
  image: {{ .Values.sidecars.samtools.image | quote }}
  imagePullPolicy: IfNotPresent
  {{- with .Values.sidecars.samtools.command }}
  command:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.sidecars.samtools.args }}
  args:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  resources:
    {{- toYaml .Values.sidecars.samtools.resources | nindent 4 }}
  {{- with .Values.sidecars.samtools.env }}
  env:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  volumeMounts:
    - name: fuse-mount
      mountPath: {{ .Values.vfs.mountPoint }}
      readOnly: true
{{- end }}

{{/* ── Custom sidecars ── */}}
{{- range .Values.sidecars.custom }}
- name: {{ .name }}
  image: {{ .image | quote }}
  imagePullPolicy: {{ .imagePullPolicy | default "IfNotPresent" }}
  {{- with .command }}
  command:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .args }}
  args:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .ports }}
  ports:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .env }}
  env:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  volumeMounts:
    - name: fuse-mount
      mountPath: {{ $.Values.vfs.mountPoint }}
      readOnly: true
    {{- with .volumeMounts }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
{{- end }}

{{- end }}{{/* if sidecars.enabled */}}
{{- end }}{{/* define */}}

{{/*
Init container specs for sidecars with mode: "init".
Renders only preset/custom sidecars that have mode set to "init".
*/}}
{{- define "medvfs.initContainers" -}}
{{- if .Values.sidecars.enabled }}
{{- $presets := list "openslide" "clamav" "libvips" "samtools" }}
{{- range $name := $presets }}
{{- $sidecar := index $.Values.sidecars $name }}
{{- if and $sidecar.enabled (eq (default "batch" $sidecar.mode) "init") }}
- name: init-{{ $name }}
  image: {{ $sidecar.image | quote }}
  imagePullPolicy: IfNotPresent
  {{- with $sidecar.command }}
  command:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $sidecar.args }}
  args:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $sidecar.resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  volumeMounts:
    - name: fuse-mount
      mountPath: {{ $.Values.vfs.mountPoint }}
      readOnly: true
{{- end }}
{{- end }}
{{- range .Values.sidecars.custom }}
{{- if eq (default "batch" .mode) "init" }}
- name: init-{{ .name }}
  image: {{ .image | quote }}
  {{- with .command }}
  command:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .args }}
  args:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  volumeMounts:
    - name: fuse-mount
      mountPath: {{ $.Values.vfs.mountPoint }}
      readOnly: true
{{- end }}
{{- end }}
{{- end }}
{{- end }}

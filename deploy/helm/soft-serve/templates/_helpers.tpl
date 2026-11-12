{{/*
Expand the name of the chart.
*/}}
{{- define "soft-serve.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name. A release named after the chart keeps the short name.
*/}}
{{- define "soft-serve.fullname" -}}
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

{{- define "soft-serve.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "soft-serve.selectorLabels" -}}
app.kubernetes.io/name: {{ include "soft-serve.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "soft-serve.labels" -}}
helm.sh/chart: {{ include "soft-serve.chart" . }}
{{ include "soft-serve.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "soft-serve.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "soft-serve.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "soft-serve.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end }}

{{/*
The port of a Soft Serve listen address (":23231", "0.0.0.0:23231").
Call with (dict "name" "ssh" "addr" .Values.config.ssh.listen_addr).
Loopback addresses are rejected: neither a Service nor a probe can reach them.
*/}}
{{- define "soft-serve.listenPort" -}}
{{- $addr := toString .addr -}}
{{- $port := regexFind "[0-9]+$" $addr -}}
{{- if not $port -}}
{{- fail (printf "config.%s.listen_addr %q must end in a port number" .name $addr) -}}
{{- end -}}
{{- if or (hasPrefix "localhost" $addr) (hasPrefix "127." $addr) (hasPrefix "[::1]" $addr) -}}
{{- fail (printf "config.%s.listen_addr %q is a loopback address, which the Service and the probes cannot reach; use \":%s\"" .name $addr $port) -}}
{{- end -}}
{{- $port -}}
{{- end }}

{{/*
Whether the OpenShell client TLS secret is mounted.
*/}}
{{- define "soft-serve.openshellTLSMounted" -}}
{{- $os := .Values.config.openshell | default dict -}}
{{- if and $os.enabled .Values.openshell.clientTLS.secretName -}}
true
{{- end -}}
{{- end }}

{{/*
Whether the OpenShell gateway token secret is mounted.
*/}}
{{- define "soft-serve.openshellTokenMounted" -}}
{{- $os := .Values.config.openshell | default dict -}}
{{- $tok := .Values.openshell.gatewayToken | default dict -}}
{{- if and $os.enabled $tok.secretName -}}
true
{{- end -}}
{{- end }}

{{/*
The path of the mounted OpenShell gateway token file.
*/}}
{{- define "soft-serve.openshellTokenPath" -}}
{{- $tok := .Values.openshell.gatewayToken -}}
{{- printf "%s/%s" ($tok.mountPath | trimSuffix "/") (required "openshell.gatewayToken.key is required when openshell.gatewayToken.secretName is set" $tok.key) -}}
{{- end }}

{{/*
The Soft Serve config file: .Values.config, plus
  - initial_admin_keys merged with .Values.initialAdminKeys;
  - when OpenShell is enabled and the client TLS secret is mounted, any empty
    openshell.tls_* path pointed at that mount;
  - when OpenShell is enabled and the gateway token secret is mounted, an
    empty openshell.token_file pointed at the mounted key.
Refuses to render secret material or an OpenShell section Soft Serve itself
would reject at startup.
*/}}
{{- define "soft-serve.config" -}}
{{- $cfg := deepCopy .Values.config -}}

{{- $keys := concat (.Values.initialAdminKeys | default list) (get $cfg "initial_admin_keys" | default list) | uniq -}}
{{- range $keys -}}
{{- if contains "PRIVATE KEY" (toString .) -}}
{{- fail "initialAdminKeys must contain public keys only; a private key was found" -}}
{{- end -}}
{{- end -}}
{{- $_ := set $cfg "initial_admin_keys" $keys -}}

{{- $os := get $cfg "openshell" | default dict -}}
{{- if get $os "token" -}}
{{- fail "config.openshell.token must not be set: the config is rendered into a ConfigMap. Put the token in a Secret and set openshell.gatewayToken.secretName instead; the chart mounts it and sets config.openshell.token_file." -}}
{{- end -}}
{{- if get $os "enabled" -}}
{{- if not (get $os "gateway_address") -}}
{{- fail "config.openshell.gateway_address is required when config.openshell.enabled is true" -}}
{{- end -}}
{{- if not (get $os "image") -}}
{{- fail "config.openshell.image (the default sandbox image) is required when config.openshell.enabled is true" -}}
{{- end -}}
{{- if include "soft-serve.openshellTLSMounted" . -}}
{{- $dir := .Values.openshell.clientTLS.mountPath | trimSuffix "/" -}}
{{- range $field, $file := dict "tls_ca" "ca.crt" "tls_cert" "tls.crt" "tls_key" "tls.key" -}}
{{- if not (get $os $field) -}}
{{- $_ := set $os $field (printf "%s/%s" $dir $file) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if and (include "soft-serve.openshellTokenMounted" .) (not (get $os "token_file")) -}}
{{- $_ := set $os "token_file" (include "soft-serve.openshellTokenPath" .) -}}
{{- end -}}
{{- if ne (empty (get $os "tls_cert")) (empty (get $os "tls_key")) -}}
{{- fail "config.openshell.tls_cert and config.openshell.tls_key must be set together" -}}
{{- end -}}
{{- $_ := set $cfg "openshell" $os -}}
{{- end -}}

{{- toYaml $cfg -}}
{{- end }}

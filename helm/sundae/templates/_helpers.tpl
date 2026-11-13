{{/*
Expand the name of the chart.
*/}}
{{- define "sundae.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name. A release named after the chart keeps the short name.
*/}}
{{- define "sundae.fullname" -}}
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

{{- define "sundae.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "sundae.selectorLabels" -}}
app.kubernetes.io/name: {{ include "sundae.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "sundae.labels" -}}
helm.sh/chart: {{ include "sundae.chart" . }}
{{ include "sundae.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "sundae.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "sundae.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "sundae.image" -}}
{{- if not .Values.image.repository -}}
{{- fail "image.repository is required: the registry path of your Sundae image, e.g. ghcr.io/<owner>/sundae" -}}
{{- end -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end }}

{{/*
The port of a Sundae listen address (":23231", "0.0.0.0:23231").
Call with (dict "name" "ssh" "addr" .Values.config.ssh.listen_addr).
Loopback addresses are rejected: neither a Service nor a probe can reach them.
*/}}
{{- define "sundae.listenPort" -}}
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
{{- define "sundae.openshellTLSMounted" -}}
{{- $os := .Values.config.openshell | default dict -}}
{{- if and $os.enabled .Values.openshell.clientTLS.secretName -}}
true
{{- end -}}
{{- end }}

{{/*
Whether the OpenShell gateway token secret is mounted.
*/}}
{{- define "sundae.openshellTokenMounted" -}}
{{- $os := .Values.config.openshell | default dict -}}
{{- $tok := .Values.openshell.gatewayToken | default dict -}}
{{- if and $os.enabled $tok.secretName -}}
true
{{- end -}}
{{- end }}

{{/*
The path of the mounted OpenShell gateway token file.
*/}}
{{- define "sundae.openshellTokenPath" -}}
{{- $tok := .Values.openshell.gatewayToken -}}
{{- printf "%s/%s" ($tok.mountPath | trimSuffix "/") (required "openshell.gatewayToken.key is required when openshell.gatewayToken.secretName is set" $tok.key) -}}
{{- end }}

{{/*
Whether the ntfy token secret is mounted.
*/}}
{{- define "sundae.ntfyTokenMounted" -}}
{{- $n := .Values.config.notifications | default dict -}}
{{- $ntfy := get $n "ntfy" | default dict -}}
{{- $tok := (.Values.notifications | default dict).ntfyToken | default dict -}}
{{- if and (get $ntfy "enabled") $tok.secretName -}}
true
{{- end -}}
{{- end }}

{{/*
The path of the mounted ntfy token file.
*/}}
{{- define "sundae.ntfyTokenPath" -}}
{{- $tok := .Values.notifications.ntfyToken -}}
{{- printf "%s/%s" ($tok.mountPath | trimSuffix "/") (required "notifications.ntfyToken.key is required when notifications.ntfyToken.secretName is set" $tok.key) -}}
{{- end }}

{{/*
Whether the S3 snapshot credentials secret is mounted.
*/}}
{{- define "sundae.s3CredentialsMounted" -}}
{{- $snap := .Values.config.snapshots | default dict -}}
{{- $s3 := get $snap "s3" | default dict -}}
{{- $creds := (.Values.snapshots | default dict).s3Credentials | default dict -}}
{{- if and (get $s3 "enabled") $creds.secretName -}}
true
{{- end -}}
{{- end }}

{{/*
The path config.snapshots.s3.credentials_file points at: the key's file, or
the mount directory when no key is set.
*/}}
{{- define "sundae.s3CredentialsPath" -}}
{{- $creds := .Values.snapshots.s3Credentials -}}
{{- $dir := $creds.mountPath | trimSuffix "/" -}}
{{- if $creds.key -}}
{{- printf "%s/%s" $dir $creds.key -}}
{{- else -}}
{{- $dir -}}
{{- end -}}
{{- end }}

{{/*
The Sundae config file: .Values.config, plus
  - initial_admin_keys merged with .Values.initialAdminKeys;
  - when OpenShell is enabled and the client TLS secret is mounted, any empty
    openshell.tls_* path pointed at that mount;
  - when OpenShell is enabled and the gateway token secret is mounted, an
    empty openshell.token_file pointed at the mounted key;
  - when ntfy notifications are enabled and the ntfy token secret is mounted,
    an empty notifications.ntfy.token_file pointed at the mounted key;
  - when S3 snapshots are enabled and their credentials secret is mounted,
    an empty snapshots.s3.credentials_file pointed at it.
Refuses to render secret material or an OpenShell section Sundae itself
would reject at startup.
*/}}
{{- define "sundae.config" -}}
{{- $cfg := deepCopy .Values.config -}}

{{- $keys := concat (.Values.initialAdminKeys | default list) (get $cfg "initial_admin_keys" | default list) | uniq -}}
{{- range $keys -}}
{{- if contains "PRIVATE KEY" (toString .) -}}
{{- fail "initialAdminKeys must contain public keys only; a private key was found" -}}
{{- end -}}
{{- end -}}
{{- with .Values.initialAdminKeysSecret -}}
{{- if .name -}}
{{- $keys = append $keys (printf "%s/%s" (.mountPath | trimSuffix "/") (required "initialAdminKeysSecret.key is required when initialAdminKeysSecret.name is set" .key)) -}}
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
{{- if include "sundae.openshellTLSMounted" . -}}
{{- $dir := .Values.openshell.clientTLS.mountPath | trimSuffix "/" -}}
{{- range $field, $file := dict "tls_ca" "ca.crt" "tls_cert" "tls.crt" "tls_key" "tls.key" -}}
{{- if not (get $os $field) -}}
{{- $_ := set $os $field (printf "%s/%s" $dir $file) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if not (get $os "clone_url") -}}
{{- $_ := set $os "clone_url" (printf "http://%s-http.%s.svc.cluster.local:%v" (include "sundae.fullname" .) .Release.Namespace .Values.service.http.port) -}}
{{- end -}}
{{- if and (include "sundae.openshellTokenMounted" .) (not (get $os "token_file")) -}}
{{- $_ := set $os "token_file" (include "sundae.openshellTokenPath" .) -}}
{{- end -}}
{{- if ne (empty (get $os "tls_cert")) (empty (get $os "tls_key")) -}}
{{- fail "config.openshell.tls_cert and config.openshell.tls_key must be set together" -}}
{{- end -}}
{{- $_ := set $cfg "openshell" $os -}}
{{- end -}}

{{- $notif := get $cfg "notifications" | default dict -}}
{{- if hasKey $notif "true" -}}
{{- fail "config.notifications has a key `true`: Helm reads a bare `on:` as true. Quote the key: \"on\": [blocked, done, error]" -}}
{{- end -}}
{{- $ntfy := get $notif "ntfy" | default dict -}}
{{- if get $ntfy "token" -}}
{{- fail "config.notifications.ntfy.token must not be set: the config is rendered into a ConfigMap. Put the token in a Secret and set notifications.ntfyToken.secretName instead; the chart mounts it and sets config.notifications.ntfy.token_file." -}}
{{- end -}}
{{- if get $ntfy "enabled" -}}
{{- if not (get $ntfy "url") -}}
{{- fail "config.notifications.ntfy.url (the ntfy server's base URL) is required when config.notifications.ntfy.enabled is true" -}}
{{- end -}}
{{- if not (get $ntfy "topic") -}}
{{- fail "config.notifications.ntfy.topic is required when config.notifications.ntfy.enabled is true" -}}
{{- end -}}
{{- if and (include "sundae.ntfyTokenMounted" .) (not (get $ntfy "token_file")) -}}
{{- $_ := set $ntfy "token_file" (include "sundae.ntfyTokenPath" .) -}}
{{- end -}}
{{- $_ := set $notif "ntfy" $ntfy -}}
{{- $_ := set $cfg "notifications" $notif -}}
{{- end -}}

{{- $snap := get $cfg "snapshots" | default dict -}}
{{- $s3 := get $snap "s3" | default dict -}}
{{- if hasKey $s3 "true" -}}
{{- fail "config.snapshots.s3 has a key `true`: Helm reads a bare `on:` as true. Quote the key: \"on\": [end]" -}}
{{- end -}}
{{- range $k := list "access_key_id" "secret_access_key" "aws_access_key_id" "aws_secret_access_key" -}}
{{- if hasKey $s3 $k -}}
{{- fail (printf "config.snapshots.s3.%s must not be set: the config is rendered into a ConfigMap. Put the keys in a Secret and set snapshots.s3Credentials.secretName; the chart mounts it and sets config.snapshots.s3.credentials_file." $k) -}}
{{- end -}}
{{- end -}}
{{- if get $s3 "enabled" -}}
{{- if not (get $s3 "bucket") -}}
{{- fail "config.snapshots.s3.bucket is required when config.snapshots.s3.enabled is true" -}}
{{- end -}}
{{- if and (include "sundae.s3CredentialsMounted" .) (not (get $s3 "credentials_file")) -}}
{{- $_ := set $s3 "credentials_file" (include "sundae.s3CredentialsPath" .) -}}
{{- end -}}
{{- if not (get $s3 "credentials_file") -}}
{{- fail "config.snapshots.s3.credentials_file is required when config.snapshots.s3.enabled is true: set snapshots.s3Credentials.secretName to mount the keys from a Secret" -}}
{{- end -}}
{{- $_ := set $snap "s3" $s3 -}}
{{- $_ := set $cfg "snapshots" $snap -}}
{{- end -}}

{{- toYaml $cfg -}}
{{- end }}

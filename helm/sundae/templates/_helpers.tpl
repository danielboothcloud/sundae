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
Whether this release installs the OpenShell gateway (openshell.install).
*/}}
{{- define "sundae.openshellEmbedded" -}}
{{- if (.Values.openshell | default dict).install -}}
true
{{- end -}}
{{- end }}

{{/*
Whether OpenShell sessions are on: config.openshell.enabled, or implied by
openshell.install.
*/}}
{{- define "sundae.openshellEnabled" -}}
{{- $os := .Values.config.openshell | default dict -}}
{{- if or $os.enabled (include "sundae.openshellEmbedded" .) -}}
true
{{- end -}}
{{- end }}

{{/*
Whether any sessions use the pod runtime: config.sessions.runtime is "pod",
or any config.sessions.profiles.<name>.runtime is.
*/}}
{{- define "sundae.podRuntime" -}}
{{- $s := .Values.config.sessions | default dict -}}
{{- $on := eq (toString (get $s "runtime")) "pod" -}}
{{- range $name, $p := (get $s "profiles" | default dict) -}}
{{- if eq (toString (get ($p | default dict) "runtime")) "pod" -}}
{{- $on = true -}}
{{- end -}}
{{- end -}}
{{- if $on -}}
true
{{- end -}}
{{- end }}

{{/*
The namespace of the pod runtime's sandbox Pods and PVCs: config.pod.namespace,
or the release namespace.
*/}}
{{- define "sundae.podNamespace" -}}
{{- (.Values.config.pod | default dict).namespace | default .Release.Namespace -}}
{{- end }}

{{/*
The default clone URL: this release's in-cluster HTTP Service.
*/}}
{{- define "sundae.defaultCloneURL" -}}
{{- printf "http://%s-http.%s.svc.cluster.local:%v" (include "sundae.fullname" .) .Release.Namespace .Values.service.http.port -}}
{{- end }}

{{/*
The embedded gateway's full name, computed as the OpenShell chart does
(openshell.fullname), with the chart aliased "openshell" and its default
nameOverride "openshell". Its Service has this name.
*/}}
{{- define "sundae.openshellFullname" -}}
{{- $v := .Values.openshell | default dict -}}
{{- if $v.fullnameOverride -}}
{{- $v.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default "openshell" $v.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The embedded gateway's in-cluster gRPC address: its Service in the release
namespace, https unless the OpenShell chart's server.disableTls is set.
*/}}
{{- define "sundae.openshellGatewayAddress" -}}
{{- $v := .Values.openshell | default dict -}}
{{- $server := $v.server | default dict -}}
{{- $scheme := ternary "http" "https" (default false $server.disableTls) -}}
{{- $port := ($v.service | default dict).port | default 8080 -}}
{{- printf "%s://%s.%s.svc.cluster.local:%v" $scheme (include "sundae.openshellFullname" .) .Release.Namespace $port -}}
{{- end }}

{{/*
The Secret with the gateway client certificate to mount: with the embedded
gateway, the one its certgen hook (or cert-manager) creates,
openshell.server.tls.clientTlsSecretName, unless TLS is off; otherwise
openshell.clientTLS.secretName.
*/}}
{{- define "sundae.openshellClientTLSSecret" -}}
{{- if include "sundae.openshellEmbedded" . -}}
{{- $server := .Values.openshell.server | default dict -}}
{{- if not $server.disableTls -}}
{{- ($server.tls | default dict).clientTlsSecretName | default "openshell-client-tls" -}}
{{- end -}}
{{- else -}}
{{- .Values.openshell.clientTLS.secretName -}}
{{- end -}}
{{- end }}

{{/*
Whether the OpenShell client TLS secret is mounted.
*/}}
{{- define "sundae.openshellTLSMounted" -}}
{{- if and (include "sundae.openshellEnabled" .) (include "sundae.openshellClientTLSSecret" .) -}}
true
{{- end -}}
{{- end }}

{{/*
Whether the OpenShell gateway token secret is mounted.
*/}}
{{- define "sundae.openshellTokenMounted" -}}
{{- $tok := .Values.openshell.gatewayToken | default dict -}}
{{- if and (include "sundae.openshellEnabled" .) $tok.secretName -}}
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
The name of the StorageClass podRuntime.storageClass creates.
*/}}
{{- define "sundae.podStorageClassName" -}}
{{- .Values.podRuntime.storageClass.name | default (printf "%s-sandbox" (include "sundae.fullname" .)) -}}
{{- end }}

{{/*
The claim name of a podRuntime.sharedVolumes entry: claimName, or
<fullname>-<name> for one the chart creates.
*/}}
{{- define "sundae.sharedVolumeClaim" -}}
{{- $v := .v -}}
{{- if $v.claimName -}}
{{- $v.claimName -}}
{{- else if $v.create -}}
{{- printf "%s-%s" (include "sundae.fullname" .root) $v.name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- fail (printf "podRuntime.sharedVolumes[%s].claimName is required unless create is true" $v.name) -}}
{{- end -}}
{{- end }}

{{/*
The Sundae config file: .Values.config, plus
  - initial_admin_keys merged with .Values.initialAdminKeys;
  - with openshell.install, openshell.enabled set and an empty
    openshell.gateway_address pointed at the embedded gateway's Service;
  - when OpenShell is enabled and the client TLS secret is mounted, any empty
    openshell.tls_* path pointed at that mount;
  - when OpenShell is enabled and the gateway token secret is mounted, an
    empty openshell.token_file pointed at the mounted key;
  - when ntfy notifications are enabled and the ntfy token secret is mounted,
    an empty notifications.ntfy.token_file pointed at the mounted key;
  - with podRuntime.storageClass.create, an empty pod.storage_class set to
    that StorageClass;
  - podRuntime.sharedVolumes (with their links) appended to
    pod.shared_volumes;
  - when any sessions use the pod runtime, an empty pod.namespace set to the
    release namespace and an empty openshell.clone_url (which pod sandboxes
    clone over too) set to this release's HTTP Service, with or without
    OpenShell.
Refuses to render secret material, removed settings, or an OpenShell section
Sundae itself would reject at startup.
*/}}
{{- define "sundae.config" -}}
{{- $cfg := deepCopy .Values.config -}}

{{- if hasKey $cfg "snapshots" -}}
{{- fail "config.snapshots was removed: S3 snapshots no longer exist (session suspend keeps a session's storage, session delete deletes it). Delete it from your values." -}}
{{- end -}}
{{- if hasKey .Values "snapshots" -}}
{{- fail "snapshots (snapshots.s3Credentials) was removed with S3 snapshots. Delete it from your values." -}}
{{- end -}}
{{- if hasKey ((get $cfg "sessions" | default dict).suspend | default dict) "fallback" -}}
{{- fail "config.sessions.suspend.fallback was removed with S3 snapshots: a session the runtime cannot suspend keeps running. Delete it from your values." -}}
{{- end -}}

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
{{- if include "sundae.openshellEnabled" . -}}
{{- $_ := set $os "enabled" true -}}
{{- if and (include "sundae.openshellEmbedded" .) (not (get $os "gateway_address")) -}}
{{- $_ := set $os "gateway_address" (include "sundae.openshellGatewayAddress" .) -}}
{{- end -}}
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
{{- $_ := set $os "clone_url" (include "sundae.defaultCloneURL" .) -}}
{{- end -}}
{{- if and (include "sundae.openshellTokenMounted" .) (not (get $os "token_file")) -}}
{{- $_ := set $os "token_file" (include "sundae.openshellTokenPath" .) -}}
{{- end -}}
{{- if ne (empty (get $os "tls_cert")) (empty (get $os "tls_key")) -}}
{{- fail "config.openshell.tls_cert and config.openshell.tls_key must be set together" -}}
{{- end -}}
{{- $_ := set $cfg "openshell" $os -}}
{{- end -}}

{{- $pod := get $cfg "pod" | default dict -}}
{{- if and .Values.podRuntime.storageClass.create (not (get $pod "storage_class")) -}}
{{- $_ := set $pod "storage_class" (include "sundae.podStorageClassName" .) -}}
{{- end -}}
{{- $shared := get $pod "shared_volumes" | default list -}}
{{- range $v := .Values.podRuntime.sharedVolumes -}}
{{- $name := required "podRuntime.sharedVolumes[].name is required" $v.name -}}
{{- range $shared -}}
{{- if eq (toString .name) $name -}}
{{- fail (printf "podRuntime.sharedVolumes %q is also in config.pod.shared_volumes: name it once" $name) -}}
{{- end -}}
{{- end -}}
{{- $sv := dict "name" $name "claim_name" (include "sundae.sharedVolumeClaim" (dict "root" $ "v" $v)) "mount_path" (required (printf "podRuntime.sharedVolumes[%s].mountPath is required" $name) $v.mountPath) -}}
{{- if $v.readOnly -}}
{{- $_ := set $sv "read_only" true -}}
{{- end -}}
{{- with $v.subPath -}}
{{- $_ := set $sv "sub_path" . -}}
{{- end -}}
{{- $links := list -}}
{{- range $l := $v.links | default list -}}
{{- $links = append $links (dict "path" (required (printf "podRuntime.sharedVolumes[%s].links[].path is required" $name) $l.path) "target" (required (printf "podRuntime.sharedVolumes[%s].links[].target is required" $name) $l.target)) -}}
{{- end -}}
{{- if $links -}}
{{- $_ := set $sv "links" $links -}}
{{- end -}}
{{- $shared = append $shared $sv -}}
{{- end -}}
{{- if $shared -}}
{{- $_ := set $pod "shared_volumes" $shared -}}
{{- end -}}
{{- $_ := set $cfg "pod" $pod -}}

{{- if include "sundae.podRuntime" . -}}
{{- $pod := get $cfg "pod" | default dict -}}
{{- if not (get $pod "namespace") -}}
{{- $_ := set $pod "namespace" .Release.Namespace -}}
{{- end -}}
{{- $_ := set $cfg "pod" $pod -}}
{{- if not (get $os "clone_url") -}}
{{- $_ := set $os "clone_url" (include "sundae.defaultCloneURL" .) -}}
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

{{- toYaml $cfg -}}
{{- end }}

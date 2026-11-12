package config

import (
	"bytes"
	"text/template"
)

var configFileTmpl = template.Must(template.New("config").Parse(`# Soft Serve Server configurations

# The name of the server.
# This is the name that will be displayed in the UI.
name: "{{ .Name }}"

# Logging configuration.
log:
  # Log format to use. Valid values are "json", "logfmt", and "text".
  format: "{{ .Log.Format }}"
  # Time format for the log "timestamp" field.
  # Should be described in Golang's time format.
  time_format: "{{ .Log.TimeFormat }}"
  # Path to the log file. Leave empty to write to stderr.
  #path: "{{ .Log.Path }}"

# The SSH server configuration.
ssh:
  # Enable SSH.
  enabled: {{ .SSH.Enabled }}

  # The address on which the SSH server will listen.
  listen_addr: "{{ .SSH.ListenAddr }}"

  # The public URL of the SSH server.
  # This is the address that will be used to clone repositories.
  public_url: "{{ .SSH.PublicURL }}"

  # The path to the SSH server's private key.
  key_path: {{ .SSH.KeyPath }}

  # The path to the server's client private key. This key will be used to
  # authenticate the server to make git requests to ssh remotes.
  client_key_path: {{ .SSH.ClientKeyPath }}

  # The maximum number of seconds a connection can take.
  # A value of 0 means no timeout.
  max_timeout: {{ .SSH.MaxTimeout }}

  # The number of seconds a connection can be idle before it is closed.
  # A value of 0 means no timeout.
  idle_timeout: {{ .SSH.IdleTimeout }}

# The Git daemon configuration.
git:
  # Enable the Git daemon.
  enabled: {{ .Git.Enabled }}

  # The address on which the Git daemon will listen.
  listen_addr: "{{ .Git.ListenAddr }}"

  # The public URL of the Git daemon server.
  # This is the address that will be used to clone repositories.
  public_url: "{{ .Git.PublicURL }}"

  # The maximum number of seconds a connection can take.
  # A value of 0 means no timeout.
  max_timeout: {{ .Git.MaxTimeout }}

  # The number of seconds a connection can be idle before it is closed.
  idle_timeout: {{ .Git.IdleTimeout }}

  # The maximum number of concurrent connections.
  max_connections: {{ .Git.MaxConnections }}

# The HTTP server configuration.
http:
  # Enable the HTTP server.
  enabled: {{ .HTTP.Enabled }}

  # The address on which the HTTP server will listen.
  listen_addr: "{{ .HTTP.ListenAddr }}"

  # The path to the TLS private key.
  tls_key_path: {{ .HTTP.TLSKeyPath }}

  # The path to the TLS certificate.
  tls_cert_path: {{ .HTTP.TLSCertPath }}

  # The public URL of the HTTP server.
  # This is the address that will be used to clone repositories.
  # Make sure to use https:// if you are using TLS.
  public_url: "{{ .HTTP.PublicURL }}"

  # The cross-origin request security options
  cors:
    # The allowed cross-origin headers
    allowed_headers:
       - "Accept"
       - "Accept-Language"
       - "Content-Language"
       - "Content-Type"
       - "Origin"
       - "X-Requested-With"
       - "User-Agent"
       - "Authorization"
       - "Access-Control-Request-Method"
       - "Access-Control-Allow-Origin"
    # The allowed cross-origin URLs
    allowed_origins:
       - "{{ .HTTP.PublicURL }}" # always allowed
       # - "https://example.com"
    # The allowed cross-origin methods
    allowed_methods:
       - "GET"
       - "HEAD"
       - "POST"
       - "PUT"
       - "OPTIONS"

# The stats server configuration.
stats:
  # Enable the stats server.
  enabled: {{ .Stats.Enabled }}

  # The address on which the stats server will listen.
  listen_addr: "{{ .Stats.ListenAddr }}"

# The database configuration.
db:
  # The database driver to use.
  # Valid values are "sqlite" and "postgres".
  driver: "{{ .DB.Driver }}"
  # The database data source name.
  # This is driver specific and can be a file path or connection string.
  # Make sure foreign key support is enabled when using SQLite.
  data_source: "{{ .DB.DataSource }}"

# Git LFS configuration.
lfs:
  # Enable Git LFS.
  enabled: {{ .LFS.Enabled }}
  # Enable Git SSH transfer.
  ssh_enabled: {{ .LFS.SSHEnabled }}

# Cron job configuration
jobs:
  mirror_pull: "{{ .Jobs.MirrorPull }}"

# OpenShell agent sessions (ssh -t <server> session start <repo>).
# Each (user, repository) gets its own OpenShell sandbox with the repository
# cloned into /sandbox/<repo>.
openshell:
  # Enable the "session" SSH command.
  enabled: {{ .OpenShell.Enabled }}
  # The OpenShell gateway gRPC address. Use http:// for plaintext; https://
  # or no scheme uses TLS.
  gateway_address: "{{ .OpenShell.GatewayAddress }}"
  # The OpenShell workspace sandboxes are created in.
  workspace: "{{ .OpenShell.Workspace }}"
  # The default sandbox base image. It must provide git and jj.
  image: "{{ .OpenShell.Image }}"
  # Optional bearer token used to authenticate to the gateway. Prefer
  # token_file, which keeps the token out of this file. Set at most one.
  #token: ""
  # Path to a file holding the bearer token, e.g. mounted from a secret.
  # Trailing whitespace is trimmed. The file is read again for every session
  # command, so a token rotated in place is picked up without a restart.
  token_file: "{{ .OpenShell.TokenFile }}"
  # Client TLS files, e.g. mounted from the openshell-client-tls secret.
  tls_ca: "{{ .OpenShell.TLSCA }}"
  tls_cert: "{{ .OpenShell.TLSCert }}"
  tls_key: "{{ .OpenShell.TLSKey }}"
  # Skip verification of the gateway's TLS certificate.
  insecure: {{ .OpenShell.Insecure }}
  # The internal HTTP base URL sandboxes use to clone and push.
  # Defaults to http.public_url when empty.
  clone_url: "{{ .OpenShell.CloneURL }}"
  # Lifetime of the access token issued to each sandbox.
  token_ttl: "{{ .OpenShell.TokenTTL }}"
  # The command run in the sandbox when a user attaches.
  command: {{ printf "%q" .OpenShell.Command }}

# Forges: GitHub and GitLab accounts whose repositories Soft Serve discovers,
# imports and keeps in sync (ssh <server> forge --help). Sync never rewrites or
# deletes a Soft Serve branch: upstream refs are fetched into
# refs/remotes/<name>/heads/* and refs/remotes/<name>/tags/*, branches that are
# strictly behind are fast-forwarded and missing ones created. Pushing to a
# forge is always explicit (forge push). Changes take effect on restart.
#
#   name        unique DNS label; used in commands, ref namespaces and git config
#   kind        github or gitlab
#   url         web base URL; defaults to https://github.com or https://gitlab.com.
#               This host is trusted: a self-hosted forge on a private address
#               is allowed by declaring it here.
#   token_file  file holding the access token (never put a token in this file)
#   include     globs matched against "owner/repo"; empty includes everything.
#               "*" does not cross "/", "**" does. Case-insensitive.
#   exclude     globs removed from what include lets through
#   sync        scheduled sync interval, at least 1m (default 10m)
#   prefix      repository namespace: "owner/repo" is imported as
#               "<prefix>/owner/repo" (default: name)
{{- if .Forges }}
forges:
{{- range .Forges }}
  - name: {{ printf "%q" .Name }}
    kind: {{ printf "%q" .Kind }}
    url: {{ printf "%q" .URL }}
    token_file: {{ printf "%q" .TokenFile }}
    {{- if .Include }}
    include: [{{ range $i, $p := .Include }}{{ if $i }}, {{ end }}{{ printf "%q" $p }}{{ end }}]
    {{- end }}
    {{- if .Exclude }}
    exclude: [{{ range $i, $p := .Exclude }}{{ if $i }}, {{ end }}{{ printf "%q" $p }}{{ end }}]
    {{- end }}
    sync: "{{ .Sync }}"
    prefix: {{ printf "%q" .Prefix }}
{{- end }}
{{- else }}
#forges:
#  - name: github
#    kind: github
#    token_file: /etc/soft-serve/secrets/github-token
#    include: ["me/*"]
#    exclude: []
#    sync: 10m
#  - name: gitlab
#    kind: gitlab
#    url: https://gitlab.com
#    token_file: /etc/soft-serve/secrets/gitlab-token
{{- end }}

# Additional admin keys.
#initial_admin_keys:
#  - "ssh-rsa AAAAB3NzaC1yc2..."

# Name of a repository to create automatically on boot if it doesn't already
# exist. Empty disables this behavior.
#default_repo: ""
`))

func newConfigFile(cfg *Config) string {
	var b bytes.Buffer
	configFileTmpl.Execute(&b, cfg) //nolint: errcheck
	return b.String()
}

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
  # Lifetime of the git access token written into a sandbox. It is renewed
  # on every "session start" and "session attach"; an agent left running
  # longer than this without anyone attaching loses git access until then.
  token_ttl: "{{ .OpenShell.TokenTTL }}"
  # The program a session runs when no profile sets one, in
  # /sandbox/<repo>. It runs inside an osewa session, so it keeps running
  # after the SSH client disconnects.
  command: {{ printf "%q" .OpenShell.Command }}

# Sessions: what runs in the OpenShell sandboxes. Each user can have several
# named sessions per repository (session start REPO --name NAME), one sandbox
# each. Changes take effect on restart, and only for sandboxes created after.
sessions:
  osewa:
    # Pinned osewa Linux builds (osewa-linux-amd64.gz, osewa-linux-arm64.gz)
    # uploaded to sandboxes whose image lacks the pinned osewa.
    binary_dir: {{ printf "%q" .Sessions.Osewa.BinaryDir }}
    # Loopback port of the osewa server inside each sandbox.
    port: {{ .Sessions.Osewa.Port }}
  status:
    # How often Soft Serve runs "osewa local ls" in each running session's
    # sandbox to read its OSC 7501 status (shown by "session ls", sent to
    # notifications). At least 1s.
    poll_interval: "{{ .Sessions.Status.PollInterval }}"
  suspend:
    # Park (suspend) a session's sandbox once nobody has been attached to it
    # and it has not reported "working" for "after". Needs an OpenShell
    # compute driver that supports suspend; otherwise sandboxes keep running.
    # "session start" and "session attach" resume a parked session.
    enabled: {{ .Sessions.Suspend.Enabled }}
    after: "{{ .Sessions.Suspend.After }}"
    # What parking does where the gateway cannot suspend (the OpenShell 0.1.8
    # Kubernetes driver): "none" leaves the sandbox running; "snapshot"
    # saves /sandbox to S3 (snapshots.s3 must be enabled) and deletes the
    # sandbox. That stops the session's program; start and attach recreate
    # the sandbox, restore /sandbox and start the program again.
    fallback: {{ printf "%q" .Sessions.Suspend.Fallback }}
  signals:
    # How many signals (every session status change and lifecycle event,
    # shown by "session signals" and the TUI) to keep, across all users.
    # They are kept in memory only, so the feed is empty after a restart.
    # At most 100000.
    keep: {{ .Sessions.Signals.Keep }}
  # Named presets for "session start --profile NAME". Empty fields fall back
  # to openshell.image and openshell.command.
{{- if .Sessions.Profiles }}
  profiles:
{{- range $name, $p := .Sessions.Profiles }}
    {{ printf "%q" $name }}:
      image: {{ printf "%q" $p.Image }}
      command: {{ printf "%q" $p.Command }}
{{- end }}
{{- else }}
  #profiles:
  #  claude:
  #    image: ghcr.io/me/agent-claude:latest
  #    command: claude
{{- end }}
  # Profiles for sessions started without --profile, by repository name
  # glob. "*" does not cross "/", "**" does; case-insensitive. The most
  # specific matching pattern wins.
{{- if .Sessions.Repos }}
  repos:
{{- range $pattern, $d := .Sessions.Repos }}
    {{ printf "%q" $pattern }}:
      profile: {{ printf "%q" $d.Profile }}
{{- end }}
{{- else }}
  #repos:
  #  "infra/*":
  #    profile: claude
{{- end }}

# Projects: how new repositories are initialised (repo create --template NAME,
# and the TUI's new-repository form, n on Repositories). A template is an
# existing Soft Serve repository whose tree, at ref and under subdir, becomes
# the new repository's single initial commit, authored by its creator; the
# template's history is not copied, and file contents are copied unchanged.
# Creating from a template needs read access to the template repository.
# Changes take effect on restart.
#
#   repo         the template repository (required)
#   ref          branch, tag or commit hash; empty is the repository's HEAD
#   subdir       directory whose contents become the new repository's root
#   description  shown where the template is offered
#   profile      a sessions.profiles entry that becomes the new repository's
#                default session profile unless one is given (repo profile)
projects:
{{- if .Projects.Templates }}
  templates:
{{- range $name, $t := .Projects.Templates }}
    {{ printf "%q" $name }}:
      repo: {{ printf "%q" $t.Repo }}
      ref: {{ printf "%q" $t.Ref }}
      subdir: {{ printf "%q" $t.Subdir }}
      description: {{ printf "%q" $t.Description }}
      profile: {{ printf "%q" $t.Profile }}
{{- end }}
{{- else }}
  templates: {}
  #  go-service:
  #    repo: templates/go-service
  #    ref: main
  #    subdir: ""
  #    description: Go service with a Containerfile and CI
  #    profile: claude
{{- end }}

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

# Notifications: agent session status changes, pushed to a phone through ntfy
# (https://ntfy.sh or a self-hosted server). Off by default: while
# ntfy.enabled is false nothing in this section is checked. Changes take
# effect on restart.
notifications:
  # What notifies. Agent states (OSC 7501): blocked, done, error, working,
  # idle. Also session_failed (starting or ending a session failed) and
  # forge_failed (a forge import, sync, push or listing failed; a repeat of
  # the same failure only notifies again after a success).
  # Quote the key: YAML 1.1 tools such as Helm read a bare on: as true.
  "on": [{{ range $i, $t := .Notifications.On }}{{ if $i }}, {{ end }}{{ printf "%q" $t }}{{ end }}]
  # Per-session rate limit. After a notification, changes of the same
  # session are held for this long and only the latest is sent, unless it
  # repeats what was already sent. "0s" sends every change at once.
  debounce: "{{ .Notifications.Debounce }}"
  ntfy:
    # Publish notifications to ntfy.
    enabled: {{ .Notifications.Ntfy.Enabled }}
    # The server's base URL, without the topic, e.g. https://ntfy.sh.
    url: {{ printf "%q" .Notifications.Ntfy.URL }}
    # The topic to publish to: 1 to 64 letters, digits, '-' or '_'. On a
    # public server anyone who knows the topic can read it; use an
    # unguessable name or an access-controlled topic with token_file.
    topic: {{ printf "%q" .Notifications.Ntfy.Topic }}
    # Optional file holding an ntfy access token (tk_...), sent as a bearer
    # token. Read again for every message, so a rotated token is picked up
    # without a restart. Never put the token in this file.
    token_file: {{ printf "%q" .Notifications.Ntfy.TokenFile }}
    # Optional URL opened when the notification is tapped. Placeholders,
    # replaced by URL-escaped values: {repo}, {session}, {sandbox}, {user}.
    # Left off a notification that has no value for a placeholder it uses.
    click_url: {{ printf "%q" .Notifications.Ntfy.ClickURL }}

# Snapshots of a session's /sandbox (checkouts and the sandbox user's home) in
# an S3 bucket: AWS S3 or an S3-compatible store (MinIO, Ceph RGW). Off by
# default: while s3.enabled is false nothing in this section is checked.
# Each session has one object, <prefix>/<user-id>/<sandbox>.tar.gz, replaced
# by every snapshot. The sandbox uploads and downloads it itself through
# presigned URLs Soft Serve signs, so the S3 keys never enter a sandbox, but
# the sandbox network policy must allow the endpoint. Changes take effect on
# restart.
snapshots:
  s3:
    enabled: {{ .Snapshots.S3.Enabled }}
    # Base URL of an S3-compatible store; empty for AWS S3.
    endpoint: {{ printf "%q" .Snapshots.S3.Endpoint }}
    # The region requests are signed for. Required for AWS; defaults to
    # us-east-1 with an endpoint.
    region: {{ printf "%q" .Snapshots.S3.Region }}
    # The bucket, which must exist.
    bucket: {{ printf "%q" .Snapshots.S3.Bucket }}
    prefix: {{ printf "%q" .Snapshots.S3.Prefix }}
    # File holding the access key, AWS shared-credentials format:
    #   [default]
    #   aws_access_key_id = ...
    #   aws_secret_access_key = ...
    # Read again for every operation. Never put the keys in this file.
    credentials_file: {{ printf "%q" .Snapshots.S3.CredentialsFile }}
    # Server-side encryption: "AES256", "aws:kms" (the bucket's default KMS
    # key) or "" (none requested; the bucket's default applies).
    sse: {{ printf "%q" .Snapshots.S3.SSE }}
    # Address the bucket as <endpoint>/<bucket>; most MinIO and RGW setups
    # need this.
    path_style: {{ .Snapshots.S3.PathStyle }}
    # When to snapshot: "end" (before "session end" deletes the sandbox; a
    # failed snapshot stops the end unless --force) and "park" (before each
    # memory-retaining park, as a backup). "session start" restores the
    # snapshot into a newly created sandbox either way.
    # Quote the key: YAML 1.1 tools such as Helm read a bare on: as true.
    "on": [{{ range $i, $t := .Snapshots.S3.On }}{{ if $i }}, {{ end }}{{ printf "%q" $t }}{{ end }}]

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

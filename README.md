# Sundae

Sundae is a self-hostable Git server for the command line, with OpenShell agent
sessions, forges, notifications and S3 snapshots.

Sundae is a fork of [Soft Serve](https://github.com/charmbracelet/soft-serve)
by [Charm](https://charm.sh), released under the same MIT license. The Git
server, the SSH TUI and most of the repository management come from Soft Serve;
see [License](#license).

- Easy to navigate TUI available over SSH
- Clone repos over SSH, HTTP, or Git protocol
- Git LFS support with both HTTP and SSH backends
- Manage repos with SSH
- Create repos on demand with SSH or `git push`
- Browse repos, files and commits with SSH-accessible UI
- Print files over SSH with or without syntax highlighting and line numbers
- Easy access control
  - SSH authentication using public keys
  - Allow/disallow anonymous access
  - Add collaborators with SSH public keys
  - Repos can be public or private
  - User access tokens

## Trying it out

Once a server is running (see [Setting up a server](#setting-up-a-server))
and has a repository named `sundae` (push this one to it, for example), try
some of the following commands against it:

```bash
# Jump directly to a repo in the TUI
ssh localhost -p 23231 -t sundae

# Print out a directory tree for a repo
ssh localhost -p 23231 repo tree sundae

# Print a specific file
ssh localhost -p 23231 repo blob sundae cmd/sundae/main.go

# Print a file with syntax highlighting and line numbers
ssh localhost -p 23231 repo blob sundae cmd/sundae/main.go -c -l
```

Or you can use Sundae to browse local repositories using `sundae browse
[directory]` or running `sundae` within a Git repository.

## Installation

Sundae is a single binary called `sundae`. No prebuilt binaries, packages or
images are published; build it from source (Go 1.26.6 or later):

```bash
git clone <this repository> sundae
cd sundae
go build -o sundae ./cmd/sundae
```

To run it in a container or on Kubernetes, build the image from the
[`Containerfile`](Containerfile) and deploy it with the Helm chart in
[`helm/sundae`](helm/sundae/README.md), which can also install the OpenShell
gateway and Agent Sandbox for agent sessions. [docker.md](docker.md) shows how to run the
image with Docker or Podman.

## Setting up a server

Make sure `git` is installed, then run `sundae serve`. That’s it.

This will create a `data` directory that will store all the repos, ssh keys,
and database.

By default, program configuration is stored within the `data` directory. But,
this can be overridden by setting a custom path to a config file with `SUNDAE_CONFIG_LOCATION`
that is pre-created. If a config file pointed to by `SUNDAE_CONFIG_LOCATION`,
the default location within the `data` dir is used for generating a default config.

To change the default data path use `SUNDAE_DATA_PATH` environment variable.

```sh
SUNDAE_DATA_PATH=/var/lib/sundae sundae serve
```

When you run Sundae for the first time, make sure you have the
`SUNDAE_INITIAL_ADMIN_KEYS` environment variable is set to your ssh
authorized key. Any added key to this variable will be treated as admin with
full privileges.

Using this environment variable, Sundae will create a new `admin` user that
has full privileges. You can rename and change the user settings later.

If you'd like a repository to exist the moment the server finishes booting,
with no human logging in to run `repo create`, set `SUNDAE_DEFAULT_REPO`
to a repository name. This is useful for GitOps tools like ArgoCD that need a
git remote to point at as part of their own bootstrap (e.g. from a Helm chart
or Terraform apply):

```sh
SUNDAE_INITIAL_ADMIN_KEYS="$(cat ~/.ssh/id_ed25519.pub)" \
SUNDAE_DEFAULT_REPO=gitops \
  sundae serve
```

The repository is created empty and public, exactly as if a human had run
`repo create gitops` with no flags; only its *existence* is guaranteed on
boot, not its reachability, which is still governed entirely by the
`anon-access`/`allow-keyless` settings described below. Booting again against
the same data directory is a no-op if the repository already exists.

Check out [Systemd](systemd.md) on how to run Sundae as a service using
Systemd.

### Server Configuration

Once you start the server for the first time, the settings will be in
`config.yaml` under your data directory. The default `config.yaml` is
self-explanatory and will look like this:

```yaml
# Sundae Server configurations

# The name of the server.
# This is the name that will be displayed in the UI.
name: "Sundae"

# Log format to use. Valid values are "json", "logfmt", and "text".
log_format: "text"

# The SSH server configuration.
ssh:
  # The address on which the SSH server will listen.
  listen_addr: ":23231"

  # The public URL of the SSH server.
  # This is the address that will be used to clone repositories.
  public_url: "ssh://localhost:23231"

  # The path to the SSH server's private key.
  key_path: "ssh/sundae_host"

  # The path to the SSH server's client private key.
  # This key will be used to authenticate the server to make git requests to
  # ssh remotes.
  client_key_path: "ssh/sundae_client"

  # The maximum number of seconds a connection can take.
  # A value of 0 means no timeout.
  max_timeout: 0

  # The number of seconds a connection can be idle before it is closed.
  idle_timeout: 120

# The Git daemon configuration.
git:
  # The address on which the Git daemon will listen.
  listen_addr: ":9418"

  # The maximum number of seconds a connection can take.
  # A value of 0 means no timeout.
  max_timeout: 0

  # The number of seconds a connection can be idle before it is closed.
  idle_timeout: 3

  # The maximum number of concurrent connections.
  max_connections: 32

# The HTTP server configuration.
http:
  # The address on which the HTTP server will listen.
  listen_addr: ":23232"

  # The path to the TLS private key.
  tls_key_path: ""

  # The path to the TLS certificate.
  tls_cert_path: ""

  # The public URL of the HTTP server.
  # This is the address that will be used to clone repositories.
  # Make sure to use https:// if you are using TLS.
  public_url: "http://localhost:23232"

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
      - "http://localhost:23232" # always allowed
      # - "https://example.com"

    # The allowed cross-origin methods
    allowed_methods:
      - "GET"
      - "HEAD"
      - "POST"
      - "PUT"
      - "OPTIONS"

# The database configuration.
db:
  # The database driver to use.
  # Valid values are "sqlite" and "postgres".
  driver: "sqlite"
  # The database data source name.
  # This is driver specific and can be a file path or connection string.
  # Make sure foreign key support is enabled when using SQLite.
  data_source: "sundae.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"

# Git LFS configuration.
lfs:
  # Enable Git LFS.
  enabled: true
  # Enable Git SSH transfer.
  ssh_enabled: false

# Cron job configuration
jobs:
  mirror_pull: "@every 10m"

# GitHub and GitLab accounts to import from and sync with. See "Forges".
#forges:
#  - name: github
#    kind: github
#    token_file: /etc/sundae/secrets/github-token
#    ssh_key_file: /etc/sundae/secrets/github-ssh/ssh-privatekey  # git over SSH (optional)
#    include: ["me/*"]
#    sync: 10m
#    git_identity:              # commits in sessions of this forge's repos
#      name: Jane Doe
#      email: 12345+jane@users.noreply.github.com

# Agent session status, parking, profiles and git identity. See "OpenShell
# Agent Sessions".
#sessions:
#  status:
#    poll_interval: 10s
#  suspend:
#    enabled: true
#    after: 30m
#  signals:
#    keep: 1000
#  profiles:
#    claude:
#      image: ghcr.io/example/agent-claude:latest
#      command: claude
#  repos:
#    "infra/*":
#      profile: claude
#  git_identity:                # commits in sessions of other repositories
#    name: Jane Doe
#    email: jane@example.com

# Templates for new repositories. See "Project templates and profiles".
#projects:
#  templates:
#    go-service:
#      repo: templates/go-service
#      ref: main
#      description: Go service with a Containerfile and CI
#      profile: claude

# Snapshots of a session's /sandbox in S3, off by default. See "S3 snapshots".
#snapshots:
#  s3:
#    enabled: true
#    endpoint: ""            # empty for AWS; e.g. http://rook-ceph-rgw-store.rook-ceph.svc:80
#    region: eu-west-2
#    bucket: agent-snapshots
#    credentials_file: /etc/sundae/secrets/s3-credentials
#    "on": [end]

# Notifications about agent sessions, through ntfy. See "Notifications".
#notifications:
#  "on": [blocked, done, error]
#  debounce: 30s
#  ntfy:
#    enabled: true
#    url: https://ntfy.sh
#    topic: my-unguessable-topic
#    token_file: /etc/sundae/secrets/ntfy-token

# The stats server configuration.
stats:
  # The address on which the stats server will listen.
  listen_addr: ":23233"
# Additional admin keys.
#initial_admin_keys:
#  - "ssh-rsa AAAAB3NzaC1yc2..."

# Override the anon-access and allow-keyless settings below (normally
# managed at runtime via the `settings` command and stored in the
# database). If set, these always take precedence over the database, for
# as long as they remain set -- not just on first run. This is intended for
# scripted, non-production bootstrapping (e.g. spinning up a throwaway
# server for local dev tooling), not for production use.
#anon_access: "admin-access"
#allow_keyless: true

# Repository name to create on boot if it does not already exist.
# Leave empty to disable.
#default_repo: ""
```

You can also use environment variables, to override these settings. All server
settings environment variables start with `SUNDAE_` followed by the setting
name all in uppercase. Here are some examples:

- `SUNDAE_NAME`: The name of the server that will appear in the TUI
- `SUNDAE_SSH_LISTEN_ADDR`: SSH listen address
- `SUNDAE_SSH_KEY_PATH`: SSH host key-pair path
- `SUNDAE_HTTP_LISTEN_ADDR`: HTTP listen address
- `SUNDAE_HTTP_PUBLIC_URL`: HTTP public URL used for cloning
- `SUNDAE_GIT_MAX_CONNECTIONS`: The number of simultaneous connections to git daemon
- `SUNDAE_ANON_ACCESS`: Overrides the `anon-access` setting (see [Authentication](#authentication))
- `SUNDAE_ALLOW_KEYLESS`: Overrides the `allow-keyless` setting (see [Authentication](#authentication))
- `SUNDAE_DEFAULT_REPO`: Repository name to create on boot if missing
- `SUNDAE_FORGES_0_NAME`, `SUNDAE_FORGES_0_KIND`, ...: The first entry of `forges` (see [Forges](#forges)); list values are comma-separated; `SUNDAE_FORGES_0_GIT_IDENTITY_NAME` and `..._EMAIL` set its `git_identity`
- `SUNDAE_SESSIONS_GIT_IDENTITY_NAME`, `SUNDAE_SESSIONS_GIT_IDENTITY_EMAIL`: `sessions.git_identity` (see [OpenShell Agent Sessions](#openshell-agent-sessions), "Git identity")
- `SUNDAE_NOTIFICATIONS_ON`, `SUNDAE_NOTIFICATIONS_NTFY_ENABLED`, ...: The `notifications` section (see [Notifications](#notifications)); `on` is comma-separated

#### Database Configuration

Sundae supports both SQLite and Postgres for its database. Like all other Sundae settings, you can change the database _driver_ and _data source_ using either `config.yaml` or environment variables. The default config uses SQLite as the default database driver.

To use Postgres as your database, first create a Sundae database:

```sh
psql -h<hostname> -p<port> -U<user> -c 'CREATE DATABASE sundae'
```

Then set the database _data source_ to point to your Postgres database. For instance, if you're running Postgres locally, using the default user `postgres` and using a database name `sundae`, you would have this config in your config file or environment variable:

```
db:
  driver: "postgres"
  data_source: "postgres://postgres@localhost:5432/sundae?sslmode=disable"
```

Environment variables equivalent:

```sh
SUNDAE_DB_DRIVER=postgres \
SUNDAE_DB_DATA_SOURCE="postgres://postgres@localhost:5432/sundae?sslmode=disable" \
sundae serve
```

You can specify a database connection password in the _data source_ url. For example, `postgres://myuser:dbpass@localhost:5432/my_sundae_db`.

#### LFS Configuration

Sundae supports both Git LFS [HTTP](https://github.com/git-lfs/git-lfs/blob/main/docs/api/README.md) and [SSH](https://github.com/git-lfs/git-lfs/blob/main/docs/proposals/ssh_adapter.md) protocols out of the box, there is no need to do any extra set up.

Use the `lfs` config section to customize your Git LFS server.

> **Note**: The pure-SSH transfer is disabled by default.

#### OpenShell Agent Sessions

Sundae can start and resume agent sessions in
[NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell) sandboxes. Each
session has its own sandbox holding one or more repositories, each cloned
into `/sandbox/<repo>`. The session's program (a shell by default, or an
agent) runs inside an [osewa](https://github.com/rjyo/osewa) session, in the
checkout of its only repository, else in `/sandbox`, so it keeps running
after your SSH connection closes. Your SSH terminal is bridged to it:

```sh
ssh -t -p 23231 localhost session start icecream                    # create or resume the session "icecream" holding the repository, then attach
ssh -t -p 23231 localhost session new bundle --repo app --repo lib  # a session holding several repositories
ssh -t -p 23231 localhost session start icecream --name review      # a second session of the same repository
ssh -t -p 23231 localhost session start icecream --profile claude   # create from a profile (see below)
ssh -t -p 23231 localhost session start icecream --image my/img     # image used only when the sandbox is first created
ssh    -p 23231 localhost session start icecream -n bg --detach     # create or resume without attaching (no -t needed)
ssh -t -p 23231 localhost session attach review                     # attach to an existing session (resuming it if parked)
ssh    -p 23231 localhost session repos bundle                      # the session's repositories
ssh    -p 23231 localhost session repos bundle add docs             # clone another repository into it
ssh    -p 23231 localhost session repos bundle remove lib           # take one out (its checkout stays)
ssh    -p 23231 localhost session ls                                # your sessions, with what each agent is doing
ssh    -p 23231 localhost session ls --json                         # the same as JSON
ssh    -p 23231 localhost session signals                           # what your sessions reported, newest first
ssh    -p 23231 localhost session signals --attention --follow      # keep printing what needs you (Ctrl-C to stop)
ssh    -p 23231 localhost session park review                       # suspend that session's sandbox now
ssh    -p 23231 localhost session resume review                     # resume it without attaching
ssh    -p 23231 localhost session end review                        # delete that session's sandbox and revoke its token
```

**Sessions and repositories.** A session is named (1 to 32 lowercase
letters, digits and hyphens) and its name is unique per user. `session new
NAME --repo R...` creates one holding the repositories given, in order;
`session start REPO` is the one-repository shorthand, naming the session
after the repository's last path segment (or a numbered name such as `tool-2`
when a session of that name holds other repositories), or `--name`. Starting
an existing session resumes it and adds any repository it lacks. Commands
that take `SESSION` (attach, park, resume, end, repos) accept the session's
name, or a repository held by exactly one of your sessions.

Each session is its own sandbox, named `sd-` and 16 hex digits of a hash of
the user and session name: OpenShell 0.1.8 refuses sandbox names over 19
characters. The sandbox records the user, session name, profile and the
repositories it was created with as annotations; OpenShell cannot change
annotations, so after `session repos add|remove` the current list is kept in
the sandbox (`~/.config/sundae/repos`, which snapshots keep too) and the
server remembers it. After a server restart a session shows the list it was
created with until it is next started or attached. Removing a repository
leaves its checkout in `/sandbox`. A new session's profile is `--profile`,
else the first repository's own profile (`repo profile`), else the first
`sessions.repos` match among its repositories, in order. Each repository
gets the git identity of the forge it was imported from (see "Git
identity"). `session end` deletes one session and leaves the others.

**Detach and re-attach.** `session start` and `session attach` need a
terminal, so pass `-t`. Detach with `~.` after a newline (osewa's escape), or
just close the connection: either ends only the bridge, and the program keeps
running. `session start` or `session attach` re-attaches to the same program,
starting it again only if it has exited or the sandbox was restarted. The
attach exits with the program's exit status when the program ends. A new
attach takes over from an earlier one, which stays connected as a read-only
viewer (`Ctrl-T` takes control back). You need read access to every
repository a session holds, and anonymous users can't use sessions. `session
attach` never creates a sandbox.

The feature is off by default. When it is disabled, `session` is an unknown
command. To enable it:

```yaml
openshell:
  enabled: true
  # The OpenShell gateway gRPC address (http:// = plaintext, https:// or no scheme = TLS).
  gateway_address: "https://openshell.sundae.svc.cluster.local:8080"
  workspace: "default"
  # Sandbox image when no profile sets one. It must provide git and jj
  # (deploy/sandbox/Containerfile builds one, with osewa).
  image: "<registry>/sundae-sandbox:<tag>"
  # Optional bearer token for the gateway. Prefer token_file, which keeps the
  # token out of this file. Set at most one of the two.
  #token: ""
  # File holding the bearer token, e.g. mounted from a secret. Trailing
  # whitespace is trimmed. The file is read again for every session command,
  # so a token rotated in place needs no restart.
  #token_file: "/etc/openshell-token/token"
  # Client TLS files, e.g. mounted from the openshell-client-tls secret.
  tls_ca: "/etc/openshell-tls/ca.crt"
  tls_cert: "/etc/openshell-tls/tls.crt"
  tls_key: "/etc/openshell-tls/tls.key"
  insecure: false
  # Internal HTTP base URL that sandboxes use to clone and push.
  # Defaults to http.public_url.
  clone_url: "http://sundae-http.sundae.svc.cluster.local:23232"
  # Lifetime of the git token in a sandbox; renewed on every start and attach.
  token_ttl: "24h"
  # How long a session start waits for its sandbox to become ready. A first
  # start pulls the image and copies its /sandbox into the new workspace
  # volume. At least 30s.
  ready_timeout: "15m"
  # The program a session runs when no profile sets one, in /sandbox/<repo>.
  command: "bash -l"
  # OpenShell providers Sundae creates and keeps up to date on the gateway
  # (see "Declared providers" below).
  providers:
    - name: my-github
      type: github
      credentials_dir: /etc/sundae-providers/my-github  # GITHUB_TOKEN
    - name: my-claude
      type: anthropic
      credentials_dir: /etc/sundae-providers/my-claude  # ANTHROPIC_API_KEY
  providers_sync: 1m       # at least 10s
  prune_providers: false   # delete undeclared providers Sundae created
  # The policy of new sandboxes: "sundae" (OpenShell's restrictive default
  # plus egress to clone_url, the S3 snapshot endpoint and network_policies)
  # or "image" (none sent; the image's /etc/openshell/policy.yaml applies).
  # See "Sandbox egress" below.
  sandbox_policy: sundae
  network_policies: []

sessions:
  osewa:
    # Pinned osewa builds uploaded to sandboxes whose image lacks osewa.
    binary_dir: "/usr/local/share/sundae/osewa"
    # Loopback port of the osewa server inside each sandbox.
    port: 7322
  status:
    # How often to read each running session's status (at least 1s).
    poll_interval: 10s
  suspend:
    # Park idle sessions (see "Parking idle sessions").
    enabled: true
    # Nobody attached and not working for this long.
    after: 30m
  signals:
    # How many signals `session signals` keeps, across all users, in memory.
    keep: 1000
  # Named presets for `session start --profile NAME`.
  profiles:
    claude:
      image: ghcr.io/example/agent-claude:latest  # empty: openshell.image
      command: claude                             # empty: openshell.command
      # OpenShell providers attached to this profile's sandboxes only.
      providers: [my-claude, my-github]
  # The profile for sessions started without --profile, by repository glob.
  repos:
    "infra/*":
      profile: claude
  # Who commits in sessions of repositories not imported from a forge (and
  # of forges without their own git_identity). See "Git identity" below.
  git_identity:
    name: Jane Doe
    email: jane@example.com
```

The `openshell` settings and `sessions.osewa`, `sessions.status`,
`sessions.suspend`, `sessions.signals` and `sessions.git_identity` can also
be set with `SUNDAE_OPENSHELL_*` and `SUNDAE_SESSIONS_OSEWA_*`,
`SUNDAE_SESSIONS_STATUS_*`, `SUNDAE_SESSIONS_SUSPEND_*`,
`SUNDAE_SESSIONS_SIGNALS_*` and `SUNDAE_SESSIONS_GIT_IDENTITY_*`
environment variables, for example
`SUNDAE_OPENSHELL_ENABLED=true` or
`SUNDAE_SESSIONS_SUSPEND_AFTER=1h` (`openshell.providers` as
`SUNDAE_OPENSHELL_PROVIDERS_<index>_<KEY>`, `CONFIG` holding `KEY=VALUE`
lines); `sessions.profiles` and
`sessions.repos` only in the file. `status.poll_interval` must be at least
`1s`, and `suspend.after` must not be shorter than it. `signals.keep` is
between 1 and 100000 (default 1000). When OpenShell is
enabled, `gateway_address` and `image` are required, and a `token_file`, if
set, must be readable and not empty. Setting both `token` and `token_file` is
an error. `sundae config check` reports every problem, naming the key.

**Profiles.** A new session is created from `--profile`, else from the
repository's own profile (`repo profile`, see
[Project templates and profiles](#project-templates-and-profiles)), else from
the `sessions.repos` entry matching the repository (`*` does not cross `/`,
`**` does, case-insensitive; when several match, the one with the most
non-wildcard characters wins), else from `openshell.image` and
`openshell.command`. A repository profile that is no longer configured is
skipped with a warning. `--image` overrides the image. A session keeps the
profile it was created from: `--profile` and `--image` are ignored, with a
warning, when it already exists. Profile names are DNS labels; every
`repos` entry must name a defined profile.

**Providers.** `sessions.profiles.<name>.providers` lists
[OpenShell providers](https://github.com/NVIDIA/OpenShell/blob/v0.1.8/docs/how-it-works/providers/overview.mdx)
to attach to the sandboxes of that profile's sessions, so a secret is
available only to sessions of the profiles that list its provider. Sessions
without a profile get none (there is no default list). Declare the
providers in `openshell.providers` and Sundae creates and updates them on the
gateway from credential files (see "Declared providers" below). A provider a
profile lists that is not declared is attached by name, as before: create it
on the gateway yourself, e.g.
`openshell provider create --name my-claude --type anthropic --from-existing`.
The gateway keeps the credentials: the sandbox only ever sees opaque
placeholders (`openshell:resolve:env:...`) in its environment variables
(`ANTHROPIC_API_KEY`, `GITHUB_TOKEN`, ...), which the sandbox proxy replaces
with the real value on requests to the provider's endpoints. Names are 1 to
253 bytes without spaces, commas or control characters, at most 32 per
profile; duplicates are dropped.

- A new session's sandbox is created with the profile's providers (recorded
  in its `sundae.providers` annotation). A provider that does not exist
  fails `session start` with an error naming it and the profile.
- On every `session start` and `session attach` of an existing session,
  Sundae makes the sandbox's providers match its profile's current list:
  it attaches the missing ones and detaches the others, including any
  attached by hand with `openshell sandbox provider attach`. A provider
  that does not exist fails the command; any other failure is a warning,
  and the session keeps the providers it has. A session whose profile is no
  longer configured keeps its providers.
- OpenShell gives a process the placeholders of the providers attached when
  it starts, and never changes a running process's environment. A running
  program keeps its credentials after an attach changes the providers
  (exit it and attach again to restart it with the new set); a detached
  provider's placeholders stop resolving at once. A sandbox applies a
  change within its policy poll interval (10 seconds by default), so a
  program started within that time of an attach may miss the new
  provider: start it again.
- osewa starts a session's program with a clean environment, so for a
  session with providers Sundae hands the program the environment
  OpenShell gives exec'd commands: on creating the osewa session it writes
  that environment (the provider placeholders and settings, the
  `SSL_CERT_FILE`-style CA variables of the sandbox proxy, `SANDBOX`),
  less `HOME`, `USER`, `LOGNAME`, `SHELL`, `PATH`, `TERM` and osewa's and
  Sundae's own settings, to `/tmp/sundae/session-env.<pid>` (mode 0600,
  outside `/sandbox` and so outside snapshots); the session's login shell
  exports it and deletes the file before it starts the program, which
  passes it on to everything it runs (`bash -l` included). The file holds
  placeholders, never credentials. Sessions without providers get exactly
  the environment they got before.
- `session ls --json` lists each session's attached `providers`.

**Declared providers.** Each `openshell.providers` entry is a provider
Sundae creates on the gateway and keeps up to date, so credentials come from
Kubernetes Secrets (synced e.g. by External Secrets) instead of a manual
`openshell provider create`:

- `name` is the provider's name, which profiles list; `type` its provider
  profile ID (`anthropic`, `github`, an imported profile; `openshell profile
  list`); unique names, same rules as above. `type` cannot change once the
  provider exists: OpenShell refuses, and Sundae reports it until you delete
  the provider (`openshell provider delete`) so it is created again.
- `credentials_dir` is a directory with one file per credential: the file
  name is the credential key, an environment variable name such as
  `GITHUB_TOKEN` (not `v<digits>_...`, which OpenShell reserves), and its
  content the value, less one trailing newline; at most 32 files of at most
  8192 bytes. A Secret mounted as a volume is such a directory (its `..data`
  link and `..<timestamp>` directory are skipped, links are followed). Omit
  it only for a provider type that needs no credentials (`pypi`).
- `config` is the provider's non-secret settings (`ANTHROPIC_BASE_URL: ...`),
  one-line values; keys starting with `sundae.` are reserved.
- `adopt: true` lets Sundae take over a provider of that name that it did not
  create (see below).

When OpenShell is enabled, the server and `sundae config check` read every
`credentials_dir` at startup: a missing, empty or malformed one is a config
error naming the provider and file, never a value. The server then syncs the
declared providers at startup and every `openshell.providers_sync` (default
`1m`, at least `10s`), reading the files again each time:

- A missing provider is created with the label `sundae.managed=true`.
- A provider is updated only when its type, credentials or config changed.
  Sundae keeps its sync state in three provider config entries:
  `sundae.sync-hash` (an HMAC-SHA256 of the type, credentials and config,
  keyed with a secret Sundae keeps in `<data>/openshell/provider-sync.key`,
  so the hash says nothing about the credentials), and
  `sundae.credential-keys` and `sundae.config-keys` (the keys it set, so a
  key removed from the directory or `config` is deleted: OpenShell merges an
  update into the stored provider). OpenShell 0.1.8 sets labels only on
  creation and never returns credentials, which is why the state is kept
  in config. A change made out of band with `openshell provider update`
  is not noticed until the files or config change.
- An existing provider without the managed label (created by hand) is left
  alone and reported as a `conflict`: delete it to let Sundae create it, or
  set `adopt: true` to have Sundae update it in place (it keeps its labels
  and any credential keys Sundae did not set).
- A provider Sundae created that is no longer declared is kept and listed as
  `undeclared`; with `openshell.prune_providers: true` Sundae deletes it.
  OpenShell refuses to delete a provider attached to a sandbox; Sundae tries
  again every sync.
- Each provider is synced on its own: one failing does not hold up the
  others or the server. `session start` of a profile that lists a declared
  provider whose last sync failed fails with an error naming it;
  `session attach` warns and goes ahead.
- Outcomes are logged (names and credential keys only), published as
  `openshell.provider` events (created, updated, deleted, and failed when
  the error changes) and counted in `sundae_openshell_provider_syncs_total`
  (`provider`, `result`); `sundae_openshell_provider_sync_ok` is 1 while a
  provider's last sync succeeded.

`ssh <host> openshell providers` (admins only; `--json`) lists the declared
providers with their state, owner (`sundae`, `adopted`, `other` for a
conflict), last sync and error, credential key names (never values) and
the profiles that list them, then the undeclared ones Sundae created, then
the `manual` ones profiles list without declaring them.

**Rotation.** A new value in the Secret reaches the mounted files (after the
kubelet's sync delay, typically a minute or two) and the gateway on the
next sync, as an update of the same provider. What the sandboxes do with it
is OpenShell's: a running sandbox notices the change within its poll
interval, and only processes started after that get references to the new
value. A running session program keeps the placeholders it started with: by
default they are revision-scoped, so they keep resolving the previous value
while OpenShell retains that revision and may then fall back to the current
one; a provider profile credential with `stable_placeholder: true` keeps one
placeholder across value-only updates, so a running program uses the new
value once the sandbox has installed it
([Profiles, Runtime Limitations](https://github.com/NVIDIA/OpenShell/blob/v0.1.8/docs/how-it-works/providers/profiles.mdx)).
To be sure a session uses a rotated credential, exit its program and attach
again (or `session end` and start it). Deleting and recreating a provider
(a type change) gives it new placeholders; Sundae never does that itself.

A placeholder only resolves when the sandbox's network policy lets the
calling binary reach the endpoint, the endpoint is in the provider's
credential binding, and the credential is in the declared place (the
`Authorization` header by default); see "How Credential Injection Works" in
the providers overview. The policy Sundae gives sandboxes (see "Sandbox
egress") allows only its own endpoints and `openshell.network_policies`,
and binds no credentials. What the operator must provide:

- **Provider profiles.** A provider's type is an imported provider profile
  (`openshell profile import`); its `endpoints` bind the credential and its
  `binaries` must be the paths of the client programs in your sandbox image
  (`/usr/bin/curl`, `/usr/local/bin/claude`, ...). A profile whose binaries
  do not match the image has its credential withheld and its traffic denied
  ([Profiles](https://github.com/NVIDIA/OpenShell/blob/v0.1.8/docs/how-it-works/providers/profiles.mdx)).
  Attached providers add their profile's `_provider_*` network policy
  entries to the sandbox's effective policy, so a profile that covers its
  endpoints and binaries needs no policy of yours.
- **Endpoints outside a profile.** Any endpoint the agent reaches that the
  provider policy does not allow needs an `openshell.network_policies`
  rule (see "Sandbox egress"). An endpointless profile binds only through a
  sandbox policy endpoint naming the provider instance
  (`credential_binding: {provider: my-api}`), which those rules cannot
  express: that needs your own policy (`openshell.sandbox_policy: image`).
- **HTTPS inspection.** The proxy substitutes credentials only in traffic
  it handles as HTTP: an endpoint with `tls: skip` or a raw tunnel never
  resolves a placeholder. The programs must trust the sandbox proxy's CA,
  which the forwarded `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`,
  `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE` and `GIT_SSL_CAINFO` point at.

**Git identity.** A sandbox has no git or jj identity of its own, so
without one the first `git commit` fails and jj commits have no author.
Sundae can set one per repository:

1. a repository imported from a forge (see [Forges](#forges)) uses that
   forge's `forges[].git_identity`, e.g. the account's noreply address;
2. any other repository, and one whose forge has no `git_identity`, uses
   `sessions.git_identity`;
3. with neither, Sundae sets nothing, as before.

The forge link comes from the repository's git config (`[forge] name`), so
it follows the repository, not the user. On every `session start` and
`session attach` the identity is written into the checkout's own repository
config: `git -C /sandbox/<repo> config user.name/user.email` (`.git/config`),
and for jj and colocated repositories `jj config set --repo` (jj's
per-repository config). The sandbox user's `~/.gitconfig` and jj user config
are never touched, so an identity set there by hand still applies to other
checkouts. The first clone already uses it (`jj git clone --config
user.name=... --config user.email=...`, `git clone -c ...`), so jj's initial
working-copy commit has the right author. A checkout that is missing or not
a repository is left alone; a failure to set the identity is a warning, not
a failed start. A changed identity reaches existing sessions on their next
start or attach and applies to commits made from then on; jj leaves the
author of the current working-copy commit as it is (`jj metaedit
--update-author` changes it). Removing an identity from the config leaves the
one already written in existing checkouts.

Both settings are server-wide, like the forge tokens: every user's sessions
of a repository commit as the same identity. `name` and `email` are both
required when the block is set; the email must look like `user@host`;
neither may contain control characters or `<`/`>`, and each is at most 256
bytes. `sundae config check` reports a problem naming the key, e.g.
`forges[0] (github): git_identity.email is required when git_identity is set`.

**osewa in the sandbox.** Sundae pins one osewa release (its protocol is a
draft). On every start and attach it:

1. uses `osewa` from the image's `PATH` if it is the pinned version, else
   uploads the pinned Linux build from `sessions.osewa.binary_dir` (checked
   against the release's SHA-256 here and again in the sandbox) to
   `/sandbox/.local/lib/sundae/`. The Sundae image built from
   `Containerfile` carries both builds; the sandbox image from
   `deploy/sandbox/Containerfile` has osewa already. The OpenShell Go SDK has
   no file transport yet, so the upload goes through exec calls, about 55 of
   them; it happens once per sandbox.
2. starts `osewa server` on `127.0.0.1:<port>`, loopback only, with QUIC and
   port forwarding off, unless it is running. It runs detached from the exec
   that started it, until the sandbox stops.
3. once per sandbox, gives the bridge an osewa client identity
   (`~/.config/sundae/osewa-client`) and enrols it with that server.
4. starts the session's program in an osewa session in `/sandbox/<repo>`
   through `/bin/sh -l`, unless the one recorded in
   `~/.config/sundae/osewa-session` is running.

The bridge is `osewa attach` against that loopback server. Sandbox images
other than the one in `deploy/sandbox` need `sh`, `git`, `gzip`, `base64`
and `sha256sum` (and `flock`, used when present). osewa ends a session that
has had nobody attached and no output for 24 hours.

**Session status.** Agents report what they are doing with the
[Program Status Protocol (OSC 7501)](https://www.superlogical.com/rex/docs/build/program-status);
osewa keeps each session's latest report. While OpenShell is enabled, Sundae
reads it from every running session: every `sessions.status.poll_interval`
(default 10s) it lists the sandboxes it created and, in each Ready one, runs
`osewa local ls` (one exec per sandbox, at most 8 at a time, each cut off
after 15 seconds) and finds the session's program by the id recorded in the
sandbox. It keeps, in memory only, the state, kind, progress, program, title
and message, how many clients are attached, how long osewa has seen no
output, whether the program exited and with what status, when the state last
changed, and when and why the last poll failed. Each change goes on the event
bus as a status event (so `notifications` see it), and a program exiting as
an `exited` lifecycle event. The first poll after Sundae starts only
records what it finds, so a restart does not notify again about every
session; "last changed" then counts from the restart.

`session ls` shows it, most urgent first: blocked, error, done, working,
idle, then sessions that never reported:

```
Repository  Name    Phase      State                           Message                   Attached  Changed
infra       claude  Suspended  ▲ blocked · permission · parked  Apply 3 to add, 1 to cha…  0         4m
sundae  main    Ready      ● working 40%                   Running go test ./pkg/…   1         now
dotfiles    main    Ready      · none                          -                         0         3h
```

(Profile and Sandbox columns left out.) The message is the agent's `msg`,
else its title, else the program's name, with control and invisible
formatting characters removed and cut to 60 characters. After the state come
the blocked kind and progress, then `parked` or `starting` for the sandbox,
`exited` when the program has ended, and `stale` when the last poll failed: a
session that cannot be read keeps its last known status. Suspended,
stopped and starting sandboxes are never exec'd; a parked session shows the
status it had when it was parked. `session ls --json` prints the same as a
list of objects (`state` is `none` when there is no report); text from the
agent is sanitised there too, but not cut. Listing never resumes a parked
session. osewa keeps only the root record, so child records (`id=...`) and
changes between two polls are not seen.

**Signals.** `session signals` shows the history behind `session ls`: every
status change and lifecycle step (`created`, `resumed`, `ready`, `parked`,
`exited`, `ended`, `failed`) of your sessions that went on the event bus,
newest first. Forge results are not signals.

```
TIME      STATE                   SESSION                       MESSAGE
14:02:11  ▲ blocked · permission  infra / claude-1              Apply 3 to add, 1 to change?
14:02:03  ● working               infra / claude-1              Planning
13:58:40  ✓ done                  sundae / forge-sync       Implemented GitHub discovery
13:12:00  ○ idle · parked         dotfiles / main               parked while idle
12:50:19  + created               sundae / tui              sandbox created
```

A status row has the state's glyph and word, its progress and blocked kind,
and the message as in `session ls` (sanitised; with a PTY, `ssh -t`, each row
is cut to the terminal's width, otherwise not cut). A child record would show
its id after the session name (`sundae / tui/lint`). A lifecycle row
shows the session's last known state with the step after it (`○ idle ·
parked`), except `+ created`, `- ended` and `✗ failed · start`, whose message
is the reason. Times are the server's; rows older than today show the date.

`--repo R` and `--name N` select sessions, `--attention` only what asks for
you (blocked, error and done reports, and failures), and `--limit K` the
newest K (default 50, `0` for all kept). `--json` prints one JSON object per
line, with the agent's text sanitised but not cut. `--follow` (no `-t`
needed) first prints the signals kept, oldest first like `tail -f`, then each
new one as it arrives, until you disconnect (Ctrl-C) or the server shuts
down. You only ever see your own sessions.

The limits: signals are kept in memory only, the last `sessions.signals.keep`
(default 1000) across all users, so the feed is empty after a restart and a
busy user can push out another's older signals. They come from polling, so a
state that lasts less than `sessions.status.poll_interval` may never show,
and child records never do; the first poll after a restart records what it
finds without a signal. A complete feed needs an in-pod watcher that pushes
every report (roadmap item C4), which is not built yet. If Sundae falls
behind the event bus, a `! missed` row (JSON `"type": "gap"`) marks where
events were lost, rather than leaving a silent hole; so does a `--follow`
reader that fell further behind than the feed keeps.

**Parking idle sessions.** A session nobody uses is parked: its sandbox is
suspended through OpenShell, keeping its memory where the compute driver can,
so it uses no CPU until someone comes back. A session is parked once, for the
whole of `sessions.suspend.after` (default 30m), nobody has been attached
(osewa counts no attached client, and Sundae has no bridge open on it)
and it has not reported `working`. Every other state counts as not working,
so an agent waiting on you (`blocked`) is parked too and still reads
`▲ blocked · parked`. A program that never reports a status must also have
been quiet that long (osewa has seen no output from it), so a plain shell
running a long build is not frozen. A program that has exited runs nothing,
so its session is parked like an idle one; the next attach starts it again
either way. Sundae also counts its own bridges: from the moment `session
start` or `session attach` begins preparing a session until its bridge
closes, however it closes, the session is not parked. The idle window starts
again whenever Sundae sees a client attached, a bridge open or close, a
`working` report, or the sandbox come back up, and when Sundae starts.
Each park is a `parked` lifecycle event (`op` `auto`, or `park` for `session
park`).

`session start`, `session attach` and `session resume` resume a parked
session (and say so), then carry on as usual; that is a `resumed` event. A
resume asks OpenShell to boot the sandbox cleanly if its memory cannot be
restored (`RestartIfRestoreFails`): the session stays usable with what the
backend kept of `/sandbox`, rather than failing until it is ended. osewa's
server and the program are gone after such a restart, so Sundae starts
them again, as for an exited program, and tells you: `Started claude again:
it was not running when the session resumed (it had exited, or the sandbox
could not restore its memory and restarted cleanly)`. Whatever the program
held in memory, such as an agent's conversation, is lost then.

`session park` parks a session at once (not while a bridge is open on it);
`session resume` resumes one without attaching. Sundae only parks
sandboxes it created (labelled `sundae.managed=true`, with an owner and a
repository). Parking needs a compute driver with OpenShell's suspend-resume
capability; the OpenShell 0.1.8 Kubernetes driver has none. When the gateway
refuses, Sundae logs it once, stops trying for that sandbox until it
restarts, and `session ls --json` shows why under `unsuspendable`. Set
`sessions.suspend.enabled: false` to not try at all, or
`sessions.suspend.fallback: snapshot` to park such sessions to S3 instead (see
"S3 snapshots"). After a Sundae restart a session that was parked before
shows no status until it is resumed: the status is kept in memory only.

**Client TLS secret.** In Kubernetes, mount the gateway's client certificate
secret, typically `openshell-client-tls`, into the Sundae pod and point the
`tls_*` settings at it:

```yaml
volumes:
  - name: openshell-tls
    secret:
      secretName: openshell-client-tls
containers:
  - name: sundae
    volumeMounts:
      - name: openshell-tls
        mountPath: /etc/openshell-tls
        readOnly: true
```

**Git credentials.** On each `session start` and `session attach`, Sundae
issues a short-lived access token named `openshell:<sandbox>` (see
`token_ttl`) and prunes expired ones of that name. It writes the token, as an
`http.<clone_url>/.extraHeader` basic-auth header, to
`/tmp/sundae/gitconfig` in the sandbox (mode 0600, in a 0700 directory),
and adds that file to the sandbox user's `~/.gitconfig` as an
`include.path`. So every git process in the sandbox, including agents in osewa
sessions, which start with a clean environment, uses the newest token. The
token is never in `.git/config`, `~/.gitconfig`, anything under `/sandbox`
(so never in a snapshot of it), a command line Sundae builds, or the
sandbox's spec; it reaches the sandbox in the environment of the exec that
writes the file. It grants your read/write access, so agents can push.
Processes inside the sandbox can read it. `session end` revokes it.

A program left running longer than `token_ttl` after the last start or
attach loses git access until the next one renews the file. Attach again, or
raise `token_ttl` for long-running agents.

**Sandbox egress.** OpenShell denies a sandbox all network access its
policy does not allow, and git (and jj, which runs git) in the sandbox must
reach Sundae's internal HTTP endpoint. With `openshell.sandbox_policy:
sundae`, the default, Sundae gives every sandbox it creates a policy that
allows it: OpenShell's restrictive default policy (`/sandbox` and `/tmp`
writable, the system directories read-only, the default process identity;
`restrictive_default_policy()` in OpenShell 0.1.8) with these network rules,
each allowing any program in the sandbox and inspecting nothing:

- `sundae_git`: the host and port of `openshell.clone_url` (or
  `http.public_url`), 80 or 443 by scheme when the URL has no port;
- `sundae_snapshots`, with S3 snapshots on: the host and port the presigned
  snapshot URLs point at, `<bucket>.<endpoint host>` (for AWS,
  `<bucket>.s3.<region>.amazonaws.com`), or the endpoint's host with
  `path_style: true`;
- the rules in `openshell.network_policies`.

Providers attached to a sandbox add their profiles' rules on the gateway.
Anything else a session needs, such as an agent's API that no provider
covers or a package registry, goes in `openshell.network_policies`:

```yaml
openshell:
  network_policies:
    - name: anthropic
      endpoints:
        - host: api.anthropic.com       # a name, an IP, or *.example.com
          port: 443                     # or ports: [80, 443]
      binaries: [/usr/local/bin/claude] # empty: any program
    - name: pypi
      endpoints:
        - host: pypi.org
          port: 443
          protocol: rest                # inspect requests...
          access: read-only             # ...and allow only reads
        - host: files.pythonhosted.org
          port: 443
```

An endpoint needs a `host` or `allowed_ips` (IP addresses or CIDR ranges
every resolved address must be in; a wildcard host needs them to reach a
private address, an exact name does not), and `port` or `ports`. `protocol`
is empty (connections pass through the sandbox proxy uninspected, right for
git, curl and most clients), `rest` (with `access`: `read-only`,
`read-write` or `full`) or `tcp` (native TCP, for non-HTTP clients). Names
may not start with `sundae_`. The environment form is
`SUNDAE_OPENSHELL_NETWORK_POLICIES_<i>_NAME`, `..._BINARIES` (comma-separated)
and `SUNDAE_OPENSHELL_NETWORK_POLICIES_<i>_ENDPOINTS_<j>_HOST`, `_PORT`,
`_PORTS`, `_PROTOCOL`, `_ACCESS`, `_ALLOWED_IPS`. `sundae config check`
checks the rules; the gateway checks the whole policy again when it creates
a sandbox.

The policy is sent only when a sandbox is created, and takes precedence over
the image's `/etc/openshell/policy.yaml`. An existing sandbox keeps the
policy it was created with, through stops, parks and resumes; a change
reaches a session when it gets a new sandbox (`session end`, then start it
again). A gateway-global policy (`openshell policy set --global`) replaces
every sandbox's policy, Sundae's included.

To ship your own policy in the image instead, set `openshell.sandbox_policy:
image`: Sundae sends none, and the image's `/etc/openshell/policy.yaml`
applies, or OpenShell's default, which allows no network access.
`network_policies` is then refused. That policy needs at least:

```yaml
network_policies:
  sundae_git:
    name: sundae_git
    endpoints:
      - host: sundae-http.sundae.svc.cluster.local
        port: 23232
    binaries:
      - path: "/**"
```

and, with S3 snapshots on, a rule like it for the snapshot host (see "S3
snapshots").

osewa needs no egress: the bridge reaches its server over loopback, and Sundae
turns its update checks off.

**Idle timeout.** A bridged session is an ordinary SSH session, so
`ssh.idle_timeout` (default 600 seconds) still applies. Terminal traffic in
either direction counts as activity. A connection that sits completely quiet
for longer than the timeout is disconnected; the program keeps running, and
`session attach` re-attaches. To avoid the disconnects, raise `idle_timeout`
or set `0`, or set `ServerAliveInterval` in your SSH client.

#### S3 snapshots

Sundae can save a session's `/sandbox` (the checkouts and the sandbox
user's home) to an S3 bucket and restore it, so `session end` loses nothing
and an idle session can be parked without a sandbox. It is off by default:
while `snapshots.s3.enabled` is false nothing runs and nothing in the section
is checked.

```yaml
snapshots:
  s3:
    enabled: true
    endpoint: ""                 # empty for AWS S3; the base URL of MinIO, Ceph RGW, ...
    region: eu-west-2            # required for AWS; defaults to us-east-1 with an endpoint
    bucket: agent-snapshots      # must exist
    prefix: sundae/sessions  # the default
    credentials_file: /etc/sundae/secrets/s3-credentials
    sse: ""                      # AES256, aws:kms, or "" for the bucket's default
    path_style: false            # true for most MinIO and RGW setups
    "on": [end]                  # add park for a backup before every memory-retaining park
sessions:
  suspend:
    fallback: snapshot           # default none; see "Parking to a snapshot" below
```

**Credentials.** `credentials_file` is an AWS shared-credentials file
(`[default]` with `aws_access_key_id`, `aws_secret_access_key` and optionally
`aws_session_token`; `AWS_ACCESS_KEY_ID=...` lines work too), or a directory
holding the files `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, which is
what a Kubernetes Secret with those keys mounts as (a Rook
ObjectBucketClaim's, for example). It is read for every operation, so
rotated keys need no restart, and `sundae config check` reads it. The keys need
`s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` and `s3:ListBucket` on the
prefix. They never enter a sandbox: Sundae signs a URL for one object
(AWS Signature V4, valid for an hour) and the sandbox uses that.

**How a snapshot moves.** The archive never passes through Sundae. To
snapshot, Sundae runs `tar | gzip` in the sandbox into a file in
`/tmp/sundae` (outside `/sandbox`), presigns a PUT that signs the
archive's MD5 and the `sse` header, and the sandbox uploads the file with
`curl`; then the file is deleted. To restore, the sandbox streams `curl |
gzip -d | tar -x` into `/sandbox`. The sandbox therefore needs `sh`, `tar`
(GNU tar for anchored exclusions), `gzip`, `md5sum` and `curl`, which the
sandbox image in `deploy/sandbox` has, egress to the endpoint (the policy
Sundae gives sandboxes allows it; see "Sandbox egress"), and free space in `/tmp` for the compressed archive. The archive is
written to a file first, not streamed, because AWS S3 refuses an upload
without a length, and so a `tar` that fails half-way never uploads a
truncated archive.

**One object per session.** A session's snapshot is
`<prefix>/<user-id>/<sandbox>.tar.gz`, replaced by every snapshot. The upload
goes straight to that key: S3 only replaces an object once the new upload is
complete and its MD5 checks out, so a failed upload leaves the previous
snapshot as it was. One upload carries at most 5 GiB (the AWS S3 and Ceph
RGW limit for a single PUT): a larger compressed archive fails the snapshot
with that message, before anything is uploaded. Interrupted uploads leave no
partial objects. Nothing deletes snapshots; use a bucket lifecycle rule if you
want old ones gone.

**What is left out.** Everything under `/sandbox` is saved except: osewa's
runtime (`~/.config/osewa`, `~/.local/state/osewa`, the bridge's client key
and session id under `~/.config/sundae`), which would point at a server
that no longer exists, and is recreated; the osewa build Sundae uploads
(`~/.local/lib/sundae`), uploaded and checksummed again; the XDG cache
(`~/.cache`); and credentials: `~/.ssh`, `~/.gnupg`, `~/.netrc`,
`~/.git-credentials`, `~/.aws`, `~/.kube`, `~/.docker/config.json`, and the
logins of `gh`, `glab`, Claude Code (`~/.claude/.credentials.json`) and Codex
(`~/.codex/auth.json`). Paths are relative to `/sandbox` only: a checkout's
own `.cache` or `.ssh` is kept. Sundae's git token is never under
`/sandbox`. Anything else secret you keep in `/sandbox` (an `.env` file in a
checkout) is in the snapshot: protect the bucket accordingly.

**`session end`.** With `end` in `on`, `/sandbox` is saved before the sandbox
is deleted (a parked sandbox is resumed for that). If the snapshot fails, the
session is not ended and the error says why; `session end --force` ends it
anyway, without a snapshot. In the TUI, ending asks again: `The snapshot
failed. End … without one?`.

**`session start`.** When a start creates the sandbox and the session has a
snapshot, `/sandbox` is restored before the clone check, so the restored
checkout is used rather than cloned again: `Restoring /sandbox from the
snapshot of 2026-11-12 10:41 UTC, 312 MiB...`. A sandbox that already exists
is never restored into. `--fresh` starts from a fresh clone instead (the
snapshot stays until the next one replaces it). If the restore fails, the new
sandbox is deleted again and the start fails, so the next start tries again
from scratch; no half-restored session is left. The snapshot wins over what
the image put in the new `/sandbox` (shell start-up files) without asking. If
the new sandbox already holds work, Sundae's own state or the checkout,
which a new sandbox only does when a volume outlived its sandbox, the start
asks first: `Restore the snapshot over them? [y/N]` (files the snapshot has
replace those in the sandbox; files only in the sandbox are kept). Without a
terminal it fails and says to run `session start --restore` (yes) or
`--fresh` (keep them); the TUI asks the same in its confirm line.

**Parking to a snapshot.** With `sessions.suspend.fallback: snapshot`, a
session the gateway cannot suspend (the OpenShell 0.1.8 Kubernetes driver
cannot) is parked by saving `/sandbox` and deleting the sandbox, automatically
when idle or with `session park`. Its program is stopped: whatever it held
in memory is lost. `session ls` shows it as `parked (snapshot)` (phase
`Snapshot`, `"park": "snapshot"` in `--json`) with `program stopped by the
snapshot park; attach restarts it`, distinct from a memory-retaining
`parked`. The session's record while it has no sandbox is a small object next
to its snapshot, `<prefix>/<user-id>/<sandbox>.parked.json` (repository,
session, profile, image and its last status): Sundae keeps no session
state of its own, and the sandbox that would carry it is gone. `session
start`, `attach` and `resume` create the sandbox again from the same image,
restore `/sandbox`, delete that object and start the program, with the usual
notice: `Started claude again: it was not running when the session resumed
(the session was parked to a snapshot of /sandbox, which keeps files but not
running programs)`. `session end` of such a session deletes the object and
keeps the snapshot. If the snapshot fails, the sandbox is left running and
the park is not tried again for another `sessions.suspend.after`.

**Events.** Snapshots and restores are lifecycle events in `session signals`,
the TUI's Signals pane and notifications: `snapshotted` (`op` `end` or
`park`, with the size), `restored`, `parked` with `snapshot: true`, and
`failed` with `op` `snapshot` or `restore`, which the `session_failed`
notification trigger covers.

#### Notifications

Sundae can push a notification to your phone when an agent session needs
you or has finished, through [ntfy](https://ntfy.sh): the public ntfy.sh
server or a self-hosted one. It is off by default.

```yaml
notifications:
  # What notifies (quote the key; see below).
  "on": [blocked, done, error]
  # Per-session rate limit. "0s" sends every change at once.
  debounce: 30s
  ntfy:
    enabled: true
    # The server's base URL, without the topic.
    url: https://ntfy.sh
    # 1 to 64 letters, digits, '-' or '_'.
    topic: my-unguessable-topic
    # Optional access token (tk_...), sent as a bearer token. The file is
    # read again for every message, so a rotated token needs no restart.
    token_file: /etc/sundae/secrets/ntfy-token
    # Optional URL opened when the notification is tapped.
    click_url: "https://git.example.com/{repo}?session={session}"
```

**What notifies.** `on` lists the triggers:

| Trigger | When | Priority |
|---------|------|----------|
| `blocked` | An agent waits for you: permission, a question, or a login (OSC 7501 `blocked`) | high |
| `done` | An agent finished a piece of work | default |
| `error` | An agent failed and stopped | default |
| `working`, `idle` | An agent started working, or came to rest. Off by default: noise on a phone | low |
| `session_failed` | Starting or ending a session failed, with the reason. Opt-in | default |
| `forge_failed` | A forge import, sync, push or listing failed. Opt-in. A repeat of the same failure (a scheduled sync failing every 10 minutes on an expired token) notifies once, and again only after a success | default |

The default is `[blocked, done, error]`. Agent states come from the
[Program Status Protocol (OSC 7501)](https://www.superlogical.com/rex/docs/build/program-status)
that agents report, as Sundae polls them (see "Session status" under
"OpenShell Agent Sessions"): a change is seen up to `sessions.status.poll_interval`
late, and a state that lasts less than that may not be seen at all.

**What a notification says.** The title has the state's glyph, the state and
the session, for example `▲ blocked · infra / claude-1`. Each record of a
session (`id`) is told apart: a child record shows as `infra / claude-1/build`.
The body is the agent's message, led by what a blocked agent waits for
(`Needs permission: Apply 3 to add?`), then the program (`app`) and progress
when reported. Tags carry the state, kind and app. Everything an agent writes
is untrusted: control characters and invisible formatting characters are
removed, the title is capped at 192 bytes and the body at 1 KB, and Markdown
is never enabled.

**Rate limit.** Per session record, the first notification goes out at once
and opens a `debounce` window. Changes inside the window are held and only the
latest is sent when it closes, unless it repeats what was already sent (same
state, kind and message) or is a state that does not notify. So a flapping
agent sends at most one notification per window, the final state is never
dropped (it is sent when the window closes, or at shutdown), and an agent
that was blocked and then lost its status (it exited, or the shell prompt
returned) is not reported as blocked again.

**Tap action.** `click_url` may use `{repo}`, `{session}`, `{sandbox}` and
`{user}`, replaced with URL-escaped values (`/` in a repository name is kept).
A notification without a value for a placeholder it uses, such as `{session}`
on a forge notification, is sent without the tap action. Any scheme ntfy
supports works, such as `https://` or an app's own scheme.

**Delivery.** Messages are posted as JSON to the server's root URL. A request
times out after 10 seconds. Network errors, timeouts, `429` and `5xx` are
retried with exponential backoff (from 1 second, honouring `Retry-After`) for
up to 2 minutes; other responses, such as `401`, are not retried. Redirects
are not followed. Sending happens off the event bus, so a slow or unreachable
ntfy server never holds up sessions; if it stays down long enough for 64
messages to queue, further messages are dropped and counted in
`sundae_notifications_total{result="dropped"}`.

**Checks.** While `ntfy.enabled` is false nothing in this section is checked
beyond its types. When it is true, `url` and `topic` are required, `url` must
be an `http(s)` base URL without credentials or the topic, every `on` entry
must be a trigger, `debounce` must not be negative, `click_url` may only use
the placeholders above, and `token_file` must be readable and not empty (a
relative path resolves against the data path). There is no inline `token`
key. `sundae config check` reports all of it.

**YAML 1.1 tools.** Quote the `on` key (`"on":`). Sundae reads a bare
`on:` correctly, but YAML 1.1 tools such as Helm read it as the boolean
`true`; Sundae then reports `unknown key notifications.true` with a hint,
and the Helm chart refuses to render.

**Topic privacy.** On a public server anyone who knows a topic can read and
publish to it. Use an unguessable topic name, or an access-controlled topic
with an access token in `token_file`. The topic itself is in the config file.

Each setting has a `SUNDAE_NOTIFICATIONS_*` environment variable, for
example `SUNDAE_NOTIFICATIONS_ON=blocked,done` or
`SUNDAE_NOTIFICATIONS_NTFY_TOKEN_FILE=...`.

**Event bus.** Notifications are one subscriber of an in-process event bus
that carries session status changes, session lifecycle steps (created,
resumed, ready, parked, exited, ended, failed, each naming the repository,
session name and sandbox) and forge outcomes. Each subscriber
has a bounded buffer; one that falls behind loses events rather than slowing
anything down, counted in `sundae_events_dropped_total{subscriber}`.
`sundae_events_published_total{kind}` counts what was published.

## Server Access

Sundae at its core manages your server authentication and authorization. Authentication verifies the identity of a user, while authorization determines their access rights to a repository.

To manage the server users, access, and repos, you can use the SSH command line interface.

Try `ssh localhost -i ~/.ssh/id_ed25519 -o IdentitiesOnly=yes -p 23231 help` for more info. Make sure
you use your key here.

> **Note** The `IdentitiesOnly` option is used to prevent SSH from using any
> other keys in your `~/.ssh` directory. This is useful when you have multiple
> keys, and you want to use a specific key for Sundae.

For ease of use, instead of specifying the key, port, and hostname every time
you SSH into Sundae, add your own Sundae instance entry to your SSH
config. For instance, to use `ssh sundae` instead of typing `ssh localhost -i
~/.ssh/id_ed25519 -o IdentitiesOnly=yes -p 23231`, we can define a `sundae` entry in our SSH config
file `~/.ssh/config`.

```conf
Host sundae
  HostName localhost
  Port 23231
  IdentityFile ~/.ssh/id_ed25519
  IdentitiesOnly yes
```

Now, we can do `ssh sundae` to SSH into Sundae. Since `git` is also aware of
this config, you can use `sundae` as the hostname for your clone commands.

```sh
git clone ssh://sundae/dotfiles
# make changes
# add & commit
git push origin main
```

> **Note** The `-i` and `-o` parts will be omitted in the examples below for brevity. You
> can add your server settings to your sshconfig for quicker access.

### Authentication

Everything that needs authentication is done using SSH. Make sure you have
added an entry for your Sundae instance in your `~/.ssh/config` file.

By default, Sundae gives read-only permission to anonymous connections to
any of the above protocols. This is controlled by two settings `anon-access`
and `allow-keyless`.

- `anon-access`: Defines the access level for anonymous users. Available
  options are `no-access`, `read-only`, `read-write`, and `admin-access`.
  Default is `read-only`.
- `allow-keyless`: Whether to allow connections that doesn't use keys to pass.
  Setting this to `false` would disable access to SSH keyboard-interactive,
  HTTP, and Git protocol connections. Default is `true`.

```sh
$ ssh -p 23231 localhost settings
Manage server settings

Usage:
  ssh -p 23231 localhost settings [command]

Available Commands:
  allow-keyless Set or get allow keyless access to repositories
  anon-access   Set or get the default access level for anonymous users

Flags:
  -h, --help   help for settings

Use "ssh -p 23231 localhost settings [command] --help" for more information about a command.
```

> **Note** These settings can only be changed by admins.

When `allow-keyless` is disabled, connections that don't use SSH Public Key
authentication will get denied. This means cloning repos over HTTP(s) or git://
will get denied.

Meanwhile, `anon-access` controls the access level granted to connections that
use SSH Public Key authentication but are not registered users. The default
setting for this is `read-only`. This will grant anonymous connections that use
SSH Public Key authentication `read-only` access to public repos.

`anon-access` is also used in combination with `allow-keyless` to determine the
access level for HTTP(s) and git:// clone requests.

#### Local/dev bootstrap

`anon-access` and `allow-keyless` are normally admin-only runtime settings,
which means scripting a fully open server from a cold start would otherwise
require booting with an admin key, waiting for the server to come up, then
SSHing in as that admin to run `settings` commands. For local development
tooling (e.g. standing up a throwaway Git remote for something like Argo CD
to pull from in a dev environment), you can instead set the `SUNDAE_ANON_ACCESS`
and `SUNDAE_ALLOW_KEYLESS` environment variables (or the equivalent
`anon_access`/`allow_keyless` `config.yaml` fields) to grant full,
unauthenticated access from the moment the server starts, with no admin key
and no `settings` command required:

```sh
SUNDAE_ANON_ACCESS=admin-access \
SUNDAE_ALLOW_KEYLESS=true \
  sundae serve
```

> **Warning** This grants anyone who can reach the server full admin access
> with no authentication at all. It is intended strictly for local/dev use on
> a machine or network you trust — never expose a server configured this way
> to an untrusted network. The server logs a warning banner on startup
> whenever this combination is active.
>
> Unlike the `settings` command, these overrides always take precedence over
> the database for as long as they're set — not just on first run. If you
> also try to tighten access via `ssh sundae settings ...` while an override is
> active, the change is saved but has no effect until the override is
> removed; the `settings` command will warn you when this happens.

#### SSH

Sundae doesn't allow duplicate SSH public keys for users. A public key can be associated with one user only. This makes SSH authentication simple and straight forward, add your public key to your Sundae user to be able to access Sundae.

#### HTTP

You can generate user access tokens through the SSH command line interface. Access tokens can have an optional expiration date. Use your access token as the basic auth user to access your Sundae repos through HTTP.

```sh
# Create a user token
ssh -p 23231 localhost token create 'my new token'
ss_1234abc56789012345678901234de246d798fghi

# Or with an expiry date
ssh -p 23231 localhost token create --expires-in 1y 'my other token'
ss_98fghi1234abc56789012345678901234de246d7
```

Now you can access to repos that require `read-write` access.

```sh
git clone http://ss_98fghi1234abc56789012345678901234de246d7@localhost:23232/my-private-repo.git my-private-repo
# Make changes and push
```

### Authorization

Sundae offers a simple access control. There are four access levels,
no-access, read-only, read-write, and admin-access.

`admin-access` has full control of the server and can make changes to users and repos.

`read-write` access gets full control of repos.

`read-only` can read public repos.

`no-access` denies access to all repos.

## User Management

Admins can manage users and their keys using the `user` command. Once a user is
created and has access to the server, they can manage their own keys and
settings.

To create a new user simply use `user create`:

```sh
# Create a new user
ssh -p 23231 localhost user create beatrice

# Add user keys
ssh -p 23231 localhost user add-pubkey beatrice ssh-rsa AAAAB3Nz...
ssh -p 23231 localhost user add-pubkey beatrice ssh-ed25519 AAAA...

# Create another user with public key
ssh -p 23231 localhost user create frankie '-k "ssh-ed25519 AAAATzN..."'

# Need help?
ssh -p 23231 localhost user help
```

Once a user is created, they get `read-only` access to public repositories.
They can also create new repositories on the server.

Users can manage their keys using the `pubkey` command:

```sh
# List user keys
ssh -p 23231 localhost pubkey list

# Add key
ssh -p 23231 localhost pubkey add ssh-ed25519 AAAA...

# Wanna change your username?
ssh -p 23231 localhost set-username yolo

# To display user info
ssh -p 23231 localhost info
```

## Repositories

You can manage repositories using the `repo` command.

```sh
# Run repo help
$ ssh -p 23231 localhost repo help
Manage repositories

Usage:
  ssh -p 23231 localhost repo [command]

Aliases:
  repo, repos, repository, repositories

Available Commands:
  blob         Print out the contents of file at path
  branch       Manage repository branches (bookmarks in jj repositories)
  collab       Manage collaborators
  create       Create a new repository
  delete       Delete a repository
  description  Set or get the description for a repository
  hide         Hide or unhide a repository
  import       Import a new repository from remote
  info         Get information about a repository
  list         List repositories
  mirror       Set or get a repository mirror property
  private      Set or get a repository private property
  profile      Set or get the default session profile of a repository
  project-name Set or get the project name for a repository
  rename       Rename an existing repository
  tag          Manage repository tags
  tree         Print repository tree at path

Flags:
  -h, --help   help for repo

Use "ssh -p 23231 localhost repo [command] --help" for more information about a command.
```

To use any of the above `repo` commands, a user must be a collaborator in the repository. More on this below.

### Creating Repositories

To create a repository, first make sure you are a registered user. Use the
`repo create <repo>` command to create a new repository:

```sh
# Create a new repository
ssh -p 23231 localhost repo create icecream

# Create a repo with description
ssh -p 23231 localhost repo create icecream '-d "This is an Ice Cream description"'

# ... and project name
ssh -p 23231 localhost repo create icecream '-d "This is an Ice Cream description"' '-n "Ice Cream"'

# I need my repository private!
ssh -p 23231 localhost repo create icecream -p '-d "This is an Ice Cream description"' '-n "Ice Cream"'

# Help?
ssh -p 23231 localhost repo create -h
```

Or you can add your Sundae server as a remote to any existing repo, given
you have write access, and push to remote:

```
git remote add origin ssh://localhost:23231/icecream
```

After you’ve added the remote just go ahead and push. If the repo doesn’t exist
on the server it’ll be created.

```
git push origin main
```

### Project templates and profiles

New repositories can start from a template and carry the session profile
their agent sessions use:

```sh
# Start from the go-service template, as a git repository
ssh -p 23231 localhost repo create icecream --template go-service --vcs git

# Store the session profile new sessions of the repository start from
ssh -p 23231 localhost repo create icecream --profile claude

# ... and start a session for yourself right away (detached)
ssh -p 23231 localhost repo create icecream --template go-service --start

# Get, set and remove a repository's profile
ssh -p 23231 localhost repo profile icecream
ssh -p 23231 localhost repo profile icecream shell
ssh -p 23231 localhost repo profile icecream --unset
```

Templates are declared in the config file only (there is no environment
form), by the server administrator:

```yaml
projects:
  templates:
    go-service:
      repo: templates/go-service   # an existing Sundae repository (required)
      ref: main                    # branch, tag or commit hash; empty: its HEAD
      subdir: ""                   # copy only this directory, as the new root
      description: Go service with a Containerfile and CI
      profile: claude              # a sessions.profiles entry (optional)
```

`--template` copies the template repository's tree at `ref` (under `subdir`,
when set) into the new repository as one initial commit on the branch its
`HEAD` names. None of the template's history comes along: only the objects of
that tree are copied. The commit is authored and committed by you, as
`<username> <username@host>`, where host is the host of `ssh.public_url`
(Sundae does not know email addresses); its message names the template,
the template repository and its commit. This works the same for `git` and
`jj` repositories; jj gives the commit a change id when it imports it. File
contents are copied unchanged: there are no placeholders. Git LFS objects are
not copied, so a template should not hold LFS files.

Creating from a template needs read access to the template repository, the
same access cloning it needs: a private template is only offered to, and
usable by, those who can read it. Unknown templates, a missing template repository,
ref or subdirectory, and missing access are reported before anything is
created; if the initial commit fails, the new repository is removed again.

A repository's session profile is kept in its git config (`session.profile`),
as forges keep their link; there is no database table. `--profile` sets it,
else the template's `profile`. `repo profile` reads it (anyone who can read
the repository) and sets or removes it (collaborators with write access); it
must name a `sessions.profiles` entry. A new session's profile is the first
of `session start --profile`, the repository's profile, the `sessions.repos`
match and none. `--start` needs OpenShell enabled, and starts the `main`
session as `session start --detach` does; a failed start leaves the
repository and says so.

`sundae config check` checks the templates: names are DNS labels, `repo` is a
repository name, `ref` is a ref or hash (no revision syntax such as `~1`),
`subdir` stays inside the repository, and `profile` is defined. It cannot
check that the repositories exist; that happens when one is used.

### Nested Repositories

Repositories can be nested too:

```sh
# Create a new nested repository
ssh -p 23231 localhost repo create charmbracelet/icecream

# Or ...
git remote add charm ssh://localhost:23231/charmbracelet/icecream
git push charm main
```

### Mirrors

You can also *import* repositories from any public remote. Use the `repo import` command.

```sh
ssh -p 23231 localhost repo import glow https://github.com/charmbracelet/glow
```

Use `--mirror` or `-m` to mark the repository as a *pull* mirror.

### Forges

Sundae can see your repositories on GitHub and GitLab, import them, and
keep them in sync without ever losing work you pushed only to Sundae.
Unlike a mirror, a forge repository is a normal, writable repository: sync
never rewrites or deletes a branch, and pushing back to the forge is a
separate, explicit command.

Forges are declared in the config file only (there is no runtime "add
forge"), and their tokens only ever come from a file:

```yaml
forges:
  - name: github                    # DNS label: commands, ref namespace, git config
    kind: github                    # github or gitlab
    token_file: /etc/sundae/secrets/github-token
    include: ["me/*", "my-org/**"]  # globs on owner/repo; empty = everything
    exclude: ["me/old-*"]
    sync: 10m                       # scheduled sync, at least 1m (default 10m)
    git_identity:                   # who commits in agent sessions of these repos
      name: Jane Doe
      email: 12345+jane@users.noreply.github.com
  - name: gitlab
    kind: gitlab
    url: https://gitlab.example.com # default https://github.com / https://gitlab.com
    token_file: /etc/sundae/secrets/gitlab-token
    prefix: work                    # import as work/<group>/<project> (default: name)
    # Optional: git over SSH instead of HTTPS (see "Git over SSH" below).
    ssh_key_file: /etc/sundae/secrets/gitlab-ssh/ssh-privatekey
    ssh_known_hosts_file: /etc/sundae/secrets/gitlab-ssh/known_hosts
    ssh_url: ssh://git@gitlab-ssh.example.com:2222   # default ssh://git@<url host>:22
```

- `url` is the forge's web address. The API is derived from it:
  `https://api.github.com` for github.com, `<url>/api/v3` for GitHub
  Enterprise Server, `<url>/api/v4` for GitLab.
- `token_file` is read again for every API call and git operation, so a
  rotated token is picked up without a restart. Discovery needs it: GitHub
  `repo` (or a fine-grained token with Contents read, plus write to push),
  GitLab `read_api` and `read_repository` (`write_repository` to push).
- `include` and `exclude` match the forge path case-insensitively. `*` does
  not cross `/`, `**` does. Write alternatives as separate entries.
- `prefix` must be unique and must not nest inside another forge's prefix, so
  two forges can never produce the same repository name.
- `git_identity` (optional, `name` and `email`) is the identity commits are
  made as in agent sessions of the forge's repositories; without it they use
  `sessions.git_identity`. It is server-wide like the token: every user's
  sessions of those repositories use it. See "Git identity" under
  [OpenShell Agent Sessions](#openshell-agent-sessions).
- `ssh_key_file`, `ssh_known_hosts_file` and `ssh_url` (optional) switch git
  operations to SSH; see "Git over SSH" below.
- Changing forges means editing the file and restarting (the Helm chart rolls
  the pod when its ConfigMap changes). `sundae config check` validates them,
  including that every token file, SSH key and known_hosts file is readable.

The forge host you declare is trusted: Sundae's SSRF guard for
user-supplied remotes (which refuses private addresses) does not apply to it,
so a self-hosted GitLab on a private address works by declaring its `url`.
What the forge API returns is not trusted: a clone URL that is not on the
declared scheme, host, port and base path is rejected, API pagination links
and redirects that leave the host are refused, and git never follows HTTP
redirects.

The `forge` command is only there when at least one forge is configured.
Listing, importing and syncing are for server admins; pushing needs write
access to the repository.

```sh
ssh -p 23231 localhost forge ls [--json]                         # forges, repo counts, last sync, last error
ssh -p 23231 localhost forge repos github [--all] [--json]       # forge repos, imported or not, ahead/behind
ssh -p 23231 localhost forge import github me/tool me/other      # import some
ssh -p 23231 localhost forge import github --all                 # import everything include/exclude let through
ssh -p 23231 localhost forge sync [github [github/me/tool]]      # sync now (also runs every `sync` interval)
ssh -p 23231 localhost forge push github/me/tool [main] [--force-with-lease]
```

**Naming.** `owner/repo` on a forge is imported as `<prefix>/owner/repo`, e.g.
`github/me/tool`; GitLab subgroups nest the same way. A name already taken by
a repository that was not imported from the same forge path is a collision:
that repository is left untouched and its import fails. Importing an
already-imported repository does nothing. A repository private (or GitLab
internal) on the forge is private on Sundae. Imported repositories get the
server's default VCS kind, `jj`, like any other new repository; pass `--vcs
git` to `forge import` for git presentation.

**Sync.** Upstream branches and tags are fetched into the forge's own
namespace, `refs/remotes/<forge>/heads/*` and `refs/remotes/<forge>/tags/*`,
which the TUI and web views do not list and pushes to Sundae cannot write.
Pruning happens only there. Then, for each upstream branch:

| Local branch | Sync |
| --- | --- |
| missing | created at upstream |
| strictly behind upstream (no local-only commits) | fast-forwarded |
| equal, ahead, or diverged | left alone; ahead/behind reported |
| exists only on Sundae, or deleted upstream | left alone, never deleted |

Upstream tags missing locally are created; an existing tag is never moved.
Every update is a compare-and-swap, so a push landing meanwhile wins. A
branch deleted on Sundae but still upstream is created again by the next
sync.

**Push.** `forge push` pushes one branch (default: the branch `HEAD` points
at) as a fast-forward. If the forge has commits the branch lacks, as of the
last sync, the push is refused and the number of missing commits reported;
if the forge moved after the last sync, the forge refuses it and you are told
to sync first. `--force-with-lease` overwrites the forge branch, but only if
it is still where the last sync saw it.

**Where the link lives.** Each imported repository keeps its link in its own
git config, the way mirrors keep their remote: a remote named after the forge
(`remote.<forge>.url` and fetch refspecs into the namespace) and a `[forge]`
section with `name`, `path`, `defaultbranch`, `lastsync` and `lasterror`.
There is no database table. The token is never written to the repository: git
gets it as an `Authorization: Basic` header scoped to the forge URL through
`GIT_CONFIG_*` environment variables (user name `x-access-token` for GitHub,
`oauth2` for GitLab; both take the token as the password).

**Git over SSH.** By default import, sync and push use HTTPS with the token.
With `ssh_key_file` set they use SSH with that key instead (a deploy key, or
a key on a user account); the API (discovery, `forge repos`) still uses
`token_file`. The key must have no passphrase.

- The remote is built from the forge path and the SSH endpoint,
  `<ssh_url>/<owner>/<repo>.git`, never taken from the API. The endpoint is
  `ssh_url` (`ssh://[user@]host[:port]`, no path; user `git` when omitted),
  defaulting to `ssh://git@<url host>` on port 22. Use it when SSH is on
  another host or port, e.g. `ssh://git@ssh.github.com:443`,
  `ssh://git@altssh.gitlab.com:443`, or a self-hosted GitLab's SSH host.
- Host keys are always checked strictly; an unknown or changed key fails the
  operation. `ssh_known_hosts_file` is a known_hosts file for the SSH host
  (`host` for port 22, `[host]:port` otherwise; hashed entries work). It is
  optional for github.com, ssh.github.com, gitlab.com and altssh.gitlab.com,
  whose published keys are built in, and required for any other host.
- Every operation reads the key and known_hosts again (rotation needs no
  restart), copies them into a private temporary directory (key mode
  `0600`, so the original's owner and mode do not matter to ssh) and removes
  it afterwards. git runs `ssh -F /dev/null` with only that key (no agent),
  only that known_hosts, `StrictHostKeyChecking=yes` and `BatchMode=yes`;
  no HTTP credentials are set.
- Adding, changing or removing the SSH settings takes effect on the next sync
  or push of each repository: `remote.<forge>.url` is rewritten to the new
  transport or endpoint before fetching (back to HTTPS, it becomes the
  listed clone URL, else `<url>/<path>.git`). The ref namespace is
  unchanged.
- `forge ls --json` shows each forge's `transport` (`https` or `ssh`) and
  `ssh_url`. Sundae needs egress to the SSH host and port.

Git LFS objects of forge repositories are not fetched.

### Deleting Repositories

You can delete repositories using the `repo delete <repo>` command.

```sh
ssh -p 23231 localhost repo delete icecream
```

### Renaming Repositories

Use the `repo rename <old> <new>` command to rename existing repositories.

```sh
ssh -p 23231 localhost repo rename icecream vanilla
```

### Repository Collaborators

Sometimes you want to restrict write access to certain repositories. This can
be achieved by adding a collaborator to your repository.

Use the `repo collab <command> <repo>` command to manage repo collaborators.

```sh
# Add collaborator to sundae
ssh -p 23231 localhost repo collab add sundae frankie

# Add collaborator with a specific access level
ssh -p 23231 localhost repo collab add sundae beatrice read-only

# Remove collaborator
ssh -p 23231 localhost repo collab remove sundae beatrice

# List collaborators
ssh -p 23231 localhost repo collab list sundae
```

### Repository Metadata

You can also change the repo's description, project name, whether it's private,
etc using the `repo <command>` command.

```sh
# Set description for repo
ssh -p 23231 localhost repo description icecream "This is a new description"

# Hide repo from listing
ssh -p 23231 localhost repo hidden icecream true

# List repository info (branches, tags, description, etc)
ssh -p 23231 localhost repo icecream info
```

To make a repository private, use `repo private <repo> [true|false]`. Private
repos can only be accessed by admins and collaborators.

```sh
ssh -p 23231 localhost repo private icecream true
```

### Jujutsu (jj) Repositories

Repositories have a VCS kind, `jj` or `git`, set with `--vcs` when you create
or import them. **When `--vcs` is omitted, new and imported repositories are
`jj`.** Repositories that existed before this option was added are `git`. The
kind is set once and can't be changed afterwards.

```sh
# jj is the default
ssh -p 23231 localhost repo create icecream
# jj git clone ssh://localhost:23231/icecream.git

# Plain git presentation
ssh -p 23231 localhost repo create icecream --vcs git
```

Storage and transport are always a bare Git repository. jj has no wire
protocol of its own, so you push and fetch with `jj git push` and
`jj git fetch`, and the server never runs `jj`. The kind only changes how the
repository is presented:

- the clone hint is `jj git clone <url>` in the CLI, TUI and the `go-get` page;
- branches are called bookmarks (`repo bookmark` is an alias of `repo branch`,
  and the TUI tab is "Bookmarks");
- commits show their jj change ID next to the short commit ID in the TUI log,
  and `repo commit` prints the full change ID on its own line. The ID is read from the `change-id` header jj writes
  into pushed commits; commits without it fall back to the short hash.

```sh
jj git clone ssh://localhost:23231/icecream.git
cd icecream
jj describe -m "first commit"
jj bookmark create main -r @
jj git push --bookmark main
```

HTTP responses for repository routes carry an `X-Sundae-VCS` header (`git`
or `jj`) for callers that are authorized to read the repository.

### Repository Branches & Tags

Use `repo branch` and `repo tag` to list, and delete branches or tags. You can
also use `repo branch default` to set or get the repository default branch.

### Repository Tree

To print a file tree for the project, just use the `repo tree` command along with
the repo name as the SSH command to your Sundae server:

```sh
ssh -p 23231 localhost repo tree sundae
```

You can also specify the sub-path and a specific reference or branch.

```sh
ssh -p 23231 localhost repo tree sundae server/config
ssh -p 23231 localhost repo tree sundae main server/config
```

From there, you can print individual files using the `repo blob` command:

```sh
ssh -p 23231 localhost repo blob sundae cmd/sundae/main.go
```

You can add the `-c` flag to enable syntax coloring and `-l` to print line
numbers:

```sh
ssh -p 23231 localhost repo blob sundae cmd/sundae/main.go -c -l

```

Use `--raw` to print raw file contents. This is useful for dumping binary data.

### Repository webhooks

Sundae supports repository webhooks using the `repo webhook` command. You
can create and manage webhooks for different repository events such as _push_,
_collaborators_, and _branch_tag_create_ events.

```
Manage repository webhooks

Usage:
  ssh -p 23231 localhost repo webhook [command]

Aliases:
  webhook, webhooks

Available Commands:
  create      Create a repository webhook
  delete      Delete a repository webhook
  deliveries  Manage webhook deliveries
  list        List repository webhooks
  update      Update a repository webhook

Flags:
  -h, --help   help for webhook
```

## The Sundae TUI

Sundae TUI is mainly used to browse repos over SSH. You can also use it to
browse local repositories with `sundae browse` or running `sundae` within a Git
repository.

```sh
ssh localhost -p 23231
```

It's also possible to “link” to a specific repo:

```sh
ssh -p 23231 localhost -t sundae
```

You can copy text to your clipboard over SSH. For instance, you can press
<kbd>c</kbd> on the highlighted repo in the menu to copy the clone command
[^osc52].

The home page has tabs: <kbd>1</kbd>–<kbd>n</kbd> jump to one, <kbd>tab</kbd>
cycles. When OpenShell is enabled and you are signed in with a key, a
**Sessions** tab lists your agent sessions, what needs you first, and the
header counts the ones blocked, errored, done and working. In it:
<kbd>enter</kbd> attaches (detach with <kbd>~.</kbd> to come back to the same
row), <kbd>n</kbd> starts a session (pick any number of repositories with
<kbd>space</kbd>, a name and a profile), <kbd>a</kbd> and <kbd>R</kbd> add
repositories to the highlighted session or remove some, <kbd>x</kbd> ends it,
<kbd>z</kbd> parks or resumes it, <kbd>c</kbd> copies its `session attach`
command, <kbd>/</kbd> filters, and <kbd>e</kbd> switches to the **Signals**
pane: the `session signals` feed, live, newest first (<kbd>a</kbd> shows only
what needs you, <kbd>g</kbd> goes back to the top and follows new signals).
When the OpenShell gateway is unreachable the tab says so and <kbd>r</kbd>
retries; the other tabs keep working.

On **Repositories**, each title line carries badges: your sessions of the
repository (the most urgent state and how many), and, when forges are
configured, where it comes from (`local`, or the forge with `↑n ↓n` commits
ahead and behind its default branch as of the last sync). They follow status
changes and forge syncs live and drop off on narrow terminals. <kbd>s</kbd>
opens the new-session form with the highlighted repository picked,
<kbd>D</kbd> deletes it after a confirm (with write access, as `repo
delete`), and <kbd>n</kbd> opens the
new-repository form (for whoever may run `repo create`): name, description,
VCS, template (those you can read), session profile, private, and whether to
start a session. On success the new repository is selected; with "Start
session" the Sessions tab starts one, named as its own new-session form
names them, and attaches to it. Inside a repository, a
**Sessions** tab after Tags lists that repository's sessions and signals with
the same keys, and the status bar ends with the attention summary.

When forges are configured, server admins (the same people who may run
`forge import` and `forge sync`) get a **Forges** tab: every forge with its
repositories, imported (`✓`, with ahead/behind and last sync) or not.
<kbd>i</kbd> imports the highlighted repository, <kbd>I</kbd> imports all
shown (the <kbd>/</kbd> filter narrows them); both ask whether to import
them as jj or git repositories,
<kbd>r</kbd> syncs the forge on its header or the repository on its row,
<kbd>p</kbd> pushes the repository's branch to the forge after a confirm
naming it (fast-forward only; a refusal says why), and <kbd>enter</kbd> opens
the imported repository. Imports, syncs and pushes run in the background with
their progress and outcome in the rows; syncs from the schedule or the command
line show up live.

[^osc52]:
    Copying over SSH depends on your terminal support of OSC52. Refer to
    [go-osc52](https://github.com/aymanbagabas/go-osc52) for more information.

## Hooks

Sundae supports git server-side hooks `pre-receive`, `update`,
`post-update`, and `post-receive`. This means you can define your own hooks to
run on repository push events. Hooks can be defined as a per-repository hook,
and/or global hooks that run for all repositories.

You can find per-repository hooks under the repository `hooks` directory.

Globs hooks can be found in your `SUNDAE_DATA_PATH` directory under
`hooks`. Defining global hooks is useful if you want to run CI/CD for example.

Here's an example of sending a message after receiving a push event. Create an
executable file `<data path>/hooks/update`:

```sh
#!/bin/sh
#
# An example hook script to echo information about the push
# and send it to the client.

refname="$1"
oldrev="$2"
newrev="$3"

# Safety check
if [ -z "$GIT_DIR" ]; then
        echo "Don't run this script from the command line." >&2
        echo " (if you want, you could supply GIT_DIR then run" >&2
        echo "  $0 <ref> <oldrev> <newrev>)" >&2
        exit 1
fi

if [ -z "$refname" -o -z "$oldrev" -o -z "$newrev" ]; then
        echo "usage: $0 <ref> <oldrev> <newrev>" >&2
        exit 1
fi

# Check types
# if $newrev is 0000...0000, it's a commit to delete a ref.
zero=$(git hash-object --stdin </dev/null | tr '[0-9a-f]' '0')
if [ "$newrev" = "$zero" ]; then
        newrev_type=delete
else
        newrev_type=$(git cat-file -t $newrev)
fi

echo "Hi from Sundae update hook!"
echo
echo "RefName: $refname"
echo "Change Type: $newrev_type"
echo "Old SHA1: $oldrev"
echo "New SHA1: $newrev"

exit 0
```

Now, you should get a message after pushing changes to any repository.

## A note about RSA keys

Unfortunately, due to a shortcoming in Go’s `x/crypto/ssh` package, Sundae
does not currently support access via new SSH RSA keys: only the old SHA-1
ones will work.

Until we sort this out you’ll either need an SHA-1 RSA key or a key with
another algorithm, e.g. Ed25519. Not sure what type of keys you have?
You can check with the following:

```sh
$ find ~/.ssh/id_*.pub -exec ssh-keygen -l -f {} \;
```

If you’re curious about the inner workings of this problem have a look at:

- https://github.com/golang/go/issues/37278
- https://go-review.googlesource.com/c/crypto/+/220037
- https://github.com/golang/crypto/pull/197

## License

Sundae is released under the [MIT license](LICENSE).

Sundae is a fork of [Soft Serve](https://github.com/charmbracelet/soft-serve),
Copyright (c) 2021-2023 Charmbracelet, Inc, also MIT licensed. Charm's
copyright notice is kept in [LICENSE](LICENSE) alongside the fork's.

# Soft Serve

<p>
    <img style="width: 451px" src="https://stuff.charm.sh/soft-serve/soft-serve-header.png?0" alt="A nice rendering of some melting ice cream with the words ‘Charm Soft Serve’ next to it"><br>
    <a href="https://github.com/charmbracelet/soft-serve/releases"><img src="https://img.shields.io/github/release/charmbracelet/soft-serve.svg" alt="Latest Release"></a>
    <a href="https://pkg.go.dev/github.com/charmbracelet/soft-serve?tab=doc"><img src="https://godoc.org/github.com/golang/gddo?status.svg" alt="GoDoc"></a>
    <a href="https://github.com/charmbracelet/soft-serve/actions"><img src="https://github.com/charmbracelet/soft-serve/workflows/build/badge.svg" alt="Build Status"></a>
    <a href="https://nightly.link/charmbracelet/soft-serve/workflows/nightly/main"><img src="https://shields.io/badge/-Nightly%20Builds-orange?logo=hackthebox&logoColor=fff&style=appveyor"/></a>
</p>

<picture>
  <source media="(max-width: 750px)" srcset="https://github.com/charmbracelet/soft-serve/assets/42545625/c754c746-dc4c-44a6-9c39-28649264cbf2">
  <source media="(min-width: 750px)" width="750" srcset="https://github.com/charmbracelet/soft-serve/assets/42545625/c754c746-dc4c-44a6-9c39-28649264cbf2">
  <img src="https://github.com/charmbracelet/soft-serve/assets/42545625/c754c746-dc4c-44a6-9c39-28649264cbf2" alt="Soft Serve screencast">
</picture>

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

## Where can I see it?

Just run `ssh git.charm.sh` for an example. You can also try some of the following commands:

```bash
# Jump directly to a repo in the TUI
ssh git.charm.sh -t soft-serve

# Print out a directory tree for a repo
ssh git.charm.sh repo tree soft-serve

# Print a specific file
ssh git.charm.sh repo blob soft-serve cmd/soft/main.go

# Print a file with syntax highlighting and line numbers
ssh git.charm.sh repo blob soft-serve cmd/soft/main.go -c -l
```

Or you can use Soft Serve to browse local repositories using `soft browse
[directory]` or running `soft` within a Git repository.

## Installation

Soft Serve is a single binary called `soft`. You can get it from a package
manager:

```bash
# macOS or Linux
brew install charmbracelet/tap/soft-serve

# Windows (with Winget)
winget install charmbracelet.soft-serve

# Arch Linux
pacman -S soft-serve

# Nix
nix-env -iA nixpkgs.soft-serve

# Debian/Ubuntu
sudo mkdir -p /etc/apt/keyrings
curl -fsSL https://repo.charm.sh/apt/gpg.key | sudo gpg --dearmor -o /etc/apt/keyrings/charm.gpg
echo "deb [signed-by=/etc/apt/keyrings/charm.gpg] https://repo.charm.sh/apt/ * *" | sudo tee /etc/apt/sources.list.d/charm.list
sudo apt update && sudo apt install soft-serve

# Fedora/RHEL
echo '[charm]
name=Charm
baseurl=https://repo.charm.sh/yum/
enabled=1
gpgcheck=1
gpgkey=https://repo.charm.sh/yum/gpg.key' | sudo tee /etc/yum.repos.d/charm.repo
sudo yum install soft-serve
```

You can also download a binary from the [releases][releases] page. Packages are
available in Alpine, Debian, and RPM formats. Binaries are available for Linux,
macOS, and Windows.

[releases]: https://github.com/charmbracelet/soft-serve/releases

Or just install it with `go`:

```bash
go install github.com/charmbracelet/soft-serve/cmd/soft@latest
```

A [Docker image][docker] is also available.

[docker]: https://github.com/charmbracelet/soft-serve/blob/main/docker.md

## Setting up a server

Make sure `git` is installed, then run `soft serve`. That’s it.

This will create a `data` directory that will store all the repos, ssh keys,
and database.

By default, program configuration is stored within the `data` directory. But,
this can be overridden by setting a custom path to a config file with `SOFT_SERVE_CONFIG_LOCATION`
that is pre-created. If a config file pointed to by `SOFT_SERVE_CONFIG_LOCATION`,
the default location within the `data` dir is used for generating a default config.

To change the default data path use `SOFT_SERVE_DATA_PATH` environment variable.

```sh
SOFT_SERVE_DATA_PATH=/var/lib/soft-serve soft serve
```

When you run Soft Serve for the first time, make sure you have the
`SOFT_SERVE_INITIAL_ADMIN_KEYS` environment variable is set to your ssh
authorized key. Any added key to this variable will be treated as admin with
full privileges.

Using this environment variable, Soft Serve will create a new `admin` user that
has full privileges. You can rename and change the user settings later.

If you'd like a repository to exist the moment the server finishes booting,
with no human logging in to run `repo create`, set `SOFT_SERVE_DEFAULT_REPO`
to a repository name. This is useful for GitOps tools like ArgoCD that need a
git remote to point at as part of their own bootstrap (e.g. from a Helm chart
or Terraform apply):

```sh
SOFT_SERVE_INITIAL_ADMIN_KEYS="$(cat ~/.ssh/id_ed25519.pub)" \
SOFT_SERVE_DEFAULT_REPO=gitops \
  soft serve
```

The repository is created empty and public, exactly as if a human had run
`repo create gitops` with no flags; only its *existence* is guaranteed on
boot, not its reachability, which is still governed entirely by the
`anon-access`/`allow-keyless` settings described below. Booting again against
the same data directory is a no-op if the repository already exists.

Check out [Systemd][systemd] on how to run Soft Serve as a service using
Systemd. Soft Serve packages in our Apt/Yum repositories come with Systemd
service units.

[systemd]: https://github.com/charmbracelet/soft-serve/blob/main/systemd.md

### Server Configuration

Once you start the server for the first time, the settings will be in
`config.yaml` under your data directory. The default `config.yaml` is
self-explanatory and will look like this:

```yaml
# Soft Serve Server configurations

# The name of the server.
# This is the name that will be displayed in the UI.
name: "Soft Serve"

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
  key_path: "ssh/soft_serve_host"

  # The path to the SSH server's client private key.
  # This key will be used to authenticate the server to make git requests to
  # ssh remotes.
  client_key_path: "ssh/soft_serve_client"

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
  data_source: "soft-serve.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"

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
#    token_file: /etc/soft-serve/secrets/github-token
#    include: ["me/*"]
#    sync: 10m

# Agent session status, parking and profiles. See "OpenShell Agent Sessions".
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

# Notifications about agent sessions, through ntfy. See "Notifications".
#notifications:
#  "on": [blocked, done, error]
#  debounce: 30s
#  ntfy:
#    enabled: true
#    url: https://ntfy.sh
#    topic: my-unguessable-topic
#    token_file: /etc/soft-serve/secrets/ntfy-token

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
settings environment variables start with `SOFT_SERVE_` followed by the setting
name all in uppercase. Here are some examples:

- `SOFT_SERVE_NAME`: The name of the server that will appear in the TUI
- `SOFT_SERVE_SSH_LISTEN_ADDR`: SSH listen address
- `SOFT_SERVE_SSH_KEY_PATH`: SSH host key-pair path
- `SOFT_SERVE_HTTP_LISTEN_ADDR`: HTTP listen address
- `SOFT_SERVE_HTTP_PUBLIC_URL`: HTTP public URL used for cloning
- `SOFT_SERVE_GIT_MAX_CONNECTIONS`: The number of simultaneous connections to git daemon
- `SOFT_SERVE_ANON_ACCESS`: Overrides the `anon-access` setting (see [Authentication](#authentication))
- `SOFT_SERVE_ALLOW_KEYLESS`: Overrides the `allow-keyless` setting (see [Authentication](#authentication))
- `SOFT_SERVE_DEFAULT_REPO`: Repository name to create on boot if missing
- `SOFT_SERVE_FORGES_0_NAME`, `SOFT_SERVE_FORGES_0_KIND`, ...: The first entry of `forges` (see [Forges](#forges)); list values are comma-separated
- `SOFT_SERVE_NOTIFICATIONS_ON`, `SOFT_SERVE_NOTIFICATIONS_NTFY_ENABLED`, ...: The `notifications` section (see [Notifications](#notifications)); `on` is comma-separated

#### Database Configuration

Soft Serve supports both SQLite and Postgres for its database. Like all other Soft Serve settings, you can change the database _driver_ and _data source_ using either `config.yaml` or environment variables. The default config uses SQLite as the default database driver.

To use Postgres as your database, first create a Soft Serve database:

```sh
psql -h<hostname> -p<port> -U<user> -c 'CREATE DATABASE soft_serve'
```

Then set the database _data source_ to point to your Postgres database. For instance, if you're running Postgres locally, using the default user `postgres` and using a database name `soft_serve`, you would have this config in your config file or environment variable:

```
db:
  driver: "postgres"
  data_source: "postgres://postgres@localhost:5432/soft_serve?sslmode=disable"
```

Environment variables equivalent:

```sh
SOFT_SERVE_DB_DRIVER=postgres \
SOFT_SERVE_DB_DATA_SOURCE="postgres://postgres@localhost:5432/soft_serve?sslmode=disable" \
soft serve
```

You can specify a database connection password in the _data source_ url. For example, `postgres://myuser:dbpass@localhost:5432/my_soft_serve_db`.

#### LFS Configuration

Soft Serve supports both Git LFS [HTTP](https://github.com/git-lfs/git-lfs/blob/main/docs/api/README.md) and [SSH](https://github.com/git-lfs/git-lfs/blob/main/docs/proposals/ssh_adapter.md) protocols out of the box, there is no need to do any extra set up.

Use the `lfs` config section to customize your Git LFS server.

> **Note**: The pure-SSH transfer is disabled by default.

#### OpenShell Agent Sessions

Soft Serve can start and resume agent sessions in
[NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell) sandboxes. Each
session has its own sandbox, with the repository cloned into
`/sandbox/<repo>`. The session's program (a shell by default, or an agent)
runs there inside an [osewa](https://github.com/rjyo/osewa) session, so it
keeps running after your SSH connection closes. Your SSH terminal is bridged
to it:

```sh
ssh -t -p 23231 localhost session start icecream                  # create or resume the "main" session, then attach
ssh -t -p 23231 localhost session start icecream --name review    # a second session of the same repository
ssh -t -p 23231 localhost session start icecream --profile claude # create from a profile (see below)
ssh -t -p 23231 localhost session start icecream --image my/img   # image used only when the sandbox is first created
ssh    -p 23231 localhost session start icecream -n bg --detach   # create or resume without attaching (no -t needed)
ssh -t -p 23231 localhost session attach icecream --name review   # attach to an existing session (resuming it if parked)
ssh    -p 23231 localhost session ls                              # your sessions, with what each agent is doing
ssh    -p 23231 localhost session ls --json                       # the same as JSON
ssh    -p 23231 localhost session signals                         # what your sessions reported, newest first
ssh    -p 23231 localhost session signals --attention --follow    # keep printing what needs you (Ctrl-C to stop)
ssh    -p 23231 localhost session park icecream --name review     # suspend that session's sandbox now
ssh    -p 23231 localhost session resume icecream --name review   # resume it without attaching
ssh    -p 23231 localhost session end icecream --name review      # delete that session's sandbox and revoke its token
```

**Named sessions.** A user can have several sessions per repository, told
apart by `--name` (1 to 32 lowercase letters, digits and hyphens; `main` when
omitted). Each is its own sandbox: `main` keeps the name `soft-<user>-<repo>`
sessions had before names existed, so existing sandboxes are its `main`
session, and other names get `soft-<user>-<repo>-<name>--<hash>` (cut to 63
characters). The sandbox records the user, repository, session name and
profile as annotations; Soft Serve stores no session state of its own.
`session end` deletes one session and leaves the others.

**Detach and re-attach.** `session start` and `session attach` need a
terminal, so pass `-t`. Detach with `~.` after a newline (osewa's escape), or
just close the connection: either ends only the bridge, and the program keeps
running. `session start` or `session attach` re-attaches to the same program,
starting it again only if it has exited or the sandbox was restarted. The
attach exits with the program's exit status when the program ends. A new
attach takes over from an earlier one, which stays connected as a read-only
viewer (`Ctrl-T` takes control back). You need read access to the repository,
and anonymous users can't use sessions. `session attach` never creates a
sandbox.

The feature is off by default. When it is disabled, `session` is an unknown
command. To enable it:

```yaml
openshell:
  enabled: true
  # The OpenShell gateway gRPC address (http:// = plaintext, https:// or no scheme = TLS).
  gateway_address: "https://openshell.openshell.svc.cluster.local:8080"
  workspace: "default"
  # Sandbox image when no profile sets one. It must provide git and jj
  # (deploy/sandbox/Containerfile builds one, with osewa).
  image: "ghcr.io/example/agent-sandbox:latest"
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
  clone_url: "http://soft-serve-http.openshell.svc.cluster.local:23232"
  # Lifetime of the git token in a sandbox; renewed on every start and attach.
  token_ttl: "24h"
  # The program a session runs when no profile sets one, in /sandbox/<repo>.
  command: "bash -l"

sessions:
  osewa:
    # Pinned osewa builds uploaded to sandboxes whose image lacks osewa.
    binary_dir: "/usr/local/share/soft-serve/osewa"
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
  # The profile for sessions started without --profile, by repository glob.
  repos:
    "infra/*":
      profile: claude
```

The `openshell` settings and `sessions.osewa`, `sessions.status`,
`sessions.suspend` and `sessions.signals` can also be set with
`SOFT_SERVE_OPENSHELL_*` and `SOFT_SERVE_SESSIONS_OSEWA_*`,
`SOFT_SERVE_SESSIONS_STATUS_*`, `SOFT_SERVE_SESSIONS_SUSPEND_*` and
`SOFT_SERVE_SESSIONS_SIGNALS_*` environment variables, for example
`SOFT_SERVE_OPENSHELL_ENABLED=true` or
`SOFT_SERVE_SESSIONS_SUSPEND_AFTER=1h`; `sessions.profiles` and
`sessions.repos` only in the file. `status.poll_interval` must be at least
`1s`, and `suspend.after` must not be shorter than it. `signals.keep` is
between 1 and 100000 (default 1000). When OpenShell is
enabled, `gateway_address` and `image` are required, and a `token_file`, if
set, must be readable and not empty. Setting both `token` and `token_file` is
an error. `soft config check` reports every problem, naming the key.

**Profiles.** A new session is created from `--profile`, else from the
`sessions.repos` entry matching the repository (`*` does not cross `/`, `**`
does, case-insensitive; when several match, the one with the most
non-wildcard characters wins), else from `openshell.image` and
`openshell.command`. `--image` overrides the image. A session keeps the
profile it was created from: `--profile` and `--image` are ignored, with a
warning, when it already exists. Profile names are DNS labels; every
`repos` entry must name a defined profile.

**osewa in the sandbox.** Soft Serve pins one osewa release (its protocol is a
draft). On every start and attach it:

1. uses `osewa` from the image's `PATH` if it is the pinned version, else
   uploads the pinned Linux build from `sessions.osewa.binary_dir` (checked
   against the release's SHA-256 here and again in the sandbox) to
   `/sandbox/.local/lib/soft-serve/`. The Soft Serve image built from
   `Containerfile` carries both builds; the sandbox image from
   `deploy/sandbox/Containerfile` has osewa already. The OpenShell Go SDK has
   no file transport yet, so the upload goes through exec calls, about 55 of
   them; it happens once per sandbox.
2. starts `osewa server` on `127.0.0.1:<port>`, loopback only, with QUIC and
   port forwarding off, unless it is running. It runs detached from the exec
   that started it, until the sandbox stops.
3. once per sandbox, gives the bridge an osewa client identity
   (`~/.config/soft-serve/osewa-client`) and enrols it with that server.
4. starts the session's program in an osewa session in `/sandbox/<repo>`
   through `/bin/sh -l`, unless the one recorded in
   `~/.config/soft-serve/osewa-session` is running.

The bridge is `osewa attach` against that loopback server. Sandbox images
other than the one in `deploy/sandbox` need `sh`, `git`, `gzip`, `base64`
and `sha256sum` (and `flock`, used when present). osewa ends a session that
has had nobody attached and no output for 24 hours.

**Session status.** Agents report what they are doing with the
[Program Status Protocol (OSC 7501)](https://www.superlogical.com/rex/docs/build/program-status);
osewa keeps each session's latest report. While OpenShell is enabled, Soft
Serve reads it from every running session: every `sessions.status.poll_interval`
(default 10s) it lists the sandboxes it created and, in each Ready one, runs
`osewa local ls` (one exec per sandbox, at most 8 at a time, each cut off
after 15 seconds) and finds the session's program by the id recorded in the
sandbox. It keeps, in memory only, the state, kind, progress, program, title
and message, how many clients are attached, how long osewa has seen no
output, whether the program exited and with what status, when the state last
changed, and when and why the last poll failed. Each change goes on the event
bus as a status event (so `notifications` see it), and a program exiting as
an `exited` lifecycle event. The first poll after Soft Serve starts only
records what it finds, so a restart does not notify again about every
session; "last changed" then counts from the restart.

`session ls` shows it, most urgent first: blocked, error, done, working,
idle, then sessions that never reported:

```
Repository  Name    Phase      State                           Message                   Attached  Changed
infra       claude  Suspended  ▲ blocked · permission · parked  Apply 3 to add, 1 to cha…  0         4m
soft-serve  main    Ready      ● working 40%                   Running go test ./pkg/…   1         now
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
13:58:40  ✓ done                  soft-serve / forge-sync       Implemented GitHub discovery
13:12:00  ○ idle · parked         dotfiles / main               parked while idle
12:50:19  + created               soft-serve / tui              sandbox created
```

A status row has the state's glyph and word, its progress and blocked kind,
and the message as in `session ls` (sanitised; with a PTY, `ssh -t`, each row
is cut to the terminal's width, otherwise not cut). A child record would show
its id after the session name (`soft-serve / tui/lint`). A lifecycle row
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
every report (roadmap item C4), which is not built yet. If Soft Serve falls
behind the event bus, a `! missed` row (JSON `"type": "gap"`) marks where
events were lost, rather than leaving a silent hole; so does a `--follow`
reader that fell further behind than the feed keeps.

**Parking idle sessions.** A session nobody uses is parked: its sandbox is
suspended through OpenShell, keeping its memory where the compute driver can,
so it uses no CPU until someone comes back. A session is parked once, for the
whole of `sessions.suspend.after` (default 30m), nobody has been attached
(osewa counts no attached client, and Soft Serve has no bridge open on it)
and it has not reported `working`. Every other state counts as not working,
so an agent waiting on you (`blocked`) is parked too and still reads
`▲ blocked · parked`. A program that never reports a status must also have
been quiet that long (osewa has seen no output from it), so a plain shell
running a long build is not frozen. A program that has exited runs nothing,
so its session is parked like an idle one; the next attach starts it again
either way. Soft Serve also counts its own bridges: from the moment `session
start` or `session attach` begins preparing a session until its bridge
closes, however it closes, the session is not parked. The idle window starts
again whenever Soft Serve sees a client attached, a bridge open or close, a
`working` report, or the sandbox come back up, and when Soft Serve starts.
Each park is a `parked` lifecycle event (`op` `auto`, or `park` for `session
park`).

`session start`, `session attach` and `session resume` resume a parked
session (and say so), then carry on as usual; that is a `resumed` event. A
resume asks OpenShell to boot the sandbox cleanly if its memory cannot be
restored (`RestartIfRestoreFails`): the session stays usable with what the
backend kept of `/sandbox`, rather than failing until it is ended. osewa's
server and the program are gone after such a restart, so Soft Serve starts
them again, as for an exited program, and tells you: `Started claude again:
it was not running when the session resumed (it had exited, or the sandbox
could not restore its memory and restarted cleanly)`. Whatever the program
held in memory, such as an agent's conversation, is lost then.

`session park` parks a session at once (not while a bridge is open on it);
`session resume` resumes one without attaching. Soft Serve only parks
sandboxes it created (labelled `soft-serve.managed=true`, with an owner and a
repository). Parking needs a compute driver with OpenShell's suspend-resume
capability; the OpenShell 0.1.8 Kubernetes driver has none. When the gateway
refuses, Soft Serve logs it once, stops trying for that sandbox until it
restarts, and `session ls --json` shows why under `unsuspendable`. Set
`sessions.suspend.enabled: false` to not try at all. After a Soft Serve
restart a session that was parked before shows no status until it is
resumed: the status is kept in memory only.

**Client TLS secret.** In Kubernetes, mount the gateway's client certificate
secret, typically `openshell-client-tls`, into the Soft Serve pod and point the
`tls_*` settings at it:

```yaml
volumes:
  - name: openshell-tls
    secret:
      secretName: openshell-client-tls
containers:
  - name: soft-serve
    volumeMounts:
      - name: openshell-tls
        mountPath: /etc/openshell-tls
        readOnly: true
```

**Git credentials.** On each `session start` and `session attach`, Soft Serve
issues a short-lived access token named `openshell:<sandbox>` (see
`token_ttl`) and prunes expired ones of that name. It writes the token, as an
`http.<clone_url>/.extraHeader` basic-auth header, to
`/tmp/soft-serve/gitconfig` in the sandbox (mode 0600, in a 0700 directory),
and adds that file to the sandbox user's `~/.gitconfig` as an
`include.path`. So every git process in the sandbox, including agents in osewa
sessions, which start with a clean environment, uses the newest token. The
token is never in `.git/config`, `~/.gitconfig`, anything under `/sandbox`
(so never in a snapshot of it), a command line Soft Serve builds, or the
sandbox's spec; it reaches the sandbox in the environment of the exec that
writes the file. It grants your read/write access, so agents can push.
Processes inside the sandbox can read it. `session end` revokes it.

A program left running longer than `token_ttl` after the last start or
attach loses git access until the next one renews the file. Attach again, or
raise `token_ttl` for long-running agents.

**Sandbox egress.** The sandbox's network policy must allow git (and jj, which
runs git) to reach Soft Serve's internal HTTP endpoint. Here is an example
sandbox policy entry. Adjust the host, port and binary paths to your cluster
and image, and the field names to your OpenShell version:

```yaml
network_policies:
  soft_serve_git:
    name: soft-serve-git
    endpoints:
      - host: soft-serve-http.openshell.svc.cluster.local
        port: 23232
        protocol: rest
        access: read-write
    binaries:
      - path: /usr/bin/git
      - path: /usr/lib/git-core/git-remote-http
      - path: /usr/bin/jj
```

osewa needs no egress: the bridge reaches its server over loopback, and Soft
Serve turns its update checks off.

**Idle timeout.** A bridged session is an ordinary SSH session, so
`ssh.idle_timeout` (default 600 seconds) still applies. Terminal traffic in
either direction counts as activity. A connection that sits completely quiet
for longer than the timeout is disconnected; the program keeps running, and
`session attach` re-attaches. To avoid the disconnects, raise `idle_timeout`
or set `0`, or set `ServerAliveInterval` in your SSH client.

#### Notifications

Soft Serve can push a notification to your phone when an agent session needs
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
    token_file: /etc/soft-serve/secrets/ntfy-token
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
that agents report, as Soft Serve polls them (see "Session status" under
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
`soft_serve_notifications_total{result="dropped"}`.

**Checks.** While `ntfy.enabled` is false nothing in this section is checked
beyond its types. When it is true, `url` and `topic` are required, `url` must
be an `http(s)` base URL without credentials or the topic, every `on` entry
must be a trigger, `debounce` must not be negative, `click_url` may only use
the placeholders above, and `token_file` must be readable and not empty (a
relative path resolves against the data path). There is no inline `token`
key. `soft config check` reports all of it.

**YAML 1.1 tools.** Quote the `on` key (`"on":`). Soft Serve reads a bare
`on:` correctly, but YAML 1.1 tools such as Helm read it as the boolean
`true`; Soft Serve then reports `unknown key notifications.true` with a hint,
and the Helm chart refuses to render.

**Topic privacy.** On a public server anyone who knows a topic can read and
publish to it. Use an unguessable topic name, or an access-controlled topic
with an access token in `token_file`. The topic itself is in the config file.

Each setting has a `SOFT_SERVE_NOTIFICATIONS_*` environment variable, for
example `SOFT_SERVE_NOTIFICATIONS_ON=blocked,done` or
`SOFT_SERVE_NOTIFICATIONS_NTFY_TOKEN_FILE=...`.

**Event bus.** Notifications are one subscriber of an in-process event bus
that carries session status changes, session lifecycle steps (created,
resumed, ready, parked, exited, ended, failed, each naming the repository,
session name and sandbox) and forge outcomes. Each subscriber
has a bounded buffer; one that falls behind loses events rather than slowing
anything down, counted in `soft_serve_events_dropped_total{subscriber}`.
`soft_serve_events_published_total{kind}` counts what was published.

## Server Access

Soft Serve at its core manages your server authentication and authorization. Authentication verifies the identity of a user, while authorization determines their access rights to a repository.

To manage the server users, access, and repos, you can use the SSH command line interface.

Try `ssh localhost -i ~/.ssh/id_ed25519 -o IdentitiesOnly=yes -p 23231 help` for more info. Make sure
you use your key here.

> **Note** The `IdentitiesOnly` option is used to prevent SSH from using any
> other keys in your `~/.ssh` directory. This is useful when you have multiple
> keys, and you want to use a specific key for Soft Serve.

For ease of use, instead of specifying the key, port, and hostname every time
you SSH into Soft Serve, add your own Soft Serve instance entry to your SSH
config. For instance, to use `ssh soft` instead of typing `ssh localhost -i
~/.ssh/id_ed25519 -o IdentitiesOnly=yes -p 23231`, we can define a `soft` entry in our SSH config
file `~/.ssh/config`.

```conf
Host soft
  HostName localhost
  Port 23231
  IdentityFile ~/.ssh/id_ed25519
  IdentitiesOnly yes
```

Now, we can do `ssh soft` to SSH into Soft Serve. Since `git` is also aware of
this config, you can use `soft` as the hostname for your clone commands.

```sh
git clone ssh://soft/dotfiles
# make changes
# add & commit
git push origin main
```

> **Note** The `-i` and `-o` parts will be omitted in the examples below for brevity. You
> can add your server settings to your sshconfig for quicker access.

### Authentication

Everything that needs authentication is done using SSH. Make sure you have
added an entry for your Soft Serve instance in your `~/.ssh/config` file.

By default, Soft Serve gives read-only permission to anonymous connections to
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
to pull from in a dev environment), you can instead set the `SOFT_SERVE_ANON_ACCESS`
and `SOFT_SERVE_ALLOW_KEYLESS` environment variables (or the equivalent
`anon_access`/`allow_keyless` `config.yaml` fields) to grant full,
unauthenticated access from the moment the server starts, with no admin key
and no `settings` command required:

```sh
SOFT_SERVE_ANON_ACCESS=admin-access \
SOFT_SERVE_ALLOW_KEYLESS=true \
  soft serve
```

> **Warning** This grants anyone who can reach the server full admin access
> with no authentication at all. It is intended strictly for local/dev use on
> a machine or network you trust — never expose a server configured this way
> to an untrusted network. The server logs a warning banner on startup
> whenever this combination is active.
>
> Unlike the `settings` command, these overrides always take precedence over
> the database for as long as they're set — not just on first run. If you
> also try to tighten access via `ssh soft settings ...` while an override is
> active, the change is saved but has no effect until the override is
> removed; the `settings` command will warn you when this happens.

#### SSH

Soft Serve doesn't allow duplicate SSH public keys for users. A public key can be associated with one user only. This makes SSH authentication simple and straight forward, add your public key to your Soft Serve user to be able to access Soft Serve.

#### HTTP

You can generate user access tokens through the SSH command line interface. Access tokens can have an optional expiration date. Use your access token as the basic auth user to access your Soft Serve repos through HTTP.

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

Soft Serve offers a simple access control. There are four access levels,
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

Or you can add your Soft Serve server as a remote to any existing repo, given
you have write access, and push to remote:

```
git remote add origin ssh://localhost:23231/icecream
```

After you’ve added the remote just go ahead and push. If the repo doesn’t exist
on the server it’ll be created.

```
git push origin main
```

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
ssh -p 23231 localhost repo import soft-serve https://github.com/charmbracelet/soft-serve
```

Use `--mirror` or `-m` to mark the repository as a *pull* mirror.

### Forges

Soft Serve can see your repositories on GitHub and GitLab, import them, and
keep them in sync without ever losing work you pushed only to Soft Serve.
Unlike a mirror, a forge repository is a normal, writable repository: sync
never rewrites or deletes a branch, and pushing back to the forge is a
separate, explicit command.

Forges are declared in the config file only (there is no runtime "add
forge"), and their tokens only ever come from a file:

```yaml
forges:
  - name: github                    # DNS label: commands, ref namespace, git config
    kind: github                    # github or gitlab
    token_file: /etc/soft-serve/secrets/github-token
    include: ["me/*", "my-org/**"]  # globs on owner/repo; empty = everything
    exclude: ["me/old-*"]
    sync: 10m                       # scheduled sync, at least 1m (default 10m)
  - name: gitlab
    kind: gitlab
    url: https://gitlab.example.com # default https://github.com / https://gitlab.com
    token_file: /etc/soft-serve/secrets/gitlab-token
    prefix: work                    # import as work/<group>/<project> (default: name)
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
- Changing forges means editing the file and restarting (the Helm chart rolls
  the pod when its ConfigMap changes). `soft config check` validates them,
  including that every token file is readable.

The forge host you declare is trusted: Soft Serve's SSRF guard for
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
internal) on the forge is private on Soft Serve. Imported repositories get the
server's default VCS kind, `jj`, like any other new repository; pass `--vcs
git` to `forge import` for git presentation.

**Sync.** Upstream branches and tags are fetched into the forge's own
namespace, `refs/remotes/<forge>/heads/*` and `refs/remotes/<forge>/tags/*`,
which the TUI and web views do not list and pushes to Soft Serve cannot write.
Pruning happens only there. Then, for each upstream branch:

| Local branch | Sync |
| --- | --- |
| missing | created at upstream |
| strictly behind upstream (no local-only commits) | fast-forwarded |
| equal, ahead, or diverged | left alone; ahead/behind reported |
| exists only on Soft Serve, or deleted upstream | left alone, never deleted |

Upstream tags missing locally are created; an existing tag is never moved.
Every update is a compare-and-swap, so a push landing meanwhile wins. A
branch deleted on Soft Serve but still upstream is created again by the next
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
# Add collaborator to soft-serve
ssh -p 23231 localhost repo collab add soft-serve frankie

# Add collaborator with a specific access level
ssh -p 23231 localhost repo collab add soft-serve beatrice read-only

# Remove collaborator
ssh -p 23231 localhost repo collab remove soft-serve beatrice

# List collaborators
ssh -p 23231 localhost repo collab list soft-serve
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

HTTP responses for repository routes carry an `X-Soft-Serve-VCS` header (`git`
or `jj`) for callers that are authorized to read the repository.

### Repository Branches & Tags

Use `repo branch` and `repo tag` to list, and delete branches or tags. You can
also use `repo branch default` to set or get the repository default branch.

### Repository Tree

To print a file tree for the project, just use the `repo tree` command along with
the repo name as the SSH command to your Soft Serve server:

```sh
ssh -p 23231 localhost repo tree soft-serve
```

You can also specify the sub-path and a specific reference or branch.

```sh
ssh -p 23231 localhost repo tree soft-serve server/config
ssh -p 23231 localhost repo tree soft-serve main server/config
```

From there, you can print individual files using the `repo blob` command:

```sh
ssh -p 23231 localhost repo blob soft-serve cmd/soft/main.go
```

You can add the `-c` flag to enable syntax coloring and `-l` to print line
numbers:

```sh
ssh -p 23231 localhost repo blob soft-serve cmd/soft/main.go -c -l

```

Use `--raw` to print raw file contents. This is useful for dumping binary data.

### Repository webhooks

Soft Serve supports repository webhooks using the `repo webhook` command. You
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

## The Soft Serve TUI

<img src="https://stuff.charm.sh/soft-serve/soft-serve-demo-commit.png" width="750" alt="TUI example showing a diff">

Soft Serve TUI is mainly used to browse repos over SSH. You can also use it to
browse local repositories with `soft browse` or running `soft` within a Git
repository.

```sh
ssh localhost -p 23231
```

It's also possible to “link” to a specific repo:

```sh
ssh -p 23231 localhost -t soft-serve
```

You can copy text to your clipboard over SSH. For instance, you can press
<kbd>c</kbd> on the highlighted repo in the menu to copy the clone command
[^osc52].

The home page has tabs: <kbd>1</kbd>–<kbd>n</kbd> jump to one, <kbd>tab</kbd>
cycles. When OpenShell is enabled and you are signed in with a key, a
**Sessions** tab lists your agent sessions, what needs you first, and the
header counts the ones blocked, errored, done and working. In it:
<kbd>enter</kbd> attaches (detach with <kbd>~.</kbd> to come back to the same
row), <kbd>n</kbd> starts a session from a profile, <kbd>x</kbd> ends one,
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
starts a session for the highlighted repository. Inside a repository, a
**Sessions** tab after Tags lists that repository's sessions and signals with
the same keys, and the status bar ends with the attention summary.

When forges are configured, server admins (the same people who may run
`forge import` and `forge sync`) get a **Forges** tab: every forge with its
repositories, imported (`✓`, with ahead/behind and last sync) or not.
<kbd>i</kbd> imports the highlighted repository, <kbd>I</kbd> imports all
shown (the <kbd>/</kbd> filter narrows them; you confirm the count),
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

Soft Serve supports git server-side hooks `pre-receive`, `update`,
`post-update`, and `post-receive`. This means you can define your own hooks to
run on repository push events. Hooks can be defined as a per-repository hook,
and/or global hooks that run for all repositories.

You can find per-repository hooks under the repository `hooks` directory.

Globs hooks can be found in your `SOFT_SERVE_DATA_PATH` directory under
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

echo "Hi from Soft Serve update hook!"
echo
echo "RefName: $refname"
echo "Change Type: $newrev_type"
echo "Old SHA1: $oldrev"
echo "New SHA1: $newrev"

exit 0
```

Now, you should get a message after pushing changes to any repository.

## A note about RSA keys

Unfortunately, due to a shortcoming in Go’s `x/crypto/ssh` package, Soft Serve
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

## Contributing

See [contributing][contribute].

[contribute]: https://github.com/charmbracelet/soft-serve/contribute

## Feedback

We’d love to hear your thoughts on this project. Feel free to drop us a note!

- [Twitter](https://twitter.com/charmcli)
- [The Fediverse](https://mastodon.social/@charmcli)
- [Discord](https://charm.sh/chat)

## License

[MIT](https://github.com/charmbracelet/soft-serve/raw/main/LICENSE)

---

Part of [Charm](https://charm.sh).

<a href="https://charm.sh/"><img alt="The Charm logo" src="https://stuff.charm.sh/charm-badge.jpg" width="400"></a>

Charm热爱开源 • Charm loves open source

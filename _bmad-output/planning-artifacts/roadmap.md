# Soft Serve: git hub + dev server roadmap

Status: open decisions settled on their leanings 2026-11-12; build-out in progress (local CLI parked). Last updated 2026-11-12 (rev 12).

This is a working document. Nothing here is frozen. Each workstream becomes its own spec under `_bmad-output/implementation-artifacts/` once its open questions are settled.

## 1. What this is for

Soft Serve becomes two things on one server, running in Kubernetes:

1. **A home for my git repos.** It sees my repositories on GitHub and GitLab and replicates them, so I can keep work on a different origin without pushing everything to a forge.
2. **A dev server.** It spawns agent or shell sessions as separate pods. Sessions keep running unattended after I disconnect. I can see them in a session pane and reach them from a local CLI. When an agent's state changes, I get a notification on my phone.

## 2. Where the code is today

| Area | State | Where |
|------|-------|-------|
| jj repos alongside git | Done | `spec-jj-first-class-repos.md` |
| OpenShell sessions (`session start\|ls\|end`, `--image`) | Built, never run against a live gateway | `pkg/openshell/`, `pkg/ssh/cmd/session.go` |
| Single-repo mirror (`repo import --mirror`, `mirror-pull` job every 10m) | Done, upstream Soft Serve behaviour | `pkg/backend/repo.go`, `pkg/jobs/mirror.go` |
| Forge discovery (GitHub/GitLab) | Not started | — |
| Session persistence across detach | Not started | — |
| Session status | Not started | — |
| Notifications on status change | Not started. Repo webhooks exist (push, branch/tag, collaborator, repository events) but are per-repo and have no session events. | `pkg/webhook/` |
| Local session CLI | Not started (`soft` has `serve`, `hook`, `admin`, `browse` only) | `cmd/soft/main.go` |
| TUI session pane | Not started (selection page has two panes: repos, readme) | `pkg/ui/pages/selection/selection.go` |
| S3 snapshot of `/sandbox` | Not started; planned as opt-in, off by default (D12). Seams marked in code | `pkg/openshell/session.go` (`Start`, `End`) |
| Project-init options | Deferred | `deferred-work.md` |
| Deployment | No `openshell` or `soft-serve` namespace in `omni-ops-microcloud-k8s`; no mention in the `microcloud` repo. Prod cluster not checked. | — |

## 3. Findings that constrain the design

**F1. Agents do not survive detach today.**
The sandbox pod stays up, but `openshell.Bridge` cancels the interactive exec when the SSH context ends. A foreground agent started from `bash -l` dies with the connection. "Leave it running unattended" needs a process inside the pod that owns the PTY.

**F2. osewa is a session layer, not a pod launcher.**
It runs a server in the host (port 7322, TCP and UDP), keeps one holder process per session with the PTY and screen, and survives client disconnects. Something else still has to create the pod and pick the image. It covers what the zellij and herdr todo items were reaching for.

**F3. osewa already keeps OSC 7501 status per session, and exposes it as JSON.**
Each holder keeps the session's latest root OSC 7501 record (`state`, `kind`, `app`, `title`, `msg`, `progress`). `osewa local ls`, run on the host, prints the session list as JSON without needing the server or an enrolled device. Soft Serve can exec that in the pod. Only the root record is kept; child records (`id=build/test`) are not listed.

**F4. Current mirrors would delete local-only work.**
Mirror imports use `git clone --mirror`, and the job runs `fetch --prune` and `remote update --prune`. A branch pushed only to Soft Serve is removed on the next sync. "Different origins" needs upstream refs kept apart from local ones.

**F5. Three session behaviours are unverified against a real gateway.**
Listed in `deferred-work.md`: the EmulatePty line-feed rewrite garbling raw-mode TUIs, resuming a Stopped sandbox, and the Stopping/Suspended phases being treated as fatal.

**F6. osewa facts that shape the design** (from its docs and `docs/PROTOCOL.md`, read 2026-11-12).
- **No sshd needed to enroll.** `osewa enroll` takes `{"name","key"}` on stdin, appends to `~/.config/osewa/devices`, and prints `{"server_key","port","version"}`. Soft Serve can run it by exec and hand the server key to the CLI, which writes the laptop's `hosts.json`.
- **Injectable.** Linux builds are static musl, amd64 and arm64, so the binary can be copied into any base image from an init container or volume.
- **Status changes are pushed, but only to attached clients.** The protocol streams `status` events on an attached stream. There is no CLI hook or watch command. Getting a push means speaking the wire protocol (to the server, or to the holder's Unix socket in the pod).
- **The protocol is a draft.** "Breaking changes are allowed until v1." Anything that speaks it directly must be pinned to an osewa version. `osewa local ls` and `osewa ls` are the stable-looking surfaces.
- **Sessions are reaped.** A running session ends after 24 hours with nobody attached and no output. An agent blocked on me over a long weekend is killed.
- **Clean environment.** Sessions do not inherit the server's environment. Git credentials, which today travel as `GIT_CONFIG_*` env on the exec, must be passed in the session-create `env`.
- **Tags fit named sessions.** A tagged create from the same client returns the existing session of that tag. Several clients can attach to one session: one owner, the rest read-only viewers.
- **Phone client exists.** Moshi (iOS and Android) has osewa as an experimental transport.

**F8. OSC 7501 lifetimes decide what a notification means.**
`working` and `blocked` are dropped at process exit or the next shell prompt; `done` and `error` survive both. A session that goes from `blocked` to no status has been unblocked or has exited, and the two need telling apart. The spec asks terminals to rate-limit anything a record causes outside the terminal, which is the debounce in H3.

**F7. Notifications need Soft Serve to see status while nobody is attached.**
A phone notification matters most when I am away, so status cannot come from parsing a bridged terminal stream. It has to be collected from the pod (D5 option a or b). Polling delays a notification by up to the poll interval; a push from the pod does not.

**F9. Agent Substrate does not fit this design as it stands** (from its README and docs, not tested).
- **No exec.** `kubectl ate` has create, suspend, resume and `proxy actor` (TCP port-forward through its router), no exec or attach. Cloning, enrolling and status polling by exec would all need an HTTP helper inside the actor.
- **Inbound only through its router; UDP is dropped.** osewa over QUIC is out; its WebSocket carrier through the router is unverified.
- **gVisor or microVM only, unprivileged.** Skaffold and container builds inside a session (workstream G) get harder.
- **Heavy to run.** API server, node DaemonSet, controller, network stack, Postgres and an object store, built for millions of mostly-idle actors. I have a handful.
- **What it would buy:** memory and filesystem snapshot with sub-second resume (replaces workstream F), and credentials injected outside the sandbox (removes the token-in-sandbox risk accepted in the OpenShell spec).

**F10. What OpenShell would do** (from the Go SDK v0.1.6 in `go.mod`; only the first two rows are used by the code today, none run against a live gateway).

| Capability | SDK surface | Use here |
|------------|-------------|----------|
| Pod lifecycle from any image | `Sandboxes()` create, start, delete, wait, list by label | Used today |
| Exec, one-shot and interactive PTY | `Exec()` | Used today: clone, attach. Would also run `osewa enroll` and `osewa local ls` |
| TCP tunnel into a sandbox port | `TCP().Forward` / `Listen`, `SSH().Tunnel` | Reach osewa's 7322 through the gateway. TCP only: nothing for UDP, so no QUIC |
| File upload and download | `Files()` | Inject the osewa binary without an init container |
| Suspend and resume with memory kept | `SandboxSuspendResume()` | Park idle sessions. Explicit calls only: the SDK has no idle policy, so Soft Serve decides when. Needs a compute driver and isolation backend that retain memory. Where the state is kept is not stated. Reduces workstream F but does not replace it, see F1 there |
| HTTP service exposure | `Services().Expose` | The "web server" idea: publish a dev server from a session |
| Egress policy with a review flow | `Policy()` drafts, approve, reject | Agent asks for network access, I approve. Could surface as `blocked` in the menu |
| AI provider credentials | `Providers()` | Model keys held by the gateway, not typed into the sandbox |

Costs: a gateway to deploy and keep running, three unverified lifecycle behaviours (F5), and no direct laptop-to-pod path, so osewa attach would go laptop → Soft Serve → gateway tunnel → pod.

## 4. Target shape

```
 laptop / phone                      Kubernetes
┌──────────────────┐        ┌──────────────────────────────────────┐
│ soft session …   │──ssh──▶│ Soft Serve (control plane)           │
│ osewa attach …   │──ws───▶│  repos, forge sync, session registry │
│                  │        │  status aggregation, TUI pane        │
│                  │        │  session event bus ──▶ ntfy ──▶ phone│
└──────────────────┘        └───────────────┬──────────────────────┘
                                            │ gRPC: lifecycle, exec,
                                            │ TCP tunnel, suspend
                            ┌───────────────▼──────────────────────┐
                            │ OpenShell gateway                    │
                            └───────────────┬──────────────────────┘
                            ┌───────────────▼──────────────────────┐
                            │ OpenShell sandbox (one per session)  │
                            │  custom base image                   │
                            │  osewa server + holder ─ agent/shell │
                            │  /sandbox/<repo>                     │
                            └──────────────────────────────────────┘
```

- **Soft Serve** owns identity, repos, forge credentials, and the session lifecycle. It stays the single place to ask "what is running".
- **Session pods** run my image. An osewa session inside owns the agent's terminal.
- **Local CLI** asks Soft Serve for the list and status, then attaches either directly with osewa or through the existing SSH bridge.

Where osewa runs:

| Place | Role | Needed |
|-------|------|--------|
| Session pod | `osewa server` plus one holder per session | Always |
| Laptop / phone | osewa client, dialling Soft Serve, which relays to the sandbox through the gateway | Only for osewa attach (D2) |
| Soft Serve pod | No server. Carries the pinned binary to copy into session pods; drives `osewa enroll` and `osewa local ls` by exec in the pod | Binary only |

The SSH bridge fallback needs an osewa client somewhere. Leaning: exec `osewa attach` inside the session pod against its own server, so Soft Serve never speaks the draft protocol. Unverified: that a pod can enrol itself and attach over loopback.

## 5. Decisions

| # | Decision | Options | Leaning | Status |
|---|----------|---------|---------|--------|
| D1 | Pod runtime | (a) OpenShell sandboxes; (b) plain pods through the Kubernetes API; (c) Agent Substrate actors | **(a) OpenShell.** Chosen for suspend/resume, gateway tunnels, egress approval and file push (F10), and because the integration is already written. Keep the `Client` interface narrow so the choice stays reversible. | **Decided 2026-11-12** |
| D2 | How I reach a session | (a) osewa through an OpenShell TCP tunnel relayed by Soft Serve (WebSocket carrier, no QUIC); (b) Soft Serve's SSH bridge into an in-pod `osewa attach`; (c) both | (c), (b) first because the bridge exists. Follows from D1: there is no direct laptop-to-pod path, so everything enters through Soft Serve. | **Decided 2026-11-12: leaning adopted** |
| D3 | Forge sync semantics | (a) strict mirror as today; (b) upstream refs under a per-forge namespace, local refs never pruned, push to forge is explicit | (b) | **Decided 2026-11-12: leaning adopted** |
| D4 | Session identity | (a) one per (user, repo) as today; (b) several named sessions per repo | (b), so two agents can work on one repo | **Decided 2026-11-12: leaning adopted** |
| D5 | How session status reaches Soft Serve | (a) Soft Serve execs `osewa local ls` in each pod on an interval; (b) a small watcher in the pod attaches to the holder as a viewer and pushes `status` events to Soft Serve; (c) Soft Serve parses OSC 7501 itself from the bridged stream | (a) first for the status column and ntfy, then (b): the Signals pane wants every report, and polling misses changes between polls and all child records. (b) depends on the draft osewa protocol, so pin the version. (c) only sees status while someone is attached. | **Decided 2026-11-12: leaning adopted** |
| D7 | Shape of the pub/sub | (a) in-process event bus in Soft Serve with pluggable subscribers (ntfy, TUI pane, CLI watch, webhooks); (b) an external broker such as NATS; (c) no bus, the status collector calls ntfy directly | (a). One process and one user: a broker is more to run than the problem needs, and (c) leaves the TUI pane and CLI without live updates. | **Decided 2026-11-12: leaning adopted** |
| D8 | How ntfy is fed | (a) native ntfy publisher (topic, title, priority, tags, click action); (b) extend the existing webhook system with session events and point it at ntfy | (a) first, (b) later if other consumers appear. Webhooks today are per-repo and send JSON that ntfy would need a template for. | **Decided 2026-11-12: leaning adopted** |
| D9 | Which transitions notify | (a) every change; (b) only `blocked`, `done`, `error`; (c) configurable | (b) as the default, (c) as config. `working` and `idle` are noise on a phone. | **Decided 2026-11-12: leaning adopted** |
| D10 | Where settings live | (a) everything in the mounted YAML, DB holds only runtime data; (b) YAML for server settings, DB for things changed at runtime (forge connections, per-repo defaults); (c) YAML seeds the DB on start | (a). It suits GitOps and one user. Costs: changing a forge or default means editing the ConfigMap, and secrets must be file references, never inline. | **Decided 2026-11-12: leaning adopted** |
| D11 | Applying config changes | (a) restart the pod (Reloader is already in the cluster); (b) watch the file and reload | (a) | **Decided 2026-11-12: leaning adopted** |
| D12 | S3 snapshots | (a) drop; (b) build, always on; (c) build, switched on in the config file, off by default | **(c).** Suspend is the main way to pause and come back; S3 is the opt-in backup for what suspend cannot cover. | **Decided 2026-11-12** |
| D13 | Deployment tooling | (a) Helmfile with a local Soft Serve chart and the upstream OpenShell chart; (b) ArgoCD Applications; (c) raw manifests | **(a).** One `helmfile apply` brings up both. ArgoCD can wrap it later. | **Decided 2026-11-12** |
| D14 | Namespaces | (a) Soft Serve and the gateway in one namespace; (b) separate namespaces with the client TLS secret copied across | (a), so Soft Serve mounts `openshell-client-tls` directly. Namespace name is a helmfile value. | **Decided 2026-11-12: leaning adopted** |
| D6 | Session state in the DB | (a) stay stateless, gateway and labels are the truth; (b) add a sessions table | (a) until named sessions or status history force it | **Decided 2026-11-12: leaning adopted** |

## 6. Open questions

Things only a spike can answer. Questions 2, 3, 5 and 9 were answered from the osewa docs and moved to F3 and F6.

1. Does osewa's WebSocket carrier work through OpenShell's `TCP().Forward` tunnel, and does a long-lived `osewa server` survive in a sandbox? (UDP has no SDK path, so QUIC is assumed out.)
4. Which of my agents emit OSC 7501 themselves, and which need a wrapper or hook?
6. Does the EmulatePty line-feed rewrite garble full-screen programs over the SSH bridge? (F5)
7. How does the laptop's osewa client reach a session through Soft Serve: a per-session port or hostname on Soft Serve relayed to `TCP().Forward`, or a local `soft session forward` that the client dials?
8. Is there an OpenShell gateway install path that fits the microcloud GitOps setup (ArgoCD is present)?
11. Can osewa's 24-hour reaping of silent, unattached sessions be turned off or lengthened? If not, something must keep long-blocked sessions alive.
12. Does `osewa local ls` JSON carry the full status record (kind, msg, progress) or only the state? The protocol says full; confirm on a real build.
13. Can a notification's tap action open the session on the phone (Moshi over osewa)? Only matters if I use the optional click URL.
16. OpenShell suspend: which driver and backend does it need, where is the memory state stored (node disk or object store), and does it survive a node drain?
17. Does SDK v0.1.6 work against gateway 0.1.8, or must the two be pinned to the same version?
14. Can a pod enrol itself as a device and `osewa attach` to its own server over loopback? The SSH bridge fallback depends on it.
15. If pods are not directly reachable, can osewa's `--via-ssh` mode go through Soft Serve's SSH server, which is not a real sshd?

## 7. Workstreams

Ordered by dependency, not by priority. A and E do not depend on each other.

### A. Prove the session foundation
- A1. Using workstream J, deploy Soft Serve and an OpenShell gateway to `omni-ops-microcloud-k8s` with a driver that supports suspend (question 16). Settle the three unverified items in F5.
- A2. Spike osewa in a sandbox: push the static binary with `Files()`, enroll a device by exec, read `osewa local ls`, attach over `TCP().Forward`, suspend and resume with an agent running. Answer questions 1, 7, 11, 12, 14 and 16.
- A3. Decide D2. Write the result as an architecture doc.

### B. Sessions that outlive the connection
- B1. The attach command becomes "attach to the osewa session", so the agent survives detach. Git credentials move from the exec env to the session-create env. (F1, F6)
- B2. Named sessions, several per repo. (D4)
- B3. Per-repo defaults: image, command, agent.
- B4. Park idle sessions: suspend when nobody is attached and status has been `idle`, `done` or `blocked` for a set time; resume on attach. Emit both as events. A suspended `blocked` session still shows as blocked. (F10)

### C. Seeing what agents are doing
- C1. Collect OSC 7501 state per session by exec of `osewa local ls`, and add state, kind and message to `session ls`. (D5, F3)
- C4. In-pod watcher that pushes every `status` event, including child records, so the Signals pane is complete. (D5b)
- C2. A wrapper for agents that do not emit OSC 7501.
- C3. TUI sessions tab with live status, enter to attach. Part of the menu in section 10.

### J. Deployment (first slice)
Facts checked 2026-11-12:
- OpenShell publishes a Helm chart at `oci://ghcr.io/nvidia/openshell/helm-chart`, latest 0.1.8. The Go SDK pinned in `go.mod` is v0.1.6.
- The chart needs the Kubernetes Agent Sandbox CRDs and controller (`kubernetes-sigs/agent-sandbox`, latest v1.0.10) installed first, and a CNI that enforces NetworkPolicy. `omni-ops-microcloud-k8s` has Cilium; it has no Agent Sandbox CRDs.
- Gateway mTLS is on by default; the chart creates `openshell-server-tls` and `openshell-client-tls`.
- This repo's `Dockerfile` copies a prebuilt binary from goreleaser and cannot build from source. There is no chart.

Items:
- J1. `Containerfile`: multi-stage build from source, non-root runtime image. **Written; image not yet built** (no container engine was running).
- J2. `deploy/helm/soft-serve`: StatefulSet with a PVC, config as a read-only ConfigMap mount (I1), secrets as file mounts (I2), Services for SSH, HTTP and stats. **Written; lints and renders.**
- J3. `deploy/helmfile.yaml` with `deploy/environment.yaml`: Agent Sandbox v1.0.10 (upstream chart, skippable), OpenShell 0.1.8, Soft Serve, in one namespace. **Written; renders end to end. Never installed.**
- J4. `deploy/README.md`. **Written.**
- J5. Build and push the image, apply to `omni-ops-microcloud-k8s`, run A1. Next.

Found while building it:
- The gateway Service is `openshell`, gRPC on port 8080. The example in the top-level `README.md` (`openshell-gateway.openshell.svc:8443`) does not match this chart.
- Sandboxes are created in the same namespace and are also given the client TLS credentials, so the certificate Soft Serve presents is not exclusive to it.
- A read-only config mount is safe: Soft Serve only writes a default config when the file is missing (closes I3 for the mounted case).
- There is no token-file option for the gateway token; the chart refuses an inline `openshell.token` and takes it from a Secret through an env var (I2 still open in code).
- Two required values have no default: the sandbox image and the Soft Serve image tag.

### H. Events and notifications
Depends on C1.
- H1. Session event bus inside Soft Serve: status changes and lifecycle events (created, ready, ended, failed), published once and consumed by many. (D7)
- H2. ntfy subscriber, optional and off by default. Server URL (self-hosted or ntfy.sh), topic, token file and optional click URL in config. `blocked` goes out at high priority with `kind` and `msg` in the body; `done` and `error` at default priority. Each message names the repo and session. (D8, D9)
- H3. Debounce and dedupe, so a flapping agent does not send a burst.
- H4. Signals feed: a capped in-memory log of session events on the bus, shown in the TUI Signals pane and by `soft session signals [--follow]` in the local CLI.
- H5. Optional: session events as a new webhook event type.

### I. Configuration
Cuts across every workstream: each one adds its section here, none invents its own settings store.
- I1. One mounted, read-only YAML holds all settings (see section 9). Soft Serve already reads `SOFT_SERVE_CONFIG_LOCATION` and `SOFT_SERVE_*` env; keep env as an override.
- I2. Secrets by file reference only (`*_file`), so the YAML can live in a ConfigMap and git. `openshell.token` is inline today and needs a `token_file`.
- I3. `soft serve` writes a default config when none exists. With a read-only mount it must not try to.
- I4. Validate on start and fail loudly, naming the bad key. Add `soft config check` for use in CI.
- I5. The Helm chart in J2 mounts the ConfigMap and the secrets.

### D. Local CLI (parked 2026-11-12, after the build-out)
- D1. `soft session ls|new|attach|end` on the laptop, talking to Soft Serve over SSH. `--json` on `ls`.
- D2. `attach` hands off to `osewa attach` when direct access is available, and falls back to the SSH bridge.

### E. Forge sync
- E1. Forge connections declared in the config file: kind, base URL, token file, include and exclude patterns. (D10)
- E2. Discovery: list my repos on each forge, import selected or all, keep the list fresh.
- E3. Non-destructive sync: upstream refs in their own namespace, local refs untouched. (F4, D3)
- E4. Explicit push to a forge, per repo and per ref.

### F. Workspace durability
- F1. S3 snapshot of `/sandbox`, in scope as a config-gated feature, **off by default** (D12). When `snapshots.s3.enabled` is false nothing runs and no S3 settings are validated.
  - What it covers that suspend (B4) does not: `session end`, a deleted sandbox, a failed restore, losing the node or cluster.
  - When on: snapshot at `session end` (and optionally before each suspend), restore at `session start` before the clone check. Seams are already marked in `Start` and `End`.
  - Carried over from `deferred-work.md`: one tarball per session, replaced each time; server-side encryption; if the sandbox directory is non-empty on restore, S3 wins after a prompt.
  - Credentials by file reference, like every other secret (I2).
- F2. Project-init options: source control kind, agent, defaults.

### G. Local CI (parked)
Needs its own architecture pass before any spec.
- Run scans and CI without pushing to a forge, through Woodpecker or similar.
- How the workspace reaches the CI pod: RWX volume or rsync to a new volume.
- Permissions for skaffold and integration tests inside a mounted context.
- Cilium global service to bridge to the dev cluster while Woodpecker runs in prod.

## 8. Where each todo.txt line landed

| todo.txt | Lands in |
|----------|----------|
| enable openshell integration | Built; A1 proves it |
| s3 backup of working directory for resuming sessions | F1 |
| options for initialising new projects (source control, agents) | F2, B3 |
| add zellij for managing sessions plus web server | Sessions: replaced by osewa (B1). "Web server": not placed, see section 11 |
| use herdr core to manage sessions on the remote? | Replaced by osewa (B1), pending the spike |
| run scans/ci locally via woodpecker, rwx volume or rsync | G |
| containers need permissions to run skaffold in a mounted context | G |
| cilium global service to bridge to dev cluster | G |
| agents menu with program status protocol, monitor and ssh into sessions | C1, C3, D1 |
| (new) pub/sub and ntfy notifications on agent state change | H |
| (new) everything configurable from a mounted YAML | I |
| (new) Containerfile and helmfile to deploy alongside OpenShell | J |

## 9. Config file sketch

Shape only, to argue about. Existing sections (`ssh`, `http`, `db`, `lfs`, `jobs`) are unchanged and omitted. Key names are not final.

```yaml
sessions:
  enabled: true
  image: ghcr.io/me/agent-sandbox:latest
  command: bash -l
  token_ttl: 24h
  osewa:
    enabled: true
    version: v0.1.0             # pinned: the protocol is a draft
    port: 7322
  status:
    poll_interval: 10s
  suspend:
    enabled: true
    after: 30m                  # unattached and not working
  profiles:                     # named presets for `session new --profile`
    claude:
      image: ghcr.io/me/agent-claude:latest
      command: claude
  repos:                        # per-repo defaults, glob match
    "infra/*":
      profile: claude

openshell:
  gateway_address: https://openshell-gateway.openshell.svc:8443
  token_file: /etc/soft-serve/secrets/openshell-token

forges:
  - name: github
    kind: github
    token_file: /etc/soft-serve/secrets/github-token
    include: ["danielboothcloud/*"]
    exclude: []
    sync: 10m
  - name: gitlab
    kind: gitlab
    url: https://gitlab.com
    token_file: /etc/soft-serve/secrets/gitlab-token

notifications:
  on: [blocked, done, error]    # D9
  debounce: 30s
  ntfy:
    enabled: false
    url: https://ntfy.example.internal
    topic: agents
    token_file: /etc/soft-serve/secrets/ntfy-token
    click_url: ""               # optional template, e.g. a deep link to the session

snapshots:                      # workstream F, D12
  s3:
    enabled: false              # off by default; nothing below is read or validated when false
    endpoint: ""                # empty for AWS; set for MinIO, Ceph RGW, etc.
    region: ""
    bucket: ""
    prefix: soft-serve/sessions
    credentials_file: /etc/soft-serve/secrets/s3-credentials
    sse: AES256                 # or aws:kms, or "" for none
    on: [end]                   # add "suspend" to snapshot before each park
```

## 10. TUI menu

Screen mockups, the review of the existing TUI, the status glyphs and the key map are in `tui-screens.md`.

Today the home page has two tabs: Repositories, About. Proposed:

| Tab | Shows | Keys | Source |
|-----|-------|------|--------|
| Repositories | As today, plus an origin badge (local, github, gitlab) and a dot when the repo has live sessions | `enter` open, `s` new session | existing |
| Sessions | Two panes. Sessions: repo, name, status glyph, kind, message, idle time. Signals: every OSC 7501 report and lifecycle event, newest first | `enter` attach (from either pane), `e` switch pane, `n` new, `x` end | C1, H1 |
| Forges | Connected forges and their repos: imported or not, last sync, ahead/behind | `i` import, `r` sync now, `p` push | E |
| About | As today | | existing |

Inside a repository, add a **Sessions** tab beside Readme, Files, Commits, Branches, Tags: that repo's sessions only, same row layout and keys.

Rules that keep it one menu:
- Every tab is a full-width list like today's home, with the same filter (`/`), copy and help keys. There is no detail pane; Sessions uses two-line rows.
- One status vocabulary everywhere: the OSC 7501 states, with one glyph and colour each, used in the TUI, `session ls` and ntfy titles.
- CLI nouns mirror the tabs: `soft repo`, `soft session` (with `soft session signals` for the feed), `soft forge`.
- Tabs for features that are off in the config are hidden, not greyed out.

## 11. Not placed yet

- **"plus web server"** from the zellij note. Possible readings: a browser terminal for sessions, a web view of the session list, or port-forwarding a dev server out of a session pod (osewa has `forward`). Needs a sentence from me on which.
- **Multi-user.** Everything above assumes a single user with one set of forge tokens. Sharing sessions or repos with others is out of scope until said otherwise.
- **Resource limits and cleanup.** No quotas, idle timeouts or garbage collection for session pods are planned yet.

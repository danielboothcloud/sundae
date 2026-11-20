# Product

<!-- impeccable:product-schema 1 -->

## Platform

terminal

Sundae's interface is a terminal UI (TUI) served over SSH (`ssh -t host`), built with Bubble Tea and Lip Gloss, plus a command line over SSH (`ssh host repo …`, `ssh host session …`). There is no web UI. None of `web`, `ios`, `android` or `adaptive` applies; design guidance for those platforms does not apply here.

## Users

One user: the owner (Daniel), on a personal homelab Kubernetes cluster, reached over Tailscale from a laptop terminal or a phone SSH client. He keeps his git repositories on Sundae, and he runs coding agents (pi, claude and others) in Sundae sessions that keep running after he disconnects. He checks in on them, answers the ones that are blocked, and moves on.

Multi-user features inherited from Soft Serve (collaborators, access levels, anonymous access, user tokens) still work, but no other user is confirmed.

## Product Purpose

Sundae is two things on one server:

1. A home for git repositories. It sees the owner's GitHub and GitLab repositories, imports them, and keeps them in sync, so work can live on Sundae without being pushed to a forge.
2. A dev server for agent sessions. Each session is a sandbox (a Kubernetes Pod with its own volume) that runs a program, usually an agent, inside a persistent osewa terminal session. Sessions keep running when the owner disconnects; idle ones suspend to save resources.

Success: the owner can see at a glance which agents need him, get into one in a key press, get out without losing anything, and manage repositories and forges in the same place.

## Positioning

A git server and an agent cockpit in one SSH-reachable TUI. Agents report their state with the Program Status Protocol (OSC 7501); Sundae collects it from every session and orders the work by what needs the owner (blocked, error, done, working, idle), with ntfy notifications on changes. The repository, its forge, and the sessions that hold it are all one place.

## Operating Context

- Entry point: `ssh -t sundae` (Tailscale host) opens the TUI; `ssh -t sundae session attach NAME` attaches directly.
- Home tabs: Repositories, Sessions (with Signals), Forges. A repository page has its own tabs, including Sessions.
- While attached, the session owns the whole terminal; `Ctrl-]` returns to the Sessions screen and a one-line status bar shows the session and the key. osewa's `~.` also detaches.
- Deployed by Argo CD from a GitOps repository (Helm chart `helm/sundae`, helmfile overlay). The sandbox image comes from the owner's dotfiles repository.
- Session runtimes: `pod` (in use), `docker`, `openshell`.

## Capabilities and Constraints

- Git over SSH, HTTP and git protocol; Git LFS; jj repositories alongside git.
- Forges: GitHub and GitLab discovery, import, scheduled sync, fast-forward push; listings persist across restarts.
- Sessions: start, attach, suspend, resume, delete; profiles; shared volumes (e.g. a mise cache at `/mise`); idle auto-suspend after 30 minutes.
- Signals feed and ntfy notifications from agent status.
- Terminal constraints: 80 columns is the design width, and narrow terminals (50 columns, phone clients) must still work. Colour comes from a fixed Lip Gloss style set. The TUI runs inside an SSH session, not a local terminal.
- Removed: S3 snapshots (config keys fail `config check`).
- Open: panes or splits across sessions (osewa's roadmap lists a host-side multiplexer); a browser terminal; resource usage per session.

## Brand Commitments

- Name: Sundae. A fork of Soft Serve by Charm, MIT licensed; the Charm attribution stays.
- Soft Serve's TUI is the starting point, not a binding style: Sundae may develop its own look, but it must read as one cohesive application.
- Status vocabulary is fixed: `▲ blocked`, `✗ error`, `✓ done`, `● working`, `○ idle`, `· none`. A glyph never appears without its word.

## Evidence on Hand

- Screen specification and mockups: `_bmad-output/planning-artifacts/tui-screens.md`.
- Roadmap and decisions: `_bmad-output/planning-artifacts/roadmap.md`.
- Golden renders of TUI screens: `pkg/ui/pages/*/testdata/*.golden`.
- No users other than the owner, no metrics, no testimonials. Do not invent any.

## Product Principles

1. What needs the owner comes first: order, notices and notifications follow the status vocabulary.
2. Leaving never loses work: detaching, suspending and switching keep the program running; only delete destroys, and it says so before it acts.
3. Discoverable over memorised: every available key is visible where it applies; no hidden prefix sequences.
4. One cohesive interface: prompts, confirms and notices belong to the screen they act on, in the same visual language as the rest of the TUI, and stay short.
5. Works from a phone: narrow widths and slow links are normal, not edge cases.

## Accessibility & Inclusion

Glyphs always carry a word, so state does not depend on colour or symbol alone. No other requirement was established.


- source_spec: `_bmad-output/implementation-artifacts/spec-jj-first-class-repos.md`
  summary: Batch or cache per-commit `git cat-file` change-ID lookups in the TUI log.
  evidence: one subprocess per commit per page load in jj repos; fine for page sizes, slow on large pages/disks.
- source_spec: `_bmad-output/implementation-artifacts/spec-jj-first-class-repos.md`
  summary: Add curl testscript assertions for `X-Soft-Serve-VCS` (jj, git, unauthorized) and go-get for a git repo; honor HideCloneCmd in go-get.
  evidence: header and go-get git/jj distinction have no test; mutation of either passes the suite.
- source_spec: `_bmad-output/implementation-artifacts/spec-jj-first-class-repos.md`
  summary: Add unit tests for TUI jj bits (Refs.TabNameFor, Tabs.SetTabs, empty-repo message, log item width) and a DB CHECK/fallback for unknown vcs values.
  evidence: no TUI harness exists; unknown vcs only reachable by manual DB edit.
- source_spec: none
  summary: S3 snapshot/restore of an OpenShell session's /sandbox working directory (single tarball replaced per session end, SSE, S3 access keys, S3-wins with user prompt when the PVC is non-empty), hooked into the session start/end lifecycle.
  evidence: split from the OpenShell-sessions intent; independently shippable on top of the session lifecycle (spec A).
- source_spec: none
  summary: Options when initialising new projects — choice of source control (git/jj), agents, and related defaults.
  evidence: split from the OpenShell-sessions intent; unrelated to session lifecycle and shippable on its own.
- source_spec: `_bmad-output/implementation-artifacts/spec-openshell-sessions.md`
  summary: TUI menu entry to list/start/resume/end OpenShell sessions from the Soft Serve browser.
  evidence: spec A ships the SSH `session` command first; the TUI menu is a separate UI deliverable on top of it.
- source_spec: `_bmad-output/implementation-artifacts/spec-openshell-sessions.md`
  summary: Bridged OpenShell output passes through charm ssh's EmulatePty writer, which rewrites every bare `\n` to `\r\n`. Raw-mode TUIs in the sandbox may render wrongly. (medium, unverified)
  evidence: `charm.land/ssh` `pty.go:31-32` rewrites all LFs, and `Session.Stderr` is wrapped the same way, so there is no raw path. To settle it, run vim and a crossterm/ratatui app (OPOST off) through `ssh -t … session start` against a live gateway and check the rendering.
- source_spec: `_bmad-output/implementation-artifacts/spec-openshell-sessions.md`
  summary: `sdkClient.resume` treats Stopping, Suspending, Suspended and Completed as fatal and tells the user to end the session, even though waiting or `Resume` could recover it. (medium, unverified)
  evidence: The SDK defines these phases (`types/types.go:18-23`), and `resume` sends them to its default error branch. To settle it, confirm whether the gateway ever moves Soft Serve sandboxes into these phases (idle suspend or stop policies).
- source_spec: `_bmad-output/implementation-artifacts/spec-openshell-sessions.md`
  summary: Resuming a Stopped sandbox may fail if the gateway's StartSandbox returns before the phase leaves Stopped, because WaitReady treats Stopped as terminal. (medium, unverified)
  evidence: SDK `sandbox_client.go:356` makes `checkTerminalPhase` return "stopped before reaching Ready" on its first poll. The fake flips straight to Ready. To settle it, check the gateway's StartSandbox semantics, or resume a stopped sandbox against a live gateway.

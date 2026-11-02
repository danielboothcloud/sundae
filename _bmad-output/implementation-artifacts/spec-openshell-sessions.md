---
title: 'OpenShell agent sessions launched from Soft Serve'
type: 'feature'
created: '2026-11-02'
status: 'done'
baseline_commit: '51a958827617e3525e57805d213e07fa2043af99'
route: 'dispatch'
review_loop_iteration: 0
context: []
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** Soft Serve hosts git/jj repos but cannot start somewhere to work on them. The user wants Soft Serve (running in Kubernetes) to act as the menu that starts and resumes agent sessions. Each session runs in a separate NVIDIA OpenShell sandbox pod.

**Approach:** Add an opt-in `openshell` config section and an SSH `session` command group (`start`, `ls`, `end`). These drive an OpenShell gateway through its official Go SDK. There is one sandbox per (user, repo); it clones the repo into `/sandbox`, and the user's SSH terminal is bridged to an interactive shell inside it. Soft Serve keeps no state: sandbox names are deterministic and the gateway is the source of truth.

## Boundaries & Constraints

**Always:**
- The feature is off unless `openshell.enabled`. When disabled, the `session` command is not registered.
- Only authenticated users may use it, and they need read access to the repo (`checkIfReadable`).
- Sandbox name: `soft-<user>-<repo>`, sanitized to a DNS label of at most 63 characters. Names that are too long or that collide get a short hash suffix.
- The first clone runs only when `/sandbox/<repo>` does not exist. jj repos use `jj git clone --colocate`, git repos use `git clone`. The image is expected to provide both tools.
- Git credentials: a short-lived Soft Serve access token, named `openshell:<sandbox>`, passed only as env vars on the exec call (`GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_0=http.extraHeader`/…). It is never written into `.git/config` or any file.
- `session end` revokes that token.
- The OpenShell client sits behind a small interface so tests can use the SDK's `fake` client.

**Never:**
- No S3 backup/restore. That is the next spec, and its hook points are start and end.
- No TUI menu entry.
- No project-init options.
- No new DB tables or migrations.
- Do not shell out to the `openshell` CLI.
- Do not switch the SSH server from `EmulatePty` to `AllocatePty`.

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
|----------|--------------|---------------------------|----------------|
| New session | `ssh -t soft session start r` with no sandbox | Create the sandbox, wait until ready, clone, then bridge to a shell in `/sandbox/r` | Creation or ready-wait fails → error, and delete the half-made sandbox |
| Resume | Sandbox exists (Ready, or Stopped → Start) | Skip the clone and bridge | — |
| No PTY | `ssh soft session start r` | Error: "requires a terminal (ssh -t)" | exit 1 |
| No access / anonymous | user cannot read `r` | Same "not found/unauthorized" as other repo commands | exit 1 |
| List | `session ls` | One row per caller's sandbox: repo, name, phase | — |
| End | `session end r` | Delete the sandbox and revoke the token | Already absent → success no-op |
| Disabled | `openshell.enabled=false` | `session` is an unknown command | — |
| Detach | User exits the bridged shell | Sandbox keeps running; `start` re-attaches; only `end` deletes | — |
| Custom image | `session start r --image img` with no sandbox | Sandbox created from `img` | Flag ignored with a warning when the sandbox already exists |

### Decisions

- **Shell exit:** the sandbox stays running so agents keep working. `start` re-attaches, and only `end` tears it down.
- **Image and command are configurable:** `openshell.image` sets the default base image, and `session start --image` overrides it when the sandbox is first created. `openshell.command` sets what runs on attach, defaulting to `bash -l`.
- **Push access:** read/write, using the user's token. Agents can push. The token is visible to processes in the sandbox; that risk is accepted.

</frozen-after-approval>

## Code Map

- `pkg/ssh/middleware.go` `CommandMiddleware`: every PTY session goes to the TUI (`if ptyReq { sh(s) }`). Add a carve-out so that when `s.Command()[0]=="session"` it falls through to cobra. Register `cmd.SessionCommand()` there when `cfg.OpenShell.Enabled`, following the LFS pattern.
- `pkg/ssh/cmd/cmd.go`: provides `checkIfReadable` and the helper patterns for commands.
- `pkg/ssh/cmd/repo.go`: example of a command group.
- `pkg/sshutils/utils.go` `SessionFromContext`: gives the `ssh.Session`. Use `Pty()` for the initial size and the window-change channel, and `Read`/`Write` for the bridge. `EmulatePty`'s writer only converts `\n` to `\r\n` and leaves existing `\r\n` alone, so passthrough is safe.
- `pkg/ssh/ssh.go`: `IdleTimeout` defaults to 600s. Bridge traffic counts as activity, but mention it in the README.
- `pkg/config/config.go`: add `OpenShell OpenShellConfig` with `envPrefix:"OPENSHELL_" yaml:"openshell"`. Update `DefaultConfig`, `Environ()` and `Validate()` (require address and image when enabled). Add the YAML template block in `pkg/config/file.go`. Fields: `enabled`, `gateway_address`, `workspace` (default `default`), `image`, `token`, `tls_ca`, `tls_cert`, `tls_key` (files, e.g. from the `openshell-client-tls` secret), `insecure`, `clone_url` (internal HTTP base URL that sandboxes can reach; defaults to `http.public_url`), `token_ttl` (default 24h), `command` (default `bash -l`).
- `pkg/backend/access_token.go`: reuse `CreateAccessToken`, `ListAccessTokens` and `DeleteAccessToken`. Access tokens are accepted as the basic-auth password (`pkg/web/auth.go`).
- `pkg/proto/repo.go`: `VCS` kind (git/jj) of the repo.
- SDK `github.com/NVIDIA/OpenShell/sdk/go/openshell/v1` (no tagged releases, so pin a pseudo-version): `NewClient(Config{Address, Auth, TLS})`, `Sandboxes().Create/Get/List/Start/Delete/WaitReady`, `Exec().Run` (clone, existence check) and `Exec().Interactive(..., cols, rows, ExecOptions)` → `Read/Write/Resize/ExitCode`. `openshell/v1/fake` is used for tests. Check the exact field names in `ExecOptions`/`SandboxSpec` against the pinned version.
- `testscript/testdata/repo-vcs.txtar`: model for an end-to-end test of "disabled → unknown command" and the no-PTY error.

## Tasks & Acceptance

**Execution:**
- [x] `go.mod` -- add `github.com/NVIDIA/OpenShell/sdk/go` -- typed gRPC client plus fake.
- [x] `pkg/config/config.go`, `pkg/config/file.go`, `pkg/config/config_test.go` -- `OpenShellConfig`, env parsing, validation tests -- configuration surface.
- [x] `pkg/openshell/openshell.go` (new) -- narrow `Client` interface (ensure/get/list/delete sandbox, run, interactive), SDK-backed constructor from config, `SandboxName(user, repo)` -- testable seam.
- [x] `pkg/openshell/session.go` (new) -- `Start` (ensure, wait, clone if missing, issue token, build env), `End` (delete, revoke token), and `Bridge(sess, interactive)` (copy in both directions, resize on window change, return the exit code) -- core logic kept apart from cobra.
- [x] `pkg/openshell/*_test.go` -- cover the I/O matrix with the fake client: name sanitizing, new/resume/stopped, cleanup after a failed create, idempotent end, and the token env never appearing in the clone URL.
- [x] `pkg/ssh/cmd/session.go` (new) + `pkg/ssh/middleware.go` -- `session start [--image] | ls | end`, PTY carve-out, and conditional registration.
- [x] `testscript/testdata/session.txtar` -- disabled → unknown command; enabled without a PTY → terminal error.
- [x] `README.md` -- OpenShell section: config, client TLS secret, the sandbox egress `network_policies` example that allows git to reach Soft Serve's internal HTTP, and the idle timeout.

**Acceptance Criteria:**
- Given an `ssh -t` session with the feature enabled, when the user runs `session start r` twice, then the second run attaches to the same sandbox without cloning again.
- Given a running bridged session, when the client terminal is resized, then the sandbox PTY is resized.
- Given a bridged shell, when it exits with code N, then the SSH session exits with N.

## Implementation Notes

## Spec Change Log

## Review Triage Log

Review iteration 1 (layers: blind-hunter B, edge-case-hunter E, verification-gap V).

| ID | Finding | Verdict | Evidence | Route |
|----|---------|---------|----------|-------|
| B1, E12 | `sessions` alias with `-t` goes to the TUI | low | `CommandMiddleware` compares only `CommandName(args)` (`args[0]`) with `"session"`. | patch: drop the alias |
| B2, E6 | `create` → `EnsureSandbox` recursion when Create says AlreadyExists and Get says NotFound | low | The loop is real and spins while the gateway stays inconsistent. Reaching that state is unlikely. The fix is a direct correction. | patch |
| B3 | The sandbox token has the user's full, cross-repo power | medium | Real, but the frozen Decisions accept it ("read/write, using the user's token… risk is accepted"). | reject: intent |
| B4 | Unexpired tokens pile up with each start | low | Real, but all are revoked by `end` and expire after the TTL. Token values can't be recovered for reuse, so a fix needs new machinery. | reject |
| B5 | Agents lose git access after `token_ttl` without being told | low | `UserByAccessToken` rejects expired tokens. The README doesn't mention it. A docs-only fix. | patch: README |
| B6, E9a | `session end` fails when the repo was deleted or renamed | medium | `end` calls `be.Repository`, which returns ErrRepoNotFound. `checkIfReadable` passes for repos that don't exist (users get ReadWrite), so the lookup alone blocks ending the sandbox. The sandbox and its tokens are orphaned. | patch |
| B7, E9b | A username rename orphans the sandbox | low | Real, but renames are rare, the name format comes from the intent, and a fix means label-based lookup. | reject |
| B8 | Any image via `--image`, no quota, no admin commands | low | `--image` is granted by the intent. Quotas and admin views are new features. Low impact on an authenticated server. | reject |
| B9 | `clone_url` falls back to a localhost `public_url` | low | Matches the spec'd default. The failure shows up loudly as a git clone error. | reject |
| B10, E14 | TLS settings with an `http://` gateway | false | The SDK's `internal/grpc/conn.go:41-43` rejects that combination with an explicit error. There is no silent plaintext. | reject |
| B11 | Missing tests: blank workspace, PTY `ls`/`end`, alias | low | Confirmed missing. | patch (grouped with V4, V5) |
| E1 | EmulatePty's writer rewrites bare `\n` from raw-mode apps | maybe-false | `charm.land/ssh` `pty.go:31-32` rewrites every `\n` to `\r\n`, and `Stderr` does the same, so there's no raw path. Whether real sandbox TUIs (vim, crossterm apps with OPOST off) emit a bare LF and render wrongly needs a live check. | defer (medium, unverified) |
| E2 | Winch not drained before Bridge blocks the request loop | low | Real: `winch` has a buffer of 1 and sends block. The only harm is a goroutine leak after a resize during a failed start or `ls -t`. Fixing it adds a drain goroutine. | reject |
| E3 | A plain sandbox name can equal another user's hashed name | medium | Reproduced: `al` + `ice-repo-a60cc299` equals the hashed name for `al-ice` + `repo`. The victim gets ErrNotOwned. | patch: `--` before the hash |
| E4 | Stopping, Suspending or Suspended sandbox → "end the session" error | maybe-false | The SDK has these phases. Whether the gateway reaches them when Soft Serve never suspends depends on gateway policy. | defer (medium, unverified) |
| E5 | Asynchronous Start: WaitReady sees Stopped and fails | maybe-false | `checkTerminalPhase` treats Stopped as terminal. Whether `StartSandbox` returns before the phase changes depends on the gateway. | defer (medium, unverified) |
| E7 | Two concurrent starts both clone | low | Rare. The second start fails loudly. A fix adds complexity. | reject |
| E8 | A partial clone dir skips cloning forever | low | git cleans up on failure in most cases. The proposed `.git` check would not help, because git creates `.git` first. | reject |
| E10 | Bridge transport error is swallowed and the exit status is -1 | medium | The SDK `ExitCode` returns `(-1, err)` when no exit event arrives. `session.go` then calls `sess.Exit(-1)` and drops `err`. | patch |
| E11 | A 0x0 window from pty-req | low | Rare client behaviour, and a fix adds a guard. | reject |
| E13 | Token left live after a failed start | low | It expires after the TTL and is revoked by `end`. A fix adds an error branch. | reject |
| V1 | Keeping unexpired tokens is untested | low | Pre-verified mutation survived. | patch |
| V2 | The AlreadyExists race branch is untested | low | Pre-verified mutation survived. | patch |
| V3 | The resume Provisioning/Starting branch is untested | low | Pre-verified mutation survived. | patch |
| V4 | The session subcommands are never exercised past the gateway | medium | Pre-verified: removing `sess.Exit(code)` passes all tests. | patch |
| V5 | The blank-workspace validation is untested | low | Pre-verified mutation survived. | patch |

## Design Notes

Why the SDK and not the CLI: the SDK's `Interactive` returns a byte stream with `Resize`, so Soft Serve needs no server-side PTY, no binary in its image, and no `~/.config/openshell` state. The fake client also makes the lifecycle unit-testable.

How the S3 spec plugs in: `Start` (restore before the clone check) and `End` (snapshot before delete). Keep those as clear seams.

## Verification

**Commands:**
- `go build ./... && go vet ./...` -- expected: clean
- `go test -race ./pkg/openshell/... ./pkg/config/... ./pkg/ssh/...` -- expected: pass
- `go test ./testscript -run TestScript/session` -- expected: pass

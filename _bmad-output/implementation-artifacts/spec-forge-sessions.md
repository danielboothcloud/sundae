---
title: 'Forge sessions: forwarded pushes, gh/glab, Sundae-only files, jj-only imports'
type: 'feature'
created: '2026-11-20'
status: 'done'
route: 'dispatch'
review_loop_iteration: 0
context:
  - '_bmad-output/planning-artifacts/roadmap.md (section E, D3, F4)'
  - '_bmad-output/implementation-artifacts/spec-jj-first-class-repos.md'
  - '_bmad-output/implementation-artifacts/spikes/forge-sessions/'
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** An agent in a session can push only to the Sundae replica. It cannot publish to GitHub or GitLab, so CI never runs, and `gh` and `glab` cannot find the repository. You also need files (for example `AGENTS.md`, `_bmad/`) that Sundae keeps but the forge never receives.

**Approach:** The session pushes to Sundae only. The Sundae pre-receive hook forwards the pushed bookmarks to the linked forge by per-forge rules. The forward is synchronous and atomic, and Sundae accepts the push only if the forge accepts it. A fetch-only `origin` remote points at the forge, so `gh` and `glab` work with the forge tokens Sundae already has. Sundae-only files live in a separate history, `refs/sundae/private`. Sundae restores them at clone and saves them on request. All forge imports are jj.

## Decisions (2026-11-20)

- **Forward:** every bookmark is forwarded by default (`forward: ["**"]`; `*` does not cross `/`). Bookmarks that match `keep` stay in Sundae only (for example `push-*` from `jj git push -c @`).
- **Default branch:** sessions cannot push the default branch of a forge repository. `protect_default_branch: false` on a forge turns this off.
- **Extra protection** per forge: `protect.deny` (sessions cannot push) and `protect.fast_forward_only` (no rewrite, no delete).
- **Tokens:** the sandbox gets the same `token_file` that the forge already uses, as `GH_TOKEN` or `GITLAB_TOKEN`. No new token configuration.
- **Private paths:** `private_paths` per forge, with a per-repository override. A path that the forge repository already tracks is skipped with a warning.
- **jj only:** forge and URL imports always create `vcs=jj`. Remove `--vcs` from `forge import`, `repo import` and the TUI import form. No data migration: the deployment starts from an empty volume.

## Accepted risk

The sandbox token can push to the forge directly (`git push https://…`), outside Sundae. The pod runtime gives the real token as an environment variable. OpenShell gives a placeholder, but its proxy still adds the real token. A determined agent can then avoid the forward, protect and private-path rules. Branch protection on the forge is the only hard guard against that. The Sundae rules guide normal agent use and block mistakes. Remediation options (OpenShell L7 allowlist, GitHub App tokens for each session, GitLab project tokens) are in `todo.txt` (2026-11-20).

## Boundaries & Constraints

**Always:**
- Forward all matching refs of one push in one `git push --atomic`. If the forge refuses any ref, refuse the whole Sundae push.
- Before the forward, read each forge ref with `git ls-remote`. Allow the ref when the forge value equals the old Sundae value, or when the new commit contains the forge value. Else refuse with: run `jj git fetch`, rebase onto `<bookmark>@origin`, push again.
- Push with `--force-with-lease=refs/heads/<b>:<forge value>` and a refspec without `+`. (Spike: a `+` refspec silently disables the lease and drops another writer's commit.)
- Refuse a forwarded commit that adds a private path (`git diff-tree --diff-filter=A`), unless the forge repository already tracks that path.
- After a successful forward, move `refs/remotes/<forge>/heads/<b>` to the new value.
- Take the forge service per-repository lock for the forward, so it does not run at the same time as sync or `forge push`.

**Never:**
- Never forward tags, `refs/sundae/*`, or refs of a repository that has no forge link.
- Never write a forge token into a repository, a URL, or the Sundae logs.
- Never run `jj` on the server.

## I/O & Edge-Case Matrix

Each row is covered by `spikes/forge-sessions/t5-cases.sh` or `t5-c4.sh` (local bare repositories, jj 0.47.1).

| Scenario | Input / State | Expected | Spike |
|---|---|---|---|
| Forward | push `develop`, rules forward `*` | forge and replica move; exit 0 | case 1 |
| Sundae-only | `jj git push -c @`, `keep: ["push-*"]` | replica only | case 2 |
| Rewrite | `jj describe` then push | forwarded with lease | case 3 |
| Forge moved | another writer pushed to the forge | refused, with fetch/rebase hint; nothing moves | case 4, t5-c4 |
| Recover | fetch, rebase onto `develop@origin`, push | forwarded | case 5, t5-c4 |
| Partial forge reject | two refs, forge refuses one | nothing moves on either side | case 6 |
| Private path | commit adds `AGENTS.md` | refused | case 7 |
| Push to origin | `jj git push --remote origin` | refused by jj | case 8 |
| Fast-forward only | rewrite a `fast_forward_only` ref | refused; fast-forward allowed | cases 9–10 |
| Deny | push a denied ref | refused | case 11 |
| Private files | new session clone | files restored, invisible to `jj status`, absent on forge | `t4.sh` |

</frozen-after-approval>

## Design

**Session remotes** (colocated jj clone):

| Remote | Fetch | Push |
|---|---|---|
| `sundae` (default) | Sundae replica | Sundae replica; the hook forwards |
| `origin` | the forge, direct, with the forge token | disabled (`pushurl` set to an invalid URL) |

Repository jj config, written at clone:
`git.push = "sundae"`, `git.fetch = ["sundae", "origin"]`, `remotes.sundae.auto-track-bookmarks = "*"`. `origin` bookmarks are not tracked, so they show forge state (`develop@origin`) without making local bookmark conflicts.

**Forge config:**
```yaml
forges:
  - name: github
    forward: ["**"]                # default
    keep: ["push-*"]               # Sundae only
    protect_default_branch: true   # default
    protect:
      deny: []
      fast_forward_only: ["develop"]
    private_paths: ["AGENTS.md", "_bmad/", ".claude/"]
```
Per-repository override of `private_paths`: repository git config `sundae.privatepath` (multi-value), set with `ssh <server> repo private-paths <repo> [paths...]`. No DB change.

**Private files:** one commit chain on `refs/sundae/private` in the Sundae repository. It is built with plumbing (temporary index, `write-tree`, `commit-tree`), so branch history never contains these files.
- At clone: fetch the ref, extract it into the workspace, and add the paths to `.git/info/exclude`.
- At save: run the plumbing in the sandbox and push the ref to `sundae`. If the push is not a fast-forward, fetch, re-parent on the new tip, and retry once (this session's content wins).
- Triggers: suspend, delete, and `ssh <server> session save-private <session>`.

## Code Map

- `pkg/config/forge.go`, `pkg/config/file.go` -- new forge fields, defaults, validation, YAML template docs.
- `pkg/hooks/hooks.go`, `pkg/backend/hooks.go`, `cmd/sundae/hook/hook.go` -- `PreReceive` returns an error; the hook exits non-zero and prints the reason to the pusher.
- `pkg/forge/forward.go` (new) -- rule matching, ls-remote check, private-path guard, atomic leased push, namespace update. Reuse `gitEnv`, `linkRemote`, `lock`, `namespace` from `service.go`, `sync.go`, `push.go`.
- `pkg/forge/service.go` -- `Import` always uses `proto.VCSJJ`; `Origin`/link exposes the forge web URL and default branch for sessions.
- `pkg/ssh/cmd/forge.go`, `pkg/ssh/cmd/import.go`, `pkg/ui/pages/forges/{forges,source}.go` -- remove `--vcs` and the import-form field.
- `pkg/openshell/session.go`, `pkg/openshell/identity.go`, `pkg/openshell/credentials.go` -- clone with `--remote sundae`; add `origin`; write jj config; credential entries for the forge host; restore and save private files.
- `pkg/sessionctl/sessionctl.go` -- `RepoSpec` carries the forge link (web URL, kind, default branch, private paths).
- `pkg/openshell/policy.go` -- built-in egress rules for the forge git host and API host (`api.github.com`, or the GitLab host).
- Token delivery -- OpenShell provider generated from each forge `token_file` (`GH_TOKEN`, `GITLAB_TOKEN`, `GITLAB_HOST` for self-hosted); env for the pod and docker runtimes.
- `deploy/sandbox/Containerfile` -- add pinned, checksum-verified `gh` and `glab`.
- `pkg/ssh/cmd/` -- `repo private-paths`, `session save-private`.
- `README.md`, `roadmap.md` section E -- document the model and the accepted risk.

## Tasks & Acceptance

**Execution:**
- [x] config: forge fields + validation + template docs + tests
- [x] hooks: `PreReceive` can refuse; testscript proves a refused push leaves refs unchanged
- [x] forge: `forward.go` + unit tests for every matrix row (port `t5-cases.sh`)
- [x] imports: jj only; remove `--vcs` and the TUI field
- [x] sessions: remotes, jj config, forge credentials, egress, token delivery
- [x] image: `gh` and `glab`
- [x] private files: restore at clone, save on triggers, guard, commands
- [x] docs: README, roadmap, config template

**Acceptance Criteria:**
- Given a session on an imported repository, when the agent runs `jj git push -b feature/x`, then the forge has `feature/x` and CI starts.
- Given the same session, when the agent runs `gh run list` or `glab ci list`, then it lists the forge runs without extra setup.
- Given `keep: ["push-*"]`, when the agent runs `jj git push -c @`, then only Sundae has the bookmark.
- Given the default config, when the agent pushes the default branch, then Sundae refuses and the forge is unchanged.
- Given `private_paths: ["AGENTS.md"]`, when a new session starts, then `AGENTS.md` is in the workspace, `jj status` is clean, and the forge has no `AGENTS.md`.
- Given a forge with commits the agent lacks, when the agent pushes, then Sundae refuses with the fetch/rebase hint and no ref moves.

## Open items

- Real-forge checks still needed: atomic push and lease on GitHub and GitLab; OpenShell placeholders for git HTTPS fetch and for `gh`/`glab` API calls.
- Push latency: each push waits for one `ls-remote` and the forge push. Check the HTTP and SSH timeouts.
- If the forge accepts and the Sundae ref update then fails, the forge is ahead until the next sync fast-forwards the replica. Log it as a warning.
- Tags are not forwarded in this version.

## Verification

**Commands:**
- `go build ./... && go test ./...` -- expected: pass
- `golangci-lint run` -- expected: clean
- `bash _bmad-output/implementation-artifacts/spikes/forge-sessions/t5-cases.sh` with `CASE=main` and `CASE=protect`, and `t5-c4.sh` -- expected: the results in the matrix
- Manual: one GitHub and one GitLab test repository, end to end, per the acceptance criteria.

## Review Triage Log

| Finding | Verdict | Route | Evidence |
|---|---|---|---|
| Default `forward: ["*"]` skips `feature/x` | high | patch | default is now `**`; config test |
| Private-path guard skipped merge commits | high | patch | `diff-tree -c`; `TestForwardPrivatePathInMerge` |
| Private save reverted other sessions' newer saves | high | patch | per-file base ref, changed paths only; `TestPrivateScriptsConcurrent` |
| Forward ran before custom pre-receive hook | medium | patch | custom hook runs first; post-receive warns on divergence |
| `fast_forward_only` skipped for kept branches | medium | patch | `TestForwardFastForwardOnlyKept` |
| Delete of suspended session lost unsaved private files | medium | patch | refused without `--discard-private` |
| Windows build broke on `syscall.Flock` | low | patch | build tags; `GOOS=windows go build ./...` |
| Unconfigured forge silently dropped protection | low | patch | warning to pusher and log |
| Control characters in private paths | low | patch | `NormalizePrivatePath` rejects them |
| `jj git fetch` failed when origin unreachable | low | patch | `origin` in `git.fetch` only with a token |
| `allowForgeEgress` opened in-cluster 443 | low | patch | `forgeEgressExcept` defaults |
| OpenShell gets the real forge token | low | defer | documented; see `todo.txt` 2026-11-20 |

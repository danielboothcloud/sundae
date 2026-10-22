---
title: 'First-class Jujutsu (jj) repos alongside git'
type: 'feature'
created: '2026-10-22'
status: 'done'
baseline_commit: '14250567539d5d4945e7f54c43761ac959f58df9'
route: 'dispatch'
review_loop_iteration: 0
context: []
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** Soft Serve only knows git repos. jj users push/fetch through git remotes, but the server, TUI, web and docs present everything in git terms (branches, commit hashes, `git clone`).

**Approach:** Add a per-repo `vcs` kind (`git` | `jj`). Storage and transport stay a bare git repo (jj has no wire protocol). jj repos get jj-native presentation: bookmarks, change IDs, `jj git clone` instructions, across Server/CLI, TUI browser, HTTP/web and docs.

## Boundaries & Constraints

**Always:** New and imported repos default to `vcs=jj` when `--vcs` is omitted (decision). Existing repos migrate to `vcs=git` with unchanged behavior. `vcs` is set at create/import and immutable afterwards. Server needs no `jj` binary: change IDs come from the `change-id` header jj writes into pushed git commits; commits without it show the short hash.

**Never:** Do not rename `pkg/git`, remove git support, or change wire protocols/auth. Do not shell out to `jj` on the server.

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
| Create repo, no flag | `repo create foo` | Row `vcs=jj`; prints `jj git clone <url>` | N/A |
| Create jj repo | `repo create foo --vcs jj` | Row `vcs=jj`; prints `jj git clone <url>` | invalid `--vcs` value → error |
| jj commit with change-id | pushed commit with header | log/commit show change ID + short commit id | N/A |
| Commit lacking change-id | plain git commit in jj repo | falls back to short hash | N/A |
| Legacy repo | pre-migration row | `vcs=git`, git UI unchanged | N/A |

</frozen-after-approval>

## Code Map

- `pkg/db/migrate/` -- add `0004_repo_vcs` (sqlite+postgres up/down SQL, register in `migrations.go`); `repos.vcs TEXT NOT NULL DEFAULT 'git'`
- `pkg/db/models/repo.go`, `pkg/store/repo.go`, `pkg/store/database/repo.go` -- add `Vcs` field; extend `CreateRepo` signature
- `pkg/proto/repo.go` -- `Repository.Vcs()`, `RepositoryOptions.Vcs`, `VCS` type/consts
- `pkg/backend/repo.go` -- `CreateRepository`/`ImportRepository` pass vcs; `repo` wrapper impl
- `pkg/ssh/cmd/{create,import,branch,commit,info,list}.go` -- `--vcs` flag; `bookmark` alias for `branch`; show change ID/vcs
- `git/commit.go` -- helper `ChangeID(*Commit)` parsing `change-id` from raw commit header (reuse git-module)
- `pkg/ui/common/common.go` (`CloneCmd`), `pkg/ui/pages/repo/{empty,log,logitem,refs,refsitem,repo}.go` -- jj clone cmd; "Bookmarks" label; change ID column
- `pkg/web/` -- clone URL display/headers for jj repos
- `README.md` -- jj section; testscript in `testscript/` for create/import

## Tasks & Acceptance

**Execution:**
- [ ] `pkg/db/migrate/0004_*` -- add vcs column + register -- persist kind
- [ ] store/model/proto/backend -- thread `Vcs` end to end -- single source of truth
- [ ] `git/commit.go` -- `ChangeID` + unit test (header, missing header)
- [ ] `pkg/ssh/cmd/*` -- `--vcs`, `bookmark` alias, change ID output
- [ ] `pkg/ui/**` -- jj terminology and clone cmd when `Vcs()==jj`
- [ ] `pkg/web/`, `README.md` -- jj clone display and docs
- [ ] tests -- migration, create/import, matrix cases; `testscript` jj scenario

**Acceptance Criteria:**
- Given an existing DB, when migrated, then all repos report `git` and `go test ./...` passes.
- Given a jj repo, when pushed to via `jj git push`, then the TUI log shows change IDs and refs page says Bookmarks.
- Given a jj repo, when viewed in TUI/CLI/web, then the clone hint is `jj git clone <url>`.

## Implementation Notes

## Spec Change Log

## Review Triage Log

| Finding | Verdict | Route | Evidence |
| --- | --- | --- | --- |
| change-id header unsanitized (escape injection) | medium | patch | header is pusher-controlled; now validated `^[A-Za-z0-9]{1,64}$` + tests |
| ShortChangeID slices bytes | false | - | moot: ParseChangeID only returns ASCII |
| per-commit `git cat-file` spawn (TUI log, ChangeID) | low | defer | page-sized lists only; batching is a perf follow-up |
| default jj is breaking for scripts / info wording | false | - | explicit human decision in frozen intent |
| auto-created repos (push-to-create, config repo) become jj | false | - | follows decided default |
| no path to change vcs of existing repo | false | - | spec: vcs immutable |
| README says `repo commit` shows short ID | low | patch | README reworded |
| CloneCmd vs proto.CloneCommand duplication/doc | low | patch | doc comment fixed; unit test added for CloneCmd |
| go-get ignores HideCloneCmd | low | defer | fix adds config plumbing; cosmetic |
| X-Soft-Serve-VCS header / go-get git case untested | medium | defer | needs curl testscript (skipped on windows); header is set post-auth |
| TUI tab/log rendering untested | low | defer | no TUI harness exists |
| no CHECK constraint / unknown Vcs() fallback; PG migration untested | low | defer | only reachable via manual DB edit |
| bookmark alias test lacks stdout assertion | low | defer | cosmetic |

## Verification

**Commands:**
- `go build ./... && go test ./...` -- expected: pass
- `golangci-lint run` -- expected: clean

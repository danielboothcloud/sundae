# Soft Serve TUI: screens

Status: draft, for shaping. Companion to `roadmap.md` (section 10). Mockups are 80 columns.

## 1. Review of the TUI as it is

Read from `pkg/ssh/ui.go`, `pkg/ui/pages/selection/`, `pkg/ui/pages/repo/`, `pkg/ui/keymap/`.

| # | Finding | Consequence for the new screens |
|---|---------|---------------------------------|
| R1 | Home has two tabs, switched only with `tab` / `shift+tab`. | Four tabs is still a lot of presses. Add `1`–`4`. |
| R2 | Home is a plain full-width list. `enter` drills into a repo. There is no detail pane. | Sessions cannot be "list plus detail" as the roadmap said. Use two-line rows; `enter` attaches. |
| R3 | Repo rows take 4 lines each (title, description, clone command, spacer). About 5 fit on a 24-line terminal. | Fine for a few repos, long once forges import dozens. Sessions, Signals and Forges use denser rows. |
| R4 | No prompt, confirm or text-input component exists. The only input is the list filter. | New session and end session need one small shared prompt. |
| R5 | Nothing ticks or subscribes. Lists load once at start. | Sessions and Signals need events delivered as messages (roadmap H1, H4). |
| R6 | Any error replaces the whole screen ("Bummer"). | A gateway outage must stay inside its tab as one line. |
| R7 | The server-name header shows on home only; the repo page has its own header and a status bar. | The attention summary has a place on home. In a repo it needs the status bar. |
| R8 | The TUI runs inside the SSH session on an emulated PTY. | Attaching from the TUI means handing the terminal to the bridge with `tea.Exec` (present in bubbletea v2.0.8) and taking it back on detach. Not yet tried through the emulated PTY. The line-feed rewrite (roadmap F5) applies too. |
| R9 | Keys already taken: `q` `?` `/` `esc` `enter` `tab` `c` `g` `G` `h` `j` `k` `l` `f` `d` `b` `u`; in the Files tab also `p` `b` `l`. | New keys chosen from what is free: `n` `x` `s` `i` `r` `p` `1`–`5`. `p` is push on Forges only. |
| R10 | Anonymous and keyless users can browse. | Sessions and Forges show only to an authenticated user. |

## 2. Status vocabulary

One set, used in the TUI, `soft session ls`, and ntfy titles. A glyph never appears without its word.

| Glyph | Word | Colour | Meaning |
|-------|------|--------|---------|
| `▲` | blocked | amber | Agent needs me. Followed by the kind: permission, question, auth. |
| `✗` | error | red | Agent failed and stopped, or the sandbox failed. |
| `✓` | done | green | Finished; I have not looked yet. |
| `●` | working | teal (the active-tab colour) | Running. Shows progress when reported. |
| `○` | idle | grey | At rest, waiting for my next instruction. |
| `·` | (none) | grey | Never reported a status, e.g. a plain shell. |

Sandbox lifecycle is a second word after the state, not another glyph: `starting`, `parked` (suspended), `resuming`. A parked session keeps its last agent state, so a parked blocked session still reads `▲ blocked · parked`.

Sort order everywhere: blocked, error, done, working, idle, none. What needs me is on top.

## 3. Frame

```
 Soft Serve                                  ▲ 1 blocked  ✓ 1 done  ● 2 working

 Repositories   • Sessions   Forges   About

 (tab content)

 ↑↓ navigate • 1-4 tab • enter attach • n new • x end • / filter • ? help
```

- Header: server name as today. Right side is the attention summary, counted over all my sessions. Zero counts are left out; nothing shows when there are no sessions.
- Tabs: today's dot style. Hidden when their feature is off in the config, or when I am not authenticated (R10).
- Footer: today's help line; its contents change per tab.

## 4. Repositories

Today's row, plus two badges on the title line.

```
 soft-serve                         ● 2 sessions   github ↑3    Updated 2h ago
 Git hub + dev server
 jj git clone ssh://git.example.com/soft-serve

 infra 🔒                           ▲ 1 session    gitlab       Updated 5h ago
 Cluster config
 git clone ssh://git.example.com/infra

 scratch                                           local        Updated 3d ago
 Notes and experiments
 jj git clone ssh://git.example.com/scratch
```

- Session badge: the most urgent state among the repo's sessions, and the count.
- Origin badge: `local`, or the forge name with `↑n ↓n` when local and upstream differ.
- Keys added: `s` new session for this repo. `enter` and `c` as today.

## 5. Sessions

```
 ▲ infra / claude-1                                 blocked · permission    4m
   Apply 3 to add, 1 to change, 0 to destroy?

 ✓ soft-serve / forge-sync                                          done   12m
   Implemented GitHub discovery; 14 tests pass

 ● soft-serve / tui                                          working 40%   now
   Running go test ./pkg/ui/...

 ● gateway / main                                                working    2m
   claude-code

 ○ dotfiles / main                                         idle · parked    3h
   bash
```

- Line 1: glyph, repo / session name, state (kind, progress, lifecycle), time since the status last changed.
- Line 2: the agent's `msg`; when there is none, the `app` or the command.
- Selected row gets today's left border and highlight.
- Keys: `enter` attach, `n` new, `x` end, `z` park or resume now, `e` switch to the Signals pane, `c` copy the attach command, `/` filter.
- Empty: `No sessions. Press n to start one.`
- Gateway down: the list is replaced by one line, `OpenShell gateway unreachable (r to retry)`. Other tabs keep working.

### 5a. New session (`n`, or `s` on a repository)

```
 New session

   Repository   soft-serve
   Name         tui-2
   Profile      ‹ claude ›      ghcr.io/me/agent-claude:latest · claude
   Attach       yes

 ↑↓ field • ←→ change • enter create • esc cancel
```

- Repository is fixed when opened with `s`; otherwise it is a filterable picker.
- Name defaults to the profile name plus a number. Profile lists come from the config file; the default is the repo's configured profile.
- No free-text image field: images come from profiles. `session start --image` stays on the CLI.
- After `enter`: `Starting sandbox… Cloning… ` progress lines in place, then attach if chosen.

### 5b. End session (`x`)

```
 End infra / claude-1?  The sandbox and any unpushed work are deleted.   y / N
```

One line where the footer is. When S3 snapshots are on it reads `…a snapshot is saved first.`

### 5c. Attach (`enter`)

```
 Attaching to infra / claude-1 …  detach with ~.
```

The TUI steps aside, the terminal belongs to the session, and on detach the TUI returns to the same row. A parked session shows `Resuming…` first. Depends on R8.

## 6. Forges

One flat list grouped by forge. Forges are declared in the config file, so there is no "add forge" here.

```
 github · danielboothcloud               42 repos · 12 imported · synced 3m ago
   ✓ soft-serve                          ↑3 ↓0          synced 3m
   ✓ pi-aio                              ↑0 ↓2          synced 3m
     dotfiles                            not imported
     quartermaster                       not imported

 gitlab · gitlab.com                           ✗ sync failed: 401 token expired
   ✓ infra                               ↑0 ↓0          synced 2h
```

- `✓` imported. `↑n` commits on Soft Serve not on the forge, `↓n` the reverse.
- Keys: `i` import, `I` import all shown (respects the filter), `r` sync now, `p` push to the forge (asks `y / N`, names the branch), `enter` open the imported repo.

## 7. Signals pane (inside Sessions)

The second pane of the Sessions tab, toggled with `e`. Sessions shows where each session is now; Signals shows every OSC 7501 report as it arrived, newest first, across all my sessions.

```
 Repositories   • Sessions   Forges   About

 Sessions   ‹ Signals ›                                    following · 214 kept

 14:02:11  ▲ infra / claude-1         blocked · permission  Apply 3 to add, 1…
 14:02:03  ● infra / claude-1         working               Planning
 13:58:40  ✓ soft-serve / forge-sync  done                  Implemented GitHu…
 13:41:07  ● soft-serve / tui         working 40%           Running go test .…
 13:40:55  ● soft-serve / tui/lint    working               golangci-lint
 13:12:00  ○ dotfiles / main          idle · parked         after 30m idle
 12:50:19  + soft-serve / tui         created               profile claude

 ↑↓ navigate • enter attach • e sessions • a attention only • / filter • ? help
```

- One row per report: time, glyph, repo / session, state (kind, progress), message. A child record shows its id after the session name (`tui/lint`).
- `enter` launches that signal's session: attach, resuming first if it is parked. If the session has ended, it opens the new-session form with the repo and profile filled in.
- `a` narrows to blocked, error and done. `/` filters by repo, session or text.
- Following: new signals appear at the top while the cursor is on the first row. Moving down pauses following; `g` returns to the top and resumes.
- Lifecycle rows (`+` created, parked, resumed, ended) are in the same feed so the order of events is clear. `+` is a row marker here only, not a session state.
- Forge sync results are not here; they show on the Forges tab.
- Kept in memory, capped (for example the last 1000), so it is empty after a restart (question 4).

What limits "every signal": with status collected by polling (roadmap D5a) the feed only has the state at each poll, so quick changes between polls and child records are missed. A complete feed needs the in-pod watcher (D5b).

## 8. Inside a repository: Sessions tab

Added after Tags. The same rows as section 5 without the repo prefix, and the same keys, including `e` for that repo's signals.

```
 soft-serve
 Git hub + dev server

 Readme   Files   Commits   Bookmarks   Tags   • Sessions

 ✓ forge-sync                                                       done   12m
   Implemented GitHub discovery; 14 tests pass

 ● tui                                                       working 40%   now
   Running go test ./pkg/ui/...

 Sessions  forge-sync                                     1 of 2   ▲1 ✓1 ●2
```

The status bar's right end carries the attention summary while in a repo (R7).

## 9. Narrow terminals (phone, under 60 columns)

```
 Soft Serve       ▲1 ✓1 ●2

 ‹ Sessions ›

 ▲ infra / claude-1
   blocked · permission  4m
   Apply 3 to add, 1 to cha…

 ✓ soft-serve / forge-sync
   done  12m
   Implemented GitHub disco…
```

- Tabs collapse to the active one. Summary drops its words. Rows wrap state onto a second line.

## 10. Keys

| Key | Where | Action |
|-----|-------|--------|
| `1`–`4` | home | jump to tab |
| `enter` | Repositories / Sessions / Signals / Forges | open / attach / attach to that signal's session / open |
| `e` | Sessions tab | switch between the Sessions and Signals panes |
| `a` | Signals | attention only: blocked, error, done |
| `s` | Repositories | new session for the selected repo |
| `n` | Sessions | new session |
| `x` | Sessions | end session (confirm) |
| `z` | Sessions | park or resume |
| `i` / `I` | Forges | import one / all shown |
| `r` | Forges, or any tab showing an error | sync now / retry |
| `p` | Forges | push to forge (confirm) |
| `c` | any list | copy the row's command |

## 11. Open questions

Settled 2026-11-12 for the first build: two-line rows; open on Repositories; Forges as one flat grouped list; Signals in memory; keep 4-line repo rows; Signals as a pane inside Sessions.


1. Sessions rows: two lines as drawn, or a one-line table with the message cut off?
2. Opening the TUI: always Repositories as today, or Sessions when something is blocked, errored or done?
3. Forges: flat grouped list as drawn, or a list of forges that drills into each?
4. Signals: in memory only, or kept in the database so the feed survives restarts?
6. Signals as a pane inside Sessions as drawn, or its own top-level tab?
5. Repository rows: keep the 4-line row, or add a compact one-line mode once forges fill the list?

## 12. Implementation notes

Where the build differs from the screens above (sections 4, 6, 8, 9, 10).

- **Forges is for server admins only**, not every authenticated user (R10): listing, importing and syncing use the server's forge tokens, and `forge ls`, `forge import` and `forge sync` are admin-only, so the tab follows the same rule. Push (collaborator write in the CLI) is always allowed to an admin.
- **Forges starts when first shown**, not with the TUI. It shows the forge service's last listing and lists (discovers) only forges never listed since the server started; the scheduled sync keeps listings fresh. Imports, syncs and pushes run in the background; forge events from the bus (other admins, the schedule) refresh the affected forge live, coalesced while a read is under way.
- **Forge header**: `name · host`, then `N repos` (those the include/exclude patterns let through), `N imported`, and `synced 3m ago`; an error replaces the last part: `✗ sync failed: …` (last forge-wide sync) or `✗ cannot list: …` (last discovery). While busy: `syncing… 3 of 12`, `importing… 2 of 7`, `listing…`. Narrow: the right part goes to a second line.
- **Forge rows** show the forge path (`owner/repo`), not the bare name, since a forge can span owners. An operation's outcome (`✓ synced`, `✓ pushed`, `✗ push refused`, `✗ import failed`) replaces the sync age in the row; the full reason is on the tab's bottom line while the row is selected. An imported repository the forge stopped listing reads `(no longer listed)`.
- **Forge keys**: `r` on a repository that is not imported syncs its forge, as on the header. `c` copies an imported repository's clone command. `I` counts per forge in its confirm: `Import 3 repositories (github 2, gitlab 1)?`. The push confirm reads `Push main of <repo> to <forge>?  Fast-forward only, no force.`; `--force-with-lease` stays CLI-only.
- **Origin badge** shows only when forges are configured (otherwise every repository would read `local`), to anyone who can see the repository. Only non-zero arrows show: `github ↑3`, `github ↓2`, `github ↑1 ↓4`.
- **Session badge** is read from the Monitor (never the gateway) whenever the Repositories tab is drawn, so it follows status changes as they arrive. Every row drops the same badges so the columns line up: first `Updated …`, then the origin, then the session badge's word (`▲1`), then the badge. A title longer than half the row drops more on its own row only.
- **In-repo Sessions tab** shows the home Sessions tab's model narrowed to the repository; each scope (home, each repository) keeps its own cursor, pane and filters, and Signals follows again after a switch. Its end-session confirm shows inside the tab (the repository page's footer is the help). `esc` first closes a form or confirm, clears the filter or cancels a start, and only then leaves the repository. Status bar: `<repo>  <session>  1 of 2  *  ? Help  ▲1 ✓1 ●2`, the summary always in its narrow form.
- **Narrow repository page**: its tabs collapse to `‹ Sessions ›` whenever they do not fit, not only under 60 columns (at 50 columns the old tab strip already hid Sessions).
- `forge ls --json` gains `list_error` and `syncing`, which the tab reads.

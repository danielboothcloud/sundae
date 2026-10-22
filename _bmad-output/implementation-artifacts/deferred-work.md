
- source_spec: `_bmad-output/implementation-artifacts/spec-jj-first-class-repos.md`
  summary: Batch or cache per-commit `git cat-file` change-ID lookups in the TUI log.
  evidence: one subprocess per commit per page load in jj repos; fine for page sizes, slow on large pages/disks.
- source_spec: `_bmad-output/implementation-artifacts/spec-jj-first-class-repos.md`
  summary: Add curl testscript assertions for `X-Soft-Serve-VCS` (jj, git, unauthorized) and go-get for a git repo; honor HideCloneCmd in go-get.
  evidence: header and go-get git/jj distinction have no test; mutation of either passes the suite.
- source_spec: `_bmad-output/implementation-artifacts/spec-jj-first-class-repos.md`
  summary: Add unit tests for TUI jj bits (Refs.TabNameFor, Tabs.SetTabs, empty-repo message, log item width) and a DB CHECK/fallback for unknown vcs values.
  evidence: no TUI harness exists; unknown vcs only reachable by manual DB edit.

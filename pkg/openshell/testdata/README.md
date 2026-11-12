# osewa `local ls` fixtures

`osewa local ls` output from osewa 0.1.4, as the status poll reads it.

- `osewa-local-ls-empty.json`, `-working.json`, `-blocked.json`,
  `-exited.json`: printed by the released osewa 0.1.4 darwin-arm64 build
  (`osewa-darwin-arm64.gz`, SHA-256 `90e9e97e…77d8c0` per the release's
  SHA256SUMS) during `TestOsewaLive`: no sessions yet, then one session whose
  program reported working, blocked, then done and exited 7.
- `osewa-local-ls-mixed.json`: built from the osewa 0.1.4 source
  (`crates/wire/src/messages.rs`: `Response`, `SessionInfo`,
  `ProgramStatus`, `Build`, in serde field order and skip rules; the first
  session is the exact string `crates/wire/src/tests.rs` asserts) to cover
  fields the live run did not produce: `owner`, `attached` > 0, `tag`,
  `creator`, `incompatible`, a session without `status`, and a message
  with control characters.

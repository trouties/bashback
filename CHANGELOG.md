# Changelog

All notable changes to bashback are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[SemVer](https://semver.org/) (on-disk formats guarded by `schema_version`).

## [Unreleased]

## [1.1.0] - 2026-11-14

Tracks the current agent releases (Claude Code 2.1.325, Codex CLI 0.169,
Cursor CLI 2026.11.13); earlier versions are not supported.

### Changed

- Claude Code: the background final capture (`bgfinal_<key>`) now triggers on
  a `Read` of the finished task's output file or on `TaskStop`, replacing the
  removed `TaskOutput` tool. The hook matcher is now `TaskStop|Read`.
- Claude Code: the post hooks match `Bash|Monitor`; a Monitor watch is recorded
  as a background command and gets a final capture when it is stopped.
- Codex CLI: session end is wired to `SessionEnd` (timeout 3 s, Codex's
  ceiling) instead of `Stop`.
- `bashback install` also converges the matcher of a hook group it owns alone,
  so re-running it upgrades an existing wiring.
- Go 1.27 is now the minimum toolchain; CI actions and golangci-lint bumped.
- Plugin manifests: Claude Code manifest gains homepage, repository, license
  and keywords; the Cursor manifest uses the documented `logo` key.

### Fixed

- Cursor: failed shell commands are wired through `postToolUseFailure` and get
  complete, restorable entries instead of `pre_only` ones.
- Claude Code: commands started with the Monitor tool no longer leave a
  permanent `pre_only` entry (Bash-matched PreToolUse hooks fire for Monitor,
  Bash-matched PostToolUse hooks do not).
- `doctor` no longer reports the Claude Code plugin as wired when only its
  marketplace clone exists; it reads Claude Code's install record and honors
  project-scope installs.

### Documentation

- README, skill and Cursor rule updated for the platform changes above,
  including Codex's `--dangerously-bypass-hook-trust` for unattended runs and
  Cursor running Claude Code hooks via Third-Party Imports.

## [1.0.1] - 2026-06-12

### Fixed

- CLI bug fixes: newest-first JSON output, plugin-aware `doctor`, shipped `bin` shim.

## [1.0.0] - 2026-06-12

Initial public release.

### Added

- Command-level snapshot and undo for the shell commands a coding agent runs:
  a shadow git repository per session captures the working tree before and
  after every command, so file side effects (`rm`, `mv`, `sed -i`, …) can be
  audited, diffed, and restored.
- Fail-open hooks: any internal error exits 0 and never blocks the agent; a
  per-session daemon serializes all snapshot access so parallel commands never
  lose snapshots to `index.lock` races.
- Three supported platforms — **Claude Code** (first-class plugin with
  SessionStart bootstrap), **Codex CLI**, and **Cursor** — with
  platform-adaptive hook payloads and origin-tagged journal entries.
- CLI: `list` (incl. `--by-session`), `diff`, `show`, `log`, `export`,
  `stats`, `restore`, `undo`, `rewind`, `snap`, `gc`, `config`, `install`,
  `doctor`, `version`.
- Restore safety: every restore first snapshots the current tree (a restore is
  itself undoable); two explicit gates (`--force` for unreliable attribution,
  `--3way` to preserve later edits); interactive hunk-level restore (`-p`);
  path-scoped restore; deletion of files the undone command created.
- Multi-session undo guard: a bare `undo` refuses when several sessions wrote
  recently, listing them instead of guessing.
- Background-command final capture (`bgfinal_<key>`) on Claude Code.
- Agent teaching: a bundled skill (Claude Code/Codex) and rule (Cursor) that
  steer the agent to `bashback restore` instead of hand-reconstructing files,
  plus a one-line SessionStart orientation hint.
- Secrets stance: `.gitignore` honored by default (opt-in `force_include`),
  command text redacted before journaling, `0700` storage; see SECURITY.md.
- Self-check and repair: `doctor` verifies environment, per-platform hook
  wiring, and recent activity; `doctor --repair` recovers a damaged journal.
- Disk control: retention policy and GC (the journal is a permanent audit
  ledger), per-file size cap (default 100 MiB) with skip-and-flag.
- Distribution: `go install`, prebuilt release binaries with checksums,
  one-shot `install.sh` (version-pinnable) wiring a chosen platform, and
  plugin panels (`.claude-plugin/`, `.codex-plugin/`, `.cursor-plugin/`).

[Unreleased]: https://github.com/trouties/bashback/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/trouties/bashback/releases/tag/v1.1.0
[1.0.1]: https://github.com/trouties/bashback/releases/tag/v1.0.1
[1.0.0]: https://github.com/trouties/bashback/releases/tag/v1.0.0

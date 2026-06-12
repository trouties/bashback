# Changelog

All notable changes to bashback are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[SemVer](https://semver.org/) (on-disk formats guarded by `schema_version`).

## [Unreleased]

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

[Unreleased]: https://github.com/trouties/bashback/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/trouties/bashback/releases/tag/v1.0.0

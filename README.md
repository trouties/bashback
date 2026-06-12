# bashback

[![ci](https://github.com/trouties/bashback/actions/workflows/ci.yml/badge.svg)](https://github.com/trouties/bashback/actions/workflows/ci.yml)

When a bash command destroys a file, a coding agent does the only thing it
can: rebuild the file from conversation memory — thousands of tokens burned,
and the result is often wrong. The exact bytes existed seconds earlier; they
just weren't saved anywhere. `/rewind` doesn't bring them back either, because
agent checkpoints track the agent's *edits*, not what bash did.

bashback is **`/rewind` for bash**: it snapshots your working tree around every
shell command your coding agent runs, giving you a command-level, auditable,
undoable history of file side effects — `rm`, `mv`, `sed -i`, the codegen
script that ran in the wrong directory, all of it. Instead of a from-memory
rebuild, the pre-command bytes are one `bashback undo` away.

![a careless sed and rm, reviewed with bashback diff and reverted with bashback undo](https://raw.githubusercontent.com/trouties/bashback/assets/demo.gif)

One static binary. The only runtime dependency is `git >= 2.32`.

## Quickstart

Get protected on your agent: **[Claude Code](#claude-code)** ·
**[Codex CLI](#codex-cli)** · **[Cursor](#cursor)**.

The shortest path, if you have Go:

```sh
go install github.com/trouties/bashback/cmd/bashback@latest
bashback install        # wire hooks + agent skill (Claude Code; --codex / --cursor for others)
bashback doctor         # verify wiring and environment
```

From then on, every shell command the agent runs is snapshotted. When one goes
wrong:

```sh
bashback list           # what changed, command by command
bashback diff <key>     # one command's file side effects
bashback undo           # revert the latest file-changing command
```

## Why bashback

**One command instead of a rebuild.** The token savings take more than an
undo command — the agent has to know to reach for it. bashback ships an
[agent skill](#teaching-the-agent) that teaches the agent to try
`bashback undo` before improvising, so a clobbered file costs one command
instead of a long, unreliable from-memory rewrite.

**Never in the way.** Hooks are **fail-open**: any internal error exits 0 and
the agent keeps working — bashback can fail to protect, but it can never
block. A snapshot adds roughly 180 ms around a command and is skipped entirely
when nothing changed. Nothing to enable per command, no change to how your
agent works.

**Undo you can trust.** Every restore first snapshots your current tree, so a
restore is itself undoable — a wrong undo never loses work. And when an undo
*would* be surprising (unreliable attribution, your later edits in the way),
bashback [refuses with an explanation](#two-gates-on-restore) instead of
guessing; an explicit flag clears each gate.

**Honest edges.** bashback covers file side effects inside your project's
working directory, and [says plainly what it does not
cover](#what-bashback-does-not-cover) rather than implying a recovery it
cannot make.

## How it works

It starts the moment your agent runs a command. A *pre* hook snapshots the
working tree before the command touches anything; a *post* hook snapshots it
again afterward and records the diff in a journal, keyed by the command's
`tool_use_id`. Failed and interrupted commands are captured too — half-completed
side effects are exactly the ones worth a record.

Under the hood, bashback is one binary playing three roles:

- **hook** — invoked by your agent's pre/post tool hooks around every shell
  command. Fail-open by design: any error exits 0 and never blocks the agent.
- **daemon** — a per-session single writer. All snapshot access is serialized
  through one queue, so parallel shell calls never lose snapshots to
  `index.lock` races.
- **CLI** — `list`, `diff`, `restore`, `undo`, `gc`, `doctor` for inspecting
  and rolling back; both you and the agent use it.

Snapshots live in a **shadow git repository** per session under `~/.bashback`
(independent `GIT_DIR`, your project as the work-tree). bashback reuses git's
storage, diff, and three-way merge; it does not parse your commands, does not
watch the filesystem, and never touches your project's own `.git`.

Undo restores content from the pre-command snapshot, **including deleting
files the command created** (a plain `git checkout` won't). And because every
restore is preceded by its own snapshot, your later edits are never silently
lost.

## Install

Runs on **Linux** and **macOS** (amd64/arm64). Windows works
**via WSL2 only** — bashback runs where your agent's shell hooks run, and that
is a unix environment.

With Go:

```sh
go install github.com/trouties/bashback/cmd/bashback@latest
```

Or grab a prebuilt binary and `checksums.txt` from the
[releases page](https://github.com/trouties/bashback/releases) and verify it:

```sh
sha256sum --ignore-missing -c checksums.txt   # macOS: shasum -a 256 --ignore-missing -c checksums.txt
tar -xzf bashback_*_*.tar.gz
```

Or build from source: `go build -o bashback ./cmd/bashback`.

Or use the one-shot installer, which fetches the release binary **and** wires
a platform in a single step:

```sh
curl -fsSLO https://raw.githubusercontent.com/trouties/bashback/main/install.sh
sh install.sh claude    # or: codex | cursor
# pin a specific release: BASHBACK_VERSION=v1.0.0 sh install.sh claude
```

bashback runs on three agents: **Claude Code**, **Codex CLI**, and **Cursor**.
The engine, the CLI (`list` / `diff` / `restore` / `undo`), and the cwd
boundaries are identical everywhere; only the wiring and a few honest edges
differ, noted per platform.

### Claude Code

Install as a plugin — no hand-edited settings.json:

```text
/plugin marketplace add trouties/bashback
/plugin install bashback
```

The plugin ships its hooks and an agent skill; its SessionStart hook
bootstraps the binary for you. The manual path still works:

```sh
bashback install            # wire the project's .claude/settings.json + skill
bashback install --user     # wire ~/.claude/settings.json instead
bashback install --print    # preview the merged settings without writing
```

`install` is idempotent and surgical: it backs up the settings file first,
updates stale paths in place, leaves unrelated settings untouched, and refuses
to touch a settings.json it can't parse. It also writes the agent skill next
to the settings file (`--no-skill` skips it). To wire by hand instead, copy
[`examples/settings.hooks.json`](examples/settings.hooks.json) — `PostToolUse`
and `PostToolUseFailure` both point at `hook post`, so a failed or interrupted
command's half-completed side effects are still captured.

Boundary: Claude Code's own checkpoints already cover its file edits; bashback
fills the bash-command gap. Background commands get a second `bgfinal_`
capture when the agent reads their output.

### Codex CLI

```sh
curl -fsSLO https://raw.githubusercontent.com/trouties/bashback/main/install.sh
sh install.sh codex          # download the binary and wire Codex hooks
bashback install --codex     # if the binary is already on PATH
```

This wires Codex's hooks and installs the skill to `~/.agents/skills/`. Codex
does **not** trust newly added hooks automatically, and it skips untrusted
hooks **silently** — no prompt. You must run `/hooks` in Codex once to trust
them, or bashback never fires. (`--dangerously-bypass-hook-trust` does not
help: it still skips untrusted hooks.) There is no headless trust path, so
automated/CI Codex runs cannot enable bashback without a one-time interactive
trust.

Boundary: the first release covers the **`Bash` shell tool only** —
`apply_patch` code edits are not snapshotted, since Codex checkpoints its own
edits and bashback fills the shell-command gap. Failed commands *are* covered:
Codex fires `PostToolUse` even on a non-zero exit, so a failed command still
produces a complete, restorable entry. There are no background `bgfinal_`
entries — Codex emits no background events, and its `Stop` event marks session
end.

### Cursor

```sh
bashback install --cursor    # wire .cursor/hooks.json + write .cursor/rules/bashback.mdc
```

Or install bashback from Cursor's plugin panel (manifest in
`.cursor-plugin/`). Cursor uses native
`preToolUse` / `postToolUse` hooks (matcher `Shell`) with a native
`tool_use_id`, so pre/post snapshots pair natively and entries are normally
`status=protected`. The `.cursor/rules/bashback.mdc` rule teaches the agent to
reach for `bashback restore`.

Boundary: a **failed** shell command does not fire `postToolUse`, so it leaves
a `pre_only` entry that `restore` will only apply under `--force` — review the
risk first (no post snapshot exists, so the failed command's own effects were
never captured) rather than forcing blindly. There are no background
`bgfinal_` entries. The `cursor-agent` CLI path is verified; the in-IDE Agent
path is expected to work but is not yet machine-verified.

### Verify and uninstall

`bashback doctor` checks git/permissions/config, hook wiring, the binary path,
and recent activity.

Uninstall: plugin — `/plugin uninstall bashback` (removes the data dir
`~/.claude/plugins/data/bashback-bashback` on the last scope); manual — drop
bashback's hook entries and the skill/rule by hand. Either way the journal and
snapshots are in `~/.bashback`: `rm -rf ~/.bashback` to reclaim it (this also
deletes the audit history).

## Usage

```sh
bashback list                      # snapshots for the current project
bashback list --by-session         # group snapshots by session
bashback undo                      # revert the latest file-changing command
bashback undo --session <id>       # scope undo to one session (id prefix)
bashback diff <key>                # what a command changed (pre..post)
bashback diff <key1> <key2>        # changes between two entries (same session)
bashback export <key> [--out f]    # git-apply patch for an entry (binary-safe)
bashback log <path> [-n N] [--since 2h] [--full] [--abs]   # history of a path
bashback restore <key> --dry-run   # preview the restore plan
bashback restore <key>             # undo a command's file side effects
bashback restore <key> --3way      # undo while preserving your later edits
bashback restore <key> path/...    # undo only specific paths
bashback restore <key> -p          # interactively pick hunks to revert
bashback rewind <key> [--dry-run]  # whole tree back to before <key> (undoes everything after it too)
bashback snap [-m note]            # manual anchor snapshot before risky work
bashback show <key> [--json]       # full detail for one entry
bashback stats                     # usage statistics
bashback config set <name> <value> # per-project settings (protect_paths, force_include, ...)
bashback gc [--older-than 336h] [--dry-run] [--all]
bashback install [--user] [--print] [--no-skill] [--codex|--cursor]
bashback doctor
bashback version
```

`<key>` is the command's `tool_use_id`. **Keys you can read are keys you can
use**: `list` and `log` print each key as a short unique prefix — no
ellipsis — so it pastes straight into `diff`/`restore`. Any unique prefix of
≥4 characters resolves; `--full` shows whole keys, and `--json` always carries
them for scripts.

### Two gates on restore

Restore refuses, rather than silently doing something surprising, in two
cases — each cleared by an explicit flag:

- **`--force`** accepts an unreliable attribution: an `overlapped` entry's
  snapshot may contain a concurrent command's partial work, so `--force` may
  undo that command's changes too. Review with `diff` first and narrow the
  blast radius with a path filter.
- **`--3way`** merges with the edits you made *after* the snapshot instead of
  discarding them; conflicts surface as standard merge markers, never a silent
  overwrite. Combine with `--force` when the target needs both.

When a command got *part* of a file right, `restore <key> -p` walks the change
hunk by hunk, reverting only what you pick — the partial restore is itself a
new undoable entry. It needs a real terminal and is mutually exclusive with
`--3way`; agents and scripts should use path filters or `--3way` instead.

### Multiple sessions

One agent conversation produces several sessions (the main session plus one
per subagent), and you may have several terminals open. The CLI cannot know
which session you mean, so `undo` does not guess: when two or more sessions
have written an entry within the last hour, a bare `bashback undo` refuses and
lists the active sessions instead of risking another session's work. Clear it
with `undo --session <id>` or by addressing an entry directly with
`restore <key>`. `list --by-session` shows what each session changed.

## What bashback does not cover

bashback covers **file side effects inside your project's cwd** — content-level
git restore, not a byte-/metadata-level time machine. It does not cover:

- paths outside cwd, network/DB effects, `git push`, process or environment
  state;
- **nested git repos / submodules** — git records them as a gitlink, so their
  contents are never snapshotted; restoring a deleted nested repo yields an
  empty directory. bashback flags these in the journal;
- empty directories, mtime/owner, permission bits beyond the exec bit, special
  files (FIFO/device/socket);
- files ignored by `.gitignore` — including `.env`. **A deleted `.gitignore`d
  `.env` cannot be recovered** unless opted in via `force_include` (see
  [SECURITY.md](SECURITY.md)), which stores a plaintext copy under
  `~/.bashback`;
- files over the size cap (default 100 MiB) — skipped and flagged.

Cross-session concurrency (two agent sessions on the same project at once) is
a known blind spot. Within a session, commands whose pre/post intervals overlap
are flagged `overlapped` and need `--force` to restore.

**Background commands** (`run_in_background`) are snapshotted when they go to
the background *and again at completion* — a second `bgfinal_<key>` entry is
captured when the agent reads the task's output or kills it. `log` attributes
these to the original command (`bg of <key>`). Two honest edges: if the agent
never reads the output there is no capture moment, and a task finishing after
the session ends is not covered.

See [SECURITY.md](SECURITY.md) for the secrets/redaction stance.

## Teaching the agent

bashback ships a **skill** that teaches the agent to reach for
`bashback restore` instead of hand-reconstructing clobbered files —
this is where the token savings come from. `bashback install` writes it
automatically next to the settings file it wires; to place it by hand instead,
copy [`skills/bashback`](skills/bashback) into `~/.claude/skills/` (user-level)
or `.claude/skills/` (project-level).

Beyond the skill, bashback injects a one-line orientation hint at SessionStart —
carrying the session id prefix so the agent can scope `undo --session` to
itself — and after large changes. These are its only two channels for teaching
the agent — it never writes to, or suggests edits to, your `CLAUDE.md`.

## Configuration

Defaults are overridable by environment variables; a malformed value falls back
silently, never breaking a hook:

| Variable | Default | Controls |
| --- | --- | --- |
| `BASHBACK_STALE_TTL` | `15m` | how long an open pre waits for its post before being treated as interrupted |
| `BASHBACK_IDLE_TIMEOUT` | `30m` | how long an idle session daemon stays up |
| `BASHBACK_MAX_FILE_BYTES` | `100MiB` | per-file size cap; larger files are skipped and flagged |

Per-project settings (`bashback config set …`) override environment, which
overrides defaults; `doctor` prints each effective value and its source. When
`protect_paths` is set, snapshots — and therefore restores — cover **only** the
listed paths.

When a hook swallows an error (fail-open), it records one best-effort JSONL
line in the project's `hook.log` under `~/.bashback` (rotated at 1 MiB), so a
silent miss becomes something `doctor`'s activity section can surface.

## Development

```sh
go test -race ./...        # unit + integration (real git) + concurrency
go vet ./...
```

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).

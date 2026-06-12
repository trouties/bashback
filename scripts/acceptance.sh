#!/usr/bin/env bash
# bashback acceptance smoke (acceptance criteria). Builds the binary and exercises, with the
# real binary and faked Claude Code stdin:
#   #1 rm -rf restore round-trip (incl. removing command-created files; undoable)
#   #3 fault injection -> hook exit code always 0
#   #5 journal redaction of an Authorization: Bearer token + undo toggle (v0<->v1)
#   #6 rewind A->B->C to A's pre, then undo the rewind in one step
#   #8 manual snap -m + dangerous op + rewind round-trip; restore refuses manual key
#   pre-only: interrupted command (pre, no post) -> list label + --force lazy undo
#   M4 export: binary-safe patch round-trips through `git apply`
# Concurrency, perf, GC, protect_paths, and CI are covered by `go test`.
#
# Usage: ./scripts/acceptance.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$(mktemp -d)/bashback"
echo "building..."
( cd "$ROOT" && go build -o "$BIN" ./cmd/bashback )

HOME_DIR="$(mktemp -d)"
WORK="$(mktemp -d)"
export BASHBACK_HOME="$HOME_DIR" BASHBACK_NO_SPAWN=1
fail() { echo "FAIL: $*" >&2; exit 1; }

git -C "$WORK" init -q
git -C "$WORK" -c user.name=t -c user.email=t@t commit -q --allow-empty -m init

payload() { printf '{"session_id":"acc","cwd":"%s","tool_use_id":"%s","tool_input":{"command":"%s"}}' "$WORK" "$1" "$2"; }

# --- #1: rm -rf restore round-trip ---
mkdir -p "$WORK/data"; echo "important" > "$WORK/data/file.txt"; echo "v1" > "$WORK/keep.txt"
echo "$(payload u1 'rm -rf data')" | "$BIN" hook pre
rm -rf "$WORK/data"; echo "v2" > "$WORK/keep.txt"; echo "new" > "$WORK/created.txt"
echo "$(payload u1 'rm -rf data')" | "$BIN" hook post

( cd "$WORK" && "$BIN" restore u1 >/dev/null )
[ "$(cat "$WORK/keep.txt")" = "v1" ]            || fail "#1 keep.txt not reverted"
[ "$(cat "$WORK/data/file.txt")" = "important" ] || fail "#1 deleted dir not restored"
[ ! -e "$WORK/created.txt" ]                     || fail "#1 created file not removed"
echo "PASS #1 restore round-trip"

# undo the restore (restore is itself a snapshot)
UNDO_KEY="$(cd "$WORK" && "$BIN" list | awk '/restored/ {print $1; exit}')"
[ -n "$UNDO_KEY" ] || fail "#1 restored entry not listed"
( cd "$WORK" && "$BIN" restore "$UNDO_KEY" >/dev/null )
[ -e "$WORK/created.txt" ] || fail "#1 undo-of-restore did not bring command state back"
echo "PASS #1 restore is undoable"

# --- #3: fault injection -> exit 0 ---
code=0; echo "not json" | "$BIN" hook pre || code=$?
[ "$code" -eq 0 ] || fail "#3 garbage payload exit $code"
code=0; echo "$(payload u9 'ls')" | env BASHBACK_HOME=/proc/nonexistent/x "$BIN" hook pre || code=$?
[ "$code" -eq 0 ] || fail "#3 broken home exit $code"
echo "PASS #3 fail-open exit 0"

# --- #5: redaction ---
echo "$(payload u2 'curl -H \"Authorization: Bearer sk-leakme123\" https://x')" | "$BIN" hook pre
echo "redacted" > "$WORK/r.txt"
echo "$(payload u2 'curl -H \"Authorization: Bearer sk-leakme123\" https://x')" | "$BIN" hook post
JOURNAL="$HOME_DIR"/repos/*/journal.jsonl
if grep -q "sk-leakme123" $JOURNAL; then fail "#5 secret leaked into journal"; fi
grep -q '\*\*\*' $JOURNAL || fail "#5 no redaction marker found"
echo "PASS #5 redaction"

# --- pre-only: interrupted command (Esc) leaves a pre with no post ---
echo "orig" > "$WORK/longrun.txt"
echo "$(payload u_int 'while true; do echo tick >> longrun.txt; done')" | "$BIN" hook pre
echo "half-finished" >> "$WORK/longrun.txt"   # the partial side effect on disk
# no `hook post` — simulates an Esc interrupt
( cd "$WORK" && "$BIN" list | grep -q 'pre-only' ) || fail "pre-only not labelled in list"
code=0; ( cd "$WORK" && "$BIN" restore u_int >/dev/null 2>&1 ) || code=$?
[ "$code" -ne 0 ] || fail "pre-only restore should refuse without --force"
( cd "$WORK" && "$BIN" restore --force u_int >/dev/null )
[ "$(cat "$WORK/longrun.txt")" = "orig" ] || fail "pre-only --force did not revert half-finished side effect"
echo "PASS pre-only lazy restore"

# --- #6: rewind A->B->C, rewind <A> returns to A's pre; undo the rewind (toggle) ---
# Commands are spaced ~1s apart so the journal's RFC3339 (second-granularity)
# timestamps stay distinct and the rewind baseline resolves to the newest post.
REW="$(mktemp -d)"
git -C "$REW" init -q
git -C "$REW" -c user.name=t -c user.email=t@t commit -q --allow-empty -m init
rp() { printf '{"session_id":"rew","cwd":"%s","tool_use_id":"%s","tool_input":{"command":"%s"}}' "$REW" "$1" "$2"; }
echo "base" > "$REW/a.txt"
echo "$(rp A 'edit a; add b')" | "$BIN" hook pre; echo "A" > "$REW/a.txt"; echo "b" > "$REW/b.txt"; echo "$(rp A 'edit a; add b')" | "$BIN" hook post
sleep 1.2
echo "$(rp B 'add c')" | "$BIN" hook pre; echo "c" > "$REW/c.txt"; echo "$(rp B 'add c')" | "$BIN" hook post
sleep 1.2
echo "$(rp C 'modify a')" | "$BIN" hook pre; echo "AAA" > "$REW/a.txt"; echo "$(rp C 'modify a')" | "$BIN" hook post
sleep 1.2 # keep the rewind entry's timestamp strictly newer than C's for undo

( cd "$REW" && "$BIN" rewind A >/dev/null )
[ "$(cat "$REW/a.txt")" = "base" ] || fail "#6 rewind did not return a.txt to A's pre"
[ ! -e "$REW/b.txt" ] || fail "#6 rewind did not remove command-created b.txt"
[ ! -e "$REW/c.txt" ] || fail "#6 rewind did not remove command-created c.txt"
echo "PASS #6 rewind to pre-command tree"

( cd "$REW" && "$BIN" undo >/dev/null )
[ "$(cat "$REW/a.txt")" = "AAA" ] || fail "#6 undo of rewind did not restore C-post a.txt"
[ -e "$REW/b.txt" ] && [ -e "$REW/c.txt" ] || fail "#6 undo of rewind did not bring files back"
echo "PASS #6 rewind is undoable in one step"

# --- #5: two consecutive undos toggle the work-tree (v0 <-> v1) ---
UND="$(mktemp -d)"
git -C "$UND" init -q
git -C "$UND" -c user.name=t -c user.email=t@t commit -q --allow-empty -m init
up() { printf '{"session_id":"und","cwd":"%s","tool_use_id":"%s","tool_input":{"command":"%s"}}' "$UND" "$1" "$2"; }
echo "v0" > "$UND/x.txt"
echo "$(up t1 'write v1')" | "$BIN" hook pre; echo "v1" > "$UND/x.txt"; echo "$(up t1 'write v1')" | "$BIN" hook post
sleep 1.2 # each undo's restore entry must outrank its target in the journal
( cd "$UND" && "$BIN" undo >/dev/null )
[ "$(cat "$UND/x.txt")" = "v0" ] || fail "#5 first undo did not revert to v0"
sleep 1.2
( cd "$UND" && "$BIN" undo >/dev/null )
[ "$(cat "$UND/x.txt")" = "v1" ] || fail "#5 second undo did not toggle back to v1"
echo "PASS #5 undo toggle"

# --- #8: manual snap -m, then dangerous op, then rewind <snap> round-trip ---
SNP="$(mktemp -d)"
git -C "$SNP" init -q
git -C "$SNP" -c user.name=t -c user.email=t@t commit -q --allow-empty -m init
sp() { printf '{"session_id":"snp","cwd":"%s","tool_use_id":"%s","tool_input":{"command":"%s"}}' "$SNP" "$1" "$2"; }
echo "safe" > "$SNP/doc.txt"
( cd "$SNP" && "$BIN" snap -m "before danger" >/dev/null )
sleep 1.2
echo "$(sp d1 'clobber doc')" | "$BIN" hook pre; rm "$SNP/doc.txt"; echo "garbage" > "$SNP/doc.txt"; echo "$(sp d1 'clobber doc')" | "$BIN" hook post
# Use the stable snap_* key (column 2), not the relative @N, since rewind appends
# an entry and shifts every @N afterward.
SNAP_KEY="$(cd "$SNP" && "$BIN" list | awk '/manual/ {print $2; exit}')"
[ -n "$SNAP_KEY" ] || fail "#8 manual snap not listed"
case "$SNAP_KEY" in snap_*) ;; *) fail "#8 unexpected snap key: $SNAP_KEY" ;; esac
# restore (not rewind) on a manual key must refuse and point at rewind
code=0; ( cd "$SNP" && "$BIN" restore "$SNAP_KEY" >/dev/null 2>&1 ) || code=$?
[ "$code" -ne 0 ] || fail "#8 restore of a manual snap should refuse"
( cd "$SNP" && "$BIN" rewind "$SNAP_KEY" >/dev/null )
[ "$(cat "$SNP/doc.txt")" = "safe" ] || fail "#8 rewind to snap did not restore doc.txt"
echo "PASS #8 snap + rewind round-trip"

# --- M4 export: binary-safe patch round-trips through `git apply` ---
EXP="$(mktemp -d)"
git -C "$EXP" init -q
git -C "$EXP" -c user.name=t -c user.email=t@t commit -q --allow-empty -m init
ep() { printf '{"session_id":"exp","cwd":"%s","tool_use_id":"%s","tool_input":{"command":"%s"}}' "$EXP" "$1" "$2"; }
printf '\x00\x01\x02bin-v0\x00\x07' > "$EXP/data.bin"
echo "$(ep e1 'mutate binary')" | "$BIN" hook pre
printf '\xff\x00bin-v1\x00\x09\x08' > "$EXP/data.bin"
echo "$(ep e1 'mutate binary')" | "$BIN" hook post
PATCH="$(mktemp)"
( cd "$EXP" && "$BIN" export e1 --out "$PATCH" )
grep -q "GIT binary patch" "$PATCH" || fail "export patch is not binary-safe"
printf '\x00\x01\x02bin-v0\x00\x07' > "$EXP/data.bin"   # reset to pre, then replay
( cd "$EXP" && git apply "$PATCH" )
cmp -s <(printf '\xff\x00bin-v1\x00\x09\x08') "$EXP/data.bin" || fail "export patch did not round-trip the binary file"
echo "PASS M4 export binary patch round-trip"

# --- M4 bgfinal: a backgrounded command's final state is captured at completion ---
BG="$(mktemp -d)"
git -C "$BG" init -q
git -C "$BG" -c user.name=t -c user.email=t@t commit -q --allow-empty -m init
bgp() { printf '{"session_id":"bg","cwd":"%s","tool_use_id":"%s","tool_input":{"command":"%s","run_in_background":true},"tool_response":{"backgroundTaskId":"%s"}}' "$BG" "$1" "$2" "$3"; }
bgevent() { printf '{"session_id":"bg","cwd":"%s","tool_name":"TaskOutput","tool_input":{"task_id":"%s"},"tool_response":{"task":{"status":"completed","exitCode":0}}}' "$BG" "$1"; }
echo "before" > "$BG/log.txt"
echo "$(bgp bg1 'long-task &' task-smoke)" | "$BIN" hook pre
echo "$(bgp bg1 'long-task &' task-smoke)" | "$BIN" hook post   # post taken at backgrounding
# the background command's writes land after backgrounding:
echo "finished output" > "$BG/log.txt"; echo "result" > "$BG/result.txt"
echo "$(bgevent task-smoke)" | "$BIN" hook bg                   # completion read -> bgfinal capture
( cd "$BG" && "$BIN" list | grep -q 'bgfinal_bg1' ) || fail "bgfinal entry not listed"
( cd "$BG" && "$BIN" restore bgfinal_bg1 >/dev/null )
[ "$(cat "$BG/log.txt")" = "before" ] || fail "bgfinal restore did not revert the background write"
[ ! -e "$BG/result.txt" ] || fail "bgfinal restore did not remove the background-created file"
echo "PASS M4 bgfinal capture + restore"

# --- M5 install + doctor wiring (isolated HOME so the wiring check is hermetic) ---
M5HOME="$(mktemp -d)"
M5WORK="$(mktemp -d)"
git -C "$M5WORK" init -q
( cd "$M5WORK" && HOME="$M5HOME" "$BIN" install >/dev/null ) || fail "install exited non-zero"
OKROWS=$( cd "$M5WORK" && HOME="$M5HOME" "$BIN" doctor | grep 'wiring ' | grep -c ': ok' )
[ "$OKROWS" -eq 6 ] || fail "doctor should report 6 wiring ok rows, got $OKROWS"
( cd "$M5WORK" && HOME="$M5HOME" "$BIN" doctor >/dev/null ) || fail "doctor should exit 0 when fully wired"
SKILLFILE="$M5WORK/.claude/skills/bashback/SKILL.md"
[ -f "$SKILLFILE" ] || fail "install should write the agent skill"
head -2 "$SKILLFILE" | grep -q '^name: bashback' || fail "skill file lacks frontmatter"
( cd "$M5WORK" && HOME="$M5HOME" "$BIN" doctor | grep -q 'skill: ok' ) || fail "doctor should report skill ok"
# Rewrite settings with one hook removed (SessionEnd) -> doctor must fail.
cat > "$M5WORK/.claude/settings.json" <<EOF
{ "hooks": {
  "PreToolUse":  [{ "matcher": "Bash", "hooks": [{ "type":"command","command":"$BIN hook pre","timeout":5 }] }],
  "PostToolUse": [
    { "matcher": "Bash", "hooks": [{ "type":"command","command":"$BIN hook post","timeout":5 }] },
    { "matcher": "TaskOutput|TaskStop|BashOutput|KillShell", "hooks": [{ "type":"command","command":"$BIN hook bg","timeout":5 }] }
  ],
  "PostToolUseFailure": [{ "matcher": "Bash", "hooks": [{ "type":"command","command":"$BIN hook post","timeout":5 }] }],
  "SessionStart": [{ "hooks": [{ "type":"command","command":"$BIN hook session-start","timeout":5 }] }]
} }
EOF
HINT=$( cd "$M5WORK" && HOME="$M5HOME" "$BIN" doctor 2>&1 || true )
printf '%s\n' "$HINT" | grep -q 'wiring SessionEnd: missing' || fail "doctor should flag the missing SessionEnd hook"
printf '%s\n' "$HINT" | grep -q 'bashback install' || fail "doctor should hint at 'bashback install'"
if ( cd "$M5WORK" && HOME="$M5HOME" "$BIN" doctor >/dev/null ); then fail "doctor should exit 1 with a missing hook"; fi
echo "PASS M5 install + doctor wiring"

# --- M7 multi-session undo gate: two recently-active sessions make bare undo
# refuse; --session scopes it and proceeds ---
M7="$(mktemp -d)"
git -C "$M7" init -q
git -C "$M7" -c user.name=t -c user.email=t@t commit -q --allow-empty -m init
m7p() { printf '{"session_id":"%s","cwd":"%s","tool_use_id":"%s","tool_input":{"command":"%s"}}' "$1" "$M7" "$2" "$3"; }
echo "a0" > "$M7/a.txt"; echo "b0" > "$M7/b.txt"
echo "$(m7p m7sessA ta 'edit a')" | "$BIN" hook pre; echo "a1" > "$M7/a.txt"; echo "$(m7p m7sessA ta 'edit a')" | "$BIN" hook post
echo "$(m7p m7sessB tb 'edit b')" | "$BIN" hook pre; echo "b1" > "$M7/b.txt"; echo "$(m7p m7sessB tb 'edit b')" | "$BIN" hook post
code=0; ( cd "$M7" && "$BIN" undo >/dev/null 2>&1 ) || code=$?
[ "$code" -ne 0 ] || fail "M7 bare undo should gate with two active sessions"
[ "$(cat "$M7/a.txt")" = "a1" ] || fail "M7 gate must not change the work-tree"
# --session scopes to one session's newest undoable entry (sessB is on top).
( cd "$M7" && "$BIN" undo --session m7sessB >/dev/null ) || fail "M7 --session undo should proceed"
[ "$(cat "$M7/b.txt")" = "b0" ] || fail "M7 --session undo did not revert sessB"
[ "$(cat "$M7/a.txt")" = "a1" ] || fail "M7 --session undo must not touch sessA"
echo "PASS M7 multi-session undo gate"

# --- M8 short keys: every KEY shown by `list` resolves directly via `show` ---
M8="$(mktemp -d)"
git -C "$M8" init -q
git -C "$M8" -c user.name=t -c user.email=t@t commit -q --allow-empty -m init
m8p() { printf '{"session_id":"m8","cwd":"%s","tool_use_id":"%s","tool_input":{"command":"%s"}}' "$M8" "$1" "$2"; }
echo "v0" > "$M8/f.txt"
echo "$(m8p toolu_alpha_smoke_one 'edit one')" | "$BIN" hook pre; echo "v1" > "$M8/f.txt"; echo "$(m8p toolu_alpha_smoke_one 'edit one')" | "$BIN" hook post
echo "$(m8p toolu_bravo_smoke_two 'edit two')" | "$BIN" hook pre; echo "v2" > "$M8/f.txt"; echo "$(m8p toolu_bravo_smoke_two 'edit two')" | "$BIN" hook post
KEYS="$(cd "$M8" && "$BIN" list | awk 'NR>1 && $1 ~ /^@/ {print $2}')"
[ -n "$KEYS" ] || fail "M8 list produced no keys"
for k in $KEYS; do
  case "$k" in *…*) fail "M8 short key has an ellipsis: $k" ;; esac
  ( cd "$M8" && "$BIN" show "$k" >/dev/null ) || fail "M8 short key not resolvable by show: $k"
done
echo "PASS M8 short keys round-trip"

# --- M8 interactive restore (-p) refuses a non-TTY: the smoke pipe is never a
# terminal, so the scripted path covers only the refusal; the y/n/q interaction
# itself is covered by the Go unit tests (injectable input). ---
OUT=$( cd "$M8" && printf 'y\nq\n' | "$BIN" restore toolu_bravo_smoke_two -p 2>&1 || true )
printf '%s\n' "$OUT" | grep -q 'requires an interactive terminal' || fail "M8 -p should refuse a non-TTY with guidance"
echo "PASS M8 partial restore refuses non-TTY"

echo "ALL ACCEPTANCE SMOKE CHECKS PASSED"

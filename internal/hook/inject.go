package hook

import (
	"encoding/json"
	"fmt"
	"io"
)

// MajorFilesThreshold is the changed-file count at or above which the `major`
// tier treats a command as a large change worth surfacing to the agent. A
// command with any deletion also qualifies regardless of count.
const MajorFilesThreshold = 10

// sessionTag renders the session id parenthetical for SessionStart hints,
// truncated to 12 chars (the short-key display convention) so the line stays
// terminal-friendly; the prefix is valid wherever --session takes an id.
func sessionTag(id string) string {
	if id == "" {
		return ""
	}
	if len(id) > 12 {
		id = id[:12]
	}
	return " (session " + id + ")"
}

// sessionHintText is the one-time SessionStart orientation under `major` and
// `all`: bashback is otherwise invisible to the model, so the agent must be told
// it can self-serve an undo.
func sessionHintText(sessionID string) string {
	return fmt.Sprintf("bashback is protecting this workspace%s: review a command's file changes with `bashback diff <key>` (keys via `bashback list`), undo the latest with `bashback undo`.", sessionTag(sessionID))
}

type hookOutput struct {
	HookSpecificOutput hookSpecific `json:"hookSpecificOutput"`
}

type hookSpecific struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// cursorContext is cursor's sessionStart injection envelope; cursor has no
// hookSpecificOutput shape, it takes additional_context directly.
type cursorContext struct {
	AdditionalContext string `json:"additional_context"`
}

// contextMessage decides what (if anything) to inject for one hook op under the
// given tier: returns the Claude event name and additionalContext text, or
// ok=false to inject nothing. An unknown tier falls through to off. source and
// sessionEntries are consulted only for session-start (post/bg pass "", 0).
func contextMessage(op, tier string, files, deletes int, key, source, sessionID string, sessionEntries int) (event, text string, ok bool) {
	switch tier {
	case "all":
		switch op {
		case "session-start":
			return "SessionStart", sessionStartText(source, sessionID, sessionEntries), true
		case "post":
			if files > 0 {
				return "PostToolUse", changeLine(files, deletes, key), true
			}
		case "bg":
			if files > 0 {
				return "PostToolUse", bgChangeLine(files, deletes, key), true
			}
		}
	case "major":
		switch op {
		case "session-start":
			return "SessionStart", sessionStartText(source, sessionID, sessionEntries), true
		case "post":
			if files >= MajorFilesThreshold || deletes > 0 {
				return "PostToolUse", changeLine(files, deletes, key), true
			}
		case "bg":
			if files >= MajorFilesThreshold || deletes > 0 {
				return "PostToolUse", bgChangeLine(files, deletes, key), true
			}
		}
	}
	return "", "", false
}

// sessionStartText picks the SessionStart orientation: a resume/compact restart
// of a session that already has snapshots gets a count-aware reminder; every
// other case (startup/clear/unknown source, or no prior entries) gets the
// generic one-time hint.
func sessionStartText(source, sessionID string, sessionEntries int) string {
	if (source == "resume" || source == "compact") && sessionEntries > 0 {
		return fmt.Sprintf("bashback is protecting this workspace%s; this session already has %d snapshot entries — review with `bashback list`, undo with `bashback undo`.", sessionTag(sessionID), sessionEntries)
	}
	return sessionHintText(sessionID)
}

// fileWord returns the singular or plural noun for a file count.
func fileWord(n int) string {
	if n == 1 {
		return "file"
	}
	return "files"
}

// changeLine is diff-first: review with `diff` before undoing with `restore`,
// the key given once per command.
func changeLine(files, deletes int, key string) string {
	if deletes > 0 {
		return fmt.Sprintf("bashback: command changed %d %s (%d deleted); review: bashback diff %s; undo: bashback restore %s", files, fileWord(files), deletes, key, key)
	}
	return fmt.Sprintf("bashback: command changed %d %s; review: bashback diff %s; undo: bashback restore %s", files, fileWord(files), key, key)
}

// bgChangeLine is the changeLine variant for a backgrounded command's final
// state, captured after it finished.
func bgChangeLine(files, deletes int, key string) string {
	if deletes > 0 {
		return fmt.Sprintf("bashback: background command finished, changed %d %s (%d deleted); review: bashback diff %s; undo: bashback restore %s", files, fileWord(files), deletes, key, key)
	}
	return fmt.Sprintf("bashback: background command finished, changed %d %s; review: bashback diff %s; undo: bashback restore %s", files, fileWord(files), key, key)
}

// emitContextFor writes the platform-appropriate context envelope to stdout when
// the tier and op call for it. Any failure to build it is swallowed — nothing
// written, caller still exits 0 (fail-open).
//
// origin "" (claude) and "codex" share the hookSpecificOutput envelope. cursor
// only accepts injected context at sessionStart (as {"additional_context": ...});
// for post/bg it stays silent — its before-hook permission is handled separately
// via emitCursorAllow.
func emitContextFor(origin string, stdout io.Writer, op, tier string, files, deletes int, key, source, sessionID string, sessionEntries int) {
	if origin == "cursor" && op != "session-start" {
		return
	}
	event, text, ok := contextMessage(op, tier, files, deletes, key, source, sessionID, sessionEntries)
	if !ok {
		return
	}
	if origin == "cursor" {
		b, err := json.Marshal(cursorContext{AdditionalContext: text})
		if err != nil {
			return
		}
		_, _ = fmt.Fprintln(stdout, string(b))
		return
	}
	b, err := json.Marshal(hookOutput{HookSpecificOutput: hookSpecific{HookEventName: event, AdditionalContext: text}})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(stdout, string(b))
}

// emitContext is the claude/codex-origin shorthand for emitContextFor.
func emitContext(stdout io.Writer, op, tier string, files, deletes int, key, source, sessionID string, sessionEntries int) {
	emitContextFor("", stdout, op, tier, files, deletes, key, source, sessionID, sessionEntries)
}

// emitCursorAllow prints the explicit allow envelope for cursor before-hooks;
// an empty stdout is also treated as allow, this is belt-and-braces.
func emitCursorAllow(w io.Writer) {
	_, _ = fmt.Fprintln(w, `{"permission":"allow"}`)
}

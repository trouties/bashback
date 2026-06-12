// Package hook is the Claude Code hook entrypoint. Its one invariant: whatever
// happens, exit 0 and never block the agent — fail-open. The outermost
// recover and a self-imposed time budget are the first line of defense; the
// hooks `timeout` config is only the last.
package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/trouties/bashback/internal/client"
	"github.com/trouties/bashback/internal/config"
	"github.com/trouties/bashback/internal/daemon"
	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/internal/snapshot"
)

// Budget is the hook's self-imposed deadline; well under the official 600s and
// the configured 5s timeout, so bashback exits cleanly on its own terms. It is a
// var only so tests can shorten it; production never reassigns it.
var Budget = 5 * time.Second

// payloadMaxBytes caps hook stdin. Real platform payloads are a few KiB; a
// runaway or never-closing reader must not stall the fail-open path.
const payloadMaxBytes = 4 << 20

// payload is the subset of the Claude Code hook stdin we consume.
type payload struct {
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	ToolUseID     string `json:"tool_use_id"`
	HookEventName string `json:"hook_event_name"`
	// Source is the SessionStart trigger: startup|resume|clear|compact. resume and
	// compact mean the agent is re-entering a session that may already have
	// snapshots, so the orientation hint carries the entry count.
	Source    string `json:"source"`
	ToolInput struct {
		Command string `json:"command"`
		// run_in_background is reported (true) on both Pre and Post when the command
		// runs in the background; absent/false otherwise.
		RunInBackground bool `json:"run_in_background"`
		// TaskID identifies the background task on TaskOutput/TaskStop payloads (the
		// `hook bg` entrypoint); equals the original Bash's backgroundTaskId.
		TaskID string `json:"task_id"`
	} `json:"tool_input"`
	// ToolResponse carries the tool's result: backgroundTaskId on the original Bash
	// post when backgrounded, plus the TaskOutput/TaskStop completion fields that
	// drive `hook bg`. Kept raw because codex sends it as a plain JSON string while
	// claude/cursor send an object; decode on demand via toolResponse so a string,
	// object, or absent value all parse without error.
	ToolResponse json.RawMessage `json:"tool_response"`
	ToolName     string          `json:"tool_name"`
	// Not decoded from hook stdin; set during payload normalization before dispatch.
	Origin string
}

// toolResponseFields is the structured view of tool_response we consume.
type toolResponseFields struct {
	BackgroundTaskID string `json:"backgroundTaskId"`
	Task             struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exitCode"`
	} `json:"task"`
}

// toolResponse decodes the raw tool_response on demand. A codex-style string or
// an absent value yields the zero struct (the unmarshal error is intentionally
// ignored — fail-open, no fields to read).
func (p payload) toolResponse() toolResponseFields {
	var tr toolResponseFields
	if len(p.ToolResponse) == 0 {
		return tr
	}
	_ = json.Unmarshal(p.ToolResponse, &tr)
	return tr
}

// Run executes one hook op (pre|post|session-start|session-end) and ALWAYS
// returns 0. The op is the bashback subcommand, not the Claude event name.
// stdout carries the optional agent-context injection envelope; it
// stays empty unless context_feedback opts in.
func Run(op string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	// Outermost guard: any panic still exits 0.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "bashback hook: recovered: %v\n", r)
			code = 0
		}
	}()

	// Resolve the layout up front (no dependency on the payload) so even a parse
	// failure has a place to record itself.
	layout, lerr := paths.Default()

	// The budget covers stdin parsing too: a reader that never closes must not
	// hang the hook before the deadline even exists.
	ctx, cancel := context.WithTimeout(context.Background(), Budget)
	defer cancel()

	type parsedPayload struct {
		p   payload
		err error
	}
	ch := make(chan parsedPayload, 1)
	go func() {
		p, perr := parsePayload(stdin)
		ch <- parsedPayload{p, perr}
	}()
	var p payload
	var err error
	select {
	case got := <-ch:
		p, err = got.p, got.err
	case <-ctx.Done():
		fmt.Fprintln(stderr, "bashback hook: stdin read exceeded the hook budget")
		return 0
	}
	if err != nil {
		// No payload, no cwd to key the project: fall back to the hook's own working
		// directory (Claude Code runs hooks with the project as cwd). No layout means
		// nowhere to record, so give up silently. Fail-open either way.
		fmt.Fprintf(stderr, "bashback hook: payload parse: %v\n", err)
		if lerr == nil {
			if wd, werr := os.Getwd(); werr == nil && wd != "" {
				logHookError(layout, wd, op, "", "", "", "payload parse: "+err.Error())
			}
		}
		return 0
	}

	if lerr != nil {
		fmt.Fprintf(stderr, "bashback hook: layout: %v\n", lerr)
		return 0
	}

	// cwd is the work-tree root every git operation writes into; an unvetted
	// relative or empty path must never reach the engine. Log under the hook's
	// own working dir (a real project root) so the bogus cwd keys nothing.
	if needsCWD(op) && !filepath.IsAbs(p.CWD) {
		fmt.Fprintf(stderr, "bashback hook: non-absolute cwd %q\n", p.CWD)
		if wd, werr := os.Getwd(); werr == nil && wd != "" {
			logHookError(layout, wd, op, p.HookEventName, p.SessionID, p.ToolUseID, "non-absolute cwd")
		}
		return 0
	}

	switch op {
	case "pre":
		dispatch(ctx, layout, p, daemon.OpPre, stderr)
		if p.Origin == "cursor" {
			emitCursorAllow(stdout)
		}
	case "post":
		resp := dispatch(ctx, layout, p, daemon.OpPost, stderr)
		emitContextFor(p.Origin, stdout, "post", tierFor(layout, p.CWD), resp.Files, resp.Deletes, resp.Key, "", "", 0)
	case "bg":
		// A backgrounded command's completion (TaskOutput status=completed) or kill
		// (TaskStop). Capture its final state now — after the task is GC'd the read
		// no longer fires. Running/missing-field events no-op (fail-open).
		taskID, trigger := bgFinalTrigger(p)
		if !trigger {
			return 0
		}
		resp := dispatchBg(ctx, layout, p, taskID, stderr)
		emitContextFor(p.Origin, stdout, "bg", tierFor(layout, p.CWD), resp.Files, resp.Deletes, resp.Key, "", "", 0)
	case "session-start":
		resp := dispatch(ctx, layout, p, daemon.OpWarm, stderr)
		emitContextFor(p.Origin, stdout, "session-start", tierFor(layout, p.CWD), 0, 0, "", p.Source, p.SessionID, resp.SessionEntries)
	case "session-end":
		// Best-effort: tolerate no daemon running.
		dispatch(ctx, layout, p, daemon.OpShutdown, stderr)
	default:
		fmt.Fprintf(stderr, "bashback hook: unknown op %q\n", op)
	}
	return 0
}

func needsCWD(op string) bool {
	return op == "pre" || op == "post" || op == "bg" || op == "session-start"
}

func tierFor(layout paths.Layout, workdir string) string {
	return config.Load(layout, workdir, config.OSEnv()).ContextFeedback
}

func newHookEngine(layout paths.Layout) *snapshot.Engine {
	eng := snapshot.New(layout, gitx.ExecRunner{})
	eng.MaxFileBytesFor = func(workdir string) int64 {
		return config.Load(layout, workdir, config.OSEnv()).MaxFileBytes
	}
	eng.ProtectPathsFor = func(workdir string) []string {
		return config.Load(layout, workdir, config.OSEnv()).ProtectPaths
	}
	return eng
}

func dispatch(ctx context.Context, layout paths.Layout, p payload, op string, stderr io.Writer) daemon.Response {
	c := client.New(layout, newHookEngine(layout), p.SessionID)
	req := daemon.Request{
		Op:         op,
		Workdir:    p.CWD,
		SessionID:  p.SessionID,
		ToolUseID:  p.ToolUseID,
		Command:    p.ToolInput.Command,
		Background: p.ToolInput.RunInBackground,
		BgTaskID:   p.toolResponse().BackgroundTaskID,
		CmdHash:    commandHash(p.ToolInput.Command),
		Source:     p.Source,
		Origin:     p.Origin,
	}
	resp := c.Snapshot(ctx, req)
	if !resp.OK {
		if resp.Error != "" {
			fmt.Fprintf(stderr, "bashback hook %s: %s\n", op, resp.Error)
		}
		logHookError(layout, p.CWD, op, p.HookEventName, p.SessionID, p.ToolUseID, firstNonEmpty(resp.Error, resp.Status))
	}
	return resp
}

// dispatchBg sends an OpBgFinal keyed by the background task id (tool_input.task_id
// on the TaskOutput/TaskStop event).
func dispatchBg(ctx context.Context, layout paths.Layout, p payload, taskID string, stderr io.Writer) daemon.Response {
	c := client.New(layout, newHookEngine(layout), p.SessionID)
	req := daemon.Request{
		Op:        daemon.OpBgFinal,
		Workdir:   p.CWD,
		SessionID: p.SessionID,
		BgTaskID:  taskID,
		Origin:    p.Origin,
	}
	resp := c.Snapshot(ctx, req)
	if !resp.OK {
		if resp.Error != "" {
			fmt.Fprintf(stderr, "bashback hook bg: %s\n", resp.Error)
		}
		logHookError(layout, p.CWD, "bg", p.HookEventName, p.SessionID, p.ToolUseID, firstNonEmpty(resp.Error, resp.Status))
	}
	return resp
}

// bgFinalTrigger reports whether a TaskOutput/TaskStop event is a background
// completion worth capturing, returning the task id. TaskOutput counts only when
// status is completed; TaskStop (kill) is unconditionally terminal.
// BashOutput/KillShell are accepted as version aliases. A missing task id no-ops.
func bgFinalTrigger(p payload) (string, bool) {
	id := p.ToolInput.TaskID
	if id == "" {
		return "", false
	}
	switch p.ToolName {
	case "TaskOutput", "BashOutput":
		if p.toolResponse().Task.Status == "completed" {
			return id, true
		}
	case "TaskStop", "KillShell":
		return id, true
	}
	return "", false
}

func parsePayload(r io.Reader) (payload, error) {
	var raw rawPayload
	if err := json.NewDecoder(io.LimitReader(r, payloadMaxBytes)).Decode(&raw); err != nil {
		return payload{}, err
	}
	return normalize(raw), nil
}

// commandHash backs the composite-key fallback; harmless to always compute.
func commandHash(cmd string) string {
	sum := sha256.Sum256([]byte(cmd))
	return hex.EncodeToString(sum[:])[:16]
}

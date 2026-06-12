package hook

import "os"

// rawPayload is the superset of all three platforms' hook stdin. Cursor carries
// conversation_id (its tool_use_id is real, keep it); codex carries turn_id
// (misreading codex as claude is benign, semantics are identical); everything
// else is claude. Cursor's cwd is always empty, so its project path comes from
// workspace_roots. Cursor can surface the command at top level when
// tool_input.command is absent, so the cursor branch falls back to Command.
type rawPayload struct {
	payload
	ConversationID string   `json:"conversation_id"`
	CursorVersion  string   `json:"cursor_version"`
	TurnID         string   `json:"turn_id"`
	WorkspaceRoots []string `json:"workspace_roots"`
	Command        string   `json:"command"`
}

func normalize(r rawPayload) payload {
	switch {
	case r.ConversationID != "" || r.CursorVersion != "":
		p := r.payload
		p.Origin = "cursor"
		if r.ConversationID != "" {
			p.SessionID = r.ConversationID
		}
		if p.ToolInput.Command == "" {
			p.ToolInput.Command = r.Command
		}
		if p.CWD == "" && len(r.WorkspaceRoots) > 0 {
			p.CWD = r.WorkspaceRoots[0]
		}
		// Cursor sends no cwd; with no workspace_roots either (a loose file, no
		// folder open) fall back to the hook process's own dir rather than "".
		if p.CWD == "" {
			if wd, err := os.Getwd(); err == nil {
				p.CWD = wd
			}
		}
		return p
	case r.TurnID != "":
		p := r.payload
		p.Origin = "codex"
		return p
	default:
		return r.payload
	}
}

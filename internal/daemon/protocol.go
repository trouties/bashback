// Package daemon is the per-session single-writer that serializes all shadow-repo
// access through one worker, eliminating index.lock races. Clients
// speak a JSON Lines protocol over a unix socket.
package daemon

// Op values for Request.Op.
const (
	OpPre      = "pre"
	OpPost     = "post"
	OpBgFinal  = "bgfinal"  // background command completion: capture final state
	OpWarm     = "warm"     // SessionStart prewarm
	OpPing     = "ping"     // liveness probe
	OpShutdown = "shutdown" // SessionEnd / explicit stop
)

// Request is one JSON line from a client. The daemon redacts Command before it
// reaches the journal; the raw text never lands on disk via the daemon path.
type Request struct {
	Op        string `json:"op"`
	Workdir   string `json:"workdir"`
	SessionID string `json:"session_id"`
	ToolUseID string `json:"tool_use_id"`
	Command   string `json:"command,omitempty"`
	// Background forwards the command's run_in_background flag: bashback only
	// snapshots up to the moment the command is backgrounded, so the entry is
	// noted as unprotected for any later writes.
	Background bool `json:"background,omitempty"`
	// BgTaskID forwards the original Bash post's tool_response.backgroundTaskId, the
	// key a later TaskOutput/TaskStop completion uses to find this entry for the
	// bgfinal capture. Empty on non-background commands.
	BgTaskID string `json:"bg_task_id,omitempty"`
	// Source forwards the SessionStart trigger (startup|resume|clear|compact) so
	// warm can decide whether to surface the session's prior snapshot count.
	Source string `json:"source,omitempty"`
	// Origin tags which agent platform produced this entry (codex|cursor; empty=Claude).
	Origin string `json:"origin,omitempty"`
	// Composite-key fallback fields, forwarded for journaling when tool_use_id
	// is unavailable.
	Seq     int    `json:"seq,omitempty"`
	CmdHash string `json:"cmd_hash,omitempty"`
}

// Response is one JSON line back to the client. Fields are additive: an older
// client simply ignores unknown keys, so new fields never break the protocol.
type Response struct {
	OK         bool   `json:"ok"`
	Status     string `json:"status,omitempty"`
	PreSHA     string `json:"pre_sha,omitempty"`
	PostSHA    string `json:"post_sha,omitempty"`
	Overlapped bool   `json:"overlapped,omitempty"`
	Error      string `json:"error,omitempty"`
	// Post accounting forwarded for agent context injection: changed-file count,
	// of which deletions, and the journal key for the just-completed entry.
	Files   int    `json:"files,omitempty"`
	Deletes int    `json:"deletes,omitempty"`
	Key     string `json:"key,omitempty"`
	// SessionEntries is the number of journal entries already owned by the
	// requesting session, filled by warm so a resume/compact SessionStart can tell
	// the agent it has prior snapshots.
	SessionEntries int `json:"session_entries,omitempty"`
}

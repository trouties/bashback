package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/trouties/bashback/internal/paths"
)

// hookLogCap caps hook.log before a single-generation rotation. Past
// 1 MiB the file is renamed to hook.log.1 (overwriting any prior one) and a
// fresh file starts.
const hookLogCap = 1 << 20

// hookLogRecord is one JSONL line. Empty fields are omitted so doctor's summary
// stays clean; ts is RFC3339 UTC.
type hookLogRecord struct {
	TS        string `json:"ts"`
	Hook      string `json:"hook"`
	Event     string `json:"event,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Err       string `json:"err"`
}

// logHookError appends one best-effort JSONL line recording a swallowed hook
// failure; every error here is dropped on the floor (fail-open, never block the
// agent). No lock — O_APPEND of a single short line is atomic enough; the log
// tolerates rare interleaving.
func logHookError(layout paths.Layout, workdir, op, event, sessionID, toolUseID, errMsg string) {
	dir := layout.RepoDir(workdir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	logPath := filepath.Join(dir, "hook.log")

	// Rotate before writing if over cap.
	if fi, err := os.Stat(logPath); err == nil && fi.Size() >= hookLogCap {
		_ = os.Rename(logPath, logPath+".1")
	}

	rec := hookLogRecord{
		TS:        time.Now().UTC().Format(time.RFC3339),
		Hook:      op,
		Event:     event,
		SessionID: sessionID,
		ToolUseID: toolUseID,
		Err:       errMsg,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	b = append(b, '\n')

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(b)
}

// firstNonEmpty returns the first non-empty string (daemon Response Error, then
// Status fallback, when recording a failure).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

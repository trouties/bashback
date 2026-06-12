//go:build unix

// Package journal is bashback's append-only audit ledger: one JSON object per
// line, per project. It is never deleted, even after the underlying
// snapshots are GC'd. The primary key is abstracted (Keyer) so it can switch
// from tool_use_id to a composite key without touching callers.
package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// SchemaVersion is the journal row format this build writes and the highest it
// reads; a newer `v` on disk is a hard error, not a guess.
const SchemaVersion = 1

// commandMax is the redacted command length cap before storage.
const commandMax = 512

// FilesMax caps the per-entry changed-files list; overflow is counted in
// FilesOmitted so the "how much changed" fact survives truncation.
const FilesMax = 100

// FileChange is one changed-path summary recorded on a post entry: the path and
// its single-letter status (A/M/D; renames are decomposed via --no-renames).
type FileChange struct {
	P string `json:"p"`
	S string `json:"s"`
}

type Status string

const (
	StatusProtected          Status = "protected"
	StatusSkippedNoChange    Status = "skipped_no_change"
	StatusUnprotectedTimeout Status = "unprotected_timeout"
	StatusUnprotectedError   Status = "unprotected_error"
	StatusRestored           Status = "restored"
	// StatusManual marks a `snap` checkpoint: a deliberate pre-only snapshot taken
	// outside any session.
	StatusManual Status = "manual"
)

// Entry is one journal row. Half-rows (degraded direct-write path) populate only
// the pre or post side and are merged on read by Keyer.
type Entry struct {
	V          int    `json:"v"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	TS         string `json:"ts,omitempty"`
	Command    string `json:"command,omitempty"`
	PreSHA     string `json:"pre_sha,omitempty"`
	PostSHA    string `json:"post_sha,omitempty"`
	Status     Status `json:"status,omitempty"`
	Overlapped bool   `json:"overlapped,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Note       string `json:"note,omitempty"`

	// Files summarizes the paths a command changed (pre..post), capped at
	// FilesMax with the remainder counted in FilesOmitted. Additive and optional:
	// old rows lack it, and `v` stays 1.
	Files        []FileChange `json:"files,omitempty"`
	FilesOmitted int          `json:"files_omitted,omitempty"`

	// BgTaskID is the Claude Code background task id (the original Bash post's
	// tool_response.backgroundTaskId), recorded only on backgrounded entries. It is
	// the lookup key the later TaskOutput/TaskStop completion event uses to find
	// this entry and append a bgfinal snapshot. Additive; `v` stays 1.
	BgTaskID string `json:"bg_task_id,omitempty"`

	// Origin tags which agent platform produced this entry (codex|cursor).
	// Empty means Claude Code, the pre-M9 writer. Additive; `v` stays 1.
	Origin string `json:"origin,omitempty"`

	// Composite-key fallback fields (used only when S2 ruled tool_use_id out).
	Seq     int    `json:"seq,omitempty"`
	CmdHash string `json:"cmd_hash,omitempty"`
}

// Keyer extracts the merge/identity key for an entry. The default keys on
// tool_use_id; CompositeKeyer is the S2 fallback.
type Keyer interface {
	Key(Entry) string
}

type ToolUseIDKeyer struct{}

func (ToolUseIDKeyer) Key(e Entry) string { return e.ToolUseID }

type CompositeKeyer struct{}

func (CompositeKeyer) Key(e Entry) string {
	return e.SessionID + "/" + strconv.Itoa(e.Seq) + "/" + e.CmdHash
}

// DefaultKeyer uses tool_use_id until proven absent.
var DefaultKeyer Keyer = ToolUseIDKeyer{}

// Append writes one row under an exclusive flock over a short critical section.
// O_APPEND keeps concurrent writers from interleaving; the flock additionally
// serializes cross-process degraded writers.
func Append(path string, e Entry) error {
	if e.V == 0 {
		e.V = SchemaVersion
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	_, err = f.Write(b)
	return err
}

// Read returns the raw rows in file order, including unmerged half-rows. A row
// whose version exceeds SchemaVersion is a hard error.
func Read(path string) ([]Entry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Entry
	lines := strings.Split(string(b), "\n")
	terminated := strings.HasSuffix(string(b), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// A torn final line (crash / ENOSPC mid-append) must not wedge the
			// whole ledger; anything earlier is real corruption and stays fatal.
			if i == len(lines)-1 && !terminated {
				continue
			}
			return nil, fmt.Errorf("journal line %d: %w (run `bashback doctor --repair`)", i+1, err)
		}
		if e.V > SchemaVersion {
			return nil, fmt.Errorf("journal line %d: schema version %d newer than supported %d", i+1, e.V, SchemaVersion)
		}
		out = append(out, e)
	}
	return out, nil
}

// ReadMerged folds half-rows that share a key into one combined entry, in
// first-seen order. Entries with an empty key never merge with each other.
func ReadMerged(path string, k Keyer) ([]Entry, error) {
	raw, err := Read(path)
	if err != nil {
		return nil, err
	}
	if k == nil {
		k = DefaultKeyer
	}
	idx := map[string]int{}
	var out []Entry
	for _, e := range raw {
		key := k.Key(e)
		if key == "" {
			out = append(out, e)
			continue
		}
		if pos, ok := idx[key]; ok {
			out[pos] = mergeEntry(out[pos], e)
			continue
		}
		idx[key] = len(out)
		out = append(out, e)
	}
	return out, nil
}

// PreSHAFor returns the pre snapshot recorded for a key, if any — the bridge a
// post-side writer uses to pair with a pre written by a different process (a
// degraded pre while the daemon was down, or vice versa). A read failure or a
// missing key yields "".
func PreSHAFor(path string, k Keyer, key string) string {
	if k == nil {
		k = DefaultKeyer
	}
	entries, err := ReadMerged(path, k)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if k.Key(e) == key {
			return e.PreSHA
		}
	}
	return ""
}

// mergeEntry folds src (written later) into dst, with later non-zero fields
// winning. Pre is written first, post second, so post fields take precedence.
func mergeEntry(dst, src Entry) Entry {
	if src.V != 0 {
		dst.V = src.V
	}
	if src.ToolUseID != "" {
		dst.ToolUseID = src.ToolUseID
	}
	if src.SessionID != "" {
		dst.SessionID = src.SessionID
	}
	if src.TS != "" {
		dst.TS = src.TS
	}
	if src.Command != "" {
		dst.Command = src.Command
	}
	if src.PreSHA != "" {
		dst.PreSHA = src.PreSHA
	}
	if src.PostSHA != "" {
		dst.PostSHA = src.PostSHA
	}
	if src.Status != "" {
		dst.Status = src.Status
	}
	if src.Overlapped {
		dst.Overlapped = true
	}
	if src.DurationMS != 0 {
		dst.DurationMS = src.DurationMS
	}
	if src.Note != "" {
		dst.Note = src.Note
	}
	if len(src.Files) > 0 {
		dst.Files = src.Files
	}
	if src.FilesOmitted != 0 {
		dst.FilesOmitted = src.FilesOmitted
	}
	if src.BgTaskID != "" {
		dst.BgTaskID = src.BgTaskID
	}
	if src.Origin != "" {
		dst.Origin = src.Origin
	}
	if src.Seq != 0 {
		dst.Seq = src.Seq
	}
	if src.CmdHash != "" {
		dst.CmdHash = src.CmdHash
	}
	return dst
}

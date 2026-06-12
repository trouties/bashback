package hook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouties/bashback/internal/paths"
)

// hookLogEnv returns a layout rooted at a temp home plus a workdir; the hook.log
// lands under the project's repo dir.
func hookLogEnv(t *testing.T) (paths.Layout, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".bashback")
	return paths.New(home), t.TempDir()
}

func readHookLog(t *testing.T, layout paths.Layout, workdir string) []map[string]any {
	t.Helper()
	f, err := os.Open(layout.HookLogPath(workdir))
	if err != nil {
		t.Fatalf("open hook.log: %v", err)
	}
	defer f.Close()
	var rows []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("hook.log line is not valid JSON: %q (%v)", line, err)
		}
		rows = append(rows, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestHookLogAppendsJSONL(t *testing.T) {
	layout, work := hookLogEnv(t)
	logHookError(layout, work, "post", "PostToolUse", "sess1", "toolu_1", "first failure")
	logHookError(layout, work, "pre", "PreToolUse", "sess1", "toolu_2", "second failure")

	rows := readHookLog(t, layout, work)
	if len(rows) != 2 {
		t.Fatalf("want 2 JSONL rows, got %d", len(rows))
	}
	r0 := rows[0]
	for _, k := range []string{"ts", "hook", "session_id", "tool_use_id", "err"} {
		if _, ok := r0[k]; !ok {
			t.Fatalf("row 0 missing field %q: %v", k, r0)
		}
	}
	if r0["hook"] != "post" || r0["err"] != "first failure" {
		t.Fatalf("row 0 unexpected: %v", r0)
	}
	if !strings.Contains(r0["ts"].(string), "T") {
		t.Fatalf("ts not RFC3339-ish: %v", r0["ts"])
	}
	if rows[1]["hook"] != "pre" {
		t.Fatalf("row 1 hook = %v", rows[1]["hook"])
	}
}

// The inbound Claude event name (hook_event_name) is recorded alongside the
// bashback op, so a hook.log line tells which platform event produced it.
func TestHookLogRecordsEventName(t *testing.T) {
	layout, work := hookLogEnv(t)
	logHookError(layout, work, "post", "PostToolUse", "sess1", "toolu_1", "boom")
	rows := readHookLog(t, layout, work)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0]["event"] != "PostToolUse" {
		t.Fatalf("event = %v, want PostToolUse", rows[0]["event"])
	}
}

func TestHookLogOmitsEmptyFields(t *testing.T) {
	layout, work := hookLogEnv(t)
	logHookError(layout, work, "post", "", "", "", "parse failure")
	rows := readHookLog(t, layout, work)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if _, ok := rows[0]["session_id"]; ok {
		t.Fatalf("empty session_id must be omitted: %v", rows[0])
	}
	if _, ok := rows[0]["tool_use_id"]; ok {
		t.Fatalf("empty tool_use_id must be omitted: %v", rows[0])
	}
}

func TestHookLogRotatesAtCap(t *testing.T) {
	layout, work := hookLogEnv(t)
	logPath := layout.HookLogPath(work)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// Preseed an over-cap file.
	big := bytes.Repeat([]byte("x"), (1<<20)+10)
	if err := os.WriteFile(logPath, big, 0o600); err != nil {
		t.Fatal(err)
	}
	logHookError(layout, work, "post", "PostToolUse", "sess1", "toolu_1", "after rotation")

	// Old content moved aside.
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("hook.log.1 should exist after rotation: %v", err)
	}
	rows := readHookLog(t, layout, work)
	if len(rows) != 1 {
		t.Fatalf("new hook.log should have a single line, got %d", len(rows))
	}
}

func TestHookLogBestEffort(t *testing.T) {
	// RepoDir resolves under a path that cannot be created (a regular file sits
	// where a directory is needed). logHookError must not panic or surface error.
	home := filepath.Join(t.TempDir(), ".bashback")
	layout := paths.New(home)
	work := t.TempDir()
	// Make the root a file so MkdirAll under it fails.
	if err := os.WriteFile(home, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("logHookError panicked: %v", r)
		}
	}()
	logHookError(layout, work, "post", "PostToolUse", "sess1", "toolu_1", "should be swallowed")
}

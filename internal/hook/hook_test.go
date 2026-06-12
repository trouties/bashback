package hook

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
)

// hookEnv points the hook at a temp home and disables daemon spawning so the
// degraded direct-write path runs in-process (no forked binary).
func hookEnv(t *testing.T) (paths.Layout, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".bashback")
	work := t.TempDir()
	t.Setenv("BASHBACK_HOME", home)
	t.Setenv("BASHBACK_NO_SPAWN", "1")
	return paths.New(home), work
}

func payloadJSON(t *testing.T, cwd, toolUseID, cmd string) string {
	t.Helper()
	p := map[string]any{
		"session_id":  "sess1",
		"cwd":         cwd,
		"tool_use_id": toolUseID,
		"tool_input":  map[string]string{"command": cmd},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunExitsZeroOnGarbageStdin(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run("pre", strings.NewReader("this is not json"), &out, &errb); code != 0 {
		t.Fatalf("garbage stdin must exit 0, got %d", code)
	}
}

func TestRunExitsZeroOnEmptyStdin(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run("post", strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("empty stdin must exit 0, got %d", code)
	}
}

func TestRunUnknownOpStillZero(t *testing.T) {
	hookEnv(t)
	var out, errb bytes.Buffer
	in := payloadJSON(t, t.TempDir(), "t1", "ls")
	if code := Run("bogus-op", strings.NewReader(in), &out, &errb); code != 0 {
		t.Fatalf("unknown op must exit 0, got %d", code)
	}
}

func TestRunDegradedPrePostJournals(t *testing.T) {
	layout, work := hookEnv(t)
	var out, errb bytes.Buffer

	if code := Run("pre", strings.NewReader(payloadJSON(t, work, "t1", "touch f")), &out, &errb); code != 0 {
		t.Fatalf("pre exit %d, stderr=%s", code, errb.String())
	}
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run("post", strings.NewReader(payloadJSON(t, work, "t1", "touch f")), &out, &errb); code != 0 {
		t.Fatalf("post exit %d, stderr=%s", code, errb.String())
	}

	merged, err := journal.ReadMerged(layout.JournalPath(work), journal.ToolUseIDKeyer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 1 || merged[0].PreSHA == "" || merged[0].PostSHA == "" {
		t.Fatalf("hook did not journal a complete entry: %+v", merged)
	}
	if merged[0].Status != journal.StatusProtected {
		t.Fatalf("status = %s, want protected", merged[0].Status)
	}
}

// A backgrounded command's post payload carries tool_response.backgroundTaskId;
// the hook parses it and records it as the entry's bg_task_id.
func TestPayloadParsesBackgroundTaskID(t *testing.T) {
	layout, work := hookEnv(t)
	var out, errb bytes.Buffer
	bgPayload := func() string {
		p := map[string]any{
			"session_id":    "sess1",
			"cwd":           work,
			"tool_use_id":   "tbg",
			"tool_input":    map[string]any{"command": "sleep 600 &", "run_in_background": true},
			"tool_response": map[string]any{"backgroundTaskId": "b32gd3xrm"},
		}
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	Run("pre", strings.NewReader(bgPayload()), &out, &errb)
	os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644)
	Run("post", strings.NewReader(bgPayload()), &out, &errb)

	merged, _ := journal.ReadMerged(layout.JournalPath(work), journal.ToolUseIDKeyer{})
	if len(merged) != 1 || merged[0].BgTaskID != "b32gd3xrm" {
		t.Fatalf("hook did not record bg_task_id: %+v", merged)
	}
}

// The SessionStart payload's top-level `source` is parsed (drives the
// resume/compact entry-count hint).
func TestPayloadParsesSource(t *testing.T) {
	p, err := parsePayload(strings.NewReader(`{"session_id":"s","cwd":"/x","source":"resume"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != "resume" {
		t.Fatalf("source = %q, want resume", p.Source)
	}
}

// runBgOrig records a backgrounded command (pre+post with backgroundTaskId) so a
// later `hook bg` can find it. preFiles exist before the snapshot baseline.
func runBgOrig(t *testing.T, work, taskID string, preFiles map[string]string) {
	t.Helper()
	mk := func() string {
		p := map[string]any{
			"session_id":    "sess1",
			"cwd":           work,
			"tool_use_id":   "tbg",
			"tool_input":    map[string]any{"command": "sleep 600 &", "run_in_background": true},
			"tool_response": map[string]any{"backgroundTaskId": taskID},
		}
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for n, c := range preFiles {
		if err := os.WriteFile(filepath.Join(work, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	Run("pre", strings.NewReader(mk()), &out, &errb)
	Run("post", strings.NewReader(mk()), &out, &errb)
}

// bgEventJSON builds a TaskOutput/TaskStop PostToolUse payload.
func bgEventJSON(t *testing.T, work, toolName, taskID, status string) string {
	t.Helper()
	p := map[string]any{
		"session_id":    "sess1",
		"cwd":           work,
		"tool_name":     toolName,
		"tool_input":    map[string]any{"task_id": taskID},
		"tool_response": map[string]any{"task": map[string]any{"status": status, "exitCode": 0}},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func hasBgFinal(t *testing.T, layout paths.Layout, work string) bool {
	t.Helper()
	merged, _ := journal.ReadMerged(layout.JournalPath(work), journal.ToolUseIDKeyer{})
	for _, e := range merged {
		if e.ToolUseID == "bgfinal_tbg" && e.PostSHA != "" {
			return true
		}
	}
	return false
}

// A TaskOutput completion fires bgfinal, capturing the background command's
// post-backgrounding writes.
func TestHookBgDispatchesOnCompletion(t *testing.T) {
	layout, work := hookEnv(t)
	runBgOrig(t, work, "taskA", nil)
	os.WriteFile(filepath.Join(work, "out.log"), []byte("done\n"), 0o644)

	var out, errb bytes.Buffer
	if code := Run("bg", strings.NewReader(bgEventJSON(t, work, "TaskOutput", "taskA", "completed")), &out, &errb); code != 0 {
		t.Fatalf("hook bg exit %d: %s", code, errb.String())
	}
	if !hasBgFinal(t, layout, work) {
		t.Fatal("TaskOutput completion should create a bgfinal entry")
	}
}

// TaskStop (kill) is unconditionally terminal and fires bgfinal regardless of any
// status field.
func TestHookBgKillShellAlwaysFinal(t *testing.T) {
	layout, work := hookEnv(t)
	runBgOrig(t, work, "taskK", nil)
	os.WriteFile(filepath.Join(work, "out.log"), []byte("partial\n"), 0o644)

	var out, errb bytes.Buffer
	if code := Run("bg", strings.NewReader(bgEventJSON(t, work, "TaskStop", "taskK", "")), &out, &errb); code != 0 {
		t.Fatalf("hook bg exit %d: %s", code, errb.String())
	}
	if !hasBgFinal(t, layout, work) {
		t.Fatal("TaskStop should create a bgfinal entry")
	}
}

// A still-running TaskOutput (no completed status) does not capture; exit 0.
func TestHookBgRunningNoop(t *testing.T) {
	layout, work := hookEnv(t)
	runBgOrig(t, work, "taskR", nil)
	os.WriteFile(filepath.Join(work, "out.log"), []byte("mid\n"), 0o644)

	var out, errb bytes.Buffer
	if code := Run("bg", strings.NewReader(bgEventJSON(t, work, "TaskOutput", "taskR", "running")), &out, &errb); code != 0 {
		t.Fatalf("hook bg exit %d", code)
	}
	if hasBgFinal(t, layout, work) {
		t.Fatal("a still-running task must not create a bgfinal entry")
	}
}

// Missing task id no-ops (fail-open), exit 0.
func TestHookBgMissingFieldsNoop(t *testing.T) {
	layout, work := hookEnv(t)
	runBgOrig(t, work, "taskX", nil)
	os.WriteFile(filepath.Join(work, "out.log"), []byte("x\n"), 0o644)

	var out, errb bytes.Buffer
	if code := Run("bg", strings.NewReader(bgEventJSON(t, work, "TaskOutput", "", "completed")), &out, &errb); code != 0 {
		t.Fatalf("hook bg exit %d", code)
	}
	if hasBgFinal(t, layout, work) {
		t.Fatal("a missing task id must not create a bgfinal entry")
	}
}

// On a major-tier bgfinal with a deletion, the hook injects a "background command
// finished" envelope carrying the undo key; off tier emits nothing.
func TestHookBgInjectsOnMajorChange(t *testing.T) {
	setTier := func(t *testing.T, layout paths.Layout, work, tier string) {
		t.Helper()
		if err := layout.EnsureRepoDirs(work); err != nil {
			t.Fatal(err)
		}
		m, _ := layout.ReadMeta(work)
		m.ContextFeedback = tier
		if err := layout.WriteMeta(work, m); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("major injects", func(t *testing.T) {
		layout, work := hookEnv(t)
		setTier(t, layout, work, "major")
		runBgOrig(t, work, "taskM", map[string]string{"out.log": "present"})
		os.Remove(filepath.Join(work, "out.log")) // a deletion → qualifies for major

		var out, errb bytes.Buffer
		Run("bg", strings.NewReader(bgEventJSON(t, work, "TaskOutput", "taskM", "completed")), &out, &errb)
		s := out.String()
		if !strings.Contains(s, "background command finished") || !strings.Contains(s, "bgfinal_tbg") {
			t.Fatalf("major bgfinal should inject a finished envelope with the undo key, got %q", s)
		}
	})

	t.Run("off is silent", func(t *testing.T) {
		layout, work := hookEnv(t)
		setTier(t, layout, work, "off")
		runBgOrig(t, work, "taskO", map[string]string{"out.log": "present"})
		os.Remove(filepath.Join(work, "out.log"))

		var out, errb bytes.Buffer
		Run("bg", strings.NewReader(bgEventJSON(t, work, "TaskOutput", "taskO", "completed")), &out, &errb)
		if out.String() != "" {
			t.Fatalf("off tier must emit nothing, got %q", out.String())
		}
	})
}

func TestRunRedactsSecretsViaHook(t *testing.T) {
	layout, work := hookEnv(t)
	var out, errb bytes.Buffer
	secret := `curl -H "Authorization: Bearer sk-leakme"`
	Run("pre", strings.NewReader(payloadJSON(t, work, "t1", secret)), &out, &errb)
	os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644)
	Run("post", strings.NewReader(payloadJSON(t, work, "t1", secret)), &out, &errb)

	merged, _ := journal.ReadMerged(layout.JournalPath(work), journal.ToolUseIDKeyer{})
	if len(merged) == 0 || strings.Contains(merged[0].Command, "sk-leakme") {
		t.Fatalf("secret leaked through hook journaling: %+v", merged)
	}
}

// End-to-end through Run (daemon Response → emitContext wiring): `off` emits
// nothing even when files changed; `all` produces the additionalContext envelope.
func TestRunContextInjectionTiers(t *testing.T) {
	runCmd := func(t *testing.T, layout paths.Layout, work, tool string) string {
		t.Helper()
		var out, errb bytes.Buffer
		Run("pre", strings.NewReader(payloadJSON(t, work, tool, "touch f")), &out, &errb)
		if err := os.WriteFile(filepath.Join(work, tool), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		Run("post", strings.NewReader(payloadJSON(t, work, tool, "touch f")), &out, &errb)
		return out.String()
	}

	t.Run("off is silent", func(t *testing.T) {
		layout, work := hookEnv(t)
		if s := runCmd(t, layout, work, "t1"); s != "" {
			t.Fatalf("default off tier must emit nothing on stdout, got %q", s)
		}
	})

	t.Run("all injects", func(t *testing.T) {
		layout, work := hookEnv(t)
		if err := layout.EnsureRepoDirs(work); err != nil {
			t.Fatal(err)
		}
		m, _ := layout.ReadMeta(work)
		m.ContextFeedback = "all"
		if err := layout.WriteMeta(work, m); err != nil {
			t.Fatal(err)
		}
		s := runCmd(t, layout, work, "t2")
		if !strings.Contains(s, "additionalContext") || !strings.Contains(s, "bashback") {
			t.Fatalf("all tier should inject an envelope, got %q", s)
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &map[string]any{}); err != nil {
			t.Fatalf("injected stdout is not valid JSON: %v\n%s", err, s)
		}
	})
}

// When the payload can't be parsed there's no cwd to key the project, so the
// hook falls back to os.Getwd() and still records the swallowed failure.
func TestRunLogsPayloadParseFailure(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".bashback")
	t.Setenv("BASHBACK_HOME", home)
	t.Chdir(t.TempDir())
	wd, _ := os.Getwd() // the resolved cwd the fallback will use

	var out, errb bytes.Buffer
	if code := Run("post", strings.NewReader("{not json"), &out, &errb); code != 0 {
		t.Fatalf("parse failure must exit 0, got %d", code)
	}
	layout := paths.New(home)
	rows := readHookLog(t, layout, wd)
	if len(rows) != 1 {
		t.Fatalf("want 1 hook.log row, got %d", len(rows))
	}
	if rows[0]["hook"] != "post" {
		t.Fatalf("hook = %v, want post", rows[0]["hook"])
	}
	if !strings.Contains(rows[0]["err"].(string), "payload parse") {
		t.Fatalf("err = %v, want it to mention payload parse", rows[0]["err"])
	}
}

// A degraded dispatch failure (EnsureRepo can't build the session dirs) exits 0
// but is recorded in hook.log. RepoDir stays a real dir so the log is writable;
// sessions/ is occupied by a file to fail EnsureRepoDirs.
func TestRunLogsDispatchFailure(t *testing.T) {
	layout, work := hookEnv(t)
	repoDir := layout.RepoDir(work)
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "sessions"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run("post", strings.NewReader(payloadJSON(t, work, "t1", "touch f")), &out, &errb); code != 0 {
		t.Fatalf("dispatch failure must exit 0, got %d", code)
	}
	rows := readHookLog(t, layout, work)
	if len(rows) != 1 {
		t.Fatalf("want 1 hook.log row, got %d", len(rows))
	}
	if rows[0]["err"] == nil || rows[0]["err"].(string) == "" {
		t.Fatalf("dispatch failure row must carry a non-empty err: %v", rows[0])
	}
}

// The normal command path writes nothing to hook.log: the hot path stays
// allocation- and I/O-free (perf budget).
func TestRunNormalPathNoLog(t *testing.T) {
	layout, work := hookEnv(t)
	var out, errb bytes.Buffer
	if code := Run("pre", strings.NewReader(payloadJSON(t, work, "t1", "touch f")), &out, &errb); code != 0 {
		t.Fatalf("pre exit %d", code)
	}
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run("post", strings.NewReader(payloadJSON(t, work, "t1", "touch f")), &out, &errb); code != 0 {
		t.Fatalf("post exit %d", code)
	}
	if _, err := os.Stat(layout.HookLogPath(work)); !os.IsNotExist(err) {
		t.Fatalf("normal path must not create hook.log (stat err=%v)", err)
	}
}

// A non-absolute cwd is structurally untrusted: the hook must reject it
// fail-open (exit 0) and never key a project repo off it.
func TestRunRejectsNonAbsoluteCWD(t *testing.T) {
	layout, _ := hookEnv(t)
	var out, errb bytes.Buffer
	const relCWD = "relative/path"
	if code := Run("pre", strings.NewReader(payloadJSON(t, relCWD, "t1", "touch f")), &out, &errb); code != 0 {
		t.Fatalf("non-absolute cwd must exit 0, got %d", code)
	}
	if !strings.Contains(errb.String(), "non-absolute cwd") {
		t.Errorf("expected rejection notice on stderr, got %q", errb.String())
	}
	if _, err := os.Stat(layout.RepoDir(relCWD)); !os.IsNotExist(err) {
		t.Errorf("repo dir created for a rejected non-absolute cwd: %v", err)
	}
}

func TestRunBoundsStdinParsing(t *testing.T) {
	old := Budget
	Budget = 200 * time.Millisecond
	defer func() { Budget = old }()
	hookEnv(t)
	pr, _ := io.Pipe() // never written, never closed
	done := make(chan int, 1)
	go func() { done <- Run("pre", pr, io.Discard, io.Discard) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("code = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run hung on unterminated stdin")
	}
}

func TestParsePayloadCapsInputSize(t *testing.T) {
	huge := strings.NewReader(`{"cwd":"` + strings.Repeat("a", 8<<20) + `"}`)
	if _, err := parsePayload(huge); err == nil {
		t.Error("oversized payload accepted")
	}
}

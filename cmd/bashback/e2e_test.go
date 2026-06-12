package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
)

// bashPayload fakes the Claude Code hook stdin for a Bash tool call.
func bashPayload(t *testing.T, cwd, toolUseID, cmd string) string {
	t.Helper()
	return bashPayloadBG(t, cwd, toolUseID, cmd, false)
}

// bashPayloadBG is bashPayload with control over the run_in_background flag.
func bashPayloadBG(t *testing.T, cwd, toolUseID, cmd string, background bool) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"session_id":  "sess-e2e",
		"cwd":         cwd,
		"tool_use_id": toolUseID,
		"tool_input":  map[string]any{"command": cmd, "run_in_background": background},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runHookE2E(t *testing.T, op, payload string) int {
	t.Helper()
	var out, errb bytes.Buffer
	return run([]string{"hook", op}, strings.NewReader(payload), &out, &errb)
}

// The hook must exit 0 under every fault: three injected faults plus a clean
// happy path, all asserting exit 0.
func TestHookAlwaysExitsZero(t *testing.T) {
	t.Setenv("BASHBACK_NO_SPAWN", "1") // socket unreachable + no spawn -> degraded

	t.Run("happy degraded path", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), ".bashback")
		work := t.TempDir()
		t.Setenv("BASHBACK_HOME", home)
		if code := runHookE2E(t, "pre", bashPayload(t, work, "t1", "echo hi")); code != 0 {
			t.Fatalf("pre exit %d", code)
		}
		if err := os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := runHookE2E(t, "post", bashPayload(t, work, "t1", "echo hi")); code != 0 {
			t.Fatalf("post exit %d", code)
		}
	})

	t.Run("storage fault: home is a file", func(t *testing.T) {
		badHome := filepath.Join(t.TempDir(), "home-is-a-file")
		if err := os.WriteFile(badHome, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BASHBACK_HOME", badHome) // mkdir under it will fail
		if code := runHookE2E(t, "pre", bashPayload(t, t.TempDir(), "t2", "ls")); code != 0 {
			t.Fatalf("storage fault must still exit 0, got %d", code)
		}
	})

	t.Run("bad cwd: nonexistent work-tree", func(t *testing.T) {
		t.Setenv("BASHBACK_HOME", filepath.Join(t.TempDir(), ".bashback"))
		missing := filepath.Join(t.TempDir(), "does", "not", "exist")
		if code := runHookE2E(t, "pre", bashPayload(t, missing, "t3", "ls")); code != 0 {
			t.Fatalf("bad cwd must still exit 0, got %d", code)
		}
	})

	t.Run("garbage payload", func(t *testing.T) {
		t.Setenv("BASHBACK_HOME", filepath.Join(t.TempDir(), ".bashback"))
		var out, errb bytes.Buffer
		if code := run([]string{"hook", "post"}, strings.NewReader("{bad json"), &out, &errb); code != 0 {
			t.Fatalf("garbage payload must exit 0, got %d", code)
		}
	})
}

// A run_in_background command flows through the hook and gets flagged in the
// journal; the hook still exits 0. Uses the degraded path
// (NO_SPAWN) so no daemon process is forked by the test binary.
func TestBackgroundCommandFlaggedE2E(t *testing.T) {
	t.Setenv("BASHBACK_NO_SPAWN", "1")
	home := filepath.Join(t.TempDir(), ".bashback")
	work := t.TempDir()
	t.Setenv("BASHBACK_HOME", home)

	if code := runHookE2E(t, "pre", bashPayloadBG(t, work, "bg1", "server &", true)); code != 0 {
		t.Fatalf("pre exit %d", code)
	}
	// The backgrounded command's writes happen after post is taken: none here.
	if code := runHookE2E(t, "post", bashPayloadBG(t, work, "bg1", "server &", true)); code != 0 {
		t.Fatalf("post exit %d", code)
	}

	layout := paths.New(home)
	entries, err := journal.ReadMerged(layout.JournalPath(work), journal.DefaultKeyer)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.ToolUseID == "bg1" && strings.Contains(e.Note, "background") {
			found = true
		}
	}
	if !found {
		t.Fatalf("journal should flag the backgrounded command: %+v", entries)
	}
}

// TestInstallE2E drives `install --print` through the top-level dispatcher: it
// emits the merged hooks block to stdout and writes nothing.
func TestInstallE2E(t *testing.T) {
	t.Setenv("BASHBACK_HOME", t.TempDir())
	t.Setenv("HOME", filepath.Join(t.TempDir(), "home"))
	t.Chdir(t.TempDir())

	var out, errb bytes.Buffer
	if code := run([]string{"install", "--print"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("install --print exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"hooks"`) {
		t.Fatalf("install --print should emit a hooks block: %q", out.String())
	}
}

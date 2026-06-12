package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouties/bashback/skills"
)

// cursorFix mirrors codexFix: isolated HOME, project workdir, test binary as exe.
func cursorFix(t *testing.T) (work, home, exe string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	work = t.TempDir()
	exe, _ = os.Executable()
	return work, home, exe
}

func TestInstallCursorWritesHooks(t *testing.T) {
	work, home, exe := cursorFix(t)
	var out, errb bytes.Buffer
	if code := installCursor(work, home, false, false, true, exe, &out, &errb); code != 0 {
		t.Fatalf("installCursor exit %d: %s", code, errb.String())
	}
	target := filepath.Join(work, ".cursor", "hooks.json")
	hooks, err := parseCursorHooksFile(target)
	if err != nil {
		t.Fatalf("generated hooks.json unreadable: %v", err)
	}

	wantEvents := map[string]struct {
		matcher string
		op      string
	}{
		"preToolUse":   {"Shell", "pre"},
		"postToolUse":  {"Shell", "post"},
		"sessionStart": {"", "session-start"},
		"sessionEnd":   {"", "session-end"},
	}
	for event, want := range wantEvents {
		arr := hooks[event]
		if len(arr) != 1 {
			t.Fatalf("%s: expected exactly one entry, got %d", event, len(arr))
		}
		entry := arr[0]
		if entry.Matcher != want.matcher {
			t.Fatalf("%s matcher = %q, want %q", event, entry.Matcher, want.matcher)
		}
		wantCmd := exe + " hook " + want.op
		if entry.Command != wantCmd {
			t.Fatalf("%s command = %q, want %q", event, entry.Command, wantCmd)
		}
		if !filepath.IsAbs(firstToken(entry.Command)) {
			t.Fatalf("%s command is not an absolute path: %q", event, entry.Command)
		}
	}

	// Verify top-level version == 1.
	b, _ := os.ReadFile(target)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("file is not valid JSON: %v", err)
	}
	var ver int
	if err := json.Unmarshal(raw["version"], &ver); err != nil || ver != 1 {
		t.Fatalf("version should be 1, got raw=%s err=%v", raw["version"], err)
	}
}

func TestInstallCursorIdempotent(t *testing.T) {
	work, home, exe := cursorFix(t)
	target := filepath.Join(work, ".cursor", "hooks.json")

	var out1, err1 bytes.Buffer
	installCursor(work, home, false, false, true, exe, &out1, &err1)
	b1, _ := os.ReadFile(target)

	var out2, err2 bytes.Buffer
	installCursor(work, home, false, false, true, exe, &out2, &err2)
	b2, _ := os.ReadFile(target)

	if !bytes.Equal(b1, b2) {
		t.Fatalf("second install changed bytes:\n--- 1 ---\n%s\n--- 2 ---\n%s", b1, b2)
	}
	if !strings.Contains(out2.String(), "already wired") {
		t.Fatalf("second install should report already wired: %q", out2.String())
	}
}

func TestInstallCursorUpdatesStalePath(t *testing.T) {
	work, home, exe := cursorFix(t)
	target := filepath.Join(work, ".cursor", "hooks.json")
	// Seed with wrong matcher ("Bash" instead of "Shell") to verify Fix 1 rewrites it.
	pre := `{"version":1,"hooks":{"preToolUse":[{"matcher":"Bash","command":"/old/path/bashback hook pre"}]}}`
	mkdirWrite(t, target, pre)

	var out, errb bytes.Buffer
	if code := installCursor(work, home, false, false, true, exe, &out, &errb); code != 0 {
		t.Fatalf("installCursor exit %d: %s", code, errb.String())
	}

	hooks, _ := parseCursorHooksFile(target)
	pre0 := hooks["preToolUse"]
	if len(pre0) != 1 {
		t.Fatalf("stale pre entry should be updated in place, not duplicated; got %d entries", len(pre0))
	}
	if pre0[0].Command != exe+" hook pre" {
		t.Fatalf("stale path not rewritten: %q", pre0[0].Command)
	}
	if pre0[0].Matcher != "Shell" {
		t.Fatalf("stale matcher not corrected: got %q, want %q", pre0[0].Matcher, "Shell")
	}
}

func TestInstallCursorBacksUp(t *testing.T) {
	work, home, exe := cursorFix(t)
	target := filepath.Join(work, ".cursor", "hooks.json")
	pre := `{"version":1,"hooks":{"preToolUse":[{"matcher":"Shell","command":"/other/tool watch"}]}}`
	mkdirWrite(t, target, pre)

	var out, errb bytes.Buffer
	if code := installCursor(work, home, false, false, true, exe, &out, &errb); code != 0 {
		t.Fatalf("installCursor exit %d: %s", code, errb.String())
	}

	bak, err := os.ReadFile(target + ".bashback-bak")
	if err != nil {
		t.Fatalf("backup should exist: %v", err)
	}
	if string(bak) != pre {
		t.Fatalf("backup should match original bytes, got %q", bak)
	}

	hooks, _ := parseCursorHooksFile(target)
	var sawOther, sawBashback bool
	for _, entry := range hooks["preToolUse"] {
		if entry.Command == "/other/tool watch" {
			sawOther = true
		}
		if entry.Command == exe+" hook pre" {
			sawBashback = true
		}
	}
	if !sawOther || !sawBashback {
		t.Fatalf("preToolUse should keep both entries: other=%v bashback=%v", sawOther, sawBashback)
	}
}

func TestInstallCursorRejectsBadJSON(t *testing.T) {
	work, home, exe := cursorFix(t)
	target := filepath.Join(work, ".cursor", "hooks.json")
	mkdirWrite(t, target, "{ not json")

	var out, errb bytes.Buffer
	if code := installCursor(work, home, false, false, true, exe, &out, &errb); code != 1 {
		t.Fatalf("bad JSON should exit 1, got %d", code)
	}
	if b, _ := os.ReadFile(target); string(b) != "{ not json" {
		t.Fatalf("bad JSON file must be left untouched, got %q", b)
	}
	if !strings.Contains(errb.String(), "--print") {
		t.Fatalf("error should guide toward --print: %q", errb.String())
	}
}

func TestInstallCursorWritesRules(t *testing.T) {
	work, home, exe := cursorFix(t)
	var out, errb bytes.Buffer
	if code := installCursor(work, home, false, false, false, exe, &out, &errb); code != 0 {
		t.Fatalf("installCursor exit %d: %s", code, errb.String())
	}
	dest := filepath.Join(work, ".cursor", "rules", "bashback.mdc")
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("rules file not written: %v", err)
	}
	if !bytes.Equal(b, skills.BashbackCursorRules) {
		t.Fatalf("rules content mismatch: got %d bytes, want %d bytes", len(b), len(skills.BashbackCursorRules))
	}
}

func TestInstallCursorUserSkipsRules(t *testing.T) {
	work, home, exe := cursorFix(t)
	var out, errb bytes.Buffer
	if code := installCursor(work, home, true, false, false, exe, &out, &errb); code != 0 {
		t.Fatalf("installCursor --user exit %d: %s", code, errb.String())
	}
	dest := filepath.Join(work, ".cursor", "rules", "bashback.mdc")
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("rules file must not be written under --user")
	}
	if !strings.Contains(out.String(), "--user skips rules") {
		t.Fatalf("stdout should mention rules are skipped: %q", out.String())
	}
}

func TestInstallCursorNoSkillSkipsRules(t *testing.T) {
	work, home, exe := cursorFix(t)
	var out, errb bytes.Buffer
	if code := installCursor(work, home, false, false, true, exe, &out, &errb); code != 0 {
		t.Fatalf("installCursor noSkill exit %d: %s", code, errb.String())
	}
	dest := filepath.Join(work, ".cursor", "rules", "bashback.mdc")
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("rules file must not be written when noSkill=true")
	}
}

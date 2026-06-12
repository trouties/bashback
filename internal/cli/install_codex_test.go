package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouties/bashback/skills"
)

// codexFix mirrors installFix but for the codex hooks.json path: an isolated
// HOME, a project workdir, and the running test binary as the exe to wire.
func codexFix(t *testing.T) (work, home, exe string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	work = t.TempDir()
	exe, _ = os.Executable()
	return work, home, exe
}

func TestInstallCodexWritesHooks(t *testing.T) {
	work, home, exe := codexFix(t)
	var out, errb bytes.Buffer
	if code := installCodex(work, home, false, false, true, exe, &out, &errb); code != 0 {
		t.Fatalf("installCodex exit %d: %s", code, errb.String())
	}
	target := filepath.Join(work, ".codex", "hooks.json")
	hooks, err := parseHooksFile(target)
	if err != nil {
		t.Fatalf("generated hooks.json unreadable: %v", err)
	}
	want := map[string]string{
		"PreToolUse":   exe + " hook pre",
		"PostToolUse":  exe + " hook post",
		"SessionStart": exe + " hook session-start",
		"Stop":         exe + " hook session-end",
	}
	for event, wantCmd := range want {
		arr := hooks[event]
		if len(arr) != 1 || len(arr[0].Hooks) != 1 {
			t.Fatalf("%s: expected exactly one wiring, got %+v", event, arr)
		}
		got := arr[0].Hooks[0].Command
		if got != wantCmd {
			t.Fatalf("%s command = %q, want %q", event, got, wantCmd)
		}
		if !filepath.IsAbs(firstToken(got)) {
			t.Fatalf("%s command is not an absolute path: %q", event, got)
		}
	}
}

func TestInstallCodexIdempotent(t *testing.T) {
	work, home, exe := codexFix(t)
	target := filepath.Join(work, ".codex", "hooks.json")

	var out1, err1 bytes.Buffer
	installCodex(work, home, false, false, true, exe, &out1, &err1)
	b1, _ := os.ReadFile(target)

	var out2, err2 bytes.Buffer
	installCodex(work, home, false, false, true, exe, &out2, &err2)
	b2, _ := os.ReadFile(target)

	if !bytes.Equal(b1, b2) {
		t.Fatalf("second install changed bytes:\n--- 1 ---\n%s\n--- 2 ---\n%s", b1, b2)
	}
	if !strings.Contains(out2.String(), "already wired") {
		t.Fatalf("second install should report already wired: %q", out2.String())
	}
}

func TestInstallCodexUpdatesStalePath(t *testing.T) {
	work, home, exe := codexFix(t)
	target := filepath.Join(work, ".codex", "hooks.json")
	pre := `{"hooks":{"PreToolUse":[{"matcher":"^Bash$","hooks":[{"type":"command","command":"/old/path/bashback hook pre","timeout":5}]}]}}`
	mkdirWrite(t, target, pre)

	var out, errb bytes.Buffer
	if code := installCodex(work, home, false, false, true, exe, &out, &errb); code != 0 {
		t.Fatalf("installCodex exit %d: %s", code, errb.String())
	}

	hooks, _ := parseHooksFile(target)
	pre0 := hooks["PreToolUse"]
	cmds := 0
	for _, m := range pre0 {
		cmds += len(m.Hooks)
	}
	if cmds != 1 {
		t.Fatalf("stale pre entry should be updated in place, not duplicated; got %d commands", cmds)
	}
	if pre0[0].Hooks[0].Command != exe+" hook pre" {
		t.Fatalf("stale path not rewritten: %q", pre0[0].Hooks[0].Command)
	}
}

func TestInstallCodexBacksUp(t *testing.T) {
	work, home, exe := codexFix(t)
	target := filepath.Join(work, ".codex", "hooks.json")
	pre := `{"hooks":{"PreToolUse":[{"matcher":"^Bash$","hooks":[{"type":"command","command":"/other/tool watch","timeout":3}]}]}}`
	mkdirWrite(t, target, pre)

	var out, errb bytes.Buffer
	if code := installCodex(work, home, false, false, true, exe, &out, &errb); code != 0 {
		t.Fatalf("installCodex exit %d: %s", code, errb.String())
	}

	bak, err := os.ReadFile(target + ".bashback-bak")
	if err != nil {
		t.Fatalf("backup should exist: %v", err)
	}
	if string(bak) != pre {
		t.Fatalf("backup should match original bytes, got %q", bak)
	}

	hooks, _ := parseHooksFile(target)
	var sawOther, sawBashback bool
	for _, m := range hooks["PreToolUse"] {
		for _, hk := range m.Hooks {
			if hk.Command == "/other/tool watch" {
				sawOther = true
			}
			if hk.Command == exe+" hook pre" {
				sawBashback = true
			}
		}
	}
	if !sawOther || !sawBashback {
		t.Fatalf("PreToolUse should keep both entries: other=%v bashback=%v", sawOther, sawBashback)
	}
}

func TestInstallCodexWritesAgentsSkill(t *testing.T) {
	work, home, exe := codexFix(t)
	var out, errb bytes.Buffer
	if code := installCodex(work, home, false, false, false, exe, &out, &errb); code != 0 {
		t.Fatalf("installCodex exit %d: %s", code, errb.String())
	}
	dest := filepath.Join(home, ".agents", "skills", "bashback", "SKILL.md")
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("skill not written: %v", err)
	}
	if !bytes.Equal(b, skills.BashbackSKILL) {
		t.Fatalf("skill content mismatch: got %d bytes, want %d bytes", len(b), len(skills.BashbackSKILL))
	}

	var out2, errb2 bytes.Buffer
	if code := installCodex(work, home, false, false, false, exe, &out2, &errb2); code != 0 {
		t.Fatalf("second run exit %d: %s", code, errb2.String())
	}
	if !strings.Contains(out2.String(), "skill up to date") {
		t.Fatalf("second run should report skill up to date: %q", out2.String())
	}
}

func TestInstallCodexNoSkillSkips(t *testing.T) {
	work, home, exe := codexFix(t)
	var out, errb bytes.Buffer
	if code := installCodex(work, home, false, false, true, exe, &out, &errb); code != 0 {
		t.Fatalf("installCodex exit %d: %s", code, errb.String())
	}
	dest := filepath.Join(home, ".agents", "skills", "bashback", "SKILL.md")
	if _, err := os.ReadFile(dest); err == nil {
		t.Fatal("skill should not be written with noSkill=true")
	}
}

func TestInstallCodexRejectsBadJSON(t *testing.T) {
	work, home, exe := codexFix(t)
	target := filepath.Join(work, ".codex", "hooks.json")
	mkdirWrite(t, target, "{ not json")

	var out, errb bytes.Buffer
	if code := installCodex(work, home, false, false, true, exe, &out, &errb); code != 1 {
		t.Fatalf("bad JSON should exit 1, got %d", code)
	}
	if b, _ := os.ReadFile(target); string(b) != "{ not json" {
		t.Fatalf("bad JSON file must be left untouched, got %q", b)
	}
	if !strings.Contains(errb.String(), "--print") {
		t.Fatalf("error should guide toward --print: %q", errb.String())
	}
}

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/skills"
)

// installFix sets up an isolated HOME, a project workdir, and a layout. The
// returned exe is the running test binary, which install wires commands at.
func installFix(t *testing.T) (layout paths.Layout, work, home, exe string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	work = t.TempDir()
	layout = paths.New(filepath.Join(t.TempDir(), ".bashback"))
	exe, _ = os.Executable()
	return layout, work, home, exe
}

func TestInstallFreshCreatesSettings(t *testing.T) {
	layout, work, home, exe := installFix(t)
	var out, errb bytes.Buffer
	if code := Install(layout, work, nil, &out, &errb); code != 0 {
		t.Fatalf("install exit %d: %s", code, errb.String())
	}
	target := filepath.Join(work, ".claude", "settings.json")
	hooks, err := parseHooksFile(target)
	if err != nil {
		t.Fatalf("generated settings unreadable: %v", err)
	}
	if n := countStatus(checkWiring(work, home, exe), "ok"); n != 6 {
		t.Fatalf("fresh install should wire all 6, got %d ok", n)
	}
	for event, arr := range hooks {
		for _, m := range arr {
			for _, hk := range m.Hooks {
				if !strings.HasPrefix(hk.Command, exe+" hook ") {
					t.Fatalf("%s command not pointed at exe: %q", event, hk.Command)
				}
				if hk.Timeout != 5 {
					t.Fatalf("%s timeout = %d, want 5", event, hk.Timeout)
				}
			}
		}
	}
}

func TestInstallIdempotent(t *testing.T) {
	layout, work, _, _ := installFix(t)
	target := filepath.Join(work, ".claude", "settings.json")

	var out1, err1 bytes.Buffer
	Install(layout, work, nil, &out1, &err1)
	b1, _ := os.ReadFile(target)

	var out2, err2 bytes.Buffer
	Install(layout, work, nil, &out2, &err2)
	b2, _ := os.ReadFile(target)

	if !bytes.Equal(b1, b2) {
		t.Fatalf("second install changed bytes:\n--- 1 ---\n%s\n--- 2 ---\n%s", b1, b2)
	}
	if !strings.Contains(out2.String(), "already wired") {
		t.Fatalf("second install should report already wired: %q", out2.String())
	}
}

func TestInstallPreservesUnrelated(t *testing.T) {
	layout, work, _, exe := installFix(t)
	target := filepath.Join(work, ".claude", "settings.json")
	pre := `{
  "permissions": { "allow": ["Bash(ls:*)"] },
  "hooks": {
    "PreToolUse": [ { "matcher": "Bash", "hooks": [ { "type": "command", "command": "/other/tool watch", "timeout": 3 } ] } ]
  }
}`
	mkdirWrite(t, target, pre)

	var out, errb bytes.Buffer
	if code := Install(layout, work, nil, &out, &errb); code != 0 {
		t.Fatalf("install exit %d: %s", code, errb.String())
	}

	var got, want map[string]any
	b, _ := os.ReadFile(target)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(pre), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["permissions"], want["permissions"]) {
		t.Fatalf("permissions changed: got %v want %v", got["permissions"], want["permissions"])
	}

	// The other tool's PreToolUse entry survives; a bashback entry is added.
	hooks, _ := parseHooksFile(target)
	pre0 := hooks["PreToolUse"]
	var sawOther, sawBashback bool
	for _, m := range pre0 {
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
		t.Fatalf("PreToolUse should keep both entries: other=%v bashback=%v (%+v)", sawOther, sawBashback, pre0)
	}
}

func TestInstallUpdatesStalePath(t *testing.T) {
	layout, work, _, exe := installFix(t)
	target := filepath.Join(work, ".claude", "settings.json")
	pre := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/old/path/bashback hook pre","timeout":5}]}]}}`
	mkdirWrite(t, target, pre)

	var out, errb bytes.Buffer
	if code := Install(layout, work, nil, &out, &errb); code != 0 {
		t.Fatalf("install exit %d: %s", code, errb.String())
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

func TestInstallPrintDoesNotWrite(t *testing.T) {
	layout, work, _, _ := installFix(t)
	var out, errb bytes.Buffer
	if code := Install(layout, work, []string{"--print"}, &out, &errb); code != 0 {
		t.Fatalf("install --print exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"hooks"`) {
		t.Fatalf("--print should emit merged JSON: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(work, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("--print must not write a file (stat err=%v)", err)
	}
}

func TestInstallRefusesBadJSON(t *testing.T) {
	layout, work, _, _ := installFix(t)
	target := filepath.Join(work, ".claude", "settings.json")
	mkdirWrite(t, target, "{ not json")

	var out, errb bytes.Buffer
	if code := Install(layout, work, nil, &out, &errb); code != 1 {
		t.Fatalf("bad JSON should exit 1, got %d", code)
	}
	if b, _ := os.ReadFile(target); string(b) != "{ not json" {
		t.Fatalf("bad JSON file must be left untouched, got %q", b)
	}
	if !strings.Contains(errb.String(), "--print") {
		t.Fatalf("error should guide toward --print: %q", errb.String())
	}
}

func TestInstallBackup(t *testing.T) {
	layout, work, _, _ := installFix(t)
	target := filepath.Join(work, ".claude", "settings.json")
	pre := `{"hooks":{}}`
	mkdirWrite(t, target, pre)

	var out, errb bytes.Buffer
	if code := Install(layout, work, nil, &out, &errb); code != 0 {
		t.Fatalf("install exit %d: %s", code, errb.String())
	}
	bak, err := os.ReadFile(target + ".bashback-bak")
	if err != nil {
		t.Fatalf("backup should exist: %v", err)
	}
	if string(bak) != pre {
		t.Fatalf("backup should match original bytes, got %q", bak)
	}
}

// installTarget must treat $HOME as a ceiling: ~/.claude/settings.json is the
// user-level file (reachable only via --user), so an implicit install below home
// with no project .claude must fall back to creating one in the workdir, never
// climb up and adopt the user's global settings.
func TestInstallTargetStopsAtHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	mkdirWrite(t, filepath.Join(home, ".claude", "settings.json"), `{"hooks":{}}`)
	work := filepath.Join(home, "projects", "foo")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	got := installTarget(work, home, false)
	if want := filepath.Join(work, ".claude", "settings.json"); got != want {
		t.Fatalf("installTarget climbed past project to user settings:\n got  %s\n want %s", got, want)
	}
}

// The legitimate use of the upward walk - a monorepo whose .claude lives at the
// repo root above the cwd - must keep working.
func TestInstallTargetFindsProjectAncestor(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	repo := filepath.Join(home, "repo")
	mkdirWrite(t, filepath.Join(repo, ".claude", "settings.json"), `{"hooks":{}}`)
	work := filepath.Join(repo, "src", "pkg")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	got := installTarget(work, home, false)
	if want := filepath.Join(repo, ".claude", "settings.json"); got != want {
		t.Fatalf("installTarget should reuse the repo-root settings:\n got  %s\n want %s", got, want)
	}
}

// Running install directly in $HOME without --user resolves to the user-level
// file; that must be refused (and nothing written), not done silently.
func TestInstallRefusesImplicitUserLevel(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	layout := paths.New(filepath.Join(t.TempDir(), ".bashback"))

	var out, errb bytes.Buffer
	if code := Install(layout, home, nil, &out, &errb); code == 0 {
		t.Fatalf("implicit install in $HOME should be refused, got exit 0\nstdout: %s", out.String())
	}
	if !strings.Contains(errb.String(), "--user") {
		t.Fatalf("refusal should point at --user: %q", errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("refused install must not write ~/.claude (stat err=%v)", err)
	}
}

// End-to-end: a real Install run in a home subdirectory leaves a pre-existing
// user-level settings byte-for-byte intact and writes the project file instead.
func TestInstallLeavesUserSettingsIntact(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	userSettings := filepath.Join(home, ".claude", "settings.json")
	const userPre = `{"hooks":{}}`
	mkdirWrite(t, userSettings, userPre)
	work := filepath.Join(home, "projects", "foo")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	layout := paths.New(filepath.Join(t.TempDir(), ".bashback"))

	var out, errb bytes.Buffer
	if code := Install(layout, work, nil, &out, &errb); code != 0 {
		t.Fatalf("install exit %d: %s", code, errb.String())
	}
	if b, _ := os.ReadFile(userSettings); string(b) != userPre {
		t.Fatalf("user-level settings was rewritten by an implicit install: %q", b)
	}
	if _, err := os.Stat(filepath.Join(work, ".claude", "settings.json")); err != nil {
		t.Fatalf("implicit install should create <workdir>/.claude: %v", err)
	}
}

func TestInstallUserLevel(t *testing.T) {
	layout, work, home, _ := installFix(t)
	var out, errb bytes.Buffer
	if code := Install(layout, work, []string{"--user"}, &out, &errb); code != 0 {
		t.Fatalf("install --user exit %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatalf("--user should write ~/.claude/settings.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("--user must not write project settings (stat err=%v)", err)
	}
}

func TestInstallWritesSkill(t *testing.T) {
	layout, work, _, _ := installFix(t)
	var out, errb bytes.Buffer
	if code := Install(layout, work, nil, &out, &errb); code != 0 {
		t.Fatalf("install exit %d: %s", code, errb.String())
	}
	dest := filepath.Join(work, ".claude", "skills", "bashback", "SKILL.md")
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("skill not written: %v", err)
	}
	if !bytes.Equal(b, skills.BashbackSKILL) {
		t.Fatal("skill content differs from embedded copy")
	}
	if !strings.Contains(out.String(), dest) {
		t.Fatalf("install should report the skill write: %q", out.String())
	}

	out.Reset()
	Install(layout, work, nil, &out, &errb)
	if !strings.Contains(out.String(), "skill up to date") {
		t.Fatalf("second install should report skill up to date: %q", out.String())
	}
}

func TestInstallSkillDriftBackedUpAndOverwritten(t *testing.T) {
	layout, work, _, _ := installFix(t)
	dest := filepath.Join(work, ".claude", "skills", "bashback", "SKILL.md")
	mkdirWrite(t, dest, "user-edited")
	var out, errb bytes.Buffer
	if code := Install(layout, work, nil, &out, &errb); code != 0 {
		t.Fatalf("install exit %d: %s", code, errb.String())
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, skills.BashbackSKILL) {
		t.Fatal("drifted skill should be refreshed from the embedded copy")
	}
	bak, err := os.ReadFile(dest + ".bashback-bak")
	if err != nil || string(bak) != "user-edited" {
		t.Fatalf("backup missing or wrong: %v %q", err, bak)
	}
}

func TestInstallNoSkill(t *testing.T) {
	layout, work, _, _ := installFix(t)
	var out, errb bytes.Buffer
	if code := Install(layout, work, []string{"--no-skill"}, &out, &errb); code != 0 {
		t.Fatalf("install exit %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(work, ".claude", "skills")); !os.IsNotExist(err) {
		t.Fatalf("--no-skill must not create a skills dir (stat err=%v)", err)
	}
}

func TestInstallPrintSkipsSkill(t *testing.T) {
	layout, work, _, _ := installFix(t)
	var out, errb bytes.Buffer
	if code := Install(layout, work, []string{"--print"}, &out, &errb); code != 0 {
		t.Fatalf("install --print exit %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(work, ".claude", "skills")); !os.IsNotExist(err) {
		t.Fatal("--print must not write the skill")
	}
	var v map[string]any
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("--print stdout must stay valid JSON: %v\n%s", err, out.String())
	}
}

// A skill write failure must not pass silently: hooks are already wired, so
// the message says so, but the exit code is 1.
func TestInstallSkillWriteFailureExitsOne(t *testing.T) {
	layout, work, _, _ := installFix(t)
	var out, errb bytes.Buffer
	if code := Install(layout, work, nil, &out, &errb); code != 0 {
		t.Fatalf("first install exit %d: %s", code, errb.String())
	}
	skillsDir := filepath.Join(work, ".claude", "skills", "bashback")
	if err := os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte("drift"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(skillsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(skillsDir, 0o755) })

	out.Reset()
	errb.Reset()
	if code := Install(layout, work, nil, &out, &errb); code != 1 {
		t.Fatalf("skill write failure should exit 1, got %d (stderr %q)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "hooks wired, skill install failed") {
		t.Fatalf("failure message should say hooks are wired: %q", errb.String())
	}
}

func TestInstallUserWritesSkillUnderHome(t *testing.T) {
	layout, work, home, _ := installFix(t)
	var out, errb bytes.Buffer
	if code := Install(layout, work, []string{"--user"}, &out, &errb); code != 0 {
		t.Fatalf("install --user exit %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "bashback", "SKILL.md")); err != nil {
		t.Fatalf("--user should write the skill under home: %v", err)
	}
}

func TestInstallBackupPreservesOriginal(t *testing.T) {
	layout, work, _, _ := installFix(t)
	target := filepath.Join(work, ".claude", "settings.json")
	bak := target + ".bashback-bak"
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	// The user's pre-bashback original.
	original := []byte(`{"permissions":{"allow":["A"]}}`)
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatal(err)
	}

	var o1, e1 bytes.Buffer
	if code := Install(layout, work, nil, &o1, &e1); code != 0 {
		t.Fatalf("first install exit %d: %s", code, e1.String())
	}
	if b, _ := os.ReadFile(bak); !bytes.Equal(b, original) {
		t.Fatalf("first backup = %q, want the original", b)
	}
	// Hand-edit settings then install again: the backup must still hold the
	// first (pre-bashback) original, not our own previous output.
	if err := os.WriteFile(target, []byte(`{"permissions":{"allow":["B"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var o2, e2 bytes.Buffer
	if code := Install(layout, work, nil, &o2, &e2); code != 0 {
		t.Fatalf("second install exit %d: %s", code, e2.String())
	}
	if b, _ := os.ReadFile(bak); !bytes.Equal(b, original) {
		t.Fatalf("backup overwritten on second install: got %q, want the original", b)
	}
}

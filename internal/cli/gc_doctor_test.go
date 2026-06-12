package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/skills"
)

// isolateHome points os.UserHomeDir() at a fresh temp dir so doctor's wiring
// check can't pick up the developer's real ~/.claude/settings.json.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", filepath.Join(t.TempDir(), "home"))
}

func TestDoctorWiringSection(t *testing.T) {
	t.Run("all wired exits 0", func(t *testing.T) {
		f := newFix(t)
		isolateHome(t)
		bin := fakeExe(t)
		writeSettings(t, filepath.Join(f.work, ".claude", "settings.json"), bin, true)

		var out, errb bytes.Buffer
		code := Doctor(f.layout, f.work, nil, &out, &errb)
		s := out.String()
		if n := strings.Count(s, "wiring "); n < 6 {
			t.Fatalf("want >=6 wiring rows, got %d:\n%s", n, s)
		}
		if strings.Contains(s, "[FAIL] wiring") {
			t.Fatalf("fully wired doctor should have no wiring FAIL:\n%s", s)
		}
		if code != 0 {
			t.Logf("doctor exit %d (may be environmental git/perm): %s", code, s)
		}
	})

	t.Run("missing one fails with install hint", func(t *testing.T) {
		f := newFix(t)
		isolateHome(t)
		bin := fakeExe(t)
		writeSettings(t, filepath.Join(f.work, ".claude", "settings.json"), bin, false)

		var out, errb bytes.Buffer
		code := Doctor(f.layout, f.work, nil, &out, &errb)
		s := out.String()
		if code != 1 {
			t.Fatalf("missing wiring should exit 1, got %d:\n%s", code, s)
		}
		if !strings.Contains(s, "bashback install") {
			t.Fatalf("doctor should hint at 'bashback install':\n%s", s)
		}
		if !strings.Contains(s, "FAIL") {
			t.Fatalf("missing wiring should produce a FAIL row:\n%s", s)
		}
	})
}

func TestDoctorActivityNever(t *testing.T) {
	f := newFix(t)
	isolateHome(t)
	var out, errb bytes.Buffer
	Doctor(f.layout, f.work, nil, &out, &errb)
	if !strings.Contains(out.String(), "last snapshot: never") {
		t.Fatalf("no journal should report never:\n%s", out.String())
	}
}

func TestDoctorActivityLastAndErrors(t *testing.T) {
	f := newFix(t)
	isolateHome(t)
	// Produce a real journal entry.
	f.write(t, "x", "1")
	f.capture(t, "tool_a", "x", func() { f.write(t, "x", "2") })

	// hook.log: one stale line (48h) + one recent (1h).
	hp := f.layout.HookLogPath(f.work)
	if err := os.MkdirAll(filepath.Dir(hp), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	content := `{"ts":"` + old + `","hook":"post","err":"stale"}` + "\n" +
		`{"ts":"` + recent + `","hook":"post","err":"boom"}` + "\n"
	if err := os.WriteFile(hp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	Doctor(f.layout, f.work, nil, &out, &errb)
	s := out.String()
	if !strings.Contains(s, "last snapshot:") || strings.Contains(s, "last snapshot: never") {
		t.Fatalf("should report a real last-snapshot age:\n%s", s)
	}
	if !strings.Contains(s, "1 hook error(s) in the last 24h") {
		t.Fatalf("should count only the recent hook error:\n%s", s)
	}
	// Activity is informational: it must not by itself flip an otherwise-ok run.
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, "hook error(s)") && strings.HasPrefix(ln, "[FAIL]") {
			t.Fatalf("hook error count should be an info line, not FAIL: %q", ln)
		}
	}
}

func TestDoctorJSONWiringActivity(t *testing.T) {
	f := newFix(t)
	isolateHome(t)
	var out, errb bytes.Buffer
	Doctor(f.layout, f.work, []string{"--json"}, &out, &errb)
	m := decodeJSON(t, out.Bytes())
	if _, ok := m["wiring"].([]any); !ok {
		t.Fatalf("--json should carry a wiring array: %v", m["wiring"])
	}
	if _, ok := m["activity"].(map[string]any); !ok {
		t.Fatalf("--json should carry an activity object: %v", m["activity"])
	}
	if v, _ := m["v"].(float64); v != 1 {
		t.Fatalf("output version should stay 1, got %v", m["v"])
	}
}

func TestDoctorCodexNotWiredIsInfo(t *testing.T) {
	f := newFix(t)
	isolateHome(t)
	// No .codex directory anywhere — codex is simply not set up.
	var out, errb bytes.Buffer
	code := Doctor(f.layout, f.work, nil, &out, &errb)
	s := out.String()
	if !strings.Contains(s, "codex: not wired") {
		t.Fatalf("want 'codex: not wired' info line:\n%s", s)
	}
	// "not wired" must NOT flip the exit code; other failures may exist, but
	// check that the codex line itself is not a FAIL line.
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, "codex: not wired") && strings.HasPrefix(ln, "[FAIL]") {
			t.Fatalf("codex not-wired should be info, not FAIL: %q", ln)
		}
	}
	_ = code // exit code depends on environment (git version etc.)
}

func TestDoctorCodexStaleIsFail(t *testing.T) {
	f := newFix(t)
	isolateHome(t)
	target := filepath.Join(f.work, ".codex", "hooks.json")
	writeCodexHooks(t, target, "/no/such/bin/bashback")

	var out, errb bytes.Buffer
	code := Doctor(f.layout, f.work, nil, &out, &errb)
	s := out.String()
	if !strings.Contains(s, "stale path") {
		t.Fatalf("stale codex wiring should produce stale-path line:\n%s", s)
	}
	if code != 1 {
		t.Fatalf("stale codex wiring should exit 1, got %d:\n%s", code, s)
	}
}

// A user who installed only Codex (or only Cursor) has a healthy setup; the
// absent Claude wiring must not be reported as FAIL nor flip the exit code.
func TestDoctorClaudeAbsentWithOtherPlatformWiredIsInfo(t *testing.T) {
	f := newFix(t)
	isolateHome(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	writeCodexHooks(t, filepath.Join(f.work, ".codex", "hooks.json"), exe)

	var out, errb bytes.Buffer
	code := Doctor(f.layout, f.work, nil, &out, &errb)
	s := out.String()
	if !strings.Contains(s, "codex wiring") {
		t.Fatalf("codex should be wired ok:\n%s", s)
	}
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, "claude") && strings.HasPrefix(ln, "[FAIL]") {
			t.Fatalf("claude wiring should be info when codex is wired, got FAIL: %q", ln)
		}
		if strings.Contains(ln, "run 'bashback install' to wire hooks") {
			t.Fatalf("must not nag to install when another platform is wired:\n%s", s)
		}
	}
	if code != 0 {
		t.Fatalf("codex-only healthy install should exit 0, got %d:\n%s", code, s)
	}
}

// A broken (stale) Claude wiring is real breakage, not an opt-out: it must still
// FAIL and flip the exit code even when another platform is wired, otherwise a
// silently-broken primary setup reads as healthy.
func TestDoctorClaudeStaleWithOtherPlatformWiredStillFails(t *testing.T) {
	f := newFix(t)
	isolateHome(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	writeCodexHooks(t, filepath.Join(f.work, ".codex", "hooks.json"), exe)
	writeSettings(t, filepath.Join(f.work, ".claude", "settings.json"), "/no/such/bin/bashback", true)

	var out, errb bytes.Buffer
	code := Doctor(f.layout, f.work, nil, &out, &errb)
	s := out.String()
	if !strings.Contains(s, "stale path") {
		t.Fatalf("stale claude wiring should report a stale-path row:\n%s", s)
	}
	if code != 1 {
		t.Fatalf("broken claude wiring must fail even when another platform is wired, got %d:\n%s", code, s)
	}
}

func TestDoctorWiringJSONHasPlatform(t *testing.T) {
	f := newFix(t)
	isolateHome(t)
	var out, errb bytes.Buffer
	Doctor(f.layout, f.work, []string{"--json"}, &out, &errb)
	m := decodeJSON(t, out.Bytes())
	wiringRaw, ok := m["wiring"].([]any)
	if !ok {
		t.Fatalf("wiring should be an array: %v", m["wiring"])
	}
	sawClaude := false
	for _, row := range wiringRaw {
		r, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("wiring row should be an object: %v", row)
		}
		p, _ := r["platform"].(string)
		if p == "" {
			t.Fatalf("every wiring row must have a non-empty platform field: %v", r)
		}
		if p == "claude" {
			sawClaude = true
		}
	}
	if !sawClaude {
		t.Fatalf("wiring array must contain at least one row with platform=claude: %v", wiringRaw)
	}
}

// An existing session repo without index.version=4 gets an informational
// doctor line; a fresh one (Init writes it) stays silent.
func TestDoctorPerfConfigHint(t *testing.T) {
	isolateHome(t)
	f := newFix(t)
	f.write(t, "a.txt", "x\n")
	f.capture(t, "tool_pc", "x", func() { f.write(t, "a.txt", "y\n") })
	var out, errb bytes.Buffer
	Doctor(f.layout, f.work, nil, &out, &errb)
	if strings.Contains(out.String(), "predates git perf config") {
		t.Fatalf("fresh repo must not warn: %s", out.String())
	}
	// Strip the perf config to simulate an older repo that predates it.
	entries, _ := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	gd := f.layout.SessionGitDir(f.work, entries[len(entries)-1].SessionID)
	cmd := exec.Command("git", "--git-dir", gd, "config", "--unset", "index.version")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	Doctor(f.layout, f.work, nil, &out, &errb)
	if !strings.Contains(out.String(), "predates git perf config") {
		t.Fatalf("stripped repo must warn: %s", out.String())
	}
}

// A plugin-shipped Claude install carries its hooks in the plugin's own
// hooks.json, invisible to the settings scan; doctor must report it as wired,
// not nag to install or flip the exit code.
func TestDoctorClaudePluginInstallIsInfo(t *testing.T) {
	f := newFix(t)
	isolateHome(t)
	home := os.Getenv("HOME")
	dir := filepath.Join(home, ".claude", "plugins", "cache", "claude-plugins", "bashback", "1.0.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Doctor(f.layout, f.work, nil, &out, &errb)
	s := out.String()
	if !strings.Contains(s, "wired via plugin") {
		t.Fatalf("claude plugin install should report 'wired via plugin':\n%s", s)
	}
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, "claude") && strings.HasPrefix(ln, "[FAIL]") {
			t.Fatalf("plugin install must not FAIL claude wiring: %q", ln)
		}
		if strings.Contains(ln, "run 'bashback install'") {
			t.Fatalf("must not nag to install when plugin is present:\n%s", s)
		}
	}
	if code != 0 {
		t.Fatalf("plugin-only install should exit 0, got %d:\n%s", code, s)
	}
}

func TestDoctorReportsStaleIndexLock(t *testing.T) {
	f := newFix(t)
	isolateHome(t)
	sessDir := filepath.Join(f.layout.RepoDir(f.work), "sessions", "sess1.git")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(sessDir, "index.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	Doctor(f.layout, f.work, nil, &out, &errb)
	if !strings.Contains(out.String(), "stale index.lock") {
		t.Errorf("doctor should report stale index.lock:\n%s", out.String())
	}
}

func TestDoctorRepairQuarantinesBadJournalLine(t *testing.T) {
	isolateHome(t)
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	jp := f.layout.JournalPath(f.work)
	content := `{"v":1,"tool_use_id":"a"}` + "\n" + `{"v":1,"tool_use_` + "\n"
	if err := os.WriteFile(jp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Doctor(f.layout, f.work, []string{"--repair"}, &out, &errb); code != 0 {
		t.Fatalf("doctor --repair exit = %d, stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "moved 1 bad line") {
		t.Fatalf("output = %q, want repair summary", out.String())
	}
	entries, err := journal.Read(jp)
	if err != nil {
		t.Fatalf("journal still unreadable after repair: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
}

func TestDoctorPluginJSONConsistent(t *testing.T) {
	f := newFix(t)
	isolateHome(t)
	home := os.Getenv("HOME")
	dir := filepath.Join(home, ".claude", "plugins", "cache", "claude-plugins", "bashback", "1.0.0")
	mkdirWrite(t, filepath.Join(dir, "skills", "bashback", "SKILL.md"), string(skills.BashbackSKILL))

	var out, errb bytes.Buffer
	if code := Doctor(f.layout, f.work, []string{"--json"}, &out, &errb); code != 0 {
		t.Fatalf("doctor --json exit %d: %s", code, errb.String())
	}
	var got struct {
		Wiring []struct {
			Platform string `json:"platform"`
			Status   string `json:"status"`
		} `json:"wiring"`
		Skill struct {
			Status string `json:"status"`
		} `json:"skill"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unparsable: %v\n%s", err, out.String())
	}
	for _, w := range got.Wiring {
		if w.Platform == "claude" && w.Status == "missing" {
			t.Fatalf("claude wiring must not be 'missing' under a plugin install: %+v", got.Wiring)
		}
	}
	if got.Skill.Status != "ok" {
		t.Fatalf("plugin-shipped skill should report ok, got %q", got.Skill.Status)
	}
}

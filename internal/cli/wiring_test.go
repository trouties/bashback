package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeExe creates an executable file standing in for the bashback binary.
func fakeExe(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bashback")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func mkdirWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSettings writes a settings.json wiring bashback at bin; includeBg toggles the background row.
func writeSettings(t *testing.T, path, bin string, includeBg bool) {
	t.Helper()
	bgLine := ""
	if includeBg {
		bgLine = `,
        { "matcher": "TaskOutput|TaskStop|BashOutput|KillShell", "hooks": [ { "type": "command", "command": "` + bin + ` hook bg", "timeout": 5 } ] }`
	}
	s := `{
  "hooks": {
    "PreToolUse": [ { "matcher": "Bash", "hooks": [ { "type": "command", "command": "` + bin + ` hook pre", "timeout": 5 } ] } ],
    "PostToolUse": [ { "matcher": "Bash", "hooks": [ { "type": "command", "command": "` + bin + ` hook post", "timeout": 5 } ] }` + bgLine + ` ],
    "PostToolUseFailure": [ { "matcher": "Bash", "hooks": [ { "type": "command", "command": "` + bin + ` hook post", "timeout": 5 } ] } ],
    "SessionStart": [ { "hooks": [ { "type": "command", "command": "` + bin + ` hook session-start", "timeout": 5 } ] } ],
    "SessionEnd": [ { "hooks": [ { "type": "command", "command": "` + bin + ` hook session-end", "timeout": 5 } ] } ]
  }
}`
	mkdirWrite(t, path, s)
}

func countStatus(st []wiringStatus, status string) int {
	n := 0
	for _, s := range st {
		if s.Status == status {
			n++
		}
	}
	return n
}

func TestCheckWiringAllWired(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	work := t.TempDir()
	bin := fakeExe(t)
	writeSettings(t, filepath.Join(work, ".claude", "settings.json"), bin, true)

	st := checkWiring(work, home, bin)
	if len(st) != 6 || countStatus(st, "ok") != 6 {
		t.Fatalf("want 6 ok rows, got %d total / %d ok: %+v", len(st), countStatus(st, "ok"), st)
	}
}

func TestCheckWiringMissingBg(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	work := t.TempDir()
	bin := fakeExe(t)
	writeSettings(t, filepath.Join(work, ".claude", "settings.json"), bin, false)

	st := checkWiring(work, home, bin)
	if countStatus(st, "ok") != 5 || countStatus(st, "missing") != 1 {
		t.Fatalf("want 5 ok + 1 missing, got %+v", st)
	}
	for _, s := range st {
		if s.Event == "PostToolUse" && strings.Contains(s.Matcher, "TaskOutput") {
			if s.Status != "missing" {
				t.Fatalf("bg row should be missing, got %q", s.Status)
			}
		}
	}
}

func TestCheckWiringUserLevelOnly(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	work := t.TempDir()
	bin := fakeExe(t)
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), bin, true)

	st := checkWiring(work, home, bin)
	if countStatus(st, "ok") != 6 {
		t.Fatalf("user-level wiring should count, got %+v", st)
	}
}

func TestCheckWiringWalksUp(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := fakeExe(t)
	writeSettings(t, filepath.Join(root, ".claude", "settings.json"), bin, true)

	st := checkWiring(sub, home, bin)
	if countStatus(st, "ok") != 6 {
		t.Fatalf("settings in an ancestor dir should be found, got %+v", st)
	}
}

func TestCheckWiringStaleBinary(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	work := t.TempDir()
	bin := fakeExe(t)
	writeSettings(t, filepath.Join(work, ".claude", "settings.json"), "/no/such/bashback", true)

	st := checkWiring(work, home, bin)
	if countStatus(st, "stale path") != 6 {
		t.Fatalf("non-existent binary should be stale path on every row, got %+v", st)
	}
}

func TestCheckWiringBadJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	work := t.TempDir()
	bin := fakeExe(t)
	badPath := filepath.Join(work, ".claude", "settings.json")
	mkdirWrite(t, badPath, "{ this is not json")
	// A good user-level file still wires everything.
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), bin, true)

	st := checkWiring(work, home, bin)
	if countStatus(st, "unreadable") != 1 {
		t.Fatalf("corrupt settings should yield one unreadable row, got %+v", st)
	}
	if countStatus(st, "ok") != 6 {
		t.Fatalf("corrupt file must not block the good file's wiring, got %+v", st)
	}
	for _, s := range st {
		if s.Status == "unreadable" && s.File != badPath {
			t.Fatalf("unreadable row should name the bad file, got %q", s.File)
		}
	}
}

func writeCodexHooks(t *testing.T, path, bin string) {
	t.Helper()
	s := `{"hooks":{` +
		`"PreToolUse":[{"matcher":"^Bash$","hooks":[{"type":"command","command":"` + bin + ` hook pre","timeout":5}]}],` +
		`"PostToolUse":[{"matcher":"^Bash$","hooks":[{"type":"command","command":"` + bin + ` hook post","timeout":5}]}],` +
		`"SessionStart":[{"hooks":[{"type":"command","command":"` + bin + ` hook session-start","timeout":5}]}],` +
		`"Stop":[{"hooks":[{"type":"command","command":"` + bin + ` hook session-end","timeout":5}]}]` +
		`}}`
	mkdirWrite(t, path, s)
}

func writeCursorHooks(t *testing.T, path, bin string) {
	t.Helper()
	s := `{"version":1,"hooks":{` +
		`"preToolUse":[{"matcher":"Shell","command":"` + bin + ` hook pre"}],` +
		`"postToolUse":[{"matcher":"Shell","command":"` + bin + ` hook post"}],` +
		`"sessionStart":[{"command":"` + bin + ` hook session-start"}],` +
		`"sessionEnd":[{"command":"` + bin + ` hook session-end"}]` +
		`}}`
	mkdirWrite(t, path, s)
}

func TestCheckCodexWiringNotWired(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	work := t.TempDir()
	bin := fakeExe(t)

	st := checkCodexWiring(work, home, bin)
	if len(st) != 1 || st[0].Status != "not wired" || st[0].Platform != "codex" {
		t.Fatalf("no codex files: want single 'not wired' row, got %+v", st)
	}
	if st[0].Event != "" {
		t.Fatalf("not wired row should have empty Event, got %q", st[0].Event)
	}
}

func TestCheckCodexWiringOk(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	work := t.TempDir()
	bin := fakeExe(t)
	writeCodexHooks(t, filepath.Join(work, ".codex", "hooks.json"), bin)

	st := checkCodexWiring(work, home, bin)
	for _, s := range st {
		if s.Platform != "codex" {
			t.Fatalf("all rows should have platform codex, got %+v", s)
		}
	}
	if n := countStatus(st, "ok"); n < 4 {
		t.Fatalf("want >=4 ok rows, got %d: %+v", n, st)
	}
}

func TestCheckCodexWiringPartial(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	work := t.TempDir()
	bin := fakeExe(t)
	// Wire only PreToolUse and PostToolUse — 2 of the 4 codex events.
	content := `{"hooks":{` +
		`"PreToolUse":[{"matcher":"^Bash$","hooks":[{"type":"command","command":"` + bin + ` hook pre","timeout":5}]}],` +
		`"PostToolUse":[{"matcher":"^Bash$","hooks":[{"type":"command","command":"` + bin + ` hook post","timeout":5}]}]` +
		`}}`
	mkdirWrite(t, filepath.Join(work, ".codex", "hooks.json"), content)

	st := checkCodexWiring(work, home, bin)

	for _, row := range st {
		if row.Status == "not wired" {
			t.Fatalf("partial wiring must not collapse to 'not wired', got %+v", st)
		}
		if row.Platform != "codex" {
			t.Fatalf("all rows must have platform codex, got %+v", row)
		}
	}
	if n := countStatus(st, "ok"); n != 2 {
		t.Fatalf("want 2 ok rows (PreToolUse, PostToolUse), got %d: %+v", n, st)
	}
	if n := countStatus(st, "missing"); n != 2 {
		t.Fatalf("want 2 missing rows (SessionStart, Stop), got %d: %+v", n, st)
	}
}

func TestCheckCodexWiringStale(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	work := t.TempDir()
	bin := fakeExe(t)
	writeCodexHooks(t, filepath.Join(work, ".codex", "hooks.json"), "/no/such/bin/bashback")

	st := checkCodexWiring(work, home, bin)
	if n := countStatus(st, "stale path"); n == 0 {
		t.Fatalf("want stale path rows, got %+v", st)
	}
}

func TestCheckCursorWiringNotWired(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	work := t.TempDir()
	bin := fakeExe(t)

	st := checkCursorWiring(work, home, bin)
	if len(st) != 1 || st[0].Status != "not wired" || st[0].Platform != "cursor" {
		t.Fatalf("no cursor files: want single 'not wired' row, got %+v", st)
	}
	if st[0].Event != "" {
		t.Fatalf("not wired row should have empty Event, got %q", st[0].Event)
	}
}

func TestCheckCursorWiringOk(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	work := t.TempDir()
	bin := fakeExe(t)
	writeCursorHooks(t, filepath.Join(work, ".cursor", "hooks.json"), bin)

	st := checkCursorWiring(work, home, bin)
	for _, s := range st {
		if s.Platform != "cursor" {
			t.Fatalf("all rows should have platform cursor, got %+v", s)
		}
	}
	if n := countStatus(st, "ok"); n < 4 {
		t.Fatalf("want >=4 ok rows, got %d: %+v", n, st)
	}
}

// A bare command name (hand-wired, relying on $PATH) must resolve via LookPath,
// not be misreported as a stale path.
func TestResolvesToExecutable(t *testing.T) {
	if !resolvesToExecutable("sh") {
		t.Fatal("bare PATH name should resolve via LookPath")
	}
	if resolvesToExecutable("no-such-binary-bashback-test") {
		t.Fatal("unknown bare name must not resolve")
	}
	if resolvesToExecutable("") {
		t.Fatal("empty token must not resolve")
	}
}

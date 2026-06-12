package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/trouties/bashback/internal/journal"
)

func listOut(t *testing.T, f *fix, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, args, &out, &errb); code != 0 {
		t.Fatalf("list %v exit: %s", args, errb.String())
	}
	return out.String()
}

func TestListLimitN(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_1", "one", func() { f.write(t, "f.txt", "v1") })
	f.capture(t, "tool_2", "two", func() { f.write(t, "f.txt", "v2") })
	f.capture(t, "tool_3", "three", func() { f.write(t, "f.txt", "v3") })

	s := listOut(t, f, "-n", "1")
	if !strings.Contains(s, "tool_3") || strings.Contains(s, "tool_1") {
		t.Fatalf("-n 1 should show only the newest: %q", s)
	}
}

func TestListStatusFilter(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_real", "real", func() { f.write(t, "f.txt", "v1") })
	f.capture(t, "tool_noop", "noop", nil) // skipped_no_change

	s := listOut(t, f, "--status", "protected")
	if !strings.Contains(s, "tool_real") || strings.Contains(s, "tool_noop") {
		t.Fatalf("--status protected: %q", s)
	}
	// Repeatable.
	s = listOut(t, f, "--status", "protected", "--status", "skipped_no_change")
	if !strings.Contains(s, "tool_real") || !strings.Contains(s, "tool_noop") {
		t.Fatalf("repeatable --status: %q", s)
	}
}

func TestListShowsBackground(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	e := journal.Entry{
		ToolUseID: "tool_bg", SessionID: "sess1", TS: "2026-06-10T00:09:00Z",
		Command: "server &", PreSHA: "p", PostSHA: "p", Status: journal.StatusSkippedNoChange,
		Note: "background (post taken at backgrounding; later writes unprotected)",
	}
	if err := journal.Append(f.layout.JournalPath(f.work), e); err != nil {
		t.Fatal(err)
	}

	// The text column truncates the annotation, but the status string and JSON do not.
	if got := listStatus(newReclaimedMemo(f.layout, f.work), e); !strings.Contains(got, "(background)") {
		t.Fatalf("listStatus = %q, want a (background) annotation", got)
	}
	if s := listOut(t, f, "--json"); !strings.Contains(s, `"background": true`) {
		t.Fatalf("list --json should mark the entry background: %q", s)
	}
}

func TestListSessionAndGrep(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_a", "deploy prod", func() { f.write(t, "f.txt", "v1") })
	// Another session entry.
	journal.Append(f.layout.JournalPath(f.work), journal.Entry{
		ToolUseID: "tool_b", SessionID: "other9", TS: "2026-06-10T00:09:00Z",
		Command: "echo hi", PreSHA: "p", PostSHA: "q", Status: journal.StatusProtected,
	})

	if s := listOut(t, f, "--session", "sess1"); strings.Contains(s, "tool_b") {
		t.Fatalf("--session sess1 should exclude other9: %q", s)
	}
	if s := listOut(t, f, "--grep", "deploy"); !strings.Contains(s, "tool_a") || strings.Contains(s, "tool_b") {
		t.Fatalf("--grep deploy: %q", s)
	}
}

func TestListSinceRFC3339(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	// Old entry well before the cutoff.
	journal.Append(f.layout.JournalPath(f.work), journal.Entry{
		ToolUseID: "tool_old", SessionID: "sess1", TS: "2020-01-01T00:00:00Z",
		Command: "ancient", PreSHA: "p", PostSHA: "q", Status: journal.StatusProtected,
	})
	f.capture(t, "tool_recent", "recent", func() { f.write(t, "f.txt", "v1") })

	s := listOut(t, f, "--since", "2026-01-01T00:00:00Z")
	if strings.Contains(s, "tool_old") || !strings.Contains(s, "tool_recent") {
		t.Fatalf("--since cutoff: %q", s)
	}
}

func TestListFullAndColumns(t *testing.T) {
	f := newFix(t)
	longCmd := "echo " + strings.Repeat("z", 120)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_long", longCmd, func() { f.write(t, "f.txt", "v1") })

	// Header has the new @N and FILES columns.
	s := listOut(t, f)
	if !strings.Contains(s, "@N") || !strings.Contains(s, "FILES") {
		t.Fatalf("header missing @N/FILES: %q", s)
	}
	// Default truncates; --full does not.
	if strings.Contains(s, strings.Repeat("z", 120)) {
		t.Fatal("default list should truncate the command")
	}
	full := listOut(t, f, "--full")
	if !strings.Contains(full, strings.Repeat("z", 120)) {
		t.Fatalf("--full should not truncate: %q", full)
	}
	// @1 index for the entry appears.
	if !strings.Contains(s, "@1") {
		t.Fatalf("should show @1 index: %q", s)
	}
}

func TestListAbsVsRelative(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_t", "x", func() { f.write(t, "f.txt", "v1") })

	abs := listOut(t, f, "--abs")
	if !strings.Contains(abs, "2026-06-10T00:00:01Z") {
		t.Fatalf("--abs should show RFC3339: %q", abs)
	}
	rel := listOut(t, f)
	if strings.Contains(rel, "2026-06-10T00:00:01Z") {
		t.Fatalf("default should be relative, not RFC3339: %q", rel)
	}
	if !strings.Contains(rel, "ago") {
		t.Fatalf("relative time should say 'ago': %q", rel)
	}
}

// With protect_paths configured, list flags sparse mode in both the banner and JSON.
func TestListShowsSparseMode(t *testing.T) {
	f := newFix(t)
	Config(f.layout, f.work, []string{"set", "protect_paths", "src"}, &bytes.Buffer{}, &bytes.Buffer{})
	f.write(t, "src/f.txt", "v0")
	f.capture(t, "tool_s", "x", func() { f.write(t, "src/f.txt", "v1") })

	s := listOut(t, f)
	if !strings.Contains(s, "sparse protection active") || !strings.Contains(s, "src") {
		t.Fatalf("list should banner sparse mode: %q", s)
	}

	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, []string{"--json"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	pp, _ := m["protect_paths"].([]any)
	if len(pp) != 1 || pp[0] != "src" {
		t.Fatalf("json should carry protect_paths: %v", m["protect_paths"])
	}
}

// Existing annotations survive the display upgrade.
func TestListAnnotationsPreserved(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.preCapture(t, "tool_int", "interrupted", func() { f.write(t, "f.txt", "half") })
	s := listOut(t, f)
	if !strings.Contains(s, "interrupted") {
		t.Fatalf("pre-only annotation lost: %q", s)
	}
}

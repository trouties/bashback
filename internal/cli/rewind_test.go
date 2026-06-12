package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/trouties/bashback/internal/journal"
)

// The dry-run entries section shows shortest-unique keys; --json keeps the full key in span[].key.
func TestRewindPreviewShortKeys(t *testing.T) {
	f := newFix(t)
	const a, b = "toolu_aaaaaaaabbbbbbbb_one", "toolu_aaaaaaaabbbbbbbb_two"
	f.write(t, "f.txt", "v0")
	f.capture(t, a, "echo a", func() { f.write(t, "f.txt", "v1") })
	f.capture(t, b, "echo b", func() { f.write(t, "f.txt", "v2") })

	// Text dry-run: span covers both; the entries section must use short keys.
	var out, errb bytes.Buffer
	if code := Rewind(f.layout, f.work, []string{"@2", "--dry-run"}, &out, &errb); code != 0 {
		t.Fatalf("rewind dry-run exit: %s", errb.String())
	}
	if strings.Contains(out.String(), a) || strings.Contains(out.String(), b) {
		t.Fatalf("dry-run should abbreviate keys, but a full key appears:\n%s", out.String())
	}
	short := shortKeys([]journal.Entry{{ToolUseID: a}, {ToolUseID: b}})
	if !strings.Contains(out.String(), short[a]) {
		t.Fatalf("dry-run missing short key %q:\n%s", short[a], out.String())
	}

	// JSON dry-run: span[].key stays full for scripting.
	var jout, jerr bytes.Buffer
	if code := Rewind(f.layout, f.work, []string{"@2", "--dry-run", "--json"}, &jout, &jerr); code != 0 {
		t.Fatalf("rewind dry-run --json exit: %s", jerr.String())
	}
	m := decodeJSON(t, jout.Bytes())
	span := m["span"].([]any)
	var sawFull bool
	for _, raw := range span {
		if raw.(map[string]any)["key"] == a {
			sawFull = true
		}
	}
	if !sawFull {
		t.Fatalf("--json span[].key should keep the full key %q, got %v", a, span)
	}
}

// dry-run text lists each touched file with its status letter, capped at 20 with "(+N more)".
func TestRewindDryRunListsFilesWithStatus(t *testing.T) {
	f := newFix(t)
	f.write(t, "keep.txt", "v0")
	keyA := f.capture(t, "tool_fa", "a", func() {
		f.write(t, "fileA.txt", "A")
		f.write(t, "keep.txt", "v1")
	})
	f.capture(t, "tool_fb", "b", func() { f.write(t, "fileB.txt", "B") })

	var out, errb bytes.Buffer
	if code := Rewind(f.layout, f.work, []string{"--dry-run", keyA}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "M  keep.txt") {
		t.Errorf("keep.txt should be listed as modified: %q", s)
	}
	for _, p := range []string{"fileA.txt", "fileB.txt"} {
		if !strings.Contains(s, "A  "+p) {
			t.Errorf("%s should be listed as added: %q", p, s)
		}
	}
}

func TestRewindDryRunFilesCapped(t *testing.T) {
	f := newFix(t)
	keyA := f.capture(t, "tool_many", "a", func() {
		for i := 0; i < 25; i++ {
			f.write(t, fmt.Sprintf("f%02d.txt", i), "x")
		}
	})
	var out, errb bytes.Buffer
	if code := Rewind(f.layout, f.work, []string{"--dry-run", keyA}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "(+5 more)") {
		t.Errorf("25 files should cap at 20 with (+5 more): %q", out.String())
	}
}

// dry-run text lists the span entries (@N, key, command) to undo, capped at 10 with "(+N more)".
func TestRewindDryRunListsSpanEntries(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	keyA := f.capture(t, "tool_sA", "cmd-a", func() { f.write(t, "f.txt", "v1") })
	f.capture(t, "tool_sB", "cmd-b", func() { f.write(t, "g.txt", "x") })
	f.capture(t, "tool_sC", "cmd-c", func() { f.write(t, "h.txt", "y") })

	var out, errb bytes.Buffer
	if code := Rewind(f.layout, f.work, []string{"--dry-run", keyA}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"tool_sA", "tool_sB", "tool_sC", "cmd-c", "@1", "@3"} {
		if !strings.Contains(s, want) {
			t.Errorf("span list missing %q: %q", want, s)
		}
	}
}

func TestRewindDryRunSpanCapped(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	var first string
	for i := 0; i < 12; i++ {
		k := f.capture(t, fmt.Sprintf("tool_c%02d", i), "x", func() {
			f.write(t, fmt.Sprintf("f%02d.txt", i), "y")
		})
		if i == 0 {
			first = k
		}
	}
	var out, errb bytes.Buffer
	if code := Rewind(f.layout, f.work, []string{"--dry-run", first}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "(+2 more)") {
		t.Errorf("12 span entries should cap at 10 with (+2 more): %q", out.String())
	}
}

// --json adds file_changes [{p,s}] and span [{key,ts,command}], keeping the files [string] array.
func TestRewindDryRunJSONFileChangesAndSpan(t *testing.T) {
	f := newFix(t)
	f.write(t, "keep.txt", "v0")
	keyA := f.capture(t, "tool_jA", "cmd-a", func() {
		f.write(t, "fileA.txt", "A")
		f.write(t, "keep.txt", "v1")
	})
	f.capture(t, "tool_jB", "cmd-b", func() { f.write(t, "fileB.txt", "B") })

	var out, errb bytes.Buffer
	if code := Rewind(f.layout, f.work, []string{"--dry-run", "--json", keyA}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	if _, ok := m["files"].([]any); !ok {
		t.Fatalf("files string array should be retained: %v", m["files"])
	}
	fc, ok := m["file_changes"].([]any)
	if !ok || len(fc) == 0 {
		t.Fatalf("file_changes should be a non-empty array: %v", m["file_changes"])
	}
	first := fc[0].(map[string]any)
	if first["p"] == nil || first["s"] == nil {
		t.Fatalf("file_changes entries need p and s: %v", first)
	}
	span, ok := m["span"].([]any)
	if !ok || len(span) == 0 {
		t.Fatalf("span should be a non-empty array: %v", m["span"])
	}
	se := span[0].(map[string]any)
	for _, k := range []string{"key", "ts", "command"} {
		if se[k] == nil {
			t.Fatalf("span entry missing %q: %v", k, se)
		}
	}
}

// A→B→C in one session, rewind to A: work-tree returns to A's pre state, journaled,
// and undo reverses it (acceptance #6).
func TestRewindCLIHappyPath(t *testing.T) {
	f := newFix(t)
	f.write(t, "keep.txt", "v0")
	keyA := f.capture(t, "tool_A", "a", func() {
		f.write(t, "fileA.txt", "A")
		f.write(t, "keep.txt", "v1")
	})
	f.capture(t, "tool_B", "b", func() { f.write(t, "fileB.txt", "B") })
	f.capture(t, "tool_C", "c", func() { f.write(t, "fileC.txt", "C") })

	var out, errb bytes.Buffer
	if code := Rewind(f.layout, f.work, []string{keyA}, &out, &errb); code != 0 {
		t.Fatalf("rewind exit %d: %s", code, errb.String())
	}
	if v := readFile(t, f, "keep.txt"); v != "v0" {
		t.Fatalf("keep.txt = %q, want v0", v)
	}
	for _, name := range []string{"fileA.txt", "fileB.txt", "fileC.txt"} {
		if readFile(t, f, name) != "" {
			t.Errorf("%s should be gone", name)
		}
	}
	// A rewind entry is journaled and is undoable in one step.
	entries, _ := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	var rewKey string
	for _, e := range entries {
		if e.Status == journal.StatusRestored {
			rewKey = e.ToolUseID
		}
	}
	if rewKey == "" {
		t.Fatal("rewind not journaled")
	}
	out.Reset()
	errb.Reset()
	if code := Undo(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatalf("undo of rewind exit %d: %s", code, errb.String())
	}
	if readFile(t, f, "fileC.txt") == "" {
		t.Fatal("undo of rewind should restore the C state")
	}
}

// Cross-session gate: another session's command in the span makes rewind refuse
// without --force (connected-undo warning), then proceed with --force.
func TestRewindCrossSessionGate(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	keyA := f.capture(t, "tool_x", "x", func() { f.write(t, "f.txt", "v1") })

	// A concurrent command from another session, later in the span.
	if err := journal.Append(f.layout.JournalPath(f.work), journal.Entry{
		ToolUseID: "other_sess_cmd", SessionID: "sess2",
		TS: "2026-06-10T00:05:00Z", PreSHA: "p", PostSHA: "q", Status: journal.StatusProtected,
	}); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Rewind(f.layout, f.work, []string{keyA}, &out, &errb); code == 0 {
		t.Fatal("cross-session rewind should refuse without --force")
	}
	if !bytes.Contains(errb.Bytes(), []byte("other sessions")) {
		t.Fatalf("want cross-session warning, got %q", errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := Rewind(f.layout, f.work, []string{"--force", keyA}, &out, &errb); code != 0 {
		t.Fatalf("forced rewind exit %d: %s", code, errb.String())
	}
	if readFile(t, f, "f.txt") != "v0" {
		t.Fatal("forced rewind should revert")
	}
}

// Uncommitted gate: a hand edit not captured by any snapshot blocks rewind without --force.
func TestRewindUncommittedGate(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	keyA := f.capture(t, "tool_u", "x", func() { f.write(t, "f.txt", "v1") })
	// Direct work-tree edit, never journaled.
	f.write(t, "f.txt", "hand-edited")

	var out, errb bytes.Buffer
	if code := Rewind(f.layout, f.work, []string{keyA}, &out, &errb); code == 0 {
		t.Fatal("uncommitted edits should block rewind without --force")
	}
	if !bytes.Contains(errb.Bytes(), []byte("work-tree has edits")) {
		t.Fatalf("want uncommitted warning, got %q", errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Rewind(f.layout, f.work, []string{"--force", keyA}, &out, &errb); code != 0 {
		t.Fatalf("forced rewind exit %d: %s", code, errb.String())
	}
	if readFile(t, f, "f.txt") != "v0" {
		t.Fatal("forced rewind should revert to A's pre")
	}
}

func TestRewindOverlappedGate(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "a")
	key := f.capture(t, "tool_ov", "x", func() { f.write(t, "f.txt", "b") })
	markOverlapped(t, f, key)

	var out, errb bytes.Buffer
	if code := Rewind(f.layout, f.work, []string{key}, &out, &errb); code == 0 {
		t.Fatal("overlapped target should block rewind without --force")
	}
	if !bytes.Contains(errb.Bytes(), []byte("overlapped")) {
		t.Fatalf("want overlapped warning, got %q", errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Rewind(f.layout, f.work, []string{"--force", key}, &out, &errb); code != 0 {
		t.Fatalf("forced rewind exit %d: %s", code, errb.String())
	}
}

// --force must take effect after the positional key (`rewind <key> --force`),
// the order the usage string advertises. Guards against Go's flag package
// stopping at the first non-flag token.
func TestRewindForceAfterKey(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "a")
	key := f.capture(t, "tool_ovfa", "x", func() { f.write(t, "f.txt", "b") })
	markOverlapped(t, f, key)

	var out, errb bytes.Buffer
	if code := Rewind(f.layout, f.work, []string{key, "--force"}, &out, &errb); code != 0 {
		t.Fatalf("--force after key should force through, got refusal: %s", errb.String())
	}
}

func TestRewindDryRun(t *testing.T) {
	f := newFix(t)
	f.write(t, "keep.txt", "v0")
	keyA := f.capture(t, "tool_dr", "a", func() {
		f.write(t, "keep.txt", "v1")
		f.write(t, "extra.txt", "x")
	})
	f.capture(t, "tool_dr2", "b", func() { f.write(t, "more.txt", "y") })

	jbefore, _ := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	var out, errb bytes.Buffer
	if code := Rewind(f.layout, f.work, []string{"--dry-run", keyA}, &out, &errb); code != 0 {
		t.Fatalf("rewind --dry-run exit %d: %s", code, errb.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("would undo")) {
		t.Fatalf("dry-run output: %q", out.String())
	}
	// No work-tree change, no journal row.
	if readFile(t, f, "keep.txt") != "v1" {
		t.Fatal("dry-run must not change the work-tree")
	}
	jafter, _ := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	if len(jafter) != len(jbefore) {
		t.Fatal("dry-run must not journal")
	}
}
